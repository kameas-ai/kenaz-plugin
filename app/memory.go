package app

import (
	"bufio"
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	sdk "github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/kameas-ai/kenaz-plugin/fleetapi"
	"github.com/kameas-ai/kenaz-plugin/internal/config"
	"github.com/kameas-ai/kenaz-plugin/internal/daemon"
	"github.com/kameas-ai/kenaz-plugin/internal/mcp"
	memorytools "github.com/kameas-ai/kenaz-plugin/internal/mcp/tools/memory"
	"github.com/kameas-ai/kenaz-plugin/internal/memory"
	"github.com/kameas-ai/kenaz-plugin/internal/variant"
	"github.com/kameas-ai/kenaz-plugin/notices"
)

// memoryConfigKey is the config.toml switch: "off" turns memory off;
// anything else (including no value) leaves it on.
const memoryConfigKey = "memory"

// memoryPaths is where memory lives: per OS user under the state root,
// not per Fleet env. The device notebook needs no sign-in; account
// profiles live beside it (memory/profiles/).
type memoryPaths struct {
	StateRoot string
	DB        string // the device notebook
	Marker    string // records that the one-time disclosure was shown
	Config    string // config.toml (the on/off switch)
	p         fleetapi.Provider
}

func (e Env) memoryPaths() (memoryPaths, error) {
	p, err := e.paths()
	if err != nil {
		return memoryPaths{}, err
	}
	return memoryPaths{
		StateRoot: p.StateRoot,
		DB:        memory.PathIn(p.StateRoot),
		Marker:    filepath.Join(p.StateRoot, memory.DisclosureMarker),
		Config:    p.ConfigFile(),
		p:         e.Provider,
	}, nil
}

func (mp memoryPaths) enabled() bool {
	f, err := config.LoadFile(mp.Config)
	if err != nil {
		return true
	}
	return strings.TrimSpace(strings.ToLower(f.Get(memoryConfigKey))) != "off"
}

func (mp memoryPaths) active(ctx context.Context) memory.Profile { return memoryProfileOf(ctx, mp.p) }

// service is memory as the CLI and hooks use it.
func (mp memoryPaths) service(sub memory.Submitter) *memory.Service {
	n := notices.MemorySync()
	return memory.NewService(memory.ServiceOptions{StateRoot: mp.StateRoot, Active: mp.active, Submitter: sub,
		Remote: memoryRemote(mp.p), ConsentVersion: n.ID, ConsentNotice: n.Text})
}

// cliSubmitter prepares approval requests through the relay daemon,
// starting it if needed. The CLI can only prepare: the user approves on
// the approval page with their authenticator.
func cliSubmitter(env Env) memory.Submitter {
	return memorytools.Submitter(approvalsClient(func(ctx context.Context) *daemon.Client {
		r, err := env.resolve("")
		if err != nil {
			return nil
		}
		c, err := daemon.EnsureRunning(ctx, r.StateDir, r.spawner(env), 3*time.Second)
		if err != nil {
			return nil
		}
		return c
	}))
}

// discloseOnce returns the one-time disclosure the first time memory is
// used on this machine (by any path), and "" afterwards or while off.
func (mp memoryPaths) discloseOnce(now time.Time) string {
	if !mp.enabled() {
		return ""
	}
	if err := os.MkdirAll(filepath.Dir(mp.Marker), 0o700); err != nil {
		return ""
	}
	if memory.ClaimDisclosure(mp.Marker, now) {
		return memory.Disclosure(mp.DB)
	}
	return ""
}

// statusLine is what status and doctor print.
func (mp memoryPaths) statusLine(ctx context.Context) string {
	if !mp.enabled() {
		return "memory: off"
	}
	svc := mp.service(nil)
	n, _ := svc.Count(ctx)
	unit := "notes"
	if n == 1 {
		unit = "note"
	}
	line := fmt.Sprintf("memory: on (%s, %d %s)", mp.DB, n, unit)
	if st, _, signedIn, err := svc.SyncStatus(ctx); err == nil && signedIn {
		line += "; sync " + syncWord(st)
	}
	return line
}

func syncWord(st memory.SyncStatus) string {
	switch {
	case st.ErasePending:
		return "erasing"
	case !st.Enabled:
		return "off"
	case st.StatusCode != "":
		return "on, " + st.StatusCode
	}
	q := st.Queue["ready"] + st.Queue["inflight"]
	return fmt.Sprintf("on (%d queued, %d waiting for approval, %d refused)", q, st.Queue["unapproved"], st.Queue["blocked"])
}

// memoryGroup is the memory_* MCP tool group for this machine.
func memoryGroup(env Env) mcp.Group {
	mp, err := env.memoryPaths()
	if err != nil {
		return mcp.GroupFunc(func(*sdk.Server, mcp.Deps) {})
	}
	return memorytools.NewGroup(memorytools.Options{
		Path:           mp.DB,
		Enabled:        mp.enabled,
		Disclose:       func() string { return mp.discloseOnce(env.now()) },
		Active:         mp.active,
		Remote:         memoryRemote(env.Provider),
		ConsentVersion: notices.MemorySync().ID,
	})
}

// memoryIndex adapts the store to the session-start hook: the signed-in
// account's notes and the device notebook (titles and ids only).
type memoryIndex struct{ mp memoryPaths }

func (m memoryIndex) Index(ctx context.Context, cwd string) string {
	if !m.mp.enabled() {
		return ""
	}
	if cwd == "" {
		cwd, _ = os.Getwd()
	}
	p, err := memory.ProjectFor(cwd)
	if err != nil {
		return ""
	}
	return m.mp.service(nil).Index(ctx, p)
}

// keptMemoryLine is printed by logout and setup --undo.
func (e Env) keptMemoryLine() string {
	mp, err := e.memoryPaths()
	if err != nil {
		return ""
	}
	return memory.KeptLine(mp.DB)
}

const memoryUsage = `usage:
  kenaz memory list [--project current|all] [--limit N]
  kenaz memory search <query> [--project current|all] [--limit N]
  kenaz memory show <id>
  kenaz memory update <id> [--title T] [--body B] [--tags a,b]
  kenaz memory forget <id>
  kenaz memory revert <id>        undo a refused edit, or a deletion waiting for approval
  kenaz memory export             every note of this account and device, as NDJSON
  kenaz memory forget-all         delete every note of the signed-in account, here and
                                  in Kameas Fleet (needs approval on the approval page)
  kenaz memory purge [--yes]      delete the device notebook (signed-out notes)
  kenaz memory path               print the device notebook's location
  kenaz memory enable | disable   turn memory on or off (default on)
  kenaz memory sync status        is memory sync on, and what is queued
  kenaz memory sync enable        turn on sync to your Kameas Fleet account (needs approval)
  kenaz memory sync disable       stop syncing (synced notes stay until deleted)
  kenaz memory sync approve       ask to send changes that wait for your approval
  kenaz memory sync adopt <id>... | --all
                                  move signed-out notes into your account so they sync

`

func memoryHelp(w io.Writer) {
	fmt.Fprint(w, memoryUsage)
	fmt.Fprintln(w, memory.Notice)
	fmt.Fprintln(w)
	fmt.Fprintln(w, "Your AI tool saves durable facts, decisions, conventions and fixes here; it must never save secrets, credentials or personal data. The device notebook is per OS user: everyone sharing this OS account sees the same notes saved while signed out.")
}

// memoryStdinTerminal is a var so tests can drive the confirmations.
var memoryStdinTerminal = func() bool {
	fi, err := os.Stdin.Stat()
	return err == nil && fi.Mode()&os.ModeCharDevice != 0
}

var memoryStdin io.Reader = os.Stdin

// confirm asks a yes/no question on the terminal.
func confirm(env Env, question string) bool {
	fmt.Fprintf(env.Stdout, "%s [y/N] ", question)
	line, _ := bufio.NewReader(memoryStdin).ReadString('\n')
	a := strings.ToLower(strings.TrimSpace(line))
	return a == "y" || a == "yes"
}

// runMemory is `kenaz memory ...`.
func runMemory(env Env, args []string) int {
	if len(args) == 0 || args[0] == "help" || args[0] == "-h" || args[0] == "--help" {
		memoryHelp(env.Stdout)
		if len(args) == 0 {
			return ExitUsage
		}
		return ExitOK
	}
	mp, err := env.memoryPaths()
	if err != nil {
		fmt.Fprintln(env.Stderr, variant.Current.Binary+": "+err.Error())
		return ExitError
	}
	sub, rest := args[0], args[1:]
	for _, a := range rest {
		if a == "-h" || a == "--help" {
			memoryHelp(env.Stdout)
			return ExitOK
		}
	}
	ctx := env.Ctx
	if ctx == nil {
		ctx = context.Background()
	}

	switch sub {
	case "enable":
		if err := config.SetValue(mp.Config, memoryConfigKey, "on"); err != nil {
			fmt.Fprintln(env.Stderr, variant.Current.Binary+": "+err.Error())
			return ExitError
		}
		fmt.Fprintf(env.Stdout, "Kenaz memory is on (%s).\n", mp.DB)
		showDisclosure(env, mp)
		return ExitOK
	case "disable":
		if err := config.SetValue(mp.Config, memoryConfigKey, "off"); err != nil {
			fmt.Fprintln(env.Stderr, variant.Current.Binary+": "+err.Error())
			return ExitError
		}
		fmt.Fprintf(env.Stdout, "Kenaz memory is off. Your AI tool can no longer read or save memories, and sessions no longer get the memory index. Existing memories were kept in %s; delete them with `kenaz memory purge`.\n", mp.DB)
		return ExitOK
	case "path":
		showDisclosure(env, mp)
		fmt.Fprintln(env.Stdout, mp.DB)
		return ExitOK
	case "purge":
		return memoryPurge(env, mp, rest)
	case "sync":
		return memorySync(ctx, env, mp, rest)
	case "list", "search", "show", "forget", "update", "revert", "export", "forget-all":
	default:
		fmt.Fprintf(env.Stderr, variant.Current.Binary+" memory: unknown command %q\n\n", sub)
		fmt.Fprint(env.Stderr, memoryUsage)
		return ExitUsage
	}

	if !mp.enabled() {
		fmt.Fprintln(env.Stderr, variant.Current.Binary+": "+memory.OffMessage)
		return ExitError
	}
	showDisclosure(env, mp)

	fs := flag.NewFlagSet("memory "+sub, flag.ContinueOnError)
	fs.SetOutput(env.Stderr)
	project := fs.String("project", "current", "current (this project and global notes) or all")
	limit := fs.Int("limit", memory.DefaultLimit, "maximum entries (1 to 20)")
	noBrowserF := fs.Bool("no-browser", false, "print the approval page URL instead of opening a browser")
	title := fs.String("title", "", "new title (update)")
	body := fs.String("body", "", "new body (update)")
	tags := fs.String("tags", "", "new comma-separated tags (update)")
	var pos []string
	for {
		if err := fs.Parse(rest); err != nil {
			return ExitUsage
		}
		if fs.NArg() == 0 {
			break
		}
		pos = append(pos, fs.Arg(0))
		rest = fs.Args()[1:]
	}
	noBrowser := *noBrowserF
	set := map[string]bool{}
	fs.Visit(func(f *flag.Flag) {
		if f.Name != "no-browser" {
			set[f.Name] = true
		}
	})
	scope, err := memoryScope(*project)
	if err != nil {
		fmt.Fprintln(env.Stderr, variant.Current.Binary+" memory: "+err.Error())
		return ExitUsage
	}
	svc := mp.service(cliSubmitter(env))
	fail := func(err error) int {
		fmt.Fprintln(env.Stderr, variant.Current.Binary+": "+err.Error())
		return ExitError
	}
	usage := func() int {
		fmt.Fprint(env.Stderr, memoryUsage)
		return ExitUsage
	}
	switch sub {
	case "list":
		if len(pos) != 0 {
			return usage()
		}
		es, err := svc.Recent(ctx, scope, *limit)
		if err != nil {
			return fail(err)
		}
		printEntries(env.Stdout, es, false)
	case "search":
		if len(pos) == 0 {
			return usage()
		}
		es, err := svc.Search(ctx, strings.Join(pos, " "), scope, *limit)
		if err != nil {
			return fail(err)
		}
		printEntries(env.Stdout, es, true)
	case "show":
		if len(pos) != 1 {
			return usage()
		}
		notes, _, err := svc.Get(ctx, pos)
		if err != nil {
			return fail(err)
		}
		if len(notes) == 0 {
			return fail(memory.ErrNotFound)
		}
		printNote(env.Stdout, notes[0])
	case "update":
		if len(pos) != 1 || len(set) == 0 {
			return usage()
		}
		var u memory.Update
		if set["title"] {
			u.Title = title
		}
		if set["body"] {
			u.Body = body
		}
		if set["tags"] {
			ts := splitTags(*tags)
			u.Tags = &ts
		}
		out, err := svc.Update(ctx, pos[0], u)
		if err != nil {
			return fail(err)
		}
		return printOutcome(env, out, "Updated memory "+out.ID+".", noBrowser)
	case "forget":
		if len(pos) != 1 {
			return usage()
		}
		out, err := svc.Forget(ctx, pos[0])
		if err != nil {
			return fail(err)
		}
		return printOutcome(env, out, fmt.Sprintf("Deleted memory %s.", pos[0]), noBrowser)
	case "revert":
		if len(pos) != 1 {
			return usage()
		}
		if err := svc.Revert(ctx, pos[0]); err != nil {
			return fail(err)
		}
		fmt.Fprintln(env.Stdout, "Reverted: the refused edit was removed and the previous version is back.")
	case "export":
		if len(pos) != 0 {
			return usage()
		}
		n, err := svc.Export(ctx, env.Stdout)
		if err != nil {
			return fail(err)
		}
		fmt.Fprintf(env.Stderr, "Exported %d memories.\n", n)
	case "forget-all":
		if len(pos) != 0 {
			return usage()
		}
		out, err := svc.ForgetAll(ctx)
		if err != nil {
			return fail(err)
		}
		return printOutcome(env, out, "", noBrowser)
	}
	return ExitOK
}

func splitTags(s string) []string {
	var out []string
	for _, t := range strings.Split(s, ",") {
		if t = strings.TrimSpace(t); t != "" {
			out = append(out, t)
		}
	}
	return out
}

// printOutcome prints a gated operation's truthful result. A request
// waiting for approval prints (and opens) the approval page: the CLI never
// approves anything itself.
func printOutcome(env Env, out memory.Outcome, done string, noBrowser bool) int {
	switch out.Status {
	case memory.OutcomeDone:
		if done != "" {
			fmt.Fprintln(env.Stdout, done)
		}
		if out.Message != "" && out.Message != "Deleted." {
			fmt.Fprintln(env.Stdout, out.Message)
		}
		return ExitOK
	case memory.OutcomeApprovalPending:
		fmt.Fprintln(env.Stdout, "Waiting for your approval; nothing has changed yet.")
		if out.ApproveURL != "" {
			fmt.Fprintf(env.Stdout, "Review and approve it with your security key or device unlock:\n  %s\n", out.ApproveURL)
			if !noBrowser {
				open := env.OpenBrowser
				if open == nil {
					open = openBrowser
				}
				if err := open(out.ApproveURL); err != nil {
					fmt.Fprintln(env.Stdout, "Could not open a browser here; open the link above on this machine.")
				}
			}
		}
		fmt.Fprintf(env.Stdout, "Status: pending. Check it with `kenaz approvals status %s`.\n", out.RequestID)
		return ExitOK
	case memory.OutcomeDenied, memory.OutcomeCanceled, memory.OutcomeExpired:
		fmt.Fprintln(env.Stdout, "Nothing was changed: "+out.Status+".")
		return ExitOK
	}
	msg := out.Message
	if msg == "" {
		msg = "not done (" + out.Status + ")"
	}
	fmt.Fprintln(env.Stderr, variant.Current.Binary+": "+msg)
	return ExitError
}

func printNote(w io.Writer, n memory.Note) {
	fmt.Fprintf(w, "id:       %s\n", n.ID)
	fmt.Fprintf(w, "title:    %s\n", n.Title)
	fmt.Fprintf(w, "created:  %s\n", n.CreatedAt.Format(time.RFC3339))
	switch {
	case n.ProjectKey == memory.GlobalProject:
		fmt.Fprintln(w, "project:  global")
	default:
		fmt.Fprintf(w, "project:  %s (%s)\n", n.ProjectRoot, n.ProjectKey)
	}
	if len(n.Tags) > 0 {
		fmt.Fprintf(w, "tags:     %s\n", strings.Join(n.Tags, ", "))
	}
	fmt.Fprintf(w, "sync:     %s\n", memory.StateExplanation(n.SyncState, n.SyncCode))
	fmt.Fprintf(w, "source:   %s\n\n%s\n", n.Source, n.Body)
}

func showDisclosure(env Env, mp memoryPaths) {
	if d := mp.discloseOnce(env.now()); d != "" {
		fmt.Fprintln(env.Stderr, d)
	}
}

func memoryScope(arg string) (memory.Scope, error) {
	switch arg {
	case "", "current":
		wd, _ := os.Getwd()
		p, err := memory.ProjectFor(wd)
		if err != nil {
			return memory.Scope{}, err
		}
		return memory.ProjectScope(p.Key), nil
	case "all":
		return memory.AllProjects, nil
	}
	return memory.Scope{}, fmt.Errorf("--project must be current or all, not %q", arg)
}

func printEntries(w io.Writer, es []memory.Entry, snippets bool) {
	if len(es) == 0 {
		fmt.Fprintln(w, "No memories found.")
		return
	}
	for _, e := range es {
		fmt.Fprintf(w, "%s  %s  %s", e.ID, e.CreatedAt.Format("2006-01-02"), e.Title)
		if len(e.Tags) > 0 {
			fmt.Fprintf(w, "  [%s]", strings.Join(e.Tags, ", "))
		}
		if e.SyncState != "" && e.SyncState != memory.StateLocal {
			fmt.Fprintf(w, "  (%s)", e.SyncState)
		}
		fmt.Fprintln(w)
		if snippets && e.Snippet != "" {
			fmt.Fprintf(w, "    %s\n", e.Snippet)
		}
	}
}

// memorySync is `kenaz memory sync ...`. Everything that reaches Fleet
// (enable, approve, adopt) is prepared for approval on the approval page;
// the CLI only prints and opens that page.
func memorySync(ctx context.Context, env Env, mp memoryPaths, args []string) int {
	noBrowser := false
	var rest []string
	for _, a := range args {
		if a == "--no-browser" {
			noBrowser = true
			continue
		}
		rest = append(rest, a)
	}
	args = rest
	if len(args) == 0 {
		fmt.Fprint(env.Stderr, memoryUsage)
		return ExitUsage
	}
	svc := mp.service(cliSubmitter(env))
	fail := func(err error) int {
		fmt.Fprintln(env.Stderr, variant.Current.Binary+": "+err.Error())
		return ExitError
	}
	switch args[0] {
	case "status":
		st, _, signedIn, err := svc.SyncStatus(ctx)
		if err != nil {
			return fail(err)
		}
		if !signedIn {
			fmt.Fprintln(env.Stdout, "Signed out: notes are saved in this device's notebook, which never syncs. Sign in with `kenaz login` to sync an account's notes.")
			return ExitOK
		}
		fmt.Fprintln(env.Stdout, memorytools.SyncSummary(st, memoryRemote(mp.p) != nil))
		fmt.Fprintf(env.Stdout, "notes:    %d synced, %d queued, %d waiting for approval, %d on this device only, %d refused\n",
			st.Notes[memory.StateSynced], st.Notes[memory.StatePending], st.Notes[memory.StateUnapproved], st.Notes[memory.StateLocal], st.Notes[memory.StateRejected])
		if st.ForgetsPending > 0 || st.EditsInProgress > 0 || st.EditsBlocked > 0 {
			fmt.Fprintf(env.Stdout, "changes:  %d deletions and %d edits on their way, %d edits refused\n", st.ForgetsPending, st.EditsInProgress, st.EditsBlocked)
		}
		if len(st.BlockedCodes) > 0 {
			fmt.Fprintf(env.Stdout, "refused:  %s\n", strings.Join(st.BlockedCodes, ", "))
		}
		if st.LastPushAt != nil || st.LastPullAt != nil {
			fmt.Fprintf(env.Stdout, "last:     sent %s, received %s\n", when(st.LastPushAt), when(st.LastPullAt))
		}
		return ExitOK
	case "enable":
		if memoryRemote(mp.p) == nil {
			return fail(memory.ErrSyncUnavailable)
		}
		n := notices.MemorySync()
		fmt.Fprintln(env.Stdout, n.Text)
		fmt.Fprintf(env.Stdout, "The approval page shows this notice (%s) again; approving there turns memory sync on.\n\n", n.ID)
		out, err := svc.EnableSync(ctx)
		if err != nil {
			return fail(err)
		}
		return printOutcome(env, out, "Memory sync is on.", noBrowser)
	case "disable":
		out, err := svc.DisableSync(ctx)
		if err != nil {
			return fail(err)
		}
		fmt.Fprintln(env.Stdout, out.Message)
		return ExitOK
	case "approve":
		out, err := svc.ApproveHeld(ctx)
		if err != nil {
			return fail(err)
		}
		return printOutcome(env, out, "", noBrowser)
	case "adopt":
		ids := args[1:]
		if len(ids) == 1 && ids[0] == "--all" {
			var err error
			if ids, err = svc.LocalIDs(ctx); err != nil {
				return fail(err)
			}
		}
		if len(ids) == 0 {
			fmt.Fprint(env.Stderr, memoryUsage)
			return ExitUsage
		}
		out, err := svc.Adopt(ctx, ids)
		if err != nil {
			return fail(err)
		}
		return printOutcome(env, out, "", noBrowser)
	}
	fmt.Fprint(env.Stderr, memoryUsage)
	return ExitUsage
}

func when(t *time.Time) string {
	if t == nil {
		return "never"
	}
	return t.Local().Format(time.RFC3339)
}

func memoryPurge(env Env, mp memoryPaths, args []string) int {
	fs := flag.NewFlagSet("memory purge", flag.ContinueOnError)
	fs.SetOutput(env.Stderr)
	yes := fs.Bool("yes", false, "delete without asking")
	if err := fs.Parse(args); err != nil || fs.NArg() != 0 {
		return ExitUsage
	}
	// Purge clears only the device notebook (signed-out notes, never
	// synced). Account stores are deleted only through an approved
	// forget-all, so this command can never erase them.
	profiles := memory.ProfileStores(mp.StateRoot)
	keptLine := func() {
		if len(profiles) > 0 {
			fmt.Fprintln(env.Stdout, "Memory saved under a Kameas Fleet account was kept. To delete it, sign in to that account and run `kenaz memory forget-all` (approved on the approval page).")
		}
	}
	if _, err := os.Stat(mp.DB); errors.Is(err, os.ErrNotExist) {
		fmt.Fprintf(env.Stdout, "No Kenaz memory notebook on this device (%s).\n", mp.DB)
		keptLine()
		return ExitOK
	}
	if !*yes {
		n := "all"
		if s, err := memory.OpenReadOnly(mp.DB, time.Second); err == nil {
			if c, err := s.Count(context.Background()); err == nil {
				n = strconv.Itoa(c)
			}
			s.Close()
		}
		if !memoryStdinTerminal() {
			fmt.Fprintln(env.Stderr, variant.Current.Binary+": purge deletes every note in this device's notebook; run it in a terminal to confirm, or pass --yes.")
			return ExitError
		}
		if !confirm(env, fmt.Sprintf("Delete %s notes in this device's notebook (%s)? This cannot be undone.", n, mp.DB)) {
			fmt.Fprintln(env.Stdout, "Nothing was deleted.")
			return ExitOK
		}
	}
	if err := memory.Purge(mp.DB); err != nil {
		fmt.Fprintln(env.Stderr, variant.Current.Binary+": "+err.Error())
		return ExitError
	}
	fmt.Fprintf(env.Stdout, "Deleted your Kenaz memory notebook (%s).\n", mp.DB)
	keptLine()
	return ExitOK
}

// memoryJSON is the memory section of `kenaz status --json`.
func memoryJSON(env Env, mp memoryPaths) map[string]any {
	out := map[string]any{"enabled": mp.enabled(), "path": mp.DB}
	if mp.enabled() {
		svc := mp.service(nil)
		n, _ := svc.Count(env.Ctx)
		out["notes"] = n
		if st, _, signedIn, err := svc.SyncStatus(env.Ctx); err == nil && signedIn {
			out["sync"] = st
		}
	}
	return out
}
