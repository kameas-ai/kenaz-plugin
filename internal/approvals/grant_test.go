package approvals

import (
	"context"
	"net/http"
	"sort"
	"strings"
	"testing"
	"time"
)

// A standing grant runs a new request through Approve itself: once, with the stored
// payload, and on record as the grant's doing. Without the grant's say the request
// waits, and a request the person approves carries no such mark.
func TestAutoApproverRunsTheApprovalPath(t *testing.T) {
	h := newHarness(t)
	grant := true
	offered := 0
	var settled []Status
	h.m.RegisterAutoApprover("context.share", func(_ context.Context, who Identity, id string, payload []byte) (func(Status), bool) {
		offered++
		if who != alice || !ValidID(id) || !strings.Contains(string(payload), "Flaky e2e") {
			t.Errorf("offered %v %s %s", who, id, payload)
		}
		return func(st Status) { settled = append(settled, st) }, grant
	})
	ctx := context.Background()
	id, st, err := h.m.Prepare(ctx, req(alice, "k1", "one"))
	if err != nil || st != StatusDone || h.execs.Load() != 1 || len(settled) != 1 || settled[0] != StatusDone {
		t.Fatalf("%s %v, %d runs, settled %v", st, err, h.execs.Load(), settled)
	}
	if _, res, _ := h.m.Status(ctx, id); !strings.Contains(string(res), `"approved_by":"grant"`) || !strings.Contains(string(res), `"id":"node-1"`) {
		t.Fatalf("%s", res)
	}
	// Asking again returns the finished request; the grant isn't offered it twice.
	if again, st, err := h.m.Prepare(ctx, req(alice, "k1", "one")); err != nil || again != id || st != StatusDone || offered != 1 || h.execs.Load() != 1 {
		t.Fatalf("%s %s %v offered=%d", again, st, err, offered)
	}
	// It can't be approved a second time either.
	if st, err := h.m.Approve(ctx, id); err != nil || st != StatusDone || h.execs.Load() != 1 {
		t.Fatalf("%s %v", st, err)
	}

	grant = false
	id2, st, err := h.m.Prepare(ctx, req(alice, "k2", "two"))
	if err != nil || st != StatusPending || h.execs.Load() != 1 || len(settled) != 1 {
		t.Fatalf("%s %v", st, err)
	}
	if st, err := h.m.Approve(ctx, id2); err != nil || st != StatusDone {
		t.Fatalf("%s %v", st, err)
	}
	if _, res, _ := h.m.Status(ctx, id2); strings.Contains(string(res), "approved_by") {
		t.Fatalf("a person's approval is marked as the grant's: %s", res)
	}
	by := map[string]string{}
	for _, i := range h.m.List(ctx) {
		by[i.RequestID] = i.ApprovedBy
	}
	if by[id] != ApprovedByGrant || by[id2] != "" {
		t.Fatalf("%v", by)
	}
	// The mark survives a restart of the relay.
	if _, res, _ := h.open().Status(ctx, id); !strings.Contains(string(res), `"approved_by":"grant"`) {
		t.Fatalf("%s", res)
	}
}

// What Prepare answers is what then happens. A grant check that hasn't answered in
// time is a no: the request is pending and stays pending, however late the check
// comes back. A yes whose write is still running is reported as executing, never
// pending, and the write finishes on its own.
func TestAutoApproverAnswerIsNeverFalse(t *testing.T) {
	oldB, oldD := prepareBudget, decideWait
	prepareBudget, decideWait = 400*time.Millisecond, 100*time.Millisecond
	t.Cleanup(func() { prepareBudget, decideWait = oldB, oldD })
	h := newHarness(t)
	ctx := context.Background()

	// A slow check: it honours its deadline, as a Fleet read does.
	slow := true
	settled := make(chan Status, 4)
	h.m.RegisterAutoApprover("context.share", func(ctx context.Context, _ Identity, _ string, _ []byte) (func(Status), bool) {
		if slow {
			<-ctx.Done()
			return nil, false
		}
		return func(st Status) { settled <- st }, true
	})
	start := time.Now()
	id, st, err := h.m.Prepare(ctx, req(alice, "slow", "one"))
	if err != nil || st != StatusPending {
		t.Fatalf("%s %v", st, err)
	}
	if took := time.Since(start); took > prepareBudget+time.Second {
		t.Fatalf("Prepare took %s", took)
	}
	time.Sleep(150 * time.Millisecond)
	if st, _, _ := h.m.Status(ctx, id); st != StatusPending || h.execs.Load() != 0 {
		t.Fatalf("told pending, then %s with %d runs", st, h.execs.Load())
	}

	// A covered request whose executor is held: executing now, done once released.
	slow = false
	release := make(chan struct{})
	running := make(chan struct{}, 4)
	h.execHook = func(context.Context, []byte, string) ([]byte, error) {
		running <- struct{}{}
		<-release
		return []byte(`{"id":"node-2"}`), nil
	}
	start = time.Now()
	id2, st, err := h.m.Prepare(ctx, req(alice, "held", "two"))
	if err != nil || st != StatusExecuting {
		t.Fatalf("%s %v", st, err)
	}
	if took := time.Since(start); took > prepareBudget+time.Second {
		t.Fatalf("Prepare took %s", took)
	}
	if st, res, _ := h.m.Status(ctx, id2); st != StatusExecuting || !strings.Contains(string(res), `"approved_by":"grant"`) {
		t.Fatalf("%s %s", st, res)
	}
	// The person can't run it a second time meanwhile.
	if st, err := h.m.Approve(ctx, id2); err != nil || st != StatusExecuting {
		t.Fatalf("%s %v", st, err)
	}
	// The executor runs in its own goroutine, and "executing" is true from the
	// claim on: Prepare may answer before that goroutine has started (when the
	// store's I/O used up the budget). So wait for the run rather than expect it
	// to have begun, then check it began once.
	select {
	case <-running:
	case <-time.After(10 * time.Second):
		t.Fatalf("told executing, but the executor never ran (%d runs)", h.execs.Load())
	}
	if n := h.execs.Load(); n != 1 {
		t.Fatalf("%d runs while executing", n)
	}
	close(release)
	if got := <-settled; got != StatusDone {
		t.Fatalf("settled %s", got)
	}
	if st, _, _ := h.m.Status(ctx, id2); st != StatusDone || h.execs.Load() != 1 {
		t.Fatalf("%s, %d runs", st, h.execs.Load())
	}
	// Nothing ran it a second time: not the person's press above, and not a
	// press after it finished.
	if st, err := h.m.Approve(ctx, id2); err != nil || st != StatusDone {
		t.Fatalf("%s %v", st, err)
	}
	select {
	case <-running:
		t.Fatal("the executor ran a second time")
	case <-time.After(50 * time.Millisecond):
	}
	if n := h.execs.Load(); n != 1 {
		t.Fatalf("%d runs", n)
	}
}

// A grant is asked only about its own operation, and a panic in the check or in the
// executor leaves the request where a person can deal with it.
func TestAutoApproverIsPerOperationAndSurvivesPanics(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	h.m.Register("context.other", h.exec)
	h.m.RegisterRenderer("context.other", renderTest)
	asked := 0
	h.m.RegisterAutoApprover("context.other", func(context.Context, Identity, string, []byte) (func(Status), bool) {
		asked++
		return nil, true
	})
	// The same payload, under an operation with no grant of its own.
	if _, st, err := h.m.Prepare(ctx, req(alice, "k1", "one")); err != nil || st != StatusPending || asked != 0 || h.execs.Load() != 0 {
		t.Fatalf("%s %v asked=%d", st, err, asked)
	}
	other := req(alice, "k2", "two")
	other.Operation = "context.other"
	if _, st, err := h.m.Prepare(ctx, other); err != nil || st != StatusDone || asked != 1 {
		t.Fatalf("%s %v asked=%d", st, err, asked)
	}

	h.m.RegisterAutoApprover("context.share", func(context.Context, Identity, string, []byte) (func(Status), bool) {
		panic("check")
	})
	if _, st, err := h.m.Prepare(ctx, req(alice, "k3", "three")); err != nil || st != StatusPending {
		t.Fatalf("a panicking check: %s %v", st, err)
	}
	var settled Status
	h.m.RegisterAutoApprover("context.share", func(context.Context, Identity, string, []byte) (func(Status), bool) {
		return func(st Status) { settled = st }, true
	})
	h.execHook = func(context.Context, []byte, string) ([]byte, error) { panic("executor") }
	id, st, err := h.m.Prepare(ctx, req(alice, "k4", "four"))
	if err != nil || st != StatusReconciling || settled != StatusReconciling {
		t.Fatalf("a panicking executor: %s %v settled=%s", st, err, settled)
	}
	// Not stuck as running: the usual reconcile path owns it now.
	if !h.m.claimRun(id) {
		t.Fatal("the request is still claimed after the panic")
	}
	h.m.releaseRun(id)
}

// The control API a tool process holds can prepare, list and read status, and nothing
// else: no route approves, and none gives or changes a grant.
func TestControlRoutesCannotApproveOrGrant(t *testing.T) {
	h := newHarness(t)
	var control []string
	h.m.Mount(func(string, http.Handler) {}, func(pattern string, _ http.HandlerFunc) { control = append(control, pattern) })
	sort.Strings(control)
	want := []string{"GET " + ControlList, "GET " + ControlStatus + "{id}", "POST " + ControlPrepare}
	if strings.Join(control, "|") != strings.Join(want, "|") {
		t.Fatalf("control routes changed: %v", control)
	}
	for _, p := range control {
		for _, bad := range []string{"approve", "grant", "auto", "allow"} {
			if strings.Contains(strings.ToLower(p), bad) {
				t.Errorf("control route %q", p)
			}
		}
	}
}
