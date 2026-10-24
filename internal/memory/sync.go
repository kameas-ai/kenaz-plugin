package memory

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"regexp"
	"time"
	"unicode/utf8"
)

// ---- settings and consent ----

// SyncSettings is this profile's sync configuration and progress.
type SyncSettings struct {
	Enabled        bool   `json:"enabled"`
	EverEnabled    bool   `json:"ever_enabled"`
	ConsentVersion string `json:"consent_version,omitempty"`
	ErasePending   bool   `json:"erase_pending"`
	Resetting      bool   `json:"resetting"`
	StatusCode     string `json:"status_code,omitempty"`
}

// SyncSettings reads the profile's sync settings.
func (s *Store) SyncSettings(ctx context.Context) (SyncSettings, error) {
	var st SyncSettings
	var phase string
	err := s.db.QueryRowContext(ctx, `SELECT enabled, ever_enabled, consent_version, erase_pending, reset_phase, status_code FROM sync_state WHERE singleton = 1`).
		Scan(&st.Enabled, &st.EverEnabled, &st.ConsentVersion, &st.ErasePending, &phase, &st.StatusCode)
	st.Resetting = phase != ""
	return st, err
}

// ErrLocalProfile refuses sync operations on the device notebook.
var ErrLocalProfile = errors.New("the device notebook never syncs; sign in to Kameas Fleet to sync an account's memory")

// EnableResult is what turning sync on queued.
type EnableResult struct {
	// Queued is how many existing notes of this account (global scope)
	// were queued for upload, as the consent notice describes.
	Queued int `json:"queued"`
	// LocalOnly counts notes that stay on the device (project scope).
	LocalOnly int `json:"local_only"`
}

// EnableSync turns sync on for this account profile under consentVersion.
// It must only be called after a human authorized memory.sync_enable and
// Fleet accepted the settings change. Existing eligible notes of this
// account (not the device notebook) are queued; held (unapproved) work is
// released.
func (s *Store) EnableSync(ctx context.Context, consentVersion string, o MutateOptions) (EnableResult, error) {
	var res EnableResult
	if s.profile.IsLocal() {
		return res, ErrLocalProfile
	}
	if consentVersion == "" {
		return res, fmt.Errorf("%w: consent version required", ErrInvalidInput)
	}
	err := s.tx(ctx, func(tx *sql.Tx) error {
		if o.RequestKey != "" {
			if prev, ok, err := appliedResult(ctx, tx, o.RequestKey); err != nil || ok {
				if ok {
					err = json.Unmarshal(prev, &res)
				}
				return err
			}
		}
		if _, err := tx.ExecContext(ctx, `UPDATE sync_state SET enabled = 1, ever_enabled = 1, consent_version = ?, status_code = '' WHERE singleton = 1`, consentVersion); err != nil {
			return err
		}
		// Only the notes the user saw listed on the approval page, with
		// the exact content and title they had then, are queued. Held (unapproved)
		// changes stay held: they need their own approval.
		for _, h := range o.Records {
			r, ok, err := loadRecord(ctx, tx, h.RecordID)
			if err != nil {
				return err
			}
			if !ok || r.Visibility != "visible" || r.Hash != h.Hash || r.Title != h.Title || r.SyncState != StateLocal {
				continue
			}
			var pending int
			if err := tx.QueryRowContext(ctx, `SELECT count(*) FROM outbox WHERE record_id = ? AND op = 'upsert'`, r.ID).Scan(&pending); err != nil {
				return err
			}
			if pending > 0 {
				continue
			}
			st, _, op, err := s.makeEligible(ctx, tx, &r, true, 0)
			if err != nil {
				return err
			}
			if st == StatePending {
				res.Queued++
				// An edit made while sync was off: link its job to the
				// create just queued.
				if _, err := tx.ExecContext(ctx, `UPDATE replacements SET new_upsert_op = ? WHERE new_id = ? AND phase = 'prepared' AND new_upsert_op = 0`, op, r.ID); err != nil {
					return err
				}
				if _, err := tx.ExecContext(ctx, `UPDATE outbox SET depends_on_op = ? WHERE op_id IN (SELECT old_forget_op FROM replacements WHERE new_id = ? AND phase = 'prepared')`, op, r.ID); err != nil {
					return err
				}
			} else {
				res.LocalOnly++
			}
		}
		if o.RequestKey != "" {
			return recordApplied(ctx, tx, o.RequestKey, "memory.sync_enable", res, s.now().UnixMilli())
		}
		return nil
	})
	return res, err
}

// DisableSync turns sync off for this profile. Queued upserts stay queued
// but are not sent; queued forgets still go (Fleet keeps synced notes until
// they are deleted explicitly). Nothing on Fleet is deleted by this.
func (s *Store) DisableSync(ctx context.Context, code string) error {
	return s.tx(ctx, func(tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx, `UPDATE sync_state SET enabled = 0, status_code = ? WHERE singleton = 1`, code)
		return err
	})
}

// SetStatusCode records the sync worker's latest condition (a short code,
// never content), e.g. "capability_not_in_tier" or "consent_reconfirm".
func (s *Store) SetStatusCode(ctx context.Context, code string) error {
	return s.tx(ctx, func(tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx, `UPDATE sync_state SET status_code = ? WHERE singleton = 1`, code)
		return err
	})
}

// Cursor returns the persisted pull cursor.
func (s *Store) Cursor(ctx context.Context) (string, error) {
	set, err := loadSettings(ctx, s.db)
	return set.Cursor, err
}

// ---- pull ----

// PulledRecord is one record of a Fleet pull page (live or tombstone).
type PulledRecord struct {
	ID, Seq, State                 string
	Content, ContentHash, Kind     string
	Source, CreatedAt              string
	Title, TitleHLC                string
	Pinned                         bool
	PinnedHLC, ScopeKind, ScopeHLC string
	Aliases                        []string
	Reason, SupersededBy           string
}

// PullPage is one page as memsync hands it over.
type PullPage struct {
	Records []PulledRecord
	Cursor  string
	HasMore bool
}

// ErrErasePending refuses pulled data while a forget-all has not reached
// Fleet: a page fetched before the erase must not bring notes back.
var ErrErasePending = errors.New("a forget-all is waiting to reach Kameas Fleet; nothing is pulled until it does")

func checkNoErase(ctx context.Context, tx *sql.Tx) error {
	set, err := loadSettings(ctx, tx)
	if err != nil {
		return err
	}
	if set.ErasePending {
		return ErrErasePending
	}
	return nil
}

// ErrInvalidPage means a pulled page failed validation; nothing in it was
// applied and the cursor did not move.
var ErrInvalidPage = errors.New("Fleet sent a memory record this device cannot accept")

var idRe = regexp.MustCompile(`^[A-Za-z0-9._-]{1,64}$`)

// MaxRemoteContentBytes is Fleet's content cap.
const MaxRemoteContentBytes = 32 << 10

func validatePulled(r PulledRecord) error {
	if !idRe.MatchString(r.ID) {
		return fmt.Errorf("%w: bad id", ErrInvalidPage)
	}
	switch r.State {
	case "tombstone":
		return nil
	case "live":
	default:
		return fmt.Errorf("%w: bad state", ErrInvalidPage)
	}
	if len(r.Content) == 0 || len(r.Content) > MaxRemoteContentBytes || !utf8.ValidString(r.Content) {
		return fmt.Errorf("%w: bad content size", ErrInvalidPage)
	}
	if ContentHash(r.Content) != r.ContentHash {
		return fmt.Errorf("%w: content hash mismatch", ErrInvalidPage)
	}
	if !syncedScope(r.ScopeKind) {
		return fmt.Errorf("%w: scope not synced", ErrInvalidPage)
	}
	if utf8.RuneCountInString(r.Title) > 256 || !utf8.ValidString(r.Title) {
		return fmt.Errorf("%w: bad title", ErrInvalidPage)
	}
	for _, a := range r.Aliases {
		if !idRe.MatchString(a) {
			return fmt.Errorf("%w: bad alias", ErrInvalidPage)
		}
	}
	return nil
}

// PullOutcome counts what a page did (no content).
type PullOutcome struct {
	Applied, Removed, Skipped int
}

// ApplyPullPage applies one page atomically: records, aliases and
// tombstones, the HLC advance and the cursor move commit together. A page
// with any invalid record is refused whole and the cursor stays.
func (s *Store) ApplyPullPage(ctx context.Context, l Lease, p PullPage) (PullOutcome, error) {
	var out PullOutcome
	for _, r := range p.Records {
		if err := validatePulled(r); err != nil {
			return out, err
		}
	}
	err := s.tx(ctx, func(tx *sql.Tx) error {
		if err := checkLease(ctx, tx, l); err != nil {
			return err
		}
		if err := checkNoErase(ctx, tx); err != nil {
			return err
		}
		for _, r := range p.Records {
			if err := s.applyPulled(ctx, tx, r, &out); err != nil {
				return err
			}
		}
		_, err := tx.ExecContext(ctx, `UPDATE sync_state SET cursor = ?, last_pull_at = ? WHERE singleton = 1`, p.Cursor, s.now().UnixMilli())
		return err
	})
	return out, err
}

func tombstoneReason(ctx context.Context, tx *sql.Tx, id string) (string, error) {
	var r string
	err := tx.QueryRowContext(ctx, `SELECT reason FROM tombstones WHERE id = ?`, id).Scan(&r)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil
	}
	return r, err
}

func (s *Store) applyPulled(ctx context.Context, tx *sql.Tx, r PulledRecord, out *PullOutcome) error {
	now := s.now()
	if r.State == "tombstone" {
		lr, ok, err := loadRecord(ctx, tx, resolveID(ctx, tx, r.ID))
		if err != nil {
			return err
		}
		if ok && r.Reason == "left_sync_scope" && !syncedScope(lr.Scope) {
			// This device demoted it out of the synced scopes: keep it.
			out.Skipped++
			return nil
		}
		// Forget work for this id is done: Fleet already holds it as a
		// tombstone.
		if _, err := tx.ExecContext(ctx, `DELETE FROM outbox WHERE record_id = ? AND op = 'forget' AND state != 'inflight'`, r.ID); err != nil {
			return err
		}
		if ok {
			out.Removed++
			if lr.Visibility == "replaced" {
				_, err := tx.ExecContext(ctx, `UPDATE replacements SET phase = 'complete' WHERE old_id = ? AND phase NOT IN ('complete','canceled','blocked')`, lr.ID)
				if err != nil {
					return err
				}
			}
			return s.removeLocal(ctx, tx, lr.ID, r.Reason, r.Seq, now)
		}
		_, err = tx.ExecContext(ctx, `UPDATE tombstones SET acked = 1, server_seq = ? WHERE id = ?`, r.Seq, r.ID)
		return err
	}
	// Deletion wins: an id (or alias) this device deleted is not revived.
	// An id hidden by an edit in progress ('replaced') is not deleted: its
	// hidden row still takes field updates from other devices, so a
	// reverted edit shows the latest title and pin.
	for _, id := range append([]string{r.ID}, r.Aliases...) {
		reason, err := tombstoneReason(ctx, tx, id)
		if err != nil {
			return err
		}
		if reason == "" {
			continue
		}
		if reason == "replaced" && id == r.ID {
			if lr, ok, err := loadRecord(ctx, tx, id); err != nil {
				return err
			} else if ok && lr.Visibility == "replaced" {
				continue
			}
		}
		out.Skipped++
		return nil
	}
	for _, h := range []string{r.TitleHLC, r.PinnedHLC, r.ScopeHLC} {
		if err := observeHLC(ctx, tx, now, h); err != nil {
			return err
		}
	}
	// Our own ids that Fleet merged into r: re-key them.
	for _, a := range r.Aliases {
		if _, ok, err := loadRecord(ctx, tx, a); err != nil {
			return err
		} else if ok {
			if err := rekey(ctx, tx, a, r.ID); err != nil {
				return err
			}
		} else if _, err := tx.ExecContext(ctx, `INSERT INTO aliases(alias_id, canonical_id) VALUES (?, ?) ON CONFLICT(alias_id) DO UPDATE SET canonical_id = excluded.canonical_id`, a, r.ID); err != nil {
			return err
		}
	}
	lr, ok, err := loadRecord(ctx, tx, r.ID)
	if err != nil {
		return err
	}
	out.Applied++
	if !ok {
		created := now.UnixMilli()
		if t, err := time.Parse(time.RFC3339Nano, r.CreatedAt); err == nil {
			created = t.UnixMilli()
		}
		kind := r.Kind
		if kind == "" {
			kind = "raw"
		}
		nr := record{ID: r.ID, Content: r.Content, Hash: r.ContentHash, Title: r.Title, Kind: kind, Scope: r.ScopeKind,
			ProjectKey: GlobalProject, Tags: "[]", Source: r.Source, Origin: "fleet", CreatedAt: created, UpdatedAt: now.UnixMilli(),
			CreatedHLC: string(FormatHLC(created, 0, "fleet")), TitleHLC: r.TitleHLC, ScopeHLC: r.ScopeHLC, PinnedHLC: r.PinnedHLC,
			Pinned: r.Pinned, Visibility: "visible", SyncState: StateSynced, ServerSeq: r.Seq}
		return insertRecord(ctx, tx, nr, "")
	}
	// Known record: content is immutable on Fleet, so Fleet's copy wins;
	// title, pinned and scope are per-field last-writer-wins by HLC.
	if lr.Hash != r.ContentHash {
		lr.Content, lr.Hash = r.Content, r.ContentHash
	}
	if r.TitleHLC > lr.TitleHLC {
		lr.Title, lr.TitleHLC = r.Title, r.TitleHLC
	}
	if r.PinnedHLC > lr.PinnedHLC {
		lr.Pinned, lr.PinnedHLC = r.Pinned, r.PinnedHLC
	}
	if r.ScopeHLC > lr.ScopeHLC {
		lr.Scope, lr.ScopeHLC = r.ScopeKind, r.ScopeHLC
	}
	if _, err := tx.ExecContext(ctx, `UPDATE records SET content = ?, content_hash = ?, title = ?, title_hlc = ?, pinned = ?, pinned_hlc = ?, scope = ?, scope_hlc = ?, server_seq = ?, updated_at = ? WHERE id = ?`,
		lr.Content, lr.Hash, lr.Title, lr.TitleHLC, lr.Pinned, lr.PinnedHLC, lr.Scope, lr.ScopeHLC, r.Seq, now.UnixMilli(), lr.ID); err != nil {
		return err
	}
	return markSyncedIfIdle(ctx, tx, lr.ID, "")
}

// ---- reset ----

// ResetState is an unfinished reset to resume after a restart.
type ResetState struct {
	Reason, ErasedBefore string
}

// PendingReset reports an interrupted reset (it restarts from the empty
// cursor: a partial snapshot is never published).
func (s *Store) PendingReset(ctx context.Context) (ResetState, bool, error) {
	var phase string
	var rs ResetState
	err := s.db.QueryRowContext(ctx, `SELECT reset_phase, reset_reason, reset_erased_before FROM sync_state WHERE singleton = 1`).Scan(&phase, &rs.Reason, &rs.ErasedBefore)
	return rs, phase != "", err
}

// BeginReset starts a staged snapshot in a new generation: upserts pause
// until CommitReset. Staged rows of an older, interrupted snapshot are
// discarded.
func (s *Store) BeginReset(ctx context.Context, l Lease, reason, erasedBefore string) (int64, error) {
	var gen int64
	err := s.tx(ctx, func(tx *sql.Tx) error {
		if err := checkLease(ctx, tx, l); err != nil {
			return err
		}
		if err := checkNoErase(ctx, tx); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `DELETE FROM snapshot_stage`); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `UPDATE sync_state SET reset_phase = 'staging', reset_reason = ?, reset_erased_before = ?, snapshot_generation = snapshot_generation + 1 WHERE singleton = 1`, reason, erasedBefore); err != nil {
			return err
		}
		return tx.QueryRowContext(ctx, `SELECT snapshot_generation FROM sync_state WHERE singleton = 1`).Scan(&gen)
	})
	return gen, err
}

// StageSnapshot stores one snapshot page in generation gen (not visible).
func (s *Store) StageSnapshot(ctx context.Context, l Lease, gen int64, p PullPage) error {
	for _, r := range p.Records {
		if err := validatePulled(r); err != nil {
			return err
		}
	}
	return s.tx(ctx, func(tx *sql.Tx) error {
		if err := checkLease(ctx, tx, l); err != nil {
			return err
		}
		if err := checkNoErase(ctx, tx); err != nil {
			return err
		}
		var cur int64
		if err := tx.QueryRowContext(ctx, `SELECT snapshot_generation FROM sync_state WHERE singleton = 1`).Scan(&cur); err != nil {
			return err
		}
		if cur != gen {
			return ErrLeaseLost
		}
		for _, r := range p.Records {
			b, _ := json.Marshal(r)
			if _, err := tx.ExecContext(ctx, `INSERT INTO snapshot_stage(generation, id, record) VALUES (?,?,?) ON CONFLICT(generation, id) DO UPDATE SET record = excluded.record`, gen, r.ID, b); err != nil {
				return err
			}
		}
		return nil
	})
}

// CommitReset reconciles the complete snapshot atomically and publishes
// it with its cursor:
//   - for reason erased, notes in synced scopes created before
//     erased_before are deleted (unless the erase was this device's own,
//     which already cleared them at its barrier);
//   - notes Fleet held (synced or pulled) that the snapshot lacks are
//     deleted, with their queued upserts;
//   - unsynced notes (pending, unapproved, local) and project-scope notes
//     are kept and their upserts resume after the commit;
//   - the snapshot is applied like a pull.
func (s *Store) CommitReset(ctx context.Context, l Lease, gen int64, cursor string) (PullOutcome, error) {
	var out PullOutcome
	err := s.tx(ctx, func(tx *sql.Tx) error {
		if err := checkLease(ctx, tx, l); err != nil {
			return err
		}
		if err := checkNoErase(ctx, tx); err != nil {
			return err
		}
		var cur int64
		var reason, erasedBefore, own string
		if err := tx.QueryRowContext(ctx, `SELECT snapshot_generation, reset_reason, reset_erased_before, own_erase_before FROM sync_state WHERE singleton = 1`).Scan(&cur, &reason, &erasedBefore, &own); err != nil {
			return err
		}
		if cur != gen {
			return ErrLeaseLost
		}
		var staged []PulledRecord
		in := map[string]bool{}
		rows, err := tx.QueryContext(ctx, `SELECT record FROM snapshot_stage WHERE generation = ? ORDER BY rowid`, gen)
		if err != nil {
			return err
		}
		for rows.Next() {
			var b []byte
			var r PulledRecord
			rows.Scan(&b)
			if err := json.Unmarshal(b, &r); err != nil {
				rows.Close()
				return err
			}
			staged = append(staged, r)
			if r.State == "live" {
				in[r.ID] = true
				for _, a := range r.Aliases {
					in[a] = true
				}
			}
		}
		rows.Close()
		var drop []string
		rrows, err := tx.QueryContext(ctx, `SELECT id, created_hlc, provenance, sync_state, server_seq FROM records WHERE scope IN ('global','long_term')`)
		if err != nil {
			return err
		}
		for rrows.Next() {
			var id, chlc, origin, st, seq string
			rrows.Scan(&id, &chlc, &origin, &st, &seq)
			erased := reason == "erased" && erasedBefore != "" && erasedBefore != own && chlc < erasedBefore
			known := origin == "fleet" || st == StateSynced || seq != ""
			if erased || (known && !in[id]) {
				drop = append(drop, id)
			}
		}
		rrows.Close()
		for _, id := range drop {
			if _, err := tx.ExecContext(ctx, `DELETE FROM outbox WHERE record_id = ? AND state != 'inflight'`, id); err != nil {
				return err
			}
			if _, err := tx.ExecContext(ctx, `DELETE FROM records WHERE id = ?`, id); err != nil {
				return err
			}
			out.Removed++
		}
		if reason == "erased" {
			// Fleet forgot everything: queued forgets have nothing left to do.
			if _, err := tx.ExecContext(ctx, `DELETE FROM outbox WHERE op = 'forget' AND state != 'inflight'`); err != nil {
				return err
			}
			if _, err := tx.ExecContext(ctx, `UPDATE tombstones SET acked = 1`); err != nil {
				return err
			}
		}
		for _, r := range staged {
			if err := s.applyPulled(ctx, tx, r, &out); err != nil {
				return err
			}
		}
		if _, err := tx.ExecContext(ctx, `DELETE FROM snapshot_stage`); err != nil {
			return err
		}
		_, err = tx.ExecContext(ctx, `UPDATE sync_state SET cursor = ?, reset_phase = '', reset_reason = '', reset_erased_before = '', last_pull_at = ? WHERE singleton = 1`, cursor, s.now().UnixMilli())
		return err
	})
	return out, err
}

// ---- status and export ----

// SyncStatus is the truthful sync picture for status output: counts and
// codes only, never content.
type SyncStatus struct {
	SyncSettings
	// Notes counts visible notes by sync state.
	Notes map[string]int `json:"notes"`
	// Queue counts outbox ops by state (ready, inflight, unapproved,
	// held, blocked).
	Queue               map[string]int `json:"queue"`
	ForgetsPending      int            `json:"forgets_pending"`
	EditsInProgress     int            `json:"edits_in_progress"`
	EditsBlocked        int            `json:"edits_blocked"`
	BlockedCodes        []string       `json:"blocked_codes,omitempty"`
	LastPullAt          *time.Time     `json:"last_pull_at,omitempty"`
	LastPushAt          *time.Time     `json:"last_push_at,omitempty"`
	ProjectNotesLocal   int            `json:"project_notes_local"`
	UnsupportedProjects bool           `json:"-"`
}

// Status reports this profile's sync state.
func (s *Store) Status(ctx context.Context) (SyncStatus, error) {
	st := SyncStatus{Notes: map[string]int{}, Queue: map[string]int{}}
	var err error
	if st.SyncSettings, err = s.SyncSettings(ctx); err != nil {
		return st, err
	}
	count := func(q string, into map[string]int) error {
		rows, err := s.db.QueryContext(ctx, q)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var k string
			var n int
			rows.Scan(&k, &n)
			into[k] = n
		}
		return rows.Err()
	}
	if err := count(`SELECT sync_state, count(*) FROM records WHERE visibility = 'visible' GROUP BY sync_state`, st.Notes); err != nil {
		return st, err
	}
	if err := count(`SELECT state, count(*) FROM outbox GROUP BY state`, st.Queue); err != nil {
		return st, err
	}
	s.db.QueryRowContext(ctx, `SELECT count(*) FROM outbox WHERE op = 'forget' AND state IN ('ready','inflight','unapproved')`).Scan(&st.ForgetsPending)
	s.db.QueryRowContext(ctx, `SELECT count(*) FROM replacements WHERE phase IN ('prepared','new_pending','new_accepted','old_forget_pending')`).Scan(&st.EditsInProgress)
	s.db.QueryRowContext(ctx, `SELECT count(*) FROM replacements WHERE phase = 'blocked'`).Scan(&st.EditsBlocked)
	s.db.QueryRowContext(ctx, `SELECT count(*) FROM records WHERE visibility = 'visible' AND scope = 'project'`).Scan(&st.ProjectNotesLocal)
	rows, err := s.db.QueryContext(ctx, `SELECT DISTINCT last_code FROM outbox WHERE state = 'blocked' AND last_code != '' ORDER BY 1`)
	if err == nil {
		for rows.Next() {
			var c string
			rows.Scan(&c)
			st.BlockedCodes = append(st.BlockedCodes, c)
		}
		rows.Close()
	}
	var pull, push int64
	s.db.QueryRowContext(ctx, `SELECT last_pull_at, last_push_at FROM sync_state WHERE singleton = 1`).Scan(&pull, &push)
	if pull > 0 {
		t := time.UnixMilli(pull).UTC()
		st.LastPullAt = &t
	}
	if push > 0 {
		t := time.UnixMilli(push).UTC()
		st.LastPushAt = &t
	}
	return st, nil
}

// HasSyncWork reports whether a sync worker has anything to do for this
// profile: sync on, an erase to deliver, or forgets of already-synced
// notes to send.
func (s *Store) HasSyncWork(ctx context.Context) (bool, error) {
	set, err := loadSettings(ctx, s.db)
	if err != nil || s.profile.IsLocal() {
		return false, err
	}
	if set.Enabled || set.ErasePending {
		return true, nil
	}
	var n int
	err = s.db.QueryRowContext(ctx, `SELECT count(*) FROM outbox WHERE op = 'forget' AND state IN ('ready','inflight')`).Scan(&n)
	return n > 0 && set.EverEnabled, err
}

// ExportRecord is one exported note.
type ExportRecord struct {
	ID        string   `json:"id"`
	Title     string   `json:"title"`
	Body      string   `json:"body"`
	Tags      []string `json:"tags"`
	Scope     string   `json:"scope"`
	Kind      string   `json:"kind"`
	Origin    string   `json:"origin"`
	Source    string   `json:"source,omitempty"`
	CreatedAt string   `json:"created_at"`
	SyncState string   `json:"sync_state"`
	Profile   string   `json:"profile"`
}

// Export writes this profile's visible notes as NDJSON (one ExportRecord
// per line) and returns the count. It reads only this store.
func (s *Store) Export(ctx context.Context, w io.Writer, profileLabel string) (int, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT `+noteCols+` FROM records r WHERE r.visibility = 'visible' ORDER BY r.created_at, r.rowid`)
	if err != nil {
		return 0, err
	}
	defer rows.Close()
	enc := json.NewEncoder(w)
	n := 0
	for rows.Next() {
		nt, err := scanNote(rows)
		if err != nil {
			return n, err
		}
		if err := enc.Encode(ExportRecord{ID: nt.ID, Title: nt.Title, Body: nt.Body, Tags: nt.Tags, Scope: nt.Scope, Kind: nt.Kind,
			Origin: nt.Origin, Source: nt.Source, CreatedAt: nt.CreatedAt.Format(time.RFC3339), SyncState: nt.SyncState, Profile: profileLabel}); err != nil {
			return n, err
		}
		n++
	}
	return n, rows.Err()
}

// HeldOp is one change waiting for a human approval, as an approval
// request lists it: an unapproved outbox op (OpID > 0, its payload is
// immutable), or a note hidden by a deletion made without approval (Op
// "delete_note", OpID 0). Hash binds a listed note to its content.
type HeldOp struct {
	OpID     int64  `json:"op_id,omitempty"`
	RecordID string `json:"record_id"`
	Op       string `json:"op"`
	Title    string `json:"title,omitempty"`
	Hash     string `json:"content_hash,omitempty"`
	// Body is the exact text the op uploads (or, for a held deletion,
	// deletes), shown on the approval page. When set, Hash is its
	// ContentHash, so the approved payload binds the full text.
	Body string `json:"body,omitempty"`
}

// OpDeleteNote is a held local deletion (the note is hidden, not deleted).
const OpDeleteNote = "delete_note"

// HeldOps lists the changes waiting for a human approval.
func (s *Store) HeldOps(ctx context.Context) ([]HeldOp, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT o.op_id, o.record_id, o.op, coalesce(r.title, ''), '', o.payload, '' FROM outbox o LEFT JOIN records r ON r.id = o.record_id
WHERE o.state = 'unapproved'
UNION ALL
SELECT 0, id, 'delete_note', title, content_hash, NULL, content FROM records WHERE visibility = 'forget_pending'
ORDER BY 1, 2`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []HeldOp
	for rows.Next() {
		var h HeldOp
		var payload []byte
		if err := rows.Scan(&h.OpID, &h.RecordID, &h.Op, &h.Title, &h.Hash, &payload, &h.Body); err != nil {
			return nil, err
		}
		// An upsert shows exactly the text its immutable payload sends.
		var w wireItem
		if len(payload) > 0 && json.Unmarshal(payload, &w) == nil {
			if w.Content != nil {
				h.Body, h.Hash = *w.Content, ContentHash(*w.Content)
			}
			// ...and the title it sends, not the row's current one.
			if f, ok := w.Fields["title"]; ok {
				var t string
				if json.Unmarshal(f.V, &t) == nil {
					h.Title = t
				}
			}
		}
		out = append(out, h)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	rows.Close() // one connection: release it before the next query
	// A forget whose note row is already gone shows the title and content
	// hash kept for it.
	for i := range out {
		if out[i].Op == OpForget && out[i].Title == "" {
			if l, ok := getHeldLabel(ctx, s.db, out[i].OpID); ok {
				out[i].Title, out[i].Hash = l.Title, l.Hash
			}
		}
	}
	return out, nil
}

// LocalSyncCandidates lists the notes turning sync on would upload (the
// approval page shows them; EnableSync queues only these).
func (s *Store) LocalSyncCandidates(ctx context.Context) ([]HeldOp, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT id, title, content_hash, content FROM records
WHERE visibility = 'visible' AND sync_state = 'local' AND scope IN ('global','long_term') ORDER BY created_at, rowid`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []HeldOp
	for rows.Next() {
		h := HeldOp{Op: OpUpsert}
		if err := rows.Scan(&h.RecordID, &h.Title, &h.Hash, &h.Body); err != nil {
			return nil, err
		}
		if !LooksLikeCredentialForSync(h.Title) {
			out = append(out, h)
		}
	}
	return out, rows.Err()
}

// HideForForget is a deletion made without approval on an account: the
// note leaves retrieval at once and the deletion waits, reversibly, for
// approval (ApproveOps) or reversal (Revert).
func (s *Store) HideForForget(ctx context.Context, id string) (string, error) {
	err := s.tx(ctx, func(tx *sql.Tx) error {
		id = resolveID(ctx, tx, id)
		r, ok, err := loadRecord(ctx, tx, id)
		if err != nil {
			return err
		}
		if !ok || r.Visibility != "visible" {
			return ErrNotFound
		}
		_, err = tx.ExecContext(ctx, `UPDATE records SET visibility = 'forget_pending', updated_at = ? WHERE id = ?`, s.now().UnixMilli(), id)
		return err
	})
	return id, err
}

// ApproveOps releases exactly the listed held ops (an approved
// memory.sync_approve request), idempotently per key. Ops that are no
// longer held (deleted or already released) are skipped.
func (s *Store) ApproveOps(ctx context.Context, ops []HeldOp, key string) (int, error) {
	n := 0
	err := s.tx(ctx, func(tx *sql.Tx) error {
		if key != "" {
			if prev, ok, err := appliedResult(ctx, tx, key); err != nil || ok {
				if ok {
					var r struct{ Queued int }
					json.Unmarshal(prev, &r)
					n = r.Queued
				}
				return err
			}
		}
		for _, op := range ops {
			if op.Op == OpDeleteNote {
				r, ok, err := loadRecord(ctx, tx, op.RecordID)
				if err != nil {
					return err
				}
				if !ok || r.Visibility != "forget_pending" || r.Hash != op.Hash {
					continue
				}
				h, err := tickHLC(ctx, tx, s.now(), s.device)
				if err != nil {
					return err
				}
				if _, err := s.forgetOne(ctx, tx, r, string(h), true); err != nil {
					return err
				}
				n++
				continue
			}
			if op.Op == OpUpsert && op.Hash != "" {
				// The text the user approved must be the text this op
				// sends.
				var payload []byte
				if err := tx.QueryRowContext(ctx, `SELECT payload FROM outbox WHERE op_id = ? AND record_id = ?`, op.OpID, op.RecordID).Scan(&payload); err != nil {
					if errors.Is(err, sql.ErrNoRows) {
						continue
					}
					return err
				}
				var w wireItem
				if json.Unmarshal(payload, &w) != nil || w.Content == nil || ContentHash(*w.Content) != op.Hash {
					continue
				}
			}
			res, err := tx.ExecContext(ctx, `UPDATE outbox SET state = 'ready' WHERE op_id = ? AND record_id = ? AND state = 'unapproved'`, op.OpID, op.RecordID)
			if err != nil {
				return err
			}
			c, _ := res.RowsAffected()
			n += int(c)
			if _, err := tx.ExecContext(ctx, `UPDATE records SET sync_state = 'pending', sync_code = '' WHERE id = ? AND sync_state = 'unapproved'
AND NOT EXISTS (SELECT 1 FROM outbox WHERE record_id = ? AND state = 'unapproved')`, op.RecordID, op.RecordID); err != nil {
				return err
			}
		}
		if key != "" {
			return recordApplied(ctx, tx, key, "memory.sync_approve", map[string]any{"status": "done", "queued": n}, s.now().UnixMilli())
		}
		return nil
	})
	return n, err
}

// AppliedResult returns the stored result of an applied request key.
func (s *Store) AppliedResult(ctx context.Context, key string) ([]byte, bool, error) {
	var b []byte
	err := s.db.QueryRowContext(ctx, `SELECT result FROM applied_requests WHERE request_key = ?`, key).Scan(&b)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, false, nil
	}
	return b, err == nil, err
}

// RecordApplied records key as applied with result (operations spanning
// stores, such as adopt). A key already recorded is kept.
func (s *Store) RecordApplied(ctx context.Context, key, op string, result any) error {
	return s.tx(ctx, func(tx *sql.Tx) error {
		if _, ok, err := appliedResult(ctx, tx, key); err != nil || ok {
			return err
		}
		return recordApplied(ctx, tx, key, op, result, s.now().UnixMilli())
	})
}

// FleetMayHold reports whether Kameas Fleet may hold note id (it came
// from Fleet, was acknowledged, or an upsert for it may have been sent).
// It only reads.
func (s *Store) FleetMayHold(ctx context.Context, id string) (bool, error) {
	tx, err := s.db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return false, err
	}
	defer tx.Rollback()
	r, ok, err := loadRecord(ctx, tx, resolveID(ctx, tx, id))
	if err != nil || !ok {
		return false, err
	}
	return remoteMayHave(ctx, tx, r)
}

// StateToken is the profile clock's last value. Every write advances it,
// so it tells "the same request again" from "the same request after the
// store changed".
func (s *Store) StateToken(ctx context.Context) (string, error) {
	var t string
	err := s.db.QueryRowContext(ctx, `SELECT last_hlc FROM sync_state WHERE singleton = 1`).Scan(&t)
	return t, err
}
