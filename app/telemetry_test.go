package app

import (
	"bytes"
	"context"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	logsv1 "go.opentelemetry.io/proto/otlp/logs/v1"
	"google.golang.org/protobuf/proto"

	"github.com/kameas-ai/kenaz-plugin/fleetapi"
	"github.com/kameas-ai/kenaz-plugin/fleetapi/allowlist"
	"github.com/kameas-ai/kenaz-plugin/fleetapi/stub"
	"github.com/kameas-ai/kenaz-plugin/internal/canon"
	"github.com/kameas-ai/kenaz-plugin/internal/consent"
	"github.com/kameas-ai/kenaz-plugin/internal/daemon"
	"github.com/kameas-ai/kenaz-plugin/internal/fixtures"
	"github.com/kameas-ai/kenaz-plugin/internal/fleetschema"
	"github.com/kameas-ai/kenaz-plugin/internal/forward"
	"github.com/kameas-ai/kenaz-plugin/internal/project"
	"github.com/kameas-ai/kenaz-plugin/internal/spool"
	"github.com/kameas-ai/kenaz-plugin/internal/variant"
)

// laneFleet is an in-memory Fleet for the telemetry-lane end-to-end test.
// Everything it does not override falls through to the stub.
type laneFleet struct {
	fleetapi.Provider

	mu        sync.Mutex
	optedIn   bool
	approvals []fleetapi.Approval
	posted    []fleetapi.ProjectedBatch
	optinPuts int
	// approvalsErr / postErr, when set, are returned by pending approvals
	// and uploads; attempts counts every upload attempt.
	approvalsErr error
	postErr      error
	attempts     int
	// user overrides the signed-in user ("" = sub-1); signedOut signs out.
	user      string
	signedOut bool
	// refused counts uploads the account binding refused (nothing sent).
	refused int
}

func (f *laneFleet) Flavor() fleetapi.Flavor   { return fleetapi.FlavorOfficial }
func (f *laneFleet) Auth() fleetapi.Auth       { return laneAuth{f: f} }
func (f *laneFleet) Me() fleetapi.Me           { return laneMe{} }
func (f *laneFleet) OptIns() fleetapi.OptIns   { return laneOptIns{f} }
func (f *laneFleet) Pollers() fleetapi.Pollers { return lanePollers{f} }
func (f *laneFleet) OTLP() fleetapi.OTLPPoster { return lanePoster{f} }
func (f *laneFleet) postedCount() int          { f.mu.Lock(); defer f.mu.Unlock(); return len(f.posted) }
func (f *laneFleet) set(fn func(*laneFleet))   { f.mu.Lock(); fn(f); f.mu.Unlock() }
func (f *laneFleet) batches() []fleetapi.ProjectedBatch {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]fleetapi.ProjectedBatch(nil), f.posted...)
}

type laneAuth struct {
	fleetapi.Auth
	f *laneFleet
}

func (a laneAuth) Session(context.Context) (fleetapi.Session, error) {
	user := "sub-1"
	if a.f != nil {
		a.f.mu.Lock()
		defer a.f.mu.Unlock()
		if a.f.signedOut {
			return fleetapi.Session{}, fleetapi.ErrNotSignedIn
		}
		if a.f.user != "" {
			user = a.f.user
		}
	}
	return fleetapi.Session{Env: fleetapi.EnvDev, UserID: user, OrgID: "org-1"}, nil
}

type laneMe struct{ fleetapi.Me }

func (laneMe) Get(context.Context) (fleetapi.MeInfo, error) {
	return fleetapi.MeInfo{UserID: "sub-1", OrgID: "org-1", Tier: "team"}, nil
}
func (laneMe) Capabilities(context.Context) (fleetapi.Capabilities, error) {
	return fleetapi.Capabilities{Keys: map[string]bool{}, TelemetrySchema: 2}, nil
}

type laneOptIns struct{ f *laneFleet }

func (o laneOptIns) Get(context.Context) ([]fleetapi.OptInState, error) {
	o.f.mu.Lock()
	defer o.f.mu.Unlock()
	var out []fleetapi.OptInState
	for _, ci := range consent.Classes {
		out = append(out, fleetapi.OptInState{
			Class: ci.Class, Available: true, OptedIn: o.f.optedIn,
			NoticeVersion: consent.NoticeVersion("", project.TableVersion),
		})
	}
	return out, nil
}
func (o laneOptIns) Put(context.Context, fleetapi.OptInPut) error {
	o.f.mu.Lock()
	o.f.optinPuts++
	o.f.mu.Unlock()
	return nil
}

type lanePollers struct{ f *laneFleet }

func (p lanePollers) PendingApprovals(context.Context) ([]fleetapi.Approval, error) {
	p.f.mu.Lock()
	defer p.f.mu.Unlock()
	if p.f.approvalsErr != nil {
		return nil, p.f.approvalsErr
	}
	return p.f.approvals, nil
}
func (lanePollers) LockdownStatus(context.Context) (fleetapi.LockdownState, error) {
	return fleetapi.LockdownState{}, nil
}
func (lanePollers) LockdownWait(ctx context.Context) (fleetapi.LockdownState, bool, error) {
	<-ctx.Done()
	return fleetapi.LockdownState{}, false, ctx.Err()
}
func (lanePollers) OrgPause(context.Context) (fleetapi.PauseState, error) {
	return fleetapi.PauseState{}, nil
}
func (lanePollers) Configs(context.Context, fleetapi.ConfigsRequest) (fleetapi.ConfigBundle, error) {
	return fleetapi.ConfigBundle{}, nil
}
func (lanePollers) AckConfigs(context.Context, string, []fleetapi.ConfigAck) error { return nil }

type lanePoster struct{ f *laneFleet }

// sub1Owner is the default signed-in account as a batch owner.
var sub1Owner = fleetapi.ExpectedSession{Env: fleetapi.EnvDev, OrgID: "org-1", Subject: "sub-1"}

// Post enforces the OTLPPoster account binding like the official adapter:
// it sends with the session active at the call and refuses (sending
// nothing) a batch whose ExpectedSession is missing or another account.
func (p lanePoster) Post(ctx context.Context, b fleetapi.ProjectedBatch) (fleetapi.ExportResult, error) {
	sess, err := laneAuth{f: p.f}.Session(ctx)
	if err != nil {
		return fleetapi.ExportResult{}, err
	}
	p.f.mu.Lock()
	defer p.f.mu.Unlock()
	if want, ok := fleetapi.ExpectedSessionFrom(ctx); !ok || !want.Matches(sess) {
		p.f.refused++
		return fleetapi.ExportResult{}, fleetapi.IdentityMismatch()
	}
	p.f.attempts++
	if p.f.postErr != nil {
		if fleetapi.PlanRequired(p.f.postErr) {
			p.f.approvalsErr = p.f.postErr // Fleet refuses the org everywhere
		}
		return fleetapi.ExportResult{}, p.f.postErr
	}
	p.f.posted = append(p.f.posted, b)
	return fleetapi.ExportResult{Accepted: b.Records}, nil
}

// newTestLane builds the real lane (receiver → pipeline → consent gate →
// forwarder) over a temp spool, with fast loop timers.
func newTestLane(t *testing.T, f *laneFleet) (*telemetryLane, http.Handler, func()) {
	t.Helper()
	return newTestLaneWith(t, f, nil)
}

// testRefreshEvery is the consent refresh interval of lanes built by
// newTestLane*. A test may raise it so only kicks and the per-upload
// session check can observe an account change.
var testRefreshEvery = 20 * time.Millisecond

// testNow is the lane clock in tests; lane goroutines read it concurrently.
var testNow atomic.Value

// newTestLaneWith is newTestLane; before is called with the opened spool
// before the lane starts.
func newTestLaneWith(t *testing.T, f *laneFleet, before func(*spool.Spool)) (*telemetryLane, http.Handler, func()) {
	t.Helper()
	return newTestLaneIn(t, f, t.TempDir(), &spool.Memory{}, before)
}

// newTestLaneIn is newTestLaneWith over an existing state directory and
// key store, so a test can restart the lane (a new daemon process).
func newTestLaneIn(t *testing.T, f *laneFleet, dir string, ks *spool.Memory, before func(*spool.Spool)) (*telemetryLane, http.Handler, func()) {
	t.Helper()
	oldRefresh, oldDrain, oldNow := consentRefreshEvery, drainEvery, laneNow
	consentRefreshEvery, drainEvery = testRefreshEvery, 20*time.Millisecond
	// Admission is observed before the fixture's events by default.
	testNow.Store(fixtures.Base.Add(-time.Hour))
	laneNow = func() time.Time { return testNow.Load().(time.Time) }
	t.Cleanup(func() { consentRefreshEvery, drainEvery, laneNow = oldRefresh, oldDrain, oldNow })

	key, err := spool.GetOrCreateKey(ks, "dev")
	if err != nil {
		t.Fatal(err)
	}
	sp, err := spool.Open(filepath.Join(dir, "spool"), key, spool.Options{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { sp.Close() })
	if before != nil {
		before(sp)
	}
	opt := &daemon.Options{
		Provider: f, Env: fleetapi.EnvDev, StateDir: dir, Version: "test",
		Logger: slog.New(slog.NewTextHandler(io.Discard, nil)), Spool: sp,
	}
	l, err := newTelemetryLane(Env{KeyStore: ks}, opt)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	if err := l.Start(ctx); err != nil {
		t.Fatal(err)
	}
	stop := func() { cancel(); l.Stop() }
	t.Cleanup(stop)
	return l, l.rx, stop
}

// postClaudeSession sends the synthetic Claude Code fixture (every content
// flag on, canaries planted) to the receiver over loopback and ends the
// session the way the SessionEnd hook does (local hash only).
func postClaudeSession(t *testing.T, l *telemetryLane, rx http.Handler) {
	t.Helper()
	// LogsData is wire-compatible with ExportLogsServiceRequest.
	body, err := proto.Marshal(fixtures.ClaudeLogs())
	if err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest(http.MethodPost, "/v1/logs", bytes.NewReader(body))
	req.RemoteAddr = "127.0.0.1:55555"
	req.Header.Set("Content-Type", "application/x-protobuf")
	w := httptest.NewRecorder()
	rx.ServeHTTP(w, req)
	if w.Code/100 != 2 {
		t.Fatalf("receiver status %d: %s", w.Code, w.Body.String())
	}
	l.pipe.SessionEndLocal(canon.ClaudeCode, canon.LocalSessionHash(fixtures.ClaudeSessionID))
}

func waitFor(t *testing.T, cond func() bool) bool {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return true
		}
		time.Sleep(10 * time.Millisecond)
	}
	return false
}

// TestTelemetryLaneEndToEnd: opted-in member → the fake Fleet receives
// allowlisted batches only, with no planted canary in any byte.
func TestTelemetryLaneEndToEnd(t *testing.T) {
	f := &laneFleet{Provider: stub.New(), optedIn: true}
	l, rx, _ := newTestLane(t, f)
	if !waitFor(t, func() bool { return l.dec.SignedIn() }) {
		t.Fatal("consent never observed the opted-in member")
	}
	postClaudeSession(t, l, rx)
	if !waitFor(t, func() bool { return f.postedCount() > 0 }) {
		t.Fatalf("nothing posted; lane status %+v", l.Status())
	}
	al := allowlist.Telemetry()
	for _, b := range f.batches() {
		for _, c := range fixtures.Canaries {
			if bytes.Contains(b.Body, []byte(c)) {
				t.Fatalf("canary %q left the device in a %s batch", c, b.Signal)
			}
		}
		if err := al.Check(b); err != nil {
			t.Fatalf("batch outside the public allowlist: %v", err)
		}
	}
	if st := l.Status(); st.Uploaded == 0 || st.State != "running" {
		t.Fatalf("status %+v", st)
	}
}

// TestTelemetryLaneHeldPostsNothing: a blocking legal acceptance holds the
// lane; nothing reaches Fleet, records wait in the spool, and they drain
// once the hold clears. A member who never opted in sends nothing at all.
func TestTelemetryLaneHeldPostsNothing(t *testing.T) {
	f := &laneFleet{Provider: stub.New(), optedIn: true, approvals: []fleetapi.Approval{{
		ID: "a1", Kind: "legal_acceptance", Title: "Terms", Blocking: true, Required: true,
	}}}
	l, rx, _ := newTestLane(t, f)
	if !waitFor(t, func() bool { return l.dec.HoldReason() != "" }) {
		t.Fatal("blocking approval did not hold the lane")
	}
	postClaudeSession(t, l, rx)
	time.Sleep(150 * time.Millisecond)
	if n := f.postedCount(); n != 0 {
		t.Fatalf("posted %d batches while held", n)
	}
	if l.sp.Empty() || l.Status().State != "held" {
		t.Fatalf("expected spooled records and a held lane, got %+v", l.Status())
	}

	f.set(func(f *laneFleet) { f.approvals = nil })
	if !waitFor(t, func() bool { return f.postedCount() > 0 }) {
		t.Fatalf("held records never drained; status %+v", l.Status())
	}
}

func TestTelemetryLaneNotOptedInSendsNothing(t *testing.T) {
	f := &laneFleet{Provider: stub.New(), optedIn: false}
	l, rx, _ := newTestLane(t, f)
	waitFor(t, func() bool { st := l.cm.State(); return st.SignedIn })
	postClaudeSession(t, l, rx)
	time.Sleep(150 * time.Millisecond)
	if n := f.postedCount(); n != 0 {
		t.Fatalf("posted %d batches without a personal opt-in", n)
	}
	if !l.sp.Empty() {
		t.Fatal("records were kept without consent; they must be dropped")
	}
}

// TestSessionEndControlRoute: the hook's control call is hash-only and
// rejects anything else.
func TestSessionEndControlRoute(t *testing.T) {
	f := &laneFleet{Provider: stub.New(), optedIn: true}
	l, _, _ := newTestLane(t, f)
	for _, tc := range []struct {
		body string
		code int
	}{
		{`{"client_kind":"claude-code","session_hash":"` + canon.LocalSessionHash("x") + `"}`, 204},
		{`{"client_kind":"claude-code","session_id":"raw"}`, 400},
		{`{"client_kind":"cursor","session_hash":"` + canon.LocalSessionHash("x") + `"}`, 400},
	} {
		w := httptest.NewRecorder()
		l.handleSessionEnd(w, httptest.NewRequest(http.MethodPost, daemon.SessionEndPath, strings.NewReader(tc.body)))
		if w.Code != tc.code {
			t.Fatalf("%s → %d, want %d", tc.body, w.Code, tc.code)
		}
	}
}

// TestTelemetryLaneLockdownStopsQueuedBatches: a batch that passed the gate
// before a lockdown arrived (queued in memory or already in the spool) is
// not uploaded while the lockdown is in effect.
func TestTelemetryLaneLockdownStopsQueuedBatches(t *testing.T) {
	f := &laneFleet{Provider: stub.New(), optedIn: true}
	l, _, _ := newTestLane(t, f)
	if !waitFor(t, func() bool { return l.dec.SignedIn() }) {
		t.Fatal("consent never observed the opted-in member")
	}
	l.cm.SetLockdown(fleetapi.LockdownState{Locked: true, Reason: "incident"})
	l.dec.invalidate()
	b := fleetapi.ProjectedBatch{Signal: fleetapi.SignalLogs, ContentType: fleetapi.ContentTypeProtobuf, Body: []byte{}, Records: 1}
	if l.send(context.Background(), queued{b, time.Now(), sub1Owner}) {
		t.Fatal("send reported success during lockdown")
	}
	l.drain(context.Background())
	time.Sleep(100 * time.Millisecond)
	if n := f.postedCount(); n != 0 {
		t.Fatalf("posted %d batches during lockdown", n)
	}
	if l.sp.Empty() {
		t.Fatal("held batch was not kept in the spool")
	}
	l.cm.SetLockdown(fleetapi.LockdownState{})
	l.dec.invalidate()
	l.kick()
	if !waitFor(t, func() bool { return f.postedCount() > 0 }) {
		t.Fatal("batch never uploaded after lockdown cleared")
	}
}

// TestOptinRefusesPipedAnswers: the opt-in flow never reads answers from a
// pipe, so an agent cannot opt the member in. Opting out stays
// available non-interactively.
func TestOptinRefusesPipedAnswers(t *testing.T) {
	old := stdinInteractive
	stdinInteractive = func() bool { return false }
	t.Cleanup(func() { stdinInteractive = old })
	f := &laneFleet{Provider: stub.New()}
	var out, errb bytes.Buffer
	env := Env{Provider: f, Stdout: &out, Stderr: &errb, Ctx: context.Background(), KeyStore: &spool.Memory{},
		Getenv: func(k string) string {
			if k == variant.Shared.EnvVar {
				return "dev"
			}
			return ""
		}}
	if code := runOptin(env, nil); code != ExitUsage || !strings.Contains(errb.String(), "your own terminal") {
		t.Fatalf("optin without a terminal: exit=%d stderr=%q", code, errb.String())
	}
	if f.optinPuts != 0 {
		t.Fatal("opt-in written without a terminal")
	}
	if code := runOptin(env, []string{"--all-off"}); code != ExitOK || f.optinPuts != 1 {
		t.Fatalf("opt-out without a terminal: exit=%d puts=%d stderr=%q", code, f.optinPuts, errb.String())
	}
}

func planRefusal() error {
	return &fleetapi.APIError{Status: 403, Code: fleetapi.CodeCapabilityNotInTier, Message: "fleet text", Details: map[string]string{"capability": fleetapi.PluginCapability}}
}

func (f *laneFleet) attemptCount() int { f.mu.Lock(); defer f.mu.Unlock(); return f.attempts }

func admitted(l *telemetryLane) bool { _, ok := l.dec.AdmittedSince(); return ok }

// A batch Fleet refuses because the org's plan does not include the plugin is dropped
// at once: nothing is kept on the device, and no further uploads are
// attempted (nor anything collected) while the org is not admitted.
func TestTelemetryLaneRefusedBatchNotPersisted(t *testing.T) {
	f := &laneFleet{Provider: stub.New(), optedIn: true, postErr: planRefusal()}
	l, rx, _ := newTestLane(t, f)
	if !waitFor(t, func() bool { return l.dec.SignedIn() && admitted(l) }) {
		t.Fatal("lane never admitted")
	}
	postClaudeSession(t, l, rx)
	if !waitFor(t, func() bool { return f.attemptCount() > 0 }) {
		t.Fatal("no upload attempted")
	}
	if !waitFor(t, func() bool { return l.cm.State().PlanRequired }) {
		t.Fatal("refusal not recorded")
	}
	time.Sleep(100 * time.Millisecond)
	if !l.sp.Empty() {
		t.Fatal("a refused batch was kept on the device")
	}
	n := f.attemptCount()
	postClaudeSession(t, l, rx)
	time.Sleep(150 * time.Millisecond)
	if f.attemptCount() != n || !l.sp.Empty() {
		t.Fatalf("while not admitted: %d more attempts, spool empty=%v", f.attemptCount()-n, l.sp.Empty())
	}
	if c := l.pipe.Counters(); c["dropped_not_admitted"] == 0 {
		t.Fatalf("records not dropped as not admitted: %v", c)
	}
	if st := l.Status(); st.State == "held" {
		t.Fatalf("not admitted is not a hold: %+v", st)
	}
}

// Data held by an earlier build (old record tags) is purged at admission
// and never sent.
func TestTelemetryLaneEarlierBuildDataPurgedAtAdmission(t *testing.T) {
	f := &laneFleet{Provider: stub.New(), optedIn: true}
	l, _, _ := newTestLaneWith(t, f, func(sp *spool.Spool) {
		b, _ := json.Marshal(fleetapi.ProjectedBatch{Signal: fleetapi.SignalLogs, ContentType: fleetapi.ContentTypeProtobuf, Body: []byte{}, Records: 1})
		if err := sp.Append(telemetryLaneName, append([]byte{tagLegacyBatch}, b...)); err != nil {
			t.Fatal(err)
		}
		if err := sp.Append(telemetryLaneName, append([]byte{tagLegacyHeld}, []byte("[]")...)); err != nil {
			t.Fatal(err)
		}
	})
	if !waitFor(t, func() bool { return admitted(l) && l.sp.Empty() }) {
		t.Fatalf("earlier data not purged at admission: admitted=%v empty=%v", admitted(l), l.sp.Empty())
	}
	time.Sleep(100 * time.Millisecond)
	if n := f.attemptCount(); n != 0 {
		t.Fatalf("%d uploads of earlier data", n)
	}
}

// Events generated before the admission moment are never sent, even when
// their session ends after admission.
func TestTelemetryLanePreAdmissionEventsNeverSent(t *testing.T) {
	f := &laneFleet{Provider: stub.New(), optedIn: true, approvalsErr: planRefusal()}
	l, rx, _ := newTestLane(t, f)
	if !waitFor(t, func() bool { return l.cm.State().PlanRequired }) {
		t.Fatal("refusal not observed")
	}
	// The fixture's events (fixtures.Base) are generated while refused.
	testNow.Store(fixtures.Base.Add(time.Hour))
	body, err := proto.Marshal(fixtures.ClaudeLogs())
	if err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest(http.MethodPost, "/v1/logs", bytes.NewReader(body))
	req.RemoteAddr = "127.0.0.1:55555"
	req.Header.Set("Content-Type", "application/x-protobuf")
	rx.ServeHTTP(httptest.NewRecorder(), req)

	f.set(func(f *laneFleet) { f.approvalsErr = nil })
	if !waitFor(t, func() bool { return admitted(l) }) {
		t.Fatal("admission not observed")
	}
	l.dec.invalidate()
	l.pipe.SessionEndLocal(canon.ClaudeCode, canon.LocalSessionHash(fixtures.ClaudeSessionID))
	time.Sleep(150 * time.Millisecond)
	if n := f.attemptCount(); n != 0 {
		t.Fatalf("%d uploads of pre-admission events", n)
	}
	if c := l.pipe.Counters(); c["dropped_not_admitted"] == 0 {
		t.Fatalf("pre-admission events not dropped before collection: %v", c)
	}
	if !l.sp.Empty() {
		t.Fatal("pre-admission records kept")
	}
}

// In an admitted org a transient failure (5xx, network, 429) still spools
// the batch, and it is sent once Fleet recovers.
func TestTelemetryLaneTransientFailureSpools(t *testing.T) {
	f := &laneFleet{Provider: stub.New(), optedIn: true, postErr: &fleetapi.APIError{Status: 503}}
	l, rx, _ := newTestLane(t, f)
	if !waitFor(t, func() bool { return l.dec.SignedIn() && admitted(l) }) {
		t.Fatal("lane never admitted")
	}
	postClaudeSession(t, l, rx)
	if !waitFor(t, func() bool { return f.attemptCount() > 0 && !l.sp.Empty() }) {
		t.Fatalf("transient failure not spooled: attempts=%d", f.attemptCount())
	}
	f.set(func(f *laneFleet) { f.postErr = nil })
	if !waitFor(t, func() bool { return f.postedCount() > 0 && l.sp.Empty() }) {
		t.Fatalf("spooled batch never sent; status %+v", l.Status())
	}
}

// shiftedClaudeLogs is fixtures.ClaudeLogs with every timestamp moved by d.
func shiftedClaudeLogs(d time.Duration) *logsv1.LogsData {
	m := fixtures.ClaudeLogs()
	for _, rl := range m.ResourceLogs {
		for _, sl := range rl.ScopeLogs {
			for _, lr := range sl.LogRecords {
				lr.TimeUnixNano += uint64(d)
			}
		}
	}
	return m
}

func postLogs(t *testing.T, rx http.Handler, m *logsv1.LogsData) {
	t.Helper()
	body, err := proto.Marshal(m)
	if err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest(http.MethodPost, "/v1/logs", bytes.NewReader(body))
	req.RemoteAddr = "127.0.0.1:55555"
	req.Header.Set("Content-Type", "application/x-protobuf")
	w := httptest.NewRecorder()
	rx.ServeHTTP(w, req)
	if w.Code/100 != 2 {
		t.Fatalf("receiver status %d", w.Code)
	}
}

// postedTokenIn sums token_in over every conversation_ended record Fleet
// received.
func (f *laneFleet) postedEndedTokenIn(t *testing.T) (n int, tokens int64) {
	t.Helper()
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, b := range f.posted {
		if b.Signal != fleetapi.SignalLogs {
			continue
		}
		var m logsv1.LogsData
		if err := proto.Unmarshal(b.Body, &m); err != nil {
			t.Fatal(err)
		}
		for _, rl := range m.ResourceLogs {
			for _, sl := range rl.ScopeLogs {
				for _, lr := range sl.LogRecords {
					if lr.Attributes[0].Value.GetStringValue() != fleetschema.KindConversationEnded {
						continue
					}
					n++
					for _, kv := range lr.Body.GetKvlistValue().GetValues() {
						if kv.Key == "token_in" {
							tokens += kv.Value.GetIntValue()
						}
					}
				}
			}
		}
	}
	return n, tokens
}

// A session that starts while the org is refused and ends after admission
// must not carry the pre-admission part of the session to Fleet: its
// aggregates (tokens, cost, duration) cover only events after admission.
func TestTelemetryLaneSessionSpanningAdmission(t *testing.T) {
	f := &laneFleet{Provider: stub.New(), optedIn: true, approvalsErr: planRefusal()}
	l, rx, _ := newTestLane(t, f)
	if !waitFor(t, func() bool { return l.cm.State().PlanRequired }) {
		t.Fatal("refusal not observed")
	}
	postLogs(t, rx, shiftedClaudeLogs(0)) // generated while refused

	testNow.Store(fixtures.Base.Add(time.Hour))
	f.set(func(f *laneFleet) { f.approvalsErr = nil })
	if !waitFor(t, func() bool { return admitted(l) }) {
		t.Fatal("admission not observed")
	}
	l.dec.invalidate()
	postLogs(t, rx, shiftedClaudeLogs(2*time.Hour)) // same session, after admission
	l.pipe.SessionEndLocal(canon.ClaudeCode, canon.LocalSessionHash(fixtures.ClaudeSessionID))
	if !waitFor(t, func() bool { return f.postedCount() > 0 }) {
		t.Fatal("post-admission events not sent")
	}
	time.Sleep(100 * time.Millisecond)
	// One copy of the fixture is 1310 input tokens (cache included).
	if n, tok := f.postedEndedTokenIn(t); n != 1 || tok != 1310 {
		t.Fatalf("conversation_ended carried pre-admission usage: %d records, token_in %d", n, tok)
	}
}

// A refusal applies to the next spool write at once, even while the
// decider's snapshot (refreshed once a second) still says admitted.
func TestTelemetryLaneNoSpoolWriteAfterRefusal(t *testing.T) {
	f := &laneFleet{Provider: stub.New(), optedIn: true}
	l, _, _ := newTestLane(t, f)
	if !waitFor(t, func() bool { return l.dec.SignedIn() && admitted(l) }) {
		t.Fatal("lane never admitted")
	}
	_ = l.dec.HoldReason() // warm the snapshot cache: admitted
	l.cm.ObserveUploadError(planRefusal())
	b := fleetapi.ProjectedBatch{Signal: fleetapi.SignalLogs, ContentType: fleetapi.ContentTypeProtobuf, Body: []byte{}, Records: 1}
	l.spoolBatch(queued{b, fixtures.Base, sub1Owner})
	l.holdRecords([]project.Record{{Class: "x", Time: fixtures.Base}}, "paused")
	if !l.sp.Empty() {
		t.Fatal("data written to the spool after a refusal")
	}
}

// A revoked org goes back to dropping: after a refusal nothing is
// collected, spooled or uploaded.
func TestTelemetryLaneRevokedOrgDrops(t *testing.T) {
	f := &laneFleet{Provider: stub.New(), optedIn: true}
	l, rx, _ := newTestLane(t, f)
	if !waitFor(t, func() bool { return l.dec.SignedIn() && admitted(l) }) {
		t.Fatal("lane never admitted")
	}
	f.set(func(f *laneFleet) { f.approvalsErr = planRefusal() })
	if !waitFor(t, func() bool { return l.cm.State().PlanRequired && !admitted(l) }) {
		t.Fatal("revocation not observed")
	}
	postClaudeSession(t, l, rx)
	time.Sleep(100 * time.Millisecond)
	if n := f.attemptCount(); n != 0 || !l.sp.Empty() {
		t.Fatalf("after revocation: %d uploads, spool empty=%v", n, l.sp.Empty())
	}
}

// The account switches while a batch waits for a rate token: the owner is
// re-checked after the wait and the batch is dropped unsent; and if the
// switch slips past that check, the poster refuses it (identity_mismatch).
// Either way Sub-1's batch never goes out on Sub-2's session.
func TestTelemetryLaneAccountSwitchDuringRateWait(t *testing.T) {
	old := testRefreshEvery
	testRefreshEvery = time.Hour // only the upload-time checks see the switch
	t.Cleanup(func() { testRefreshEvery = old })
	for _, recheck := range []bool{true, false} {
		t.Run(fmt.Sprint("recheck=", recheck), func(t *testing.T) { laneSwitchDuringWait(t, recheck) })
	}
}

func laneSwitchDuringWait(t *testing.T, recheck bool) {
	{
		f := &laneFleet{Provider: stub.New(), optedIn: true}
		l, _, _ := newTestLane(t, f)
		if !waitFor(t, func() bool { return l.dec.SignedIn() && admitted(l) }) {
			t.Fatal("lane never admitted")
		}
		var mu sync.Mutex
		now := time.Now()
		fwd := &forward.Forwarder{Poster: l.fwd.Poster, PerMinute: 1,
			Now: func() time.Time { mu.Lock(); defer mu.Unlock(); return now },
			Sleep: func(_ context.Context, d time.Duration) error {
				f.set(func(f *laneFleet) { f.user = "sub-2" }) // switch mid-wait
				mu.Lock()
				now = now.Add(d)
				mu.Unlock()
				return nil
			}}
		if recheck {
			fwd.Current = l.ownerIs
		} else {
			// Model a switch that lands after the re-check: the poster's
			// binding is the last line.
			fwd.Current = func(context.Context, fleetapi.ExpectedSession) bool { return true }
		}
		l.fwd = fwd
		b := fleetapi.ProjectedBatch{Signal: fleetapi.SignalLogs, ContentType: fleetapi.ContentTypeProtobuf, Body: []byte{}, Records: 1}
		if !l.send(context.Background(), queued{b, time.Now(), sub1Owner}) {
			t.Fatalf("recheck=%v: first send failed", recheck)
		}
		_ = l.send(context.Background(), queued{b, time.Now(), sub1Owner})
		f.mu.Lock()
		posted, refused := len(f.posted), f.refused
		f.mu.Unlock()
		if posted != 1 {
			t.Fatalf("recheck=%v: posted %d batches, want only the one before the switch", recheck, posted)
		}
		if recheck && refused != 0 {
			t.Fatalf("the owner was not re-checked after the wait: the poster saw the batch (refused=%d)", refused)
		}
		if !recheck && refused != 1 {
			t.Fatalf("poster did not refuse the switched batch: refused=%d", refused)
		}
	}
}

// An untagged 'b' batch written by an earlier build has no owner: it may
// belong to another account (earlier builds recorded no queue owner and a
// login over an existing session did not purge the spool), so it is
// dropped, never sent under whoever is signed in now. An owner-stamped
// batch for the signed-in account still goes.
func TestTelemetryLaneUntaggedBatchNeverSent(t *testing.T) {
	f := &laneFleet{Provider: stub.New(), optedIn: true}
	rec := func(tag byte, v any) []byte {
		b, _ := json.Marshal(v)
		out := make([]byte, 9, 9+len(b))
		out[0] = tag
		binary.BigEndian.PutUint64(out[1:], uint64(fixtures.Base.UnixNano()))
		return append(out, b...)
	}
	batch := func(n int) fleetapi.ProjectedBatch {
		return fleetapi.ProjectedBatch{Signal: fleetapi.SignalLogs, ContentType: fleetapi.ContentTypeProtobuf, Body: []byte{byte(n)}, Records: 1}
	}
	l, _, _ := newTestLane(t, f)
	if !waitFor(t, func() bool { return admitted(l) }) {
		t.Fatal("lane never admitted")
	}
	// Queued after admission (a restart keeps the stored admission): another
	// account's batches from an earlier build, then one stamped for sub-1.
	for i := 0; i < 3; i++ {
		if err := l.sp.Append(telemetryLaneName, rec(tagBatch, batch(i))); err != nil {
			t.Fatal(err)
		}
	}
	if err := l.sp.Append(telemetryLaneName, rec(tagOwnedBatch, ownedBatch{Owner: sub1Owner, Batch: batch(9)})); err != nil {
		t.Fatal(err)
	}
	if !waitFor(t, func() bool { return l.sp.Empty() }) {
		t.Fatal("spool not drained")
	}
	time.Sleep(100 * time.Millisecond)
	got := f.batches()
	if len(got) != 1 || len(got[0].Body) != 1 || got[0].Body[0] != 9 {
		t.Fatalf("posted %d batches (want only the stamped one): %+v", len(got), got)
	}
}
