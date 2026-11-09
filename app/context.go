package app

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/kameas-ai/kenaz-plugin/fleetapi"

	"github.com/kameas-ai/kenaz-plugin/internal/config"
	"github.com/kameas-ai/kenaz-plugin/internal/contextlib"
	"github.com/kameas-ai/kenaz-plugin/internal/features"
	"github.com/kameas-ai/kenaz-plugin/internal/hook"
	"github.com/kameas-ai/kenaz-plugin/internal/mcp"
	contexttools "github.com/kameas-ai/kenaz-plugin/internal/mcp/tools/context"
	"github.com/kameas-ai/kenaz-plugin/internal/mcp/tools/localcontext"
	"github.com/kameas-ai/kenaz-plugin/internal/usertext"
	"github.com/kameas-ai/kenaz-plugin/internal/variant"
)

// The context file settings in config.toml. All are optional. The defaults are
// the least disruptive that still work with no setup: the short index is on,
// which says nothing until there are files; loading always: files is off.
const (
	contextIndexKey       = "context_index"        // on (default) or off
	contextIndexBytesKey  = "context_index_bytes"  // default 1536, 512 to 4096
	contextAlwaysKey      = "context_always"       // off (default) or on
	contextAlwaysBytesKey = "context_always_bytes" // default 4096, 512 to 7680
)

// commandHidden reports a command whose build feature is off: it is then
// neither listed nor runnable.
func commandHidden(name string) bool { return name == "context" && !features.ContextLibrary }

// contextPersonalDir is ~/.kenaz/contexts: beside the plugin's own folder, not
// inside it, so the files outlive the plugin and do not move with its layout.
func (e Env) contextPersonalDir() (string, error) {
	p, err := config.Layout(e.getenv)
	if err != nil {
		return "", err
	}
	return filepath.Join(filepath.Dir(p.ConfigDir), "contexts"), nil
}

// contextViewer is the signed-in account, or nil. Team folders are bound to an account,
// and this is who sees them. It reads the local session, and never talks to Fleet.
func (e Env) contextViewer(ctx context.Context) *contextlib.Account {
	if e.Provider == nil {
		return nil
	}
	s, err := e.Provider.Auth().Session(ctx)
	if err != nil {
		return nil
	}
	return &contextlib.Account{Env: string(s.Env), OrgID: s.OrgID, UserID: s.UserID}
}

// contextLedgerDir holds what Kenaz remembers about context files, or is empty
// when the layout can't be worked out.
func (e Env) contextLedgerDir() string {
	p, err := config.Layout(e.getenv)
	if err != nil {
		return ""
	}
	return contextLedgerIn(p.StateRoot)
}

func contextLedgerIn(stateRoot string) string { return filepath.Join(stateRoot, "context-files") }

// contextOptions reads the settings. A value that is missing or makes no sense
// means the default, and an unreadable config file means all the defaults.
func (e Env) contextOptions() contextlib.Options {
	var f config.File
	if p, err := e.paths(); err == nil {
		if loaded, err := config.LoadFile(p.ConfigFile()); err == nil {
			f = loaded
		}
	}
	return contextOptionsFrom(f)
}

func contextOptionsFrom(f config.File) contextlib.Options {
	word := func(k string) string { return strings.ToLower(strings.TrimSpace(f.Get(k))) }
	size := func(k string, def, hi int) int {
		n, err := strconv.Atoi(word(k))
		if err != nil || n < contextlib.MinBlockBytes || n > hi {
			return def
		}
		return n
	}
	return contextlib.Options{
		Index:       !switchedOff(word(contextIndexKey)),
		IndexBytes:  size(contextIndexBytesKey, contextlib.DefaultIndexBytes, contextlib.MaxIndexBytes),
		Always:      switchedOn(word(contextAlwaysKey)),
		AlwaysBytes: size(contextAlwaysBytesKey, contextlib.DefaultAlwaysBytes, contextlib.MaxSessionBytes),
	}
}

func switchedOff(v string) bool { return v == "off" || v == "false" || v == "no" || v == "0" }
func switchedOn(v string) bool  { return v == "on" || v == "true" || v == "yes" || v == "1" }

// contextOfferWait bounds asking Kameas Fleet whether it offers context files.
const contextOfferWait = 3 * time.Second

// contextFilesOffered asks Kameas Fleet whether it offers team context files to the
// signed-in member's organization: context_files present and true. It fails closed:
// a build without the library, an unofficial build, signed out, and a failed or slow
// read are all a no. It gates only team sharing; the files on this device don't
// need it.
func (e Env) contextFilesOffered(ctx context.Context) bool {
	if !features.ContextLibrary {
		return false
	}
	if e.Provider == nil || e.Provider.Flavor() != fleetapi.FlavorOfficial {
		return false
	}
	if s, err := e.Provider.Auth().Session(ctx); err != nil || s.OrgID == "" || s.UserID == "" {
		return false
	}
	ctx, cancel := context.WithTimeout(ctx, contextOfferWait)
	defer cancel()
	caps, err := e.Provider.Me().Capabilities(ctx)
	return err == nil && fleetapi.HasKey(caps, fleetapi.CapContextFiles)
}

// contextFilesHook is the session-start index, or nil when the feature is off.
// It needs no Fleet env or sign-in.
func (e Env) contextFilesHook() hook.ContextFiles {
	if !features.ContextLibrary {
		return nil
	}
	dir, err := e.contextPersonalDir()
	if err != nil {
		return nil
	}
	return contextFilesIndex{env: e, dir: dir}
}

type contextFilesIndex struct {
	env Env
	dir string
}

func (c contextFilesIndex) Index(ctx context.Context, cwd string) string {
	if cwd == "" {
		cwd, _ = os.Getwd()
	}
	return contextlib.SessionContext(ctx, contextlib.Open(c.dir, cwd).WithViewer(c.env.contextViewer(ctx)), c.env.contextOptions())
}

// contextFilesGroups are the context tools: the local ones (context_list,
// context_read, context_write) in every build with the library, signed in or not,
// and the team-sharing ones (context_share_file, context_pull_files) only while
// Kameas Fleet offers context_files to the organization (asked as the server
// starts). Sharing and pulling ask again on every call.
func (e Env) contextFilesGroups(ctx context.Context) (local, team []mcp.Group) {
	if !features.ContextLibrary {
		return nil, nil
	}
	dir, err := e.contextPersonalDir()
	if err != nil {
		return nil, nil
	}
	ledger := e.contextLedgerDir()
	viewer := func() *contextlib.Account { return e.contextViewer(e.ctx()) }
	local = []mcp.Group{localcontext.NewGroup(localcontext.Options{PersonalDir: dir, LedgerDir: ledger, Viewer: viewer})}
	if e.contextFilesOffered(ctx) {
		team = []mcp.Group{contexttools.NewFilesGroup(contexttools.FilesOptions{PersonalDir: dir, LedgerDir: ledger, Viewer: viewer})}
	}
	return local, team
}

const contextUsage = `usage:
  kenaz context sync [--team name]   share your team's folder, both ways
  kenaz context list [--json]

Edit Markdown in ~/.kenaz/team-contexts/<team>/, or have your AI tool do it, then
run kenaz context sync. It brings your team's changes down, lists what would go up,
and asks once. If you both changed a file, your team's text stays at the file's name,
and yours is saved beside it as "<name> (your version).md". Nothing is lost.

Context files are long-term notes about your company and projects that you
write and edit. Your AI tool can add and change your own files when you ask. Kenaz
sends a file to Kameas Fleet only when you share it and approve the exact text.
Shared files are stored in Kameas Fleet and aren't end-to-end encrypted, under a
short name you can see before you approve, never their full path on your device.
What your AI tool reads becomes part of its session, like the rest of your
conversation.

  ~/.kenaz/team-contexts/<team>/   your team's files: this is what sync shares
  ~/.kenaz/contexts/        your own files, for every project, which stay on this device
  <repo>/.kenaz/context/    project files, shared through git
  ~/.kenaz/team-contexts/   working copies of files your team shared; your AI
                            tool pulls them when you ask

Use .md, .markdown or .txt files, up to 1 MiB each. Names can't contain < > [ ] : or
a backtick, or start with a dot; files with those names are skipped. Project files
come with the repository, and team files can be written by anyone on the team, so
both are treated as untrusted and are never loaded automatically.

Optional frontmatter sets what the session index shows:

  ---
  title: Release process
  description: How we cut and verify a release
  always: true
  ---

Settings in config.toml (none are required):

  context_index = "off"          no index at session start (default on)
  context_index_bytes = "1536"   the index's size limit
  context_always = "on"          load your own always: files at session start
                                 (default off)
  context_always_bytes = "4096"  their total size limit
`

func contextHelp(env Env) {
	fmt.Fprint(env.Stdout, strings.ReplaceAll(contextUsage, "kenaz context", variant.Current.Binary+" context"))
}

// runContext is `kenaz context`. Listing never talks to Fleet, and works signed out for
// everything but team folders; only sync talks to Fleet, from a person's terminal.
func runContext(env Env, args []string) int {
	if len(args) == 0 || args[0] == "help" || args[0] == "-h" || args[0] == "--help" {
		contextHelp(env)
		if len(args) == 0 {
			return ExitUsage
		}
		return ExitOK
	}
	if args[0] == "sync" {
		// Fleet's context_files decides it: runContextSync asks, with the session.
		return runContextSync(env, args[1:])
	}
	if args[0] != "list" {
		fmt.Fprintf(env.Stderr, "%s context: unknown subcommand %q\n\n", variant.Current.Binary, args[0])
		contextHelp(env)
		return ExitUsage
	}
	fs := flag.NewFlagSet("context list", flag.ContinueOnError)
	fs.SetOutput(env.Stderr)
	asJSON := fs.Bool("json", false, "print JSON")
	if err := fs.Parse(args[1:]); err != nil {
		return ExitUsage
	}
	dir, err := env.contextPersonalDir()
	if err != nil {
		fmt.Fprintln(env.Stderr, variant.Current.Binary+": "+err.Error())
		return ExitError
	}
	ctx := env.Ctx
	if ctx == nil {
		ctx = context.Background()
	}
	cwd, _ := os.Getwd()
	set := contextlib.Open(dir, cwd).WithViewer(env.contextViewer(ctx))
	// Scanning is on demand, and so is emptying the trash.
	_ = set.Personal.SweepTrash()
	_ = set.Team.SweepTrash()
	l := set.List(ctx)
	if *asJSON {
		out := map[string]any{"personal_dir": set.Personal.Dir(), "team_dir": set.Team.Dir(), "files": l.Files, "skipped": l.Skipped, "truncated": l.Truncated}
		if len(l.Files) == 0 {
			out["files"] = []contextlib.Entry{}
		}
		if set.Project != nil {
			out["project_dir"] = set.Project.Dir()
		}
		enc := json.NewEncoder(env.Stdout)
		enc.SetIndent("", "  ")
		if err := enc.Encode(out); err != nil {
			return ExitError
		}
		return ExitOK
	}
	writeContextList(env, set, l)
	if l.NeedsSignIn > 0 {
		fmt.Fprintln(env.Stdout, usertext.TeamFilesNeedSignIn)
	}
	return ExitOK
}

func writeContextList(env Env, set contextlib.Set, l contextlib.Listing) {
	w := env.Stdout
	if len(l.Files) == 0 {
		where := set.Personal.Dir()
		if set.Project != nil {
			where += " or " + set.Project.Dir()
		}
		fmt.Fprintf(w, "No context files. Add .md or .txt files to %s.\n", where)
	}
	var section contextlib.Source
	for _, e := range l.Files {
		if e.Source != section {
			section = e.Source
			switch section {
			case contextlib.SourceProject:
				fmt.Fprintf(w, "Project files (untrusted), %s:\n", set.Project.Dir())
			case contextlib.SourceTeam:
				fmt.Fprintf(w, "Team files (untrusted), %s:\n", set.Team.Dir())
			default:
				fmt.Fprintf(w, "Your files, %s:\n", set.Personal.Dir())
			}
		}
		line := "  " + e.Path + "  " + e.Title
		if e.Description != "" {
			line += ": " + e.Description
		}
		if e.TooLarge {
			line += " (over 1 MiB, not readable)"
		}
		if e.Always {
			line += " (always)"
		}
		fmt.Fprintln(w, line)
	}
	if l.Skipped > 0 {
		fmt.Fprintf(w, "Skipped %d that could not be read safely (a link leaving the folder, an unsafe name or a read error).\n", l.Skipped)
	}
	if l.Truncated {
		fmt.Fprintln(w, "This list is cut short: the folder has too many files.")
	}
}
