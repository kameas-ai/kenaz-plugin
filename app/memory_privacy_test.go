package app

import (
	"bytes"
	"context"
	"encoding/json"
	"path/filepath"
	"testing"

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
	n := 0
	if _, err := l2.sp.Drain(func(r spool.Record) error {
		n++
		if bytes.Contains(r.Payload, []byte(memoryMarker)) {
			t.Fatalf("memory marker in a spooled %s payload", r.Lane)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if n == 0 {
		t.Fatal("no spooled payloads were checked")
	}
}
