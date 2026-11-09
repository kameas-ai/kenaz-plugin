package memory

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"

	"github.com/kameas-ai/kenaz-plugin/notices"
)

// grantEnv is a signed-in account with sync on and a submitter that records
// what asked for approval.
type grantEnv struct {
	svc  *Service
	sub  *countingSubmitter
	st   *Store
	prof Profile
}

type countingSubmitter struct {
	mu  sync.Mutex
	ops []string
}

func (c *countingSubmitter) Submit(_ context.Context, r ApprovalRequest) (Pending, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.ops = append(c.ops, r.Operation)
	return Pending{RequestID: "r", Status: ApprovalPending, ApproveURL: "http://x"}, nil
}

func (c *countingSubmitter) Status(context.Context, string) (ApprovalStatus, []byte, error) {
	return ApprovalPending, nil, nil
}

func newGrantEnv(t *testing.T) *grantEnv {
	t.Helper()
	ctx := context.Background()
	root := t.TempDir()
	p := Profile{Env: "dev", OrgID: "o", UserID: "u"}
	st, err := OpenProfile(ProfilePath(root, p), p)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	if _, err := st.EnableSync(ctx, "memory-sync/v4", MutateOptions{}); err != nil {
		t.Fatal(err)
	}
	sub := &countingSubmitter{}
	svc := NewService(ServiceOptions{StateRoot: root, Submitter: sub, Active: func(context.Context) Profile { return p }, ConsentVersion: "memory-sync/v4"})
	return &grantEnv{svc: svc, sub: sub, st: st, prof: p}
}

func globalNote(title string) NewNote {
	return NewNote{ProjectKey: GlobalProject, Title: title, Body: "body", Source: SourceClaudeCode}
}

// A note that can never sync does not ask for an approval it does not need.
func TestSaveThatCannotSyncNeedsNoApproval(t *testing.T) {
	e := newGrantEnv(t)
	out, err := e.svc.Save(context.Background(), NewNote{ProjectKey: "p1", Title: "project note", Body: "b", Source: SourceClaudeCode})
	if err != nil || out.Status != OutcomeDone || len(e.sub.ops) != 0 {
		t.Fatalf("out %+v, err %v, approvals %v", out, err, e.sub.ops)
	}
	// Editing or deleting it stays on the device too.
	if out, err := e.svc.Update(context.Background(), out.ID, Update{Title: ptrString("renamed")}); err != nil || out.Status != OutcomeDone {
		t.Errorf("update: %+v %v", out, err)
	}
	if out, err := e.svc.Forget(context.Background(), out.ID); err != nil || out.Status != OutcomeDone {
		t.Errorf("forget: %+v %v", out, err)
	}
	if len(e.sub.ops) != 0 {
		t.Errorf("approvals %v", e.sub.ops)
	}
}

func ptrString(s string) *string { return &s }

func TestStandingGrantCoversSavesOnly(t *testing.T) {
	e := newGrantEnv(t)
	ctx := context.Background()
	// Without the grant, a save that would sync asks.
	if out, err := e.svc.Save(ctx, globalNote("first")); err != nil || out.Status != OutcomeApprovalPending {
		t.Fatalf("no grant: %+v %v", out, err)
	}
	// Asking for the grant asks, once.
	if out, err := e.svc.SetSyncAuto(ctx, true); err != nil || out.Status != OutcomeApprovalPending {
		t.Fatalf("grant request: %+v %v", out, err)
	}
	if e.sub.ops[len(e.sub.ops)-1] != OpSyncAuto {
		t.Errorf("ops %v", e.sub.ops)
	}
	if err := e.st.GrantSaves(ctx, "k"); err != nil {
		t.Fatal(err)
	}
	if err := e.st.GrantSaves(ctx, "k"); err != nil {
		t.Errorf("a replayed approval: %v", err)
	}
	before := len(e.sub.ops)
	out, err := e.svc.Save(ctx, globalNote("second"))
	if err != nil || out.Status != OutcomeDone || !out.SyncAuto || out.SyncState != StatePending || len(e.sub.ops) != before {
		t.Fatalf("with grant: %+v %v, approvals %d", out, err, len(e.sub.ops)-before)
	}
	st, err := e.st.Status(ctx)
	if err != nil || !st.SaveGrant || st.Queue["ready"] != 1 {
		t.Errorf("status %+v %v", st, err)
	}
	// Update and forget still ask.
	if o, err := e.svc.Update(ctx, out.ID, Update{Title: ptrString("x")}); err != nil || o.Status != OutcomeApprovalPending {
		t.Errorf("update under the grant: %+v %v", o, err)
	}
	if o, err := e.svc.Forget(ctx, out.ID); err != nil || o.Status != OutcomeApprovalPending {
		t.Errorf("forget under the grant: %+v %v", o, err)
	}
	if o, err := e.svc.ForgetAll(ctx); err != nil || o.Status != OutcomeApprovalPending {
		t.Errorf("forget-all under the grant: %+v %v", o, err)
	}
}

func TestGrantRevocationAndClearing(t *testing.T) {
	ctx := context.Background()
	has := func(e *grantEnv) bool {
		s, err := e.st.Status(ctx)
		if err != nil {
			t.Fatal(err)
		}
		return s.SaveGrant
	}
	e := newGrantEnv(t)
	if err := e.st.GrantSaves(ctx, ""); err != nil {
		t.Fatal(err)
	}
	if out, err := e.svc.SetSyncAuto(ctx, false); err != nil || out.Status != OutcomeDone || has(e) {
		t.Errorf("revoke: %+v %v", out, err)
	}
	if out, _ := e.svc.Save(ctx, globalNote("after revoke")); out.Status != OutcomeApprovalPending {
		t.Errorf("after revoke: %+v", out)
	}
	if err := e.st.GrantSaves(ctx, ""); err != nil {
		t.Fatal(err)
	}
	if err := e.st.DisableSync(ctx, "disabled_by_user"); err != nil || has(e) {
		t.Errorf("disabling sync must clear the grant: %v", err)
	}
	if err := e.st.GrantSaves(ctx, ""); !errors.Is(err, ErrSyncOffForGrant) {
		t.Errorf("a grant with sync off: %v", err)
	}
	if out, err := e.svc.SetSyncAuto(ctx, true); err != nil || out.Message != SyncAutoNeedsSync {
		t.Errorf("asking with sync off: %+v %v", out, err)
	}
	f := newGrantEnv(t)
	if err := f.st.GrantSaves(ctx, ""); err != nil {
		t.Fatal(err)
	}
	if _, err := f.st.ForgetAll(ctx, MutateOptions{}); err != nil || has(f) {
		t.Errorf("forget-all must clear the grant: %v", err)
	}
}

func TestHourlyCapHoldsSavesWithoutLosingThem(t *testing.T) {
	e := newGrantEnv(t)
	ctx := context.Background()
	if err := e.st.GrantSaves(ctx, ""); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < MaxGrantedSavesPerHour; i++ {
		granted, within, err := e.st.SpendSaveGrant(ctx)
		if err != nil || !granted || !within {
			t.Fatalf("spend %d: %v %v %v", i, granted, within, err)
		}
	}
	out, err := e.svc.Save(ctx, globalNote("over the cap"))
	if err != nil || out.Status != OutcomeDone || out.SyncAuto || out.SyncState != StateUnapproved {
		t.Fatalf("over the cap: %+v %v", out, err)
	}
	if found, _, _ := e.st.Get(ctx, []string{out.ID}); len(found) != 1 {
		t.Error("the note was lost")
	}
}

// The approval page follows the notice in force.
func TestForgetAllWordingFollowsTheRelease(t *testing.T) {
	if ProjectScopeRelease {
		t.Skip("project-scope build")
	}
	if eraseScopes() != "'global','long_term'" || !strings.Contains(ForgetAllImpact(0), "single project are kept") {
		t.Errorf("before the release: %s / %s", eraseScopes(), ForgetAllImpact(0))
	}
	ProjectScopeRelease = true
	defer func() { ProjectScopeRelease = false }()
	if eraseScopes() != "'global','long_term','project'" || ForgetAllSummary() != "Erase all your memory notes?" ||
		!strings.Contains(ForgetAllImpact(0), "including notes on other devices that never synced") {
		t.Errorf("at the release: %s / %s / %s", eraseScopes(), ForgetAllSummary(), ForgetAllImpact(0))
	}
	// The page says what memory-sync/v4 says about other devices, word for word, and
	// nothing the interim text replaced.
	if !strings.Contains(ForgetAllImpact(0), ForgetAllInterim) || !strings.Contains(notices.MemorySyncV4, ForgetAllInterim) {
		t.Errorf("the page and v4 differ: %s", ForgetAllImpact(0))
	}
	for _, gone := range []string{"On a Kenaz Harness device", "isn't syncing keeps its notes", "aren't available"} {
		if strings.Contains(ForgetAllImpact(0), gone) {
			t.Errorf("the page still says %q", gone)
		}
	}
}

// A grant holds under the notice it was given under.
func TestGrantIsBoundToTheNotice(t *testing.T) {
	ctx := context.Background()
	e := newGrantEnv(t)
	if err := e.st.GrantSaves(ctx, ""); err != nil {
		t.Fatal(err)
	}
	if _, err := e.st.EnableSync(ctx, "memory-sync/v5", MutateOptions{}); err != nil {
		t.Fatal(err)
	}
	if granted, _, err := e.st.SpendSaveGrant(ctx); err != nil || granted {
		t.Errorf("after a new notice: granted %v, %v", granted, err)
	}
	// Re-enabling under the same notice keeps it.
	if err := e.st.GrantSaves(ctx, ""); err != nil {
		t.Fatal(err)
	}
	if _, err := e.st.EnableSync(ctx, "memory-sync/v5", MutateOptions{}); err != nil {
		t.Fatal(err)
	}
	if granted, _, _ := e.st.SpendSaveGrant(ctx); !granted {
		t.Error("the same notice lost the grant")
	}
	// A grant whose notice changed behind it no longer stands either.
	if _, err := e.st.db.ExecContext(ctx, `UPDATE sync_state SET consent_version = 'memory-sync/v6' WHERE singleton = 1`); err != nil {
		t.Fatal(err)
	}
	if granted, _, _ := e.st.SpendSaveGrant(ctx); granted {
		t.Error("spent a grant from another notice")
	}
}

// Which scopes reach Fleet follows the project-scope release.
func TestSyncedScopeFollowsTheRelease(t *testing.T) {
	if syncedScope(ScopeProject) {
		t.Error("project notes sync before the release")
	}
	ProjectScopeRelease = true
	defer func() { ProjectScopeRelease = false }()
	if !syncedScope(ScopeProject) || !syncedScope(ScopeGlobal) {
		t.Error("project notes do not sync at the release")
	}
}

// A note that can never sync because of its kind asks for nothing, and saves.
// Changing an automatic note that never left this device asks nothing either; a
// deliberate note still does.
func TestAutomaticUpdateNeedsNoApproval(t *testing.T) {
	ctx := context.Background()
	e := newGrantEnv(t)
	auto, err := e.svc.Save(ctx, NewNote{ProjectKey: GlobalProject, Title: "agent note", Body: "b", Source: SourceCLI, Kind: KindObservation})
	if err != nil || auto.ID == "" {
		t.Fatalf("%+v %v", auto, err)
	}
	body := "changed"
	if out, err := e.svc.Update(ctx, auto.ID, Update{Body: &body}); err != nil || out.Status != OutcomeDone || len(e.sub.ops) != 0 {
		t.Errorf("update: %+v %v, approvals %v", out, err, e.sub.ops)
	}
	if out, err := e.svc.Forget(ctx, auto.ID); err != nil || out.Status != OutcomeDone || len(e.sub.ops) != 0 {
		t.Errorf("forget: %+v %v, approvals %v", out, err, e.sub.ops)
	}
	note, err := e.st.SaveNote(ctx, NewNote{ProjectKey: GlobalProject, Title: "on purpose", Body: "b2", Source: SourceCLI}, SaveOptions{SyncApproved: true})
	if err != nil {
		t.Fatal(err)
	}
	if out, err := e.svc.Update(ctx, note.ID, Update{Body: &body}); err != nil || out.Status != OutcomeApprovalPending {
		t.Errorf("deliberate update: %+v %v", out, err)
	}
}

func TestAutomaticSaveNeedsNoApproval(t *testing.T) {
	e := newGrantEnv(t)
	out, err := e.svc.Save(context.Background(), NewNote{ProjectKey: GlobalProject, Title: "agent note", Body: "b", Source: SourceCodex, Kind: KindObservation})
	if err != nil || out.Status != OutcomeDone || len(e.sub.ops) != 0 {
		t.Fatalf("%+v %v, approvals %v", out, err, e.sub.ops)
	}
}

// The grant's page says it covers automatic notes, in a build that has them.
func TestGrantPageNamesAutomaticNotes(t *testing.T) {
	defer func(old bool) { AutomaticNotesBuild = old }(AutomaticNotesBuild)
	AutomaticNotesBuild = false
	if strings.Contains(SyncAutoImpactText(), "Automatic") {
		t.Error("a build without capture mentions automatic notes")
	}
	AutomaticNotesBuild = true
	if !strings.Contains(SyncAutoImpactText(), "Automatic notes sync the same way, including ones already on this device.") {
		t.Error(SyncAutoImpactText())
	}
}
