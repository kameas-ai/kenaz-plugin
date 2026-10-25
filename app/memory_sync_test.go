package app

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/kameas-ai/kenaz-plugin/fleetapi"
	"github.com/kameas-ai/kenaz-plugin/fleetapi/fleetapitest"
	"github.com/kameas-ai/kenaz-plugin/fleetapi/stub"
	"github.com/kameas-ai/kenaz-plugin/internal/approvals"
	memorytools "github.com/kameas-ai/kenaz-plugin/internal/mcp/tools/memory"
	"github.com/kameas-ai/kenaz-plugin/internal/memory"
	"github.com/kameas-ai/kenaz-plugin/notices"
)

const syncCanary = "MEMSYNCCANARYz9Q4w0000000000000077"

// memProvider is an official-flavor provider whose memory client talks to
// a contract fake as whoever is signed in.
type memProvider struct {
	fleetapi.Provider
	fleet *fleetapitest.MemoryFleet
	mu    sync.Mutex
	sess  *fleetapi.Session
}

func (p *memProvider) Flavor() fleetapi.Flavor { return fleetapi.FlavorOfficial }
func (p *memProvider) Auth() fleetapi.Auth     { return memAuth{Auth: p.Provider.Auth(), p: p} }
func (p *memProvider) Me() fleetapi.Me         { return memMe{} }
func (p *memProvider) Memory() fleetapi.Memory { return memRouter{p} }

func (p *memProvider) signIn(user string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if user == "" {
		p.sess = nil
		return
	}
	p.sess = &fleetapi.Session{Env: fleetapi.EnvDev, OrgID: "org-1", UserID: user}
}

type memAuth struct {
	fleetapi.Auth
	p *memProvider
}

func (a memAuth) Session(context.Context) (fleetapi.Session, error) {
	a.p.mu.Lock()
	defer a.p.mu.Unlock()
	if a.p.sess == nil {
		return fleetapi.Session{}, fleetapi.ErrNotSignedIn
	}
	return *a.p.sess, nil
}

type memMe struct{ fleetapi.Me }

func (memMe) Capabilities(context.Context) (fleetapi.Capabilities, error) {
	return fleetapi.Capabilities{Keys: map[string]bool{fleetapi.CapMemorySync: true}}, nil
}

// memRouter sends with the active session's identity, as a real adapter
// uses the active token.
type memRouter struct{ p *memProvider }

func (r memRouter) c() fleetapi.Memory {
	r.p.mu.Lock()
	defer r.p.mu.Unlock()
	if r.p.sess == nil {
		return r.p.fleet.Client("", "")
	}
	return r.p.fleet.Client(r.p.sess.OrgID, r.p.sess.UserID)
}
func (r memRouter) Settings(ctx context.Context) (fleetapi.MemorySettings, error) {
	return r.c().Settings(ctx)
}
func (r memRouter) PutSettings(ctx context.Context, u fleetapi.MemorySettingsUpdate) (fleetapi.MemorySettings, error) {
	return r.c().PutSettings(ctx, u)
}
func (r memRouter) Pull(ctx context.Context, q fleetapi.MemoryPullRequest) (fleetapi.MemoryPullResponse, error) {
	return r.c().Pull(ctx, q)
}
func (r memRouter) Push(ctx context.Context, q fleetapi.MemoryPushRequest) (fleetapi.MemoryPushResponse, error) {
	return r.c().Push(ctx, q)
}
func (r memRouter) Forget(ctx context.Context, q fleetapi.MemoryForgetRequest) (fleetapi.MemoryForgetResponse, error) {
	return r.c().Forget(ctx, q)
}
func (r memRouter) ForgetAll(ctx context.Context, q fleetapi.MemoryForgetAllRequest) (fleetapi.MemoryForgetAllResponse, error) {
	return r.c().ForgetAll(ctx, q)
}
func (r memRouter) Export(ctx context.Context, fn func(fleetapi.MemoryRecord) error) (fleetapi.MemoryExportTrailer, error) {
	return r.c().Export(ctx, fn)
}

func waitUntil(t *testing.T, cond func() bool) bool {
	t.Helper()
	for end := time.Now().Add(5 * time.Second); time.Now().Before(end); time.Sleep(20 * time.Millisecond) {
		if cond() {
			return true
		}
	}
	return false
}

// The relay's memory sync lane end to end: an approved enable and save
// reach the signed-in user's Fleet store; the canary never appears in the
// relay log, the lane status or anyone else's store (the privacy canary);
// sign-out stops the worker and removes the account's notes from
// retrieval; another account never gets them.
func TestMemorySyncLaneEndToEnd(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	f := fleetapitest.NewMemoryFleet()
	p := &memProvider{Provider: stub.New(), fleet: f}
	p.signIn("alice")
	mp := memoryPaths{StateRoot: root, DB: memory.PathIn(root), p: p}

	// The user approves at once: the relay runs the daemon-side executor
	// with the stored payload and key.
	relay := &instantRelay{exec: map[string]memory.Executor{}, res: map[string][]byte{}}
	memory.RegisterExecutors(relay, memory.ExecOptions{StateRoot: root, Active: mp.active, Remote: memoryRemote(p), ConsentVersion: notices.MemorySync().ID})
	svc := mp.service(relay)
	if out, err := svc.Save(ctx, memory.NewNote{ProjectKey: memory.GlobalProject, Title: "canary " + syncCanary, Body: "remember " + syncCanary, Source: memory.SourceCLI}); err != nil || out.Status != memory.OutcomeDone {
		t.Fatalf("save = %+v %v", out, err)
	}
	if out, err := svc.EnableSync(ctx); err != nil || out.Status != memory.OutcomeDone || out.Queued != 1 {
		t.Fatalf("enable = %+v %v", out, err)
	}

	var logs bytes.Buffer
	l := &memorySyncLane{p: p, stateRoot: root, log: slog.New(slog.NewTextHandler(&logs, &slog.HandlerOptions{Level: slog.LevelDebug})),
		holder: "test-relay", caps: &capsCache{p: p}, kick: make(chan struct{}, 1), period: 30 * time.Millisecond}
	if err := l.Start(ctx); err != nil {
		t.Fatal(err)
	}
	defer l.Stop()
	if !waitUntil(t, func() bool { return len(f.LiveContents("org-1", "alice")) == 1 }) {
		t.Fatalf("note never synced; lane %+v logs %s", l.Status(), logs.String())
	}
	if c := f.LiveContents("org-1", "alice"); !strings.Contains(strings.Join(values(c), ""), syncCanary) {
		t.Fatal("positive control: the synced note should carry the canary")
	}
	st, _ := json.Marshal(l.Status())
	if strings.Contains(logs.String(), syncCanary) || strings.Contains(string(st), syncCanary) {
		t.Fatal("memory text in the relay log or lane status")
	}
	if !waitUntil(t, func() bool { return l.Status().State == "running" && l.Status().Uploaded >= 1 }) {
		t.Fatalf("lane status %+v", l.Status())
	}

	// Sign-out: the worker stops; the account's notes leave retrieval.
	p.signIn("")
	l.Resume() // kick the supervisor
	if !waitUntil(t, func() bool { return l.Status().State == "stopped" }) {
		t.Fatalf("worker still running after sign-out: %+v", l.Status())
	}
	if hits, _ := mp.service(nil).Search(ctx, syncCanary, memory.AllProjects, 10); len(hits) != 0 {
		t.Fatal("signed-out retrieval still returns the account's note")
	}
	// Another account: no worker for it, nothing of alice's.
	p.signIn("bob")
	l.Resume()
	time.Sleep(100 * time.Millisecond)
	if hits, _ := mp.service(nil).Search(ctx, syncCanary, memory.AllProjects, 10); len(hits) != 0 {
		t.Fatal("another account retrieves alice's note")
	}
	if len(f.LiveContents("org-1", "bob")) != 0 {
		t.Fatal("bob's Fleet store has content")
	}
	if strings.Contains(logs.String(), syncCanary) {
		t.Fatal("memory text in the relay log")
	}
	// Signing back in as alice brings them back.
	p.signIn("alice")
	if hits, _ := mp.service(nil).Search(ctx, syncCanary, memory.AllProjects, 10); len(hits) != 1 {
		t.Fatal("alice's note did not come back after sign-in")
	}
}

func values(m map[string]string) []string {
	var out []string
	for _, v := range m {
		out = append(out, v)
	}
	return out
}

// The unofficial build cannot sync: the remote is nil and the lane is not
// installed, and the CLI says so instead of pretending.
func TestMemorySyncUnavailableInStubBuild(t *testing.T) {
	if memoryRemote(stub.New()) != nil || memorySyncAvailable(stub.New()) {
		t.Fatal("stub build claims memory sync")
	}
	if notices.MemorySync().ID != "memory-sync/v2" {
		t.Fatal("notice edition")
	}
}

// instantRelay approves every request immediately (as if the user did on
// the approval page) by running the registered executor.
type instantRelay struct {
	mu   sync.Mutex
	exec map[string]memory.Executor
	res  map[string][]byte
	n    int
}

func (r *instantRelay) Register(op string, ex memory.Executor)       { r.exec[op] = ex }
func (r *instantRelay) RegisterReconciler(string, memory.Reconciler) {}
func (r *instantRelay) Submit(ctx context.Context, q memory.ApprovalRequest) (memory.Pending, error) {
	b, err := r.exec[q.Operation](ctx, q.Payload, q.IdempotencyKey)
	if err != nil {
		return memory.Pending{}, err
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.n++
	id := fmt.Sprint("apr_", r.n)
	r.res[id] = b
	return memory.Pending{RequestID: id, Status: memory.ApprovalDone}, nil
}
func (r *instantRelay) Status(_ context.Context, id string) (memory.ApprovalStatus, []byte, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return memory.ApprovalDone, r.res[id], nil
}

type fakeVerifier struct{}

func (fakeVerifier) VerifyIdentity(context.Context, func(string) error, string) (approvals.Identity, error) {
	return approvals.Identity{}, errors.New("not in this test")
}

// The real approval service in the daemon: every memory operation has an
// executor (Prepare refuses unknown operations), an account write is
// stored pending with an approval page link and changes nothing; without
// any way to approve, device-only edits still apply (their Fleet effect
// held) and account-wide operations are refused.
func TestMemoryApprovalsWiring(t *testing.T) {
	ctx := context.Background()
	stateRoot := t.TempDir()
	stateDir := filepath.Join(stateRoot, "dev")
	f := fleetapitest.NewMemoryFleet()
	p := &memProvider{Provider: stub.New(), fleet: f}
	p.signIn("alice")
	newManager := func(v approvals.IdentityVerifier) *approvals.Manager {
		m, err := approvals.New(approvals.Options{Dir: filepath.Join(t.TempDir(), "approvals"), Identity: sessionIdentity(p), Verifier: v,
			StoreKey: bytes.Repeat([]byte{7}, 32),
			BaseURL:  func() string { return "http://localhost:4318" }})
		if err != nil {
			t.Fatal(err)
		}
		registerMemoryExecutors(m, p, stateDir, nil)
		return m
	}
	mp := memoryPaths{StateRoot: stateRoot, DB: memory.PathIn(stateRoot), p: p}

	// An enrolment path exists: requests wait on the approval page.
	m := newManager(fakeVerifier{})
	svc := mp.service(memorytools.Submitter(m))
	nb, err := memory.Open(memory.PathIn(stateRoot))
	if err != nil {
		t.Fatal(err)
	}
	devID, _ := nb.Save(ctx, memory.NewNote{ProjectKey: memory.GlobalProject, Title: "d", Body: "device", Source: memory.SourceCLI})
	nb.Close()
	// With sync off a save is device data: it runs without an approval.
	if out, err := svc.Save(ctx, memory.NewNote{ProjectKey: memory.GlobalProject, Title: "t", Body: "b", Source: memory.SourceCLI}); err != nil ||
		out.Status != memory.OutcomeDone || out.SyncState != memory.StateLocal {
		t.Fatalf("save with sync off = %+v %v", out, err)
	}
	for name, op := range map[string]func() (memory.Outcome, error){
		"enable":     func() (memory.Outcome, error) { return svc.EnableSync(ctx) },
		"forget-all": func() (memory.Outcome, error) { return svc.ForgetAll(ctx) },
		"adopt":      func() (memory.Outcome, error) { return svc.Adopt(ctx, []string{devID}) },
	} {
		out, err := op()
		if err != nil || out.Status != memory.OutcomeApprovalPending || !strings.HasPrefix(out.ApproveURL, "http://localhost:4318/approvals/r/") {
			t.Fatalf("%s = %+v %v", name, out, err)
		}
	}
	if len(f.LiveContents("org-1", "alice")) != 0 {
		t.Fatal("something was written before approval")
	}
	for _, r := range m.List(ctx) {
		if r.Status != approvals.StatusPending {
			t.Fatalf("request %+v", r)
		}
	}

	// No authenticator and no way to enroll: approval_unavailable.
	m2 := newManager(nil)
	svc2 := mp.service(memorytools.Submitter(m2))
	out, err := svc2.Save(ctx, memory.NewNote{ProjectKey: memory.GlobalProject, Title: "t", Body: "local only", Source: memory.SourceCLI})
	if err != nil || out.Status != memory.OutcomeDone || out.SyncState != memory.StateLocal {
		t.Fatalf("save without approvals = %+v %v", out, err)
	}
	if out, _ := svc2.EnableSync(ctx); out.Status != memory.OutcomeApprovalUnavailable {
		t.Fatalf("enable without approvals = %+v", out)
	}
	if out, _ := svc2.ApproveHeld(ctx); out.Status != memory.OutcomeDone {
		t.Fatalf("approve with nothing held = %+v", out)
	}
	if err := memoryExecError(memory.OpSyncEnable, errors.New("connection reset")); !errors.Is(err, approvals.ErrOutcomeUnknown) {
		t.Fatalf("network failure on enable = %v", err)
	}
	if err := memoryExecError(memory.OpSave, memory.ErrAccountChanged); err.Error() != "write refused: account_changed" {
		t.Fatalf("definite refusal = %v", err)
	}
}

type perUserMe struct {
	fleetapi.Me
	p     *memProvider
	calls *int
}

func (m perUserMe) Capabilities(context.Context) (fleetapi.Capabilities, error) {
	*m.calls++
	m.p.mu.Lock()
	defer m.p.mu.Unlock()
	return fleetapi.Capabilities{Keys: map[string]bool{fleetapi.CapMemorySync: m.p.sess != nil && m.p.sess.UserID == "alice"}}, nil
}

type perUserProvider struct {
	*memProvider
	calls *int
}

func (p perUserProvider) Me() fleetapi.Me { return perUserMe{p: p.memProvider, calls: p.calls} }

// The capability cache never serves one account's entitlement to another.
func TestCapsCacheKeyedByAccount(t *testing.T) {
	ctx := context.Background()
	mp := &memProvider{Provider: stub.New(), fleet: fleetapitest.NewMemoryFleet()}
	calls := 0
	c := &capsCache{p: perUserProvider{mp, &calls}}
	mp.signIn("alice")
	if caps, _ := c.get(ctx); !caps.Has(fleetapi.CapMemorySync) {
		t.Fatal("alice should be entitled")
	}
	mp.signIn("bob")
	if caps, _ := c.get(ctx); caps.Has(fleetapi.CapMemorySync) {
		t.Fatal("bob got alice's cached entitlement")
	}
	mp.signIn("alice")
	c.get(ctx)
	if calls != 2 {
		t.Fatalf("calls = %d, want 2 (alice cached)", calls)
	}
}

// The approval page for sync-enable and adopt lists exactly the notes the
// stored payload moves, with their full text, never text a caller
// supplied; a body that does not match its hash is refused.
func TestRenderMemoryListsPayloadRecords(t *testing.T) {
	long := strings.Repeat("full text ", 100)
	mk := func(body2 string) []byte {
		recs := []map[string]string{
			{"record_id": "m1", "op": "memory.adopt", "title": "first", "body": "one", "content_hash": memory.ContentHash("one")},
			{"record_id": "m2", "op": "memory.adopt", "title": "second", "body": body2, "content_hash": memory.ContentHash(long)},
		}
		b, _ := json.Marshal(map[string]any{"profile": map[string]string{"env": "dev", "org_id": "org-1", "user_id": "alice"}, "records": recs})
		return b
	}
	for _, op := range []string{memory.OpSyncEnable, memory.OpAdopt} {
		r, err := renderMemory(op)(mk(long))
		if err != nil {
			t.Fatalf("%s: %v", op, err)
		}
		if len(r.Targets) != 2 || r.Targets[0] != "m1" || r.Targets[1] != "m2" {
			t.Fatalf("%s targets = %v", op, r.Targets)
		}
		if len(r.Details) != 2 || !strings.Contains(r.Details[0].Label, `"first"`) || r.Details[1].Text != long {
			t.Fatalf("%s details = %+v", op, r.Details)
		}
		if _, err := renderMemory(op)(mk("not the hashed text")); err == nil {
			t.Fatalf("%s: a body that does not match its hash rendered", op)
		}
	}
	if _, err := renderMemory(memory.OpAdopt)([]byte(`{"profile":{"user_id":"alice"}}`)); err == nil {
		t.Fatal("adopt with no records rendered")
	}
}

// More than 64 notes can be approved: sync-enable, sync-approve and adopt
// --all each prepare a request the real approval service accepts (at most
// 64 targets, every listed note's full text bound by the payload hash) and
// report how many remain for the next request.
func TestBatchedMemoryApprovalsAccepted(t *testing.T) {
	for _, n := range []int{65, 200} {
		t.Run(fmt.Sprint(n), func(t *testing.T) {
			ctx := context.Background()
			stateRoot := t.TempDir()
			p := &memProvider{Provider: stub.New(), fleet: fleetapitest.NewMemoryFleet()}
			p.signIn("alice")
			m, err := approvals.New(approvals.Options{Dir: filepath.Join(t.TempDir(), "approvals"), Identity: sessionIdentity(p), Verifier: fakeVerifier{},
				StoreKey: bytes.Repeat([]byte{7}, 32), BaseURL: func() string { return "http://localhost:4318" }})
			if err != nil {
				t.Fatal(err)
			}
			registerMemoryExecutors(m, p, filepath.Join(stateRoot, "dev"), nil)
			mp := memoryPaths{StateRoot: stateRoot, DB: memory.PathIn(stateRoot), p: p}
			svc := mp.service(memorytools.Submitter(m))
			acct := memory.Profile{Env: "dev", OrgID: "org-1", UserID: "alice"}
			st, err := memory.OpenProfile(memory.ProfilePath(stateRoot, acct), acct)
			if err != nil {
				t.Fatal(err)
			}
			nb, err := memory.Open(memory.PathIn(stateRoot))
			if err != nil {
				t.Fatal(err)
			}
			for i := 0; i < n; i++ {
				body := fmt.Sprintf("note %d %s", i, strings.Repeat("y", 4000))
				if _, err := st.SaveNote(ctx, memory.NewNote{ProjectKey: memory.GlobalProject, Title: fmt.Sprint("t", i), Body: body, Source: memory.SourceCLI}, memory.SaveOptions{}); err != nil {
					t.Fatal(err)
				}
				if _, err := nb.Save(ctx, memory.NewNote{ProjectKey: memory.GlobalProject, Title: fmt.Sprint("d", i), Body: body, Source: memory.SourceCLI}); err != nil {
					t.Fatal(err)
				}
			}
			nb.Close()
			ids, _ := svc.LocalIDs(ctx)
			for name, run := range map[string]func() (memory.Outcome, error){
				"enable": func() (memory.Outcome, error) { return svc.EnableSync(ctx) },
				"adopt":  func() (memory.Outcome, error) { return svc.Adopt(ctx, ids) },
			} {
				out, err := run()
				if err != nil || out.Status != memory.OutcomeApprovalPending || out.Remaining != n-64 {
					t.Fatalf("%s = %+v %v", name, out, err)
				}
				v, err := m.ViewRequest(ctx, out.RequestID)
				if err != nil || len(v.Targets) != 64 || len(v.Details) != 64 || !strings.Contains(v.Details[0].Text, strings.Repeat("y", 4000)) {
					t.Fatalf("%s view: %d targets, %d details, %v", name, len(v.Targets), len(v.Details), err)
				}
			}
			// Held changes: n unapproved uploads.
			if _, err := st.EnableSync(ctx, "memory-sync/v1", memory.MutateOptions{}); err != nil {
				t.Fatal(err)
			}
			for i := 0; i < n; i++ {
				if _, err := st.SaveNote(ctx, memory.NewNote{ProjectKey: memory.GlobalProject, Title: fmt.Sprint("h", i), Body: fmt.Sprint("held ", i), Source: memory.SourceCLI}, memory.SaveOptions{}); err != nil {
					t.Fatal(err)
				}
			}
			st.Close()
			out, err := svc.ApproveHeld(ctx)
			if err != nil || out.Status != memory.OutcomeApprovalPending || out.Remaining != n-64 {
				t.Fatalf("approve held = %+v %v", out, err)
			}
		})
	}
}
