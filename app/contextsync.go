package app

import (
	"bufio"
	"context"
	"flag"
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/kameas-ai/kenaz-plugin/fleetapi"
	"github.com/kameas-ai/kenaz-plugin/internal/approvals"
	"github.com/kameas-ai/kenaz-plugin/internal/contextlib"
	contexttools "github.com/kameas-ai/kenaz-plugin/internal/mcp/tools/context"
	"github.com/kameas-ai/kenaz-plugin/internal/safetext"
	"github.com/kameas-ai/kenaz-plugin/internal/usertext"
	"github.com/kameas-ai/kenaz-plugin/internal/variant"
)

// syncRetryWaits are the pauses before each new try of a file while Fleet is busy or
// out of reach. A var for tests.
var syncRetryWaits = []time.Duration{2 * time.Second, 4 * time.Second, 8 * time.Second, 16 * time.Second, 30 * time.Second}

// runContextSync is `kenaz context sync`: one team's folder and Kameas Fleet, both ways.
// It needs a real terminal, so an agent's shell call without one is refused.
func runContextSync(env Env, args []string) int {
	fs := flag.NewFlagSet("context sync", flag.ContinueOnError)
	fs.SetOutput(env.Stderr)
	team := fs.String("team", "", "team name")
	if err := fs.Parse(args); err != nil {
		return ExitUsage
	}
	if fs.NArg() != 0 {
		fmt.Fprintf(env.Stderr, "usage: %s context sync [--team name]\n", variant.Current.Binary)
		return ExitUsage
	}
	fail := func(msg string) int {
		fmt.Fprintln(env.Stderr, variant.Current.Binary+": "+msg)
		return ExitError
	}
	if !memoryStdinTerminal() {
		return fail(usertext.NeedsTerminal(variant.Current.Binary + " context sync"))
	}
	dir, err := env.contextPersonalDir()
	if err != nil {
		return fail(err.Error())
	}
	ctx := env.ctx()
	in := bufio.NewReader(memoryStdin)
	p := env.Provider
	sess, terr := contexttools.Check(ctx, p, nil, contexttools.OpShare)
	if terr == nil {
		terr = contexttools.RequireContextFiles(sess.Caps)
	}
	if terr != nil {
		return fail(terr.Message)
	}
	acct, err := p.Auth().Session(ctx)
	if err != nil {
		return fail(contexttools.MapError(err).Message)
	}
	// The person is about to say yes in this terminal, as this account. A write checks the
	// session is still that one, as it does for an approved request.
	ctx = approvals.ContextWithIdentity(ctx, approvals.Identity{Env: string(acct.Env), OrgID: acct.OrgID, UserID: acct.UserID})
	chosen, ok := pickSyncTeam(env, in, *team, sess.Me.Teams)
	if !ok {
		return ExitError
	}
	viewer := &contextlib.Account{Env: string(acct.Env), OrgID: acct.OrgID, UserID: acct.UserID}
	cwd, _ := os.Getwd()
	led := contextlib.NewLedger(env.contextLedgerDir())
	s := &contexttools.Syncer{Store: contexttools.NodeStore(p, nil, led), Set: contextlib.Open(dir, cwd).WithViewer(viewer), Led: led,
		Acct: *viewer, Team: chosen}

	pulled, err := s.Pull(ctx)
	if err != nil {
		return fail(syncErrorMessage(err))
	}
	writeSyncPull(env, pulled)
	plan, err := s.Plan(ctx)
	if err != nil {
		return fail(syncErrorMessage(err))
	}
	writeSyncPlan(env, s, plan, pulled)
	unclaimed := unclaimedFolders(s, sess.Me.Teams)
	for _, folder := range unclaimed {
		fmt.Fprintln(env.Stdout, safetext.Terminal(usertext.SyncUnclaimed(folder)))
	}
	// A folder that held files and just became this account's: the person hears of it,
	// and it's named in the question, since two organizations can both have a Platform.
	claim := pulled.Legacy.Claim
	claimed := claim != nil && claim.HadFiles
	safeTeam := safetext.Terminal(safetext.OneLine(chosen.Name, 80))
	label := safeTeam
	if claimed {
		org := sess.Me.OrgName
		if org == "" {
			org = sess.Me.OrgID
		}
		label = safeTeam + " in " + safetext.Terminal(safetext.OneLine(org, 80))
	}
	bound := func() {
		if claimed {
			fmt.Fprintln(env.Stdout, safetext.Terminal(usertext.SyncFolderBound(claim.Folder, label)))
		}
	}
	if len(plan.Items) == 0 {
		bound()
		if pulled.Legacy.Unreadable {
			return ExitError
		}
		if len(plan.Refused) == 0 && len(unclaimed) == 0 && !claimed {
			fmt.Fprintln(env.Stdout, usertext.SyncUpToDate)
		}
		return ExitOK
	}
	cancel := func() int {
		fmt.Fprintln(env.Stdout, usertext.SyncCancelled)
		// A person who says no may be the wrong account: the folder goes back as it was, if
		// nothing has come to depend on it, and otherwise they hear whose it now is.
		if !s.UndoClaim(claim) {
			bound()
		}
		return ExitOK
	}
	for {
		fmt.Fprint(env.Stdout, usertext.PromptSyncShare(len(plan.Items), label, plan.FromMemory()))
		line, err := in.ReadString('\n')
		if err != nil && line == "" {
			return cancel()
		}
		switch strings.ToLower(strings.TrimSpace(line)) {
		case "", "y", "yes":
			return sendSyncItems(ctx, env, s, plan, label)
		case "v":
			viewSyncItems(env, in, plan)
		default:
			return cancel()
		}
	}
}

// unclaimedFolders are this account's folders with no team yet, whose names match none of
// its teams, so that no sync will ever take them.
func unclaimedFolders(s *contexttools.Syncer, teams []fleetapi.TeamRef) []string {
	names := map[string]bool{}
	for _, t := range teams {
		names[contextlib.FolderName(t.Name)] = true
	}
	var out []string
	for _, folder := range s.Set.Team.UnassignedFolders(s.Acct) {
		if !names[folder] {
			out = append(out, folder)
		}
	}
	return out
}

func syncErrorMessage(err error) string {
	if te, ok := err.(*contexttools.ToolError); ok {
		return te.Message
	}
	return "Kameas Fleet couldn't be reached."
}

// pickSyncTeam is the team asked for, the configured default, the only one, or else
// asks once.
func pickSyncTeam(env Env, in *bufio.Reader, want string, teams []fleetapi.TeamRef) (fleetapi.TeamRef, bool) {
	t, terr := contexttools.ResolveTeam(want, contexttools.ConfiguredDefaultTeam(), teams)
	if terr == nil {
		return t, true
	}
	if strings.TrimSpace(want) != "" || len(teams) < 2 {
		fmt.Fprintln(env.Stderr, variant.Current.Binary+": "+terr.Message)
		return fleetapi.TeamRef{}, false
	}
	for i, t := range teams {
		fmt.Fprintf(env.Stdout, "  %d. %s\n", i+1, safetext.OneLine(t.Name, 80))
	}
	fmt.Fprint(env.Stdout, usertext.SyncPickTeam)
	line, _ := in.ReadString('\n')
	line = strings.TrimSpace(line)
	if n, err := strconv.Atoi(line); err == nil && n >= 1 && n <= len(teams) {
		return teams[n-1], true
	}
	if line != "" {
		if t, terr := contexttools.ResolveTeam(line, "", teams); terr == nil {
			return t, true
		}
	}
	fmt.Fprintln(env.Stderr, variant.Current.Binary+": "+usertext.SyncNoSuchTeam)
	return fleetapi.TeamRef{}, false
}

func writeSyncPull(env Env, r contexttools.PullResult) {
	w := env.Stdout
	if n := r.Saved + r.Updated; n > 0 {
		fmt.Fprintf(w, "Brought %d from your team.\n", n)
	}
	for _, c := range r.Conflicts {
		fmt.Fprintln(w, safetext.Terminal(usertext.SyncConflictLine(c.Name, c.Yours)))
	}
	for _, g := range r.Gone {
		line := g.Name + ": removed from Kameas Fleet. Your copy stays."
		if g.Message != "" {
			line = g.Name + ": " + g.Message
		}
		fmt.Fprintln(w, safetext.Terminal(line))
	}
	for _, name := range r.Failed {
		fmt.Fprintln(w, safetext.Terminal(name+": couldn't be saved to this device."))
	}
	if r.Notes > 0 {
		fmt.Fprintln(w, usertext.SyncNotesLine(r.Notes))
	}
}

func syncSize(n int64) string {
	if n < 1024 {
		return fmt.Sprintf("%d B", n)
	}
	return fmt.Sprintf("%.1f KB", float64(n)/1024)
}

func writeSyncPlan(env Env, s *contexttools.Syncer, plan contexttools.SyncPlan, pulled contexttools.PullResult) {
	w := env.Stdout
	section := func(title string, isNew bool) {
		first := true
		for _, it := range plan.Items {
			if it.New != isNew {
				continue
			}
			if first {
				fmt.Fprintln(w, title)
				first = false
			}
			mark := ""
			if it.Drawn {
				mark = " (drawn from your private memory)"
			}
			fmt.Fprintf(w, "  %s  %s%s\n", safetext.Terminal(it.Name), syncSize(it.Size), mark)
		}
	}
	section("New:", true)
	section("Changed:", false)
	if len(pulled.Removed) > 0 {
		fmt.Fprintln(w, "Removed here, and still on Kameas Fleet, because Kenaz can't remove files there yet:")
		for _, name := range pulled.Removed {
			fmt.Fprintln(w, "  "+safetext.Terminal(name))
		}
	}
	for _, r := range plan.Refused {
		where := ""
		for _, f := range r.Findings {
			where += fmt.Sprintf(" %s, line %d;", f.Rule, f.Line)
		}
		fmt.Fprintf(w, "Not sent: %s: %s%s\n", safetext.Terminal(r.Name), r.Reason, strings.TrimSuffix(where, ";"))
	}
	if pulled.Legacy.Unreadable {
		fmt.Fprintln(w, safetext.Terminal(usertext.SyncFolderUnreadable(pulled.Legacy.Folder)))
	}
	if pulled.Legacy.Files > 0 {
		fmt.Fprintln(w, safetext.Terminal(usertext.SyncLegacyLine(pulled.Legacy.Files, pulled.Legacy.Folder, s.Folder())))
	}
	for _, name := range plan.OtherTeam {
		fmt.Fprintln(w, "Not sent: "+safetext.Terminal(name)+": this copy belongs to another of your teams.")
	}
	for _, name := range plan.OtherAccount {
		fmt.Fprintln(w, "Not sent: "+safetext.Terminal(name)+": this copy belongs to another account on this device.")
	}
	if plan.Unchanged > 0 {
		fmt.Fprintf(w, "%d unchanged.\n", plan.Unchanged)
	}
	if plan.InSubfolders > 0 {
		fmt.Fprintf(w, "%d in subfolders don't sync yet. Only files directly in %s do.\n", plan.InSubfolders, s.Folder())
	}
	if plan.Truncated {
		fmt.Fprintln(w, "This folder has too many files, so some weren't looked at.")
	}
}

// viewSyncItems shows the exact text, a file at a time.
func viewSyncItems(env Env, in *bufio.Reader, plan contexttools.SyncPlan) {
	for i, it := range plan.Items {
		fmt.Fprintf(env.Stdout, "\n--- %s\n%s\n", safetext.Terminal(it.Name), approvalText(it.Text))
		if i == len(plan.Items)-1 {
			break
		}
		fmt.Fprint(env.Stdout, usertext.SyncNextFile)
		line, err := in.ReadString('\n')
		if err != nil || strings.TrimSpace(line) != "" {
			break
		}
	}
	fmt.Fprintln(env.Stdout)
}

func sendSyncItems(ctx context.Context, env Env, s *contexttools.Syncer, plan contexttools.SyncPlan, team string) int {
	type failed struct{ name, why string }
	var fails []failed
	var conflicts []contexttools.SyncConflict
	sent := 0
	line := newProgressLine(env)
	stopped := 0
	for i, it := range plan.Items {
		line.update(fmt.Sprintf("Sharing: %d of %d…", i+1, len(plan.Items)))
		res, err := s.Send(ctx, it)
		for _, wait := range syncRetryWaits {
			if err == nil || !contexttools.Retryable(err) {
				break
			}
			select {
			case <-ctx.Done():
			case <-time.After(wait):
			}
			res, err = s.Send(ctx, it)
		}
		// Fleet isn't answering: the rest would only wait as long, each.
		if err != nil && contexttools.Retryable(err) {
			fails = append(fails, failed{it.Name, usertext.SyncUnreachLine})
			stopped = len(plan.Items) - i - 1
			break
		}
		switch res.Outcome {
		case contexttools.SyncSent:
			sent++
		case contexttools.SyncConflicted:
			conflicts = append(conflicts, res.Conflict)
		case contexttools.SyncNotYours:
			fails = append(fails, failed{it.Name, usertext.SyncNotYoursLine})
		case contexttools.SyncRemoved:
			why := usertext.SyncRemovedLine
			if it.New {
				why = usertext.SyncNameGone
			}
			fails = append(fails, failed{it.Name, why})
		case contexttools.SyncPromoted:
			fails = append(fails, failed{it.Name, usertext.SyncPromotedLine})
		case contexttools.SyncBlocked:
			fails = append(fails, failed{it.Name, usertext.SyncBlockedLine})
		case contexttools.SyncUnreachable:
			fails = append(fails, failed{it.Name, usertext.SyncUnreachLine})
		default:
			fails = append(fails, failed{it.Name, usertext.SyncRefusedLine})
		}
	}
	line.finish()
	for _, f := range fails {
		fmt.Fprintln(env.Stdout, safetext.Terminal(f.name)+": "+f.why)
	}
	for _, c := range conflicts {
		fmt.Fprintln(env.Stdout, safetext.Terminal(usertext.SyncConflictLine(c.Name, c.Yours)))
	}
	if stopped > 0 {
		fmt.Fprintln(env.Stdout, usertext.SyncStopped(stopped))
	}
	fmt.Fprintln(env.Stdout, safetext.Terminal(usertext.SyncResult(sent, len(plan.Items), team)))
	if len(fails) > 0 || len(conflicts) > 0 {
		return ExitError
	}
	return ExitOK
}
