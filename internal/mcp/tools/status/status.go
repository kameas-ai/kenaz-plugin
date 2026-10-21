// Package status is the fleet_status tool:
// sign-in state, env, tier, daemon health, lane counters and pending
// approval titles. It never returns tokens, email, user ids or hostnames.
package status

import (
	"context"

	sdk "github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/kameas-ai/kenaz-plugin/internal/daemon"
	"github.com/kameas-ai/kenaz-plugin/internal/mcp"
)

// Input takes no arguments.
type Input struct{}

// Group registers fleet_status.
var Group = mcp.GroupFunc(register)

func register(s *sdk.Server, d mcp.Deps) {
	tool := &sdk.Tool{
		Name:        "fleet_status",
		Description: "Show the Kameas Fleet connection status for this machine: whether you are signed in, the Fleet environment and plan tier, whether telemetry uploads are running or held (for example while an approval is pending or the organization is paused), and the titles of any pending approvals. Read-only. Accepting an approval happens in the Kameas Fleet dashboard, not here.",
		Annotations: &sdk.ToolAnnotations{Title: "Kameas Fleet status", ReadOnlyHint: true, IdempotentHint: true},
	}
	sdk.AddTool(s, tool, func(ctx context.Context, _ *sdk.CallToolRequest, _ Input) (*sdk.CallToolResult, daemon.Status, error) {
		if d.Touch != nil {
			d.Touch()
		}
		return nil, Get(ctx, d), nil
	})
}

// Get returns the daemon's status, or a local fallback when the daemon is
// unreachable, so the tool always answers with schema-valid output.
func Get(ctx context.Context, d mcp.Deps) daemon.Status {
	if d.Status != nil {
		if st, err := d.Status.Status(ctx); err == nil {
			return st
		}
	}
	return daemon.LocalStatus(ctx, d.Provider, d.Env, "The Kenaz relay is not running; it starts automatically with your next session.")
}
