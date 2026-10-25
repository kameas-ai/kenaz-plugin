package memory

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"sort"
	"strings"
	"time"
)

// Remote is the Fleet side of turning sync on and off. memsync implements
// it over fleetapi; nil means sync is not available in this build.
type Remote interface {
	// EnableSync checks the account's entitlement and records the opt-in
	// (with consentVersion) on Fleet for p.
	EnableSync(ctx context.Context, p Profile, consentVersion string) error
	// DisableSync records the opt-out on Fleet (best effort).
	DisableSync(ctx context.Context, p Profile) error
}

// ServiceOptions configure a Service.
type ServiceOptions struct {
	// StateRoot is the kenaz state root (stores live under memory/).
	StateRoot string
	// Active returns the signed-in profile, or Local. It must not do
	// network I/O. nil means always Local.
	Active func(ctx context.Context) Profile
	// Submitter prepares human-approval requests (nil: none wired; see
	// authz.go for what is gated then).
	Submitter Submitter
	// Remote turns Fleet sync on and off (nil: unavailable).
	Remote Remote
	// ConsentVersion is the memory-sync notice edition a user accepts by
	// enabling sync; ConsentNotice is its text, shown on the approval page.
	ConsentVersion string
	ConsentNotice  string
	// Busy bounds waits for another writer.
	Busy time.Duration
}

// Service is memory as the tools and CLI use it: the active account
// profile plus the device notebook, with every mutation going through the
// authorization gate (authz.go). In the relay daemon it also hosts the
// approved executors (RegisterExecutors).
type Service struct {
	o ServiceOptions
}

// Outcome statuses.
const (
	OutcomeDone                = "done"
	OutcomeApprovalPending     = "approval_pending"
	OutcomeDenied              = "denied"
	OutcomeExpired             = "expired"
	OutcomeCanceled            = "canceled"
	OutcomeFailed              = "failed"
	OutcomeApprovalUnavailable = "approval_unavailable"
	OutcomeReconciling         = "reconciling"
)

// Outcome is the truthful result of a gated operation.
type Outcome struct {
	Status    string `json:"status"`
	RequestID string `json:"request_id,omitempty"`
	// ApproveURL is the local approval page for a pending request.
	ApproveURL string `json:"approve_url,omitempty"`
	ID         string `json:"id,omitempty"`
	Replaced   string `json:"replaced,omitempty"`
	// SyncState is the note's state afterwards: local, unapproved,
	// pending, synced, rejected.
	SyncState string `json:"sync_state,omitempty"`
	SyncCode  string `json:"sync_code,omitempty"`
	// Remote is a forget's Fleet effect: none, pending, unapproved.
	Remote string `json:"remote,omitempty"`
	// Hidden: a deletion without approval hid the note; it is not deleted.
	Hidden            bool `json:"hidden,omitempty"`
	Deleted           int  `json:"deleted,omitempty"`
	FleetErasePending bool `json:"fleet_erase_pending,omitempty"`
	Queued            int  `json:"queued,omitempty"`
	// Profile is "device" (the device notebook) or "account".
	Profile string `json:"profile,omitempty"`
	Message string `json:"message,omitempty"`
	// Remaining counts items left for a later approval: one approval
	// lists at most ApprovalBatch items, so a larger sync-enable,
	// sync-approve or adopt goes in successive batches.
	Remaining int `json:"remaining,omitempty"`
}

// ApprovalBatch is the most items one approval request lists (the
// approval service's MaxTargets). approvalBatchBytes keeps the payload,
// which carries every listed note's full text, well under the service's
// 1 MiB payload limit.
const (
	ApprovalBatch      = 64
	approvalBatchBytes = 768 << 10
)

// firstBatch is the leading run of items that fits one approval.
func firstBatch(items []HeldOp) []HeldOp {
	size := 0
	for i, it := range items {
		b, _ := json.Marshal(it)
		if i == ApprovalBatch || (i > 0 && size+len(b)+1 > approvalBatchBytes) {
			return items[:i]
		}
		size += len(b) + 1
	}
	return items
}

// withRemaining notes the items a batched request left for later.
func withRemaining(out Outcome, err error, remaining int, again string) (Outcome, error) {
	if err != nil || remaining == 0 {
		return out, err
	}
	out.Remaining = remaining
	out.Message = strings.TrimSpace(out.Message + fmt.Sprintf(" This request covers the first %d; %d more wait for their own approval: after this one, run `%s` again.", ApprovalBatch, remaining, again))
	return out, nil
}

// NewService builds a Service.
func NewService(o ServiceOptions) *Service {
	if o.Active == nil {
		o.Active = func(context.Context) Profile { return Local }
	}
	if o.Busy <= 0 {
		o.Busy = 5 * time.Second
	}
	return &Service{o: o}
}

// Active returns the profile mutations apply to now.
func (s *Service) Active(ctx context.Context) Profile {
	p := s.o.Active(ctx)
	if !p.Valid() {
		return Local
	}
	return p
}

func (s *Service) path(p Profile) string { return ProfilePath(s.o.StateRoot, p) }

func profileLabel(p Profile) string {
	if p.IsLocal() {
		return "device"
	}
	return "account"
}

// readers opens the stores retrieval reads: the active account profile
// (if signed in and it exists) and the device notebook. A missing store is
// skipped.
func (s *Service) readers(ctx context.Context) ([]*Store, error) {
	var out []*Store
	ps := []Profile{s.Active(ctx)}
	if !ps[0].IsLocal() {
		ps = append(ps, Local)
	}
	for _, p := range ps {
		st, err := OpenProfileReadOnly(s.path(p), p, s.o.Busy)
		if errors.Is(err, ErrNoStore) {
			continue
		}
		if err != nil {
			for _, o := range out {
				o.Close()
			}
			return nil, err
		}
		out = append(out, st)
	}
	return out, nil
}

func closeAll(ss []*Store) {
	for _, s := range ss {
		s.Close()
	}
}

// Search searches the active account profile and the device notebook.
func (s *Service) Search(ctx context.Context, q string, sc Scope, limit int) ([]Entry, error) {
	if _, err := ftsQuery(q); err != nil {
		return nil, err
	}
	rs, err := s.readers(ctx)
	if err != nil {
		return nil, err
	}
	defer closeAll(rs)
	var all []Entry
	for _, r := range rs {
		es, err := r.Search(ctx, q, sc, limit)
		if err != nil {
			return nil, err
		}
		all = append(all, es...)
	}
	sort.SliceStable(all, func(i, j int) bool { return all[i].Rank < all[j].Rank })
	if l := clampLimit(limit); len(all) > l {
		all = all[:l]
	}
	return all, nil
}

// Recent lists the newest notes across the readable stores.
func (s *Service) Recent(ctx context.Context, sc Scope, limit int) ([]Entry, error) {
	rs, err := s.readers(ctx)
	if err != nil {
		return nil, err
	}
	defer closeAll(rs)
	all := []Entry{}
	for _, r := range rs {
		es, err := r.Recent(ctx, sc, limit)
		if err != nil {
			return nil, err
		}
		all = append(all, es...)
	}
	sort.SliceStable(all, func(i, j int) bool { return all[i].CreatedAt.After(all[j].CreatedAt) })
	if l := clampLimit(limit); len(all) > l {
		all = all[:l]
	}
	return all, nil
}

// Get reads notes by id from the readable stores.
func (s *Service) Get(ctx context.Context, ids []string) ([]Note, []string, error) {
	rs, err := s.readers(ctx)
	if err != nil {
		return nil, nil, err
	}
	defer closeAll(rs)
	var found []Note
	missing := ids
	for _, r := range rs {
		f, m, err := r.Get(ctx, missing)
		if err != nil {
			return nil, nil, err
		}
		found, missing = append(found, f...), m
	}
	// Keep the order asked.
	pos := map[string]int{}
	for i, id := range ids {
		if _, ok := pos[id]; !ok {
			pos[id] = i
		}
	}
	sort.SliceStable(found, func(i, j int) bool { return pos[found[i].ID] < pos[found[j].ID] })
	return found, missing, nil
}

// Count counts notes in the readable stores.
func (s *Service) Count(ctx context.Context) (int, error) {
	rs, err := s.readers(ctx)
	if err != nil {
		return 0, err
	}
	defer closeAll(rs)
	n := 0
	for _, r := range rs {
		c, err := r.Count(ctx)
		if err != nil {
			return 0, err
		}
		n += c
	}
	return n, nil
}

// Export writes the member's own notes (the active account profile and
// the device notebook) as NDJSON.
func (s *Service) Export(ctx context.Context, w io.Writer) (int, error) {
	rs, err := s.readers(ctx)
	if err != nil {
		return 0, err
	}
	defer closeAll(rs)
	n := 0
	for _, r := range rs {
		c, err := r.Export(ctx, w, profileLabel(r.Profile()))
		n += c
		if err != nil {
			return n, err
		}
	}
	return n, nil
}

// Index is the session-start memory index for project p across the
// readable stores (titles and ids only).
func (s *Service) Index(ctx context.Context, p Project) string {
	ctx, cancel := context.WithTimeout(ctx, indexTimeout)
	defer cancel()
	es, err := s.Recent(ctx, ProjectScope(p.Key), IndexMaxEntries)
	if err != nil || len(es) == 0 {
		return ""
	}
	return FormatIndex(es)
}

// SyncStatus reports the active account profile's sync state; ok is false
// on the device notebook (it never syncs).
func (s *Service) SyncStatus(ctx context.Context) (SyncStatus, Profile, bool, error) {
	p := s.Active(ctx)
	if p.IsLocal() {
		return SyncStatus{}, p, false, nil
	}
	st, err := OpenProfileReadOnly(s.path(p), p, s.o.Busy)
	if errors.Is(err, ErrNoStore) {
		return SyncStatus{Notes: map[string]int{}, Queue: map[string]int{}}, p, true, nil
	}
	if err != nil {
		return SyncStatus{}, p, true, err
	}
	defer st.Close()
	ss, err := st.Status(ctx)
	return ss, p, true, err
}

// ---- gated operations ----

// opPayload is the exact payload the approval service stores and the
// executor applies. It carries the profile so an executor run after an
// account switch refuses instead of writing into the wrong account, and
// the note content so the approval page shows what will be written.
type opPayload struct {
	Profile Profile   `json:"profile"`
	Note    *NewNote  `json:"note,omitempty"`
	ID      string    `json:"id,omitempty"`
	Title   *string   `json:"title,omitempty"`
	Body    *string   `json:"body,omitempty"`
	Tags    *[]string `json:"tags,omitempty"`
	// IDs: notes to adopt.
	IDs []string `json:"ids,omitempty"`
	// Ops: held changes to release (sync approve), with what they do.
	Ops []HeldOp `json:"ops,omitempty"`
	// Records: the notes (and their content hashes) sync-enable uploads
	// or adopt moves; nothing else is touched.
	Records []HeldOp `json:"records,omitempty"`
}

// ErrAccountChanged refuses an approved operation whose account is no
// longer the signed-in one.
var ErrAccountChanged = errors.New("the signed-in account changed since this was requested; nothing was done")

// ErrSyncUnavailable means this build cannot sync memory.
var ErrSyncUnavailable = errors.New("memory sync is not available in this build")

// Save saves a note in the active profile through the gate.
func (s *Service) Save(ctx context.Context, n NewNote) (Outcome, error) {
	if err := n.Validate(); err != nil {
		return Outcome{}, err
	}
	p := s.Active(ctx)
	return s.gate(ctx, OpSave, p, opPayload{Profile: p, Note: &n}, nil, "Save a memory: "+Clip(n.Title, 80), "")
}

// Update edits a note through the gate.
func (s *Service) Update(ctx context.Context, id string, u Update) (Outcome, error) {
	p, err := s.owner(ctx, id)
	if err != nil {
		return Outcome{}, err
	}
	return s.gate(ctx, OpUpdate, p, opPayload{Profile: p, ID: id, Title: u.Title, Body: u.Body, Tags: u.Tags}, []string{id}, "Edit memory "+id, "")
}

// Forget deletes a note through the gate.
func (s *Service) Forget(ctx context.Context, id string) (Outcome, error) {
	p, err := s.owner(ctx, id)
	if err != nil {
		return Outcome{}, err
	}
	return s.gate(ctx, OpForgetOne, p, opPayload{Profile: p, ID: id}, []string{id}, "Delete memory "+id, "Deletes this memory on this device and, if it was synced, in your Kameas Fleet account.")
}

// ForgetAll deletes every note of the signed-in account, here and (if it
// ever synced) on Fleet. It always needs an approval; the signed-out
// notebook's delete-all is `kenaz memory purge`.
func (s *Service) ForgetAll(ctx context.Context) (Outcome, error) {
	p := s.Active(ctx)
	n := 0
	if st, err := OpenProfileReadOnly(s.path(p), p, s.o.Busy); err == nil {
		n, _ = st.Count(ctx)
		st.Close()
	}
	return s.gate(ctx, OpForgetAll, p, opPayload{Profile: p}, nil,
		"Delete every memory of this Kameas Fleet account, on this device and in Kameas Fleet",
		fmt.Sprintf("Deletes %d memories on this device and erases this account's memory in Kameas Fleet. %s", n, ForgetAllOtherDevices))
}

// ForgetAllOtherDevices is the forget-all disclosure: an erase reaches the
// account's other devices, including notes there that were never synced.
const ForgetAllOtherDevices = "This also erases your account's notes on your other devices when they next connect, including notes there that hadn't been synced yet. Notes that belong to a single project are kept."

// EnableSync turns Fleet sync on for the active account. It always needs
// an approval of the Memory Sync Notice edition it names.
func (s *Service) EnableSync(ctx context.Context) (Outcome, error) {
	p := s.Active(ctx)
	if p.IsLocal() {
		return Outcome{}, ErrLocalProfile
	}
	if s.o.Remote == nil {
		return Outcome{}, ErrSyncUnavailable
	}
	var recs []HeldOp
	if st, err := OpenProfileReadOnly(s.path(p), p, s.o.Busy); err == nil {
		recs, err = st.LocalSyncCandidates(ctx)
		st.Close()
		if err != nil {
			return Outcome{}, err
		}
	} else if !errors.Is(err, ErrNoStore) {
		return Outcome{}, err
	}
	batch := firstBatch(recs)
	targets := make([]string, len(batch))
	for i, r := range batch {
		targets[i] = r.RecordID
	}
	out, err := s.gate(ctx, OpSyncEnable, p, opPayload{Profile: p, Records: batch}, targets,
		"Turn on memory sync to your Kameas Fleet account under the Memory Sync Notice "+s.o.ConsentVersion,
		s.enableImpact())
	return withRemaining(out, err, len(recs)-len(batch), "kenaz memory sync enable")
}

func (s *Service) enableImpact() string {
	if s.o.ConsentNotice != "" {
		return s.o.ConsentNotice
	}
	return "Uploads this account's notes saved with project=global on this device, and keeps them in sync with your other devices."
}

// DisableSync turns sync off for the active account. Not gated: it only
// reduces what leaves the device. Notes already on Fleet stay there until
// deleted explicitly.
func (s *Service) DisableSync(ctx context.Context) (Outcome, error) {
	p := s.Active(ctx)
	if p.IsLocal() {
		return Outcome{}, ErrLocalProfile
	}
	st, err := OpenProfile(s.path(p), p)
	if err != nil {
		return Outcome{}, err
	}
	defer st.Close()
	if err := st.DisableSync(ctx, "disabled_by_user"); err != nil {
		return Outcome{}, err
	}
	out := Outcome{Status: OutcomeDone, Profile: "account", Message: SyncOffMessage}
	if s.o.Remote != nil {
		if err := s.o.Remote.DisableSync(ctx, p); err != nil {
			out.Message += " Kameas Fleet could not be told yet; this device sends no new notes either way."
		}
	}
	return out, nil
}

// SyncOffMessage explains what turning sync off does and does not do.
const SyncOffMessage = "Memory sync is off for this account. New and edited notes stay on this device. Notes already synced stay in your Kameas Fleet account until you delete them: forget them one by one, or run `kenaz memory forget-all` to erase them there and here."

// ApproveHeld submits the account's unapproved Fleet work (changes made
// while no approval was possible) for one approval. The approval page
// lists exactly the ops it releases.
func (s *Service) ApproveHeld(ctx context.Context) (Outcome, error) {
	p := s.Active(ctx)
	if p.IsLocal() {
		return Outcome{}, ErrLocalProfile
	}
	st, err := OpenProfileReadOnly(s.path(p), p, s.o.Busy)
	if errors.Is(err, ErrNoStore) {
		return Outcome{Status: OutcomeDone, Profile: "account", Message: "Nothing is waiting for approval."}, nil
	}
	if err != nil {
		return Outcome{}, err
	}
	ops, err := st.HeldOps(ctx)
	st.Close()
	if err != nil {
		return Outcome{}, err
	}
	if len(ops) == 0 {
		return Outcome{Status: OutcomeDone, Profile: "account", Message: "Nothing is waiting for approval."}, nil
	}
	batch := firstBatch(ops)
	ids := make([]string, len(batch))
	for i, o := range batch {
		ids[i] = o.RecordID
	}
	out, err := s.gate(ctx, OpSyncApprove, p, opPayload{Profile: p, Ops: batch}, ids,
		fmt.Sprintf("Send %d waiting memory changes to your Kameas Fleet account", len(batch)), "")
	return withRemaining(out, err, len(ops)-len(batch), "kenaz memory sync approve")
}

// Revert undoes a blocked edit in the active account (device-only: the
// refused copy never reached Fleet).
func (s *Service) Revert(ctx context.Context, id string) error {
	p := s.Active(ctx)
	st, err := OpenProfile(s.path(p), p)
	if err != nil {
		return err
	}
	defer st.Close()
	return st.Revert(ctx, id)
}

// Adopt submits moving device-notebook notes into the active account (so
// they can sync). This is the only way a note saved while signed out can
// ever reach Fleet, so it always needs an approval.
func (s *Service) Adopt(ctx context.Context, ids []string) (Outcome, error) {
	p := s.Active(ctx)
	if p.IsLocal() {
		return Outcome{}, ErrLocalProfile
	}
	src, err := OpenReadOnly(s.path(Local), s.o.Busy)
	if errors.Is(err, ErrNoStore) {
		return Outcome{}, ErrNotFound
	}
	if err != nil {
		return Outcome{}, err
	}
	notes, _, err := src.Get(ctx, ids)
	src.Close()
	if err != nil {
		return Outcome{}, err
	}
	if len(notes) == 0 {
		return Outcome{}, ErrNotFound
	}
	recs := make([]HeldOp, len(notes))
	for i, n := range notes {
		recs[i] = HeldOp{RecordID: n.ID, Op: OpAdopt, Title: n.Title, Hash: ContentHash(n.Body), Body: n.Body}
	}
	batch := firstBatch(recs)
	ids = make([]string, len(batch))
	for i, r := range batch {
		ids[i] = r.RecordID
	}
	out, err := s.gate(ctx, OpAdopt, p, opPayload{Profile: p, Records: batch}, ids,
		fmt.Sprintf("Move %d notes from this device's notebook into your Kameas Fleet account's memory", len(ids)),
		"The notes leave the device notebook and can then sync to your account.")
	return withRemaining(out, err, len(recs)-len(batch), "kenaz memory sync adopt --all")
}

func (s *Service) adopt(ctx context.Context, p Profile, recs []HeldOp) (int, error) {
	want := map[string]string{}
	wantTitle := map[string]string{}
	ids := make([]string, 0, len(recs))
	for _, r := range recs {
		want[r.RecordID] = r.Hash
		wantTitle[r.RecordID] = r.Title
		ids = append(ids, r.RecordID)
	}
	src, err := Open(s.path(Local))
	if err != nil {
		return 0, err
	}
	defer src.Close()
	dst, err := OpenProfile(s.path(p), p)
	if err != nil {
		return 0, err
	}
	defer dst.Close()
	notes, _, err := src.Get(ctx, ids)
	if err != nil {
		return 0, err
	}
	n := 0
	for _, nt := range notes {
		if ContentHash(nt.Body) != want[nt.ID] || nt.Title != wantTitle[nt.ID] {
			continue // changed since it was approved
		}
		nn := NewNote{ProjectKey: nt.ProjectKey, ProjectRoot: nt.ProjectRoot, Title: nt.Title, Body: nt.Body, Tags: nt.Tags, Source: nt.Source}
		if _, err := dst.SaveNote(ctx, nn, SaveOptions{RequestKey: "adopt:" + nt.ID, SyncApproved: true}); err != nil {
			return n, err
		}
		if err := src.Forget(ctx, nt.ID); err != nil && !errors.Is(err, ErrNotFound) {
			return n, err
		}
		n++
	}
	return n, nil
}

// LocalIDs lists the device notebook's note ids (for adopt --all).
func (s *Service) LocalIDs(ctx context.Context) ([]string, error) {
	st, err := OpenReadOnly(s.path(Local), s.o.Busy)
	if errors.Is(err, ErrNoStore) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	defer st.Close()
	rows, err := st.db.QueryContext(ctx, `SELECT id FROM records WHERE visibility = 'visible' ORDER BY created_at`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var ids []string
	for rows.Next() {
		var id string
		rows.Scan(&id)
		ids = append(ids, id)
	}
	return ids, rows.Err()
}

// RequestStatus reports an approval request's outcome.
func (s *Service) RequestStatus(ctx context.Context, id string) (Outcome, error) {
	if s.o.Submitter == nil {
		return Outcome{Status: OutcomeApprovalUnavailable, RequestID: id}, nil
	}
	st, res, err := s.o.Submitter.Status(ctx, id)
	if err != nil {
		return Outcome{}, err
	}
	return fromApproval(Pending{RequestID: id, Status: st}, res), nil
}

// owner finds which readable profile holds id (account first).
func (s *Service) owner(ctx context.Context, id string) (Profile, error) {
	rs, err := s.readers(ctx)
	if err != nil {
		return Local, err
	}
	defer closeAll(rs)
	for _, r := range rs {
		f, _, err := r.Get(ctx, []string{id})
		if err != nil {
			return Local, err
		}
		if len(f) > 0 {
			return r.Profile(), nil
		}
	}
	return Local, ErrNotFound
}

// UnavailableMessage explains an operation that needs an approval nobody
// can give here.
const UnavailableMessage = "Nothing was changed. This needs your approval with a security key or this device's unlock on the Kenaz approval page, and none is available here. Run `kenaz approvals enroll` on a machine with a browser and an authenticator."

func (s *Service) gate(ctx context.Context, op string, p Profile, pl opPayload, targets []string, summary, impact string) (Outcome, error) {
	payload, err := json.Marshal(pl)
	if err != nil {
		return Outcome{}, err
	}
	if p.IsLocal() {
		// The signed-out notebook: no Fleet effect and no identity to
		// approve with. Device-only edits run as they always did.
		if alwaysHuman(op) {
			msg := "Sign in to Kameas Fleet first: this applies to an account's memory."
			if op == OpForgetAll {
				msg = "Signed out, the notebook on this device is deleted with `kenaz memory purge`."
			}
			return Outcome{Status: OutcomeApprovalUnavailable, Profile: "device", Message: msg}, nil
		}
		return s.run(ctx, op, payload)
	}
	if !alwaysHuman(op) {
		// A save, edit or delete that changes nothing in Kameas Fleet (sync
		// is off and Fleet never had the note) is the user's own device
		// data: it runs directly, like the notebook. If sync is turned on
		// in between, whatever it would send is queued unapproved.
		need, err := s.reachesFleet(ctx, op, p, pl)
		if err != nil {
			return Outcome{}, err
		}
		if !need {
			return s.runDeviceOnly(ctx, op, payload)
		}
	}
	if s.o.Submitter != nil {
		key, err := s.requestKey(ctx, op, p, payload)
		if err != nil {
			return Outcome{}, err
		}
		pend, err := s.o.Submitter.Submit(ctx, ApprovalRequest{Operation: op, Env: p.Env, OrgID: p.OrgID, UserID: p.UserID,
			TargetIDs: targets, Payload: payload, IdempotencyKey: key, Summary: summary, Impact: impact})
		if err != nil {
			return Outcome{}, err
		}
		if pend.Status != ApprovalUnavailable {
			var res []byte
			if pend.Status == ApprovalDone {
				_, res, _ = s.o.Submitter.Status(ctx, pend.RequestID)
			}
			out := fromApproval(pend, res)
			out.Profile = "account"
			return out, nil
		}
	}
	if alwaysHuman(op) {
		return Outcome{Status: OutcomeApprovalUnavailable, Profile: "account", Message: UnavailableMessage}, nil
	}
	// No approval possible: apply on this device only; any Fleet effect
	// waits as unapproved.
	return s.run(ctx, op, payload)
}

// reachesFleet reports whether op on p would change anything in Kameas
// Fleet: sync is on for the account, or (edit, delete) Fleet may hold the
// note. It only reads.
func (s *Service) reachesFleet(ctx context.Context, op string, p Profile, pl opPayload) (bool, error) {
	st, err := OpenProfileReadOnly(s.path(p), p, s.o.Busy)
	if errors.Is(err, ErrNoStore) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	defer st.Close()
	set, err := st.SyncSettings(ctx)
	if err != nil {
		return false, err
	}
	if set.Enabled {
		return true, nil
	}
	switch op {
	case OpUpdate, OpForgetOne:
		return st.FleetMayHold(ctx, pl.ID)
	}
	return false, nil
}

// requestKey is the approval idempotency key: derived from the operation,
// the exact payload and the profile store's clock (which every write
// advances), so an agent that retries the same request gets the same
// approval back, while the same request made after the store changed (a
// second forget-all next week) is a new one.
func (s *Service) requestKey(ctx context.Context, op string, p Profile, payload []byte) (string, error) {
	token := ""
	if st, err := OpenProfileReadOnly(s.path(p), p, s.o.Busy); err == nil {
		token, err = st.StateToken(ctx)
		st.Close()
		if err != nil {
			return "", err
		}
	} else if !errors.Is(err, ErrNoStore) {
		return "", err
	}
	h := sha256.New()
	for _, part := range []string{"kenaz/memory-approval/v1", op, token} {
		h.Write([]byte(part))
		h.Write([]byte{0})
	}
	h.Write(payload)
	return "mem_" + hex.EncodeToString(h.Sum(nil))[:40], nil
}

// runDeviceOnly applies a write that has no Fleet effect, without an
// approval (and without marking anything approved for Fleet).
func (s *Service) runDeviceOnly(ctx context.Context, op string, payload []byte) (Outcome, error) {
	b, err := s.execute(ctx, op, payload, "", false, true)
	if err != nil {
		return Outcome{}, err
	}
	var out Outcome
	err = json.Unmarshal(b, &out)
	return out, err
}

func (s *Service) run(ctx context.Context, op string, payload []byte) (Outcome, error) {
	b, err := s.execute(ctx, op, payload, "", false, false)
	if err != nil {
		return Outcome{}, err
	}
	var out Outcome
	err = json.Unmarshal(b, &out)
	return out, err
}

func fromApproval(p Pending, res []byte) Outcome {
	out := Outcome{}
	switch p.Status {
	case ApprovalDone:
		if len(res) > 0 {
			_ = json.Unmarshal(res, &out)
		}
		out.Status = OutcomeDone
	case ApprovalDenied:
		out.Status, out.Message = OutcomeDenied, "Not done: the request was denied."
	case ApprovalExpired:
		out.Status, out.Message = OutcomeExpired, "Not done: the request expired before it was approved."
	case ApprovalCanceled:
		out.Status, out.Message = OutcomeCanceled, "Not done: the request was canceled."
	case ApprovalFailed:
		out.Status, out.Message = OutcomeFailed, "Not done: the approved change was refused ("+string(res)+")."
	case ApprovalReconciling:
		out.Status, out.Message = OutcomeReconciling, "The outcome is being checked; ask again for the request's status."
	case ApprovalUnavailable:
		out.Status, out.Message = OutcomeApprovalUnavailable, UnavailableMessage
	default: // pending, approved, executing
		out.Status, out.ApproveURL = OutcomeApprovalPending, p.ApproveURL
		out.Message = "Nothing has been written yet. Ask the user to open approve_url, review the exact change, and approve it with their security key or device unlock."
		if p.Message != "" {
			out.Message = p.Message
		}
	}
	out.RequestID = p.RequestID
	return out
}

// applied is the reconciler: was key applied in the payload's profile
// store? It only reads.
func (s *Service) applied(ctx context.Context, payload []byte, key string) ([]byte, bool, error) {
	var pl opPayload
	if err := json.Unmarshal(payload, &pl); err != nil {
		return nil, false, err
	}
	st, err := OpenProfileReadOnly(s.path(pl.Profile), pl.Profile, s.o.Busy)
	if errors.Is(err, ErrNoStore) {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, err
	}
	defer st.Close()
	b, ok, err := st.AppliedResult(ctx, key)
	if err != nil || !ok {
		return nil, false, err
	}
	var r json.RawMessage = b
	out := Outcome{Status: OutcomeDone}
	_ = json.Unmarshal(r, &out)
	b, _ = json.Marshal(out)
	return b, true, nil
}

// execute runs op. approved is true only inside an approval executor
// (the daemon, after a verified WebAuthn assertion).
// execute applies op. approved: a verified approval authorizes its Fleet
// effect. deviceOnly: the caller established the write has no Fleet
// effect, so an account deletion is applied rather than held (anything
// that turns out to need Fleet is still queued unapproved).
func (s *Service) execute(ctx context.Context, op string, payload []byte, key string, approved, deviceOnly bool) ([]byte, error) {
	var pl opPayload
	if err := json.Unmarshal(payload, &pl); err != nil {
		return nil, fmt.Errorf("%w: payload", ErrInvalidInput)
	}
	if pl.Profile != s.Active(ctx) {
		return nil, ErrAccountChanged
	}
	p := pl.Profile
	if alwaysHuman(op) && (!approved || p.IsLocal()) {
		return nil, fmt.Errorf("%w: %s needs an approval", ErrInvalidInput, op)
	}
	out := Outcome{Status: OutcomeDone, Profile: profileLabel(p)}
	if op == OpAdopt {
		n, err := s.adopt(ctx, p, pl.Records)
		if err != nil {
			return nil, err
		}
		out.Queued = n
		out.Message = fmt.Sprintf("Moved %d notes into your account.", n)
		return s.finish(ctx, p, key, op, out)
	}
	if op == OpSyncEnable {
		if s.o.Remote == nil {
			return nil, ErrSyncUnavailable
		}
		if err := s.o.Remote.EnableSync(ctx, p, s.o.ConsentVersion); err != nil {
			return nil, err
		}
	}
	st, err := OpenProfile(s.path(p), p)
	if err != nil {
		return nil, err
	}
	defer st.Close()
	mo := MutateOptions{RequestKey: key, SyncApproved: approved, Records: pl.Records}
	switch op {
	case OpSave:
		if pl.Note == nil {
			return nil, fmt.Errorf("%w: no note", ErrInvalidInput)
		}
		r, err := st.SaveNote(ctx, *pl.Note, SaveOptions{RequestKey: key, SyncApproved: approved})
		if err != nil {
			return nil, err
		}
		out.ID, out.SyncState, out.SyncCode = r.ID, r.State, r.Code
	case OpUpdate:
		r, err := st.UpdateNote(ctx, pl.ID, Update{Title: pl.Title, Body: pl.Body, Tags: pl.Tags}, mo)
		if err != nil {
			return nil, err
		}
		out.ID, out.Replaced, out.SyncState, out.SyncCode = r.ID, r.Replaced, r.State, r.Code
	case OpForgetOne:
		if !approved && !deviceOnly && !p.IsLocal() {
			// No approval possible: an account note is hidden, not
			// deleted; the deletion waits for approval and can be undone.
			id, err := st.HideForForget(ctx, pl.ID)
			if err != nil {
				return nil, err
			}
			out.ID, out.Remote, out.Hidden = id, "unapproved", true
			out.Message = "Hidden on this device. It is deleted only after you approve (`kenaz memory sync approve`); `kenaz memory revert " + id + "` shows it again."
			return json.Marshal(out)
		}
		r, err := st.ForgetNote(ctx, pl.ID, mo)
		if err != nil {
			return nil, err
		}
		out.ID, out.Remote = r.ID, r.Remote
	case OpForgetAll:
		r, err := st.ForgetAll(ctx, mo)
		if err != nil {
			return nil, err
		}
		out.Deleted, out.FleetErasePending = r.Deleted, r.FleetErasePending
	case OpSyncEnable:
		r, err := st.EnableSync(ctx, s.o.ConsentVersion, mo)
		if err != nil {
			return nil, err
		}
		out.Queued = r.Queued
	case OpSyncApprove:
		n, err := st.ApproveOps(ctx, pl.Ops, key)
		if err != nil {
			return nil, err
		}
		out.Queued = n
		out.Message = fmt.Sprintf("Approved %d changes; the relay sends them shortly.", n)
		return json.Marshal(out)
	default:
		return nil, fmt.Errorf("unknown memory operation %q", op)
	}
	out.Message = explain(op, out)
	return json.Marshal(out)
}

// finish records an applied key for operations that span stores.
func (s *Service) finish(ctx context.Context, p Profile, key, op string, out Outcome) ([]byte, error) {
	b, err := json.Marshal(out)
	if err != nil || key == "" {
		return b, err
	}
	st, err := OpenProfile(s.path(p), p)
	if err != nil {
		return nil, err
	}
	defer st.Close()
	return b, st.RecordApplied(ctx, key, op, out)
}

// explain states plainly where the change took effect.
func explain(op string, o Outcome) string {
	switch op {
	case OpForgetOne:
		switch o.Remote {
		case "pending":
			return "Deleted on this device. Its copy in your Kameas Fleet account is being deleted."
		case "unapproved":
			return "Deleted on this device. Deleting its copy in your Kameas Fleet account waits for your approval (`kenaz memory sync approve`)."
		}
		return "Deleted."
	case OpForgetAll:
		if o.FleetErasePending {
			return "Deleted on this device. Erasing this account's memory in Kameas Fleet is in progress; until Fleet confirms, nothing new is synced."
		}
		return "Deleted every memory."
	case OpSyncEnable:
		return "Memory sync is on for this account."
	}
	return StateExplanation(o.SyncState, o.SyncCode)
}

// StateExplanation is the user-facing meaning of a note's sync state.
func StateExplanation(state, code string) string {
	switch state {
	case StatePending:
		return "Saved on this device; queued to sync to your Kameas Fleet account."
	case StateSynced:
		return "Saved on this device and in your Kameas Fleet account."
	case StateUnapproved:
		return "Saved on this device only. Syncing it to your Kameas Fleet account waits for your approval (`kenaz memory sync approve`)."
	case StateRejected:
		if code == "secret_detected" {
			return "Saved on this device only: it looks like it contains a credential, so it will not be synced."
		}
		return "Saved on this device only: Kameas Fleet refused it (" + code + ")."
	case StateDenied:
		return "Saved on this device only: syncing it was denied."
	}
	if code == "scope_not_supported_yet" {
		return "Saved on this device only: project notes do not sync yet (save with project=global to sync)."
	}
	return "Saved on this device only."
}
