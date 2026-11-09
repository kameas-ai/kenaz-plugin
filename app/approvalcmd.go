package app

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"path/filepath"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/kameas-ai/kenaz-plugin/fleetapi"
	"github.com/kameas-ai/kenaz-plugin/internal/approvals"
	"github.com/kameas-ai/kenaz-plugin/internal/contextlib"
	"github.com/kameas-ai/kenaz-plugin/internal/credstore"
	"github.com/kameas-ai/kenaz-plugin/internal/daemon"
	"github.com/kameas-ai/kenaz-plugin/internal/features"
	contexttools "github.com/kameas-ai/kenaz-plugin/internal/mcp/tools/context"
	skillstools "github.com/kameas-ai/kenaz-plugin/internal/mcp/tools/skills"
	"github.com/kameas-ai/kenaz-plugin/internal/spool"
	"github.com/kameas-ai/kenaz-plugin/internal/variant"
)

// Human-approved writes. The daemon hosts
// the approval service and page; the MCP process prepares requests over the
// daemon's control API; `kenaz approvals` can only list requests and open
// the page. Nothing in the CLI or MCP surface approves, enrolls an
// exports a receipt.

// approvalsDir is the pending-write store for an env.
func approvalsDir(stateDir string) string { return filepath.Join(stateDir, "approvals") }

// ApprovalRegistrar registers executors (and reconcilers) for approval
// operations with the daemon's approval service. It runs in the daemon
// before it serves. status reports lockdown/pause for write rechecks.
type ApprovalRegistrar func(m *approvals.Manager, p fleetapi.Provider, stateDir string, status contexttools.StatusSource)

// approvalRegistrars are the registered write operations. Other
// workstreams (memory) append their registrar here.
var approvalRegistrars = []ApprovalRegistrar{registerFleetWriteExecutors, registerContextFileExecutors}

func registerFleetWriteExecutors(m *approvals.Manager, p fleetapi.Provider, _ string, st contexttools.StatusSource) {
	m.Register(skillstools.PublishOperation, skillstools.ExecutePublish(p))
	m.RegisterReconciler(skillstools.PublishOperation, skillstools.ReconcilePublish(p))
	m.Register(contexttools.ShareOperation, contexttools.ExecuteShare(p, st))
	m.RegisterReconciler(contexttools.ShareOperation, contexttools.ReconcileShare(p))
	m.Register(contexttools.PromoteOperation, contexttools.ExecutePromote(p, st))
	m.RegisterReconciler(contexttools.PromoteOperation, contexttools.ReconcilePromote(p))
	m.RegisterRenderer(skillstools.PublishOperation, skillstools.RenderPublish)
	m.RegisterRenderer(contexttools.ShareOperation, contexttools.RenderShare(teamNames(p)))
	m.RegisterRenderer(contexttools.PromoteOperation, contexttools.RenderPromote(teamNames(p)))
}

// registerContextFileExecutors is sharing a local context file, in builds with
// the context library. The relay keeps the file's link in the same ledger the
// tools read.
func registerContextFileExecutors(m *approvals.Manager, p fleetapi.Provider, stateDir string, st contexttools.StatusSource) {
	if !features.ContextLibrary {
		return
	}
	led := contextlib.NewLedger(contextLedgerIn(filepath.Dir(stateDir)))
	m.Register(contexttools.ShareFileOperation, contexttools.ExecuteShareFile(p, st, led))
	m.RegisterReconciler(contexttools.ShareFileOperation, contexttools.ReconcileShareFile(p, led))
	m.RegisterRenderer(contexttools.ShareFileOperation, contexttools.RenderShareFile(teamNames(p)))
}

// teamNames resolves a team id to its display name for the approval page
// (best effort; the page shows the id too).
func teamNames(p fleetapi.Provider) func(string) string {
	return func(id string) string {
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		me, err := p.Me().Get(ctx)
		if err != nil {
			return ""
		}
		for _, t := range me.Teams {
			if t.ID == id {
				return t.Name
			}
		}
		return ""
	}
}

// approvalsKeyItem names the approvals store key in the OS keychain.
func approvalsKeyItem(e fleetapi.Env) string { return "approvals-" + string(e) }

// approvalsStoreKey returns the key that authenticates the approvals store,
// kept in the OS keychain. With the file credential store (no OS keychain)
// there is no key outside user-writable files, so it returns nil and
// approvals are unavailable rather than trusting files an agent could
// write.
//
// The keychain protects against agents that act only through MCP tools or
// write files. It is not a boundary against a same-user agent with a
// shell: on macOS go-keyring creates the item via /usr/bin/security, which
// lets any process of the user read it without a prompt. That agent can
// read Fleet tokens too and is out of scope (README "Threat model").
func approvalsStoreKey(env Env, e fleetapi.Env, create bool) []byte {
	if env.cred == nil || env.cred.store != credstore.Keychain {
		return nil
	}
	if create {
		k, err := spool.GetOrCreateKey(env.keyStore(), approvalsKeyItem(e))
		if err != nil {
			return nil
		}
		return k
	}
	k, err := env.keyStore().Get(approvalsKeyItem(e))
	if err != nil || len(k) < 32 {
		return nil
	}
	return k
}

// sessionIdentity reads the active session (no network I/O).
func sessionIdentity(p fleetapi.Provider) approvals.IdentitySource {
	return func(ctx context.Context) (approvals.Identity, error) {
		s, err := p.Auth().Session(ctx)
		if err != nil {
			return approvals.Identity{}, err
		}
		return approvals.Identity{Env: string(s.Env), OrgID: s.OrgID, UserID: s.UserID}, nil
	}
}

// daemonStatus adapts the daemon to contexttools.StatusSource.
type daemonStatus struct{ d *daemon.Daemon }

func (s daemonStatus) Status(ctx context.Context) (daemon.Status, error) { return s.d.Status(ctx), nil }

// approvalsDaemonHook installs the approval service in the daemon: the
// page and control routes, the executors and a lane whose Purge (logout,
// node removal) cancels every open request.
func approvalsDaemonHook(env Env) DaemonHook {
	return func(opt *daemon.Options) (func(*daemon.Daemon), error) {
		return installApprovals(opt, approvalsStoreKey(env, opt.Env, true))
	}
}

func installApprovals(opt *daemon.Options, key []byte) (func(*daemon.Daemon), error) {
	var d *daemon.Daemon
	m, err := approvals.New(approvals.Options{
		StoreKey: key,
		Env:      string(opt.Env),
		Dir:      approvalsDir(opt.StateDir),
		Identity: sessionIdentity(opt.Provider),
		BaseURL:  func() string { return fmt.Sprintf("http://localhost:%d", d.Info().Port) },
		Logger:   opt.Logger,
	})
	if err != nil {
		return nil, err
	}
	opt.Lanes = append(opt.Lanes, approvalsLane{m})
	p, stateDir := opt.Provider, opt.StateDir
	return func(dd *daemon.Daemon) {
		d = dd
		for _, r := range approvalRegistrars {
			r(m, p, stateDir, daemonStatus{dd})
		}
		m.Mount(dd.Handle, dd.HandleControl)
	}, nil
}

// approvalsLane hooks the approval service into the daemon lifecycle.
type approvalsLane struct{ m *approvals.Manager }

func (approvalsLane) Name() string { return "approvals" }
func (l approvalsLane) Start(ctx context.Context) error {
	go l.m.Run(ctx)
	return nil
}
func (approvalsLane) Stop() error    { return nil }
func (approvalsLane) Hold(string)    {}
func (approvalsLane) Resume()        {}
func (l approvalsLane) Purge() error { l.m.SignOut(); return nil }
func (l approvalsLane) Status() daemon.LaneStatus {
	n := 0
	for _, r := range l.m.List(context.Background()) {
		if r.Status == approvals.StatusPending {
			n++
		}
	}
	return daemon.LaneStatus{Name: "approvals", State: "running", Held: int64(n)}
}

// approvalsClient is the RemoteClient for a CLI or MCP process.
func approvalsClient(conn func(context.Context) *daemon.Client) approvals.RemoteClient {
	return approvals.RemoteClient{Conn: func(ctx context.Context) (approvals.Doer, error) {
		if c := conn(ctx); c != nil {
			return c, nil
		}
		return nil, daemon.ErrNotRunning
	}}
}

// refusedApprovalVerbs are subcommands people (or agents) might try here. Approving has
// its own commands, which need a real terminal.
var refusedApprovalVerbs = map[string]bool{
	"approve": true, "accept": true, "confirm": true, "allow": true, "yes": true, "deny": true, "cancel": true, "decline": true,
	"enroll": true, "register": true, "add-key": true, "export": true, "receipt": true, "token": true, "sign": true,
}

const approvalsUsage = "usage: kenaz approvals [list | status <request_id> | open [request_id]]"

// runApprovals implements `kenaz approvals`. It shows requests and opens the approval
// page. Approving is the page's button, or `kenaz approve` and `kenaz decline`.
func runApprovals(env Env, args []string) int {
	fs := flag.NewFlagSet("approvals", flag.ContinueOnError)
	fs.SetOutput(env.Stderr)
	envName := fs.String("env", "", "Fleet environment")
	noBrowser := fs.Bool("no-browser", false, "print the page URL instead of opening a browser")
	if err := fs.Parse(args); err != nil {
		return ExitUsage
	}
	rest := fs.Args()
	sub := "list"
	if len(rest) > 0 {
		sub = rest[0]
	}
	if refusedApprovalVerbs[strings.ToLower(sub)] {
		fmt.Fprintln(env.Stderr, variant.Current.Binary+": use `kenaz approve [id]` or `kenaz decline [id]` in a terminal, or the page from `kenaz approvals open <request_id>`.")
		return ExitUsage
	}
	switch sub {
	case "list", "status", "open":
	default:
		fmt.Fprintln(env.Stderr, approvalsUsage)
		return ExitUsage
	}
	if sub == "status" && (len(rest) != 2 || !approvals.ValidID(rest[1])) {
		fmt.Fprintln(env.Stderr, approvalsUsage)
		return ExitUsage
	}
	if sub == "open" && len(rest) > 1 && !approvals.ValidID(rest[1]) {
		fmt.Fprintln(env.Stderr, variant.Current.Binary+": not a request id: "+rest[1])
		return ExitUsage
	}
	r, err := env.resolve(*envName)
	if err != nil {
		fmt.Fprintln(env.Stderr, variant.Current.Binary+": "+err.Error())
		return ExitUsage
	}
	spawn := r.spawner(env)
	rc := approvalsClient(func(ctx context.Context) *daemon.Client {
		c, err := daemon.EnsureRunning(ctx, r.StateDir, spawn, 3*time.Second)
		if err != nil {
			return nil
		}
		return c
	})
	l, err := rc.List(env.Ctx)
	if err != nil {
		if errors.Is(err, approvals.ErrNotSignedIn) {
			fmt.Fprintln(env.Stderr, variant.Current.Binary+": not signed in; run `kenaz login`")
			return ExitError
		}
		fmt.Fprintln(env.Stderr, variant.Current.Binary+": the Kenaz relay could not be reached: "+err.Error())
		return ExitError
	}
	switch sub {
	case "list":
		if len(l.Requests) == 0 {
			fmt.Fprintln(env.Stdout, "No write requests.")
		} else {
			tw := tabwriter.NewWriter(env.Stdout, 0, 2, 2, ' ', 0)
			fmt.Fprintln(tw, "REQUEST\tOPERATION\tSTATUS\tCREATED\tEXPIRES")
			for _, q := range l.Requests {
				exp := ""
				if q.Status == approvals.StatusPending {
					exp = q.ExpiresAt.Local().Format("15:04:05")
				}
				fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\n", q.RequestID, q.Operation, q.Status, q.CreatedAt.Local().Format("2006-01-02 15:04"), exp)
			}
			tw.Flush()
		}
		fmt.Fprintln(env.Stdout, "Approve or decline with `kenaz approve [request]`, or at "+l.PageURL+"r/<request>.")
		return ExitOK
	case "status":
		st, res, err := rc.Status(env.Ctx, rest[1])
		if err != nil {
			fmt.Fprintln(env.Stderr, variant.Current.Binary+": "+err.Error())
			return ExitError
		}
		fmt.Fprintln(env.Stdout, string(st))
		if len(res) > 0 {
			fmt.Fprintln(env.Stdout, string(res))
		}
		return ExitOK
	}
	url := l.PageURL
	if len(rest) > 1 {
		url += "r/" + rest[1]
	}
	fmt.Fprintf(env.Stdout, "Open the Kenaz approval page:\n  %s\n", url)
	if *noBrowser {
		return ExitOK
	}
	open := env.OpenBrowser
	if open == nil {
		open = openBrowser
	}
	if err := open(url); err != nil {
		fmt.Fprintf(env.Stdout, "Could not open a browser here (%s). Open the link above in a browser on this machine.\n", err.Error())
	}
	return ExitOK
}
