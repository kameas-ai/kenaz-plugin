package app

import (
	"bytes"
	"context"
	"encoding/json"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	sdk "github.com/modelcontextprotocol/go-sdk/mcp"
	commonv1 "go.opentelemetry.io/proto/otlp/common/v1"
	logsv1 "go.opentelemetry.io/proto/otlp/logs/v1"
	"google.golang.org/protobuf/proto"

	"github.com/kameas-ai/kenaz-plugin/fleetapi"
	"github.com/kameas-ai/kenaz-plugin/fleetapi/stub"
	"github.com/kameas-ai/kenaz-plugin/internal/canon"
	"github.com/kameas-ai/kenaz-plugin/internal/fixtures"
	"github.com/kameas-ai/kenaz-plugin/internal/mcp"
	memorytools "github.com/kameas-ai/kenaz-plugin/internal/mcp/tools/memory"
	"github.com/kameas-ai/kenaz-plugin/internal/memory"
	"github.com/kameas-ai/kenaz-plugin/internal/memory/capture"
	"github.com/kameas-ai/kenaz-plugin/internal/spool"
)

const memoryMarker = "MEMMARKERq7Zr2xKf0000000000000099"

// saveMarker saves a note carrying memoryMarker through the memory_save MCP
// tool, the way an agent would.
func saveMarker(t *testing.T) {
	t.Helper()
	ctx := context.Background()
	g := memorytools.NewGroup(memorytools.Options{Path: filepath.Join(t.TempDir(), "memory", "memory.db"), Workdir: func() string { return t.TempDir() }})
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
	res, err := cs.CallTool(ctx, &sdk.CallToolParams{Name: memorytools.ToolSave, Arguments: map[string]any{
		"title": "note " + memoryMarker, "body": "remember " + memoryMarker, "tags": []string{memoryMarker[:20]},
	}})
	if err != nil || res.IsError {
		t.Fatalf("memory_save: %v %+v", err, res)
	}
}

// markerLogs is the Claude fixture session plus what the harness would log
// about the memory_save call: the MCP tool name, its arguments and its
// result, and a prompt quoting the note. Every one carries the marker.
func markerLogs() *logsv1.LogsData {
	m := fixtures.ClaudeLogs()
	recs := m.ResourceLogs[0].ScopeLogs[0].LogRecords
	var tool *logsv1.LogRecord
	for _, r := range recs {
		for _, kv := range r.Attributes {
			if kv.Key == "tool_name" && kv.Value.GetStringValue() != "Bash" {
				tool = proto.Clone(r).(*logsv1.LogRecord)
			}
			if kv.Key == "prompt" {
				kv.Value = &commonv1.AnyValue{Value: &commonv1.AnyValue_StringValue{StringValue: "save " + memoryMarker}}
			}
		}
	}
	args, _ := json.Marshal(map[string]string{"title": "note " + memoryMarker, "body": "remember " + memoryMarker})
	str := func(k, v string) *commonv1.KeyValue {
		return &commonv1.KeyValue{Key: k, Value: &commonv1.AnyValue{Value: &commonv1.AnyValue_StringValue{StringValue: v}}}
	}
	for i, kv := range tool.Attributes {
		switch kv.Key {
		case "tool_name":
			tool.Attributes[i] = str("tool_name", "mcp__kenaz__"+memorytools.ToolSave)
		case "tool_parameters":
			tool.Attributes[i] = str("tool_parameters", string(args))
		case "success":
			tool.Attributes[i] = str("success", "true")
		}
	}
	tool.Attributes = append(tool.Attributes, str("tool_input", string(args)), str("tool_output", `{"id":"`+memoryMarker+`"}`))
	tool.Body = &commonv1.AnyValue{Value: &commonv1.AnyValue_StringValue{StringValue: memoryMarker}}
	m.ResourceLogs[0].ScopeLogs[0].LogRecords = append(recs, tool)
	return m
}

// A note saved with memory_save never leaves the device: not in any
// projected batch Fleet receives, and not in any spooled payload, even
// when the harness's own telemetry quotes the memory_save call.
func TestMemoryMarkerNeverReachesFleet(t *testing.T) {
	saveMarker(t)

	// Each lane runs in its own subtest: the lane helpers swap
	// package-level timers that are restored at subtest cleanup.
	t.Run("uploaded", func(t *testing.T) { checkUploaded(t) })
	t.Run("spooled", func(t *testing.T) { checkSpooled(t) })
}

func checkUploaded(t *testing.T) {
	f := &laneFleet{Provider: stub.New(), optedIn: true}
	l, rx, _ := newTestLane(t, f)
	if !waitFor(t, func() bool { return l.dec.SignedIn() && admitted(l) }) {
		t.Fatal("lane never admitted")
	}
	postLogs(t, rx, markerLogs())
	l.pipe.SessionEndLocal(canon.ClaudeCode, canon.LocalSessionHash(fixtures.ClaudeSessionID))
	if !waitFor(t, func() bool { return f.postedCount() > 0 }) {
		t.Fatalf("nothing posted; lane status %+v", l.Status())
	}
	for _, b := range f.batches() {
		if bytes.Contains(b.Body, []byte(memoryMarker)) || bytes.Contains(b.Body, []byte(memoryMarker[:20])) {
			t.Fatalf("memory marker in a %s batch sent to Fleet", b.Signal)
		}
	}

}

// checkSpooled runs the session while Fleet is unavailable, so batches
// are spooled, and inspects every spooled payload.
func checkSpooled(t *testing.T) {
	f2 := &laneFleet{Provider: stub.New(), optedIn: true, postErr: &fleetapi.APIError{Status: 503}}
	l2, rx2, stop := newTestLane(t, f2)
	if !waitFor(t, func() bool { return l2.dec.SignedIn() && admitted(l2) }) {
		t.Fatal("lane never admitted")
	}
	postLogs(t, rx2, markerLogs())
	l2.pipe.SessionEndLocal(canon.ClaudeCode, canon.LocalSessionHash(fixtures.ClaudeSessionID))
	if !waitFor(t, func() bool { return f2.attemptCount() > 0 && !l2.sp.Empty() }) {
		t.Fatal("nothing spooled")
	}
	stop()
	n, bodies := 0, 0
	if _, err := l2.sp.Drain(func(r spool.Record) error {
		n++
		if bytes.Contains(r.Payload, []byte(memoryMarker)) {
			t.Fatalf("memory marker in a spooled %s payload", r.Lane)
		}
		// An owned batch holds its body base64-encoded in JSON, so the raw check
		// above cannot see inside it.
		if len(r.Payload) > 9 && r.Payload[0] == tagOwnedBatch {
			var ob ownedBatch
			if err := json.Unmarshal(r.Payload[9:], &ob); err != nil {
				t.Fatalf("spooled batch: %v", err)
			}
			if len(ob.Batch.Body) == 0 {
				t.Fatal("a spooled batch with no body checks nothing")
			}
			bodies++
			if bytes.Contains(ob.Batch.Body, []byte(memoryMarker)) {
				t.Fatalf("memory marker in a spooled %s batch body", ob.Batch.Signal)
			}
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if n == 0 {
		t.Fatal("no spooled payloads were checked")
	}
	if bodies == 0 {
		t.Fatal("no spooled batch bodies were checked")
	}
}

// canaryModel writes a note quoting the marker, as a model might.
type canaryModel struct{ saw string }

func (m *canaryModel) Complete(_ context.Context, r capture.Request) (capture.Response, error) {
	m.saw += r.User
	return capture.Response{Text: `{"title":"summary ` + memoryMarker + `","completed":"` + memoryMarker + `","observations":[{"type":"change","title":"note ` + memoryMarker + `","narrative":"quotes ` + memoryMarker + `"}]}`,
		Usage: capture.Usage{In: 10, Out: 10}}, nil
}

// Telemetry canary. A marker capture has seen in a tool's output, that the worker
// has sent to a model and written into a note, appears in nothing telemetry uploads
// or spools, even when the harness's own telemetry quotes the same call, and in no
// file on the device but the note's store.
func TestCapturedTextNeverReachesTelemetry(t *testing.T) {
	root := t.TempDir()
	paths := capture.PathsIn(root)
	if err := capture.RecordOffered(paths, time.Now(), "local"); err != nil {
		t.Fatal(err)
	}
	settings := capture.Settings{On: true, Acked: true, Writer: capture.WriterLocal, Command: "x", Model: "m"}
	cwd := t.TempDir()
	send := func(event string, body map[string]any) {
		b, _ := json.Marshal(body)
		capture.Handle(context.Background(), event, bytes.NewReader(b), capture.HookDeps{Paths: paths, Settings: settings, Host: "codex", Now: time.Now})
	}
	send(capture.HookTool, map[string]any{"session_id": "s", "cwd": cwd, "tool_name": "Bash",
		"tool_input": map[string]any{"command": "run " + memoryMarker}, "tool_response": map[string]any{"stdout": memoryMarker}})
	send(capture.HookStop, map[string]any{"session_id": "s", "cwd": cwd, "last_assistant_message": memoryMarker})
	if capture.QueueLen(paths) != 2 {
		t.Fatal("capture did not see the marker")
	}
	t.Run("uploaded", func(t *testing.T) { checkUploaded(t) })
	t.Run("spooled", func(t *testing.T) { checkSpooled(t) })

	model := &canaryModel{}
	db := filepath.Join(root, "memory", "memory.db")
	w := &capture.Worker{Paths: paths, Settings: func() capture.Settings { return settings },
		Offered:       func(context.Context) (bool, string, string) { return true, "", "local" },
		NewSummarizer: func(capture.Settings) capture.Summarizer { return model },
		Open: func(context.Context) (*memory.Store, string, error) {
			st, err := memory.Open(db)
			return st, "local", err
		}}
	w.Pass(context.Background())
	if !strings.Contains(model.saw, memoryMarker) || capture.QueueLen(paths) != 0 {
		t.Fatalf("the worker did not process the marker: queue %d", capture.QueueLen(paths))
	}
	if st := capture.LoadState(paths); st.Saved == 0 || st.FailedBatches != 0 {
		t.Fatalf("the worker did not run cleanly: %+v", st)
	}
	report, _ := json.Marshal(capture.BuildReport(paths, settings, time.Now()))
	state, _ := json.Marshal(capture.LoadState(paths))
	if bytes.Contains(report, []byte(memoryMarker)) || bytes.Contains(state, []byte(memoryMarker)) {
		t.Error("capture's status carries captured text")
	}
	stored := 0
	if err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		b, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		if strings.HasPrefix(path, db) {
			stored += bytes.Count(b, []byte(memoryMarker))
		} else if bytes.Contains(b, []byte(memoryMarker)) {
			t.Errorf("%s holds captured text", path)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if stored == 0 {
		t.Error("the note was not saved, so this checked nothing")
	}
}
