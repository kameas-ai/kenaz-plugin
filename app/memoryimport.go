package app

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"time"

	"github.com/kameas-ai/kenaz-plugin/fleetapi"
	"github.com/kameas-ai/kenaz-plugin/internal/features"
	"github.com/kameas-ai/kenaz-plugin/internal/memory"
	"github.com/kameas-ai/kenaz-plugin/internal/memory/capture"
	"github.com/kameas-ai/kenaz-plugin/internal/memory/memimport"
	"github.com/kameas-ai/kenaz-plugin/internal/memory/memsync"
	"github.com/kameas-ai/kenaz-plugin/internal/safetext"
	"github.com/kameas-ai/kenaz-plugin/internal/variant"
)

const memoryImportUsage = `usage:
  kenaz memory import claude-mem --dry-run [options]   show what would come over; writes nothing
  kenaz memory import claude-mem --yes [options]       bring it over

Options:
  --db PATH       the database to read (default: ~/.claude-mem/claude-mem.db)
  --days N        how far back fixes and summaries go (default 30); decisions have no limit
  --max-notes N   at most this many notes (default: half the room your Fleet limit leaves)
  --max-bytes N   at most this much text (default: half the room your Fleet limit leaves)
  --root DIR      a directory whose repositories name your projects, and may be repeated
                  (default: the current directory and the one above it)

It reads that database read-only, never its prompts or its queue. Imported notes count as
automatic, so they're labelled untrusted and scrubbed, and any that look like they hold a
credential or an email address stay behind. A project with no checkout found stays on this
device. Run it again and nothing comes over twice.

Imported notes can sync to your Kameas Fleet account like other automatic notes, if memory sync is on.

` + memoryImportKeptLine + `
`

// memoryImportKeptLine says what neither the import nor forget-all removes.
const memoryImportKeptLine = "This import doesn't change claude-mem's database, and neither does `kenaz memory forget-all`, so its notes stay there until you delete them."

// otherMemoryDB is where the other tool keeps its database.
func otherMemoryDB() string {
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	return filepath.Join(home, ".claude-mem", "claude-mem.db")
}

type stringList []string

func (l *stringList) String() string     { return fmt.Sprint(*l) }
func (l *stringList) Set(v string) error { *l = append(*l, v); return nil }

// runMemoryImport is `kenaz memory import`.
func runMemoryImport(ctx context.Context, env Env, mp memoryPaths, args []string) int {
	if !captureAvailable() {
		fmt.Fprintln(env.Stderr, variant.Current.Binary+": automatic memory capture is not available in this build.")
		return ExitError
	}
	if len(args) == 0 || args[0] != "claude-mem" {
		fmt.Fprint(env.Stderr, memoryImportUsage)
		return ExitUsage
	}
	fs := flag.NewFlagSet("memory import", flag.ContinueOnError)
	fs.SetOutput(env.Stderr)
	dry := fs.Bool("dry-run", false, "show what would come over")
	yes := fs.Bool("yes", false, "bring it over")
	db := fs.String("db", otherMemoryDB(), "the database to read")
	days := fs.Int("days", memimport.DefaultDays, "days of fixes and summaries")
	maxNotes := fs.Int("max-notes", 0, "at most this many notes (default: half the room Kameas Fleet reports)")
	maxBytes := fs.Int64("max-bytes", 0, "at most this much text (default: half the room Kameas Fleet reports)")
	var roots stringList
	fs.Var(&roots, "root", "a directory whose repositories name your projects")
	if err := fs.Parse(args[1:]); err != nil || fs.NArg() != 0 || *dry == *yes {
		fmt.Fprint(env.Stderr, memoryImportUsage)
		return ExitUsage
	}
	if !mp.enabled() {
		fmt.Fprintln(env.Stderr, variant.Current.Binary+": "+memory.OffMessage)
		return ExitError
	}
	if *yes && !mp.captureSettings().Active() {
		// Imported notes are automatic notes, so the member has to have read that notice.
		fmt.Fprintln(env.Stderr, variant.Current.Binary+": turn on automatic memory capture first (`kenaz memory capture enable`): imported notes are automatic notes.")
		return ExitError
	}
	if why := env.captureRefusal(ctx, mp.captureSettings().Writer); *yes && why != "" {
		// Imported notes are automatic notes: they come over only while Kameas Fleet
		// offers capture to the organization.
		fmt.Fprintln(env.Stderr, variant.Current.Binary+": "+why)
		return ExitError
	}
	if len(roots) == 0 {
		if cwd, err := os.Getwd(); err == nil {
			roots = stringList{cwd, filepath.Dir(cwd)}
		}
	}
	plan, err := prepareImport(ctx, env, mp, *db, *days, *maxNotes, *maxBytes, roots)
	if errors.Is(err, os.ErrNotExist) {
		fmt.Fprintf(env.Stderr, "%s: no database at %s\n", variant.Current.Binary, *db)
		return ExitError
	}
	if err != nil {
		fmt.Fprintln(env.Stderr, variant.Current.Binary+": "+err.Error())
		return ExitError
	}
	printImportPlan(env, plan)
	if *dry {
		fmt.Fprintln(env.Stdout, "Nothing was written. Run it with --yes to bring these over.")
		return ExitOK
	}
	p := mp.active(ctx)
	st, err := memory.OpenProfile(memory.ProfilePath(mp.StateRoot, p), p)
	if err != nil {
		fmt.Fprintln(env.Stderr, variant.Current.Binary+": "+err.Error())
		return ExitError
	}
	defer st.Close()
	res, err := memimport.Apply(ctx, st, plan)
	if err != nil {
		fmt.Fprintln(env.Stderr, variant.Current.Binary+": "+err.Error())
		return ExitError
	}
	// Run by hand, it uploads at Kameas Fleet's pace. A command an agent ran has no
	// terminal, so its notes stay within the hourly cap.
	if memoryStdinTerminal() {
		allowImportUpload(ctx, mp)
	}
	fmt.Fprintf(env.Stdout, "Imported %d notes.\n", res.Saved)
	if res.Skipped > 0 {
		fmt.Fprintf(env.Stdout, "%d couldn't be saved.\n", res.Skipped)
	}
	fmt.Fprintln(env.Stdout, memoryImportKeptLine)
	return ExitOK
}

// fleetUsage reads what's stored and the effective limits from Fleet. Limits are never
// assumed here. ok is false when Fleet can't say.
func fleetUsage(ctx context.Context, mp memoryPaths) (fleetapi.MemoryUsage, bool) {
	r, is := memoryRemote(mp.p).(memsync.Remote)
	p := mp.active(ctx)
	if !is || p.IsLocal() {
		return fleetapi.MemoryUsage{}, false
	}
	rctx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	u, err := r.Usage(rctx, p)
	return u, err == nil
}

// prepareImport works out what would come over. What an earlier run brought over doesn't
// come again, so the plan doesn't count it.
func prepareImport(ctx context.Context, env Env, mp memoryPaths, db string, days, maxNotes int, maxBytes int64, roots []string) (memimport.Plan, error) {
	home, _ := os.UserHomeDir()
	if u, ok := fleetUsage(ctx, mp); ok {
		n, b := memimport.RoomFor(u.LiveRecords, u.MaxRecords, u.LiveBytes, u.MaxBytes)
		if maxNotes == 0 {
			maxNotes = n
		}
		if maxBytes == 0 {
			maxBytes = b
		}
	} else if memoryRemote(mp.p) != nil && !mp.active(ctx).IsLocal() {
		// These notes will sync, and Fleet couldn't say how much room there is.
		if maxNotes == 0 {
			maxNotes = memimport.NoAnswerNotes
		}
		if maxBytes == 0 {
			maxBytes = memimport.NoAnswerBytes
		}
	}
	// The device keeps at most this many automatic notes, and past that drops the oldest
	// while the ledger says they came over.
	if maxNotes == 0 || int64(maxNotes) > memory.MaxAutoNotes {
		maxNotes = int(memory.MaxAutoNotes)
	}
	if maxBytes == 0 || maxBytes > memory.MaxAutoBytes {
		maxBytes = memory.MaxAutoBytes
	}
	plan, err := memimport.Prepare(ctx, memimport.Options{DB: db, Days: days, MaxNotes: maxNotes, MaxBytes: maxBytes, Roots: roots, Home: home, Now: env.now()})
	if err != nil {
		return plan, err
	}
	active := mp.active(ctx)
	st, err := memory.OpenProfileReadOnly(memory.ProfilePath(mp.StateRoot, active), active, 2*time.Second)
	if errors.Is(err, memory.ErrNoStore) {
		return plan, nil
	}
	if err != nil {
		return plan, err
	}
	defer st.Close()
	return plan, plan.LeaveOutImported(ctx, st)
}

func printImportPlan(env Env, plan memimport.Plan) {
	w := env.Stdout
	fmt.Fprintf(w, "Would bring over %d notes, %.1f MiB: %d decisions, %d fixes, %d summaries.\n",
		plan.Total(), float64(plan.Bytes)/(1<<20), plan.Decisions, plan.Bugfixes, plan.Summaries)
	if plan.Before > 0 {
		fmt.Fprintf(w, "%d were imported before, and don't come over again, even if you've since deleted them.\n", plan.Before)
	}
	fmt.Fprintf(w, "Left behind: %d that look like they hold a credential, %d with an email address, %d that aren't valid notes, %d over the limits.\n",
		plan.ExcludedCredential, plan.ExcludedEmail, plan.ExcludedInvalid, plan.OverBudget)
	names := make([]string, 0, len(plan.Projects))
	for n := range plan.Projects {
		names = append(names, n)
	}
	sort.Strings(names)
	for _, n := range names {
		pp := plan.Projects[n]
		where := "stays on this device: no checkout found, or it has no remote"
		switch {
		case pp.Ambiguous:
			where = "stays on this device: more than one folder has this name"
		case pp.Portable && !features.MemoryProjectScope:
			where = "stays on this device in this build"
		case pp.Portable:
			where = "can sync"
		}
		if pp.Checkout != "" {
			where += ", filed under " + pp.Checkout
		}
		fmt.Fprintf(w, "  %-32s %6d notes  %s\n", safetext.OneLine(n, 64), pp.Notes, where)
	}
}

// captureCompare puts this tool's activity next to the other's, for the period when
// both run: two indexes in every session, and two background models to pay for. It
// reads counts only from the other's database.
func captureCompare(ctx context.Context, env Env, mp memoryPaths, args []string) int {
	now := env.now()
	since := now.AddDate(0, 0, -7)
	r := capture.BuildReport(mp.captureRoot(), mp.captureSettings(), now)
	var ours memory.AutoCounts
	p := mp.active(ctx)
	if st, err := memory.OpenProfileReadOnly(memory.ProfilePath(mp.StateRoot, p), p, 2*time.Second); err == nil {
		ours, _ = st.CountAutoSince(ctx, since)
		st.Close()
	}
	db := otherMemoryDB()
	theirs, theirsErr := memimport.CountSince(ctx, db, since)
	if len(args) == 1 && args[0] == "--json" {
		out := map[string]any{"this": map[string]any{"notes_7d": ours, "tokens_30d": r.Last30Days.Tokens(), "usd_30d": r.Last30Days.USD, "line": r.Line()}}
		if theirsErr == nil {
			out["other"] = map[string]any{"notes_7d": theirs}
		}
		enc := json.NewEncoder(env.Stdout)
		enc.SetIndent("", "  ")
		_ = enc.Encode(out)
		return ExitOK
	}
	w := env.Stdout
	fmt.Fprintln(w, r.Line())
	fmt.Fprintf(w, "Kenaz, last 7 days:      %d observations, %d summaries\n", ours.Observations, ours.Summaries)
	fmt.Fprintf(w, "Kenaz, last 30 days:     %d tokens, about $%.2f, %d calls\n", r.Last30Days.Tokens(), r.Last30Days.USD, r.Last30Days.Calls)
	if theirsErr != nil {
		fmt.Fprintln(w, "No other memory tool's database found.")
		return ExitOK
	}
	fmt.Fprintf(w, "claude-mem, last 7 days: %d observations, %d summaries, %d tokens of work\n", theirs.Observations, theirs.Summaries, theirs.WorkTokens)
	fmt.Fprintln(w, "Both run while both are on: each session gets two indexes, and you pay for two background models. Turn this one off with `kenaz memory capture disable`.")
	return ExitOK
}
