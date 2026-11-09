package app

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"strings"
	"time"

	"github.com/kameas-ai/kenaz-plugin/internal/approvals"
	"github.com/kameas-ai/kenaz-plugin/internal/daemon"
	"github.com/kameas-ai/kenaz-plugin/internal/memory"
	"github.com/kameas-ai/kenaz-plugin/internal/safetext"
	"github.com/kameas-ai/kenaz-plugin/internal/usertext"
	"github.com/kameas-ai/kenaz-plugin/internal/variant"
)

// requestLister is what the commands need from the relay. Reading only: approving goes
// through the page routes.
type requestLister interface {
	List(ctx context.Context) (approvals.WireList, error)
	Status(ctx context.Context, requestID string) (approvals.Status, []byte, error)
}

// relayRequests reaches the relay, starting one if needed. A var for tests.
var relayRequests = func(env Env, r resolved) requestLister {
	spawn := r.spawner(env)
	return approvalsClient(func(ctx context.Context) *daemon.Client {
		c, err := daemon.EnsureRunning(ctx, r.StateDir, spawn, 3*time.Second)
		if err != nil {
			return nil
		}
		return c
	})
}

func runApprove(env Env, args []string) int { return runDecide(env, "approve", args) }
func runDecline(env Env, args []string) int { return runDecide(env, "decline", args) }

// runDecide is `kenaz approve [id]` and `kenaz decline [id]`. It shows the exact content
// and asks once. With no id and one request waiting, that one; with several, a list. It
// needs a real terminal, so an agent's shell call without one is refused.
func runDecide(env Env, verb string, args []string) int {
	fs := flag.NewFlagSet(verb, flag.ContinueOnError)
	fs.SetOutput(env.Stderr)
	envName := fs.String("env", "", "Fleet environment")
	if err := fs.Parse(args); err != nil {
		return ExitUsage
	}
	id := ""
	switch fs.NArg() {
	case 0:
	case 1:
		id = fs.Arg(0)
		if !approvals.ValidID(id) {
			fmt.Fprintln(env.Stderr, variant.Current.Binary+": not a request id: "+id)
			return ExitUsage
		}
	default:
		fmt.Fprintf(env.Stderr, "usage: %s %s [request_id]\n", variant.Current.Binary, verb)
		return ExitUsage
	}
	if !memoryStdinTerminal() {
		fmt.Fprintln(env.Stderr, variant.Current.Binary+": "+usertext.NeedsTerminal(strings.TrimSpace(variant.Current.Binary+" "+verb+" "+id)))
		return ExitError
	}
	r, err := env.resolve(*envName)
	if err != nil {
		fmt.Fprintln(env.Stderr, variant.Current.Binary+": "+err.Error())
		return ExitUsage
	}
	ctx := env.ctx()
	rc := relayRequests(env, r)
	l, err := rc.List(ctx)
	if err != nil {
		if errors.Is(err, approvals.ErrNotSignedIn) {
			fmt.Fprintln(env.Stderr, variant.Current.Binary+": not signed in; run `kenaz login`")
			return ExitError
		}
		fmt.Fprintln(env.Stderr, variant.Current.Binary+": the Kenaz relay could not be reached: "+err.Error())
		return ExitError
	}
	if id == "" {
		var open []approvals.Info
		for _, q := range l.Requests {
			if q.Status == approvals.StatusPending {
				open = append(open, q)
			}
		}
		switch len(open) {
		case 0:
			fmt.Fprintln(env.Stdout, "Nothing is waiting.")
			return ExitOK
		case 1:
			id = open[0].RequestID
		default:
			fmt.Fprintf(env.Stdout, "%d are waiting. Run `%s %s <request>` for one of these:\n", len(open), variant.Current.Binary, verb)
			for _, q := range open {
				fmt.Fprintf(env.Stdout, "  %s  %s\n", q.RequestID, q.Operation)
			}
			return ExitOK
		}
	}
	page := &approvals.PageClient{PageURL: l.PageURL}
	v, err := page.View(ctx, id)
	if err != nil {
		fmt.Fprintln(env.Stderr, variant.Current.Binary+": "+err.Error())
		return ExitError
	}
	if v.Status != approvals.StatusPending {
		fmt.Fprintf(env.Stdout, "Not waiting: this request is %s.\n", v.Status)
		return ExitOK
	}
	writeRequest(env, v)
	prompt := usertext.PromptApprove
	switch {
	case verb == "decline":
		prompt = usertext.PromptDecline
	case v.Operation == memory.OpForgetAll:
		prompt = usertext.PromptForgetAll
	}
	if !askYes(env, prompt) {
		fmt.Fprintln(env.Stdout, usertext.NothingChanged)
		return ExitOK
	}
	if verb == "decline" {
		if err := page.Deny(ctx, id); err != nil {
			fmt.Fprintln(env.Stderr, variant.Current.Binary+": "+err.Error())
			return ExitError
		}
		fmt.Fprintln(env.Stdout, "Declined. Nothing was written.")
		return ExitOK
	}
	st, err := page.Approve(ctx, id)
	if err != nil {
		fmt.Fprintln(env.Stderr, variant.Current.Binary+": "+err.Error())
		return ExitError
	}
	return writeApproved(ctx, env, rc, id, st)
}

// writeRequest shows what the page shows: what will happen, and the exact content.
// approvalText makes text safe to print above the question, and short enough that blank
// lines can't push the part that matters off the screen. A long run of them is named
// rather than dropped, so what's shown still says what's there.
func approvalText(s string) string {
	lines := strings.Split(safetext.Terminal(s), "\n")
	out := make([]string, 0, len(lines))
	blank := 0
	flush := func() {
		if blank > maxBlankLinesShown {
			out = append(out, usertext.BlankLines(blank))
		} else {
			for i := 0; i < blank; i++ {
				out = append(out, "")
			}
		}
		blank = 0
	}
	for _, line := range lines {
		if strings.TrimSpace(line) == "" {
			blank++
			continue
		}
		flush()
		out = append(out, line)
	}
	flush()
	return strings.Join(out, "\n")
}

// maxBlankLinesShown is the longest run of blank lines printed as it is.
const maxBlankLinesShown = 2

func writeRequest(env Env, v approvals.View) {
	w := env.Stdout
	// All of this can come from what an agent wrote, right above a question that takes
	// Enter, so none of it may reach the terminal as control codes.
	fmt.Fprintln(w, approvalText(v.Summary))
	if v.Impact != "" {
		fmt.Fprintln(w, approvalText(v.Impact))
	}
	if len(v.Details) == 0 {
		fmt.Fprintln(w, "\nExact content:")
		fmt.Fprintln(w, approvalText(v.PayloadText))
	}
	for _, d := range v.Details {
		fmt.Fprintf(w, "\n--- %s\n%s\n", approvalText(d.Label), approvalText(d.Text))
	}
	fmt.Fprintln(w)
}

func writeApproved(ctx context.Context, env Env, rc requestLister, id string, st approvals.Status) int {
	_, res, _ := rc.Status(ctx, id)
	switch st {
	case approvals.StatusDone:
		fmt.Fprintln(env.Stdout, "Done.")
		if len(res) > 0 {
			fmt.Fprintln(env.Stdout, safetext.Terminal(string(res)))
		}
		return ExitOK
	case approvals.StatusReconciling, approvals.StatusExecuting:
		fmt.Fprintf(env.Stdout, "Sent. Checking that it arrived: `%s approvals status %s`.\n", variant.Current.Binary, safetext.Terminal(id))
		return ExitOK
	}
	fmt.Fprintf(env.Stderr, "%s: not done (%s).\n", variant.Current.Binary, safetext.Terminal(string(st)))
	if len(res) > 0 {
		fmt.Fprintln(env.Stderr, safetext.Terminal(string(res)))
	}
	return ExitError
}
