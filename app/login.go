package app

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"time"

	"github.com/kameas-ai/kenaz-plugin/fleetapi"
	"github.com/kameas-ai/kenaz-plugin/internal/consent"
	"github.com/kameas-ai/kenaz-plugin/internal/enroll"
	"github.com/kameas-ai/kenaz-plugin/internal/variant"
)

// runLogin signs in (PKCE loopback via fleetapi.Auth), then hands the
// daemon a kick so it enrolls right away. Signing in
// never turns telemetry on: each member opts in personally.
func runLogin(env Env, args []string) int {
	fs := flag.NewFlagSet("login", flag.ContinueOnError)
	fs.SetOutput(env.Stderr)
	envName := fs.String("env", "", "Fleet environment: "+variant.Current.EnvChoices("|")+" (default: "+variant.Current.EnvVar+", config, "+variant.Current.DefaultEnv+")")
	noBrowser := fs.Bool("no-browser", false, "print the sign-in URL instead of opening a browser")
	if err := fs.Parse(args); err != nil {
		return ExitUsage
	}
	r, err := env.resolve(*envName)
	if err != nil {
		fmt.Fprintln(env.Stderr, variant.Current.Binary+": "+err.Error())
		return ExitUsage
	}
	if err := env.cred.usable(); err != nil {
		fmt.Fprintln(env.Stderr, variant.Current.Binary+": "+err.Error())
		return ExitError
	}
	open := env.OpenBrowser
	if open == nil {
		open = openBrowser
	}
	sess, err := env.Provider.Auth().Login(env.Ctx, fleetapi.LoginRequest{
		Env: r.Env,
		OpenURL: func(url string) error {
			fmt.Fprintf(env.Stdout, "Sign in to Kameas Fleet (%s) in your browser:\n  %s\n", r.Env, url)
			if *noBrowser {
				return nil
			}
			if err := open(url); err != nil {
				// No browser here (a headless VM, no display): carry on as
				// --no-browser does. The link above still completes sign-in.
				fmt.Fprintf(env.Stdout, "Could not open a browser here (%s). Open the link above in a browser that can reach this machine's 127.0.0.1; see `kenaz login` in the README for VMs.\n", err.Error())
			}
			return nil
		},
	})
	if err != nil {
		return providerErr(env, err)
	}
	fmt.Fprintf(env.Stdout, "Signed in to Kameas Fleet (%s).\n", sess.Env)
	if err := env.cred.record(); err != nil {
		fmt.Fprintln(env.Stderr, variant.Current.Binary+": "+err.Error())
	}
	if c := connectDaemon(env.Ctx, r.StateDir); c != nil {
		c.Kick(env.Ctx)
	} else {
		r.spawner(env)()
	}
	// Fleet refuses orgs outside the early-access program. Wait briefly for the daemon's first enroll so the member sees
	// that here rather than only in `kenaz status`.
	if awaitEnroll(env.Ctx, r.StateDir, loginEnrollWait) == string(enroll.StateEarlyAccess) {
		fmt.Fprintln(env.Stderr, variant.Current.Binary+": "+fleetapi.EarlyAccessMessage)
		return ExitError
	}
	// First-run opt-in. Nothing is sent until the member
	// chooses here or later with `kenaz optin`.
	// Sign-in already succeeded; a consent-step failure (offline, stdin
	// not a terminal) must not undo it. Nothing is sent until the member
	// opts in, so deferring the choice is always safe.
	m, err := newConsentManagerAt(env, r)
	if err == nil {
		if st := m.State(); !st.AnyOn() || st.NoticeReconfirm {
			if stdinInteractive() {
				_, err = consent.RunFirstRun(env.Ctx, m, noticeVars(m), os.Stdin, env.Stdout)
			} else {
				err = errors.New("stdin is not a terminal")
			}
		}
	}
	if err != nil {
		fmt.Fprintln(env.Stdout, "Nothing about your sessions is sent until you choose what to share. Run `kenaz optin` when you're ready.")
	}
	return ExitOK
}

// loginEnrollWait bounds how long login waits for the first enroll result.
var loginEnrollWait = 5 * time.Second

// loginDaemonWait bounds how long login waits for a daemon to answer at all.
var loginDaemonWait = 2 * time.Second

// awaitEnroll polls the daemon until its enroll state is settled (enrolled,
// early access required, removed) or wait elapses, and returns the last
// state seen ("" if the daemon was never reachable within loginDaemonWait).
func awaitEnroll(ctx context.Context, daemonDir string, wait time.Duration) string {
	start := time.Now()
	deadline := start.Add(wait)
	last := ""
	reached := false
	for {
		if !reached && time.Since(start) > loginDaemonWait {
			return ""
		}
		if c := connectDaemon(ctx, daemonDir); c != nil {
			reached = true
			if st, err := c.Status(ctx); err == nil {
				last = st.Enroll
				switch enroll.State(last) {
				case enroll.StateEnrolled, enroll.StateEarlyAccess, enroll.StateRemoved:
					return last
				}
			}
		}
		if time.Now().After(deadline) {
			return last
		}
		select {
		case <-ctx.Done():
			return last
		case <-time.After(100 * time.Millisecond):
		}
	}
}
