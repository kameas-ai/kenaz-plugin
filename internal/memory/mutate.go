package memory

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
)

// MutateOptions qualify update, forget and forget-all.
type MutateOptions struct {
	// RequestKey makes the operation idempotent (an approval executor
	// replaying the same request returns the first result).
	RequestKey string
	// SyncApproved: a human authorized this change reaching Fleet. Without
	// it, any Fleet-side effect is queued as unapproved and not sent.
	SyncApproved bool
	// Records (EnableSync only) are the notes the approval listed, bound
	// to their content hash; only those are queued.
	Records []HeldOp
}

// ForgetResult is what a forget did.
type ForgetResult struct {
	ID string `json:"id"`
	// Remote is "none" (Fleet never had it), "pending" (a forget is
	// queued), "unapproved" (queued, waiting for authorization).
	Remote string `json:"remote"`
}

// Forget deletes one note (aliases resolve). It is the store-level delete
// with no Fleet authorization: any Fleet-side forget it needs is queued
// unapproved.
func (s *Store) Forget(ctx context.Context, id string) error {
	_, err := s.ForgetNote(ctx, id, MutateOptions{})
	return err
}

// ForgetNote hides the note from retrieval immediately and durably
// records the deletion. If Fleet may hold it (it came from Fleet, was
// acknowledged, or an upsert for it may have been sent), a forget op is
// queued and the tombstone is kept until Fleet acknowledges it; if every
// op for it is provably unsent, the create and the delete cancel out and
// nothing is sent. Deleting the new copy of an edit in progress deletes
// the note: unsent work is cancelled and the old copy is forgotten too.
func (s *Store) ForgetNote(ctx context.Context, id string, o MutateOptions) (ForgetResult, error) {
	var res ForgetResult
	err := s.tx(ctx, func(tx *sql.Tx) error {
		if o.RequestKey != "" {
			if prev, ok, err := appliedResult(ctx, tx, o.RequestKey); err != nil || ok {
				if ok {
					err = json.Unmarshal(prev, &res)
				}
				return err
			}
		}
		id = resolveID(ctx, tx, id)
		r, ok, err := loadRecord(ctx, tx, id)
		if err != nil {
			return err
		}
		if !ok || r.Visibility != "visible" {
			return ErrNotFound
		}
		h, err := tickHLC(ctx, tx, s.now(), s.device)
		if err != nil {
			return err
		}
		remote, err := s.forgetOne(ctx, tx, r, string(h), o.SyncApproved)
		if err != nil {
			return err
		}
		// An edit in progress whose new copy this is: the old copy goes
		// too, and deletion wins over the replacement's retries.
		rows, err := tx.QueryContext(ctx, `SELECT job_id, old_id, old_forget_op FROM replacements WHERE new_id = ? AND phase NOT IN ('complete','canceled')`, id)
		if err != nil {
			return err
		}
		type job struct {
			id       int64
			old      string
			forgetOp int64
		}
		var jobs []job
		for rows.Next() {
			var j job
			rows.Scan(&j.id, &j.old, &j.forgetOp)
			jobs = append(jobs, j)
		}
		rows.Close()
		for _, j := range jobs {
			if _, err := tx.ExecContext(ctx, `DELETE FROM outbox WHERE op_id = ?`, j.forgetOp); err != nil {
				return err
			}
			if old, ok, err := loadRecord(ctx, tx, j.old); err != nil {
				return err
			} else if ok {
				if r2, err := s.forgetOne(ctx, tx, old, string(h), o.SyncApproved); err != nil {
					return err
				} else if remote == "none" {
					remote = r2
				}
			}
			if _, err := tx.ExecContext(ctx, `UPDATE replacements SET phase = 'canceled' WHERE job_id = ?`, j.id); err != nil {
				return err
			}
		}
		res = ForgetResult{ID: id, Remote: remote}
		if o.RequestKey != "" {
			return recordApplied(ctx, tx, o.RequestKey, "memory.forget", res, s.now().UnixMilli())
		}
		return nil
	})
	return res, err
}

// forgetOne deletes r locally and queues its forget when Fleet may hold
// it. It returns the remote effect.
func (s *Store) forgetOne(ctx context.Context, tx *sql.Tx, r record, hlc string, approved bool) (string, error) {
	may, err := remoteMayHave(ctx, tx, r)
	if err != nil {
		return "", err
	}
	if err := dropUnsentOps(ctx, tx, r.ID); err != nil {
		return "", err
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM records WHERE id = ?`, r.ID); err != nil {
		return "", err
	}
	if !may || s.profile.IsLocal() {
		return "none", nil
	}
	state, remote := opReady, "pending"
	if !approved {
		state, remote = opUnapproved, "unapproved"
	}
	now := s.now().UnixMilli()
	if _, err := tx.ExecContext(ctx, `INSERT INTO tombstones(id, reason, hlc, acked, created_at) VALUES (?, 'forgotten', ?, 0, ?)
ON CONFLICT(id) DO UPDATE SET reason = 'forgotten', acked = 0`, r.ID, hlc, now); err != nil {
		return "", err
	}
	op, err := enqueue(ctx, tx, r.ID, OpForget, forgetPayload(r.ID, hlc), 0, state, now)
	if err != nil {
		return "", err
	}
	if !approved {
		// The note row is gone, but the approval page must still say what
		// this deletion removes: keep its title and content hash (never
		// sent; removed with the op).
		if err := setHeldLabel(ctx, tx, op, r.Title, r.Hash); err != nil {
			return "", err
		}
	}
	return remote, nil
}

// Update is an edit. Nil fields are unchanged.
type Update struct {
	Title *string
	Body  *string
	Tags  *[]string
}

// UpdateResult is what an edit did.
type UpdateResult struct {
	// ID is the note's id after the edit: a new id when the body changed
	// on a note Fleet may hold (content is immutable there).
	ID       string `json:"id"`
	Replaced string `json:"replaced,omitempty"`
	State    string `json:"sync_state"`
	Code     string `json:"sync_code,omitempty"`
}

// UpdateNote edits a note in one transaction.
//
// Title and tag edits keep the id; a title change on a note Fleet may
// hold queues a title register write with a fresh HLC (per-field
// last-writer-wins). A body change on a note Fleet may hold creates a
// replacement: a new note (new id, the new text) becomes visible and the
// old one is hidden at once, and a replacement job releases the old
// note's forget only after Fleet accepts the new one. If Fleet refuses the
// new text for good, the old synchronized copy is kept (hidden) and the
// job is blocked for correction or reversal (Revert). A note Fleet never
// had is edited in place.
func (s *Store) UpdateNote(ctx context.Context, id string, u Update, o MutateOptions) (UpdateResult, error) {
	var res UpdateResult
	if u.Title == nil && u.Body == nil && u.Tags == nil {
		return res, fmt.Errorf("%w: nothing to change", ErrInvalidInput)
	}
	var title string
	var tags []string
	var err error
	if u.Title != nil {
		if title, err = validTitle(*u.Title); err != nil {
			return res, err
		}
	}
	if u.Body != nil {
		if err := validBody(*u.Body); err != nil {
			return res, err
		}
	}
	if u.Tags != nil {
		if tags, err = validTags(*u.Tags); err != nil {
			return res, err
		}
	}
	if (u.Title != nil && LooksLikeSecret(title)) || (u.Body != nil && LooksLikeSecret(*u.Body)) || LooksLikeSecret(fmt.Sprint(tags)) {
		return res, ErrLooksSecret
	}
	err = s.tx(ctx, func(tx *sql.Tx) error {
		if o.RequestKey != "" {
			if prev, ok, err := appliedResult(ctx, tx, o.RequestKey); err != nil || ok {
				if ok {
					err = json.Unmarshal(prev, &res)
				}
				return err
			}
		}
		id = resolveID(ctx, tx, id)
		r, ok, err := loadRecord(ctx, tx, id)
		if err != nil {
			return err
		}
		if !ok || r.Visibility != "visible" {
			return ErrNotFound
		}
		h, err := tickHLC(ctx, tx, s.now(), s.device)
		if err != nil {
			return err
		}
		now := s.now().UnixMilli()
		may, err := remoteMayHave(ctx, tx, r)
		if err != nil {
			return err
		}
		if u.Title != nil {
			r.Title, r.TitleHLC = title, string(h)
		}
		if u.Tags != nil {
			b, _ := json.Marshal(tags)
			r.Tags = string(b)
		}
		bodyChanged := u.Body != nil && *u.Body != r.Content
		if bodyChanged && may && !s.profile.IsLocal() {
			res, err = s.replace(ctx, tx, r, *u.Body, string(h), o.SyncApproved, now)
		} else {
			if bodyChanged {
				r.Content, r.Hash = *u.Body, ContentHash(*u.Body)
			}
			res, err = s.editInPlace(ctx, tx, r, may, bodyChanged, u.Title != nil, o.SyncApproved, now)
		}
		if err != nil {
			return err
		}
		if o.RequestKey != "" {
			return recordApplied(ctx, tx, o.RequestKey, "memory.update", res, now)
		}
		return nil
	})
	return res, err
}

func (s *Store) editInPlace(ctx context.Context, tx *sql.Tx, r record, may, bodyChanged, titleChanged, approved bool, now int64) (UpdateResult, error) {
	if _, err := tx.ExecContext(ctx, `UPDATE records SET title = ?, title_hlc = ?, tags = ?, content = ?, content_hash = ?, updated_at = ?, version = version + 1 WHERE id = ?`,
		r.Title, r.TitleHLC, r.Tags, r.Content, r.Hash, now, r.ID); err != nil {
		return UpdateResult{}, err
	}
	res := UpdateResult{ID: r.ID, State: r.SyncState, Code: r.SyncCode}
	switch {
	case bodyChanged || (!may && titleChanged):
		// Fleet never had it: requeue the create with the new text/title
		// (nothing was submitted, so no payload is rewritten in flight).
		if err := dropUnsentOps(ctx, tx, r.ID); err != nil {
			return res, err
		}
		// New content never inherits an earlier approval: the superseded
		// create is dropped and the new text waits for its own approval.
		st, code, _, err := s.makeEligible(ctx, tx, &r, approved, 0)
		if err != nil {
			return res, err
		}
		res.State, res.Code = st, code
	case may && titleChanged && !s.profile.IsLocal():
		state, rs := opReady, StatePending
		if !approved {
			state, rs = opUnapproved, StateUnapproved
		}
		var dep int64
		_ = tx.QueryRowContext(ctx, `SELECT coalesce(max(op_id), 0) FROM outbox WHERE record_id = ? AND op = 'upsert' AND state != 'blocked'`, r.ID).Scan(&dep)
		if _, err := enqueue(ctx, tx, r.ID, OpUpsert, titlePayload(r.ID, r.Title, r.TitleHLC), dep, state, now); err != nil {
			return res, err
		}
		if err := setSync(ctx, tx, r.ID, rs, ""); err != nil {
			return res, err
		}
		res.State, res.Code = rs, ""
	}
	return res, nil
}

// replace creates the new copy of an edited note and the replacement job
// (phase prepared) in one transaction.
func (s *Store) replace(ctx context.Context, tx *sql.Tx, old record, body, hlc string, approved bool, now int64) (UpdateResult, error) {
	nr := old
	nr.ID, nr.Content, nr.Hash = newID(), body, ContentHash(body)
	nr.Origin, nr.CreatedAt, nr.UpdatedAt = "device", now, now
	nr.CreatedHLC, nr.ScopeHLC, nr.TitleHLC = hlc, hlc, hlc
	nr.Visibility, nr.SyncState, nr.SyncCode, nr.ServerSeq = "visible", StateLocal, "", ""
	if err := insertRecord(ctx, tx, nr, ""); err != nil {
		return UpdateResult{}, err
	}
	// Hide the old copy now; it stays until Fleet holds the new one.
	if err := dropUnsentOps(ctx, tx, old.ID); err != nil {
		return UpdateResult{}, err
	}
	if _, err := tx.ExecContext(ctx, `UPDATE records SET visibility = 'replaced', updated_at = ? WHERE id = ?`, now, old.ID); err != nil {
		return UpdateResult{}, err
	}
	st, code, newOp, err := s.makeEligible(ctx, tx, &nr, approved, 0)
	if err != nil {
		return UpdateResult{}, err
	}
	forgetOp, err := enqueue(ctx, tx, old.ID, OpForget, forgetPayload(old.ID, hlc), newOp, opHeld, now)
	if err != nil {
		return UpdateResult{}, err
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO tombstones(id, reason, hlc, acked, created_at) VALUES (?, 'replaced', ?, 0, ?) ON CONFLICT(id) DO NOTHING`, old.ID, hlc, now); err != nil {
		return UpdateResult{}, err
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO replacements(old_id, new_id, new_upsert_op, old_forget_op, phase, code) VALUES (?,?,?,?, 'prepared', ?)`,
		old.ID, nr.ID, newOp, forgetOp, code); err != nil {
		return UpdateResult{}, err
	}
	if st == StateRejected {
		// The on-device scan refused the new text: the job is blocked at
		// once and the old synchronized copy is kept.
		if _, err := tx.ExecContext(ctx, `UPDATE replacements SET phase = 'blocked' WHERE new_id = ?`, nr.ID); err != nil {
			return UpdateResult{}, err
		}
	}
	return UpdateResult{ID: nr.ID, Replaced: old.ID, State: st, Code: code}, nil
}

// ErrNotBlocked means Revert was asked for an edit that is not blocked.
var ErrNotBlocked = errors.New("that note has no blocked edit to revert")

// Revert undoes a blocked edit: the new copy (which Fleet refused) is
// deleted and the old, still synchronized copy becomes visible again.
func (s *Store) Revert(ctx context.Context, newID string) error {
	return s.tx(ctx, func(tx *sql.Tx) error {
		var job int64
		var old string
		var forgetOp int64
		err := tx.QueryRowContext(ctx, `SELECT job_id, old_id, old_forget_op FROM replacements WHERE new_id = ? AND phase = 'blocked'`, newID).Scan(&job, &old, &forgetOp)
		if errors.Is(err, sql.ErrNoRows) {
			// A deletion still waiting for approval is undone by showing
			// the note again.
			res, err := tx.ExecContext(ctx, `UPDATE records SET visibility = 'visible' WHERE id = ? AND visibility = 'forget_pending'`, newID)
			if err != nil {
				return err
			}
			if n, _ := res.RowsAffected(); n == 1 {
				return nil
			}
			return ErrNotBlocked
		}
		if err != nil {
			return err
		}
		for _, q := range []struct {
			sql  string
			args []any
		}{
			{`DELETE FROM outbox WHERE op_id = ? OR (record_id = ? AND state != 'inflight')`, []any{forgetOp, newID}},
			{`DELETE FROM records WHERE id = ?`, []any{newID}},
			{`DELETE FROM tombstones WHERE id = ? AND reason = 'replaced'`, []any{old}},
			{`UPDATE records SET visibility = 'visible' WHERE id = ?`, []any{old}},
			{`UPDATE replacements SET phase = 'canceled' WHERE job_id = ?`, []any{job}},
		} {
			if _, err := tx.ExecContext(ctx, q.sql, q.args...); err != nil {
				return err
			}
		}
		return nil
	})
}

// ForgetAllResult is what a forget-all did on this device.
type ForgetAllResult struct {
	Deleted int `json:"deleted"`
	// FleetErasePending: Fleet may hold notes for this account, so Fleet's
	// copy is erased next (forget-all route), before any new sync send.
	// It is not done until Fleet confirms; a network failure never counts
	// as erased.
	FleetErasePending bool `json:"fleet_erase_pending"`
}

// ForgetAll deletes every note of this profile on this device in one
// transaction and, when sync was ever on, records a local erase barrier:
// nothing is sent again until Fleet's forget-all succeeds and the
// following reset is reconciled. Must only be called after a confirmed
// human authorization.
func (s *Store) ForgetAll(ctx context.Context, o MutateOptions) (ForgetAllResult, error) {
	var res ForgetAllResult
	err := s.tx(ctx, func(tx *sql.Tx) error {
		if o.RequestKey != "" {
			if prev, ok, err := appliedResult(ctx, tx, o.RequestKey); err != nil || ok {
				if ok {
					err = json.Unmarshal(prev, &res)
				}
				return err
			}
		}
		if err := tx.QueryRowContext(ctx, `SELECT count(*) FROM records WHERE visibility = 'visible'`).Scan(&res.Deleted); err != nil {
			return err
		}
		h, err := tickHLC(ctx, tx, s.now(), s.device)
		if err != nil {
			return err
		}
		if err := dropHeldLabels(ctx, tx, ""); err != nil {
			return err
		}
		for _, q := range []string{`DELETE FROM records`, `DELETE FROM outbox`, `DELETE FROM replacements`, `DELETE FROM aliases`, `DELETE FROM tombstones`, `DELETE FROM snapshot_stage`} {
			if _, err := tx.ExecContext(ctx, q); err != nil {
				return err
			}
		}
		// An account profile always erases on Fleet too, whether or not
		// this device ever enabled sync (another device may have), so
		// the approval's "and in Kameas Fleet" is true.
		if !s.profile.IsLocal() {
			res.FleetErasePending = true
			// The barrier also ends every lease: a worker with a pull or
			// push in flight can no longer apply anything it fetched.
			if _, err := tx.ExecContext(ctx, `UPDATE sync_state SET erase_pending = 1, erase_barrier_hlc = ?, reset_phase = '',
profile_generation = profile_generation + 1, lease_holder = '', lease_until = 0 WHERE singleton = 1`, string(h)); err != nil {
				return err
			}
		}
		if o.RequestKey != "" {
			return recordApplied(ctx, tx, o.RequestKey, "memory.forget_all", res, s.now().UnixMilli())
		}
		return nil
	})
	return res, err
}

// ErasePending reports whether a forget-all still has to reach Fleet.
func (s *Store) ErasePending(ctx context.Context) (bool, error) {
	set, err := loadSettings(ctx, s.db)
	return set.ErasePending, err
}

// CompleteErase records Fleet's forget-all answer. The erase barrier lifts;
// the next pull resets (reason erased) and, because erasedBefore is this
// device's own erase, notes saved after the barrier are kept.
func (s *Store) CompleteErase(ctx context.Context, l Lease, erasedBefore string) error {
	return s.tx(ctx, func(tx *sql.Tx) error {
		if err := checkLease(ctx, tx, l); err != nil {
			return err
		}
		_, err := tx.ExecContext(ctx, `UPDATE sync_state SET erase_pending = 0, own_erase_before = ?, erased_before = ? WHERE singleton = 1`, erasedBefore, erasedBefore)
		return err
	})
}
