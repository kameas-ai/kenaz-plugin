// Package app is the kenaz entry point shared by both build flavors.
// The public cmd/kenaz passes the stub provider. Official builds pass the
// proprietary Fleet connection adapter.
//
// Subcommands are dispatched with the standard library's flag package. The
// CLI surface is small and fixed, and staying dependency-free keeps the
// public binary's audit surface (and THIRD_PARTY_NOTICES) minimal.
package app

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/signal"
	"sort"
	"syscall"
	"time"

	sdk "github.com/modelcontextprotocol/go-sdk/mcp"

	kenaz "github.com/kameas-ai/kenaz-plugin"
	"github.com/kameas-ai/kenaz-plugin/fleetapi"
	"github.com/kameas-ai/kenaz-plugin/internal/spool"
)

// Version is set at link time: -ldflags "-X .../app.Version=1.2.3".
var Version = "0.0.0-dev"

// Exit codes.
const (
	ExitOK             = 0
	ExitError          = 1
	ExitUnofficial     = 2 // fleetapi.ErrUnofficialBuild
	ExitNotImplemented = 3
	ExitUsage          = 64
)

// Env is what a command sees. The optional fields are test seams; nil
// means the real implementation.
type Env struct {
	Provider fleetapi.Provider
	Stdout   io.Writer
	Stderr   io.Writer
	Ctx      context.Context

	Getenv   func(string) string // nil: os.Getenv
	KeyStore spool.KeyStore      // nil: OS keychain, or files without one
	// KeychainProbe reports whether the OS keychain is usable; an error
	// selects the file credential store. nil: the real probe (skipped when
	// KeyStore is set).
	KeychainProbe func() error
	OpenBrowser   func(url string) error                      // nil: platform opener
	Spawn         func(stateDir string, e fleetapi.Env) error // nil: detached self-exec
	ServeMCP      func(context.Context, *sdk.Server) error    // nil: stdio
	Now           func() time.Time                            // nil: time.Now

	cred *credStore // set once by RunEnv
}

func (e Env) now() time.Time {
	if e.Now != nil {
		return e.Now()
	}
	return time.Now()
}

// Command is one subcommand.
type Command struct {
	Summary string
	Run     func(env Env, args []string) int
}

// commands is the subcommand table. Extend it to add a subcommand.
var commands = map[string]Command{
	"init":     {"Set everything up: PATH, sign-in, and every app found", runInit},
	"login":    {"Sign in to Kameas Fleet", runLogin},
	"logout":   {"Sign out and purge local queued data", runLogout},
	"memory":   {"Manage memories (on this device; optional sync to your Kameas Fleet account)", runMemory},
	"status":   {"Show sign-in, telemetry and consent state (--json)", runStatus},
	"optin":    {"Choose which usage data to share (shows the notice)", runOptin},
	"doctor":   {"Diagnose installation and configuration", runDoctor},
	"daemon":   {"Run the on-demand relay (started by mcp and hooks)", runDaemon},
	"mcp":      {"Run the MCP stdio server (started by the harness)", runMCP},
	"hook":     {"Harness hook entry point (SessionStart/SessionEnd)", runHook},
	"setup":    {"Configure one app (Claude Code or Codex) to use Kameas Fleet", runSetup},
	"skills":   {"List or sync shared Agent Skills from Kameas Fleet", runSkills},
	"device":   {"Show or rename this device's display name", runDevice},
	"licenses": {"Print license and notice texts", runLicenses},
	"version":  {"Print version and build flavor", runVersion},

	// Approving happens only on the approval page, never in the CLI.
	"approvals": {"List write requests and open the approval page", runApprovals},
}

// Main runs the CLI with p and returns the process exit code. SIGINT and
// SIGTERM cancel the command's context (the daemon shuts down cleanly).
func Main(p fleetapi.Provider) int {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	return Run(ctx, p, os.Args[1:], os.Stdout, os.Stderr)
}

// Run is Main with injectable arguments and streams (for tests).
func Run(ctx context.Context, p fleetapi.Provider, args []string, stdout, stderr io.Writer) int {
	return RunEnv(Env{Provider: p, Stdout: stdout, Stderr: stderr, Ctx: ctx}, args)
}

// RunEnv is Run with every test seam available.
func RunEnv(env Env, args []string) int {
	stdout, stderr := env.Stdout, env.Stderr
	if len(args) == 0 || args[0] == "help" || args[0] == "-h" || args[0] == "--help" {
		usage(stdout)
		if len(args) == 0 {
			return ExitUsage
		}
		return ExitOK
	}
	if args[0] == "--version" {
		return runVersion(env, nil)
	}
	cmd, ok := commands[args[0]]
	if !ok {
		fmt.Fprintf(stderr, "kenaz: unknown command %q\n\n", args[0])
		usage(stderr)
		return ExitUsage
	}
	return cmd.Run(env.withCredentialStore(args[0]), args[1:])
}

func usage(w io.Writer) {
	fmt.Fprintln(w, "usage: kenaz <command> [flags]")
	fmt.Fprintln(w)
	names := make([]string, 0, len(commands))
	for n := range commands {
		names = append(names, n)
	}
	sort.Strings(names)
	for _, n := range names {
		fmt.Fprintf(w, "  %-9s %s\n", n, commands[n].Summary)
	}
}

// providerErr maps seam errors to exit codes and prints them.
func providerErr(env Env, err error) int {
	if errors.Is(err, fleetapi.ErrUnofficialBuild) {
		fmt.Fprintln(env.Stderr, "kenaz: "+err.Error())
		return ExitUnofficial
	}
	fmt.Fprintln(env.Stderr, "kenaz: "+err.Error())
	return ExitError
}

func runVersion(env Env, args []string) int {
	if len(args) > 0 && args[0] == "--json" {
		enc := json.NewEncoder(env.Stdout)
		enc.SetIndent("", "  ")
		enc.SetEscapeHTML(false)
		if err := enc.Encode(CurrentBuildInfo(env.Provider)); err != nil {
			return ExitError
		}
		return ExitOK
	}
	fmt.Fprintf(env.Stdout, "kenaz %s (%s build)\n", Version, env.Provider.Flavor())
	return ExitOK
}
func runLicenses(env Env, _ []string) int {
	for _, l := range append([]License{
		{Name: "LICENSE-APACHE (public source)", Text: kenaz.LicenseApache},
		{Name: "NOTICE", Text: kenaz.Notice},
		{Name: "THIRD_PARTY_NOTICES", Text: kenaz.ThirdPartyNotices},
	}, ExtraLicenses...) {
		fmt.Fprintf(env.Stdout, "== %s ==\n%s\n", l.Name, l.Text)
	}
	return ExitOK
}
