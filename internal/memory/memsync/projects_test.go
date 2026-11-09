package memsync

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/kameas-ai/kenaz-plugin/fleetapi"
	"github.com/kameas-ai/kenaz-plugin/fleetapi/allowlist"
	"github.com/kameas-ai/kenaz-plugin/fleetapi/fleetapitest"
	"github.com/kameas-ai/kenaz-plugin/internal/memory"
)

const (
	consentV4 = "memory-sync/v4"
	repoKey   = "rk1-0123456789abcdef0123456789abcdef"
)

func projectRelease(t *testing.T) {
	t.Helper()
	old := memory.ProjectScopeRelease
	memory.ProjectScopeRelease = true
	t.Cleanup(func() { memory.ProjectScopeRelease = old })
}

// projectDevice is a device with sync on under memory-sync/v4 and the standing grant.
func projectDevice(t *testing.T, f *fleetapitest.MemoryFleet) *device {
	t.Helper()
	d := newDevice(t, f, alice)
	d.useProjects(t, f.SessionClient(d.sessionFn))
	ctx := context.Background()
	on := true
	scopes := []string{memory.ScopeGlobal, memory.ScopeLongTerm}
	if _, err := f.SessionClient(d.sessionFn).PutSettings(fleetapi.WithExpectedSession(ctx, fleetapi.ExpectedSession{Env: fleetapi.Env(alice.Env), OrgID: alice.OrgID, Subject: alice.UserID}),
		fleetapi.MemorySettingsUpdate{Enabled: &on, Scopes: &scopes, ConsentVersion: consentV4}); err != nil {
		t.Fatal(err)
	}
	if _, err := d.st.EnableSync(ctx, consentV4, memory.MutateOptions{SyncApproved: true}); err != nil {
		t.Fatal(err)
	}
	if err := d.st.GrantSaves(ctx, ""); err != nil {
		t.Fatal(err)
	}
	return d
}

func (d *device) useProjects(t *testing.T, m fleetapi.Memory) {
	t.Helper()
	d.w = New(Options{Store: d.st, Memory: m, Projects: d.fleet.ProjectsClient(), Session: d.sessionFn, ConsentVersion: consentV4,
		Holder: d.holder, Log: slog.New(slog.DiscardHandler), PullLimit: 50, Capabilities: d.fleetCaps})
}

// fleetCaps is what Fleet's capabilities say: memory_project_scope exactly while it
// syncs project notes (FLEET_MEMORY_PROJECT_SCOPE), absent otherwise.
func (d *device) fleetCaps(context.Context) (fleetapi.Capabilities, error) {
	keys := map[string]bool{fleetapi.CapMemorySync: true}
	if d.fleet.ProjectScope {
		keys[fleetapi.CapMemoryProjectScope] = true
	}
	return fleetapi.Capabilities{Keys: keys}, nil
}

func (d *device) saveProject(key, title, body string) string {
	d.t.Helper()
	r, err := d.st.SaveNote(context.Background(), memory.NewNote{ProjectKey: key, ProjectRoot: "/somewhere/secret-repo-name", Title: title, Body: body, Source: memory.SourceCLI},
		memory.SaveOptions{SyncApproved: true})
	if err != nil {
		d.t.Fatal(err)
	}
	return r.ID
}

func (d *device) syncN(n int) {
	d.t.Helper()
	for i := 0; i < n; i++ {
		d.sync()
	}
}

// Against a Fleet with the project flag off, no note leaves and nothing is lost.
func TestProjectNotesWaitForAFleetWithoutTheFlag(t *testing.T) {
	projectRelease(t)
	f := fleetapitest.NewMemoryFleet()
	puts := 0
	f.Fault = func(call string, after bool) error {
		if call == "put_settings" && !after {
			puts++
		}
		return nil
	}
	d := projectDevice(t, f)
	id := d.saveProject(repoKey, "a project note", "project text")
	global := d.save("a global note", "global text", true)
	base := time.Now()
	d.st.SetClock(func() time.Time { return base })

	d.syncN(5)
	st := d.status()
	// Fleet's capabilities don't carry memory_project_scope, so the device doesn't even ask.
	if st.ProjectHeld != memory.CodeProjectScopeNotOffered {
		t.Errorf("%+v", st)
	}
	if st.Notes[memory.StateRejected] != 0 {
		t.Errorf("refused for good: %+v", st)
	}
	if got := f.ProjectCalls(memory.ProjectRequestID(repoKey)); got != 0 {
		t.Errorf("asked for an id %d times while Fleet can't take the notes", got)
	}
	if puts != 1 { // one to turn sync on; project notes aren't asked for
		t.Errorf("%d settings writes in 5 cycles", puts)
	}
	if n, ok := d.get(global); !ok || n.SyncState != memory.StateSynced {
		t.Errorf("a global note was held up: %+v", n)
	}

	// The flag comes on: the next try after the wait syncs it, under its id.
	f.ProjectScope = true
	d.st.SetClock(func() time.Time { return base.Add(2 * time.Hour) })
	d.syncN(2)
	n, ok := d.get(id)
	if !ok || n.SyncState != memory.StateSynced {
		t.Fatalf("after the flag: %+v %+v", n, d.status())
	}
	if d.status().ProjectHeld != "" {
		t.Errorf("%+v", d.status())
	}
	if got := f.ProjectCalls(memory.ProjectRequestID(repoKey)); got != 1 {
		t.Errorf("%d id requests", got)
	}
	// Another device of the same account sees it under that id.
	other := projectDevice(t, f)
	other.syncN(1)
	got, _, err := other.st.Get(context.Background(), []string{id})
	if err != nil || len(got) != 1 {
		t.Fatalf("%+v %v", got, err)
	}
}

// What leaves is the id. The key, the path and the repository's name don't.
func TestProjectKeyAndNameStayHere(t *testing.T) {
	projectRelease(t)
	f := fleetapitest.NewMemoryFleet()
	f.ProjectScope = true
	d := projectDevice(t, f)
	d.saveProject(repoKey, "a note", "text")
	ctx := context.Background()
	if err := d.st.SetProject(ctx, repoKey, "00000000-0000-4000-8000-000000000042", "active", ""); err != nil {
		t.Fatal(err)
	}
	l, err := d.st.AcquireLease(ctx, d.holder, leaseTTL)
	if err != nil {
		t.Fatal(err)
	}
	b, err := d.st.LeasePushBatch(ctx, l)
	if err != nil || len(b.Ops) != 1 {
		t.Fatalf("%+v %v", b, err)
	}
	if s := string(b.Ops[0].Payload); strings.Contains(s, repoKey) || strings.Contains(s, "/somewhere") || strings.Contains(s, "project_key") || !strings.Contains(s, `"scope_id":"00000000-0000-4000-8000-000000000042"`) {
		t.Errorf("payload %s", s)
	}
	items, err := toItems(b.Ops)
	if err != nil {
		t.Fatal(err)
	}
	body, _ := json.Marshal(fleetapi.MemoryPushRequest{DeviceID: d.st.DeviceID(), Items: items})
	if err := allowlist.Memory().CheckPushBody(body); err != nil {
		t.Fatal(err)
	}
}

// A deleted key is final: it is never asked for again, and its notes stay here.
func TestDeletedProjectKeyIsFinal(t *testing.T) {
	projectRelease(t)
	f := fleetapitest.NewMemoryFleet()
	f.ProjectScope = true
	req := memory.ProjectRequestID(repoKey)
	// Another device minted it, and it was deleted.
	first, err := f.ProjectsClient().Create(context.Background(), fleetapi.ProjectCreateRequest{Name: "x", Audience: "private", ClientRequestID: req})
	if err != nil {
		t.Fatal(err)
	}
	f.SetProjectState(first.ID, fleetapi.ProjectDeleted, "")
	d := projectDevice(t, f)
	id := d.saveProject(repoKey, "a note", "text")
	d.syncN(6)
	if got := f.ProjectCalls(req); got != 2 { // that device's, and this one's replay
		t.Errorf("%d id requests", got)
	}
	n, ok := d.get(id)
	if !ok || n.SyncState != memory.StateRejected || n.SyncCode != "project_deleted" {
		t.Errorf("%+v", n)
	}
	// A new note under that key stays here from the start.
	id2 := d.saveProject(repoKey, "another", "more text")
	d.syncN(3)
	n, _ = d.get(id2)
	if n.SyncState != memory.StateLocal || n.SyncCode != "project_deleted" {
		t.Errorf("%+v", n)
	}
	if got := f.ProjectCalls(req); got != 2 {
		t.Errorf("%d id requests after the key was final", got)
	}
}

// Deleted after it was mapped: Fleet's refusal makes it final the same way.
func TestProjectDeletedOnPushIsFinal(t *testing.T) {
	projectRelease(t)
	f := fleetapitest.NewMemoryFleet()
	f.ProjectScope = true
	d := projectDevice(t, f)
	d.saveProject(repoKey, "first", "text one")
	d.syncN(2)
	req := memory.ProjectRequestID(repoKey)
	f.SetProjectState(f.ProjectID(req), fleetapi.ProjectDeleted, "")
	id := d.saveProject(repoKey, "second", "text two")
	d.syncN(3)
	n, _ := d.get(id)
	if n.SyncState != memory.StateRejected || n.SyncCode != "project_deleted" {
		t.Errorf("%+v", n)
	}
	if got := f.ProjectCalls(req); got != 1 {
		t.Errorf("%d id requests", got)
	}
}

// A merged project: push follows canonical_id, and a replay follows merged_into.
func TestMergedProjectIsFollowed(t *testing.T) {
	projectRelease(t)
	f := fleetapitest.NewMemoryFleet()
	f.ProjectScope = true
	ctx := context.Background()
	target, err := f.ProjectsClient().Create(ctx, fleetapi.ProjectCreateRequest{Name: "t", Audience: "private", ClientRequestID: "memproj:target"})
	if err != nil {
		t.Fatal(err)
	}
	d := projectDevice(t, f)
	d.saveProject(repoKey, "first", "text one")
	d.syncN(2)
	req := memory.ProjectRequestID(repoKey)
	f.SetProjectState(f.ProjectID(req), fleetapi.ProjectMerged, target.ID)

	// On push.
	id := d.saveProject(repoKey, "second", "text two")
	d.syncN(2)
	n, _ := d.get(id)
	if n.SyncState != memory.StateSynced {
		t.Errorf("push: %+v %+v", n, d.status())
	}

	// On a replay.
	other := projectDevice(t, f)
	id2 := other.saveProject(repoKey, "third", "text three")
	other.syncN(2)
	n, _ = other.get(id2)
	if n.SyncState != memory.StateSynced {
		t.Errorf("replay: %+v %+v", n, other.status())
	}
}

// A note pulled under an id this device hasn't mapped files under its real project
// once the device maps that key.
func TestPulledNoteFindsItsProject(t *testing.T) {
	projectRelease(t)
	f := fleetapitest.NewMemoryFleet()
	f.ProjectScope = true
	a := projectDevice(t, f)
	id := a.saveProject(repoKey, "from a", "text from a")
	a.syncN(2)

	b := projectDevice(t, f)
	b.syncN(1)
	n, ok := b.get(id)
	if !ok || n.Scope != memory.ScopeProject {
		t.Fatalf("%+v", n)
	}
	if n.ProjectKey == repoKey {
		t.Fatalf("mapped already: %+v", n)
	}
	b.saveProject(repoKey, "from b", "text from b")
	b.syncN(2)
	n, _ = b.get(id)
	if n.ProjectKey != repoKey {
		t.Errorf("still under %q", n.ProjectKey)
	}
	// A merge arrives by pull: the key stays, and the note stays filed under it.
	target, err := f.ProjectsClient().Create(context.Background(), fleetapi.ProjectCreateRequest{Name: "t", Audience: "private", ClientRequestID: "memproj:target"})
	if err != nil {
		t.Fatal(err)
	}
	from := f.ProjectID(memory.ProjectRequestID(repoKey))
	f.SetProjectState(from, fleetapi.ProjectMerged, target.ID)
	f.RepointNotes(alice.OrgID, alice.UserID, from, target.ID)
	b.syncN(1)
	n, _ = b.get(id)
	if n.ProjectKey != repoKey {
		t.Errorf("after the merge: %q", n.ProjectKey)
	}
}

// A project with no network remote can't mean the same thing elsewhere, so it stays.
func TestPathKeyedProjectNeverSyncs(t *testing.T) {
	projectRelease(t)
	f := fleetapitest.NewMemoryFleet()
	f.ProjectScope = true
	d := projectDevice(t, f)
	id := d.saveProject("0123456789abcdef", "local project", "text")
	d.syncN(2)
	n, _ := d.get(id)
	if n.SyncState != memory.StateLocal || n.SyncCode != "project_not_portable" {
		t.Errorf("%+v", n)
	}
	if got := f.Calls("push"); got != 0 {
		t.Errorf("%d pushes", got)
	}
}

// Both waiting refusals use up no attempts and refuse nothing for good, through
// ApplyPushResults.
func TestProjectRefusalsBackOffWithoutUsingAttempts(t *testing.T) {
	projectRelease(t)
	f := fleetapitest.NewMemoryFleet()
	d := projectDevice(t, f)
	ctx := context.Background()
	id := d.saveProject(repoKey, "a note", "text")
	if err := d.st.SetProject(ctx, repoKey, "00000000-0000-4000-8000-000000000001", "active", ""); err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	yes, no := true, false
	for i, c := range []struct {
		code  string
		retry *bool
	}{{"consent_upgrade_required", &yes}, {"consent_upgrade_required", &no}, {"scope_not_supported_yet", &no}, {"scope_not_enabled", &no}} {
		// Past every wait: this is a later try.
		d.st.SetClock(func() time.Time { return now.Add(time.Duration(i) * 30 * time.Hour) })
		l, err := d.st.AcquireLease(ctx, d.holder, leaseTTL)
		if err != nil {
			t.Fatal(err)
		}
		b, err := d.st.LeasePushBatch(ctx, l)
		if err != nil || len(b.Ops) != 1 {
			t.Fatalf("%s: %+v %v", c.code, b, err)
		}
		attempt := b.Ops[0].Attempt
		if attempt != 1 {
			t.Errorf("%s: attempt %d: an earlier refusal used one up", c.code, attempt)
		}
		if _, err := d.st.ApplyPushResults(ctx, l, b.Ops, []memory.PushResult{{ID: id, Status: "rejected", Code: c.code, Retry: c.retry}}); err != nil {
			t.Fatal(err)
		}
		st := d.status()
		if st.ProjectHeld != c.code || st.Notes[memory.StateRejected] != 0 {
			t.Errorf("%s: %+v", c.code, st)
		}
		// While the wait runs nothing goes.
		if b, err := d.st.LeasePushBatch(ctx, l); err != nil || len(b.Ops) != 0 {
			t.Errorf("%s: sent during the wait: %+v %v", c.code, b, err)
		}
	}
}

// A settings write that Fleet accepts but doesn't apply: read the answer.
type ignoresScope struct{ fleetapi.Memory }

func (m ignoresScope) PutSettings(ctx context.Context, u fleetapi.MemorySettingsUpdate) (fleetapi.MemorySettings, error) {
	return m.Memory.Settings(ctx)
}

func TestSettingsResponseIsReadNotAssumed(t *testing.T) {
	projectRelease(t)
	f := fleetapitest.NewMemoryFleet()
	f.ProjectScope = true
	d := projectDevice(t, f)
	d.useProjects(t, ignoresScope{f.SessionClient(d.sessionFn)})
	d.saveProject(repoKey, "a note", "text")
	d.syncN(2)
	if got := d.status().ProjectHeld; got != "scope_not_enabled" {
		t.Errorf("held %q", got)
	}
	if got := f.ProjectCalls(memory.ProjectRequestID(repoKey)); got != 0 {
		t.Errorf("%d id requests", got)
	}
}

// The version sent is the one this store acknowledged, never one made up here.
func TestSettingsWriteSendsTheAcknowledgedNotice(t *testing.T) {
	projectRelease(t)
	f := fleetapitest.NewMemoryFleet()
	f.ProjectScope = true
	d := projectDevice(t, f)
	var sent []string
	d.useProjects(t, recordConsent{f.SessionClient(d.sessionFn), &sent})
	d.saveProject(repoKey, "a note", "text")
	d.syncN(1)
	if len(sent) != 1 || sent[0] != consentV4 {
		t.Errorf("sent %q", sent)
	}
}

type recordConsent struct {
	fleetapi.Memory
	sent *[]string
}

func (m recordConsent) PutSettings(ctx context.Context, u fleetapi.MemorySettingsUpdate) (fleetapi.MemorySettings, error) {
	*m.sent = append(*m.sent, u.ConsentVersion)
	return m.Memory.PutSettings(ctx, u)
}

// An automatic note in a project waits on either refusal, and goes once both are open.
func TestAutomaticProjectNoteNeedsBoth(t *testing.T) {
	projectRelease(t)
	f := fleetapitest.NewMemoryFleet()
	f.ProjectScope = true
	f.RefuseCaptureKinds = true
	d := projectDevice(t, f)
	ctx := context.Background()
	if _, _, err := d.st.SaveAuto(ctx, memory.AutoNote{ProjectKey: repoKey, ProjectRoot: "/r", Title: "observed", Body: "what happened", Source: memory.SourceCLI, Kind: memory.KindObservation}); err != nil {
		t.Fatal(err)
	}
	base := time.Now()
	d.st.SetClock(func() time.Time { return base })
	d.syncN(3)
	st := d.status()
	if st.AutoHeld != "invalid_field" || st.Notes[memory.StateRejected] != 0 {
		t.Errorf("%+v", st)
	}
	f.RefuseCaptureKinds = false
	d.st.SetClock(func() time.Time { return base.Add(2 * time.Hour) })
	d.syncN(2)
	if st := d.status(); st.Notes[memory.StateSynced] != 1 || st.AutoHeld != "" {
		t.Errorf("%+v", st)
	}
}

// A forget on an id that yielded to another note forgets only that id: the kept note is
// a separate record.
func TestForgetOnYieldedIDLeavesTheKeptNote(t *testing.T) {
	projectRelease(t)
	f := fleetapitest.NewMemoryFleet()
	f.ProjectScope = true
	d := projectDevice(t, f)
	ctx := context.Background()
	kept := d.save("kept", "same text", true)
	yielded := d.save("yielding", "other text", true)
	l, err := d.st.AcquireLease(ctx, d.holder, leaseTTL)
	if err != nil {
		t.Fatal(err)
	}
	b, err := d.st.LeasePushBatch(ctx, l)
	if err != nil || len(b.Ops) != 2 {
		t.Fatalf("%+v %v", b, err)
	}
	if _, err := d.st.ApplyPushResults(ctx, l, b.Ops, []memory.PushResult{{ID: kept, Status: "accepted", Seq: "1"}, {ID: yielded, Status: "superseded", SupersededBy: kept, Seq: "2"}}); err != nil {
		t.Fatal(err)
	}
	if _, ok := d.get(yielded); ok {
		t.Fatal("the yielded id is still here")
	}
	if _, err := d.st.ForgetNote(ctx, yielded, memory.MutateOptions{SyncApproved: true}); err == nil {
		t.Log("forgetting the yielded id did something")
	}
	if n, ok := d.get(kept); !ok || n.SyncState != memory.StateSynced {
		t.Errorf("the kept note: %+v %v", n, ok)
	}
}

// Project notes held up don't hold up automatic notes outside a project.
func TestHeldProjectNoteDoesNotBlockOtherAutomaticNotes(t *testing.T) {
	projectRelease(t)
	f := fleetapitest.NewMemoryFleet()
	d := projectDevice(t, f)
	ctx := context.Background()
	if _, _, err := d.st.SaveAuto(ctx, memory.AutoNote{ProjectKey: repoKey, ProjectRoot: "/r", Title: "in a project", Body: "project text", Source: memory.SourceCLI, Kind: memory.KindObservation}); err != nil {
		t.Fatal(err)
	}
	d.syncN(2)
	if got := d.status().ProjectHeld; got == "" {
		t.Fatal("not held")
	}
	if _, _, err := d.st.SaveAuto(ctx, memory.AutoNote{ProjectKey: memory.GlobalProject, Title: "global", Body: "global text", Source: memory.SourceCLI, Kind: memory.KindObservation}); err != nil {
		t.Fatal(err)
	}
	d.syncN(2)
	if st := d.status(); st.Notes[memory.StateSynced] != 1 {
		t.Errorf("%+v", st)
	}
}

// The name is the folder's: never a path or a remote, and never a credential.
func TestProjectName(t *testing.T) {
	const home = "/somewhere/me"
	for _, c := range []struct{ root, want string }{
		{"/somewhere/app", "app"},
		{"/somewhere/app/", "app"},
		{"", "Project 01234567"},
		{"/", "Project 01234567"},
		{"/somewhere/AKIAABCDEFGHIJKLMNOP", "Project 01234567"},
		{"/somewhere/" + strings.Repeat("x", 121), "Project 01234567"},
		{"/somewhere/a\nb", "Project 01234567"},
		{home, "Project 01234567"},
		{"/Users/alex", "Project 01234567"},
		{"/home/someone", "Project 01234567"},
		{`C:\Users\someone`, "Project 01234567"},
		{"/Users/alex/code/app", "app"},
		{"/SOMEWHERE/ME", "Project 01234567"},
		{"/nfs/u/me", "Project 01234567"},
		{`\\server\alice`, "Project 01234567"},
		{`\\server`, "Project 01234567"},
		{`\\server\share\repo`, "repo"},
		{`\\server\users\alice`, "Project 01234567"},
		{"..", "Project 01234567"},
		{"/x/..", "Project 01234567"},
		{".", "Project 01234567"},
		{`C:\\`, "Project 01234567"},
	} {
		if got := memory.ProjectName(repoKey, c.root, home, "/nfs/u/me"); got != c.want {
			t.Errorf("%q: %q, want %q", c.root, got, c.want)
		}
	}
}

// What was asked for is the folder's name, and Fleet refusing it costs nothing.
type refusesNames struct{ fleetapi.Projects }

func (r refusesNames) Create(ctx context.Context, req fleetapi.ProjectCreateRequest) (fleetapi.Project, error) {
	if !strings.HasPrefix(req.Name, "Project ") {
		return fleetapi.Project{}, &fleetapi.APIError{Status: 400, Code: "secret_detected"}
	}
	return r.Projects.Create(ctx, req)
}

func TestRefusedProjectNameFallsBack(t *testing.T) {
	projectRelease(t)
	f := fleetapitest.NewMemoryFleet()
	f.ProjectScope = true
	d := projectDevice(t, f)
	d.w = New(Options{Store: d.st, Memory: f.SessionClient(d.sessionFn), Projects: refusesNames{f.ProjectsClient()}, Session: d.sessionFn, ConsentVersion: consentV4,
		Holder: d.holder, Log: slog.New(slog.DiscardHandler), PullLimit: 50, Capabilities: d.fleetCaps})
	id := d.saveProject(repoKey, "a note", "text")
	d.syncN(2)
	if n, _ := d.get(id); n.SyncState != memory.StateSynced {
		t.Errorf("%+v", n)
	}
}

// Sync turned back on through an older client drops project notes. This device doesn't
// bring them back on its own; a fresh acknowledgement does.
func TestOlderClientReEnableNeedsAFreshAcknowledgement(t *testing.T) {
	projectRelease(t)
	f := fleetapitest.NewMemoryFleet()
	f.ProjectScope = true
	d := projectDevice(t, f)
	ctx := fleetapi.WithExpectedSession(context.Background(), fleetapi.ExpectedSession{Env: fleetapi.Env(alice.Env), OrgID: alice.OrgID, Subject: alice.UserID})
	m := f.SessionClient(d.sessionFn)
	d.saveProject(repoKey, "first", "text one")
	d.syncN(2)
	cur, err := m.Settings(ctx)
	if err != nil || !strings.Contains(strings.Join(cur.Scopes, ","), "project") {
		t.Fatalf("%+v %v", cur, err)
	}
	// An older client saves its fixed scopes, as a re-enable from off does.
	off, on := false, true
	if _, err := m.PutSettings(ctx, fleetapi.MemorySettingsUpdate{Enabled: &off}); err != nil {
		t.Fatal(err)
	}
	old := []string{memory.ScopeGlobal, memory.ScopeLongTerm}
	if _, err := m.PutSettings(ctx, fleetapi.MemorySettingsUpdate{Enabled: &on, Scopes: &old, ConsentVersion: "2026-10-07"}); err != nil {
		t.Fatal(err)
	}
	id := d.saveProject(repoKey, "second", "text two")
	base := time.Now()
	d.st.SetClock(func() time.Time { return base.Add(48 * time.Hour) })
	d.useProjects(t, m) // the relay restarts, knowing nothing
	d.syncN(3)
	if got := d.status().ProjectHeld; got != "consent_upgrade_required" {
		t.Errorf("held %q", got)
	}
	if cur, _ := m.Settings(ctx); strings.Contains(strings.Join(cur.Scopes, ","), "project") {
		t.Error("this device put project back by itself")
	}
	if n, _ := d.get(id); n.SyncState != memory.StatePending {
		t.Errorf("%+v", n)
	}
	// The member confirms the notice again.
	if _, err := m.PutSettings(ctx, fleetapi.MemorySettingsUpdate{Enabled: &on, Scopes: &old, ConsentVersion: consentV4}); err != nil {
		t.Fatal(err)
	}
	if _, err := d.st.EnableSync(context.Background(), consentV4, memory.MutateOptions{SyncApproved: true}); err != nil {
		t.Fatal(err)
	}
	d.syncN(2)
	if n, _ := d.get(id); n.SyncState != memory.StateSynced {
		t.Errorf("after the acknowledgement: %+v %+v", n, d.status())
	}
}

// Notes kept only on this device change directly. What Fleet may hold doesn't.
func TestFleetMayHoldTracksWhatLeft(t *testing.T) {
	d, f := autoDevice(t, true)
	f.RefuseCaptureKinds = true
	ctx := context.Background()
	d.saveAuto(2)
	d.sync() // one went, and was refused
	ids := map[bool]int{}
	es, err := d.st.Recent(ctx, memory.AllProjects, 10)
	if err != nil || len(es) != 2 {
		t.Fatalf("%+v %v", es, err)
	}
	for _, e := range es {
		may, err := d.st.FleetMayHold(ctx, e.ID)
		if err != nil {
			t.Fatal(err)
		}
		ids[may]++
	}
	if ids[true] != 1 || ids[false] != 1 {
		t.Errorf("%v: a note that was sent may be held, and one never sent can't be", ids)
	}
}

// erasedSignal answers the first pull with a reset, reason erased: an empty cursor told
// of an erase. Fleet is choosing how it says this.
type erasedSignal struct {
	fleetapi.Memory
	before string
	sent   bool
}

func (m *erasedSignal) Pull(ctx context.Context, r fleetapi.MemoryPullRequest) (fleetapi.MemoryPullResponse, error) {
	if !m.sent {
		m.sent = true
		return fleetapi.MemoryPullResponse{Enabled: true, Reset: true, ResetReason: fleetapi.MemoryResetErased, ErasedBefore: m.before}, nil
	}
	return m.Memory.Pull(ctx, r)
}

// A device with an empty cursor and unsynced notes, told of a forget-all elsewhere,
// erases what came before it and sends none of that.
func TestEmptyCursorDeviceErasesBeforePushing(t *testing.T) {
	f := fleetapitest.NewMemoryFleet()
	d, _ := autoDeviceOn(t, f)
	ctx := context.Background()
	base := time.Now().Add(-time.Hour)
	d.at(base.Add(-time.Hour))
	old := d.save("from before", "text from before", true)
	d.st.SetClock(time.Now)
	kept := d.save("from after", "text from after", true)
	if c, err := d.st.Cursor(ctx); err != nil || c != "" {
		t.Fatalf("cursor %q %v", c, err)
	}
	var seen []string
	f.Fault = func(call string, after bool) error {
		if call == "push" && !after {
			seen = append(seen, call)
		}
		return nil
	}
	m := &erasedSignal{Memory: f.SessionClient(d.sessionFn), before: string(memory.FormatHLC(base.UnixMilli(), 0, "fleet"))}
	d.w = New(Options{Store: d.st, Memory: m, Session: d.sessionFn, ConsentVersion: consent, Holder: d.holder, Log: slog.New(slog.DiscardHandler), PullLimit: 50})
	d.sync()
	if _, ok := d.get(old); ok {
		t.Error("a note from before the erase survived")
	}
	if n, ok := d.get(kept); !ok {
		t.Error("a note from after the erase was lost")
	} else if n.SyncState != memory.StateSynced {
		t.Errorf("%+v", n)
	}
	exp, err := f.SessionClient(d.sessionFn).Export(fleetapi.WithExpectedSession(ctx, fleetapi.ExpectedSession{Env: fleetapi.Env(alice.Env), OrgID: alice.OrgID, Subject: alice.UserID}), func(r fleetapi.MemoryRecord) error {
		if r.ID == old {
			t.Error("Fleet got a note from before the erase")
		}
		return nil
	})
	if err != nil || exp.Count != 1 {
		t.Errorf("%+v %v", exp, err)
	}
}

// serviceFor is what `kenaz memory forget` goes through on this device, with an approval
// page if withApprovals.
func (d *device) serviceFor(withApprovals bool) (*memory.Service, *approvalsStub) {
	active := func(context.Context) memory.Profile { return d.p }
	a := &approvalsStub{exec: map[string]memory.Executor{}}
	memory.RegisterExecutors(a, memory.ExecOptions{StateRoot: d.dir, Active: active, ConsentVersion: consent})
	o := memory.ServiceOptions{StateRoot: d.dir, Active: active, ConsentVersion: consent}
	if withApprovals {
		o.Submitter = a
	}
	return memory.NewService(o), a
}

type approvalsStub struct {
	exec map[string]memory.Executor
	last memory.ApprovalRequest
	n    int
}

func (a *approvalsStub) Register(op string, ex memory.Executor)       { a.exec[op] = ex }
func (a *approvalsStub) RegisterReconciler(string, memory.Reconciler) {}
func (a *approvalsStub) Status(context.Context, string) (memory.ApprovalStatus, []byte, error) {
	return memory.ApprovalPending, nil, nil
}
func (a *approvalsStub) Submit(_ context.Context, q memory.ApprovalRequest) (memory.Pending, error) {
	a.last, a.n = q, a.n+1
	return memory.Pending{RequestID: "r", Status: memory.ApprovalPending, ApproveURL: "http://localhost:1/r"}, nil
}

// returnedNote is a note stamped ahead of Fleet's erase time, which an erased reset
// skips, so that it goes back to Fleet: a device whose clock was wrong.
func returnedNote(t *testing.T, f *fleetapitest.MemoryFleet, d *device) (string, time.Time) {
	t.Helper()
	base := time.Now()
	d.at(base.Add(4 * time.Minute)) // ahead of Fleet, within what Fleet allows
	id := d.save("a returned note", "it came back", true)
	d.at(base.Add(time.Second))
	m := &erasedSignal{Memory: f.SessionClient(d.sessionFn), before: string(memory.FormatHLC(base.UnixMilli(), 0, "fleet"))}
	d.w = New(Options{Store: d.st, Memory: m, Session: d.sessionFn, ConsentVersion: consent, Holder: d.holder, Log: slog.New(slog.DiscardHandler), PullLimit: 50})
	d.sync()
	if n, ok := d.get(id); !ok || n.SyncState != memory.StateSynced {
		t.Fatalf("the note should have gone back: %+v %v", n, ok)
	}
	return id, base
}

func autoDeviceOn(t *testing.T, f *fleetapitest.MemoryFleet) (*device, *fleetapitest.MemoryFleet) {
	t.Helper()
	d := newDevice(t, f, alice)
	d.enable()
	return d, f
}

func fleetHolds(t *testing.T, f *fleetapitest.MemoryFleet, d *device, id string) bool {
	t.Helper()
	held := false
	_, err := f.SessionClient(d.sessionFn).Export(fleetapi.WithExpectedSession(context.Background(), fleetapi.ExpectedSession{Env: fleetapi.Env(alice.Env), OrgID: alice.OrgID, Subject: alice.UserID}), func(r fleetapi.MemoryRecord) error {
		held = held || r.ID == id
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return held
}

// A note that came back can be forgotten the ordinary way: it asks for approval, and
// once approved it goes from this device, from Fleet and from the next device to pull.
func TestReturnedNoteCanBeForgotten(t *testing.T) {
	ctx := context.Background()
	f := fleetapitest.NewMemoryFleet()
	a, _ := autoDeviceOn(t, f)
	id, base := returnedNote(t, f, a)
	b, _ := autoDeviceOn(t, f)
	b.sync()
	if _, ok := b.get(id); !ok {
		t.Fatal("the second device should hold it")
	}
	// The first device's clock is right now: the note's stamp is ahead of it.
	a.st.SetClock(func() time.Time { return base.Add(2 * time.Second) })
	svc, appr := a.serviceFor(true)
	out, err := svc.Forget(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	if out.Status != memory.OutcomeApprovalPending || appr.n != 1 {
		t.Fatalf("no approval asked: %+v", out)
	}
	// Until it is approved, nothing has changed anywhere.
	if _, ok := a.get(id); !ok {
		t.Error("the note left before the approval")
	}
	if !fleetHolds(t, f, a, id) {
		t.Error("Fleet dropped it before the approval")
	}
	if _, err := appr.exec[appr.last.Operation](ctx, appr.last.Payload, appr.last.IdempotencyKey); err != nil {
		t.Fatal(err)
	}
	if _, ok := a.get(id); ok {
		t.Error("the note is still on this device after the approval")
	}
	if !fleetHolds(t, f, a, id) {
		t.Log("Fleet dropped it before this device synced")
	}
	a.sync()
	if fleetHolds(t, f, a, id) {
		t.Error("Fleet still holds it")
	}
	b.sync()
	if _, ok := b.get(id); ok {
		t.Error("the second device still holds it")
	}
}

// With no approval page to reach, the note leaves what the AI tool can read at once, and
// goes for good once approved.
func TestReturnedNoteHiddenWithoutAnApprovalPage(t *testing.T) {
	ctx := context.Background()
	f := fleetapitest.NewMemoryFleet()
	a, _ := autoDeviceOn(t, f)
	id, base := returnedNote(t, f, a)
	a.st.SetClock(func() time.Time { return base.Add(2 * time.Second) })
	svc, _ := a.serviceFor(false)
	out, err := svc.Forget(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	if !out.Hidden {
		t.Fatalf("%+v", out)
	}
	if _, ok := a.get(id); ok {
		t.Error("still readable on this device")
	}
	if !fleetHolds(t, f, a, id) {
		t.Error("Fleet dropped it without an approval")
	}
}

// A second device with the right clock, holding the note with a stamp ahead of it,
// forgets it the same way.
func TestReturnedNoteForgottenFromADeviceWithTheRightClock(t *testing.T) {
	ctx := context.Background()
	f := fleetapitest.NewMemoryFleet()
	a, _ := autoDeviceOn(t, f)
	id, _ := returnedNote(t, f, a)
	b, _ := autoDeviceOn(t, f)
	b.sync()
	svc, appr := b.serviceFor(true)
	out, err := svc.Forget(ctx, id)
	if err != nil || out.Status != memory.OutcomeApprovalPending {
		t.Fatalf("%+v %v", out, err)
	}
	if _, err := appr.exec[appr.last.Operation](ctx, appr.last.Payload, appr.last.IdempotencyKey); err != nil {
		t.Fatal(err)
	}
	b.sync()
	if fleetHolds(t, f, b, id) {
		t.Error("Fleet still holds it")
	}
	a.st.SetClock(time.Now)
	a.w = New(Options{Store: a.st, Memory: f.SessionClient(a.sessionFn), Session: a.sessionFn, ConsentVersion: consent, Holder: a.holder, Log: slog.New(slog.DiscardHandler), PullLimit: 50})
	a.sync()
	if _, ok := a.get(id); ok {
		t.Error("the first device still holds it")
	}
}

// queuedAuto has notes queued under the grant and not yet sent.
func queuedAuto(t *testing.T, n int) *device {
	t.Helper()
	d, _ := autoDevice(t, true)
	d.saveAuto(1)
	d.syncN(3) // Fleet has taken one
	if st := d.status(); st.Notes[memory.StateSynced] != 1 {
		t.Fatalf("%+v", st)
	}
	d.saveAuto(n)
	ctx := context.Background()
	l, err := d.st.AcquireLease(ctx, d.holder, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if q, err := d.st.QueueAutoBacklog(ctx, l); err != nil || q != n {
		t.Fatalf("queued %d %v", q, err)
	}
	return d
}

// What the grant queued and hasn't sent stops when the grant does.
func TestRevokingTheGrantStopsWhatItQueued(t *testing.T) {
	d := queuedAuto(t, 5)
	if err := d.st.RevokeSaveGrant(context.Background()); err != nil {
		t.Fatal(err)
	}
	st := d.status()
	if st.Queue["ready"] != 0 || st.Notes[memory.StateLocal] != 5 || st.AutoWaiting != 5 {
		t.Errorf("%+v", st)
	}
	d.syncN(2)
	if st := d.status(); st.Notes[memory.StateSynced] != 1 {
		t.Errorf("sent after the grant ended: %+v", st.Notes)
	}
	// A new grant picks them up.
	if err := d.st.GrantSaves(context.Background(), ""); err != nil {
		t.Fatal(err)
	}
	d.syncN(2)
	if st := d.status(); st.Notes[memory.StateSynced] != 6 {
		t.Errorf("after a new grant: %+v", st.Notes)
	}
}

func TestSyncOffAndOnSendsNothingUnlisted(t *testing.T) {
	d := queuedAuto(t, 5)
	if err := d.st.DisableSync(context.Background(), "disabled_by_user"); err != nil {
		t.Fatal(err)
	}
	d.sync()
	d.enable() // the approval page listed none of them
	d.syncN(2)
	st := d.status()
	if st.Notes[memory.StateSynced] != 1 || st.AutoWaiting != 5 {
		t.Errorf("%+v", st)
	}
}

// A new notice ends the grant too.
func TestEditionChangeStopsWhatTheGrantQueued(t *testing.T) {
	d := queuedAuto(t, 3)
	if _, err := d.st.EnableSync(context.Background(), "memory-sync/v9", memory.MutateOptions{SyncApproved: true}); err != nil {
		t.Fatal(err)
	}
	if st := d.status(); st.Queue["ready"] != 0 || st.AutoWaiting != 3 {
		t.Errorf("%+v", st)
	}
}

// An edit queued with the note it replaces goes back to waiting as a job with no create.
func TestRevokeUnlinksAnEditsJob(t *testing.T) {
	d, f := autoDevice(t, true)
	_ = f
	ctx := context.Background()
	d.saveAuto(1)
	d.syncN(3)
	es, err := d.st.Recent(ctx, memory.AllProjects, 1)
	if err != nil || len(es) != 1 {
		t.Fatalf("%+v %v", es, err)
	}
	body := "corrected text"
	up, err := d.st.UpdateNote(ctx, es[0].ID, memory.Update{Body: &body}, memory.MutateOptions{SyncApproved: true})
	if err != nil || up.Replaced == "" {
		t.Fatalf("%+v %v", up, err)
	}
	if err := d.st.RevokeSaveGrant(ctx); err != nil {
		t.Fatal(err)
	}
	if n, _ := d.get(up.ID); n.SyncState != memory.StateLocal {
		t.Errorf("%+v", n)
	}
	d.syncN(2)
	if !fleetHolds(t, f, d, up.Replaced) || fleetHolds(t, f, d, up.ID) {
		t.Error("the edit went on, or the old copy was forgotten, after the grant ended")
	}
	// Queued again, the job finishes as it should.
	if err := d.st.GrantSaves(ctx, ""); err != nil {
		t.Fatal(err)
	}
	d.syncN(3)
	if !fleetHolds(t, f, d, up.ID) || fleetHolds(t, f, d, up.Replaced) {
		t.Error("the edit didn't finish once queued again")
	}
}

// Work after this device's own forget-all counts, even when Fleet is reached later.
func TestOwnForgetAllKeepsItsTime(t *testing.T) {
	d, _ := autoDevice(t, true)
	ctx := context.Background()
	t0 := time.Now()
	d.at(t0)
	d.saveAuto(2)
	d.syncN(4)
	if _, err := d.st.ForgetAll(ctx, memory.MutateOptions{SyncApproved: true}); err != nil {
		t.Fatal(err)
	}
	first := d.st.ErasedAt(ctx)
	d.at(t0.Add(time.Hour))
	d.w = New(d.w.o)
	d.syncN(4)
	if later := d.st.ErasedAt(ctx); !later.Equal(first) {
		t.Errorf("moved from %v to %v", first, later)
	}
	if _, _, err := d.st.SaveAuto(ctx, memory.AutoNote{ProjectKey: memory.GlobalProject, Title: "new", Body: "new work", Source: memory.SourceCLI, Kind: memory.KindObservation, EventsFrom: t0.Add(30 * time.Minute)}); err != nil {
		t.Error(err)
	}
}

// A clock that was ahead at the forget-all doesn't stop capture once it's right.
func TestFutureEraseTimeDoesNotStopCapture(t *testing.T) {
	d, _ := autoDevice(t, false)
	ctx := context.Background()
	t0 := time.Now()
	d.at(t0.Add(365 * 24 * time.Hour))
	if _, err := d.st.ForgetAll(ctx, memory.MutateOptions{SyncApproved: true}); err != nil {
		t.Fatal(err)
	}
	d.at(t0)
	if d.st.ErasedClockWasAhead(ctx) {
		t.Error("said so too soon")
	}
	if got := d.st.ErasedAt(ctx); !got.Equal(time.UnixMilli(t0.UnixMilli())) {
		t.Errorf("erased at %v", got)
	}
	if !d.st.ErasedClockWasAhead(ctx) {
		t.Error("status can't say so")
	}
	note := memory.AutoNote{ProjectKey: memory.GlobalProject, Title: "new", Body: "new work", Source: memory.SourceCLI, Kind: memory.KindObservation}
	note.EventsFrom = t0.Add(-time.Minute)
	if _, _, err := d.st.SaveAuto(ctx, note); err == nil {
		t.Error("activity from before was saved")
	}
	d.at(t0.Add(time.Minute))
	note.EventsFrom = t0.Add(time.Second)
	if _, _, err := d.st.SaveAuto(ctx, note); err != nil {
		t.Error(err)
	}
}

// An edit queued under a stand-in key still goes after the stand-in is replaced.
func TestEditUnderAStandInKeyGoesAfterItsProjectMaps(t *testing.T) {
	projectRelease(t)
	f := fleetapitest.NewMemoryFleet()
	f.ProjectScope = true
	a := projectDevice(t, f)
	ctx := context.Background()
	id := a.saveProject(repoKey, "from a", "text from a")
	a.syncN(2)
	b := projectDevice(t, f)
	b.syncN(1)
	if n, _ := b.get(id); n.ProjectKey == repoKey {
		t.Fatalf("mapped already: %+v", n)
	}
	// Edited while the note has only a stand-in, and not sent yet.
	body := "edited text"
	up, err := b.st.UpdateNote(ctx, id, memory.Update{Body: &body}, memory.MutateOptions{SyncApproved: true})
	if err != nil {
		t.Fatal(err)
	}
	// The real key maps now, which replaces the stand-in.
	if err := b.st.SetProject(ctx, repoKey, f.ProjectID(memory.ProjectRequestID(repoKey)), "active", ""); err != nil {
		t.Fatal(err)
	}
	b.syncN(2)
	if n, _ := b.get(up.ID); n.SyncState != memory.StateSynced || n.ProjectKey != repoKey {
		t.Errorf("the edit is stranded: %+v %+v", n, b.status())
	}
}

// Notes an older relay had refused for good go again once project notes are confirmed.
func TestStrandedProjectNotesGoAgain(t *testing.T) {
	projectRelease(t)
	f := fleetapitest.NewMemoryFleet()
	d := projectDevice(t, f)
	ctx := context.Background()
	id := d.saveProject(repoKey, "stranded", "stranded text")
	made, err := f.ProjectsClient().Create(ctx, fleetapi.ProjectCreateRequest{Name: "x", Audience: "private", ClientRequestID: memory.ProjectRequestID(repoKey)})
	if err != nil {
		t.Fatal(err)
	}
	if err := d.st.SetProject(ctx, repoKey, made.ID, made.State, ""); err != nil {
		t.Fatal(err)
	}
	l, err := d.st.AcquireLease(ctx, d.holder, leaseTTL)
	if err != nil {
		t.Fatal(err)
	}
	b, err := d.st.LeasePushBatch(ctx, l)
	if err != nil || len(b.Ops) != 1 {
		t.Fatalf("%+v %v", b, err)
	}
	no := false
	// The older relay's refusal: it sent the note with no project id.
	if _, err := d.st.ApplyPushResults(ctx, l, b.Ops, []memory.PushResult{{ID: id, Status: "rejected", Code: "invalid_field", Field: "scope_id", Retry: &no}}); err != nil {
		t.Fatal(err)
	}
	if n, _ := d.get(id); n.SyncState != memory.StateRejected {
		t.Fatalf("%+v", n)
	}
	if err := d.st.ReleaseLease(ctx, l); err != nil {
		t.Fatal(err)
	}
	f.ProjectScope = true
	d.useProjects(t, f.SessionClient(d.sessionFn))
	d.syncN(2)
	if n, _ := d.get(id); n.SyncState != memory.StateSynced {
		t.Errorf("not released: %+v %+v", n, d.status())
	}
}

// A person's approved title edit on a note Fleet holds survives the grant ending.
func TestRevokeLeavesWhatFleetHoldsAlone(t *testing.T) {
	d, f := autoDevice(t, true)
	ctx := context.Background()
	d.saveAuto(1)
	d.syncN(3)
	es, err := d.st.Recent(ctx, memory.AllProjects, 5)
	if err != nil || len(es) != 1 {
		t.Fatalf("%+v %v", es, err)
	}
	id := es[0].ID
	title := "edited by a person"
	if _, err := d.st.UpdateNote(ctx, id, memory.Update{Title: &title}, memory.MutateOptions{SyncApproved: true}); err != nil {
		t.Fatal(err)
	}
	if err := d.st.RevokeSaveGrant(ctx); err != nil {
		t.Fatal(err)
	}
	st := d.status()
	if st.Notes[memory.StateLocal] != 0 || st.Queue["ready"] != 1 {
		t.Errorf("%+v", st)
	}
	d.sync()
	got := ""
	if _, err := f.SessionClient(d.sessionFn).Export(fleetapi.WithExpectedSession(ctx, fleetapi.ExpectedSession{Env: fleetapi.Env(alice.Env), OrgID: alice.OrgID, Subject: alice.UserID}), func(r fleetapi.MemoryRecord) error {
		if r.ID == id {
			got = r.Title
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if got != title {
		t.Errorf("the approved edit was lost: %q", got)
	}
}

// The note tried and definitely refused goes back too. One whose outcome is unknown
// doesn't: Fleet may hold it.
func TestRevokeWithAProbeRefusedOrUnknown(t *testing.T) {
	d, f := autoDevice(t, true)
	f.RefuseCaptureKinds = true
	d.saveAuto(3)
	base := time.Now()
	d.at(base)
	d.sync()
	ctx := context.Background()
	if err := d.st.RevokeSaveGrant(ctx); err != nil {
		t.Fatal(err)
	}
	f.RefuseCaptureKinds = false
	d.at(base.Add(25 * time.Hour))
	d.syncN(2)
	if st := d.status(); st.Notes[memory.StateSynced] != 0 || st.Notes[memory.StateLocal] != 3 {
		t.Errorf("refused: %+v", st)
	}

	// Network failure: the outcome is unknown.
	u, g := autoDevice(t, true)
	u.saveAuto(1)
	g.Fault = func(call string, after bool) error {
		if call == "push" && !after {
			return errors.New("network")
		}
		return nil
	}
	_ = u.w.RunOnce(ctx)
	if err := u.st.RevokeSaveGrant(ctx); err != nil {
		t.Fatal(err)
	}
	st := u.status()
	if st.Queue["ready"] != 1 || st.Notes[memory.StateLocal] != 0 {
		t.Errorf("unknown: %+v", st)
	}
	es, err := u.st.Recent(ctx, memory.AllProjects, 1)
	if err != nil || len(es) != 1 {
		t.Fatalf("%+v %v", es, err)
	}
	if may, err := u.st.FleetMayHold(ctx, es[0].ID); err != nil || !may {
		t.Errorf("may hold %v %v", may, err)
	}
}

// A person's edit made while Fleet refuses the note survives the grant ending: the create
// stays for it, and the note syncs when Fleet takes it.
func TestTitleEditDuringAHoldSurvivesRevoke(t *testing.T) {
	for name, approved := range map[string]bool{"approved": true, "unapproved": false} {
		t.Run(name, func(t *testing.T) {
			d, f := autoDevice(t, true)
			ctx := context.Background()
			f.RefuseCaptureKinds = true
			d.saveAuto(1)
			base := time.Now()
			d.at(base)
			d.sync()
			es, err := d.st.Recent(ctx, memory.AllProjects, 5)
			if err != nil || len(es) != 1 {
				t.Fatalf("%+v %v", es, err)
			}
			id := es[0].ID
			title := "a person's title"
			if _, err := d.st.UpdateNote(ctx, id, memory.Update{Title: &title}, memory.MutateOptions{SyncApproved: approved}); err != nil {
				t.Fatal(err)
			}
			unknown, err := d.st.EndSaveGrant(ctx)
			if err != nil || unknown != 0 {
				t.Errorf("told of %d, %v: Fleet refused it", unknown, err)
			}
			if n, _ := d.get(id); n.SyncState == memory.StateLocal {
				t.Errorf("reads as device-only: %+v", n)
			}
			f.RefuseCaptureKinds = false
			d.at(base.Add(25 * time.Hour))
			d.syncN(3)
			st := d.status()
			if st.Notes[memory.StateRejected] != 0 || len(st.BlockedCodes) != 0 {
				t.Errorf("refused for good: %+v", st)
			}
			if !approved {
				held, err := d.st.HeldOps(ctx)
				if err != nil || len(held) != 1 {
					t.Fatalf("the edit should ask: %+v %v", held, err)
				}
				if _, err := d.st.ApproveOps(ctx, held, ""); err != nil {
					t.Fatal(err)
				}
				d.syncN(2)
			}
			if n, _ := d.get(id); n.SyncState != memory.StateSynced {
				t.Errorf("%+v %+v", n, d.status())
			}
			got := ""
			if _, err := f.SessionClient(d.sessionFn).Export(fleetapi.WithExpectedSession(ctx, fleetapi.ExpectedSession{Env: fleetapi.Env(alice.Env), OrgID: alice.OrgID, Subject: alice.UserID}), func(r fleetapi.MemoryRecord) error {
				if r.ID == id {
					got = r.Title
				}
				return nil
			}); err != nil {
				t.Fatal(err)
			}
			if got != title {
				t.Errorf("the edit was lost: %q", got)
			}
		})
	}
}

// A new notice: once the member has confirmed it, a note whose outcome was unknown goes.
func TestUnknownOutcomeFinishesAfterANewNotice(t *testing.T) {
	d, f := autoDevice(t, true)
	ctx := context.Background()
	d.saveAuto(1)
	f.Fault = func(call string, after bool) error {
		if call == "push" && !after {
			return errors.New("network")
		}
		return nil
	}
	_ = d.w.RunOnce(ctx)
	f.Fault = nil
	const edition = "memory-sync/v9"
	// Until the member confirms, the worker sends nothing new.
	d.w = New(Options{Store: d.st, Memory: f.SessionClient(d.sessionFn), Session: d.sessionFn, ConsentVersion: edition, Holder: d.holder, Log: slog.New(slog.DiscardHandler)})
	d.st.SetClock(func() time.Time { return time.Now().Add(time.Hour) })
	d.sync()
	if st := d.status(); st.Notes[memory.StateSynced] != 0 {
		t.Fatalf("sent before confirming: %+v", st)
	}
	r := Remote{Memory: f.SessionClient(d.sessionFn), Session: d.sessionFn}
	if err := r.EnableSync(ctx, d.p, edition); err != nil {
		t.Fatal(err)
	}
	res, err := d.st.EnableSync(ctx, edition, memory.MutateOptions{SyncApproved: true})
	if err != nil || res.StillSending != 1 {
		t.Fatalf("%+v %v", res, err)
	}
	d.sync()
	if st := d.status(); st.Notes[memory.StateSynced] != 1 {
		t.Errorf("it didn't finish: %+v", st)
	}
}

// Project notes are sent only while Fleet's capabilities carry memory_project_scope.
// Absent, a failed read, or no capabilities at all hold them on this device, even when
// Fleet would take them; the key going away holds them again; deletions still go.
func TestProjectNotesNeedTheCapability(t *testing.T) {
	projectRelease(t)
	f := fleetapitest.NewMemoryFleet()
	f.ProjectScope = true // Fleet would take them: only the capability stops them
	d := projectDevice(t, f)
	offered, fail := false, false
	caps := func(context.Context) (fleetapi.Capabilities, error) {
		if fail {
			return fleetapi.Capabilities{}, errors.New("offline")
		}
		keys := map[string]bool{fleetapi.CapMemorySync: true}
		if offered {
			keys[fleetapi.CapMemoryProjectScope] = true
		}
		return fleetapi.Capabilities{Keys: keys}, nil
	}
	newW := func(c func(context.Context) (fleetapi.Capabilities, error)) {
		d.w = New(Options{Store: d.st, Memory: f.SessionClient(d.sessionFn), Projects: f.ProjectsClient(), Session: d.sessionFn, ConsentVersion: consentV4,
			Holder: d.holder, Log: slog.New(slog.DiscardHandler), PullLimit: 50, Capabilities: c})
	}
	newW(caps)
	id := d.saveProject(repoKey, "a project note", "project text")
	global := d.save("a global note", "global text", true)
	check := func(name string) {
		t.Helper()
		d.syncN(3)
		if n, _ := d.get(id); n.SyncState == memory.StateSynced {
			t.Fatalf("%s: a project note was sent", name)
		}
		if len(f.LiveContents(alice.OrgID, alice.UserID)) != 1 {
			t.Fatalf("%s: Fleet holds %d notes", name, len(f.LiveContents(alice.OrgID, alice.UserID)))
		}
		if got := f.ProjectCalls(memory.ProjectRequestID(repoKey)); got != 0 {
			t.Fatalf("%s: asked for a project id %d times", name, got)
		}
		if st := d.status(); st.ProjectHeld != memory.CodeProjectScopeNotOffered {
			t.Fatalf("%s: %+v", name, st)
		}
	}
	check("absent")
	if n, ok := d.get(global); !ok || n.SyncState != memory.StateSynced {
		t.Fatalf("a global note was held up: %+v", n)
	}
	fail = true
	check("read fails")
	newW(nil)
	check("no capabilities")

	// The key comes: the note goes at the next cycle.
	newW(caps)
	offered, fail = true, false
	d.syncN(2)
	if n, _ := d.get(id); n.SyncState != memory.StateSynced {
		t.Fatalf("offered: %+v %+v", n, d.status())
	}
	if d.status().ProjectHeld != "" {
		t.Errorf("%+v", d.status())
	}

	// It goes away: a new project note and an edit of the synced one stay here; a
	// deletion still goes.
	offered = false
	id2 := d.saveProject(repoKey, "another", "more text")
	if _, err := d.st.UpdateNote(context.Background(), id, memory.Update{Title: ptr("renamed")}, memory.MutateOptions{SyncApproved: true}); err != nil {
		t.Fatal(err)
	}
	d.syncN(3)
	if n, _ := d.get(id2); n.SyncState == memory.StateSynced {
		t.Fatalf("sent after the key went: %+v", n)
	}
	if st := d.status(); st.ProjectHeld != memory.CodeProjectScopeNotOffered || st.Queue["ready"] != 2 {
		t.Errorf("the new note and the title edit should both wait: %+v", st)
	}
	// A deletion of the synced project note still goes.
	if _, err := d.st.ForgetNote(context.Background(), id, memory.MutateOptions{SyncApproved: true}); err != nil {
		t.Fatal(err)
	}
	d.syncN(2)
	if _, ok := f.LiveContents(alice.OrgID, alice.UserID)[id]; ok {
		t.Error("a deletion was held with the project notes")
	}
}
