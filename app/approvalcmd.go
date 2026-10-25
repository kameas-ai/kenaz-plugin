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
	"github.com/kameas-ai/kenaz-plugin/internal/credstore"
	"github.com/kameas-ai/kenaz-plugin/internal/daemon"
	contexttools "github.com/kameas-ai/kenaz-plugin/internal/mcp/tools/context"
	skillstools "github.com/kameas-ai/kenaz-plugin/internal/mcp/tools/skills"
	"github.com/kameas-ai/kenaz-plugin/internal/spool"
	"github.com/kameas-ai/kenaz-plugin/internal/variant"
)

// Human-authorized writes. The daemon hosts
// the approval service and page; the MCP process prepares requests over the
// daemon's control API; `kenaz approvals` can only list requests and open
// the page. Nothing in the CLI or MCP surface approves, enrolls an
// authenticator or exports a receipt.

// approvalsDir is the pending-write store for an env.
func approvalsDir(stateDir string) string { return filepath.Join(stateDir, "approvals") }

// ApprovalRegistrar registers executors (and reconcilers) for approval
// operations with the daemon's approval service. It runs in the daemon
// before it serves. status reports lockdown/pause for write rechecks.
type ApprovalRegistrar func(m *approvals.Manager, p fleetapi.Provider, stateDir string, status contexttools.StatusSource)

// approvalRegistrars are the registered write operations. Other
// workstreams (memory) append their registrar here.
var approvalRegistrars = []ApprovalRegistrar{registerFleetWriteExecutors}

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

// identityVerifier adapts the private client's optional browser identity
// verification. Without it (public builds, older clients) enrollment is
// unavailable.
type identityVerifier struct {
	v   fleetapi.IdentityVerifier
	env fleetapi.Env
}

func (iv identityVerifier) VerifyIdentity(ctx context.Context, open func(string) error, completeURL string) (approvals.Identity, error) {
	start := time.Now().Add(-time.Minute) // clock skew allowance
	got, err := iv.v.VerifyIdentity(ctx, fleetapi.LoginRequest{Env: iv.env, OpenURL: open, CompleteURL: completeURL, Timeout: 10 * time.Minute})
	if err != nil {
		return approvals.Identity{}, err
	}
	// Only a sign-in completed during this ceremony counts; a reused
	// identity-provider session does not.
	if got.AuthTime.IsZero() || got.AuthTime.Before(start) {
		return approvals.Identity{}, errors.New("identity verification was not a fresh sign-in")
	}
	return approvals.Identity{Env: string(got.Env), OrgID: got.OrgID, UserID: got.UserID}, nil
}

func verifierFor(p fleetapi.Provider, env fleetapi.Env) approvals.IdentityVerifier {
	if p.Flavor() != fleetapi.FlavorOfficial {
		return nil
	}
	if v, ok := p.Auth().(fleetapi.IdentityVerifier); ok {
		return identityVerifier{v: v, env: env}
	}
	return nil
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
		Dir:      approvalsDir(opt.StateDir),
		Identity: sessionIdentity(opt.Provider),
		Verifier: verifierFor(opt.Provider, opt.Env),
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

// refusedApprovalVerbs are subcommands people (or agents) might try. The
// CLI can never approve, enroll or export anything.
var refusedApprovalVerbs = map[string]bool{
	"approve": true, "accept": true, "confirm": true, "allow": true, "yes": true, "deny": true, "cancel": true,
	"register": true, "add-key": true, "export": true, "receipt": true, "token": true, "sign": true,
}

const approvalsUsage = "usage: kenaz approvals [list | status <request_id> | open [request_id] | enroll]"

// runApprovals implements `kenaz approvals`. It shows pending and completed
// requests and opens the approval page; approving, canceling and setting up
// an authenticator happen only on that page.
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
		fmt.Fprintln(env.Stderr, variant.Current.Binary+": the CLI cannot approve, cancel or set up anything. Open the approval page with `kenaz approvals open <request_id>` and use your security key or device unlock there.")
		return ExitUsage
	}
	switch sub {
	case "list", "status", "open", "enroll":
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
		if !l.Enrolled {
			fmt.Fprintln(env.Stdout, "No authenticator is set up for approvals yet. Run `kenaz approvals enroll`.")
		}
		fmt.Fprintln(env.Stdout, "Approve or cancel requests at "+l.PageURL+"r/<request>  (`kenaz approvals open <request>`).")
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
	switch {
	case sub == "enroll":
		url += "enroll"
	case len(rest) > 1:
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
