package memory

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
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

// approve is the person pressing Approve: the executor runs.
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

// Forget-all's approval discloses that the erase reaches the account's
// other devices, never-synced notes included.
func TestForgetAllDisclosesOtherDevices(t *testing.T) {
	ctx := context.Background()
	e := newSvc(t, true)
	e.syncOn(t)
	out, err := e.svc.ForgetAll(ctx)
	if err != nil || out.Status != OutcomeApprovalPending {
		t.Fatalf("forget-all = %+v %v", out, err)
	}
	if q := e.relay.reqs[out.RequestID]; !strings.HasSuffix(q.Impact, ForgetAllOtherDevices) {
		t.Fatalf("impact = %q", q.Impact)
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

// A forget-all clears capture's waiting events, and failing to doesn't lose it.
func TestForgetAllClearsWhatCaptureHolds(t *testing.T) {
	ctx := context.Background()
	for _, fail := range []bool{false, true} {
		e := newSvc(t, true)
		called := 0
		purge := func() error {
			called++
			if fail {
				return errors.New("disk")
			}
			return nil
		}
		active := func(context.Context) Profile { return e.active }
		RegisterExecutors(e.relay, ExecOptions{StateRoot: e.root, Active: active, Remote: e.remote, ConsentVersion: "memory-sync/v1", AfterForgetAll: purge})
		st, err := OpenProfile(ProfilePath(e.root, acct), acct)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := st.SaveNote(ctx, note(GlobalProject, "t", "b"), SaveOptions{SyncApproved: true}); err != nil {
			t.Fatal(err)
		}
		st.Close()
		out, err := e.svc.ForgetAll(ctx)
		if err != nil || out.RequestID == "" {
			t.Fatalf("%+v %v", out, err)
		}
		b, err := e.relay.approve(t, ctx, out.RequestID)
		if err != nil {
			t.Fatalf("fail=%v: %v", fail, err)
		}
		if called != 1 {
			t.Errorf("purged %d times", called)
		}
		if n, _ := e.svc.Count(ctx); n != 0 {
			t.Errorf("fail=%v: %d notes left", fail, n)
		}
		if got := strings.Contains(string(b), "couldn't be cleared"); got != fail {
			t.Errorf("fail=%v: %s", fail, b)
		}
	}
}

// With no approval possible the note is hidden, and the output says Kameas Fleet still
// holds it only when that can be so.
func TestForgetWithNoApprovalSaysWhatFleetHolds(t *testing.T) {
	ctx := context.Background()
	e := newSvc(t, false)
	e.syncOn(t)
	st, err := OpenProfile(ProfilePath(e.root, acct), acct)
	if err != nil {
		t.Fatal(err)
	}
	held, err := st.SaveNote(ctx, note(GlobalProject, "held", "held text"), SaveOptions{SyncApproved: true})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.db.ExecContext(ctx, `UPDATE records SET sync_state = 'synced', server_seq = '5' WHERE id = ?`, held.ID); err != nil {
		t.Fatal(err)
	}
	never, err := st.SaveNote(ctx, note(GlobalProject, "never sent", "never sent text"), SaveOptions{SyncApproved: true})
	if err != nil {
		t.Fatal(err)
	}
	st.Close()
	const says = "Kameas Fleet still holds it."
	out, err := e.svc.Forget(ctx, held.ID)
	if err != nil || !out.Hidden || !strings.Contains(out.Message, "Hidden on this device. "+says) {
		t.Errorf("held: %+v %v", out, err)
	}
	out, err = e.svc.Forget(ctx, never.ID)
	if err != nil || !out.Hidden || strings.Contains(out.Message, says) || !strings.Contains(out.Message, "Hidden on this device.") {
		t.Errorf("never sent: %+v %v", out, err)
	}
}

func strandedStore(t *testing.T) (*Store, string, Lease) {
	t.Helper()
	old := ProjectScopeRelease
	ProjectScopeRelease = true
	t.Cleanup(func() { ProjectScopeRelease = old })
	ctx := context.Background()
	st, err := OpenProfile(filepath.Join(t.TempDir(), "m.db"), acct)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	if _, err := st.EnableSync(ctx, "memory-sync/v4", MutateOptions{SyncApproved: true}); err != nil {
		t.Fatal(err)
	}
	const key = "rk1-0123456789abcdef0123456789abcdef"
	if err := st.SetProject(ctx, key, "11111111-1111-4111-8111-111111111111", "active", ""); err != nil {
		t.Fatal(err)
	}
	l, err := st.AcquireLease(ctx, "h", time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	return st, key, l
}

func opStates(t *testing.T, st *Store) map[string]string {
	t.Helper()
	rows, err := st.db.QueryContext(context.Background(), `SELECT CAST(payload AS TEXT), state FROM outbox ORDER BY op_id`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	out := map[string]string{}
	for rows.Next() {
		var p, s string
		if err := rows.Scan(&p, &s); err != nil {
			t.Fatal(err)
		}
		out[p] = s
	}
	return out
}

// An edit swept up in an older relay's refusal comes back asking, not ready to send.
func TestReleaseLeavesAnUnapprovedEditAsking(t *testing.T) {
	st, key, l := strandedStore(t)
	ctx := context.Background()
	r, err := st.SaveNote(ctx, NewNote{ProjectKey: key, Title: "t", Body: "b", Source: SourceCLI}, SaveOptions{SyncApproved: true})
	if err != nil {
		t.Fatal(err)
	}
	b, err := st.LeasePushBatch(ctx, l)
	if err != nil || len(b.Ops) != 1 {
		t.Fatalf("%v %+v", err, b)
	}
	title := "an edit nobody approved"
	if _, err := st.UpdateNote(ctx, r.ID, Update{Title: &title}, MutateOptions{}); err != nil {
		t.Fatal(err)
	}
	no := false
	if _, err := st.ApplyPushResults(ctx, l, b.Ops, []PushResult{{ID: r.ID, Status: "rejected", Code: "invalid_field", Field: "scope_id", Retry: &no}}); err != nil {
		t.Fatal(err)
	}
	if err := st.ReleaseStrandedProjectNotes(ctx); err != nil {
		t.Fatal(err)
	}
	found := false
	for p, state := range opStates(t, st) {
		if strings.Contains(p, "nobody approved") {
			found = true
			if state != "unapproved" {
				t.Errorf("the unapproved edit is %s", state)
			}
		} else if state != "ready" {
			t.Errorf("the create is %s", state)
		}
	}
	if !found {
		t.Fatal("the edit's op is gone")
	}
}

// A blocked automatic note goes only under the grant, and no note goes if it no longer
// passes the scan.
func TestReleaseKeepsItsGates(t *testing.T) {
	st, key, l := strandedStore(t)
	ctx := context.Background()
	id, _, err := st.SaveAuto(ctx, AutoNote{ProjectKey: key, Title: "o", Body: "b", Source: SourceCLI, Kind: KindObservation})
	if err != nil {
		t.Fatal(err)
	}
	if err := st.GrantSaves(ctx, ""); err != nil {
		t.Fatal(err)
	}
	if q, err := st.QueueAutoBacklog(ctx, l); q != 1 || err != nil {
		t.Fatal(q, err)
	}
	strand := func() {
		t.Helper()
		if _, err := st.db.ExecContext(ctx, `UPDATE outbox SET state = 'blocked', ever_sent = 1, attempt = 1, last_code = 'invalid_field'`); err != nil {
			t.Fatal(err)
		}
		if _, err := st.db.ExecContext(ctx, `UPDATE records SET sync_state = 'rejected', sync_code = 'invalid_field'`); err != nil {
			t.Fatal(err)
		}
	}
	strand()
	if err := st.RevokeSaveGrant(ctx); err != nil {
		t.Fatal(err)
	}
	if err := st.ReleaseStrandedProjectNotes(ctx); err != nil {
		t.Fatal(err)
	}
	for _, state := range opStates(t, st) {
		if state != "blocked" {
			t.Errorf("released with the grant gone: %s", state)
		}
	}
	if err := st.GrantSaves(ctx, ""); err != nil {
		t.Fatal(err)
	}
	if _, err := st.db.ExecContext(ctx, `UPDATE records SET content = 'key AKIAABCDEFGHIJKLMNOP' WHERE id = ?`, id); err != nil {
		t.Fatal(err)
	}
	if err := st.ReleaseStrandedProjectNotes(ctx); err != nil {
		t.Fatal(err)
	}
	for _, state := range opStates(t, st) {
		if state != "blocked" {
			t.Errorf("released despite the scan: %s", state)
		}
	}
	if _, err := st.db.ExecContext(ctx, `UPDATE records SET content = 'b' WHERE id = ?`, id); err != nil {
		t.Fatal(err)
	}
	if err := st.ReleaseStrandedProjectNotes(ctx); err != nil {
		t.Fatal(err)
	}
	for _, state := range opStates(t, st) {
		if state != "ready" {
			t.Errorf("not released under the grant: %s", state)
		}
	}
}

// A pulled erase with no time still records one.
func TestPulledEraseWithNoTimestampRecordsOne(t *testing.T) {
	st, _, l := strandedStore(t)
	ctx := context.Background()
	gen, err := st.BeginReset(ctx, l, "erased", "")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.CommitReset(ctx, l, gen, "c1"); err != nil {
		t.Fatal(err)
	}
	if st.ErasedAt(ctx).IsZero() {
		t.Error("no erase time")
	}
}

// What a forget-all covers, with the clock ahead, and without.
func TestEraseCutoff(t *testing.T) {
	ctx := context.Background()
	st, err := OpenProfile(filepath.Join(t.TempDir(), "m.db"), acct)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	t0 := time.Now()
	if st.EraseCutoffNow(ctx).Covers(t0.Add(24 * time.Hour)) {
		t.Error("a store never erased covers nothing")
	}
	ahead := t0.Add(24 * time.Hour)
	st.SetClock(func() time.Time { return ahead })
	if _, err := st.ForgetAll(ctx, MutateOptions{SyncApproved: true}); err != nil {
		t.Fatal(err)
	}
	st.SetClock(func() time.Time { return t0 })
	cut := st.EraseCutoffNow(ctx)
	for _, c := range []struct {
		name string
		at   time.Time
		want bool
	}{
		{"before the corrected time", t0.Add(-time.Minute), true},
		{"stamped before the erase on the clock that was ahead", ahead.Add(-time.Hour), true},
		{"a little ahead of the clock, right after correcting", t0.Add(time.Second), true},
		{"after the erase on the clock that was ahead", ahead.Add(2 * time.Minute), true},
	} {
		if got := cut.Covers(c.at); got != c.want {
			t.Errorf("%s: %v", c.name, got)
		}
	}
	// New activity once the clock has moved on is new.
	st.SetClock(func() time.Time { return t0.Add(time.Minute) })
	cut = st.EraseCutoffNow(ctx)
	if cut.Covers(t0.Add(30 * time.Second)) {
		t.Error("new activity was refused")
	}
	if cut.Covers(t0.Add(time.Minute)) {
		t.Error("an event from now was refused")
	}

	// With the clock right at the forget-all: only a stamp well ahead of it.
	right, err := OpenProfile(filepath.Join(t.TempDir(), "m.db"), acct)
	if err != nil {
		t.Fatal(err)
	}
	defer right.Close()
	right.SetClock(func() time.Time { return t0 })
	if _, err := right.ForgetAll(ctx, MutateOptions{SyncApproved: true}); err != nil {
		t.Fatal(err)
	}
	right.SetClock(func() time.Time { return t0.Add(time.Second) })
	c := right.EraseCutoffNow(ctx)
	if c.Covers(t0.Add(30 * time.Second)) {
		t.Error("small clock movement dropped an event")
	}
	if !c.Covers(t0.Add(time.Hour)) {
		t.Error("a stamp an hour ahead passed")
	}
}

// sendingStore has automatic notes queued: two whose outcome is unknown, if unknown is
// set, and one never sent.
func sendingStore(t *testing.T, e *svcEnv, unknown bool) {
	t.Helper()
	ctx := context.Background()
	st, err := OpenProfile(ProfilePath(e.root, acct), acct)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	if err := st.GrantSaves(ctx, ""); err != nil {
		t.Fatal(err)
	}
	if err := st.tx(ctx, func(tx *sql.Tx) error { return saveClass(ctx, tx, syncClassAuto, classState{Confirmed: true}) }); err != nil {
		t.Fatal(err)
	}
	save := func(n int) {
		for i := 0; i < n; i++ {
			if _, _, err := st.SaveAuto(ctx, AutoNote{ProjectKey: GlobalProject, Title: "o", Body: fmt.Sprint("note ", n, i, time.Now().UnixNano()), Source: SourceCLI, Kind: KindObservation}); err != nil {
				t.Fatal(err)
			}
		}
	}
	l, err := st.AcquireLease(ctx, "h", time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if unknown {
		save(2)
		if q, err := st.QueueAutoBacklog(ctx, l); q != 2 || err != nil {
			t.Fatal(q, err)
		}
		b, err := st.LeasePushBatch(ctx, l)
		if err != nil || len(b.Ops) != 2 {
			t.Fatalf("%+v %v", b, err)
		}
		if err := st.ReturnUnknown(ctx, l, b.Ops, 0); err != nil {
			t.Fatal(err)
		}
	}
	save(1)
	if q, err := st.QueueAutoBacklog(ctx, l); q != 1 || err != nil {
		t.Fatal(q, err)
	}
	if err := st.ReleaseLease(ctx, l); err != nil {
		t.Fatal(err)
	}
}

func queuedNow(t *testing.T, e *svcEnv) int {
	t.Helper()
	st, err := OpenProfile(ProfilePath(e.root, acct), acct)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	s, err := st.Status(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	return s.Queue["ready"]
}

// Stopping says how many notes stay queued because Fleet may hold them, and only then.
func TestStoppingSaysWhatFinishesSending(t *testing.T) {
	ctx := context.Background()
	stop := map[string]struct {
		run func(*svcEnv) (Outcome, error)
		two string
	}{
		"auto off": {func(e *svcEnv) (Outcome, error) { return e.svc.SetSyncAuto(ctx, false) },
			"2 notes may already have reached Kameas Fleet, so they'll finish sending. You can delete any of them with `kenaz memory forget`."},
		"sync off": {func(e *svcEnv) (Outcome, error) { return e.svc.DisableSync(ctx) },
			"2 notes may already have reached Kameas Fleet. They'll finish sending if you turn sync back on. You can delete any of them with `kenaz memory forget`."},
	}
	for name, c := range stop {
		for _, unknown := range []bool{false, true} {
			e := newSvc(t, false)
			e.syncOn(t)
			sendingStore(t, e, unknown)
			out, err := c.run(e)
			if err != nil {
				t.Fatal(err)
			}
			says := strings.Contains(out.Message, c.two)
			if says != unknown || (!unknown && strings.Contains(out.Message, "may already have reached")) {
				t.Errorf("%s, unknown=%v: %q", name, unknown, out.Message)
			}
			want := 0
			if unknown {
				want = 2
			}
			if got := queuedNow(t, e); got != want {
				t.Errorf("%s, unknown=%v: %d stay queued, want %d", name, unknown, got, want)
			}
		}
	}
}

// Legal's four sentences, and nothing at all for none.
func TestStopWording(t *testing.T) {
	for _, c := range []struct{ got, want string }{
		{StillSendingMessage(0), ""},
		{StillQueuedMessage(0), ""},
		{StillSendingMessage(1), "1 note may already have reached Kameas Fleet, so it'll finish sending. You can delete it with `kenaz memory forget`."},
		{StillSendingMessage(3), "3 notes may already have reached Kameas Fleet, so they'll finish sending. You can delete any of them with `kenaz memory forget`."},
		{StillQueuedMessage(1), "1 note may already have reached Kameas Fleet. It'll finish sending if you turn sync back on. You can delete it with `kenaz memory forget`."},
		{StillQueuedMessage(3), "3 notes may already have reached Kameas Fleet. They'll finish sending if you turn sync back on. You can delete any of them with `kenaz memory forget`."},
	} {
		if c.got != c.want {
			t.Errorf("%q, want %q", c.got, c.want)
		}
	}
}

// A new notice ends the grant too, and says the same.
func TestNewNoticeSaysWhatFinishesSending(t *testing.T) {
	ctx := context.Background()
	e := newSvc(t, false)
	e.syncOn(t)
	sendingStore(t, e, true)
	st, err := OpenProfile(ProfilePath(e.root, acct), acct)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	r, err := st.EnableSync(ctx, "memory-sync/v9", MutateOptions{SyncApproved: true})
	if err != nil || r.StillSending != 2 {
		t.Errorf("%+v %v", r, err)
	}
}

// An unknown outcome, then a refusal from an older relay, a release, and the grant ending:
// the note may be on Fleet, so it isn't treated as never sent.
func TestReleaseKeepsAnUnknownOutcomeOnRecord(t *testing.T) {
	st, key, l := strandedStore(t)
	ctx := context.Background()
	if err := st.GrantSaves(ctx, ""); err != nil {
		t.Fatal(err)
	}
	id, _, err := st.SaveAuto(ctx, AutoNote{ProjectKey: key, Title: "o", Body: "b", Source: SourceCLI, Kind: KindObservation})
	if err != nil {
		t.Fatal(err)
	}
	if q, err := st.QueueAutoBacklog(ctx, l); q != 1 || err != nil {
		t.Fatal(q, err)
	}
	reset := func() {
		t.Helper()
		if _, err := st.db.ExecContext(ctx, `DELETE FROM meta WHERE key LIKE 'sync_class:%'`); err != nil {
			t.Fatal(err)
		}
		if _, err := st.db.ExecContext(ctx, `UPDATE outbox SET next_attempt_at = 0`); err != nil {
			t.Fatal(err)
		}
	}
	reset()
	b, err := st.LeasePushBatch(ctx, l)
	if err != nil || len(b.Ops) != 1 {
		t.Fatalf("%+v %v", b, err)
	}
	if err := st.ReturnUnknown(ctx, l, b.Ops, 0); err != nil {
		t.Fatal(err)
	}
	reset()
	if b, err = st.LeasePushBatch(ctx, l); err != nil || len(b.Ops) != 1 {
		t.Fatalf("%+v %v", b, err)
	}
	no := false
	if _, err := st.ApplyPushResults(ctx, l, b.Ops, []PushResult{{ID: id, Status: "rejected", Code: "invalid_field", Field: "scope_id", Retry: &no}}); err != nil {
		t.Fatal(err)
	}
	if err := st.ReleaseStrandedProjectNotes(ctx); err != nil {
		t.Fatal(err)
	}
	unknown, err := st.EndSaveGrant(ctx)
	if err != nil || unknown != 1 {
		t.Errorf("told of %d, %v", unknown, err)
	}
	if may, err := st.FleetMayHold(ctx, id); err != nil || !may {
		t.Errorf("may hold %v %v", may, err)
	}
}
