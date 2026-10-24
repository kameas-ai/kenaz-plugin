package app

import (
	"flag"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"

	"github.com/kameas-ai/kenaz-plugin/fleetapi"
	"github.com/kameas-ai/kenaz-plugin/internal/consent"
	"github.com/kameas-ai/kenaz-plugin/internal/device"
	"github.com/kameas-ai/kenaz-plugin/internal/enroll"
	"github.com/kameas-ai/kenaz-plugin/internal/features"
	"github.com/kameas-ai/kenaz-plugin/internal/project"
)

// projectionVersion is the projection-table version. A change bumps
// the notice version and forces re-confirmation.
const projectionVersion = project.TableVersion

// deviceDisplayName fills the notice's "{device display name}" from the
// persisted device name.
var deviceDisplayName = func() string { return "this device" }

// newConsentManager builds the consent manager over the state dir for
// the default environment.
func newConsentManager(env Env) (*consent.Manager, error) {
	r, err := env.resolve("")
	if err != nil {
		return nil, err
	}
	return newConsentManagerAt(env, r)
}

func newConsentManagerAt(env Env, r resolved) (*consent.Manager, error) {
	if id, err := enroll.LoadOrCreateNodeID(r.StateDir, env.now()); err == nil {
		s, _ := device.Load(r.StateDir)
		name := device.Resolve(s, enroll.LocalHost().GOOS, id, nil)
		deviceDisplayName = func() string { return name }
	}
	m := consent.NewManager(consent.Options{
		Provider:          env.Provider,
		Env:               r.Env,
		ProjectionVersion: projectionVersion,
		Store:             consent.NewStore(filepath.Join(r.StateDir, "consent.json")),
	})
	if err := m.Refresh(env.Ctx); err != nil {
		return nil, err
	}
	return m, nil
}

// noticeVars detects which supported harnesses are installed so the notice
// names the tools that may send data. It falls back to naming both.
func noticeVars(m *consent.Manager) consent.NoticeVars {
	home, _ := os.UserHomeDir()
	type h struct{ name, dir, bin string }
	var names, versions []string
	hosts := []h{{"Claude Code", ".claude", "claude"}, {"Codex", ".codex", "codex"}}
	if features.OpenCode {
		hosts = append(hosts, h{"OpenCode", filepath.Join(".config", "opencode"), "opencode"})
	}
	for _, x := range hosts {
		if _, err := os.Stat(filepath.Join(home, x.dir)); err != nil {
			continue
		}
		names = append(names, x.name)
		versions = append(versions, x.name+versionOf(x.bin))
	}
	if len(names) == 0 {
		names = []string{"Claude Code", "Codex"}
		versions = names
	}
	me := m.Me()
	org := me.OrgName
	if org == "" {
		org = "your organization"
	}
	return consent.NoticeVars{OrgName: org, Harnesses: names, DeviceDisplayName: deviceDisplayName(), HarnessVersions: versions}
}

// versionOf asks a harness CLI for its version. It returns "" on any
// failure. Only the version token is kept.
func versionOf(bin string) string {
	p, err := exec.LookPath(bin)
	if err != nil {
		return ""
	}
	out, err := exec.Command(p, "--version").Output()
	if err != nil {
		return ""
	}
	for _, f := range splitFields(string(out)) {
		if len(f) > 0 && f[0] >= '0' && f[0] <= '9' && len(f) <= 32 {
			return " " + f
		}
	}
	return ""
}

func splitFields(s string) []string {
	var out []string
	cur := ""
	for _, r := range s {
		if r == ' ' || r == '\n' || r == '\t' || r == '(' || r == ')' {
			if cur != "" {
				out = append(out, cur)
			}
			cur = ""
			continue
		}
		cur += string(r)
	}
	if cur != "" {
		out = append(out, cur)
	}
	return out
}

// runOptin changes the member's own telemetry choices. Without flags it
// runs the interactive first-run flow with the notice.
func runOptin(env Env, args []string) int {
	fs := flag.NewFlagSet("optin", flag.ContinueOnError)
	fs.SetOutput(env.Stderr)
	class := fs.String("class", "", "class to change: "+classList())
	on := fs.Bool("on", false, "opt in to --class (shows the notice first)")
	off := fs.Bool("off", false, "opt out of --class")
	allOff := fs.Bool("all-off", false, "opt out of every class")
	if err := fs.Parse(args); err != nil {
		return ExitUsage
	}
	m, err := newConsentManager(env)
	if err != nil {
		return providerErr(env, err)
	}
	switch {
	case *allOff:
		sel := map[fleetapi.TelemetryClass]bool{}
		for _, ci := range consent.Classes {
			sel[ci.Class] = false
		}
		return finishOptin(env, m.SetOptIns(env.Ctx, sel))
	case *class != "" && *off && !*on:
		return finishOptin(env, m.SetOptIns(env.Ctx, map[fleetapi.TelemetryClass]bool{fleetapi.TelemetryClass(*class): false}))
	case *class != "" || *on || *off:
		// Opting in always goes through the notice; there is no silent
		// opt-in flag.
		fmt.Fprintln(env.Stderr, "kenaz: to opt in, run `kenaz optin` without flags so you can review what is sent")
		return ExitUsage
	}
	if !stdinInteractive() {
		fmt.Fprintln(env.Stderr, errOptinNotInteractive)
		return ExitUsage
	}
	if _, err := consent.RunFirstRun(env.Ctx, m, noticeVars(m), os.Stdin, env.Stdout); err != nil {
		return providerErr(env, err)
	}
	return ExitOK
}

// stdinInteractive reports whether stdin is a terminal. Opting in must be
// the member's own choice after reading the notice, so
// the first-run flow never reads answers from a pipe: an agent running
// `printf '1\ny\n' | kenaz optin` must not be able to opt the member
// in. Opting out stays available non-interactively. A var so tests can
// override it.
var stdinInteractive = func() bool {
	fi, err := os.Stdin.Stat()
	return err == nil && fi.Mode()&os.ModeCharDevice != 0
}

const errOptinNotInteractive = "kenaz: choosing what to share needs your own terminal. Run `kenaz optin` in a terminal window (not through an agent) to review the notice and choose."

func finishOptin(env Env, err error) int {
	if err != nil {
		return providerErr(env, err)
	}
	fmt.Fprintln(env.Stdout, "Saved.")
	return ExitOK
}

func classList() string {
	s := ""
	for i, ci := range consent.Classes {
		if i > 0 {
			s += ", "
		}
		s += string(ci.Class)
	}
	return s
}
