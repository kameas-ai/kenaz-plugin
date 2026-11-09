package memory

import (
	"context"
	"database/sql"
	"encoding/json"
	"strconv"
	"time"
)

// Automatic notes reach Fleet in the background: capture saves them on this device,
// and the sync worker queues them while the member's standing grant holds (or a member
// approved the exact list by turning sync on). That keeps capture itself away from
// anything that sends, and it covers notes saved before sync was on.
//
// A Fleet can refuse them for reasons outside this device: its flag for these kinds is
// off, or an organization turned capture off. They wait, with no loss. So that waiting
// costs nothing, a batch carries only one such note until Fleet has taken one, and
// after a refusal none at all until the next try, an hour on and doubling to a day.

// Classes of note a Fleet may not be taking.
const (
	syncClassAuto    = "auto"
	syncClassProject = "project"
)

const (
	minClassHold = time.Hour
	maxClassHold = 24 * time.Hour

	// autoQueueDepth bounds how many automatic notes wait in the outbox at once.
	autoQueueDepth = 100
	// grantReservedForSaves is the part of the hourly cap automatic notes leave for
	// notes saved on purpose, which otherwise wait for an approval.
	grantReservedForSaves = 100
)

// classState is what this device knows about Fleet taking a class of note.
type classState struct {
	// Confirmed: Fleet has taken one.
	Confirmed bool `json:"confirmed,omitempty"`
	// Code is the refusal being waited out, and Until when to try again.
	Code  string `json:"code,omitempty"`
	Until int64  `json:"until,omitempty"`
	Step  int    `json:"step,omitempty"`
}

func classKey(class string) string { return "sync_class:" + class }

func loadClass(ctx context.Context, q rowQuerier, class string) classState {
	v, err := readMeta(ctx, q, classKey(class))
	var st classState
	if err == nil && v != "" {
		_ = json.Unmarshal([]byte(v), &st)
	}
	return st
}

func saveClass(ctx context.Context, tx *sql.Tx, class string, st classState) error {
	b, _ := json.Marshal(st)
	return setMeta(ctx, tx, classKey(class), string(b))
}

// holding reports a refusal still being waited out.
func (st classState) holding(now int64) bool { return st.Code != "" && st.Until > now }

// probing reports that the next note is a test of whether Fleet takes this class.
func (st classState) probing() bool { return !st.Confirmed || st.Code != "" }

// holdClass records a refusal. Several in one batch count once.
func holdClass(ctx context.Context, tx *sql.Tx, class, code string, now time.Time) error {
	st := loadClass(ctx, tx, class)
	if st.Code == code && st.holding(now.UnixMilli()) {
		return nil
	}
	wait := minClassHold << st.Step
	if wait > maxClassHold || wait <= 0 {
		wait = maxClassHold
	} else {
		st.Step++
	}
	st.Code, st.Until, st.Confirmed = code, now.Add(wait).UnixMilli(), false
	return saveClass(ctx, tx, class, st)
}

func confirmClass(ctx context.Context, tx *sql.Tx, class string) error {
	st := loadClass(ctx, tx, class)
	if st.Confirmed && st.Code == "" {
		return nil
	}
	return saveClass(ctx, tx, class, classState{Confirmed: true})
}

// classesOf names the classes an upsert of such a note belongs to.
func classesOf(kind, scope string) []string {
	var out []string
	if IsAuto(kind) {
		out = append(out, syncClassAuto)
	}
	if scope == ScopeProject {
		out = append(out, syncClassProject)
	}
	return out
}

// refusalClass reports a refusal that is about a whole class of note, rather than this
// one, so that the note waits instead of being refused for good. Fleet answers
// invalid_field on kind while its flag for these kinds is off.
func refusalClass(it wireItem, r PushResult, scope string) string {
	switch {
	case r.Code == "capture_off" && IsAuto(it.Kind):
		return syncClassAuto
	case r.Code == "invalid_field" && r.Field == "kind" && IsAuto(it.Kind):
		return syncClassAuto
	case scope != ScopeProject:
		return ""
	case r.Code == "scope_not_supported_yet", r.Code == "scope_not_enabled", r.Code == "consent_upgrade_required":
		return syncClassProject
	}
	return ""
}

// holdOp puts an op back to wait out a refusal, with its attempts as they were.
func (s *Store) holdOp(ctx context.Context, tx *sql.Tx, class string, op OutboxOp, code string, now time.Time, out *PushOutcome) error {
	if err := holdClass(ctx, tx, class, code, now); err != nil {
		return err
	}
	out.Retry++
	if _, err := tx.ExecContext(ctx, `UPDATE outbox SET state = 'ready', next_attempt_at = 0, attempt = max(attempt - 1, 0), last_code = ? WHERE op_id = ?`, code, op.OpID); err != nil {
		return err
	}
	return setSync(ctx, tx, op.RecordID, StatePending, code)
}

// classGate decides which upserts a batch carries.
type classGate struct {
	ctx    context.Context
	q      rowQuerier
	now    int64
	seen   map[string]classState
	probed map[string]bool
}

func newClassGate(ctx context.Context, q rowQuerier, now int64) *classGate {
	return &classGate{ctx: ctx, q: q, now: now, seen: map[string]classState{}, probed: map[string]bool{}}
}

func (g *classGate) allow(classes []string) bool {
	var probes []string
	for _, class := range classes {
		st, ok := g.seen[class]
		if !ok {
			st = loadClass(g.ctx, g.q, class)
			g.seen[class] = st
		}
		if st.holding(g.now) {
			return false
		}
		if st.probing() {
			if g.probed[class] {
				return false
			}
			probes = append(probes, class)
		}
	}
	for _, class := range probes {
		g.probed[class] = true
	}
	return true
}

// NoteCaptureSwitch records whether the organization has capture off, which Fleet says
// without being sent a note. While it does, none is sent; when it no longer does, they
// go again at once.
func (s *Store) NoteCaptureSwitch(ctx context.Context, off bool) error {
	now := s.now()
	st := loadClass(ctx, s.db, syncClassAuto)
	switch {
	case off && st.Code == "capture_off" && st.holding(now.UnixMilli()):
		return nil
	case !off && st.Code != "capture_off":
		return nil
	}
	return s.tx(ctx, func(tx *sql.Tx) error {
		if off {
			return holdClass(ctx, tx, syncClassAuto, "capture_off", now)
		}
		return saveClass(ctx, tx, syncClassAuto, classState{})
	})
}

// syncableNotes is the SQL for notes that can be queued: in a scope Fleet syncs, and for
// a project, one whose key can leave the device and hasn't been refused for good.
func syncableNotes() string {
	if ProjectScopeRelease {
		return `(scope IN ('global','long_term') OR (scope = 'project' AND project_key LIKE 'rk1-%' AND sync_code NOT IN ('project_deleted','project_not_found')))`
	}
	return `scope IN ('global','long_term')`
}

// grantRoom reports how many more notes this hour's cap leaves automatic notes, and
// whether the grant stands.
func (s *Store) grantRoom(ctx context.Context, tx *sql.Tx, set settings) (room, used int, granted bool, err error) {
	v, err := readMeta(ctx, tx, metaSaveGrant)
	if err != nil || v == "" {
		return 0, 0, false, err
	}
	notice, err := readMeta(ctx, tx, metaSaveGrantNotice)
	if err != nil || notice != set.ConsentVersion {
		return 0, 0, false, err
	}
	hour := strconv.FormatInt(s.now().Unix()/3600, 10)
	h, err := readMeta(ctx, tx, metaSaveGrantHour)
	if err != nil {
		return 0, 0, false, err
	}
	if h == hour {
		c, err := readMeta(ctx, tx, metaSaveGrantCount)
		if err != nil {
			return 0, 0, false, err
		}
		used, _ = strconv.Atoi(c)
	}
	return max(MaxGrantedSavesPerHour-grantReservedForSaves-used, 0), used, true, nil
}

// QueueAutoBacklog queues automatic notes waiting on this device, newest first, within
// the standing grant's hourly cap. Imported notes a person let through with
// AllowImportUpload don't count against the cap, and go after everything else: the cap is
// there for an agent running away, and an import isn't that. It returns how many it queued.
func (s *Store) QueueAutoBacklog(ctx context.Context, l Lease) (int, error) {
	queued := 0
	err := s.tx(ctx, func(tx *sql.Tx) error {
		if err := checkLease(ctx, tx, l); err != nil {
			return err
		}
		set, err := loadSettings(ctx, tx)
		if err != nil || !set.Enabled || set.ErasePending || set.ResetPhase != "" || s.profile.IsLocal() {
			return err
		}
		room, used, granted, err := s.grantRoom(ctx, tx, set)
		if err != nil || !granted {
			return err
		}
		st := loadClass(ctx, tx, syncClassAuto)
		if st.holding(s.now().UnixMilli()) {
			return nil
		}
		// With project notes held up too, only notes outside a project can go.
		where := syncableNotes()
		if loadClass(ctx, tx, syncClassProject).holding(s.now().UnixMilli()) {
			where = `scope IN ('global','long_term')`
		}
		// A note waiting out a hold on project notes can't be sent, so it doesn't count: it
		// must not stop one outside a project going.
		counted := `o.op = 'upsert' AND o.state = 'ready' AND r.kind IN ('observation','session_summary')`
		if loadClass(ctx, tx, syncClassProject).holding(s.now().UnixMilli()) {
			counted += ` AND r.scope != 'project'`
		}
		var waiting int
		if err := tx.QueryRowContext(ctx, `SELECT count(*) FROM outbox o JOIN records r ON r.id = o.record_id WHERE `+counted).Scan(&waiting); err != nil {
			return err
		}
		depth := autoQueueDepth - waiting
		if st.probing() {
			// One at a time until Fleet has taken one: a refusal leaves one in the queue.
			depth = min(depth, 1-waiting)
		}
		queueSome := func(which string, limit int, args ...any) (int, error) {
			if limit <= 0 {
				return 0, nil
			}
			rows, err := tx.QueryContext(ctx, `SELECT `+recordCols+` FROM records WHERE kind IN ('observation','session_summary') AND visibility = 'visible' AND sync_state = 'local'
AND `+where+` AND `+which+` AND NOT EXISTS (SELECT 1 FROM outbox o WHERE o.record_id = records.id AND o.op = 'upsert') ORDER BY created_at DESC, rowid DESC LIMIT ?`, append(args, limit)...)
			if err != nil {
				return 0, err
			}
			var batch []record
			for rows.Next() {
				r, err := scanRecord(rows)
				if err != nil {
					rows.Close()
					return 0, err
				}
				batch = append(batch, r)
			}
			rows.Close()
			if err := rows.Err(); err != nil {
				return 0, err
			}
			n := 0
			for i := range batch {
				done, err := s.queueExisting(ctx, tx, &batch[i])
				if err != nil {
					return n, err
				}
				if done {
					n++
				}
			}
			return n, nil
		}
		imported := `source = '` + SourceImport + `'`
		capped, err := queueSome(`source != '`+SourceImport+`'`, min(room, depth))
		if err != nil {
			return err
		}
		depth -= capped
		through, err := readMeta(ctx, tx, metaImportUploadThrough)
		if err != nil {
			return err
		}
		free := 0
		if through != "" {
			if free, err = queueSome(imported+` AND rowid <= ?`, depth, through); err != nil {
				return err
			}
			depth -= free
		}
		more, err := queueSome(imported, min(room-capped, depth))
		if err != nil {
			return err
		}
		capped += more
		queued = capped + free
		if capped == 0 {
			return nil
		}
		if err := setMeta(ctx, tx, metaSaveGrantHour, strconv.FormatInt(s.now().Unix()/3600, 10)); err != nil {
			return err
		}
		return setMeta(ctx, tx, metaSaveGrantCount, strconv.Itoa(used+capped))
	})
	return queued, err
}

// unqueueAuto ends what the grant queued and hasn't gone: automatic notes go back to
// waiting on this device, and an edit made of one goes back to waiting with it. It only
// touches creates of notes Fleet can't hold. A note that may be on Fleet keeps its ops,
// or a later forget wouldn't know to go there. An op that went out and came back with a
// definite refusal counts as not gone: holdOp leaves attempt at 0, and an unknown outcome
// always leaves it above that. Unless an edit made since depends on it: without the
// create that edit could never land, so the create stays queued and goes when Fleet takes
// it. It isn't in the count returned, because Fleet refused it.
//
// It returns how many notes it left queued because Fleet may already hold them: their
// outcome is unknown, so they finish sending, and a member stopping sync is told.
func unqueueAuto(ctx context.Context, tx *sql.Tx) (int, error) {
	rows, err := tx.QueryContext(ctx, `SELECT o.op_id, o.record_id, o.payload FROM outbox o JOIN records r ON r.id = o.record_id
WHERE o.op = 'upsert' AND o.state = 'ready'
AND (o.ever_sent = 0 OR (o.attempt = 0 AND NOT EXISTS (SELECT 1 FROM outbox y WHERE y.record_id = o.record_id AND y.op = 'upsert' AND y.op_id != o.op_id)))
AND r.kind IN ('observation','session_summary') AND r.server_seq = '' AND r.provenance != 'fleet' AND r.sync_state != 'synced'
AND NOT EXISTS (SELECT 1 FROM outbox x WHERE x.record_id = o.record_id AND x.ever_sent = 1 AND x.attempt > 0)`)
	if err != nil {
		return 0, err
	}
	type queuedOp struct {
		op int64
		id string
	}
	var ops []queuedOp
	for rows.Next() {
		var q queuedOp
		var payload []byte
		if err := rows.Scan(&q.op, &q.id, &payload); err != nil {
			rows.Close()
			return 0, err
		}
		var it wireItem
		if json.Unmarshal(payload, &it) == nil && it.Content != nil {
			ops = append(ops, q)
		}
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return 0, err
	}
	for _, q := range ops {
		for _, stmt := range []struct {
			sql  string
			args []any
		}{
			{`UPDATE outbox SET depends_on_op = 0 WHERE depends_on_op = ? AND state = 'held'`, []any{q.op}},
			{`UPDATE replacements SET new_upsert_op = 0 WHERE new_upsert_op = ? AND phase = 'prepared'`, []any{q.op}},
			{`DELETE FROM outbox WHERE op_id = ?`, []any{q.op}},
			{`UPDATE records SET sync_state = ?, sync_code = ? WHERE id = ?`, []any{StateLocal, CodeAutoWaits, q.id}},
		} {
			if _, err := tx.ExecContext(ctx, stmt.sql, stmt.args...); err != nil {
				return 0, err
			}
		}
	}
	// A create kept for an edit that waits for approval does go to Fleet, so the note
	// reads as queued. The edit still waits: ApproveOps goes by its op, not by this state.
	if _, err := tx.ExecContext(ctx, `UPDATE records SET sync_state = 'pending' WHERE sync_state = 'unapproved' AND kind IN ('observation','session_summary')
AND id IN (SELECT record_id FROM outbox WHERE op = 'upsert' AND state = 'ready' AND ever_sent = 1 AND attempt = 0)
AND id IN (SELECT record_id FROM outbox WHERE op = 'upsert' AND state = 'unapproved')`); err != nil {
		return 0, err
	}
	var unknown int
	err = tx.QueryRowContext(ctx, `SELECT count(DISTINCT o.record_id) FROM outbox o JOIN records r ON r.id = o.record_id
WHERE o.op = 'upsert' AND o.state IN ('ready','inflight') AND r.kind IN ('observation','session_summary')
AND r.server_seq = '' AND r.provenance != 'fleet' AND r.sync_state != 'synced'
AND EXISTS (SELECT 1 FROM outbox x WHERE x.record_id = o.record_id AND x.ever_sent = 1 AND x.attempt > 0)`).Scan(&unknown)
	return unknown, err
}

// queueExisting queues a note already saved, whose sending has been authorized, and
// links an edit made before then to the create it queued.
func (s *Store) queueExisting(ctx context.Context, tx *sql.Tx, r *record) (bool, error) {
	r.queueAuto = true
	st, _, op, err := s.makeEligible(ctx, tx, r, true, 0)
	if err != nil || st != StatePending {
		return false, err
	}
	if _, err := tx.ExecContext(ctx, `UPDATE replacements SET new_upsert_op = ? WHERE new_id = ? AND phase = 'prepared' AND new_upsert_op = 0`, op, r.ID); err != nil {
		return false, err
	}
	if _, err := tx.ExecContext(ctx, `UPDATE outbox SET depends_on_op = ? WHERE op_id IN (SELECT old_forget_op FROM replacements WHERE new_id = ? AND phase = 'prepared')`, op, r.ID); err != nil {
		return false, err
	}
	return true, nil
}
