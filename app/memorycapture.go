package app

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"
	"runtime"
	"strings"
	"time"

	"github.com/kameas-ai/kenaz-plugin/fleetapi"
	"github.com/kameas-ai/kenaz-plugin/internal/config"
	"github.com/kameas-ai/kenaz-plugin/internal/features"
	"github.com/kameas-ai/kenaz-plugin/internal/hook"
	"github.com/kameas-ai/kenaz-plugin/internal/memory"
	"github.com/kameas-ai/kenaz-plugin/internal/memory/capture"
	"github.com/kameas-ai/kenaz-plugin/internal/usertext"
	"github.com/kameas-ai/kenaz-plugin/internal/variant"
	"github.com/kameas-ai/kenaz-plugin/notices"
)

const captureUsage = `usage:
  kenaz memory capture status [--json]
  kenaz memory capture enable --writer api-key [--model name]
                                         a background model, with your or your
                                         organization's own API key on this device
  kenaz memory capture enable --writer local --command "<program and args>"
                                         a model on this device: the command gets the
                                         request on stdin and prints the answer
  kenaz memory capture enable --writer host-cli [--model name]
                                         a background model through your AI tool's own
                                         CLI, under your own login on this device
  kenaz memory capture enable --writer session-agent
                                         your AI tool's own session saves what it is
                                         reminded to; far fewer notes
  kenaz memory capture disable           turn it off and delete what is queued
  kenaz memory capture exclude [path]    never capture this project (default: the current one)
  kenaz memory capture include [path]    undo exclude
  kenaz memory capture compare [--json]  counts and costs here, next to another memory tool's

The plugin's hooks do nothing until you turn this on: they return without
reading the session. Sessions already open start capturing from then on.
`

// captureSyncNote follows the notice in a build where project notes do not sync, which
// is most of what capture writes. It is empty once they do.
func captureSyncNote() string {
	if features.MemoryProjectScope {
		return ""
	}
	return "In this build, notes that belong to a project stay on this device. Other notes sync if you have memory sync on and let new notes sync without asking (`kenaz memory sync auto on`)."
}

// captureModelLine is the notice's model line for a writer.
func captureModelLine(writer string) string {
	switch writer {
	case capture.WriterSessionAgent:
		return notices.MemoryCaptureSessionAgent
	case capture.WriterAPIKey:
		return notices.MemoryCaptureAPIKey(capture.DefaultProvider)
	case capture.WriterLocal:
		return notices.MemoryCaptureLocal
	}
	return notices.MemoryCaptureHostCLI(capture.DefaultTool, capture.DefaultProvider)
}

// captureAvailable reports whether capture can run here. It stays off on Windows
// until its tests have run there for a release.
func captureAvailable() bool { return features.MemoryCapture && runtime.GOOS != "windows" }

func (mp memoryPaths) captureRoot() capture.Paths { return capture.PathsIn(mp.StateRoot) }

// captureSettings is capture's current settings. Off in a build without the
// feature and while memory as a whole is off.
func (mp memoryPaths) captureSettings() capture.Settings {
	f, err := config.LoadFile(mp.Config)
	if err != nil {
		return capture.Settings{}
	}
	s := capture.LoadSettings(f.Get)
	if !captureAvailable() || !mp.enabled() {
		s.On = false
	}
	return s
}

// kenazEnvVar admits this program's own settings into the worker's environment.
func kenazEnvVar(name string) bool {
	if name == variant.Shared.HomeVar || name == capture.WorkerEnv {
		return true
	}
	for _, v := range variant.Shared.EnvVars {
		if v == name {
			return true
		}
	}
	return false
}

// isCaptureHook reports a capture hook invocation (kenaz hook memory-*).
func isCaptureHook(args []string) bool {
	return len(args) >= 2 && args[0] == "hook" && capture.IsHookEvent(args[1])
}

// captureHost names the host a capture hook runs under, or "" for one capture does
// not cover. The plugin's hooks file is shared, so this comes from the host itself.
func captureHost(env Env) string {
	if hook.DetectHarness(env.getenv) == hook.HarnessClaude {
		return memory.SourceClaudeCode
	}
	return ""
}

// captureHookWanted reports whether this hook has anything to do. The host starts
// these on every prompt and tool call with the session's content on stdin, so when the
// answer is no, the program leaves before it reads that or sets anything up: it looks
// at the environment and the config file.
func captureHookWanted(env Env, event string) bool {
	if !captureAvailable() || captureHost(env) == "" {
		return false
	}
	mp, err := env.memoryPaths()
	if err != nil {
		return false
	}
	s := mp.captureSettings()
	if !s.Active() {
		return false
	}
	return s.Background() || event == capture.HookPrompt || event == capture.HookIndex
}

// runCaptureHook handles a capture hook before any of the CLI's usual set-up:
// it needs no Fleet environment or relay. It always exits 0, and
// writes nothing but a due session-agent reminder.
func runCaptureHook(env Env, args []string) int {
	defer func() { _ = recover() }()
	if !captureAvailable() {
		return ExitOK
	}
	mp, err := env.memoryPaths()
	if err != nil {
		return ExitOK
	}
	host := captureHost(env)
	home, _ := os.UserHomeDir()
	ctx := env.Ctx
	if ctx == nil {
		ctx = context.Background()
	}
	if args[0] == capture.HookIndex {
		return runCaptureIndex(ctx, env, mp)
	}
	text := capture.Handle(ctx, args[0], memoryStdin, capture.HookDeps{
		Paths: mp.captureRoot(), Settings: mp.captureSettings(), Host: host, Home: home, Now: env.now,
		StartWorker: func() error {
			if env.StartCaptureWorker != nil {
				return env.StartCaptureWorker()
			}
			bin, err := os.Executable()
			if err != nil {
				return err
			}
			return capture.StartDetached(bin, []string{"memory", "capture", "worker"}, mp.StateRoot, capture.WorkerEnviron(os.Environ(), mp.captureSettings(), kenazEnvVar))
		},
	})
	if text != "" {
		if b, err := capture.PromptOutput(text); err == nil {
			_, _ = env.Stdout.Write(append(b, '\n'))
		}
	}
	return ExitOK
}

// runCaptureIndex prints the rich session-start index. It does this only while
// capture is on: everyone else keeps the plain index of the session-start hook.
func runCaptureIndex(ctx context.Context, env Env, mp memoryPaths) int {
	if !mp.captureRunning(env.now()) {
		// The plain index of the session-start hook stands in.
		return ExitOK
	}
	cwd := capture.ReadCwd(ctx, memoryStdin)
	if cwd == "" {
		cwd, _ = os.Getwd()
	}
	p, err := memory.ProjectFor(cwd)
	if err != nil {
		return ExitOK
	}
	text := mp.service(nil).RichIndex(ctx, p, env.now())
	if text == "" {
		return ExitOK
	}
	if b, err := capture.SessionStartOutput(text); err == nil {
		_, _ = env.Stdout.Write(append(b, '\n'))
	}
	return ExitOK
}

// captureWorker builds the worker for this machine.
// captureTrialAllowed reports whether the local trial applies: it never does with an
// official provider, whatever the build.
func captureTrialAllowed(p fleetapi.Provider) bool {
	return features.MemoryCaptureLocalTrial && (p == nil || p.Flavor() != fleetapi.FlavorOfficial)
}

func captureWorker(env Env, mp memoryPaths) *capture.Worker {
	home, _ := os.UserHomeDir()
	return &capture.Worker{
		Paths:               mp.captureRoot(),
		Settings:            mp.captureSettings,
		Home:                home,
		Now:                 env.now,
		HostCLIUnverifiedOK: captureTrialAllowed(env.Provider),
		Offered:             env.captureOffered,
		OrgAllows: func(ctx context.Context) (bool, bool) {
			return env.captureOrgAllows(ctx)
		},
		HostCLIAllowed: func(ctx context.Context) (bool, bool) {
			return env.captureCapAllows(ctx, fleetapi.CapMemoryCaptureHostCLIOff(capture.DefaultVendor))
		},
		Open: func(ctx context.Context) (*memory.Store, string, error) {
			p := mp.active(ctx)
			s, err := memory.OpenProfile(memory.ProfilePath(mp.StateRoot, p), p)
			return s, p.Key(), err
		},
	}
}

// runMemoryCapture is `kenaz memory capture ...`.
func runMemoryCapture(ctx context.Context, env Env, mp memoryPaths, args []string) int {
	if !captureAvailable() {
		fmt.Fprintln(env.Stderr, variant.Current.Binary+": automatic memory capture is not available in this build.")
		return ExitError
	}
	if len(args) == 0 {
		fmt.Fprint(env.Stderr, captureUsage)
		return ExitUsage
	}
	switch args[0] {
	case "status":
		return captureStatus(env, mp, args[1:])
	case "enable":
		return captureEnable(env, mp, args[1:])
	case "disable":
		return captureDisable(env, mp)
	case "exclude", "include":
		return captureExclude(env, mp, args[0], args[1:])
	case "compare":
		return captureCompare(ctx, env, mp, args[1:])
	case "worker":
		// Started by the hooks. Failures end up in `capture status`.
		if err := captureWorker(env, mp).Run(ctx); err != nil {
			return ExitError
		}
		return ExitOK
	}
	fmt.Fprint(env.Stderr, captureUsage)
	return ExitUsage
}

func captureStatus(env Env, mp memoryPaths, args []string) int {
	s := mp.captureSettings()
	r := capture.BuildReport(mp.captureRoot(), s, env.now())
	if len(args) == 1 && args[0] == "--json" {
		enc := json.NewEncoder(env.Stdout)
		enc.SetIndent("", "  ")
		_ = enc.Encode(r)
		return ExitOK
	}
	w := env.Stdout
	fmt.Fprintln(w, r.Line())
	if !s.Active() {
		return ExitOK
	}
	if p := mp.active(env.ctx()); true {
		if st, err := memory.OpenProfileReadOnly(memory.ProfilePath(mp.StateRoot, p), p, 2*time.Second); err == nil {
			if st.ErasedClockWasAhead(env.ctx()) {
				fmt.Fprintln(w, "  note:        this device's clock was ahead at your last forget-all, so capture counted from when that was noticed.")
			}
			st.Close()
		}
	}
	if r.Writer != "" {
		fmt.Fprintf(w, "  writer:      %s", r.Writer)
		if r.Model != "" {
			fmt.Fprintf(w, " (model %s)", r.Model)
		}
		fmt.Fprintln(w)
	}
	for _, e := range r.Excluded {
		fmt.Fprintf(w, "  excluded:    %s\n", e)
	}
	if !s.Background() {
		if r.PausedUntil != "" {
			fmt.Fprintf(w, "  paused until: %s (%s)\n", r.PausedUntil, r.PauseReason)
		}
		fmt.Fprintln(w, "  usage:       on your plan with your AI tool, as part of your session")
		return ExitOK
	}
	fmt.Fprintf(w, "  queue:       %d waiting, %d dropped, %d dropped over the daily cap\n", r.Queued, r.Dropped, r.DroppedOverCap)
	fmt.Fprintf(w, "  batches:     %d done, %d failed\n", r.Batches, r.FailedBatches)
	fmt.Fprintf(w, "  notes:       %d saved, %d dropped as credential-like; %d credential shapes redacted from input\n", r.Saved, r.DroppedSecret, r.Redactions)
	fmt.Fprintf(w, "  today:       %d tokens, about $%.4f, %d calls\n", r.Today.Tokens(), r.Today.USD, r.Today.Calls)
	if r.DailyTokens > 0 || r.DailyUSD > 0 {
		fmt.Fprintf(w, "  daily cap:   %d tokens, $%.2f (0: none)\n", r.DailyTokens, r.DailyUSD)
	}
	fmt.Fprintf(w, "  last 30 days: %d tokens, about $%.4f, %d calls\n", r.Last30Days.Tokens(), r.Last30Days.USD, r.Last30Days.Calls)
	if s.Writer == capture.WriterHostCLI {
		fmt.Fprintln(w, "  usage is on:  your AI tool's own login on this machine, and it counts against your plan")
	} else {
		fmt.Fprintln(w, "  usage is on:  your own arrangement with your provider, not in Kameas Fleet usage")
	}
	if r.LastFailure != "" {
		fmt.Fprintf(w, "  last failure: %s at %s\n", r.LastFailure, r.LastFailureAt)
	}
	if r.PausedUntil != "" {
		fmt.Fprintf(w, "  paused until: %s (%s)\n", r.PausedUntil, r.PauseReason)
	}
	return ExitOK
}

func captureEnable(env Env, mp memoryPaths, args []string) int {
	fs := flag.NewFlagSet("memory capture enable", flag.ContinueOnError)
	fs.SetOutput(env.Stderr)
	writer := fs.String("writer", "", "api-key, local, host-cli or session-agent")
	command := fs.String("command", "", "program and args for --writer local")
	model := fs.String("model", "", "model name (default "+capture.DefaultModel+")")
	if err := fs.Parse(args); err != nil || fs.NArg() != 0 {
		return ExitUsage
	}
	switch *writer {
	case capture.WriterSessionAgent, capture.WriterAPIKey, capture.WriterHostCLI:
	case capture.WriterLocal:
		if strings.TrimSpace(*command) == "" {
			fmt.Fprint(env.Stderr, captureUsage)
			return ExitUsage
		}
	default:
		fmt.Fprint(env.Stderr, captureUsage)
		return ExitUsage
	}
	if !mp.enabled() {
		fmt.Fprintln(env.Stderr, variant.Current.Binary+": "+memory.OffMessage)
		return ExitError
	}
	if why := env.captureRefusal(env.ctx(), *writer); why != "" {
		fmt.Fprintln(env.Stderr, variant.Current.Binary+": "+why)
		return ExitError
	}
	hostCLIConfirmed := false
	if *writer == capture.WriterHostCLI && !captureTrialAllowed(env.Provider) {
		allowed, known := env.captureCapAllows(env.ctx(), fleetapi.CapMemoryCaptureHostCLIOff(capture.DefaultVendor))
		if !known || !allowed {
			// It runs only on Fleet's positive answer. Not knowing is a no.
			fmt.Fprintln(env.Stderr, variant.Current.Binary+": this writer is not available right now. Use --writer api-key with your own API key, or --writer local with a model on this device.")
			return ExitError
		}
		hostCLIConfirmed = true
	}
	if *writer == capture.WriterAPIKey && !capture.ValidKeyEnv(capture.LoadSettings(func(k string) string {
		f, err := config.LoadFile(mp.Config)
		if err != nil {
			return ""
		}
		return f.Get(k)
	}).APIKeyEnv) {
		fmt.Fprintln(env.Stderr, variant.Current.Binary+": "+capture.KeyAPIKeyEnv+" must name an API key, not a login token.")
		return ExitError
	}
	fmt.Fprintln(env.Stdout, notices.MemoryCapture(captureModelLine(*writer)).Text)
	if n := captureSyncNote(); n != "" {
		fmt.Fprintln(env.Stdout, n)
	}
	if !memoryStdinTerminal() {
		fmt.Fprintln(env.Stderr, variant.Current.Binary+": run this in a terminal: turning capture on needs you to read the text above.")
		return ExitError
	}
	fmt.Fprintln(env.Stdout)
	yes := askYes(env, usertext.PromptCaptureOn)
	old := mp.captureSettings()
	if !yes {
		// Nothing is recorded. A writer already chosen stays.
		if old.Active() {
			fmt.Fprintf(env.Stdout, "Nothing changed: %s still writes the notes.\n", old.Writer)
		} else {
			fmt.Fprintln(env.Stdout, "Capture stays off. "+usertext.NothingChanged)
		}
		return ExitOK
	}
	if err := applyCapture(env, mp, *writer, *command, *model, hostCLIConfirmed); err != nil {
		fmt.Fprintln(env.Stderr, variant.Current.Binary+": "+err.Error())
		return ExitError
	}
	if err := env.recordCaptureOn(env.ctx(), mp); err != nil {
		fmt.Fprintln(env.Stderr, variant.Current.Binary+": "+err.Error())
		return ExitError
	}
	fmt.Fprintf(env.Stdout, "Capture is on: %s writes the notes. Turn it off with `kenaz memory capture disable`.\n", *writer)
	if *writer == capture.WriterAPIKey {
		key := env.getenv(mp.captureSettings().APIKeyEnv)
		if key == "" {
			fmt.Fprintf(env.Stdout, "%s is not set here. Capture needs it, or cloud credentials, in the session's environment, and stays paused without them. It never falls back to a signed-in account.\n", mp.captureSettings().APIKeyEnv)
		}
	}
	return ExitOK
}

// applyCapture records the member's yes: it turns capture on with writer.
func applyCapture(env Env, mp memoryPaths, writer, command, model string, hostCLIConfirmed bool) error {
	old := mp.captureSettings()
	// Moving between writers loses nothing: what the old writer has waiting is
	// written with the old writer first, under the line the member saw for it.
	if old.Background() && old.Writer != writer {
		ctx, cancel := context.WithTimeout(env.ctx(), captureFlushTimeout)
		defer cancel()
		if err := captureWorker(env, mp).Flush(ctx); err != nil {
			fmt.Fprintln(env.Stdout, "Some queued activity could not be written before the change; it was deleted.")
		}
	}
	if hostCLIConfirmed {
		if err := capture.RecordHostCLIAnswer(mp.captureRoot(), env.now()); err != nil {
			return err
		}
	}
	values := [][2]string{
		{capture.KeyWriter, writer}, {capture.KeyAck, capture.AckVersion}, {capture.KeySwitch, "on"},
	}
	if command != "" {
		values = append(values, [2]string{capture.KeyCommand, command})
	}
	if model != "" {
		values = append(values, [2]string{capture.KeyModel, model})
	}
	for _, kv := range values {
		if err := config.SetValue(mp.Config, kv[0], kv[1]); err != nil {
			return err
		}
	}
	return nil
}

// captureFlushTimeout bounds how long a change of writer waits to write what
// the old writer has queued.
const captureFlushTimeout = 3 * time.Minute

func (e Env) ctx() context.Context {
	if e.Ctx != nil {
		return e.Ctx
	}
	return context.Background()
}

func captureDisable(env Env, mp memoryPaths) int {
	if err := config.SetValue(mp.Config, capture.KeySwitch, "off"); err != nil {
		fmt.Fprintln(env.Stderr, variant.Current.Binary+": "+err.Error())
		return ExitError
	}
	err := capture.PurgeQueue(mp.captureRoot())
	if err != nil {
		fmt.Fprintln(env.Stderr, variant.Current.Binary+": "+err.Error())
		return ExitError
	}
	fmt.Fprintln(env.Stdout, "Capture is off, and what was queued was deleted. Notes already saved were kept; delete them with `kenaz memory forget` or `kenaz memory purge`.")
	return ExitOK
}

func captureExclude(env Env, mp memoryPaths, verb string, args []string) int {
	dir := ""
	if len(args) == 1 {
		dir = args[0]
	} else if len(args) > 1 {
		fmt.Fprint(env.Stderr, captureUsage)
		return ExitUsage
	} else {
		dir, _ = os.Getwd()
	}
	proj, err := memory.ProjectFor(dir)
	// The checkout named, so that a worktree can be left out on its own.
	root := proj.Checkout
	if err == nil && strings.Contains(root, ",") {
		err = errors.New("a path with a comma cannot be listed; put an empty " + capture.ExclusionMarker + " file in the project instead")
	}
	if err != nil {
		fmt.Fprintln(env.Stderr, variant.Current.Binary+": "+err.Error())
		return ExitError
	}
	f, err := config.LoadFile(mp.Config)
	if err != nil {
		fmt.Fprintln(env.Stderr, variant.Current.Binary+": "+err.Error())
		return ExitError
	}
	var next []string
	for _, e := range capture.LoadSettings(f.Get).Exclude {
		if e != root {
			next = append(next, e)
		}
	}
	if verb == "exclude" {
		next = append(next, root)
	}
	if err := config.SetValue(mp.Config, capture.KeyExclude, capture.JoinList(next)); err != nil {
		fmt.Fprintln(env.Stderr, variant.Current.Binary+": "+err.Error())
		return ExitError
	}
	if verb == "exclude" {
		fmt.Fprintf(env.Stdout, "Capture will skip %s.\n", root)
	} else {
		fmt.Fprintf(env.Stdout, "Capture no longer skips %s.\n", root)
	}
	return ExitOK
}

// captureOrgAllows asks Fleet whether the member's organization allows capture
// (a condition of the legal ruling). known is false while signed out or offline. It is called
// by the worker and at opt-in, never by a hook.
func (e Env) captureOrgAllows(ctx context.Context) (allowed, known bool) {
	return e.captureCapAllows(ctx, fleetapi.CapMemoryCaptureOff)
}

// captureOffered says whether capture may run for the active profile, and names it.
// Signed in, Kameas Fleet must offer it to the organization: memory_capture_kinds
// present and true. A failed read and an absent key are both a no (fail closed).
// Signed out there is no organization: the api-key and local writers, which reach
// only the member's own provider or nothing, may run, if capture was turned on
// signed out. An unofficial build hears no answer and doesn't run capture. It is
// called by the worker and at opt-in, never by a hook.
func (e Env) captureOffered(ctx context.Context) (offered bool, reason, profile string) {
	if !captureAvailable() || e.Provider == nil || e.Provider.Flavor() != fleetapi.FlavorOfficial {
		return false, capture.PauseUnconfirmed, ""
	}
	p := memoryProfileOf(ctx, e.Provider)
	if p.IsLocal() {
		mp, err := e.memoryPaths()
		if err != nil {
			return false, capture.PauseUnconfirmed, p.Key()
		}
		if capture.SignedOutWriter(mp.captureSettings().Writer) && capture.LoadState(mp.captureRoot()).EnabledFor == p.Key() {
			return true, "", p.Key()
		}
		return false, capture.PauseSignedOutWriter, p.Key()
	}
	caps, err := e.Provider.Me().Capabilities(ctx)
	if err != nil {
		return false, capture.PauseUnconfirmed, p.Key()
	}
	if !fleetapi.HasKey(caps, fleetapi.CapMemoryCaptureKinds) {
		return false, capture.PauseNotOffered, p.Key()
	}
	return true, "", p.Key()
}

// captureRefusal is why capture can't be turned on, or keep running, with writer
// now, or "" when it can.
func (e Env) captureRefusal(ctx context.Context, writer string) string {
	if !captureAvailable() || e.Provider == nil || e.Provider.Flavor() != fleetapi.FlavorOfficial {
		return usertext.CaptureNeedsOfficialBuild
	}
	if memoryProfileOf(ctx, e.Provider).IsLocal() {
		if capture.SignedOutWriter(writer) {
			return ""
		}
		return usertext.CaptureSignedOutWriters
	}
	switch offered, reason, _ := e.captureOffered(ctx); {
	case reason == capture.PauseNotOffered:
		return usertext.CaptureNotOffered
	case !offered:
		return usertext.CaptureUnconfirmedForOrg
	}
	if allowed, known := e.captureOrgAllows(ctx); known && !allowed {
		return usertext.CaptureOrgOff
	}
	return ""
}

// recordCaptureOn keeps, with the member's yes, the profile capture was turned on
// under and the answer just heard for it.
func (e Env) recordCaptureOn(ctx context.Context, mp memoryPaths) error {
	key := memoryProfileOf(ctx, e.Provider).Key()
	if err := capture.RecordEnabled(mp.captureRoot(), key); err != nil {
		return err
	}
	return capture.RecordOffered(mp.captureRoot(), e.now(), key)
}

// captureRunning reports that capture is on and Kameas Fleet's answer that it is
// offered is current: only then do the hooks queue, the session-start index carry
// capture's summary, or the session agent's saves count as automatic notes. It reads
// files only.
func (mp memoryPaths) captureRunning(now time.Time) bool {
	return mp.captureSettings().Active() && capture.LoadState(mp.captureRoot()).Offered(now)
}

// captureCapAllows reads one of capture's off switches from Fleet. A signed-out
// device has no way to hear one.
func (e Env) captureCapAllows(ctx context.Context, off string) (allowed, known bool) {
	if e.Provider == nil || memoryProfileOf(ctx, e.Provider).IsLocal() {
		return true, false
	}
	caps, err := e.Provider.Me().Capabilities(ctx)
	if err != nil {
		return true, false
	}
	// Case does not matter, as on Fleet.
	for k, on := range caps.Keys {
		if on && strings.EqualFold(k, off) {
			return false, true
		}
	}
	return true, true
}

// captureLine is the capture line of `kenaz status`: nothing in a build
// without the feature.
func (mp memoryPaths) captureLine(now time.Time) string {
	if !captureAvailable() {
		return ""
	}
	return capture.BuildReport(mp.captureRoot(), mp.captureSettings(), now).Line()
}

// purgeCaptureQueue removes what is queued. It is safe in any build.
func (mp memoryPaths) purgeCaptureQueue() error { return capture.PurgeQueue(mp.captureRoot()) }
