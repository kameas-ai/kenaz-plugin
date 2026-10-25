// Package status is the fleet_status tool:
// sign-in state, env, tier, daemon health, lane counters and pending
// approval titles. It never returns tokens, email, user ids or hostnames.
package status

import (
	"context"

	sdk "github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/kameas-ai/kenaz-plugin/internal/consent"
	"github.com/kameas-ai/kenaz-plugin/internal/daemon"
	"github.com/kameas-ai/kenaz-plugin/internal/mcp"
	"github.com/kameas-ai/kenaz-plugin/internal/setup"
)

// closedWorld is the tool's explicit openWorldHint: it reads only the local
// relay on this machine, never an open set of external services.
var closedWorld = false

// Input takes no arguments.
type Input struct{}

// Group registers fleet_status.
var Group = mcp.GroupFunc(register)

func register(s *sdk.Server, d mcp.Deps) {
	tool := &sdk.Tool{
		Name:        "fleet_status",
		Title:       "Kameas Fleet status",
		Description: "Show the Kameas Fleet connection status for this machine: whether you are signed in, the Fleet environment and plan tier, whether telemetry uploads are running or held (for example while an approval is pending or the organization is paused), and the titles of any pending approvals with the dashboard link to review them, and whether reporting is working (consent, last successful upload, queued count, last error category, receiver port, per-host setup). Read-only. Accepting an approval happens in the Kameas Fleet dashboard, not here.",
		Annotations: &sdk.ToolAnnotations{Title: "Kameas Fleet status", ReadOnlyHint: true, IdempotentHint: true, OpenWorldHint: &closedWorld},
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
	st := daemon.LocalStatus(ctx, d.Provider, d.Env, "The Kenaz relay is not running; it starts automatically with your next session.")
	st.DashboardURL = consent.DashboardBase(d.Env)
	if d.StateDir != "" {
		for _, h := range setup.HostStates(d.StateDir) {
			st.Hosts = append(st.Hosts, daemon.HostSetup{Host: h.Host, State: h.State})
		}
	}
	return st
}
