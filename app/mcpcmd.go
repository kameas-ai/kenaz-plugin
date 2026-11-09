package app

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"flag"
	"fmt"
	"log/slog"
	"path/filepath"
	"sync"
	"time"

	sdk "github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/kameas-ai/kenaz-plugin/fleetapi"
	"github.com/kameas-ai/kenaz-plugin/internal/daemon"
	"github.com/kameas-ai/kenaz-plugin/internal/logx"
	"github.com/kameas-ai/kenaz-plugin/internal/mcp"
	approvaltools "github.com/kameas-ai/kenaz-plugin/internal/mcp/tools/approvals"
	contexttools "github.com/kameas-ai/kenaz-plugin/internal/mcp/tools/context"
	skillstools "github.com/kameas-ai/kenaz-plugin/internal/mcp/tools/skills"
	"github.com/kameas-ai/kenaz-plugin/internal/mcp/tools/status"
	"github.com/kameas-ai/kenaz-plugin/internal/variant"
)

// mcpGroups is the static tool registry.
var mcpGroups = []mcp.Group{status.Group, contexttools.Group, skillstools.Group, approvaltools.Group}

// daemonLink keeps the MCP process registered with the daemon and
// reconnects (spawning if needed) when the daemon goes away.
type daemonLink struct {
	stateDir string
	spawn    daemon.SpawnFunc
	id       string
	log      *slog.Logger
	// conflict is set when another build's relay uses the environment:
	// the link never connects or spawns, and reports it.
	conflict error

	mu sync.Mutex
	c  *daemon.Client
}

func (l *daemonLink) client(ctx context.Context) *daemon.Client {
	if l.conflict != nil {
		return nil
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.c != nil {
		if err := daemon.Probe(ctx, l.c.Info); err == nil {
			return l.c
		}
		l.c = nil
	}
	c, err := daemon.EnsureRunning(ctx, l.stateDir, l.spawn, 2*time.Second)
	if err != nil {
		if ctx.Err() == nil { // shutdown cancels a pending ping: not a daemon fault
			l.log.Warn("daemon unavailable", "err", err.Error())
		}
		return nil
	}
	l.c = c
	return c
}

// Status implements mcp.StatusSource.
func (l *daemonLink) Status(ctx context.Context) (daemon.Status, error) {
	c := l.client(ctx)
	if l.conflict != nil {
		return daemon.Status{}, l.conflict
	}
	if c == nil {
		return daemon.Status{}, daemon.ErrNotRunning
	}
	return c.Status(ctx)
}

func (l *daemonLink) ping(ctx context.Context) {
	if c := l.client(ctx); c != nil {
		c.Ping(ctx, l.id)
	}
}

// reloadSessionOnToolCall re-reads the stored session before each tool call,
// without network I/O. The provider loads the session once per process, and
// an MCP server lives as long as its host, so without this a `kenaz login`
// or logout in a terminal is not seen until the host restarts: Claude Desktop
// chat's "run kenaz login, then try again" would fail the second time too.
//
// Known limits: where no credential store is recorded and mcp may not probe
// (a first sign-in on Linux with a Secret Service), this process has no
// keychain to read and still needs a restart. After a store read error the
// provider's periodic reload backs off for 5 minutes. The read has no timeout
// and the official provider holds its lock across it, so a hung keychain
// stalls tool calls; a bounded wait here would not help, because each tool
// then blocks on that same lock when it asks for the session.
func reloadSessionOnToolCall(p fleetapi.Provider) sdk.Middleware {
	return func(next sdk.MethodHandler) sdk.MethodHandler {
		return func(ctx context.Context, method string, req sdk.Request) (sdk.Result, error) {
			if method == "tools/call" && p != nil {
				// A store that cannot be read changes nothing. An identity
				// mismatch clears the session and fails closed. Either way the
				// tool reports what is left, so the error needs no handling.
				_ = fleetapi.ReloadSession(ctx, p.Auth())
			}
			return next(ctx, method, req)
		}
	}
}

// runMCP serves MCP on stdio. It must never write anything but protocol
// to stdout; diagnostics go to relay.log. The daemon is lazily spawned and
// a failure to reach it never fails the MCP handshake.
func runMCP(env Env, args []string) int {
	fs := flag.NewFlagSet("mcp", flag.ContinueOnError)
	fs.SetOutput(env.Stderr)
	envName := fs.String("env", "", "Fleet environment")
	if err := fs.Parse(args); err != nil {
		return ExitUsage
	}
	r, err := env.resolveState(*envName)
	if err != nil {
		fmt.Fprintln(env.Stderr, variant.Current.Binary+": "+err.Error())
		return ExitUsage
	}
	// Another build is using this environment: serve anyway (a failure
	// never fails the MCP handshake), with no relay and with every Fleet
	// tool saying why. On-device memory has no environment and works.
	provider := env.Provider
	if r.Conflict != nil {
		fmt.Fprintln(env.Stderr, variant.Current.Binary+": "+r.Conflict.Error())
		provider = conflictProvider(provider, r.Conflict)
	}
	rot := &logx.Rotator{Path: filepath.Join(r.StateDir, "logs", "mcp.log")}
	defer rot.Close()
	log := slog.New(logx.NewHandler(rot, slog.LevelInfo))

	idb := make([]byte, 8)
	rand.Read(idb)
	spawn := r.spawner(env)
	link := &daemonLink{stateDir: r.StateDir, spawn: spawn, id: "mcp-" + hex.EncodeToString(idb), log: log, conflict: r.Conflict}

	ctx, cancel := context.WithCancel(env.Ctx)
	// The pinger logs through rot. Stop it and wait for it before rot is
	// closed (defers run in reverse), so no write reopens mcp.log after
	// runMCP has returned.
	pingDone := make(chan struct{})
	defer func() { cancel(); <-pingDone }()
	go func() {
		defer close(pingDone)
		link.ping(ctx)
		for {
			select {
			case <-ctx.Done():
				return
			case <-time.After(time.Minute):
				link.ping(ctx)
			}
		}
	}()
	defer func() {
		bctx, bcancel := context.WithTimeout(context.Background(), time.Second)
		defer bcancel()
		if c := connectDaemon(bctx, r.StateDir); c != nil {
			c.Bye(bctx, link.id)
		}
	}()

	menv := env
	menv.Provider = provider
	local, team := menv.contextFilesGroups(ctx)
	mcp.ContextFilesServed = len(local) > 0 // the local tools; no text names the team ones
	groups := append(append([]mcp.Group(nil), mcpGroups...), memoryGroup(menv, len(local) > 0))
	groups = append(append(groups, local...), team...)
	srv := mcp.NewServer(Version, mcp.Deps{Provider: provider, Env: r.Env, Status: link, StateDir: r.StateDir, Approvals: approvalsClient(link.client)}, groups...)
	srv.AddReceivingMiddleware(reloadSessionOnToolCall(provider))
	serve := env.ServeMCP
	if serve == nil {
		serve = mcp.ServeStdio
	}
	if err := serve(ctx, srv); err != nil && ctx.Err() == nil {
		log.Warn("mcp server stopped", "err", err.Error())
		return ExitError
	}
	return ExitOK
}
