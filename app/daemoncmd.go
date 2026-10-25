package app

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"path/filepath"

	"github.com/kameas-ai/kenaz-plugin/internal/daemon"
	"github.com/kameas-ai/kenaz-plugin/internal/logx"
	"github.com/kameas-ai/kenaz-plugin/internal/spool"
	"github.com/kameas-ai/kenaz-plugin/internal/variant"
)

// DaemonHook lets other parts of the app (the telemetry receiver, consent) extend the
// daemon: it may add Lanes and Contributors to opt, and return a mount
// func that registers HTTP handlers (e.g. d.Handle("POST /v1/logs",
// d.RequireSession(h))) on the constructed daemon. Hooks must not block.
type DaemonHook func(opt *daemon.Options) (mount func(*daemon.Daemon), err error)

// daemonHooks is extended by app wiring.
var daemonHooks []DaemonHook

// runDaemon runs the relay daemon in the foreground of this process. The
// MCP process and the harness hooks start it detached on demand,
// and it exits when idle.
func runDaemon(env Env, args []string) int {
	fs := flag.NewFlagSet("daemon", flag.ContinueOnError)
	fs.SetOutput(env.Stderr)
	envName := fs.String("env", "", "Fleet environment")
	foreground := fs.Bool("foreground", false, "also log to stderr")
	if err := fs.Parse(args); err != nil {
		return ExitUsage
	}
	r, err := env.resolve(*envName)
	if err != nil {
		fmt.Fprintln(env.Stderr, variant.Current.Binary+": "+err.Error())
		return ExitUsage
	}
	rot := &logx.Rotator{Path: filepath.Join(r.StateDir, "logs", "relay.log")}
	defer rot.Close()
	var out io.Writer = rot
	if *foreground {
		out = io.MultiWriter(rot, env.Stderr)
	}
	log := slog.New(logx.NewHandler(out, slog.LevelInfo))

	opt := daemon.Options{
		Provider: env.Provider, Env: r.Env, StateDir: r.StateDir, Version: Version,
		Logger: log,
	}
	// Spool key from the OS keychain or, on a device without one, from an
	// owner-only file (see withCredentialStore). If neither works the
	// daemon refuses to start rather than keep an unencrypted queue.
	if err := env.cred.usable(); err != nil {
		log.Error("credential store unusable; refusing to start", "err", err.Error())
		fmt.Fprintln(env.Stderr, variant.Current.Binary+": "+err.Error()+"; the relay cannot start")
		return ExitError
	}
	key, err := spool.GetOrCreateKey(env.keyStore(), string(r.Env))
	if err != nil {
		if env.cred.files() {
			log.Error("spool key file unusable; refusing to start", "err", err.Error())
			fmt.Fprintln(env.Stderr, variant.Current.Binary+": the spool key file cannot be used; the relay cannot start: "+err.Error())
			return ExitError
		}
		log.Error("keychain unavailable; refusing to start", "err", err.Error())
		fmt.Fprintln(env.Stderr, variant.Current.Binary+": "+errKeychainGone.Error()+"; the relay cannot start")
		return ExitError
	}
	if err := env.cred.record(); err != nil {
		log.Warn("credential store not recorded", "err", err.Error())
	}
	sp, err := spool.Open(filepath.Join(r.StateDir, "spool"), key, spool.Options{})
	if err != nil {
		fmt.Fprintln(env.Stderr, variant.Current.Binary+": "+err.Error())
		return ExitError
	}
	opt.Spool = sp
	var mounts []func(*daemon.Daemon)
	// The telemetry lane, the skills lane and the approvals lane are always
	// installed; the memory sync lane when the build can sync (it idles until a
	// user turns memory sync on). daemonHooks lets tests add more.
	hooks := append([]DaemonHook{telemetryHook(env), skillsDaemonHook, hostSetupHook, approvalsDaemonHook(env), memorySyncHook(env)}, daemonHooks...)
	for _, h := range hooks {
		mount, err := h(&opt)
		if err != nil {
			fmt.Fprintln(env.Stderr, variant.Current.Binary+": "+err.Error())
			return ExitError
		}
		if mount != nil {
			mounts = append(mounts, mount)
		}
	}
	d := daemon.New(opt)
	for _, m := range mounts {
		m(d)
	}
	err = d.Run(env.Ctx)
	if errors.Is(err, daemon.ErrAlreadyRunning) {
		fmt.Fprintln(env.Stderr, variant.Current.Binary+": relay already running")
		return ExitOK
	}
	if err != nil {
		fmt.Fprintln(env.Stderr, variant.Current.Binary+": "+err.Error())
		return ExitError
	}
	return ExitOK
}
