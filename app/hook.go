package app

import (
	"context"
	"os"
	"time"

	"github.com/kameas-ai/kenaz-plugin/internal/consent"
	"github.com/kameas-ai/kenaz-plugin/internal/daemon"
	"github.com/kameas-ai/kenaz-plugin/internal/daemonstate"
	"github.com/kameas-ai/kenaz-plugin/internal/hook"
)

// runHook is the harness hook entry point. It must never
// fail or slow a harness session: it always exits 0, writes nothing on
// error, and writes nothing to stderr.
func runHook(env Env, args []string) int {
	if len(args) != 1 {
		return ExitOK
	}
	deps := hook.Deps{Getenv: os.Getenv}
	// Memory needs no Fleet env or sign-in.
	if mp, err := env.memoryPaths(); err == nil {
		deps.Memory = memoryIndex{mp: mp}
	}
	if r, err := env.resolve(""); err == nil {
		deps.Daemon = relayDaemon{stateDir: r.StateDir, spawn: r.spawner(env)}
		deps.Banners = consentBanners{env: env, r: r}
		// reloadSkills is emitted only under Claude Code (hook.Run checks).
		deps.Skills = skillsReload{stateDir: r.StateDir}
	}
	hook.Run(env.Ctx, args[0], os.Stdin, env.Stdout, deps)
	return ExitOK
}

// relayDaemon adapts the daemon client (HMAC-verified health check,
// nonce-bearer control API) to the hook's Daemon interface.
type relayDaemon struct {
	stateDir string
	spawn    func() error
}

func (d relayDaemon) Ensure(ctx context.Context) error {
	_, err := daemon.EnsureRunning(ctx, d.stateDir, d.spawn, 2*time.Second)
	return err
}

func (d relayDaemon) SessionEnd(ctx context.Context, end daemonstate.SessionEnd) error {
	if end.SessionHash == "" {
		return nil
	}
	c, err := daemon.Connect(ctx, d.stateDir)
	if err != nil {
		return err
	}
	return c.SessionEnd(ctx, end.Harness, end.SessionHash)
}

// consentBanners renders the consent session-start context.
type consentBanners struct {
	env Env
	r   resolved
}

func (b consentBanners) Banners(ctx context.Context) []string {
	env := b.env
	env.Ctx = ctx
	m, err := newConsentManagerAt(env, b.r)
	if err != nil {
		return nil
	}
	if t := consent.SessionStartContext(m.State()); t != "" {
		return []string{t}
	}
	return nil
}
