package app

import (
	"context"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/kameas-ai/kenaz-plugin/fleetapi"
	"github.com/kameas-ai/kenaz-plugin/internal/daemon"
	"github.com/kameas-ai/kenaz-plugin/internal/enroll"
	"github.com/kameas-ai/kenaz-plugin/internal/skills"
)

// Skills wiring. The MCP group (mcpcmd.go), the daemon lane
// (daemoncmd.go) and the `skills` subcommand (main.go) are registered
// explicitly in those tables; there are no init() side effects.

// skillsHome is the home directory whose harness skill roots are managed.
var skillsHome = os.UserHomeDir

func skillsDaemonHook(opt *daemon.Options) (func(*daemon.Daemon), error) {
	home, err := skillsHome()
	if err != nil {
		return nil, err
	}
	nodeID, err := enroll.LoadOrCreateNodeID(opt.StateDir, time.Now())
	if err != nil {
		return nil, err
	}
	lane := newSkillsLane(opt.Provider, opt.StateDir, home, nodeID, opt.Logger)
	opt.Lanes = append(opt.Lanes, lane)
	opt.Contributors = append(opt.Contributors, lane)
	return nil, nil
}

// newSkillsLane builds the daemon's skills lane. The lane is the config
// poll for mandated items: it fetches GET /configs (ETag/304) every
// skills.DefaultInterval or on Kick, and the daemon holds/resumes it with the
// other lanes on org pause and lockdown.
func newSkillsLane(p fleetapi.Provider, stateDir, home, nodeID string, log *slog.Logger) *skills.Lane {
	return &skills.Lane{Syncer: &skills.Syncer{
		Provider:  p,
		Store:     skills.NewStore(stateDir),
		Mat:       &skills.Materializer{Roots: skills.DetectRoots(home)},
		MachineID: nodeID,
		Log:       log,
	}}
}

// purgeSkills removes every Kameas-managed skill directory and the local
// skills state, independent of whether the daemon is running (logout and
// withdrawal: org content does not outlive the session). Unmarked
// directories are never touched.
func purgeSkills(p fleetapi.Provider, stateDir string) error {
	home, err := skillsHome()
	if err != nil {
		return err
	}
	return newSkillsLane(p, stateDir, home, "", nil).Purge()
}

// skillsReload implements hook.Skills for the Claude SessionStart hook.
type skillsReload struct{ stateDir string }

func (s skillsReload) ChangedSinceLastScan(context.Context) bool {
	return skills.NewStore(s.stateDir).ReloadNeeded()
}

// runSkills implements `kenaz skills [list|sync]`.
func runSkills(env Env, args []string) int {
	fs := flag.NewFlagSet("skills", flag.ContinueOnError)
	fs.SetOutput(env.Stderr)
	envName := fs.String("env", "", "Fleet environment")
	if err := fs.Parse(args); err != nil {
		return ExitUsage
	}
	r, err := env.resolve(*envName)
	if err != nil {
		fmt.Fprintln(env.Stderr, "kenaz: "+err.Error())
		return ExitUsage
	}
	home, err := skillsHome()
	if err != nil {
		fmt.Fprintln(env.Stderr, "kenaz: "+err.Error())
		return ExitError
	}
	mat := &skills.Materializer{Roots: skills.DetectRoots(home)}
	store := skills.NewStore(r.StateDir)
	sub := "list"
	if rest := fs.Args(); len(rest) > 0 {
		sub = rest[0]
	}
	switch sub {
	case "list":
		owned, err := mat.Owned()
		if err != nil {
			fmt.Fprintln(env.Stderr, "kenaz: "+err.Error())
			return ExitError
		}
		if len(mat.Roots) == 0 {
			fmt.Fprintln(env.Stdout, "No supported agent harness found in your home directory.")
			return ExitOK
		}
		if len(owned) == 0 {
			fmt.Fprintln(env.Stdout, "No Kameas Fleet skills installed.")
		} else {
			tw := tabwriter.NewWriter(env.Stdout, 0, 2, 2, ' ', 0)
			fmt.Fprintln(tw, "SKILL\tVERSION\tSOURCE\tHARNESS\tNOTES")
			for _, o := range owned {
				var notes []string
				if len(o.Marker.Stripped) > 0 {
					notes = append(notes, "without "+strings.Join(o.Marker.Stripped, ", "))
				}
				if o.Marker.TranslatedFrom != "" {
					notes = append(notes, "translated")
				}
				fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\n", o.Marker.Slug, o.Marker.Version, o.Marker.Source, o.Root.Harness, strings.Join(notes, "; "))
			}
			tw.Flush()
		}
		if st, err := store.LoadSyncState(); err == nil {
			for _, n := range skills.StatusNotes(st.Report) {
				fmt.Fprintln(env.Stdout, n)
			}
		}
		return ExitOK
	case "sync":
		nodeID, err := enroll.LoadOrCreateNodeID(r.StateDir, env.now())
		if err != nil {
			fmt.Fprintln(env.Stderr, "kenaz: "+err.Error())
			return ExitError
		}
		s := &skills.Syncer{Provider: env.Provider, Store: store, Mat: mat, MachineID: nodeID}
		rep, err := s.Sync(env.Ctx)
		if err != nil {
			return providerErr(env, err)
		}
		fmt.Fprintf(env.Stdout, "Skills synced: %d installed or current, %d failed, %d removed.\n", rep.Applied, rep.Failed, rep.Removed)
		for _, n := range skills.StatusNotes(&rep) {
			fmt.Fprintln(env.Stdout, n)
		}
		return ExitOK
	default:
		fmt.Fprintln(env.Stderr, "usage: kenaz skills [list|sync]")
		return ExitUsage
	}
}
