package app

import (
	"bytes"
	"encoding/json"
	"flag"
	"fmt"
	"strings"

	"github.com/kameas-ai/kenaz-plugin/internal/consent"
	"github.com/kameas-ai/kenaz-plugin/internal/daemon"
	"github.com/kameas-ai/kenaz-plugin/internal/device"
	"github.com/kameas-ai/kenaz-plugin/internal/enroll"
)

// runStatus prints the relay status; --json emits the versioned schema
// shared with the fleet_status MCP tool.
func runStatus(env Env, args []string) int {
	fs := flag.NewFlagSet("status", flag.ContinueOnError)
	fs.SetOutput(env.Stderr)
	envName := fs.String("env", "", "Fleet environment")
	asJSON := fs.Bool("json", false, "print machine-readable JSON (schema_version 1)")
	if err := fs.Parse(args); err != nil {
		return ExitUsage
	}
	r, err := env.resolve(*envName)
	if err != nil {
		fmt.Fprintln(env.Stderr, "kenaz: "+err.Error())
		return ExitUsage
	}
	var st daemon.Status
	if c := connectDaemon(env.Ctx, r.StateDir); c != nil {
		if st, err = c.Status(env.Ctx); err != nil {
			st = daemon.LocalStatus(env.Ctx, env.Provider, r.Env, "daemon unreachable")
		}
	} else {
		st = daemon.LocalStatus(env.Ctx, env.Provider, r.Env, "relay not running")
	}
	if st.DeviceName == "" {
		if id, err := enroll.LoadOrCreateNodeID(r.StateDir, env.now()); err == nil {
			s, _ := device.Load(r.StateDir)
			st.DeviceName = device.Resolve(s, enroll.LocalHost().GOOS, id, nil)
		}
	}
	// Consent / org state is part of the same report.
	// A manager that can't refresh (signed out, offline) is simply omitted.
	cm, cerr := newConsentManagerAt(env, r)
	if *asJSON {
		out := map[string]any{}
		b, _ := json.Marshal(st)
		_ = json.Unmarshal(b, &out)
		if mp, err := env.memoryPaths(); err == nil {
			out["memory"] = memoryJSON(env, mp)
		}
		if cerr == nil {
			var cs bytes.Buffer
			if consent.WriteJSON(&cs, cm.State()) == nil {
				out["consent"] = json.RawMessage(cs.Bytes())
			}
		}
		enc := json.NewEncoder(env.Stdout)
		enc.SetIndent("", "  ")
		enc.Encode(out)
		return ExitOK
	}
	w := env.Stdout
	fmt.Fprintf(w, "Kenaz %s — %s build (%s)\n", Version, st.Flavor, flavorLine(env.Provider))
	fmt.Fprintf(w, "  environment:  %s\n", st.Env)
	fmt.Fprintf(w, "  session:      %s\n", strings.ReplaceAll(st.Session, "_", " "))
	if st.Tier != "" {
		fmt.Fprintf(w, "  plan:         %s\n", st.Tier)
	}
	fmt.Fprintf(w, "  device name:  %s\n", st.DeviceName)
	fmt.Fprintf(w, "  credential store: %s\n", env.cred.label())
	fmt.Fprintf(w, "  enrollment:   %s\n", st.Enroll)
	if st.OrgPaused {
		fmt.Fprintf(w, "  organization: paused (%s); uploads are held\n", st.PausedCategory)
	}
	if st.Lockdown {
		fmt.Fprintln(w, "  lockdown:     on; uploads are held (your agent is not blocked)")
	}
	if st.Daemon.Running {
		fmt.Fprintf(w, "  relay:        running on 127.0.0.1:%d\n", st.Daemon.Port)
	} else {
		fmt.Fprintln(w, "  relay:        not running (starts on demand with your next session)")
	}
	for _, l := range st.Lanes {
		fmt.Fprintf(w, "  lane %-9s %s %s (uploaded %d, held %d, dropped %d)\n", l.Name+":", l.State, l.Reason, l.Uploaded, l.Held, l.Dropped)
	}
	for _, a := range st.PendingApprovals {
		fmt.Fprintf(w, "  pending:      %s (open the Kameas Fleet dashboard to review)\n", a)
	}
	for _, n := range st.Notes {
		fmt.Fprintf(w, "  note:         %s\n", n)
	}
	if mp, err := env.memoryPaths(); err == nil {
		fmt.Fprintf(w, "  %s\n", mp.statusLine(env.Ctx))
	}
	if cerr == nil {
		consent.WriteText(w, cm.State())
	}
	return ExitOK
}
