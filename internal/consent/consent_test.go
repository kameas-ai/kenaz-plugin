package consent

import (
	"bytes"
	"context"
	"errors"
	"html"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/kameas-ai/kenaz-plugin/fleetapi"
	"github.com/kameas-ai/kenaz-plugin/notices"
)

// Nothing leaves for a class the member has not opted in to.
func TestOptInGate(t *testing.T) {
	f := newFake(allClasses()...)
	m := newTestManager(t, f, nil)
	for _, ci := range Classes {
		if d := m.Decide(ci.Class); d.Action != ActionDrop || d.Reason != ReasonOptOut {
			t.Fatalf("%s: got %+v, want drop/optout", ci.Class, d)
		}
	}
	if m.State().Drops.OptOut == 0 {
		t.Fatal("drops.optout not counted")
	}
	optIn(t, m, fleetapi.ClassUsageCounts)
	if d := m.Decide(fleetapi.ClassUsageCounts); d.Action != ActionSend {
		t.Fatalf("usage_counts after opt-in: %+v", d)
	}
	for _, c := range []fleetapi.TelemetryClass{fleetapi.ClassToolCalls, fleetapi.ClassErrors, fleetapi.ClassModelCallTelemetry} {
		if d := m.Decide(c); d.Action != ActionDrop {
			t.Fatalf("%s should still drop: %+v", c, d)
		}
	}
	if d := m.Decide("harness.something_new"); d.Action != ActionDrop {
		t.Fatalf("unknown class must drop: %+v", d)
	}
}

// A plan without classes renders them unavailable and never PUTs.
func TestTierRender(t *testing.T) {
	f := newFake() // nothing available
	m := newTestManager(t, f, nil)
	st := m.State()
	for _, ci := range Classes {
		if st.Classes[string(ci.Class)] != ClassUnavailable {
			t.Fatalf("%s = %s, want unavailable", ci.Class, st.Classes[string(ci.Class)])
		}
	}
	var out bytes.Buffer
	res, err := RunFirstRun(context.Background(), m, testVars(), strings.NewReader("1\ny\n"), &out)
	if err != nil {
		t.Fatal(err)
	}
	if !res.NoneAvl || len(f.puts) != 0 {
		t.Fatalf("expected no PUT on a plan with no classes: %+v puts=%d", res, len(f.puts))
	}
	if !strings.Contains(out.String(), "not available on your plan") {
		t.Fatal("unavailable classes must be shown greyed")
	}
	if err := m.SetOptIns(context.Background(), map[fleetapi.TelemetryClass]bool{fleetapi.ClassUsageCounts: true}); err == nil {
		t.Fatal("opting in to an unavailable class must fail")
	}
}

// Blocking legal acceptance and unknown blocking kinds hold; a
// blocking ml_notice does not.
func TestApprovalHold(t *testing.T) {
	f := newFake(allClasses()...)
	m := newTestManager(t, f, nil)
	optIn(t, m, fleetapi.ClassUsageCounts)
	ctx := context.Background()

	cases := []struct {
		name string
		a    fleetapi.Approval
		want Action
	}{
		{"legal blocking", fleetapi.Approval{ID: "1", Kind: fleetapi.ApprovalKindLegalAcceptance, Title: "Terms", Blocking: true}, ActionHold},
		{"unknown blocking", fleetapi.Approval{ID: "2", Kind: "future_kind", Title: "New", Blocking: true}, ActionHold},
		{"ml_notice blocking", fleetapi.Approval{ID: "3", Kind: fleetapi.ApprovalKindMLNotice, Title: "ML", Blocking: true}, ActionSend},
		{"legal non-blocking", fleetapi.Approval{ID: "4", Kind: fleetapi.ApprovalKindLegalAcceptance, Title: "Terms"}, ActionSend},
		{"required past deferral", fleetapi.Approval{ID: "5", Kind: fleetapi.ApprovalKindLegalAcceptance, Title: "Terms", Required: true, DeferrableUntil: ptr(time.Now().Add(-time.Hour))}, ActionHold},
		{"required before deferral", fleetapi.Approval{ID: "6", Kind: fleetapi.ApprovalKindLegalAcceptance, Title: "Terms", Required: true, DeferrableUntil: ptr(time.Now().Add(time.Hour))}, ActionSend},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f.set(func(f *fakeFleet) { f.approvals = []fleetapi.Approval{tc.a} })
			if err := m.Refresh(ctx); err != nil {
				t.Fatal(err)
			}
			if d := m.Decide(fleetapi.ClassUsageCounts); d.Action != tc.want {
				t.Fatalf("got %v (%s), want %v", d.Action, d.Reason, tc.want)
			}
			if len(m.State().Pending) != 1 {
				t.Fatal("item must be listed as pending either way")
			}
		})
	}
	f.set(func(f *fakeFleet) { f.approvals = nil })
	if err := m.Refresh(ctx); err != nil {
		t.Fatal(err)
	}
	if d := m.Decide(fleetapi.ClassUsageCounts); d.Action != ActionSend {
		t.Fatalf("hold must clear when the item clears: %+v", d)
	}
}

// Nothing before the member personally signs in and opts in.
// Admin pre-selection is display-only.
func TestNoTelemetryBeforeMemberOptIn(t *testing.T) {
	f := newFake(allClasses()...)
	f.signedIn = false
	for c, s := range f.optins {
		s.AdminSuggested = true // admin pre-selected everything
		f.optins[c] = s
	}
	m := newTestManager(t, f, nil)
	for _, ci := range Classes {
		if d := m.Decide(ci.Class); d.Action != ActionDrop || d.Reason != ReasonNotSignedIn {
			t.Fatalf("signed out: %+v", d)
		}
	}
	var st bytes.Buffer
	WriteText(&st, m.State())
	if !strings.Contains(st.String(), "not signed in") {
		t.Fatalf("status must say not signed in: %q", st.String())
	}

	f.set(func(f *fakeFleet) { f.signedIn = true })
	if err := m.Refresh(context.Background()); err != nil {
		t.Fatal(err)
	}
	for _, ci := range Classes {
		if d := m.Decide(ci.Class); d.Action != ActionDrop {
			t.Fatalf("signed in, nothing chosen: %+v", d)
		}
	}
	var out bytes.Buffer
	res, err := RunFirstRun(context.Background(), m, testVars(), strings.NewReader("\n"), &out)
	if err != nil {
		t.Fatal(err)
	}
	if !res.NotNow || len(f.puts) != 0 {
		t.Fatalf("Not now must write nothing: %+v puts=%d", res, len(f.puts))
	}
	if !strings.Contains(out.String(), "[ ] 1. Usage counts (suggested by your administrator)") {
		t.Fatalf("suggested class must render unchecked with the tag:\n%s", out.String())
	}
	if m.State().AnyOn() {
		t.Fatal("admin suggestion must never enable a class")
	}
}

// Terminal and browser notice are the same bytes; notice version is
// sent with the opt-in; a projection change forces re-confirmation.
func TestNoticeRender(t *testing.T) {
	v := testVars()
	text, err := RenderNotice(v)
	if err != nil {
		t.Fatal(err)
	}
	page, err := RenderNoticeHTML(v)
	if err != nil {
		t.Fatal(err)
	}
	pre := strings.Index(page, "<pre")
	start := pre + strings.Index(page[pre:], ">") + 1
	end := strings.LastIndex(page, "</pre>")
	if got := html.UnescapeString(page[start:end]); got != text {
		t.Fatal("browser notice differs from terminal notice")
	}
	want := strings.NewReplacer(
		"{Organization name}", "Acme",
		"{harnesses}", "Claude Code and Codex",
		"{device display name}", "macOS device 3f2a",
		"{harness versions}", "Claude Code 2.1.219 and Codex 0.147.0",
	).Replace(notices.AgentTelemetryV1)
	if text != want {
		t.Fatal("rendered notice is not the shipped asset with variables filled")
	}
	if _, err := RenderNotice(NoticeVars{OrgName: "Acme"}); !errors.Is(err, ErrUnfilledNotice) {
		t.Fatalf("unfilled notice must be refused: %v", err)
	}

	// The shipped asset must match the approved notice sentences we rely on.
	for _, s := range []string{"Nothing is shared until you choose below", "Never sent:", "Never the error message."} {
		if !strings.Contains(notices.AgentTelemetryV1, s) {
			t.Fatalf("asset missing %q", s)
		}
	}

	f := newFake(allClasses()...)
	store := NewStore(filepath.Join(t.TempDir(), "c.json"))
	m := NewManager(Options{Provider: f, ProjectionVersion: 1, Store: store})
	if err := m.Refresh(context.Background()); err != nil {
		t.Fatal(err)
	}
	optIn(t, m, fleetapi.ClassUsageCounts)
	if got := f.puts[len(f.puts)-1].NoticeVersion; got != NoticeVersion("", 1) {
		t.Fatalf("PUT notice_version = %q", got)
	}
	if d := m.Decide(fleetapi.ClassUsageCounts); d.Action != ActionSend {
		t.Fatalf("accepted under current notice: %+v", d)
	}

	// New build with a changed projection table: same store, version 2.
	m2 := NewManager(Options{Provider: f, ProjectionVersion: 2, Store: store})
	if err := m2.Refresh(context.Background()); err != nil {
		t.Fatal(err)
	}
	if d := m2.Decide(fleetapi.ClassUsageCounts); d.Action != ActionDrop || d.Reason != ReasonNoticeReconfirm {
		t.Fatalf("projection change must block until re-confirmed: %+v", d)
	}
	if !m2.State().NoticeReconfirm {
		t.Fatal("state must flag re-confirmation")
	}
	optIn(t, m2, fleetapi.ClassUsageCounts)
	if d := m2.Decide(fleetapi.ClassUsageCounts); d.Action != ActionSend {
		t.Fatalf("after re-confirm: %+v", d)
	}

	// Fleet's own record wins over the local store.
	f.set(func(f *fakeFleet) {
		s := f.optins[fleetapi.ClassUsageCounts]
		s.NoticeVersion = "agent-telemetry/v0+p1"
		f.optins[fleetapi.ClassUsageCounts] = s
	})
	if err := m2.Refresh(context.Background()); err != nil {
		t.Fatal(err)
	}
	if d := m2.Decide(fleetapi.ClassUsageCounts); d.Reason != ReasonNoticeReconfirm {
		t.Fatalf("Fleet-recorded old version must force re-confirm: %+v", d)
	}
}

// No code path can accept an approval. The seam has no accept
// method and Approval carries no replayable request.
func TestNoAutoAccept(t *testing.T) {
	at := reflect.TypeOf(fleetapi.Approval{})
	for i := 0; i < at.NumField(); i++ {
		n := strings.ToLower(at.Field(i).Name)
		if strings.Contains(n, "approve") || strings.Contains(n, "accept") || strings.Contains(n, "method") || strings.Contains(n, "path") {
			t.Fatalf("fleetapi.Approval must not carry a replayable request: field %s", at.Field(i).Name)
		}
	}
	for _, it := range []reflect.Type{reflect.TypeOf((*fleetapi.Pollers)(nil)).Elem(), reflect.TypeOf((*fleetapi.Me)(nil)).Elem()} {
		for i := 0; i < it.NumMethod(); i++ {
			n := strings.ToLower(it.Method(i).Name)
			if strings.Contains(n, "approve") || strings.Contains(n, "accept") || strings.Contains(n, "ack") && !strings.Contains(n, "ackconfigs") {
				t.Fatalf("%s.%s looks like an approval write", it.Name(), it.Method(i).Name)
			}
		}
	}
	root := filepath.Join("..", "..")
	err := filepath.Walk(root, func(p string, info os.FileInfo, err error) error {
		if err != nil || info.IsDir() || !strings.HasSuffix(p, ".go") || strings.HasSuffix(p, "_test.go") {
			return err
		}
		b, err := os.ReadFile(p)
		if err != nil {
			return err
		}
		for _, bad := range []string{"notice-ack", "pending-approvals/", "/accept"} {
			if strings.Contains(string(b), bad) {
				t.Errorf("%s references an approval write endpoint (%q)", p, bad)
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

// org_paused latches a hold; an unpaused /me clears it.
func TestOrgPause(t *testing.T) {
	f := newFake(allClasses()...)
	m := newTestManager(t, f, nil)
	optIn(t, m, fleetapi.ClassUsageCounts)
	m.ObserveUploadError(&fleetapi.APIError{Status: 403, Code: fleetapi.CodeOrgPaused, Details: map[string]string{"paused_category": "billing"}})
	if d := m.Decide(fleetapi.ClassUsageCounts); d.Action != ActionHold || d.Reason != HoldPaused {
		t.Fatalf("paused: %+v", d)
	}
	if err := m.Refresh(context.Background()); err != nil {
		t.Fatal(err)
	}
	if d := m.Decide(fleetapi.ClassUsageCounts); d.Action != ActionSend {
		t.Fatalf("unpaused /me must clear the latch: %+v", d)
	}
	f.set(func(f *fakeFleet) { f.me.OrgPaused = true; f.me.PausedCategory = "staff" })
	if err := m.Refresh(context.Background()); err != nil {
		t.Fatal(err)
	}
	if d := m.Decide(fleetapi.ClassUsageCounts); d.Action != ActionHold {
		t.Fatalf("/me paused must hold: %+v", d)
	}
}

// Lockdown holds uploads and refuses Fleet writes, but status and
// reads keep working and nothing else is touched.
func TestLockdownNoBlock(t *testing.T) {
	f := newFake(allClasses()...)
	f.caps.Keys[CapEmergencyLockdown] = true
	m := newTestManager(t, f, nil)
	optIn(t, m, fleetapi.ClassUsageCounts)

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { _ = m.RunLockdownWatch(ctx); close(done) }()
	f.lockWaits <- fleetapi.LockdownState{Locked: true, Reason: "incident\x07 42"}
	waitFor(t, func() bool { return m.WriteAllowed() != nil })
	if d := m.Decide(fleetapi.ClassUsageCounts); d.Action != ActionHold || d.Reason != HoldLockdown {
		t.Fatalf("lockdown: %+v", d)
	}
	if !errors.Is(m.WriteAllowed(), ErrLockedDown) {
		t.Fatal("write tools must refuse during lockdown")
	}
	st := m.State()
	if !st.SignedIn || len(st.Holds) != 1 || strings.ContainsRune(st.Holds[0].Detail, '\x07') {
		t.Fatalf("status must still render with a sanitized reason: %+v", st)
	}
	f.lockWaits <- fleetapi.LockdownState{Locked: false}
	waitFor(t, func() bool { return m.WriteAllowed() == nil })
	cancel()
	<-done

	// Without the capability the watch is skipped.
	f2 := newFake(allClasses()...)
	m2 := newTestManager(t, f2, nil)
	if err := m2.RunLockdownWatch(context.Background()); err != nil {
		t.Fatal(err)
	}
}

// Withdrawal stops lanes and purges immediately.
func TestWithdrawal(t *testing.T) {
	ctx := context.Background()
	f := newFake(allClasses()...)
	lanes := &fakeLanes{}
	m := newTestManager(t, f, lanes)
	optIn(t, m, fleetapi.ClassUsageCounts, fleetapi.ClassToolCalls)
	lanes.purged = 0 // the first admitted response purged once; count from here

	// Per-class opt-out purges only that class.
	if err := m.SetOptIns(ctx, map[fleetapi.TelemetryClass]bool{fleetapi.ClassToolCalls: false}); err != nil {
		t.Fatal(err)
	}
	if len(lanes.classes) != 1 || lanes.classes[0] != fleetapi.ClassToolCalls || lanes.purged != 0 {
		t.Fatalf("per-class purge: %+v", lanes)
	}

	// All-off is withdrawal.
	start := time.Now()
	if err := m.SetOptIns(ctx, map[fleetapi.TelemetryClass]bool{fleetapi.ClassUsageCounts: false}); err != nil {
		t.Fatal(err)
	}
	if lanes.stopped != 1 || lanes.purged != 1 || time.Since(start) > time.Second {
		t.Fatalf("all-off withdrawal: %+v", lanes)
	}

	for name, trigger := range map[string]func(){
		"logout":       func() { m.Withdraw("logout") },
		"node removed": func() { m.ObserveUploadError(&fleetapi.APIError{Status: 410, Code: fleetapi.CodeNodeRemoved}) },
		"signed out": func() {
			f.set(func(f *fakeFleet) { f.signedIn = false })
			_ = m.Refresh(ctx)
			f.set(func(f *fakeFleet) { f.signedIn = true })
		},
		"membership lost": func() {
			f.set(func(f *fakeFleet) { f.meErr = &fleetapi.APIError{Status: 401} })
			_ = m.Refresh(ctx)
			f.set(func(f *fakeFleet) { f.meErr = nil })
		},
	} {
		before := lanes.purged
		trigger()
		if lanes.purged != before+1 {
			t.Fatalf("%s: no purge", name)
		}
		if d := m.Decide(fleetapi.ClassUsageCounts); d.Action != ActionDrop {
			t.Fatalf("%s: must drop after withdrawal: %+v", name, d)
		}
	}
}

// The SessionStart banner has titles but never body text, and stays
// within budget.
func TestBanner(t *testing.T) {
	f := newFake(allClasses()...)
	f.approvals = []fleetapi.Approval{{ID: "1", Kind: fleetapi.ApprovalKindMLNotice, Title: "Workflow data notice", BodyText: "SECRET BODY TEXT do not leak", Blocking: true}}
	m := newTestManager(t, f, nil)
	optIn(t, m, fleetapi.ClassUsageCounts)
	b := SessionStartContext(m.State())
	if !strings.Contains(b, "Workflow data notice") || strings.Contains(b, "SECRET BODY") {
		t.Fatalf("banner: %q", b)
	}
	if !strings.Contains(b, "Do not summarize or accept") {
		t.Fatal("banner must tell the model not to accept")
	}

	long := strings.Repeat("x", 500)
	var many []fleetapi.Approval
	for i := 0; i < 20; i++ {
		many = append(many, fleetapi.Approval{ID: "a", Kind: "future_kind", Title: long, Blocking: true})
	}
	f.set(func(f *fakeFleet) { f.approvals = many })
	if err := m.Refresh(context.Background()); err != nil {
		t.Fatal(err)
	}
	if b := SessionStartContext(m.State()); len([]rune(b)) > BannerMax {
		t.Fatalf("banner too long: %d", len([]rune(b)))
	}

	// Nothing actionable → silent. Signed out → silent.
	f.set(func(f *fakeFleet) { f.approvals = nil })
	_ = m.Refresh(context.Background())
	if b := SessionStartContext(m.State()); b != "" {
		t.Fatalf("expected no banner, got %q", b)
	}
	if b := SessionStartContext(OrgState{}); b != "" {
		t.Fatalf("signed-out banner must be empty, got %q", b)
	}
	// All classes off → a banner that says nothing is sent.
	f2 := newFake(allClasses()...)
	m2 := newTestManager(t, f2, nil)
	if b := SessionStartContext(m2.State()); !strings.Contains(b, "nothing is sent") {
		t.Fatalf("all-off banner: %q", b)
	}
}

func TestStoreRoundTrip(t *testing.T) {
	s := NewStore(filepath.Join(t.TempDir(), "sub", "c.json"))
	if s.Accepted("u") != "" {
		t.Fatal("empty store")
	}
	if err := s.SetAccepted("u", "v1"); err != nil {
		t.Fatal(err)
	}
	if s.Accepted("u") != "v1" || s.Accepted("other") != "" {
		t.Fatal("per-user isolation")
	}
	if err := s.Forget("u"); err != nil || s.Accepted("u") != "" {
		t.Fatal("forget")
	}
}

func testVars() NoticeVars {
	return NoticeVars{
		OrgName:           "Acme",
		Harnesses:         []string{"Claude Code", "Codex"},
		DeviceDisplayName: "macOS device 3f2a",
		HarnessVersions:   []string{"Claude Code 2.1.219", "Codex 0.147.0"},
	}
}

func ptr[T any](v T) *T { return &v }

func waitFor(t *testing.T, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal("condition not met")
}

// An unresolved build-state marker (for example a per-plan retention period
// such as "{30 / 90 / 365}") is refused, never shown to the member; braces
// in an organization or device name are just text.
func TestRenderNoticeRefusesUnresolvedMarkers(t *testing.T) {
	for _, marker := range []string{"{30 / 90 / 365}", `{Enterprise only: ". Totals"}`, "{publication date}"} {
		v := testVars()
		v.Edition = notices.Edition{ID: "test", Text: notices.AgentTelemetryV1 + "\n" + marker + "\n"}
		if _, err := RenderNotice(v); !errors.Is(err, ErrUnfilledNotice) {
			t.Errorf("%s: unresolved marker must be refused, got %v", marker, err)
		}
	}
	v := testVars()
	v.OrgName, v.DeviceDisplayName = "Acme {EU}", "my {work} laptop"
	text, err := RenderNotice(v)
	if err != nil {
		t.Fatalf("braces in member-controlled names must render: %v", err)
	}
	if !strings.Contains(text, "Acme {EU}") || !strings.Contains(text, "my {work} laptop") {
		t.Fatal("names not filled verbatim")
	}
}
