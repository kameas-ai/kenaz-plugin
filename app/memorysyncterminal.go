package app

import (
	"context"
	"fmt"
	"time"

	"github.com/kameas-ai/kenaz-plugin/internal/daemon"
	"github.com/kameas-ai/kenaz-plugin/internal/memory"
	"github.com/kameas-ai/kenaz-plugin/internal/safetext"
	"github.com/kameas-ai/kenaz-plugin/internal/usertext"
	"github.com/kameas-ai/kenaz-plugin/internal/variant"
	"github.com/kameas-ai/kenaz-plugin/notices"
)

// Turning memory sync on from a real terminal. The person is
// at their own keyboard, reads the notice and answers, as for automatic capture, so no
// approval page is involved. What it does after that
// is the approval executor's code (memory.Service), so nothing reaching Fleet differs.
// With no terminal it refuses: an agent can't answer for the person.

// maxListedTitles bounds the note titles shown before sync's first upload.
const maxListedTitles = 20

// refuseWithoutTerminal prints the command to run and reports whether it refused.
func refuseWithoutTerminal(env Env, args string) bool {
	if memoryStdinTerminal() {
		return false
	}
	fmt.Fprintln(env.Stderr, variant.Current.Binary+": "+usertext.NeedsTerminal(variant.Current.Binary+" memory sync "+args))
	return true
}

func memorySyncEnableTyped(ctx context.Context, env Env, svc *memory.Service) int {
	fail := func(err error) int {
		fmt.Fprintln(env.Stderr, variant.Current.Binary+": "+err.Error())
		return ExitError
	}
	plan, err := svc.PlanTypedEnable(ctx)
	if err != nil {
		return fail(err)
	}
	if refuseWithoutTerminal(env, "enable") {
		return ExitError
	}
	n := notices.MemorySync()
	fmt.Fprintln(env.Stdout, n.Text)
	writeUploadList(env, plan)
	fmt.Fprintln(env.Stdout)
	if !askYes(env, usertext.PromptSyncOn) {
		fmt.Fprintln(env.Stdout, usertext.NothingChanged)
		return ExitOK
	}
	out, err := svc.EnableSyncTyped(ctx, n.ID, plan)
	if err != nil {
		return fail(typedError(memory.OpSyncEnable, err))
	}
	code := printOutcome(env, out, "", true)
	if code == ExitOK {
		startSyncRelay(ctx, env)
	}
	return code
}

func memorySyncAutoOnTyped(ctx context.Context, env Env, svc *memory.Service) int {
	fail := func(err error) int {
		fmt.Fprintln(env.Stderr, variant.Current.Binary+": "+err.Error())
		return ExitError
	}
	if out, ready, err := svc.SaveGrantReady(ctx); err != nil {
		return fail(err)
	} else if !ready {
		return printOutcome(env, out, "", true)
	}
	if refuseWithoutTerminal(env, "auto on") {
		return ExitError
	}
	n := notices.MemorySync()
	fmt.Fprintln(env.Stdout, n.Text)
	fmt.Fprintf(env.Stdout, "\n%s. %s\n", memory.SyncAutoSummary, memory.SyncAutoImpactText())
	fmt.Fprintln(env.Stdout)
	if !askYes(env, usertext.PromptAutoOn) {
		fmt.Fprintln(env.Stdout, usertext.NothingChanged)
		return ExitOK
	}
	out, err := svc.SetSyncAutoTyped(ctx, n.ID)
	if err != nil {
		return fail(typedError(memory.OpSyncAuto, err))
	}
	code := printOutcome(env, out, "", true)
	if code == ExitOK {
		startSyncRelay(ctx, env)
	}
	return code
}

// writeUploadList names what the first upload sends. A title comes from a note, so it
// is reduced to one line of visible text.
func writeUploadList(env Env, plan memory.TypedEnable) {
	if len(plan.Records) == 0 {
		return
	}
	what := fmt.Sprintf("these %d notes", len(plan.Records))
	if len(plan.Records) == 1 {
		what = "this note"
	}
	fmt.Fprintf(env.Stdout, "\nTurning it on uploads %s now. See one with `%s memory show <id>`.\n", what, variant.Current.Binary)
	for i, r := range plan.Records {
		if i == maxListedTitles {
			fmt.Fprintf(env.Stdout, "  and %d more\n", len(plan.Records)-i)
			break
		}
		fmt.Fprintf(env.Stdout, "  %s  %s\n", r.RecordID, safetext.OneLine(r.Title, 80))
	}
	if plan.Remaining > 0 {
		fmt.Fprintf(env.Stdout, "%d more wait: run this command again afterward to send them.\n", plan.Remaining)
	}
}

// typedError keeps Fleet's reason to its code, as the approval executor does: no text
// from a response reaches the terminal.
func typedError(op string, err error) error {
	return fmt.Errorf("nothing changed on this device: %w", memoryExecError(op, err))
}

// startSyncRelay makes sure a relay is running, because the relay is what sends. One
// already running notices within a minute. A failure here costs nothing but time.
func startSyncRelay(ctx context.Context, env Env) {
	r, err := env.resolve("")
	if err == nil {
		rctx, cancel := context.WithTimeout(ctx, 3*time.Second)
		defer cancel()
		_, err = daemon.EnsureRunning(rctx, r.StateDir, r.spawner(env), 3*time.Second)
	}
	if err != nil {
		fmt.Fprintln(env.Stdout, "Notes send the next time your AI tool starts.")
	}
}
