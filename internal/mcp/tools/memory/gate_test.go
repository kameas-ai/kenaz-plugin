package memorytools

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"testing"

	sdk "github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/kameas-ai/kenaz-plugin/internal/mcp"
	"github.com/kameas-ai/kenaz-plugin/internal/memory"
)

// relay stands in for the daemon's approval service: requests wait until
// approve runs the daemon-side executor with the stored payload and key.
type relay struct {
	mu   sync.Mutex
	exec map[string]memory.Executor
	reqs map[string]memory.ApprovalRequest
	res  map[string][]byte
	n    int
}

func newRelay() *relay {
	return &relay{exec: map[string]memory.Executor{}, reqs: map[string]memory.ApprovalRequest{}, res: map[string][]byte{}}
}

func (r *relay) Register(op string, ex memory.Executor)       { r.exec[op] = ex }
func (r *relay) RegisterReconciler(string, memory.Reconciler) {}

func (r *relay) Submit(_ context.Context, q memory.ApprovalRequest) (memory.Pending, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	for id, ex := range r.reqs { // same key and payload: the same request
		if ex.IdempotencyKey == q.IdempotencyKey && string(ex.Payload) == string(q.Payload) {
			return memory.Pending{RequestID: id, Status: memory.ApprovalPending, ApproveURL: "http://localhost:4318/approvals/r/" + id}, nil
		}
	}
	r.n++
	id := fmt.Sprint("apr_", r.n)
	r.reqs[id] = q
	return memory.Pending{RequestID: id, Status: memory.ApprovalPending, ApproveURL: "http://localhost:4318/approvals/r/" + id}, nil
}

func (r *relay) Status(_ context.Context, id string) (memory.ApprovalStatus, []byte, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if b, ok := r.res[id]; ok {
		return memory.ApprovalDone, b, nil
	}
	return memory.ApprovalPending, nil, nil
}

func (r *relay) approve(t *testing.T, id string) {
	t.Helper()
	q := r.reqs[id]
	b, err := r.exec[q.Operation](context.Background(), q.Payload, q.IdempotencyKey)
	if err != nil {
		t.Fatal(err)
	}
	r.mu.Lock()
	r.res[id] = b
	r.mu.Unlock()
}

func gatedSession(t *testing.T, root string, active memory.Profile, r *relay) *sdk.ClientSession {
	t.Helper()
	ctx := context.Background()
	activeFn := func(context.Context) memory.Profile { return active }
	o := Options{Path: memory.PathIn(root), Workdir: func() string { return root }, Active: activeFn, ConsentVersion: "memory-sync/v1"}
	if r != nil {
		o.Submitter = r
		memory.RegisterExecutors(r, memory.ExecOptions{StateRoot: root, Active: activeFn, ConsentVersion: "memory-sync/v1"})
	}
	srv := mcp.NewServer("test", mcp.Deps{Provider: panicProvider{}}, NewGroup(o))
	ct, st := sdk.NewInMemoryTransports()
	if _, err := srv.Connect(ctx, st, nil); err != nil {
		t.Fatal(err)
	}
	cs, err := sdk.NewClient(&sdk.Implementation{Name: "claude-code", Version: "1"}, nil).Connect(ctx, ct, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { cs.Close() })
	return cs
}

// Signed out (the device notebook): update, export and single forget
// work directly, as before sync existed; forget-all points at purge and
// sync cannot be turned on.
func TestToolsDeviceNotebook(t *testing.T) {
	root := t.TempDir()
	r := newRelay()
	cs := gatedSession(t, root, memory.Local, r)
	var s SaveOutput
	call(t, cs, ToolSave, map[string]any{"title": "Retry", "body": "backoff 30 s", "project": "global"}, &s)
	if s.Status != "done" || s.SyncState != memory.StateLocal || s.Where != "device" || !strings.Contains(s.Message, "this device only") {
		t.Fatalf("save = %+v", s)
	}
	var u UpdateOutput
	if isErr, _ := call(t, cs, ToolUpdate, map[string]any{"id": s.ID, "body": "backoff 60 s"}, &u); isErr || u.ID != s.ID {
		t.Fatalf("update = %+v", u)
	}
	var ex ExportOutput
	call(t, cs, ToolExport, map[string]any{}, &ex)
	if ex.Total != 1 || ex.Notes[0].Body != "backoff 60 s" || ex.Notes[0].Profile != "device" {
		t.Fatalf("export = %+v", ex)
	}
	var fa ForgetAllOutput
	if isErr, _ := call(t, cs, ToolForgetAll, map[string]any{}, &fa); !isErr || fa.Error.Code != "approval_unavailable" || !strings.Contains(fa.Error.Message, "kenaz memory purge") {
		t.Fatalf("forget_all = %+v", fa)
	}
	var ss SyncSettingsOutput
	if isErr, _ := call(t, cs, ToolSyncSettings, map[string]any{"enabled": true}, &ss); !isErr || ss.Error.Code != "not_signed_in" {
		t.Fatalf("sync enable signed out = %+v", ss)
	}
	var st SyncStatusOutput
	call(t, cs, ToolSyncStatus, map[string]any{}, &st)
	if st.Profile != "device" || st.Available || !strings.Contains(st.Message, "never syncs") {
		t.Fatalf("status = %+v", st)
	}
	var f ForgetOutput
	if isErr, _ := call(t, cs, ToolForget, map[string]any{"id": s.ID}, &f); isErr || !f.Deleted || f.Remote != "none" {
		t.Fatalf("forget = %+v", f)
	}
	if r.n != 0 {
		t.Fatal("device-notebook edits were submitted for approval")
	}
}

// Signed in with sync off: a save is the user's own device data and
// runs directly (nothing is submitted). With sync on, every write that
// reaches Fleet waits for approval on the approval page and nothing
// happens before; the approved executor applies it.
func TestToolsAccountNeedsApproval(t *testing.T) {
	root := t.TempDir()
	acct := memory.Profile{Env: "dev", OrgID: "o", UserID: "u"}
	r := newRelay()
	cs := gatedSession(t, root, acct, r)
	var local SaveOutput
	call(t, cs, ToolSave, map[string]any{"title": "local", "body": "sync is off"}, &local)
	if local.Status != "done" || local.SyncState != memory.StateLocal || r.n != 0 {
		t.Fatalf("save with sync off = %+v (submitted %d)", local, r.n)
	}
	// Sync on (as an approved sync-enable leaves it).
	st0, err := memory.OpenProfile(memory.ProfilePath(root, acct), acct)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st0.EnableSync(context.Background(), "memory-sync/v1", memory.MutateOptions{SyncApproved: true}); err != nil {
		t.Fatal(err)
	}
	st0.Close()
	var s SaveOutput
	call(t, cs, ToolSave, map[string]any{"title": "t", "body": "b", "project": "global"}, &s)
	if s.Status != "approval_pending" || s.ID != "" || s.RequestID == "" || !strings.Contains(s.ApproveURL, "/approvals/r/") || s.Error != nil {
		t.Fatalf("pending save = %+v", s)
	}
	// An agent retrying the same save gets the same request back.
	var again SaveOutput
	call(t, cs, ToolSave, map[string]any{"title": "t", "body": "b", "project": "global"}, &again)
	if again.RequestID != s.RequestID {
		t.Fatalf("retry made a second request: %s vs %s", again.RequestID, s.RequestID)
	}
	var rec RecentOutput
	call(t, cs, ToolRecent, map[string]any{"project": "all"}, &rec)
	if len(rec.Entries) != 1 {
		t.Fatal("saved before approval")
	}
	r.approve(t, s.RequestID)
	var st SyncStatusOutput
	call(t, cs, ToolSyncStatus, map[string]any{"request_id": s.RequestID}, &st)
	if st.Request == nil || st.Request.Status != "done" || st.Profile != "account" || st.Sync.Notes[memory.StatePending] != 1 {
		t.Fatalf("status = %+v", st)
	}
	var fa ForgetAllOutput
	call(t, cs, ToolForgetAll, map[string]any{}, &fa)
	if fa.Status != "approval_pending" || fa.Error != nil {
		t.Fatalf("forget_all = %+v", fa)
	}
	q := r.reqs[fa.RequestID]
	if q.Operation != memory.OpForgetAll || !strings.Contains(q.Impact, "Deletes 2 memories") || q.UserID != "u" {
		t.Fatalf("forget-all request = %+v", q)
	}
	r.approve(t, fa.RequestID)
	call(t, cs, ToolRecent, map[string]any{"project": "all"}, &rec)
	if len(rec.Entries) != 0 {
		t.Fatal("approved forget-all left notes")
	}
	var ss SyncSettingsOutput
	if isErr, _ := call(t, cs, ToolSyncSettings, map[string]any{"enabled": true}, &ss); !isErr || ss.Error.Code != "sync_unavailable" {
		t.Fatalf("enable without remote = %+v", ss)
	}
}

// The harness that saved a note is recorded from the MCP client name,
// including OpenCode.
func TestSourceDetection(t *testing.T) {
	none := func(string) string { return "" }
	for name, want := range map[string]string{
		"opencode": memory.SourceOpenCode, "OpenCode": memory.SourceOpenCode, "codex-mcp-client": memory.SourceCodex,
		"claude-code": memory.SourceClaudeCode, "": memory.SourceClaudeCode,
		"claude-ai": memory.SourceClaudeDesktop, "Claude-AI": memory.SourceClaudeDesktop, "claude-desktop": memory.SourceClaudeDesktop,
	} {
		if got := sourceFrom(name, none); got != want {
			t.Errorf("%q = %s, want %s", name, got, want)
		}
	}
	// The Claude Desktop extension sets KENAZ_MCP_HOST=claude-desktop: a
	// note saved there is labelled claude-desktop even when the client
	// reports no (or another Claude) name, but an explicit Claude Code,
	// Codex or OpenCode client keeps its own label.
	desktop := func(k string) string {
		if k == mcp.HostEnvVar {
			return mcp.HostClaudeDesktop
		}
		return ""
	}
	for name, want := range map[string]string{
		"": memory.SourceClaudeDesktop, "claude-ai": memory.SourceClaudeDesktop, "claude": memory.SourceClaudeDesktop,
		"claude-code": memory.SourceClaudeCode, "codex-mcp-client": memory.SourceCodex, "opencode": memory.SourceOpenCode,
	} {
		if got := sourceFrom(name, desktop); got != want {
			t.Errorf("KENAZ_MCP_HOST=claude-desktop, %q = %s, want %s", name, got, want)
		}
	}
	// A note labelled claude-desktop is a valid note.
	if err := (&memory.NewNote{ProjectKey: memory.GlobalProject, Title: "t", Body: "b", Source: memory.SourceClaudeDesktop}).Validate(); err != nil {
		t.Fatalf("claude-desktop source rejected: %v", err)
	}
	if got := sourceFrom("", func(k string) string {
		if k == "OPENCODE" {
			return "1"
		}
		return ""
	}); got != memory.SourceOpenCode {
		t.Fatalf("OPENCODE env = %s", got)
	}
	// End to end through an MCP client that names itself opencode.
	ctx := context.Background()
	root := t.TempDir()
	srv := mcp.NewServer("test", mcp.Deps{Provider: panicProvider{}}, NewGroup(Options{Path: memory.PathIn(root), Workdir: func() string { return root }}))
	ct, st := sdk.NewInMemoryTransports()
	if _, err := srv.Connect(ctx, st, nil); err != nil {
		t.Fatal(err)
	}
	cs, err := sdk.NewClient(&sdk.Implementation{Name: "opencode", Version: "1"}, nil).Connect(ctx, ct, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer cs.Close()
	var s SaveOutput
	call(t, cs, ToolSave, map[string]any{"title": "t", "body": "from opencode"}, &s)
	var got GetOutput
	call(t, cs, ToolGet, map[string]any{"ids": []string{s.ID}}, &got)
	if len(got.Notes) != 1 || got.Notes[0].Source != memory.SourceOpenCode {
		t.Fatalf("source = %+v", got)
	}
}
