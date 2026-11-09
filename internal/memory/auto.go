package memory

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"time"
)

// Automatically captured notes (internal/memory/capture) live in the same
// store as notes saved on purpose, told apart by their kind. They never
// count against the limits on deliberate notes, so capture can never make a
// memory_save fail, and they have their own limits.
const (
	// KindObservation is one automatically captured fact about a session.
	KindObservation = "observation"
	// KindSessionSummary is the summary of one turn of a captured session.
	KindSessionSummary = "session_summary"
	// AutoTag labels every automatically captured note.
	AutoTag = "auto"
)

// Limits on automatic notes, generous enough for months of steady work. Past
// them, the oldest automatic notes make room. They are variables for tests.
var (
	MaxAutoNotes int64 = 100_000
	MaxAutoBytes int64 = 1 << 30
)

// notAuto selects deliberate notes.
const notAuto = `kind NOT IN ('observation','session_summary')`

// AutoNote is what capture saves.
type AutoNote struct {
	ProjectKey  string
	ProjectRoot string
	Title       string
	Body        string
	Tags        []string
	Source      string
	Kind        string
	// RequestKey makes the save idempotent, so a batch retried after a
	// crash stores nothing twice.
	RequestKey string
	// CreatedAt is when the note was first written, for one imported from elsewhere.
	// Zero: now.
	CreatedAt time.Time
	// EventsFrom is when the earliest thing this note was written from happened. A
	// note from before a forget-all isn't saved, whenever the model finishes.
	EventsFrom time.Time
}

// ErrErasedSince refuses an automatic note written from what happened before a
// forget-all.
var ErrErasedSince = errors.New("written from activity before a forget-all")

// metaErasedAt is when this store's notes were last erased, on this device's clock:
// the same clock that stamps capture's events.
const metaErasedAt = "erased_at"

func markErased(ctx context.Context, tx *sql.Tx, now time.Time) error {
	if _, err := tx.ExecContext(ctx, `DELETE FROM meta WHERE key = ?`, metaErasedClockAhead); err != nil {
		return err
	}
	return setMeta(ctx, tx, metaErasedAt, strconv.FormatInt(now.UnixMilli(), 10))
}

// metaErasedClockAhead keeps a future erase time that was corrected.
const metaErasedClockAhead = "erased_clock_ahead"

func erasedAt(ctx context.Context, q rowQuerier) int64 {
	v, err := readMeta(ctx, q, metaErasedAt)
	if err != nil {
		return 0
	}
	n, _ := strconv.ParseInt(v, 10, 64)
	return n
}

// settleErasedAt reads the erase time. A time still in the future says the clock was
// ahead when it was recorded, and would stop capture until the clock caught up: it
// becomes now, once, and what happens after this moment counts.
func (s *Store) settleErasedAt(ctx context.Context, tx *sql.Tx) (int64, error) {
	n := erasedAt(ctx, tx)
	now := s.now().UnixMilli()
	if n <= now {
		return n, nil
	}
	if err := setMeta(ctx, tx, metaErasedClockAhead, strconv.FormatInt(n, 10)); err != nil {
		return 0, err
	}
	return now, setMeta(ctx, tx, metaErasedAt, strconv.FormatInt(now, 10))
}

// eraseStampSlack is how far ahead of the clock an event may be stamped before it can't
// be trusted to be from after a forget-all. The clock moves a little in ordinary use, and
// an event dropped for that is lost for nothing.
const eraseStampSlack = time.Minute

// EraseCutoff says which activity a forget-all covers.
type EraseCutoff struct {
	at, ahead, now int64
}

func (s *Store) cutoff(ctx context.Context, q rowQuerier, at int64) EraseCutoff {
	c := EraseCutoff{at: at, now: s.now().UnixMilli()}
	if v, err := readMeta(ctx, q, metaErasedClockAhead); err == nil {
		c.ahead, _ = strconv.ParseInt(v, 10, 64)
	}
	return c
}

// Covers reports that an event stamped at this time may come from before the forget-all,
// so it isn't written up. Besides what's before the cut-off, that's two cases of a stamp
// ahead of the clock. With the clock known to have been ahead at the forget-all, a stamp
// up to that time is from then. Otherwise a stamp more than the slack ahead proves
// nothing about when it happened. A store never erased covers nothing.
func (c EraseCutoff) Covers(at time.Time) bool {
	if c.at == 0 {
		return false
	}
	ms := at.UnixMilli()
	switch {
	case ms <= c.at:
		return true
	case c.ahead > 0 && ms > c.now && ms <= c.ahead:
		return true
	}
	return ms > c.now+eraseStampSlack.Milliseconds()
}

// EraseCutoffNow reads the cut-off, settling a time left in the future.
func (s *Store) EraseCutoffNow(ctx context.Context) EraseCutoff {
	erased := s.ErasedAt(ctx)
	if erased.IsZero() {
		return EraseCutoff{}
	}
	return s.cutoff(ctx, s.db, erased.UnixMilli())
}

// ErasedAt is when this store's notes were last erased, or zero.
func (s *Store) ErasedAt(ctx context.Context) time.Time {
	n := erasedAt(ctx, s.db)
	if n > s.now().UnixMilli() {
		_ = s.tx(ctx, func(tx *sql.Tx) error {
			var err error
			n, err = s.settleErasedAt(ctx, tx)
			return err
		})
	}
	if n > 0 {
		return time.UnixMilli(n)
	}
	return time.Time{}
}

// ErasedClockWasAhead reports that the last forget-all was recorded with this device's
// clock ahead, and capture's cut-off was moved to when that was noticed.
func (s *Store) ErasedClockWasAhead(ctx context.Context) bool {
	v, err := readMeta(ctx, s.db, metaErasedClockAhead)
	return err == nil && v != ""
}

// SaveAuto stores an automatic note on this device. It never queues
// anything for Fleet: whether an automatic note may leave the device is a
// separate decision, made elsewhere. The oldest automatic notes make room
// when their limits are reached.
func (s *Store) SaveAuto(ctx context.Context, n AutoNote) (id string, duplicate bool, err error) {
	if n.Kind != KindObservation && n.Kind != KindSessionSummary {
		return "", false, fmt.Errorf("%w: unknown automatic kind %q", ErrInvalidInput, n.Kind)
	}
	nn := NewNote{ProjectKey: n.ProjectKey, ProjectRoot: n.ProjectRoot, Title: n.Title, Body: n.Body, Tags: n.Tags, Source: n.Source}
	if err := nn.Validate(); err != nil {
		return "", false, err
	}
	tags, _ := json.Marshal(nn.Tags)
	err = s.tx(ctx, func(tx *sql.Tx) error {
		// In this transaction, so that a forget-all is either before this check or
		// after this note, which it then deletes.
		if !n.EventsFrom.IsZero() {
			at, err := s.settleErasedAt(ctx, tx)
			if err != nil {
				return err
			}
			if s.cutoff(ctx, tx, at).Covers(n.EventsFrom) {
				return ErrErasedSince
			}
		}
		if n.RequestKey != "" {
			if prev, ok, err := appliedResult(ctx, tx, n.RequestKey); err != nil {
				return err
			} else if ok {
				var r SaveResult
				if err := json.Unmarshal(prev, &r); err != nil {
					return err
				}
				id, duplicate = r.ID, true
				return nil
			}
		}
		h, err := tickHLC(ctx, tx, s.now(), s.device)
		if err != nil {
			return err
		}
		now := s.now().UnixMilli()
		created := now
		if !n.CreatedAt.IsZero() {
			created = n.CreatedAt.UnixMilli()
		}
		r := record{
			ID: newID(), Content: nn.Body, Hash: ContentHash(nn.Body), Title: nn.Title, Kind: n.Kind,
			Scope: scopeOf(nn.ProjectKey), ProjectKey: nn.ProjectKey, ProjectRoot: nn.ProjectRoot, Tags: string(tags),
			Source: nn.Source, Origin: "device", CreatedAt: created, UpdatedAt: now,
			CreatedHLC: string(h), TitleHLC: string(h), ScopeHLC: string(h), Visibility: "visible",
			SyncState: StateLocal,
		}
		if err := insertRecord(ctx, tx, r, n.RequestKey); err != nil {
			return err
		}
		id = r.ID
		if n.RequestKey != "" {
			if err := recordApplied(ctx, tx, n.RequestKey, "memory.auto", SaveResult{ID: r.ID, State: StateLocal}, now); err != nil {
				return err
			}
		}
		_, err = pruneAuto(ctx, tx)
		return err
	})
	return id, duplicate, err
}

// pruneAuto drops the oldest automatic notes over the count and size limits.
// Only notes still local are touched.
func pruneAuto(ctx context.Context, tx *sql.Tx) (int64, error) {
	const auto = `kind IN ('observation','session_summary') AND sync_state = 'local'`
	var dropped, count, size int64
	if err := tx.QueryRowContext(ctx, `SELECT count(*), coalesce(sum(length(CAST(title AS BLOB)) + length(CAST(content AS BLOB)) + length(CAST(tags AS BLOB))), 0) FROM records WHERE `+auto).Scan(&count, &size); err != nil {
		return dropped, err
	}
	for count > MaxAutoNotes || size > MaxAutoBytes {
		// Remove in small steps: the over-limit case is one note at a time
		// in normal use, and a big step could overshoot on size.
		var rowid, n int64
		if err := tx.QueryRowContext(ctx, `SELECT rowid, length(CAST(title AS BLOB)) + length(CAST(content AS BLOB)) + length(CAST(tags AS BLOB)) FROM records WHERE `+auto+` ORDER BY created_at, rowid LIMIT 1`).Scan(&rowid, &n); err != nil {
			return dropped, err
		}
		if _, err := tx.ExecContext(ctx, `DELETE FROM records WHERE rowid = ?`, rowid); err != nil {
			return dropped, err
		}
		count, size, dropped = count-1, size-n, dropped+1
	}
	return dropped, nil
}

// TurnObservations returns the session's observations since its last summary,
// oldest first, at most limit.
func (s *Store) TurnObservations(ctx context.Context, sessionTag string, limit int) ([]Note, error) {
	q, _ := json.Marshal(sessionTag)
	rows, err := s.db.QueryContext(ctx, `SELECT `+noteCols+` FROM records r WHERE r.kind = 'observation' AND r.visibility = 'visible' AND instr(r.tags, ?) > 0
AND r.rowid > coalesce((SELECT max(rowid) FROM records WHERE kind = 'session_summary' AND instr(tags, ?) > 0), 0)
ORDER BY r.rowid LIMIT ?`, string(q), string(q), limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Note
	for rows.Next() {
		n, err := scanNote(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, n)
	}
	return out, rows.Err()
}

// SessionObservations returns the observations tagged with sessionTag, oldest
// first, at most limit.
func (s *Store) SessionObservations(ctx context.Context, sessionTag string, limit int) ([]Note, error) {
	q, _ := json.Marshal(sessionTag)
	rows, err := s.db.QueryContext(ctx, `SELECT `+noteCols+` FROM records r WHERE r.kind = 'observation' AND r.visibility = 'visible' AND instr(r.tags, ?) > 0 ORDER BY r.created_at, r.rowid LIMIT ?`, string(q), limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Note
	for rows.Next() {
		n, err := scanNote(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, n)
	}
	return out, rows.Err()
}

// AutoCounts are automatic notes saved since a time.
type AutoCounts struct {
	Observations int `json:"observations"`
	Summaries    int `json:"summaries"`
}

// CountAutoSince counts automatic notes saved since a time, by when this store got
// them. Imported notes aren't counted.
func (s *Store) CountAutoSince(ctx context.Context, since time.Time) (AutoCounts, error) {
	var c AutoCounts
	ms := since.UnixMilli()
	err := s.db.QueryRowContext(ctx, `SELECT coalesce(sum(kind = 'observation'), 0), coalesce(sum(kind = 'session_summary'), 0) FROM records
WHERE kind IN ('observation','session_summary') AND visibility = 'visible' AND created_at >= ? AND source != ?`, ms, SourceImport).Scan(&c.Observations, &c.Summaries)
	return c, err
}
