package app

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"time"

	"github.com/kameas-ai/kenaz-plugin/fleetapi"
	"github.com/kameas-ai/kenaz-plugin/internal/credstore"
	"github.com/kameas-ai/kenaz-plugin/internal/daemon"
	"github.com/kameas-ai/kenaz-plugin/internal/spool"
)

// runDoctor reports the build flavor first, then local checks.
// It is read-only: it never writes harness config (`kenaz setup` owns that).
func runDoctor(env Env, args []string) int {
	fs := flag.NewFlagSet("doctor", flag.ContinueOnError)
	fs.SetOutput(env.Stderr)
	envName := fs.String("env", "", "Fleet environment")
	if err := fs.Parse(args); err != nil {
		return ExitUsage
	}
	w := env.Stdout
	fmt.Fprintf(w, "build flavor: %s\n", flavorLine(env.Provider))
	if env.Provider.Flavor() == fleetapi.FlavorUnofficial {
		fmt.Fprintln(w, "note: "+fleetapi.ErrUnofficialBuild.Error())
	}
	failed := false
	check := func(name string, err error, ok string) {
		if err != nil {
			failed = true
			fmt.Fprintf(w, "[FAIL] %-18s %s\n", name, err.Error())
			return
		}
		fmt.Fprintf(w, "[ ok ] %-18s %s\n", name, ok)
	}
	warn := func(name, msg string) { fmt.Fprintf(w, "[warn] %-18s %s\n", name, msg) }

	r, err := env.resolve(*envName)
	check("environment", err, fmt.Sprintf("%s (from %s)", r.Env, r.Source))
	if err != nil {
		return ExitError
	}
	probe := filepath.Join(r.StateDir, ".doctor-probe")
	err = os.WriteFile(probe, []byte("ok"), 0o600)
	os.Remove(probe)
	check("state directory", err, "writable")

	_, kerr := env.keyStore().Get(string(r.Env))
	if errors.Is(kerr, spool.ErrNoKey) {
		kerr = nil
	}
	cs := env.cred
	if cs == nil {
		cs = &credStore{store: credstore.Keychain}
	}
	where := cs.label()
	if cs.files() {
		where += " in " + cs.dir
	}
	switch {
	case cs.recorded:
		where += " (recorded)"
	case cs.store != "":
		where += " (recorded at the next sign-in or relay start)"
	}
	if kerr == nil {
		kerr = cs.err
	}
	var probeErr error
	if cs.probe != nil {
		probeErr = cs.probe()
	}
	if cs.recorded && cs.store == credstore.Keychain && probeErr != nil {
		kerr = errKeychainGone
	}
	check("credential store", kerr, where)
	if cs.files() {
		warn("credential store", fileStoreNote)
	}
	probeNow := "OS keychain available"
	if probeErr != nil {
		probeNow = "no usable OS keychain"
	}
	switch agree := (probeErr == nil) == (cs.store != credstore.Files); {
	case cs.store == "":
		fmt.Fprintf(w, "[info] %-18s %s\n", "keychain probe", probeNow)
	case agree:
		fmt.Fprintf(w, "[info] %-18s %s (agrees)\n", "keychain probe", probeNow)
	case cs.files():
		fmt.Fprintf(w, "[info] %-18s %s now; staying with files (run `kenaz logout` to choose again)\n", "keychain probe", probeNow)
	default:
		fmt.Fprintf(w, "[info] %-18s %s (differs)\n", "keychain probe", probeNow)
	}

	c := connectDaemon(env.Ctx, r.StateDir)
	if c != nil {
		check("relay", nil, fmt.Sprintf("healthy on 127.0.0.1:%d", c.Info.Port))
		if c.Info.Port != daemon.DefaultPorts[0] {
			warn("receiver port", fmt.Sprintf("bound %d, not %d; `kenaz setup` writes the bound port", c.Info.Port, daemon.DefaultPorts[0]))
		}
	} else {
		warn("relay", "not running (starts on demand with your next session)")
		ports := daemon.DefaultPorts
		if ln, err := net.Listen("tcp", fmt.Sprintf("127.0.0.1:%d", ports[0])); err == nil {
			ln.Close()
			check("receiver port", nil, fmt.Sprintf("%d available", ports[0]))
		} else {
			warn("receiver port", fmt.Sprintf("%d is in use by another program; the relay will use %d or an ephemeral port", ports[0], ports[1]))
		}
	}

	switch _, err := env.Provider.Auth().Session(env.Ctx); {
	case err == nil:
		check("sign-in", nil, "signed in")
		if _, err := env.Provider.Me().Get(env.Ctx); err != nil {
			check("Kameas Fleet API", err, "")
		} else {
			check("Kameas Fleet API", nil, "reachable")
		}
		if c != nil {
			if st, err := c.Status(env.Ctx); err == nil {
				fmt.Fprintf(w, "[info] %-18s %s\n", "enrollment", st.Enroll)
				doctorReporting(w, st)
			}
		}
	case errors.Is(err, fleetapi.ErrUnofficialBuild):
		warn("sign-in", "not available in this build")
	default:
		warn("sign-in", "not signed in; run `kenaz login`")
	}
	if mp, err := env.memoryPaths(); err == nil {
		fmt.Fprintf(w, "[info] %s\n", mp.statusLine(env.Ctx))
	}
	fmt.Fprintf(w, "[info] %-18s %s\n", "clock skew", "not checked yet")
	for _, h := range hostSetups(r.StateDir) {
		fmt.Fprintf(w, "[info] %-18s %s (`kenaz setup` changes it)\n", "setup "+h.Host, h.State)
	}
	if failed {
		return ExitError
	}
	return ExitOK
}

// doctorReporting turns the daemon's reporting summary into doctor lines.
func doctorReporting(w io.Writer, st daemon.Status) {
	r := st.Reporting
	if r == nil {
		return
	}
	tag := "[ ok ]"
	switch r.State {
	case daemon.ReportingWorking, daemon.ReportingWaitingForFirst:
	case daemon.ReportingFailing:
		tag = "[warn]"
	default:
		tag = "[info]"
	}
	line := reportingText[r.State]
	if r.HoldReason != "" {
		line += " (" + r.HoldReason + ")"
	}
	fmt.Fprintf(w, "%s %-18s %s; queued %d\n", tag, "reporting", line, r.Queued)
	if r.LastUploadAt != nil {
		fmt.Fprintf(w, "[info] %-18s %s\n", "last upload", r.LastUploadAt.Format(time.RFC3339))
	}
	if r.LastError != "" && r.LastErrorAt != nil {
		fmt.Fprintf(w, "[info] %-18s %s at %s\n", "last upload error", r.LastError, r.LastErrorAt.Format(time.RFC3339))
	}
}
