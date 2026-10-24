package memory

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
)

// relay simulates the daemon's approval service: the tool side submits
// requests, the "user" approves them, and the registered executor runs
// with exactly the stored payload and key.
type relay struct {
	mu     sync.Mutex
	avail  bool
	exec   map[string]Executor
	recon  map[string]Reconciler
	reqs   map[string]ApprovalRequest
	state  map[string]ApprovalStatus
	res    map[string][]byte
	n      int
	active func(context.Context) Profile
}

func newRelay() *relay {
	return &relay{avail: true, exec: map[string]Executor{}, recon: map[string]Reconciler{}, reqs: map[string]ApprovalRequest{},
		state: map[string]ApprovalStatus{}, res: map[string][]byte{}}
}

func (r *relay) Register(op string, ex Executor)             { r.exec[op] = ex }
func (r *relay) RegisterReconciler(op string, rc Reconciler) { r.recon[op] = rc }

func (r *relay) Submit(_ context.Context, q ApprovalRequest) (Pending, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if !r.avail {
		return Pending{Status: ApprovalUnavailable}, nil
	}
	r.n++
	id := fmt.Sprintf("req-%d", r.n)
	r.reqs[id], r.state[id] = q, ApprovalPending
	return Pending{RequestID: id, Status: ApprovalPending, ApproveURL: "http://localhost:1/approvals/r/" + id}, nil
}

func (r *relay) Status(_ context.Context, id string) (ApprovalStatus, []byte, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.state[id], r.res[id], nil
}

// approve is the verified WebAuthn assertion: the executor runs.
func (r *relay) approve(t *testing.T, ctx context.Context, id string) ([]byte, error) {
	t.Helper()
	r.mu.Lock()
	q := r.reqs[id]
	ex := r.exec[q.Operation]
	r.mu.Unlock()
	if ex == nil {
		t.Fatalf("no executor for %s", q.Operation)
	}
	b, err := ex(ctx, q.Payload, q.IdempotencyKey)
	r.mu.Lock()
	defer r.mu.Unlock()
	if err != nil {
		r.state[id], r.res[id] = ApprovalFailed, []byte(ErrorCode(err))
		return nil, err
	}
	r.state[id], r.res[id] = ApprovalDone, b
	return b, nil
}

type fakeRemote struct {
	enabled []string
	err     error
}

func (r *fakeRemote) EnableSync(_ context.Context, p Profile, v string) error {
	if r.err != nil {
		return r.err
	}
	r.enabled = append(r.enabled, p.UserID+"@"+v)
	return nil
}
func (r *fakeRemote) DisableSync(context.Context, Profile) error { return nil }

var acct = Profile{Env: "dev", OrgID: "org", UserID: "u1"}

type svcEnv struct {
	root   string
	active Profile
	svc    *Service
	relay  *relay
	remote *fakeRemote
}

// newSvc builds the tool-side service and, in the same test, the
// daemon-side executors over the same state root.
func newSvc(t *testing.T, withRelay bool) *svcEnv {
	e := &svcEnv{root: t.TempDir(), active: acct, remote: &fakeRemote{}}
	activeFn := func(context.Context) Profile { return e.active }
	o := ServiceOptions{StateRoot: e.root, Active: activeFn, Remote: e.remote, ConsentVersion: "memory-sync/v1"}
	if withRelay {
		e.relay = newRelay()
		o.Submitter = e.relay
		RegisterExecutors(e.relay, ExecOptions{StateRoot: e.root, Active: activeFn, Remote: e.remote, ConsentVersion: "memory-sync/v1"})
	}
	e.svc = NewService(o)
	return e
}

// syncOn turns sync on for acct (as an approved sync-enable leaves it),
// so its writes reach Fleet and need approval.
func (e *svcEnv) syncOn(t *testing.T) {
	t.Helper()
	s, err := OpenProfile(ProfilePath(e.root, acct), acct)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if _, err := s.EnableSync(context.Background(), "memory-sync/v1", MutateOptions{SyncApproved: true}); err != nil {
		t.Fatal(err)
	}
}

func (e *svcEnv) store(t *testing.T, p Profile) *Store {
	s, err := OpenProfile(ProfilePath(e.root, p), p)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

// The signed-out notebook: device-only edits run directly; account-wide
// or Fleet-reaching operations are refused there.
func TestGateDeviceNotebook(t *testing.T) {
	ctx := context.Background()
	e := newSvc(t, true)
	e.active = Local
	out, err := e.svc.Save(ctx, note(GlobalProject, "t", "b"))
	if err != nil || out.Status != OutcomeDone || out.SyncState != StateLocal || out.Profile != "device" {
		t.Fatalf("save = %+v %v", out, err)
	}
	if e.relay.n != 0 {
		t.Fatal("a device-notebook save was submitted for approval")
	}
	if out, _ := e.svc.ForgetAll(ctx); out.Status != OutcomeApprovalUnavailable || !strings.Contains(out.Message, "kenaz memory purge") {
		t.Fatalf("forget-all = %+v", out)
	}
	if out, _ := e.svc.Adopt(ctx, []string{"x"}); out.Status != "" {
		t.Fatalf("adopt signed out = %+v", out)
	}
	if _, err := e.svc.EnableSync(ctx); !errors.Is(err, ErrLocalProfile) {
		t.Fatalf("enable on device notebook = %v", err)
	}
	if out, err := e.svc.Forget(ctx, out.ID); err != nil || out.Remote != "none" || out.Status != OutcomeDone {
		t.Fatalf("forget = %+v %v", out, err)
	}
}

// No approval possible on an account: device-side effect only, the Fleet
// effect waits as unapproved; account-wide operations are refused.
func TestGateAccountWithoutApproval(t *testing.T) {
	ctx := context.Background()
	e := newSvc(t, true)
	e.relay.avail = false
	st := e.store(t, acct)
	if _, err := st.EnableSync(ctx, "memory-sync/v1", MutateOptions{SyncApproved: true}); err != nil {
		t.Fatal(err)
	}
	out, err := e.svc.Save(ctx, note(GlobalProject, "t", "b"))
	if err != nil || out.Status != OutcomeDone || out.SyncState != StateUnapproved {
		t.Fatalf("save = %+v %v", out, err)
	}
	if s, _ := st.Status(ctx); s.Queue["unapproved"] != 1 || s.Queue["ready"] != 0 {
		t.Fatalf("queue = %+v", s.Queue)
	}
	e.active = Local
	dev, _ := e.svc.Save(ctx, note(GlobalProject, "d", "device note"))
	e.active = acct
	for name, f := range map[string]func() (Outcome, error){
		"enable": func() (Outcome, error) { return e.svc.EnableSync(ctx) }, "forget-all": func() (Outcome, error) { return e.svc.ForgetAll(ctx) },
		"approve": func() (Outcome, error) { return e.svc.ApproveHeld(ctx) }, "adopt": func() (Outcome, error) { return e.svc.Adopt(ctx, []string{dev.ID}) },
	} {
		if out, err := f(); err != nil || out.Status != OutcomeApprovalUnavailable {
			t.Fatalf("%s = %+v %v", name, out, err)
		}
	}
	if len(e.remote.enabled) != 0 {
		t.Fatal("enable reached Fleet without approval")
	}
}

// With the relay: nothing happens until the user approves; the daemon
// executor applies the stored payload once under its key; the
// reconciler reports it applied.
func TestGatePendingThenApproved(t *testing.T) {
	ctx := context.Background()
	e := newSvc(t, true)
	e.syncOn(t)
	out, err := e.svc.Save(ctx, note(GlobalProject, "t", "b"))
	if err != nil || out.Status != OutcomeApprovalPending || out.RequestID == "" || out.ApproveURL == "" || out.ID != "" {
		t.Fatalf("save = %+v %v", out, err)
	}
	if n, _ := e.svc.Count(ctx); n != 0 {
		t.Fatal("saved before approval")
	}
	q := e.relay.reqs[out.RequestID]
	if q.Operation != OpSave || q.UserID != "u1" || q.OrgID != "org" || q.Env != "dev" || q.IdempotencyKey == "" || !strings.Contains(q.Summary, "t") {
		t.Fatalf("request = %+v", q)
	}
	if _, err := e.relay.approve(t, ctx, out.RequestID); err != nil {
		t.Fatal(err)
	}
	// A replay of the same request (relay retry) writes nothing new.
	if _, err := e.relay.exec[OpSave](ctx, q.Payload, q.IdempotencyKey); err != nil {
		t.Fatal(err)
	}
	if n, _ := e.svc.Count(ctx); n != 1 {
		t.Fatalf("count = %d", n)
	}
	if b, applied, err := e.relay.recon[OpSave](ctx, q.Payload, q.IdempotencyKey); err != nil || !applied || !strings.Contains(string(b), `"status":"done"`) {
		t.Fatalf("reconcile = %s %v %v", b, applied, err)
	}
	if _, applied, _ := e.relay.recon[OpSave](ctx, q.Payload, "never-run"); applied {
		t.Fatal("reconciler claims an unapplied key")
	}
	got, err := e.svc.RequestStatus(ctx, out.RequestID)
	if err != nil || got.Status != OutcomeDone || got.ID == "" {
		t.Fatalf("status = %+v %v", got, err)
	}
}

// An approval that lands after an account switch does nothing.
func TestGateAccountChangedBeforeApproval(t *testing.T) {
	ctx := context.Background()
	e := newSvc(t, true)
	e.syncOn(t)
	out, _ := e.svc.Save(ctx, note(GlobalProject, "t", "b"))
	e.active = Profile{Env: "dev", OrgID: "org", UserID: "u2"}
	if _, err := e.relay.approve(t, ctx, out.RequestID); !errors.Is(err, ErrAccountChanged) || ErrorCode(err) != "account_changed" {
		t.Fatalf("approve after switch = %v", err)
	}
	e.active = acct
	if n, _ := e.svc.Count(ctx); n != 0 {
		t.Fatal("wrote into an account after the switch")
	}
}

// The executors never run an always-human operation without approval.
func TestExecuteRefusesUnapprovedAccountWide(t *testing.T) {
	e := newSvc(t, true)
	b, _ := json.Marshal(opPayload{Profile: acct})
	if _, err := e.svc.execute(context.Background(), OpForgetAll, b, "", false, false); err == nil {
		t.Fatal("forget-all ran without approval")
	}
}

// Sync enable, held-work approval, adopt and forget-all through the
// relay's executors.
func TestGateAccountWideOperations(t *testing.T) {
	ctx := context.Background()
	e := newSvc(t, true)
	st := e.store(t, acct)
	approve := func(out Outcome, err error) Outcome {
		t.Helper()
		if err != nil || out.Status != OutcomeApprovalPending {
			t.Fatalf("submit = %+v %v", out, err)
		}
		b, err := e.relay.approve(t, ctx, out.RequestID)
		if err != nil {
			t.Fatal(err)
		}
		var o Outcome
		json.Unmarshal(b, &o)
		return o
	}
	// A note saved while approval was unavailable is held.
	e.relay.avail = false
	st.EnableSync(ctx, "memory-sync/v1", MutateOptions{SyncApproved: true})
	held, _ := e.svc.Save(ctx, note(GlobalProject, "h", "held note"))
	st.DisableSync(ctx, "")
	e.relay.avail = true
	// Sync is off: a save is device data and needs no approval.
	if out, err := e.svc.Save(ctx, note("proj", "p", "project note")); err != nil || out.Status != OutcomeDone {
		t.Fatalf("save with sync off = %+v %v", out, err)
	}

	o := approve(e.svc.EnableSync(ctx))
	if o.Queued != 0 || len(e.remote.enabled) != 1 || e.remote.enabled[0] != "u1@memory-sync/v1" {
		t.Fatalf("enable = %+v %v", o, e.remote.enabled)
	}
	// Enabling never releases changes that wait for their own approval.
	if s, _ := st.Status(ctx); s.Queue["unapproved"] != 1 || s.Notes[StatePending] != 0 {
		t.Fatalf("after enable: %+v", s)
	}
	_ = held

	// Held work approved as one listed request; ops added later are not
	// released by it.
	e.relay.avail = false
	e.svc.Save(ctx, note(GlobalProject, "a", "first held"))
	e.relay.avail = true
	pend, err := e.svc.ApproveHeld(ctx)
	if err != nil || len(e.relay.reqs[pend.RequestID].TargetIDs) != 2 {
		t.Fatalf("approve-held = %+v %v", pend, err)
	}
	e.relay.avail = false
	e.svc.Save(ctx, note(GlobalProject, "b", "second held"))
	e.relay.avail = true
	if b, err := e.relay.approve(t, ctx, pend.RequestID); err != nil || !strings.Contains(string(b), `"queued":2`) {
		t.Fatalf("approve = %s %v", b, err)
	}
	if s, _ := st.Status(ctx); s.Queue["unapproved"] != 1 {
		t.Fatalf("released more than approved: %+v", s.Queue)
	}

	// Adopt a device-notebook note.
	e.active = Local
	dev, _ := e.svc.Save(ctx, note(GlobalProject, "d", "device note"))
	e.active = acct
	if o := approve(e.svc.Adopt(ctx, []string{dev.ID})); o.Queued != 1 {
		t.Fatalf("adopt = %+v", o)
	}
	e.active = Local
	if n, _ := e.svc.Count(ctx); n != 0 {
		t.Fatal("adopted note left in the notebook")
	}
	e.active = acct

	// Forget-all.
	if o := approve(e.svc.ForgetAll(ctx)); o.Deleted == 0 || !o.FleetErasePending {
		t.Fatalf("forget-all = %+v", o)
	}
	if n, _ := e.svc.Count(ctx); n != 0 {
		t.Fatal("forget-all left notes")
	}

	// A refused opt-in changes nothing locally.
	e2 := newSvc(t, true)
	e2.remote.err = errors.New("not in tier")
	out, _ := e2.svc.EnableSync(ctx)
	if _, err := e2.relay.approve(t, ctx, out.RequestID); err == nil {
		t.Fatal("enable succeeded despite a refused opt-in")
	}
	if st, _, _, _ := e2.svc.SyncStatus(ctx); st.Enabled {
		t.Fatal("local sync on after Fleet refused")
	}
}

// Retrieval reads the active account and the device notebook only; after
// sign-out or an account switch the old account's notes leave retrieval.
func TestRetrievalFollowsActiveAccount(t *testing.T) {
	ctx := context.Background()
	e := newSvc(t, false)
	e.svc.Save(ctx, note(GlobalProject, "acct", "ACCOUNTWORD note"))
	e.active = Local
	e.svc.Save(ctx, note(GlobalProject, "dev", "DEVICEWORD note"))
	e.active = acct
	hits, err := e.svc.Search(ctx, "note", AllProjects, 10)
	if err != nil || len(hits) != 2 {
		t.Fatalf("signed in = %v %v", hits, err)
	}
	for _, p := range []Profile{Local, {Env: "dev", OrgID: "org", UserID: "u2"}} {
		e.active = p
		hits, _ := e.svc.Search(ctx, "ACCOUNTWORD", AllProjects, 10)
		if len(hits) != 0 {
			t.Fatalf("%+v still retrieves the old account's note", p)
		}
		if hits, _ := e.svc.Search(ctx, "DEVICEWORD", AllProjects, 10); len(hits) != 1 {
			t.Fatal("device notebook lost from retrieval")
		}
	}
	// Export holds only the active member's notes and the device notebook.
	e.active = Profile{Env: "dev", OrgID: "org", UserID: "u2"}
	var buf bytes.Buffer
	n, err := e.svc.Export(ctx, &buf)
	if err != nil || n != 1 || strings.Contains(buf.String(), "ACCOUNTWORD") {
		t.Fatalf("export = %d %v %s", n, err, buf.String())
	}
}

// With sync off, an account's save, edit and delete of a note Fleet never
// had are device data: they run directly and nothing is submitted. A
// delete of a note Fleet may hold still needs approval, because it queues
// a Fleet forget even with sync off.
func TestGateOnlyWhatReachesFleet(t *testing.T) {
	ctx := context.Background()
	e := newSvc(t, true)
	out, err := e.svc.Save(ctx, note(GlobalProject, "t", "local only"))
	if err != nil || out.Status != OutcomeDone || out.SyncState != StateLocal {
		t.Fatalf("save = %+v %v", out, err)
	}
	nb := "edited locally"
	if up, err := e.svc.Update(ctx, out.ID, Update{Body: &nb}); err != nil || up.Status != OutcomeDone {
		t.Fatalf("update = %+v %v", up, err)
	}
	if f, err := e.svc.Forget(ctx, out.ID); err != nil || f.Status != OutcomeDone || f.Hidden || f.Remote != "none" {
		t.Fatalf("forget = %+v %v", f, err)
	}
	if e.relay.n != 0 {
		t.Fatalf("%d device-only writes were submitted for approval", e.relay.n)
	}

	// A note Fleet holds (synced earlier), sync now off: deleting it
	// reaches Fleet, so it is submitted.
	e.syncOn(t)
	st := e.store(t, acct)
	r, err := st.SaveNote(ctx, note(GlobalProject, "s", "synced"), SaveOptions{SyncApproved: true})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.db.Exec(`UPDATE records SET sync_state = 'synced', server_seq = '7' WHERE id = ?`, r.ID); err != nil {
		t.Fatal(err)
	}
	st.DisableSync(ctx, "")
	f, err := e.svc.Forget(ctx, r.ID)
	if err != nil || f.Status != OutcomeApprovalPending || e.relay.n != 1 {
		t.Fatalf("forget of a synced note = %+v %v (submitted %d)", f, err, e.relay.n)
	}
}

// The idempotency key is derived from the operation, the payload and the
// store state: an agent's retry gets the same key (the same approval),
// the same request after the store changed gets a new one.
func TestApprovalKeyIdempotent(t *testing.T) {
	ctx := context.Background()
	e := newSvc(t, true)
	e.syncOn(t)
	a, _ := e.svc.ForgetAll(ctx)
	b, _ := e.svc.ForgetAll(ctx)
	ka, kb := e.relay.reqs[a.RequestID].IdempotencyKey, e.relay.reqs[b.RequestID].IdempotencyKey
	if ka == "" || ka != kb {
		t.Fatalf("retry keys differ: %q %q", ka, kb)
	}
	s1, _ := e.svc.Save(ctx, note(GlobalProject, "x", "same"))
	s2, _ := e.svc.Save(ctx, note(GlobalProject, "x", "same"))
	if e.relay.reqs[s1.RequestID].IdempotencyKey != e.relay.reqs[s2.RequestID].IdempotencyKey {
		t.Fatal("a retried save got a new key")
	}
	if _, err := e.relay.approve(t, ctx, a.RequestID); err != nil {
		t.Fatal(err)
	}
	c, _ := e.svc.ForgetAll(ctx)
	if e.relay.reqs[c.RequestID].IdempotencyKey == ka {
		t.Fatal("a forget-all after the store changed reused the applied key")
	}
}

// More notes than one approval may list go in successive batches of at
// most ApprovalBatch: each request lists (and its payload binds, with
// every note's full text) only its batch, and running the command again
// after approving reaches the rest. Nothing is ever unreachable.
func TestBatchedApprovals(t *testing.T) {
	for _, n := range []int{65, 200} {
		t.Run(fmt.Sprint(n), func(t *testing.T) {
			ctx := context.Background()
			e := newSvc(t, true)
			st := e.store(t, acct)
			body := strings.Repeat("x", MaxBodyBytes-16)
			for i := 0; i < n; i++ {
				if _, err := st.SaveNote(ctx, note(GlobalProject, fmt.Sprint("n", i), fmt.Sprintf("%04d %s", i, body)), SaveOptions{}); err != nil {
					t.Fatal(err)
				}
			}
			uploaded := 0
			for round := 0; uploaded < n; round++ {
				if round > n {
					t.Fatal("batches never finished")
				}
				out, err := e.svc.EnableSync(ctx)
				if err != nil || out.Status != OutcomeApprovalPending {
					t.Fatalf("enable round %d = %+v %v", round, out, err)
				}
				q := e.relay.reqs[out.RequestID]
				var pl opPayload
				json.Unmarshal(q.Payload, &pl)
				if len(pl.Records) == 0 || len(pl.Records) > ApprovalBatch || len(q.TargetIDs) != len(pl.Records) || len(q.Payload) > 1<<20 {
					t.Fatalf("round %d: %d records, %d targets, %d bytes", round, len(pl.Records), len(q.TargetIDs), len(q.Payload))
				}
				for _, r := range pl.Records {
					if r.Body == "" || ContentHash(r.Body) != r.Hash {
						t.Fatalf("round %d: a listed note is not bound by its text", round)
					}
				}
				if want := n - uploaded - len(pl.Records); out.Remaining != want {
					t.Fatalf("round %d: remaining = %d, want %d", round, out.Remaining, want)
				}
				b, err := e.relay.approve(t, ctx, out.RequestID)
				if err != nil {
					t.Fatal(err)
				}
				var o Outcome
				json.Unmarshal(b, &o)
				uploaded += o.Queued
			}
			if s, _ := st.Status(ctx); s.Notes[StatePending] != n || s.Notes[StateLocal] != 0 {
				t.Fatalf("after all batches: %+v", s.Notes)
			}
		})
	}
}

// A note retitled after sync-enable or adopt was prepared (device-only
// edits need no approval) is not uploaded or moved under the old
// approval: the approved title is checked as well as the content.
func TestApprovedTitleBound(t *testing.T) {
	ctx := context.Background()
	e := newSvc(t, true)
	st := e.store(t, acct)
	r, err := st.SaveNote(ctx, note(GlobalProject, "shown title", "body"), SaveOptions{})
	if err != nil {
		t.Fatal(err)
	}
	out, err := e.svc.EnableSync(ctx)
	if err != nil || out.Status != OutcomeApprovalPending {
		t.Fatalf("enable = %+v %v", out, err)
	}
	nt := "retitled after prepare"
	if _, err := st.UpdateNote(ctx, r.ID, Update{Title: &nt}, MutateOptions{}); err != nil {
		t.Fatal(err)
	}
	b, err := e.relay.approve(t, ctx, out.RequestID)
	if err != nil {
		t.Fatal(err)
	}
	var o Outcome
	json.Unmarshal(b, &o)
	if o.Queued != 0 {
		t.Fatalf("a retitled note was uploaded under the old approval: %+v", o)
	}

	nb, err := Open(e.svc.path(Local))
	if err != nil {
		t.Fatal(err)
	}
	id, _ := nb.Save(ctx, note(GlobalProject, "device title", "device body"))
	nb.Close()
	ad, err := e.svc.Adopt(ctx, []string{id})
	if err != nil || ad.Status != OutcomeApprovalPending {
		t.Fatalf("adopt = %+v %v", ad, err)
	}
	nb, _ = Open(e.svc.path(Local))
	nb.UpdateNote(ctx, id, Update{Title: &nt}, MutateOptions{})
	nb.Close()
	b, err = e.relay.approve(t, ctx, ad.RequestID)
	if err != nil {
		t.Fatal(err)
	}
	json.Unmarshal(b, &o)
	if o.Queued != 0 {
		t.Fatalf("a retitled note was moved under the old approval: %+v", o)
	}
}

// A held Fleet forget whose note row is already gone still names the note
// on the approval page (its title and content hash, kept device-only and
// removed with the op), and approving it releases the forget.
func TestHeldForgetKeepsLabel(t *testing.T) {
	ctx := context.Background()
	e := newSvc(t, true)
	st := e.store(t, acct)
	r, err := st.SaveNote(ctx, note(GlobalProject, "old runbook", "steps"), SaveOptions{})
	if err != nil {
		t.Fatal(err)
	}
	st.db.Exec(`UPDATE records SET sync_state = 'synced', server_seq = '3' WHERE id = ?`, r.ID)
	if _, err := st.ForgetNote(ctx, r.ID, MutateOptions{}); err != nil { // unapproved forget
		t.Fatal(err)
	}
	ops, err := st.HeldOps(ctx)
	if err != nil || len(ops) != 1 || ops[0].Op != OpForget || ops[0].Title != "old runbook" || ops[0].Hash != ContentHash("steps") {
		t.Fatalf("held ops = %+v %v", ops, err)
	}
	if n, err := st.ApproveOps(ctx, ops, ""); err != nil || n != 1 {
		t.Fatalf("approve = %d %v", n, err)
	}
	if _, err := st.ForgetAll(ctx, MutateOptions{}); err != nil {
		t.Fatal(err)
	}
	var left int
	st.db.QueryRow(`SELECT count(*) FROM meta WHERE key LIKE 'held_label:%'`).Scan(&left)
	if left != 0 {
		t.Fatalf("%d held labels left after forget-all", left)
	}
}

// Held-forget labels never outlive their op, whichever path removes it
// (revert, a pulled tombstone, a reset, an ack): every write transaction
// drops labels whose op is gone.
func TestHeldLabelsPrunedWithTheirOps(t *testing.T) {
	ctx := context.Background()
	e := newSvc(t, true)
	st := e.store(t, acct)
	r, err := st.SaveNote(ctx, note(GlobalProject, "gone", "x"), SaveOptions{})
	if err != nil {
		t.Fatal(err)
	}
	st.db.Exec(`UPDATE records SET sync_state = 'synced', server_seq = '5' WHERE id = ?`, r.ID)
	if _, err := st.ForgetNote(ctx, r.ID, MutateOptions{}); err != nil {
		t.Fatal(err)
	}
	labels := func() int {
		var n int
		st.db.QueryRow(`SELECT count(*) FROM meta WHERE key LIKE 'held_label:%'`).Scan(&n)
		return n
	}
	if labels() != 1 {
		t.Fatalf("labels = %d, want 1", labels())
	}
	// Another path removes the op (as a pulled tombstone or reset does).
	if _, err := st.db.Exec(`DELETE FROM outbox WHERE record_id = ?`, r.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := st.SaveNote(ctx, note(GlobalProject, "next", "y"), SaveOptions{}); err != nil {
		t.Fatal(err)
	}
	if labels() != 0 {
		t.Fatalf("a label outlived its op: %d", labels())
	}
}

// A held upsert is listed with the title its payload sends, not the
// note's current title.
func TestHeldUpsertShowsPayloadTitle(t *testing.T) {
	ctx := context.Background()
	e := newSvc(t, true)
	e.syncOn(t)
	st := e.store(t, acct)
	if _, err := st.SaveNote(ctx, note(GlobalProject, "sent title", "body"), SaveOptions{}); err != nil { // unapproved
		t.Fatal(err)
	}
	st.db.Exec(`UPDATE records SET title = 'renamed underneath'`)
	ops, err := st.HeldOps(ctx)
	if err != nil || len(ops) != 1 || ops[0].Title != "sent title" {
		t.Fatalf("held ops = %+v %v", ops, err)
	}
}
