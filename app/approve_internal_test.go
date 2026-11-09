package app

import (
	"bytes"
	"context"
	"encoding/json"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	"github.com/kameas-ai/kenaz-plugin/fleetapi/fleetapitest"
	"github.com/kameas-ai/kenaz-plugin/fleetapi/stub"
	"github.com/kameas-ai/kenaz-plugin/internal/approvals"
	memorytools "github.com/kameas-ai/kenaz-plugin/internal/mcp/tools/memory"
	"github.com/kameas-ai/kenaz-plugin/internal/memory"
	"github.com/kameas-ai/kenaz-plugin/internal/usertext"
	"github.com/kameas-ai/kenaz-plugin/notices"
)

// A write that reaches Fleet waits for a person, through the real executors: not on
// preparing, not on a sweep, not on a restart. The Fleet writes (context_share,
// context_share_file, context_promote, skills_publish) have the same test beside their
// tools, counting pushes.
func TestMemoryWritesWaitForApproval(t *testing.T) {
	ctx := context.Background()
	f := &memProvider{Provider: stub.New(), fleet: fleetapitest.NewMemoryFleet()}
	f.signIn("alice")
	key := bytes.Repeat([]byte{7}, 32)
	dir, stateDir := t.TempDir(), t.TempDir()
	newManager := func() *approvals.Manager {
		m, err := approvals.New(approvals.Options{Dir: dir, Identity: sessionIdentity(f), StoreKey: key, BaseURL: func() string { return "http://localhost:1" }})
		if err != nil {
			t.Fatal(err)
		}
		registerMemoryExecutors(m, f, stateDir, nil)
		return m
	}
	m := newManager()
	mp := memoryPaths{StateRoot: stateDir, DB: memory.PathIn(stateDir), p: f}
	svc := mp.service(memorytools.Submitter(m))
	saved, err := svc.Save(ctx, memory.NewNote{ProjectKey: memory.GlobalProject, Title: "t", Body: "before", Source: memory.SourceCLI})
	if err != nil {
		t.Fatal(err)
	}
	plan, err := svc.PlanTypedEnable(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := svc.EnableSyncTyped(ctx, notices.MemorySync().ID, plan); err != nil {
		t.Fatal(err)
	}
	body := "after"
	var ids []string
	for op, p := range map[string]func() (memory.Outcome, error){
		memory.OpForgetOne: func() (memory.Outcome, error) { return svc.Forget(ctx, saved.ID) },
		memory.OpForgetAll: func() (memory.Outcome, error) { return svc.ForgetAll(ctx) },
		memory.OpUpdate:    func() (memory.Outcome, error) { return svc.Update(ctx, saved.ID, memory.Update{Body: &body}) },
	} {
		out, err := p()
		if err != nil || out.RequestID == "" || out.Status != memory.OutcomeApprovalPending {
			t.Fatalf("%s: %+v %v", op, out, err)
		}
		if !strings.Contains(out.Message, "kenaz approve") || strings.Contains(out.Message, "security key") {
			t.Fatalf("%s: %s", op, out.Message)
		}
		ids = append(ids, out.RequestID)
	}
	for _, mm := range []*approvals.Manager{m, newManager()} {
		for _, id := range ids {
			if s, _, err := mm.Status(ctx, id); err != nil || s != approvals.StatusPending {
				t.Fatalf("%s %s %v", id, s, err)
			}
		}
	}
	if hits, _ := mp.service(nil).Search(ctx, "before", memory.AllProjects, 5); len(hits) != 1 {
		t.Fatalf("the note changed before approval: %+v", hits)
	}
}

// A served relay for `kenaz approve`: the real page routes on localhost, with control routes
// behind the nonce as the relay has them.
type approveRelay struct {
	m   *approvals.Manager
	srv *httptest.Server
	env Env
	out *bytes.Buffer
	err *bytes.Buffer
}

func newApproveRelay(t *testing.T) (*approveRelay, *int) {
	t.Helper()
	writes := 0
	p := &memProvider{Provider: stub.New(), fleet: fleetapitest.NewMemoryFleet()}
	p.signIn("alice")
	home := t.TempDir()
	r := &approveRelay{out: &bytes.Buffer{}, err: &bytes.Buffer{}}
	r.env = devEnv(home, p, r.out, r.err)
	var port string
	var err error
	r.m, err = approvals.New(approvals.Options{Dir: t.TempDir(), Identity: sessionIdentity(p), StoreKey: bytes.Repeat([]byte{7}, 32),
		BaseURL: func() string { return "http://localhost:" + port }})
	if err != nil {
		t.Fatal(err)
	}
	r.m.Register("test.write", func(context.Context, []byte, string) ([]byte, error) { writes++; return []byte(`{"deleted":3}`), nil })
	r.m.RegisterRenderer("test.write", renderApproveTest)
	r.m.Register(memory.OpForgetAll, func(context.Context, []byte, string) ([]byte, error) { writes++; return []byte(`{"deleted":3}`), nil })
	r.m.RegisterRenderer(memory.OpForgetAll, renderApproveTest)
	mux := http.NewServeMux()
	const nonce = "nonce"
	r.m.Mount(func(pat string, h http.Handler) { mux.Handle(pat, h) }, func(pat string, h http.HandlerFunc) {
		mux.HandleFunc(pat, func(w http.ResponseWriter, q *http.Request) {
			if q.Header.Get("Authorization") != "Bearer "+nonce {
				http.Error(w, "no", http.StatusUnauthorized)
				return
			}
			h(w, q)
		})
	})
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	r.srv = &httptest.Server{Listener: ln, Config: &http.Server{Handler: mux}}
	r.srv.Start()
	t.Cleanup(r.srv.Close)
	_, port, _ = net.SplitHostPort(ln.Addr().String())
	old := relayRequests
	relayRequests = func(Env, resolved) requestLister {
		return localRequests{r.m, "http://localhost:" + port + approvals.PagePrefix}
	}
	t.Cleanup(func() { relayRequests = old })
	return r, &writes
}

type localRequests struct {
	m   *approvals.Manager
	url string
}

func (l localRequests) List(ctx context.Context) (approvals.WireList, error) {
	return approvals.WireList{PageURL: l.url, Requests: l.m.List(ctx)}, nil
}

func (l localRequests) Status(ctx context.Context, id string) (approvals.Status, []byte, error) {
	return l.m.Status(ctx, id)
}

func renderApproveTest(p []byte) (approvals.Rendered, error) {
	var v struct{ Body string }
	if err := json.Unmarshal(p, &v); err != nil {
		return approvals.Rendered{}, err
	}
	return approvals.Rendered{Summary: "Share a note", Impact: "Everyone can read it.", Details: []approvals.Detail{{Label: "note", Text: v.Body}}}, nil
}

func (r *approveRelay) prepare(t *testing.T, op, key, body string) string {
	t.Helper()
	payload, _ := json.Marshal(map[string]string{"body": body})
	id, st, err := r.m.Prepare(context.Background(), approvals.Request{Operation: op, Env: "dev", OrgID: "org-1", UserID: "alice", Payload: payload, IdempotencyKey: key})
	if err != nil || st != approvals.StatusPending {
		t.Fatalf("%s %v", st, err)
	}
	return id
}

func (r *approveRelay) run(t *testing.T, terminal bool, input string, args ...string) int {
	t.Helper()
	oldT, oldR := memoryStdinTerminal, memoryStdin
	memoryStdinTerminal = func() bool { return terminal }
	memoryStdin = strings.NewReader(input)
	defer func() { memoryStdinTerminal, memoryStdin = oldT, oldR }()
	r.out.Reset()
	r.err.Reset()
	if args[0] == "approve" {
		return runApprove(r.env, args[1:])
	}
	return runDecline(r.env, args[1:])
}

// `kenaz approve` shows the exact content and takes Enter; it writes once.
func TestApproveCommand(t *testing.T) {
	r, writes := newApproveRelay(t)
	id := r.prepare(t, "test.write", "k1", "the exact text")
	if code := r.run(t, true, "\n", "approve"); code != ExitOK {
		t.Fatalf("exit %d: %s %s", code, r.out, r.err)
	}
	out := r.out.String()
	for _, want := range []string{"the exact text", "Share a note", "Everyone can read it.", usertext.PromptApprove, "Done."} {
		if !strings.Contains(out, want) {
			t.Errorf("no %q in %s", want, out)
		}
	}
	if *writes != 1 {
		t.Fatalf("%d writes", *writes)
	}
	if code := r.run(t, true, "\n", "approve", id); code != ExitOK || !strings.Contains(r.out.String(), "Not waiting") || *writes != 1 {
		t.Fatalf("again: %d %s %d", code, r.out, *writes)
	}
}

// A no leaves it waiting; decline closes it. Several waiting are listed, and nothing runs.
func TestApproveChoices(t *testing.T) {
	r, writes := newApproveRelay(t)
	id := r.prepare(t, "test.write", "k1", "one")
	for _, answer := range []string{"n\n", "no\n", "nope\n", ""} {
		if code := r.run(t, true, answer, "approve"); code != ExitOK || !strings.Contains(r.out.String(), usertext.NothingChanged) {
			t.Fatalf("%q: %d %s", answer, code, r.out)
		}
	}
	two := r.prepare(t, "test.write", "k2", "two")
	if code := r.run(t, true, "\n", "approve"); code != ExitOK || !strings.Contains(r.out.String(), id) || !strings.Contains(r.out.String(), two) {
		t.Fatalf("%d %s", code, r.out)
	}
	if code := r.run(t, true, "\n", "decline", two); code != ExitOK || !strings.Contains(r.out.String(), "Declined") {
		t.Fatalf("%d %s", code, r.out)
	}
	if s, _, _ := r.m.Status(context.Background(), two); s != approvals.StatusDenied {
		t.Fatal(s)
	}
	if *writes != 0 {
		t.Fatalf("%d writes", *writes)
	}
	// With no terminal, it's refused and names the command.
	if code := r.run(t, false, "y\n", "approve", id); code != ExitError || !strings.Contains(r.err.String(), "terminal") || *writes != 0 {
		t.Fatalf("%d %s", code, r.err)
	}
}

// A real pipe of yes, and /dev/null, are refused for real.
func TestApproveRefusesPipeAndDevNull(t *testing.T) {
	r, writes := newApproveRelay(t)
	r.prepare(t, "test.write", "k1", "x")
	pr, pw, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer pr.Close()
	pw.WriteString("y\ny\n")
	pw.Close()
	null, err := os.Open(os.DevNull)
	if err != nil {
		t.Fatal(err)
	}
	defer null.Close()
	old := os.Stdin
	defer func() { os.Stdin = old }()
	for _, f := range []*os.File{pr, null} {
		os.Stdin = f
		if code := runApprove(r.env, nil); code != ExitError || *writes != 0 {
			t.Fatalf("%s: %d, %d writes", f.Name(), code, *writes)
		}
	}
}

// Forget-all has its own words, and Enter takes it.
func TestApproveForgetAllPrompt(t *testing.T) {
	r, writes := newApproveRelay(t)
	r.prepare(t, memory.OpForgetAll, "k1", "x")
	if code := r.run(t, true, "\n", "approve"); code != ExitOK || *writes != 1 {
		t.Fatalf("%d %s", code, r.err)
	}
	if !strings.Contains(r.out.String(), usertext.PromptForgetAll) || !strings.Contains(r.out.String(), `"deleted":3`) {
		t.Fatalf("%s", r.out)
	}
}
