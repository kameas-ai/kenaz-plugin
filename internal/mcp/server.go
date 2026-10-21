// Package mcp is the stdio MCP server the harness starts. It is deliberately thin: tool groups register
// through Group, and anything stateful lives in the daemon, reached over the
// nonce-authenticated loopback control API.
//
// Tool groups live in internal/mcp/tools/<group>/ (status, skills,
// context, memory). No tool may make a permission
// decision or otherwise act as policy.
package mcp

import (
	"context"

	sdk "github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/kameas-ai/kenaz-plugin/fleetapi"
	"github.com/kameas-ai/kenaz-plugin/internal/daemon"
)

// ServerName is the MCP implementation name the harness shows.
const ServerName = "kenaz"

// StatusSource returns the relay status (the daemon client in production).
type StatusSource interface {
	Status(ctx context.Context) (daemon.Status, error)
}

// Deps are what tool groups may use.
type Deps struct {
	Provider fleetapi.Provider
	Env      fleetapi.Env
	Status   StatusSource
	// StateDir is the per-env state directory (skills subscriptions).
	StateDir string
	// Touch records tool activity with the daemon (optional).
	Touch func()
}

// Group registers one tool group's tools.
type Group interface {
	Register(s *sdk.Server, d Deps)
}

// GroupFunc adapts a func.
type GroupFunc func(*sdk.Server, Deps)

func (f GroupFunc) Register(s *sdk.Server, d Deps) { f(s, d) }

// NewServer builds the server with the given groups.
func NewServer(version string, d Deps, groups ...Group) *sdk.Server {
	s := sdk.NewServer(&sdk.Implementation{Name: ServerName, Title: "Kenaz", Version: version}, nil)
	for _, g := range groups {
		g.Register(s, d)
	}
	return s
}

// ServeStdio runs s on stdin/stdout until the harness disconnects.
func ServeStdio(ctx context.Context, s *sdk.Server) error {
	return s.Run(ctx, &sdk.StdioTransport{})
}
