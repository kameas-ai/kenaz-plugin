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

	"github.com/kameas-ai/kenaz-plugin/internal/daemon"
	"github.com/kameas-ai/kenaz-plugin/internal/logx"
	"github.com/kameas-ai/kenaz-plugin/internal/mcp"
	contexttools "github.com/kameas-ai/kenaz-plugin/internal/mcp/tools/context"
	skillstools "github.com/kameas-ai/kenaz-plugin/internal/mcp/tools/skills"
	"github.com/kameas-ai/kenaz-plugin/internal/mcp/tools/status"
)

// mcpGroups is the static tool registry.
var mcpGroups = []mcp.Group{status.Group, contexttools.Group, skillstools.Group}

// daemonLink keeps the MCP process registered with the daemon and
// reconnects (spawning if needed) when the daemon goes away.
type daemonLink struct {
	stateDir string
	spawn    daemon.SpawnFunc
	id       string
	log      *slog.Logger

	mu sync.Mutex
	c  *daemon.Client
}

func (l *daemonLink) client(ctx context.Context) *daemon.Client {
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
		l.log.Warn("daemon unavailable", "err", err.Error())
		return nil
	}
	l.c = c
	return c
}

// Status implements mcp.StatusSource.
func (l *daemonLink) Status(ctx context.Context) (daemon.Status, error) {
	c := l.client(ctx)
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
	r, err := env.resolve(*envName)
	if err != nil {
		fmt.Fprintln(env.Stderr, "kenaz: "+err.Error())
		return ExitUsage
	}
	rot := &logx.Rotator{Path: filepath.Join(r.StateDir, "logs", "mcp.log")}
	defer rot.Close()
	log := slog.New(logx.NewHandler(rot, slog.LevelInfo))

	idb := make([]byte, 8)
	rand.Read(idb)
	spawn := r.spawner(env)
	link := &daemonLink{stateDir: r.StateDir, spawn: spawn, id: "mcp-" + hex.EncodeToString(idb), log: log}

	ctx, cancel := context.WithCancel(env.Ctx)
	defer cancel()
	go func() {
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

	groups := append(append([]mcp.Group(nil), mcpGroups...), memoryGroup(env))
	srv := mcp.NewServer(Version, mcp.Deps{Provider: env.Provider, Env: r.Env, Status: link, StateDir: r.StateDir}, groups...)
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
