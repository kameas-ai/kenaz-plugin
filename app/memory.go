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

	"github.com/kameas-ai/kenaz-plugin/internal/config"
	"github.com/kameas-ai/kenaz-plugin/internal/mcp"
	memorytools "github.com/kameas-ai/kenaz-plugin/internal/mcp/tools/memory"
	"github.com/kameas-ai/kenaz-plugin/internal/memory"
)

// memoryConfigKey is the config.toml switch: "off" turns memory off;
// anything else (including no value) leaves it on.
const memoryConfigKey = "memory"

// memoryPaths is where memory lives. It is per OS user, not per Fleet env,
// and does not depend on sign-in.
type memoryPaths struct {
	DB     string // the store
	Marker string // records that the one-time disclosure was shown
	Config string // config.toml (the on/off switch)
}

func (e Env) memoryPaths() (memoryPaths, error) {
	p, err := e.paths()
	if err != nil {
		return memoryPaths{}, err
	}
	return memoryPaths{
		DB:     memory.PathIn(p.StateRoot),
		Marker: filepath.Join(p.StateRoot, memory.DisclosureMarker),
		Config: p.ConfigFile(),
	}, nil
}

func (mp memoryPaths) enabled() bool {
	f, err := config.LoadFile(mp.Config)
	if err != nil {
		return true
	}
	return strings.TrimSpace(strings.ToLower(f.Get(memoryConfigKey))) != "off"
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

// memoryStatusLine is what status and doctor print.
func (mp memoryPaths) statusLine(ctx context.Context) string {
	if !mp.enabled() {
		return "memory: off"
	}
	n := 0
	if s, err := memory.OpenReadOnly(mp.DB, 250*time.Millisecond); err == nil {
		n, _ = s.Count(ctx)
		s.Close()
	}
	unit := "notes"
	if n == 1 {
		unit = "note"
	}
	return fmt.Sprintf("memory: on (%s, %d %s)", mp.DB, n, unit)
}

// memoryGroup is the memory_* MCP tool group for this machine.
func memoryGroup(env Env) mcp.Group {
	mp, err := env.memoryPaths()
	if err != nil {
		return mcp.GroupFunc(func(*sdk.Server, mcp.Deps) {})
	}
	return memorytools.NewGroup(memorytools.Options{
		Path:     mp.DB,
		Enabled:  mp.enabled,
		Disclose: func() string { return mp.discloseOnce(env.now()) },
	})
}

// memoryIndex adapts the store to the session-start hook.
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
	return memory.SessionIndex(ctx, m.mp.DB, p)
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
  kenaz memory forget <id>
  kenaz memory purge [--yes]     delete every memory on this device
  kenaz memory path              print the store's location
  kenaz memory enable | disable  turn memory on or off (default on)

`

func memoryHelp(w io.Writer) {
	fmt.Fprint(w, memoryUsage)
	fmt.Fprintln(w, memory.Notice)
	fmt.Fprintln(w)
	fmt.Fprintln(w, "Your AI tool saves durable facts, decisions, conventions and fixes here; it must never save secrets, credentials or personal data. The store is per OS user: everyone sharing this OS account sees the same memories.")
}

// memoryStdinTerminal is a var so tests can drive the purge prompt.
var memoryStdinTerminal = func() bool {
	fi, err := os.Stdin.Stat()
	return err == nil && fi.Mode()&os.ModeCharDevice != 0
}

var memoryStdin io.Reader = os.Stdin

// runMemory is `kenaz memory ...`. It never talks to Kameas Fleet.
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
		fmt.Fprintln(env.Stderr, "kenaz: "+err.Error())
		return ExitError
	}
	sub, rest := args[0], args[1:]
	for _, a := range rest {
		if a == "-h" || a == "--help" {
			memoryHelp(env.Stdout)
			return ExitOK
		}
	}

	switch sub {
	case "enable":
		if err := config.SetValue(mp.Config, memoryConfigKey, "on"); err != nil {
			fmt.Fprintln(env.Stderr, "kenaz: "+err.Error())
			return ExitError
		}
		fmt.Fprintf(env.Stdout, "Kenaz memory is on (%s).\n", mp.DB)
		showDisclosure(env, mp)
		return ExitOK
	case "disable":
		if err := config.SetValue(mp.Config, memoryConfigKey, "off"); err != nil {
			fmt.Fprintln(env.Stderr, "kenaz: "+err.Error())
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
	case "list", "search", "show", "forget":
	default:
		fmt.Fprintf(env.Stderr, "kenaz memory: unknown command %q\n\n", sub)
		fmt.Fprint(env.Stderr, memoryUsage)
		return ExitUsage
	}

	if !mp.enabled() {
		fmt.Fprintln(env.Stderr, "kenaz: "+memory.OffMessage)
		return ExitError
	}
	showDisclosure(env, mp)

	fs := flag.NewFlagSet("memory "+sub, flag.ContinueOnError)
	fs.SetOutput(env.Stderr)
	project := fs.String("project", "current", "current (this project and global notes) or all")
	limit := fs.Int("limit", memory.DefaultLimit, "maximum entries (1 to 20)")
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
	scope, err := memoryScope(*project)
	if err != nil {
		fmt.Fprintln(env.Stderr, "kenaz memory: "+err.Error())
		return ExitUsage
	}

	ctx := env.Ctx
	if ctx == nil {
		ctx = context.Background()
	}
	switch sub {
	case "list":
		if len(pos) != 0 {
			fmt.Fprint(env.Stderr, memoryUsage)
			return ExitUsage
		}
		return withReader(env, mp, func(s *memory.Store) error {
			es, err := s.Recent(ctx, scope, *limit)
			if err == nil {
				printEntries(env.Stdout, es, false)
			}
			return err
		})
	case "search":
		if len(pos) == 0 {
			fmt.Fprint(env.Stderr, memoryUsage)
			return ExitUsage
		}
		return withReader(env, mp, func(s *memory.Store) error {
			es, err := s.Search(ctx, strings.Join(pos, " "), scope, *limit)
			if err == nil {
				printEntries(env.Stdout, es, true)
			}
			return err
		})
	case "show":
		if len(pos) != 1 {
			fmt.Fprint(env.Stderr, memoryUsage)
			return ExitUsage
		}
		return withReader(env, mp, func(s *memory.Store) error {
			notes, _, err := s.Get(ctx, pos)
			if err != nil {
				return err
			}
			if len(notes) == 0 {
				return memory.ErrNotFound
			}
			n := notes[0]
			w := env.Stdout
			fmt.Fprintf(w, "id:       %s\n", n.ID)
			fmt.Fprintf(w, "title:    %s\n", n.Title)
			fmt.Fprintf(w, "created:  %s\n", n.CreatedAt.Format(time.RFC3339))
			if n.ProjectKey == memory.GlobalProject {
				fmt.Fprintln(w, "project:  global")
			} else {
				fmt.Fprintf(w, "project:  %s (%s)\n", n.ProjectRoot, n.ProjectKey)
			}
			if len(n.Tags) > 0 {
				fmt.Fprintf(w, "tags:     %s\n", strings.Join(n.Tags, ", "))
			}
			fmt.Fprintf(w, "source:   %s\n\n%s\n", n.Source, n.Body)
			return nil
		})
	default: // forget
		if len(pos) != 1 {
			fmt.Fprint(env.Stderr, memoryUsage)
			return ExitUsage
		}
		if _, err := os.Stat(mp.DB); err != nil {
			fmt.Fprintln(env.Stderr, "kenaz: "+memory.ErrNotFound.Error())
			return ExitError
		}
		s, err := memory.Open(mp.DB)
		if err != nil {
			fmt.Fprintln(env.Stderr, "kenaz: "+err.Error())
			return ExitError
		}
		defer s.Close()
		if err := s.Forget(ctx, pos[0]); err != nil {
			fmt.Fprintln(env.Stderr, "kenaz: "+err.Error())
			return ExitError
		}
		fmt.Fprintf(env.Stdout, "Deleted memory %s.\n", pos[0])
		return ExitOK
	}
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

// withReader runs fn on the store opened read-only; no store yet is an
// empty result.
func withReader(env Env, mp memoryPaths, fn func(*memory.Store) error) int {
	s, err := memory.OpenReadOnly(mp.DB, 2*time.Second)
	if errors.Is(err, memory.ErrNoStore) {
		fmt.Fprintln(env.Stdout, "No memories saved on this device yet.")
		return ExitOK
	}
	if err == nil {
		defer s.Close()
		err = fn(s)
	}
	if err != nil {
		fmt.Fprintln(env.Stderr, "kenaz: "+err.Error())
		return ExitError
	}
	return ExitOK
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
		fmt.Fprintln(w)
		if snippets && e.Snippet != "" {
			fmt.Fprintf(w, "    %s\n", e.Snippet)
		}
	}
}

func memoryPurge(env Env, mp memoryPaths, args []string) int {
	fs := flag.NewFlagSet("memory purge", flag.ContinueOnError)
	fs.SetOutput(env.Stderr)
	yes := fs.Bool("yes", false, "delete without asking")
	if err := fs.Parse(args); err != nil || fs.NArg() != 0 {
		return ExitUsage
	}
	if _, err := os.Stat(mp.DB); errors.Is(err, os.ErrNotExist) {
		fmt.Fprintf(env.Stdout, "No Kenaz memory on this device (%s).\n", mp.DB)
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
			fmt.Fprintln(env.Stderr, "kenaz: purge deletes every memory on this device; run it in a terminal to confirm, or pass --yes.")
			return ExitError
		}
		fmt.Fprintf(env.Stdout, "Delete %s memories in %s? This cannot be undone. [y/N] ", n, mp.DB)
		line, _ := bufio.NewReader(memoryStdin).ReadString('\n')
		if a := strings.ToLower(strings.TrimSpace(line)); a != "y" && a != "yes" {
			fmt.Fprintln(env.Stdout, "Nothing was deleted.")
			return ExitOK
		}
	}
	if err := memory.Purge(mp.DB); err != nil {
		fmt.Fprintln(env.Stderr, "kenaz: "+err.Error())
		return ExitError
	}
	fmt.Fprintf(env.Stdout, "Deleted your Kenaz memory (%s).\n", mp.DB)
	return ExitOK
}

// memoryJSON is the memory section of `kenaz status --json`.
func memoryJSON(env Env, mp memoryPaths) map[string]any {
	out := map[string]any{"enabled": mp.enabled(), "path": mp.DB}
	if mp.enabled() {
		n := 0
		if s, err := memory.OpenReadOnly(mp.DB, 250*time.Millisecond); err == nil {
			n, _ = s.Count(env.Ctx)
			s.Close()
		}
		out["notes"] = n
	}
	return out
}
