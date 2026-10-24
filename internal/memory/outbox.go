package memory

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"
)

// record is one records row.
type record struct {
	ID, Content, Hash, Title, Kind, Scope, ProjectKey, ProjectRoot, Tags, Source, Origin string
	CreatedAt, UpdatedAt                                                                 int64
	CreatedHLC, TitleHLC, ScopeHLC, PinnedHLC                                            string
	Pinned                                                                               bool
	Visibility, SyncState, SyncCode, ServerSeq                                           string
}

const recordCols = `id, content, content_hash, title, kind, scope, project_key, project_root, tags, source, provenance, created_at, updated_at, created_hlc, title_hlc, scope_hlc, pinned_hlc, pinned, visibility, sync_state, sync_code, server_seq`

func scanRecord(row interface{ Scan(...any) error }) (record, error) {
	var r record
	err := row.Scan(&r.ID, &r.Content, &r.Hash, &r.Title, &r.Kind, &r.Scope, &r.ProjectKey, &r.ProjectRoot, &r.Tags, &r.Source, &r.Origin,
		&r.CreatedAt, &r.UpdatedAt, &r.CreatedHLC, &r.TitleHLC, &r.ScopeHLC, &r.PinnedHLC, &r.Pinned, &r.Visibility, &r.SyncState, &r.SyncCode, &r.ServerSeq)
	return r, err
}

func loadRecord(ctx context.Context, q rowQuerier, id string) (record, bool, error) {
	r, err := scanRecord(q.QueryRowContext(ctx, `SELECT `+recordCols+` FROM records WHERE id = ?`, id))
	if errors.Is(err, sql.ErrNoRows) {
		return record{}, false, nil
	}
	return r, err == nil, err
}

func insertRecord(ctx context.Context, tx *sql.Tx, r record, requestKey string) error {
	var key any
	if requestKey != "" {
		key = requestKey
	}
	if r.SyncState == "" {
		r.SyncState = StateLocal
	}
	_, err := tx.ExecContext(ctx, `INSERT INTO records(id, request_key, content, content_hash, title, kind, scope, project_key, project_root, tags, source, provenance, created_at, updated_at, created_hlc, title_hlc, scope_hlc, pinned_hlc, pinned, visibility, sync_state, sync_code, server_seq)
VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`,
		r.ID, key, r.Content, r.Hash, r.Title, r.Kind, r.Scope, r.ProjectKey, r.ProjectRoot, r.Tags, r.Source, r.Origin,
		r.CreatedAt, r.UpdatedAt, r.CreatedHLC, r.TitleHLC, r.ScopeHLC, r.PinnedHLC, r.Pinned, r.Visibility, r.SyncState, r.SyncCode, r.ServerSeq)
	return err
}

func setSync(ctx context.Context, tx *sql.Tx, id, state, code string) error {
	_, err := tx.ExecContext(ctx, `UPDATE records SET sync_state = ?, sync_code = ? WHERE id = ?`, state, code, id)
	return err
}

// syncedScope reports whether Fleet syncs scope.
func syncedScope(scope string) bool { return scope == ScopeGlobal || scope == ScopeLongTerm }

// settings is the sync_state row, as the store's transactions need it.
type settings struct {
	Enabled, EverEnabled, ErasePending bool
	ConsentVersion, Cursor, ResetPhase string
	Generation                         int64
}

func loadSettings(ctx context.Context, q rowQuerier) (settings, error) {
	var s settings
	err := q.QueryRowContext(ctx, `SELECT enabled, ever_enabled, erase_pending, consent_version, cursor, reset_phase, profile_generation FROM sync_state WHERE singleton = 1`).
		Scan(&s.Enabled, &s.EverEnabled, &s.ErasePending, &s.ConsentVersion, &s.Cursor, &s.ResetPhase, &s.Generation)
	return s, err
}

// ---- outbox ----

// Outbox op states.
const (
	opReady      = "ready"      // eligible to send
	opInflight   = "inflight"   // leased to a worker; result unknown
	opUnapproved = "unapproved" // waits for a human to authorize it
	opHeld       = "held"       // waits on a replacement phase
	opBlocked    = "blocked"    // permanently rejected; visible, never resent
)

// Ops.
const (
	OpUpsert = "upsert"
	OpForget = "forget"
)

// wireField and wireItem mirror Fleet's push item exactly (field names,
// omitempty). memsync decodes these payloads into fleetapi types; a test
// there pins that both encodings agree. The payload is built here, in
// public code, and stored immutably: a retry resends the same bytes.
type wireField struct {
	V   json.RawMessage `json:"v"`
	HLC string          `json:"hlc"`
}

type wireItem struct {
	Op          string               `json:"op"`
	ID          string               `json:"id"`
	Content     *string              `json:"content,omitempty"`
	ContentHash string               `json:"content_hash,omitempty"`
	Kind        string               `json:"kind,omitempty"`
	Source      string               `json:"source,omitempty"`
	CreatedAt   string               `json:"created_at,omitempty"`
	Fields      map[string]wireField `json:"fields,omitempty"`
	HLC         string               `json:"hlc,omitempty"`
}

func jsonValue(v any) json.RawMessage {
	b, _ := json.Marshal(v)
	return b
}

// createPayload is the upsert that creates r on Fleet: content, hash,
// kind, source app, creation time, and the title and scope registers.
func createPayload(r record) []byte {
	c := r.Content
	it := wireItem{Op: OpUpsert, ID: r.ID, Content: &c, ContentHash: r.Hash, Kind: r.Kind, Source: r.Source,
		CreatedAt: time.UnixMilli(r.CreatedAt).UTC().Format("2006-01-02T15:04:05.000Z07:00"),
		Fields: map[string]wireField{
			"scope_kind": {V: jsonValue(r.Scope), HLC: r.ScopeHLC},
		}}
	if r.Title != "" {
		it.Fields["title"] = wireField{V: jsonValue(r.Title), HLC: r.TitleHLC}
	}
	b, _ := json.Marshal(it)
	return b
}

func titlePayload(id, title, hlc string) []byte {
	b, _ := json.Marshal(wireItem{Op: OpUpsert, ID: id, Fields: map[string]wireField{"title": {V: jsonValue(title), HLC: hlc}}})
	return b
}

func forgetPayload(id, hlc string) []byte {
	b, _ := json.Marshal(wireItem{Op: OpForget, ID: id, HLC: hlc})
	return b
}

func enqueue(ctx context.Context, tx *sql.Tx, recordID, op string, payload []byte, dependsOn int64, state string, now int64) (int64, error) {
	res, err := tx.ExecContext(ctx, `INSERT INTO outbox(request_key, record_id, op, payload, depends_on_op, state, created_at) VALUES (?,?,?,?,?,?,?)`,
		newKey(), recordID, op, payload, dependsOn, state, now)
	if err != nil {
		return 0, err
	}
	return res.LastInsertId()
}

// makeEligible decides r's sync state inside the transaction that wrote it
// and, when it is eligible, queues its create upsert. A note is queued only
// when all hold: an account profile, sync on, a scope Fleet syncs, and the
// on-device credential scan passes. Without a human authorization the op
// is queued as unapproved and is not sent.
func (s *Store) makeEligible(ctx context.Context, tx *sql.Tx, r *record, approved bool, dependsOn int64) (state, code string, opID int64, err error) {
	set, err := loadSettings(ctx, tx)
	if err != nil {
		return "", "", 0, err
	}
	switch {
	case s.profile.IsLocal() || !set.Enabled:
		state = StateLocal
	case !syncedScope(r.Scope):
		state, code = StateLocal, "scope_not_supported_yet"
	case LooksLikeCredentialForSync(r.Title, r.Content, r.Source):
		state, code = StateRejected, "secret_detected"
	default:
		opState := opReady
		state = StatePending
		if !approved {
			opState, state = opUnapproved, StateUnapproved
		}
		if opID, err = enqueue(ctx, tx, r.ID, OpUpsert, createPayload(*r), dependsOn, opState, s.now().UnixMilli()); err != nil {
			return "", "", 0, err
		}
	}
	r.SyncState, r.SyncCode = state, code
	return state, code, opID, setSync(ctx, tx, r.ID, state, code)
}

// remoteMayHave reports whether Fleet may hold id: it came from Fleet, was
// acknowledged, or an op for it was ever handed to a sender.
func remoteMayHave(ctx context.Context, tx *sql.Tx, r record) (bool, error) {
	if r.Origin == "fleet" || r.SyncState == StateSynced || r.ServerSeq != "" {
		return true, nil
	}
	var n int
	err := tx.QueryRowContext(ctx, `SELECT count(*) FROM outbox WHERE record_id = ? AND ever_sent = 1`, r.ID).Scan(&n)
	return n > 0, err
}

// dropUnsentOps removes id's queued ops that are not in flight. Callers
// decide first (remoteMayHave) whether a forget must follow.
func dropUnsentOps(ctx context.Context, tx *sql.Tx, id string) error {
	if err := dropHeldLabels(ctx, tx, id); err != nil {
		return err
	}
	_, err := tx.ExecContext(ctx, `DELETE FROM outbox WHERE record_id = ? AND (ever_sent = 0 OR state IN ('ready','unapproved','held','blocked'))`, id)
	return err
}

// ---- leases ----

// Lease is a sync worker's exclusive right to send for this profile. A
// new holder bumps the profile generation, so a stale worker (an old
// daemon, or a worker for an account that has since signed out) can no
// longer apply results or lease ops.
type Lease struct {
	Holder     string
	Generation int64
}

// ErrLeaseHeld means another live worker holds the lease.
var ErrLeaseHeld = errors.New("another Kenaz relay is syncing this memory profile")

// ErrLeaseLost means the lease was taken over or invalidated.
var ErrLeaseLost = errors.New("memory sync lease lost")

// AcquireLease takes or renews the profile's send lease for ttl. Taking it
// over from a dead holder returns that holder's in-flight ops to the queue
// (their outcome is unknown; they stay marked as possibly sent).
func (s *Store) AcquireLease(ctx context.Context, holder string, ttl time.Duration) (Lease, error) {
	var l Lease
	err := s.tx(ctx, func(tx *sql.Tx) error {
		var cur string
		var until, gen int64
		if err := tx.QueryRowContext(ctx, `SELECT lease_holder, lease_until, profile_generation FROM sync_state WHERE singleton = 1`).Scan(&cur, &until, &gen); err != nil {
			return err
		}
		now := s.now().UnixMilli()
		if cur != holder && cur != "" && until > now {
			return ErrLeaseHeld
		}
		if cur != holder {
			gen++
			if _, err := tx.ExecContext(ctx, `UPDATE outbox SET state = 'ready' WHERE state = 'inflight'`); err != nil {
				return err
			}
		}
		l = Lease{Holder: holder, Generation: gen}
		_, err := tx.ExecContext(ctx, `UPDATE sync_state SET lease_holder = ?, lease_until = ?, profile_generation = ? WHERE singleton = 1`, holder, now+ttl.Milliseconds(), gen)
		return err
	})
	return l, err
}

// ReleaseLease gives the lease up (worker stop).
func (s *Store) ReleaseLease(ctx context.Context, l Lease) error {
	return s.tx(ctx, func(tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx, `UPDATE sync_state SET lease_until = 0 WHERE singleton = 1 AND lease_holder = ? AND profile_generation = ?`, l.Holder, l.Generation)
		return err
	})
}

// InvalidateLeases bumps the profile generation (sign-out, account switch)
// so no worker started before can write again.
func (s *Store) InvalidateLeases(ctx context.Context) error {
	return s.tx(ctx, func(tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx, `UPDATE sync_state SET profile_generation = profile_generation + 1, lease_holder = '', lease_until = 0 WHERE singleton = 1`)
		return err
	})
}

func checkLease(ctx context.Context, tx *sql.Tx, l Lease) error {
	var holder string
	var gen int64
	if err := tx.QueryRowContext(ctx, `SELECT lease_holder, profile_generation FROM sync_state WHERE singleton = 1`).Scan(&holder, &gen); err != nil {
		return err
	}
	if holder != l.Holder || gen != l.Generation {
		return ErrLeaseLost
	}
	return nil
}

// ---- push ----

// OutboxOp is one leased op. Payload is the exact item JSON to send.
type OutboxOp struct {
	OpID     int64
	RecordID string
	Op       string
	Payload  []byte
	Attempt  int
}

// PushBatch is what a worker sends next.
type PushBatch struct {
	BaseCursor string
	Ops        []OutboxOp
}

// Batch limits (Fleet: <= 100 items, body <= 2 MiB).
const (
	MaxPushItems = 100
	maxPushBytes = 3 << 19 // 1.5 MiB of items leaves room for the envelope
)

// LeasePushBatch marks up to MaxPushItems ready ops in flight and returns
// them in queue order. Nothing is leased while a forget-all erase or a
// reset is unfinished. While sync is off only forget ops go (Fleet always
// honours them; they remove already-synced notes).
func (s *Store) LeasePushBatch(ctx context.Context, l Lease) (PushBatch, error) {
	return s.leaseBatch(ctx, l, false)
}

// LeaseForgetBatch leases only forget ops (sent through Fleet's
// data-rights forget route while sync is off or the plan lapsed).
func (s *Store) LeaseForgetBatch(ctx context.Context, l Lease) (PushBatch, error) {
	return s.leaseBatch(ctx, l, true)
}

func (s *Store) leaseBatch(ctx context.Context, l Lease, forgetsOnly bool) (PushBatch, error) {
	var b PushBatch
	err := s.tx(ctx, func(tx *sql.Tx) error {
		if err := checkLease(ctx, tx, l); err != nil {
			return err
		}
		set, err := loadSettings(ctx, tx)
		if err != nil {
			return err
		}
		if set.ErasePending || set.ResetPhase != "" {
			return nil
		}
		b.BaseCursor = set.Cursor
		q := `SELECT o.op_id, o.record_id, o.op, o.payload, o.attempt FROM outbox o
WHERE o.state = 'ready' AND o.next_attempt_at <= ?
  AND NOT EXISTS (SELECT 1 FROM outbox d WHERE d.op_id = o.depends_on_op AND o.depends_on_op != 0)`
		if !set.Enabled || forgetsOnly {
			q += ` AND o.op = 'forget'`
		}
		q += ` ORDER BY o.op_id LIMIT ?`
		rows, err := tx.QueryContext(ctx, q, s.now().UnixMilli(), MaxPushItems)
		if err != nil {
			return err
		}
		size := 0
		for rows.Next() {
			var op OutboxOp
			if err := rows.Scan(&op.OpID, &op.RecordID, &op.Op, &op.Payload, &op.Attempt); err != nil {
				rows.Close()
				return err
			}
			if size+len(op.Payload) > maxPushBytes && len(b.Ops) > 0 {
				break
			}
			size += len(op.Payload)
			b.Ops = append(b.Ops, op)
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return err
		}
		for i := range b.Ops {
			b.Ops[i].Attempt++
			if _, err := tx.ExecContext(ctx, `UPDATE outbox SET state = 'inflight', attempt = attempt + 1, ever_sent = 1 WHERE op_id = ?`, b.Ops[i].OpID); err != nil {
				return err
			}
			if _, err := tx.ExecContext(ctx, `UPDATE replacements SET phase = 'new_pending' WHERE new_upsert_op = ? AND phase = 'prepared'`, b.Ops[i].OpID); err != nil {
				return err
			}
		}
		return nil
	})
	return b, err
}

// Backoff bounds for per-op retries.
const (
	MaxOpAttempts = 12
	minOpBackoff  = 5 * time.Second
	maxOpBackoff  = 30 * time.Minute
)

// OpBackoff is the delay before retry n (1-based) of one op: doubling from
// 5 s, capped at 30 min.
func OpBackoff(n int) time.Duration {
	d := minOpBackoff
	for i := 1; i < n && d < maxOpBackoff; i++ {
		d *= 2
	}
	if d > maxOpBackoff {
		d = maxOpBackoff
	}
	return d
}

// ReturnUnknown puts in-flight ops whose outcome is unknown (network
// error, 5xx, 429, cancellation) back in the queue after delay. They stay
// marked as possibly sent, so a later delete still sends a forget.
func (s *Store) ReturnUnknown(ctx context.Context, l Lease, ops []OutboxOp, delay time.Duration) error {
	return s.tx(ctx, func(tx *sql.Tx) error {
		if err := checkLease(ctx, tx, l); err != nil {
			return err
		}
		at := s.now().Add(delay).UnixMilli()
		for _, op := range ops {
			if _, err := tx.ExecContext(ctx, `UPDATE outbox SET state = 'ready', next_attempt_at = ? WHERE op_id = ? AND state = 'inflight'`, at, op.OpID); err != nil {
				return err
			}
		}
		return nil
	})
}

// PushResult is Fleet's per-item outcome, as memsync hands it over.
type PushResult struct {
	ID           string
	Status       string
	Seq          string
	CanonicalID  string
	SupersededBy string
	Code         string
	Field        string
	Retry        *bool
}

// PushOutcome counts what one batch did (no content).
type PushOutcome struct {
	Synced, Merged, Removed, Retry, Blocked, Forgotten int
	// ResyncRequired: Fleet answered resync_required; pull (reset) first.
	ResyncRequired bool
	// SyncDisabled: Fleet says sync is off for this account.
	SyncDisabled bool
	// QuotaExceeded: Fleet's per-user quota refused a create.
	QuotaExceeded bool
}

// ApplyPushResults applies Fleet's per-item results for a leased batch in
// one transaction. Every result is inspected: HTTP 200 alone never marks
// anything synced. A missing or mismatched result counts as unknown.
func (s *Store) ApplyPushResults(ctx context.Context, l Lease, ops []OutboxOp, results []PushResult) (PushOutcome, error) {
	var out PushOutcome
	err := s.tx(ctx, func(tx *sql.Tx) error {
		if err := checkLease(ctx, tx, l); err != nil {
			return err
		}
		now := s.now()
		for i, op := range ops {
			var it wireItem
			_ = json.Unmarshal(op.Payload, &it)
			if i >= len(results) || (results[i].ID != "" && results[i].ID != it.ID) {
				if err := s.retryOp(ctx, tx, op, "no_result", now, &out); err != nil {
					return err
				}
				continue
			}
			if err := s.applyResult(ctx, tx, op, it, results[i], now, &out); err != nil {
				return err
			}
		}
		_, err := tx.ExecContext(ctx, `UPDATE sync_state SET last_push_at = ? WHERE singleton = 1`, now.UnixMilli())
		return err
	})
	return out, err
}

func (s *Store) retryOp(ctx context.Context, tx *sql.Tx, op OutboxOp, code string, now time.Time, out *PushOutcome) error {
	if op.Attempt >= MaxOpAttempts {
		return s.blockOp(ctx, tx, op, code, out)
	}
	out.Retry++
	delay := OpBackoff(op.Attempt)
	if code == "quota_exceeded" && delay < time.Hour {
		delay = time.Hour
	}
	_, err := tx.ExecContext(ctx, `UPDATE outbox SET state = 'ready', next_attempt_at = ?, last_code = ? WHERE op_id = ?`, now.Add(delay).UnixMilli(), code, op.OpID)
	return err
}

func (s *Store) blockOp(ctx context.Context, tx *sql.Tx, op OutboxOp, code string, out *PushOutcome) error {
	out.Blocked++
	if _, err := tx.ExecContext(ctx, `UPDATE outbox SET state = 'blocked', last_code = ? WHERE op_id = ?`, code, op.OpID); err != nil {
		return err
	}
	// Ops that only make sense after this one (a title edit after a
	// refused create) are refused with it, so they never wait forever.
	if _, err := tx.ExecContext(ctx, `UPDATE outbox SET state = 'blocked', last_code = ? WHERE depends_on_op = ? AND state IN ('ready','unapproved')`, code, op.OpID); err != nil {
		return err
	}
	if op.Op == OpUpsert {
		if err := setSync(ctx, tx, op.RecordID, StateRejected, code); err != nil {
			return err
		}
		// A rejected replacement keeps the old synchronized copy (hidden)
		// for correction or reversal; its forget is never released.
		if _, err := tx.ExecContext(ctx, `UPDATE replacements SET phase = 'blocked', code = ? WHERE new_upsert_op = ? AND phase IN ('prepared','new_pending')`, code, op.OpID); err != nil {
			return err
		}
	}
	return nil
}

func doneOp(ctx context.Context, tx *sql.Tx, opID int64) error {
	if _, err := tx.ExecContext(ctx, `DELETE FROM meta WHERE key = ?`, heldLabelKey(opID)); err != nil {
		return err
	}
	_, err := tx.ExecContext(ctx, `DELETE FROM outbox WHERE op_id = ?`, opID)
	return err
}

// Held labels: the title and content hash of a note whose unapproved
// forget op outlives its row, for the approval page. Device-only; removed
// with the op.
func heldLabelKey(opID int64) string { return fmt.Sprintf("held_label:%d", opID) }

type heldLabel struct {
	Title string `json:"title"`
	Hash  string `json:"content_hash"`
}

func setHeldLabel(ctx context.Context, tx *sql.Tx, opID int64, title, hash string) error {
	b, _ := json.Marshal(heldLabel{Title: title, Hash: hash})
	_, err := tx.ExecContext(ctx, `INSERT INTO meta(key, value) VALUES (?, ?) ON CONFLICT(key) DO UPDATE SET value = excluded.value`, heldLabelKey(opID), string(b))
	return err
}

func getHeldLabel(ctx context.Context, q rowQuerier, opID int64) (heldLabel, bool) {
	var v string
	if err := q.QueryRowContext(ctx, `SELECT value FROM meta WHERE key = ?`, heldLabelKey(opID)).Scan(&v); err != nil {
		return heldLabel{}, false
	}
	var l heldLabel
	return l, json.Unmarshal([]byte(v), &l) == nil
}

// pruneHeldLabels drops labels whose op is no longer in the outbox.
func pruneHeldLabels(ctx context.Context, tx *sql.Tx) error {
	_, err := tx.ExecContext(ctx, `DELETE FROM meta WHERE key LIKE 'held_label:%'
AND CAST(substr(key, 12) AS INTEGER) NOT IN (SELECT op_id FROM outbox)`)
	return err
}

// dropHeldLabels removes the labels of record id's ops (or every label).
func dropHeldLabels(ctx context.Context, tx *sql.Tx, recordID string) error {
	if recordID == "" {
		_, err := tx.ExecContext(ctx, `DELETE FROM meta WHERE key LIKE 'held_label:%'`)
		return err
	}
	_, err := tx.ExecContext(ctx, `DELETE FROM meta WHERE key IN (SELECT 'held_label:' || op_id FROM outbox WHERE record_id = ?)`, recordID)
	return err
}

// markSyncedIfIdle marks id synced unless other ops for it still wait.
func markSyncedIfIdle(ctx context.Context, tx *sql.Tx, id, seq string) error {
	var n int
	if err := tx.QueryRowContext(ctx, `SELECT count(*) FROM outbox WHERE record_id = ? AND op = 'upsert' AND state != 'blocked'`, id).Scan(&n); err != nil {
		return err
	}
	if seq != "" {
		if _, err := tx.ExecContext(ctx, `UPDATE records SET server_seq = ? WHERE id = ?`, seq, id); err != nil {
			return err
		}
	}
	if n > 0 {
		return nil
	}
	_, err := tx.ExecContext(ctx, `UPDATE records SET sync_state = 'synced', sync_code = '' WHERE id = ? AND sync_state IN ('pending','synced')`, id)
	return err
}

func (s *Store) applyResult(ctx context.Context, tx *sql.Tx, op OutboxOp, it wireItem, r PushResult, now time.Time, out *PushOutcome) error {
	switch r.Status {
	case "accepted", "unchanged", "merged":
		if err := doneOp(ctx, tx, op.OpID); err != nil {
			return err
		}
		if op.Op == OpForget {
			out.Forgotten++
			return s.forgetAcked(ctx, tx, op)
		}
		id := op.RecordID
		if r.Status == "merged" && r.CanonicalID != "" && r.CanonicalID != id {
			out.Merged++
			if err := rekey(ctx, tx, id, r.CanonicalID); err != nil {
				return err
			}
			id = r.CanonicalID
		} else {
			out.Synced++
		}
		if err := markSyncedIfIdle(ctx, tx, id, r.Seq); err != nil {
			return err
		}
		return s.advanceReplacement(ctx, tx, op.OpID)
	case "superseded", "forgotten":
		if err := doneOp(ctx, tx, op.OpID); err != nil {
			return err
		}
		if op.Op == OpForget {
			out.Forgotten++
			return s.forgetAcked(ctx, tx, op)
		}
		out.Removed++
		return s.removeLocal(ctx, tx, op.RecordID, r.Status, r.Seq, now)
	case "rejected":
		retry := r.Retry != nil && *r.Retry
		switch r.Code {
		case "resync_required":
			out.ResyncRequired = true
		case "sync_disabled":
			out.SyncDisabled = true
		case "quota_exceeded":
			out.QuotaExceeded = true
		case "tombstone_quota":
			// Fleet's tombstone quota is full (equal-rank superseded-id
			// path). Retrying this record cannot succeed until the user
			// frees space: never retry it, whatever the hint says.
			retry = false
		}
		if retry {
			return s.retryOp(ctx, tx, op, r.Code, now, out)
		}
		return s.blockOp(ctx, tx, op, r.Code, out)
	default:
		return s.retryOp(ctx, tx, op, "unknown_status", now, out)
	}
}

// forgetAcked records that Fleet tombstoned a forgotten id.
func (s *Store) forgetAcked(ctx context.Context, tx *sql.Tx, op OutboxOp) error {
	if _, err := tx.ExecContext(ctx, `UPDATE tombstones SET acked = 1 WHERE id = ?`, op.RecordID); err != nil {
		return err
	}
	// A released replacement forget completes the job and drops the
	// hidden old copy for good.
	rows, err := tx.QueryContext(ctx, `SELECT old_id FROM replacements WHERE old_forget_op = ? AND phase = 'old_forget_pending'`, op.OpID)
	if err != nil {
		return err
	}
	var olds []string
	for rows.Next() {
		var o string
		rows.Scan(&o)
		olds = append(olds, o)
	}
	rows.Close()
	for _, o := range olds {
		if _, err := tx.ExecContext(ctx, `DELETE FROM records WHERE id = ? AND visibility = 'replaced'`, o); err != nil {
			return err
		}
	}
	_, err = tx.ExecContext(ctx, `UPDATE replacements SET phase = 'complete' WHERE old_forget_op = ? AND phase = 'old_forget_pending'`, op.OpID)
	return err
}

// advanceReplacement: the new copy of an edit is on Fleet, so the old
// copy's forget may go (new_accepted → old_forget_pending).
func (s *Store) advanceReplacement(ctx context.Context, tx *sql.Tx, newOp int64) error {
	if _, err := tx.ExecContext(ctx, `UPDATE replacements SET phase = 'new_accepted' WHERE new_upsert_op = ? AND phase IN ('prepared','new_pending')`, newOp); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `UPDATE outbox SET state = 'ready' WHERE state = 'held' AND op_id IN (SELECT old_forget_op FROM replacements WHERE new_upsert_op = ? AND phase = 'new_accepted')`, newOp); err != nil {
		return err
	}
	_, err := tx.ExecContext(ctx, `UPDATE replacements SET phase = 'old_forget_pending' WHERE new_upsert_op = ? AND phase = 'new_accepted'`, newOp)
	return err
}

// removeLocal deletes id locally because Fleet holds it as a tombstone
// (forgotten elsewhere, or superseded). Deletion wins over pending work.
func (s *Store) removeLocal(ctx context.Context, tx *sql.Tx, id, reason, seq string, now time.Time) error {
	if _, err := tx.ExecContext(ctx, `DELETE FROM outbox WHERE record_id = ? AND op = 'upsert'`, id); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM records WHERE id = ?`, id); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO tombstones(id, reason, server_seq, acked, created_at) VALUES (?,?,?,1,?)
ON CONFLICT(id) DO UPDATE SET acked = 1, server_seq = excluded.server_seq`, id, reason, seq, now.UnixMilli()); err != nil {
		return err
	}
	// If id was the new copy of an edit, the edit is gone: its old copy
	// is forgotten too (deletion wins over replacement).
	if _, err := tx.ExecContext(ctx, `UPDATE outbox SET state = 'ready' WHERE state = 'held' AND op_id IN (SELECT old_forget_op FROM replacements WHERE new_id = ? AND phase NOT IN ('complete','canceled'))`, id); err != nil {
		return err
	}
	_, err := tx.ExecContext(ctx, `UPDATE replacements SET phase = 'old_forget_pending' WHERE new_id = ? AND phase NOT IN ('complete','canceled')`, id)
	return err
}

// rekey moves local id to Fleet's canonical id (merged duplicate content).
func rekey(ctx context.Context, tx *sql.Tx, id, canonical string) error {
	if _, ok, err := loadRecord(ctx, tx, canonical); err != nil {
		return err
	} else if ok {
		if _, err := tx.ExecContext(ctx, `DELETE FROM records WHERE id = ?`, id); err != nil {
			return err
		}
	} else if _, err := tx.ExecContext(ctx, `UPDATE records SET id = ? WHERE id = ?`, canonical, id); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO aliases(alias_id, canonical_id) VALUES (?, ?) ON CONFLICT(alias_id) DO UPDATE SET canonical_id = excluded.canonical_id`, id, canonical); err != nil {
		return err
	}
	for _, q := range []string{
		`UPDATE aliases SET canonical_id = ? WHERE canonical_id = ?`,
		`UPDATE outbox SET record_id = ? WHERE record_id = ?`,
		`UPDATE replacements SET new_id = ? WHERE new_id = ?`,
		`UPDATE replacements SET old_id = ? WHERE old_id = ?`,
	} {
		if _, err := tx.ExecContext(ctx, q, canonical, id); err != nil {
			return err
		}
	}
	return nil
}
