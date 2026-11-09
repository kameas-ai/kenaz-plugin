package app

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/mattn/go-isatty"

	"github.com/kameas-ai/kenaz-plugin/fleetapi"
	"github.com/kameas-ai/kenaz-plugin/internal/config"
	"github.com/kameas-ai/kenaz-plugin/internal/daemon"
	"github.com/kameas-ai/kenaz-plugin/internal/memory"
	"github.com/kameas-ai/kenaz-plugin/internal/memory/capture"
	"github.com/kameas-ai/kenaz-plugin/internal/memory/memimport"
	"github.com/kameas-ai/kenaz-plugin/internal/memory/memsync"
	"github.com/kameas-ai/kenaz-plugin/internal/usertext"
	"github.com/kameas-ai/kenaz-plugin/internal/variant"
	"github.com/kameas-ai/kenaz-plugin/notices"
)

// `kenaz memory on` is the one thing a person needs: one notice, one question, and
// capture, sync, automatic sync and earlier notes are all set up. The individual
// commands stay for scripting and run the same code.

// hostCLICommand is the program the host-cli writer needs on PATH. A var for tests.
var hostCLICommand = "claude"

// memoryOnPlan is what the one question covers.
type memoryOnPlan struct {
	writer      string
	captureSkip string // why capture won't turn on, in one sentence
	sync        memory.TypedEnable
	syncOn      bool // sync will be turned on
	syncSkip    string
	autoAlready bool
	imp         *memimport.Plan
}

func memoryOn(ctx context.Context, env Env, mp memoryPaths) int {
	if !memoryStdinTerminal() {
		fmt.Fprintln(env.Stderr, variant.Current.Binary+": "+usertext.NeedsTerminal(variant.Current.Binary+" memory on"))
		return ExitError
	}
	svc := mp.service(cliSubmitter(env))
	plan := planMemoryOn(ctx, env, mp, svc)
	// Nothing a yes would change: say where things stand, and ask nothing.
	if plan.writer == "" && !plan.syncOn && plan.imp == nil && (plan.autoAlready || plan.syncSkip != "") {
		for _, s := range []string{plan.captureSkip, plan.syncSkip} {
			if s != "" {
				fmt.Fprintln(env.Stdout, s)
			}
		}
		if plan.syncSkip != "" && !mp.captureSettings().Active() {
			return ExitError
		}
		fmt.Fprintln(env.Stdout, usertext.MemoryAlreadyOn)
		allowImportUpload(ctx, mp)
		// Nothing to ask doesn't mean the right relay is the one running: a line that says
		// close your tools and run this again sends people here.
		if plan.syncSkip == "" {
			if line := refreshRelay(ctx, env); line != "" {
				fmt.Fprintln(env.Stdout, line)
			}
		}
		if st, _, _, err := svc.SyncStatus(ctx); err == nil && st.QuotaReached {
			fmt.Fprintln(env.Stdout, usertext.QuotaReached)
		}
		return ExitOK
	}
	writeMemoryOnNotice(env, mp, plan)
	if !askYes(env, usertext.PromptMemoryOn) {
		fmt.Fprintln(env.Stdout, usertext.NothingChanged)
		return ExitOK
	}
	return applyMemoryOn(ctx, env, mp, svc, plan)
}

func planMemoryOn(ctx context.Context, env Env, mp memoryPaths, svc *memory.Service) memoryOnPlan {
	var plan memoryOnPlan
	plan.writer, plan.captureSkip = pickWriter(ctx, env, mp)

	p := mp.active(ctx)
	switch {
	case p.IsLocal():
		plan.syncSkip = usertext.SignedOutSkip
	case memoryRemote(mp.p) == nil:
		plan.syncSkip = usertext.NoSyncInBuild
	default:
		st, _, _, err := svc.SyncStatus(ctx)
		switch {
		case err != nil:
			plan.syncSkip = usertext.SyncUnreadable
		default:
			plan.autoAlready = st.Enabled && st.SaveGrant
			t, err := svc.PlanTypedEnable(ctx)
			if errors.Is(err, memory.ErrProjectScopeNotOffered) {
				plan.syncSkip = usertext.ProjectNotesNotOffered
				break
			}
			if err != nil {
				plan.syncSkip = usertext.SyncUnreadable
				break
			}
			// A re-run finishes what an earlier one left, such as batches not sent. Once
			// the standing grant is on, it takes care of automatic notes by itself.
			if !st.Enabled || !st.SaveGrant || savedOnPurpose(t) {
				if st.Enabled && len(t.Records) == 0 {
					break
				}
				plan.sync, plan.syncOn = t, true
			}
		}
	}
	if captureAvailable() && (plan.writer != "" || mp.captureSettings().Active()) && plan.captureSkip == "" {
		plan.imp = planEarlierNotes(ctx, env, mp)
	}
	return plan
}

// savedOnPurpose says a plan holds a note saved on purpose. Those come first in a plan.
func savedOnPurpose(t memory.TypedEnable) bool {
	for _, r := range t.Records {
		if !memory.IsAuto(r.Kind) {
			return true
		}
	}
	return false
}

// pickWriter chooses how notes get written. A writer already chosen stays; otherwise the
// member's own login, when that program is here. It returns "" and a sentence when
// capture won't change.
func pickWriter(ctx context.Context, env Env, mp memoryPaths) (string, string) {
	if !captureAvailable() {
		return "", usertext.NoCaptureInBuild
	}
	// Capture runs only while Kameas Fleet offers it to the organization, and the
	// organization hasn't turned it off. That holds for a writer already chosen too.
	writer := capture.WriterHostCLI // what `memory on` turns on
	if s := mp.captureSettings(); s.Active() {
		writer = s.Writer
	}
	if why := env.captureRefusal(ctx, writer); why != "" {
		return "", why
	}
	if mp.captureSettings().Active() {
		if offered, _, key := env.captureOffered(ctx); offered {
			_ = capture.RecordOffered(mp.captureRoot(), env.now(), key)
		}
		return "", ""
	}
	if _, err := exec.LookPath(hostCLICommand); err != nil {
		return "", usertext.CaptureNeedsTool(hostCLICommand)
	}
	if !captureTrialAllowed(env.Provider) {
		allowed, known := env.captureCapAllows(ctx, fleetapi.CapMemoryCaptureHostCLIOff(capture.DefaultVendor))
		// It runs only on Fleet's positive answer. Not knowing is a no, and says so.
		if !known {
			return "", usertext.CaptureUnconfirmed
		}
		if !allowed {
			return "", usertext.CaptureSwitchedOff
		}
	}
	return capture.WriterHostCLI, ""
}

// planEarlierNotes works out what would come over from the other memory tool, with the
// defaults. No database, or nothing new in it, means no plan.
func planEarlierNotes(ctx context.Context, env Env, mp memoryPaths) *memimport.Plan {
	db := otherMemoryDB()
	if db == "" {
		return nil
	}
	if _, err := os.Stat(db); err != nil {
		return nil
	}
	fmt.Fprintln(env.Stdout, usertext.CheckingEarlierNotes)
	var roots []string
	if cwd, err := os.Getwd(); err == nil {
		roots = []string{cwd, filepath.Dir(cwd)}
	}
	plan, err := prepareImport(ctx, env, mp, db, memimport.DefaultDays, 0, 0, roots)
	if err != nil || plan.Total() == 0 {
		return nil
	}
	return &plan
}

func writeMemoryOnNotice(env Env, mp memoryPaths, plan memoryOnPlan) {
	w := env.Stdout
	if plan.writer != "" {
		fmt.Fprintln(w, notices.MemoryCapture(captureModelLine(plan.writer)).Text)
		if n := captureSyncNote(); n != "" {
			fmt.Fprintln(w, n)
		}
		fmt.Fprintln(w)
	}
	if plan.syncOn {
		fmt.Fprintln(w, notices.MemorySync().Text)
		fmt.Fprintln(w)
	}
	if plan.syncOn || !plan.autoAlready {
		fmt.Fprintf(w, "%s. %s\n\n", memory.SyncAutoSummary, memory.SyncAutoImpactText())
	}
	var does []string
	if plan.writer != "" {
		does = append(does, usertext.CaptureWith(capture.DefaultTool))
	}
	if plan.syncOn {
		does = append(does, usertext.SyncUploads(len(plan.sync.Records)+plan.sync.Remaining))
	}
	if plan.imp != nil {
		does = append(does, usertext.ImportTotals(plan.imp.Total()))
		if plan.imp.OverBudget > 0 {
			does = append(does, usertext.ImportLeftOut(plan.imp.OverBudget))
		}
	}
	for _, d := range does {
		fmt.Fprintln(w, d)
	}
	for _, s := range []string{plan.captureSkip, plan.syncSkip} {
		if s != "" {
			fmt.Fprintln(w, s)
		}
	}
}

func applyMemoryOn(ctx context.Context, env Env, mp memoryPaths, svc *memory.Service, plan memoryOnPlan) int {
	if !mp.enabled() {
		if err := config.SetValue(mp.Config, memoryConfigKey, "on"); err != nil {
			fmt.Fprintln(env.Stderr, variant.Current.Binary+": "+err.Error())
			return ExitError
		}
	}
	var done, problems []string
	syncIsOn := false
	if plan.writer != "" {
		if err := applyCapture(env, mp, plan.writer, "", "", plan.writer == capture.WriterHostCLI && !captureTrialAllowed(env.Provider)); err != nil {
			problems = append(problems, usertext.CaptureDidntStart)
		} else if err := env.recordCaptureOn(ctx, mp); err != nil {
			problems = append(problems, usertext.CaptureDidntStart)
		} else {
			done = append(done, "capture")
		}
	} else if mp.captureSettings().Active() && plan.captureSkip == "" {
		done = append(done, "capture")
	}
	if plan.syncOn {
		on, why := turnOnAllSync(ctx, svc, plan.sync)
		if why != "" {
			problems = append(problems, why)
		}
		if on {
			done = append(done, "sync")
		}
		syncIsOn = on
	} else if plan.syncSkip == "" {
		done = append(done, "sync")
		syncIsOn = true
	}
	if syncIsOn && !plan.autoAlready {
		if _, err := svc.SetSyncAutoTyped(ctx, notices.MemorySync().ID); err != nil {
			problems = append(problems, usertext.AutoDidntStart)
		}
	}
	imported, skipped := 0, 0
	if plan.imp != nil && mp.captureSettings().Active() && plan.captureSkip == "" {
		res, err := applyEarlierNotes(ctx, env, mp, *plan.imp)
		if err != nil {
			problems = append(problems, usertext.ImportDidntFinish)
		}
		imported, skipped = res.Saved, res.Skipped
	}
	allowImportUpload(ctx, mp)
	relayLine := refreshRelay(ctx, env)
	for _, s := range problems {
		fmt.Fprintln(env.Stdout, s)
	}
	if len(done) == 0 {
		fmt.Fprintln(env.Stdout, usertext.NothingChanged)
		return ExitError
	}
	fmt.Fprintln(env.Stdout, usertext.MemoryOnResult(done, imported, skipped))
	if st, _, _, err := svc.SyncStatus(ctx); err == nil && st.QuotaReached {
		fmt.Fprintln(env.Stdout, usertext.QuotaReached)
	}
	if plan.writer != "" {
		fmt.Fprintln(env.Stdout, usertext.RestartForHooks)
	}
	if relayLine != "" {
		fmt.Fprintln(env.Stdout, relayLine)
	}
	if len(problems) > 0 {
		return ExitError
	}
	return ExitOK
}

// turnOnAllSync turns sync on with every note waiting: a person said yes once, so it
// runs as many batches as it takes. It says whether sync is on, and in one sentence what
// went wrong, if anything did.
func turnOnAllSync(ctx context.Context, svc *memory.Service, first memory.TypedEnable) (bool, string) {
	id := notices.MemorySync().ID
	plan := first
	for i := 0; ; i++ {
		out, err := svc.EnableSyncTyped(ctx, id, plan)
		if err != nil {
			if i > 0 {
				return true, usertext.SyncPartly
			}
			return false, usertext.SyncRefusal(fleetRefusalCode(err))
		}
		if plan.Remaining == 0 || out.Queued == 0 || i > 1000 {
			return true, ""
		}
		if plan, err = svc.PlanTypedEnable(ctx); err != nil {
			return true, usertext.SyncPartly
		}
	}
}

// fleetRefusalCode is Fleet's reason for an error, or "" when there is none to give.
func fleetRefusalCode(err error) string {
	if c := memory.ErrorCode(err); c != "" {
		return c
	}
	if errors.Is(err, memsync.ErrNotInTier) {
		return "capability_not_in_tier"
	}
	if _, paused := fleetapi.OrgPaused(err); paused {
		return "org_paused"
	}
	return fleetapi.APICode(err)
}

func applyEarlierNotes(ctx context.Context, env Env, mp memoryPaths, plan memimport.Plan) (memimport.Result, error) {
	p := mp.active(ctx)
	st, err := memory.OpenProfile(memory.ProfilePath(mp.StateRoot, p), p)
	if err != nil {
		return memimport.Result{}, err
	}
	defer st.Close()
	line := newProgressLine(env)
	defer line.finish()
	line.update(usertext.ImportingNotes(0, plan.Total()))
	return memimport.ApplyEach(ctx, st, plan, func(done, total int) {
		if done%importProgressEvery == 0 || done == total {
			line.update(usertext.ImportingNotes(done, total))
		}
	})
}

// allowImportUpload lets imported notes upload as fast as Kameas Fleet takes them. Call it
// only from a command that checked for a real terminal. If it fails, they upload anyway,
// within the hourly cap.
func allowImportUpload(ctx context.Context, mp memoryPaths) {
	p := mp.active(ctx)
	if p.IsLocal() {
		return
	}
	st, err := memory.OpenProfile(memory.ProfilePath(mp.StateRoot, p), p)
	if err != nil {
		return
	}
	defer st.Close()
	_ = st.AllowImportUpload(ctx)
}

// importProgressEvery is how many notes pass between updates of the progress line.
const importProgressEvery = 100

// progressLine keeps one line up to date on a terminal. Anywhere else it writes it once,
// so that a log doesn't fill up.
type progressLine struct {
	w     io.Writer
	tty   bool
	shown bool
}

func newProgressLine(env Env) *progressLine {
	p := &progressLine{w: env.Stdout}
	if f, ok := env.Stdout.(*os.File); ok {
		p.tty = isatty.IsTerminal(f.Fd())
	}
	return p
}

func (p *progressLine) update(s string) {
	switch {
	case p.tty:
		fmt.Fprintf(p.w, "\r%s\x1b[K", s)
	case !p.shown:
		fmt.Fprintln(p.w, s)
	}
	p.shown = true
}

func (p *progressLine) finish() {
	if p.tty && p.shown {
		fmt.Fprintln(p.w)
	}
}

// refreshRelay makes sure this build's relay is the one running, because the relay sends
// the notes. An older one is stopped and started again without touching the AI tools. An
// open session's older `kenaz mcp` can start its own again in the gap, so this checks what
// answers afterward and tries once more. It returns a line only if something is left to say.
func refreshRelay(ctx context.Context, env Env) string {
	r, err := env.resolve("")
	if err != nil {
		// Another build's relay is its own to stop.
		var other *daemon.OtherBuildError
		if errors.As(err, &other) {
			return usertext.RelayOtherBuild
		}
		return usertext.RelayNextSession
	}
	rctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	current := func(c *daemon.Client) bool {
		return c.Info.Version == Version && c.Info.Build() == variant.Current.Name
	}
	for try := 0; try < 2; try++ {
		if c, err := daemon.ConnectAny(rctx, r.StateDir); err == nil && !current(c) {
			if err := c.Shutdown(rctx, false); err != nil {
				continue
			}
			// Its lock takes a moment to free.
			for end := time.Now().Add(3 * time.Second); time.Now().Before(end); time.Sleep(50 * time.Millisecond) {
				if _, err := daemon.ConnectAny(rctx, r.StateDir); err != nil {
					break
				}
			}
		}
		if _, err := daemon.EnsureRunning(rctx, r.StateDir, r.spawner(env), 3*time.Second); err != nil {
			break
		}
		if c, err := daemon.ConnectAny(rctx, r.StateDir); err == nil && current(c) {
			return ""
		}
	}
	c, err := daemon.ConnectAny(rctx, r.StateDir)
	switch {
	case err != nil:
		return usertext.RelayNextSession
	case c.Info.Build() != variant.Current.Name:
		return usertext.RelayOtherBuild
	case c.Info.Version != Version:
		return usertext.RelayStillOld(c.Info.Version)
	}
	return ""
}

// memoryOff stops capture and sync. It needs no terminal: it only reduces what leaves the
// device. Notes stay.
func memoryOff(ctx context.Context, env Env, mp memoryPaths) int {
	failed := false
	if captureAvailable() && mp.captureSettings().Active() {
		if err := config.SetValue(mp.Config, capture.KeySwitch, "off"); err != nil {
			failed = true
		} else if err := capture.PurgeQueue(mp.captureRoot()); err != nil {
			failed = true
		}
	}
	extra := ""
	if !mp.active(ctx).IsLocal() {
		out, err := mp.service(nil).DisableSync(ctx)
		if err != nil && !errors.Is(err, memory.ErrNoStore) {
			failed = true
		}
		extra = strings.TrimSpace(strings.TrimPrefix(out.Message, memory.SyncOffMessage))
	}
	if !failed {
		fmt.Fprintln(env.Stdout, usertext.MemoryOff)
	}
	if extra != "" {
		fmt.Fprintln(env.Stdout, extra)
	}
	if failed {
		fmt.Fprintln(env.Stderr, variant.Current.Binary+": "+usertext.MemoryOffFailed)
		return ExitError
	}
	return ExitOK
}
