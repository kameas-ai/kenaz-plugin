package status

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	sdk "github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/kameas-ai/kenaz-plugin/fleetapi"
	"github.com/kameas-ai/kenaz-plugin/fleetapi/stub"
	"github.com/kameas-ai/kenaz-plugin/internal/daemon"
	"github.com/kameas-ai/kenaz-plugin/internal/mcp"
)

type src struct {
	st  daemon.Status
	err error
}

func (s src) Status(context.Context) (daemon.Status, error) { return s.st, s.err }

func call(t *testing.T, d mcp.Deps) map[string]any {
	t.Helper()
	ctx := context.Background()
	srv := mcp.NewServer("test", d, Group)
	ct, st := sdk.NewInMemoryTransports()
	if _, err := srv.Connect(ctx, st, nil); err != nil {
		t.Fatal(err)
	}
	cs, err := sdk.NewClient(&sdk.Implementation{Name: "t", Version: "1"}, nil).Connect(ctx, ct, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer cs.Close()
	tools, err := cs.ListTools(ctx, nil)
	if err != nil || len(tools.Tools) != 1 || tools.Tools[0].Name != "fleet_status" {
		t.Fatalf("tools = %+v err=%v", tools, err)
	}
	res, err := cs.CallTool(ctx, &sdk.CallToolParams{Name: "fleet_status"})
	if err != nil || res.IsError {
		t.Fatalf("call: %+v %v", res, err)
	}
	raw, _ := json.Marshal(res.StructuredContent)
	var out map[string]any
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatal(err)
	}
	for _, k := range []string{"schema_version", "flavor", "env", "session", "daemon", "lanes", "pending_approvals"} {
		if _, ok := out[k]; !ok {
			t.Fatalf("missing %q in %s", k, raw)
		}
	}
	for _, bad := range []string{"token", "email", "user_id", "hostname"} {
		if strings.Contains(string(raw), bad) {
			t.Fatalf("status leaks %q: %s", bad, raw)
		}
	}
	return out
}

// Schema-valid JSON over MCP, signed in (daemon) and signed out
// (fallback, unofficial build).
func TestFleetStatus(t *testing.T) {
	signedIn := daemon.Status{SchemaVersion: 1, Flavor: "official", Env: "dev", Session: daemon.SessionSignedIn,
		Tier: "team", Enroll: "enrolled", Daemon: daemon.DaemonStatus{Running: true, Port: 4318},
		Lanes: []daemon.LaneStatus{{Name: "telemetry", State: "held", Reason: "approval_pending"}}, PendingApprovals: []string{"Agent Telemetry Notice"}}
	out := call(t, mcp.Deps{Provider: stub.New(), Env: fleetapi.EnvDev, Status: src{st: signedIn}})
	if out["session"] != daemon.SessionSignedIn || out["tier"] != "team" {
		t.Fatalf("signed-in = %v", out)
	}
	out = call(t, mcp.Deps{Provider: stub.New(), Env: fleetapi.EnvProd, Status: src{err: errors.New("down")}})
	if out["session"] != daemon.SessionUnofficial || out["daemon"].(map[string]any)["running"] != false {
		t.Fatalf("fallback = %v", out)
	}
}
