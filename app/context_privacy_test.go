package app

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	sdk "github.com/modelcontextprotocol/go-sdk/mcp"
	commonv1 "go.opentelemetry.io/proto/otlp/common/v1"
	logsv1 "go.opentelemetry.io/proto/otlp/logs/v1"
	"google.golang.org/protobuf/proto"

	"github.com/kameas-ai/kenaz-plugin/fleetapi"
	"github.com/kameas-ai/kenaz-plugin/fleetapi/stub"
	"github.com/kameas-ai/kenaz-plugin/internal/canon"
	"github.com/kameas-ai/kenaz-plugin/internal/contextlib"
	"github.com/kameas-ai/kenaz-plugin/internal/fixtures"
	"github.com/kameas-ai/kenaz-plugin/internal/mcp"
	"github.com/kameas-ai/kenaz-plugin/internal/mcp/tools/localcontext"
	"github.com/kameas-ai/kenaz-plugin/internal/spool"
)

// Separate markers for a file's content and its path, since the promise covers
// both: file content and paths never enter telemetry.
const (
	contextContentMarker = "CTXCONTENTq7Zr2xKf00000000000000"
	contextPathMarker    = "CTXPATHq7Zr2xKf00000000000000000"
)

// contextCanary makes real files and reads them through the real tools,
// returning what the agent saw: the raw material a harness could quote.
func contextCanary(t *testing.T) (repoFile string, seen []string) {
	t.Helper()
	repo, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	personal := filepath.Join(t.TempDir(), "contexts")
	body := "---\ntitle: T " + contextContentMarker + "\ndescription: D " + contextContentMarker + "\n---\n" + contextContentMarker + "\n"
	repoFile = filepath.Join(repo, ".kenaz", "context", contextPathMarker+".md")
	for _, p := range []string{repoFile, filepath.Join(personal, contextPathMarker+".md")} {
		if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Mkdir(filepath.Join(repo, ".git"), 0o700); err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	g := localcontext.NewGroup(localcontext.Options{PersonalDir: personal, Workdir: func() string { return repo }})
	srv := mcp.NewServer("test", mcp.Deps{}, g)
	ct, st := sdk.NewInMemoryTransports()
	if _, err := srv.Connect(ctx, st, nil); err != nil {
		t.Fatal(err)
	}
	cs, err := sdk.NewClient(&sdk.Implementation{Name: "claude-code", Version: "1"}, nil).Connect(ctx, ct, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer cs.Close()
	for _, c := range []struct {
		tool string
		args map[string]any
	}{
		{localcontext.ToolWrite, map[string]any{"path": "personal/" + contextPathMarker + "-new.md", "content": body}},
		{localcontext.ToolList, nil},
		{localcontext.ToolRead, map[string]any{"path": "project/" + contextPathMarker + ".md"}},
		{localcontext.ToolRead, map[string]any{"path": "personal/" + contextPathMarker + ".md"}},
	} {
		res, err := cs.CallTool(ctx, &sdk.CallToolParams{Name: c.tool, Arguments: c.args})
		if err != nil || res.IsError {
			t.Fatalf("%s: %v %+v", c.tool, err, res)
		}
		raw, _ := json.Marshal(res.StructuredContent)
		if !bytes.Contains(raw, []byte(contextPathMarker)) {
			t.Fatalf("%s did not return the marker: %s", c.tool, raw)
		}
		seen = append(seen, string(raw))
	}
	seen = append(seen, contextlib.SessionContext(ctx, contextlib.Open(personal, repo), contextlib.Options{
		Index: true, IndexBytes: contextlib.DefaultIndexBytes, Always: true, AlwaysBytes: contextlib.DefaultAlwaysBytes}))
	return repoFile, seen
}

// contextLogs is a fixture session plus everything a harness could log about
// reading context files: the MCP call's arguments and result, a built-in Read
// of the file, a shell command naming it, the session's hook context and a
// prompt quoting it. Every free-text attribute it can carry holds a marker.
func contextLogs(base *logsv1.LogsData, repoFile string, seen []string) *logsv1.LogsData {
	m := proto.Clone(base).(*logsv1.LogsData)
	sl := m.ResourceLogs[0].ScopeLogs[0]
	str := func(k, v string) *commonv1.KeyValue {
		return &commonv1.KeyValue{Key: k, Value: &commonv1.AnyValue{Value: &commonv1.AnyValue_StringValue{StringValue: v}}}
	}
	var tool *logsv1.LogRecord
	for _, r := range sl.LogRecords {
		for _, kv := range r.Attributes {
			if kv.Key == "tool_name" || kv.Key == "tool.name" {
				tool = r
			}
		}
	}
	if tool == nil {
		tool = sl.LogRecords[len(sl.LogRecords)-1]
	}
	variants := []struct{ name, params, output string }{
		{"mcp__kenaz__context_read", `{"path":"project/` + contextPathMarker + `.md"}`, seen[1]},
		{"mcp__kenaz__context_list", `{}`, seen[0]},
		{"Read", `{"file_path":"` + repoFile + `"}`, contextContentMarker},
		{"Bash", `{"command":"cat ` + repoFile + `"}`, contextContentMarker},
		{"mcp__x__" + contextPathMarker, `{}`, contextContentMarker},
		{contextPathMarker, `{}`, contextContentMarker},
	}
	for _, v := range variants {
		r := proto.Clone(tool).(*logsv1.LogRecord)
		for i, kv := range r.Attributes {
			switch kv.Key {
			case "tool_name", "tool.name":
				r.Attributes[i] = str(kv.Key, v.name)
			case "tool_parameters", "arguments":
				r.Attributes[i] = str(kv.Key, v.params)
			}
		}
		r.Attributes = append(r.Attributes,
			str("tool_input", v.params), str("tool_output", v.output), str("file_path", repoFile),
			str("cwd", filepath.Dir(repoFile)), str("additionalContext", seen[len(seen)-1]),
			str("prompt", "see "+contextContentMarker+" in "+repoFile))
		r.Body = &commonv1.AnyValue{Value: &commonv1.AnyValue_StringValue{StringValue: v.output}}
		sl.LogRecords = append(sl.LogRecords, r)
	}
	return m
}

func hasContextMarker(b []byte) bool {
	return bytes.Contains(b, []byte(contextContentMarker)) || bytes.Contains(b, []byte(contextPathMarker)) ||
		bytes.Contains(b, []byte(contextContentMarker[:20])) || bytes.Contains(b, []byte(contextPathMarker[:20]))
}

// The telemetry promise holds for context files: neither content nor paths
// reach any batch sent to Kameas Fleet or any spooled payload, in either
// harness, even when the harness's own telemetry quotes them.
func TestContextFileCanaryNeverReachesFleet(t *testing.T) {
	repoFile, seen := contextCanary(t)
	for _, tc := range []struct {
		name    string
		logs    *logsv1.LogsData
		client  canon.ClientKind
		session string
	}{
		{"claude", fixtures.ClaudeLogs(), canon.ClaudeCode, fixtures.ClaudeSessionID},
		{"codex", fixtures.CodexLogs(false), canon.Codex, fixtures.CodexSessionID},
	} {
		logs := contextLogs(tc.logs, repoFile, seen)
		t.Run(tc.name+"/uploaded", func(t *testing.T) {
			f := &laneFleet{Provider: stub.New(), optedIn: true}
			l, rx, _ := newTestLane(t, f)
			if !waitFor(t, func() bool { return l.dec.SignedIn() && admitted(l) }) {
				t.Fatal("lane never admitted")
			}
			postLogs(t, rx, logs)
			l.pipe.SessionEndLocal(tc.client, canon.LocalSessionHash(tc.session))
			if !waitFor(t, func() bool { return f.postedCount() > 0 }) {
				t.Fatalf("nothing posted; lane status %+v", l.Status())
			}
			for _, b := range f.batches() {
				if hasContextMarker(b.Body) {
					t.Fatalf("context file marker in a %s batch sent to Fleet", b.Signal)
				}
			}
		})
		t.Run(tc.name+"/spooled", func(t *testing.T) {
			f := &laneFleet{Provider: stub.New(), optedIn: true, postErr: &fleetapi.APIError{Status: 503}}
			l, rx, stop := newTestLane(t, f)
			if !waitFor(t, func() bool { return l.dec.SignedIn() && admitted(l) }) {
				t.Fatal("lane never admitted")
			}
			postLogs(t, rx, logs)
			l.pipe.SessionEndLocal(tc.client, canon.LocalSessionHash(tc.session))
			if !waitFor(t, func() bool { return f.attemptCount() > 0 && !l.sp.Empty() }) {
				t.Fatal("nothing spooled")
			}
			stop()
			n, owned := 0, 0
			if _, err := l.sp.Drain(func(r spool.Record) error {
				n++
				if hasContextMarker(r.Payload) {
					t.Fatalf("context file marker in a spooled %s payload", r.Lane)
				}
				// An owned batch holds the body base64-encoded in JSON, so
				// the raw check above cannot see inside it.
				if len(r.Payload) > 9 && r.Payload[0] == tagOwnedBatch {
					var ob ownedBatch
					if err := json.Unmarshal(r.Payload[9:], &ob); err != nil {
						t.Fatalf("spooled batch: %v", err)
					}
					if len(ob.Batch.Body) == 0 {
						t.Fatal("a spooled batch with no body checks nothing")
					}
					owned++
					if hasContextMarker(ob.Batch.Body) {
						t.Fatalf("context file marker in a spooled %s batch body", ob.Batch.Signal)
					}
				}
				return nil
			}); err != nil {
				t.Fatal(err)
			}
			if n == 0 {
				t.Fatal("no spooled payloads were checked")
			}
			if owned == 0 {
				t.Fatal("no spooled batch bodies were checked")
			}
		})
	}
}

// The test proves itself: the markers really are in what was posted to the
// relay, so a pass above means the relay dropped them.
func TestContextFileCanaryIsPlanted(t *testing.T) {
	repoFile, seen := contextCanary(t)
	raw, err := proto.Marshal(contextLogs(fixtures.ClaudeLogs(), repoFile, seen))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(raw, []byte(contextContentMarker)) || !bytes.Contains(raw, []byte(contextPathMarker)) {
		t.Fatal("markers are not in the logs")
	}
}
