package app

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"fmt"
	"path/filepath"
	"time"

	"github.com/kameas-ai/kenaz-plugin/fleetapi"
	"github.com/kameas-ai/kenaz-plugin/internal/approvals"
	"github.com/kameas-ai/kenaz-plugin/internal/contextlib"
	"github.com/kameas-ai/kenaz-plugin/internal/credstore"
	"github.com/kameas-ai/kenaz-plugin/internal/daemon"
	contexttools "github.com/kameas-ai/kenaz-plugin/internal/mcp/tools/context"
	"github.com/kameas-ai/kenaz-plugin/internal/safetext"
	"github.com/kameas-ai/kenaz-plugin/internal/spool"
	"github.com/kameas-ai/kenaz-plugin/internal/usertext"
	"github.com/kameas-ai/kenaz-plugin/internal/variant"
)

// The context share grant (`kenaz context grant on|off|status`). A person turns it on
// here, at a real terminal, after reading what it does and typing y. This file's `on`
// branch is the only caller of ShareGrant.Give: no MCP tool, control route, hook,
// environment variable or config.toml key gives the grant. The relay reads it when
// context_share_file prepares a share. What the grant file's seal does and doesn't
// stop is in internal/contextlib/grant.go.

// contextShareGrantDir holds the grant and its history for one Fleet environment.
func contextShareGrantDir(stateDir string) string {
	return filepath.Join(stateDir, "context-share-grant")
}

// contextShareGrantText is what turning the grant on does, shown before the question.
func contextShareGrantText() string {
	return fmt.Sprintf(`Turning this on lets your AI tool share context files with your teams in Kameas Fleet
without asking you to approve each one.

  - You won't see a file's text before your team can read it. Kenaz still checks it
    for credentials and hidden characters, and Kameas Fleet checks it again.
  - It covers sharing a context file with a team (the context_share_file tool), for the
    account signed in now, up to %d files per clock hour (the count starts again on
    the hour). Past that, shares ask again.
  - It covers every team you are in, including the all-members team (usually named
    Everyone). A file shared with that team is one your whole organization can read.
  - These still ask every time: text your AI tool marked as drafted from your memory
    notes, promoting a note to your whole organization, shared notes, skills and
    memory. Kenaz can't tell memory-derived text your AI tool didn't mark.
  - Files shared this way are listed by `+"`%[2]s context grant status`"+`, from a
    record on this device that signing out deletes.
  - Turn it off with `+"`%[2]s context grant off`"+`. Signing out turns it off too.
`, contextlib.MaxGrantedSharesPerHour, variant.Current.Binary)
}

const (
	contextGrantUsage    = "usage: %s context grant on|off|status\n"
	contextGrantQuestion = "Turn on sharing context files without asking each time?"
	contextGrantOn       = "On. Your AI tool can now share context files with your teams without asking each time."
	contextGrantOff      = "Off. Sharing a context file asks for your approval each time."
	contextGrantNoOrg    = "your organization doesn't allow sharing context files without an approval for each"
)

// shareGrantBinding is what a grant is tied to in the OS keychain, for one Fleet
// environment: a random id made when the grant is turned on and deleted when it ends,
// and the environment's spool key. Every `kenaz logout` deletes the spool key,
// including an older release's that knows nothing of the grant, and the next relay
// makes a new one; so a grant can't outlive the sign-in it was given in, and a copy of
// its file put back later is no grant.
type shareGrantBinding struct {
	ks   spool.KeyStore
	env  fleetapi.Env
	keep bool // the credential store is the OS keychain
}

func shareGrantItem(e fleetapi.Env) string { return "context-share-grant-" + string(e) }

func (b shareGrantBinding) Current() []byte {
	if !b.keep {
		return nil
	}
	id, err := b.ks.Get(shareGrantItem(b.env))
	if err != nil || len(id) != 32 {
		return nil
	}
	session, err := b.ks.Get(string(b.env))
	if err != nil || len(session) != 32 {
		return nil
	}
	sum := sha256.Sum256(append([]byte("kenaz-context-share-grant-session\x00"), session...))
	return append(id, sum[:]...)
}

func (b shareGrantBinding) Renew() error {
	if !b.keep {
		return contextlib.ErrGrantUnavailable
	}
	if _, err := spool.GetOrCreateKey(b.ks, string(b.env)); err != nil {
		return err
	}
	id := make([]byte, 32)
	if _, err := rand.Read(id); err != nil {
		return err
	}
	return b.ks.Set(shareGrantItem(b.env), id)
}

func (b shareGrantBinding) Forget() error { return b.ks.Delete(shareGrantItem(b.env)) }

// shareGrant is the grant store for one Fleet environment. create makes the approvals
// keychain key if there is none yet.
func (e Env) shareGrant(stateDir string, fe fleetapi.Env, create bool) contextlib.ShareGrant {
	bind := shareGrantBinding{ks: e.keyStore(), env: fe, keep: e.cred != nil && e.cred.store == credstore.Keychain}
	return contextlib.NewShareGrant(contextShareGrantDir(stateDir), approvalsStoreKey(e, fe, create), bind)
}

// accountShareGrant is the grant store for r's environment and the signed-in account,
// or nil when signed out.
func (e Env) accountShareGrant(ctx context.Context, r resolved, create bool) (contextlib.ShareGrant, *contextlib.Account) {
	return e.shareGrant(r.StateDir, r.Env, create), e.contextViewer(ctx)
}

// runContextGrant is `kenaz context grant`.
func runContextGrant(env Env, args []string) int {
	if len(args) != 1 {
		fmt.Fprintf(env.Stderr, contextGrantUsage, variant.Current.Binary)
		return ExitUsage
	}
	fail := func(msg string) int {
		fmt.Fprintln(env.Stderr, variant.Current.Binary+": "+msg)
		return ExitError
	}
	r, err := env.resolve("")
	if err != nil {
		return fail(err.Error())
	}
	ctx := env.ctx()
	switch args[0] {
	case "off":
		// Turning it off only reduces what leaves the device, so it needs no terminal
		// and no sign-in.
		g, _ := env.accountShareGrant(ctx, r, false)
		if err := g.Revoke(); err != nil {
			return fail(err.Error())
		}
		fmt.Fprintln(env.Stdout, contextGrantOff)
		return ExitOK
	case "status":
		g, acct := env.accountShareGrant(ctx, r, false)
		if acct == nil {
			_ = g.Revoke()
			fmt.Fprintf(env.Stdout, "Signed out, so this is off. Sign in with `%s login`.\n", variant.Current.Binary)
			return ExitOK
		}
		writeContextGrantStatus(env, g, *acct, contexttools.OrgAllowsShareGrant(ctx, env.Provider))
		return ExitOK
	case "on":
	default:
		fmt.Fprintf(env.Stderr, contextGrantUsage, variant.Current.Binary)
		return ExitUsage
	}
	if env.Provider == nil || env.Provider.Flavor() != fleetapi.FlavorOfficial {
		return fail(fleetapi.ErrUnofficialBuild.Error())
	}
	if !contexttools.OrgAllowsShareGrant(ctx, env.Provider) {
		return fail("nothing changed: " + contextGrantNoOrg)
	}
	// With no terminal it refuses: an agent can't answer for the person.
	if !memoryStdinTerminal() {
		return fail(usertext.NeedsTerminal(variant.Current.Binary + " context grant on"))
	}
	g, acct := env.accountShareGrant(ctx, r, true)
	if acct == nil || acct.OrgID == "" || acct.UserID == "" {
		return fail("not signed in; run `" + variant.Current.Binary + " login`")
	}
	if !g.Available() {
		return fail("nothing changed: this needs the device's keychain, which isn't available here")
	}
	fmt.Fprintln(env.Stdout, contextShareGrantText())
	if !confirm(env, contextGrantQuestion) {
		fmt.Fprintln(env.Stdout, usertext.NothingChanged)
		return ExitOK
	}
	// The answer was for the account read above: check it is still the one signed in.
	if now := env.contextViewer(ctx); now == nil || *now != *acct {
		return fail("nothing changed: the signed-in account changed")
	}
	if err := g.Give(*acct, env.now()); err != nil {
		return fail("nothing changed: " + err.Error())
	}
	fmt.Fprintln(env.Stdout, contextGrantOn)
	return ExitOK
}

// maxGrantHistoryShown bounds the shares `kenaz context grant status` lists.
const maxGrantHistoryShown = 50

func writeContextGrantStatus(env Env, g contextlib.ShareGrant, acct contextlib.Account, orgAllows bool) {
	w := env.Stdout
	since, on := g.On(acct)
	switch {
	case on && !orgAllows:
		fmt.Fprintln(w, "Off: "+contextGrantNoOrg+".")
	case on:
		fmt.Fprintf(w, "On since %s: your AI tool can share context files with your teams without asking each time.\n", since.Local().Format("2006-01-02 15:04"))
		fmt.Fprintf(w, "Used this hour: %d of %d. Turn it off with `%s context grant off`.\n", g.UsedThisHour(acct, env.now()), contextlib.MaxGrantedSharesPerHour, variant.Current.Binary)
		if n := g.RefusedThisHour(acct, env.now()); n >= contextlib.MaxRefusedGrantSharesPerHour {
			fmt.Fprintf(w, "Paused until the next hour: %d shares this hour were refused because you are not a member of the team, so sharing asks for your approval until then.\n", n)
		}
	default:
		fmt.Fprintf(w, "Off: sharing a context file asks for your approval each time. Turn it on with `%s context grant on`.\n", variant.Current.Binary)
	}
	hist := g.History(acct)
	if len(hist) == 0 {
		fmt.Fprintln(w, "Nothing has been shared without an approval.")
		return
	}
	fmt.Fprintln(w, "Shared without an approval, newest first (file, team, sha256 of the text, result). This list is kept on this device and goes when you sign out:")
	for i := len(hist) - 1; i >= 0; i-- {
		if len(hist)-i > maxGrantHistoryShown {
			fmt.Fprintf(w, "  and %d older\n", i+1)
			break
		}
		e := hist[i]
		team := e.TeamID
		if e.TeamName != "" {
			team = e.TeamName + " (" + e.TeamID + ")"
		}
		fmt.Fprintf(w, "  %s  %s  %s  %s  %s\n", e.At.Local().Format("2006-01-02 15:04"), safetext.Terminal(e.Slug), safetext.Terminal(safetext.OneLine(team, 80)), e.SHA256, grantResultText(e))
	}
}

func grantResultText(e contextlib.ShareAudit) string {
	status := e.Status
	switch {
	case !e.Counts():
		return "not shared: you are not a member of this team (not counted toward the hourly limit)"
	case e.RefusedNotMember():
		return "not shared: you are not a member of this team"
	}
	switch approvals.Status(status) {
	case approvals.StatusDone:
		return "shared"
	case approvals.StatusExecuting, approvals.StatusReconciling:
		return "sent, not confirmed (see `" + variant.Current.Binary + " approvals`)"
	}
	return "not shared (" + safetext.Terminal(status) + ")"
}

// installContextShareGrant wires the grant into the relay: its line in the status and,
// through the returned funcs, the check that runs a prepared file share under it and
// its end, history included, when the relay is purged (sign-out, node removal). It
// adds no route.
func installContextShareGrant(opt *daemon.Options, m *approvals.Manager, g contextlib.ShareGrant) (register func(), purge func()) {
	p := opt.Provider
	account := func(ctx context.Context) contextlib.Account {
		s, err := p.Auth().Session(ctx)
		if err != nil {
			return contextlib.Account{}
		}
		return contextlib.Account{Env: string(s.Env), OrgID: s.OrgID, UserID: s.UserID}
	}
	opt.Contributors = append(opt.Contributors, daemon.StatusContributorFunc(func(s *daemon.Status) {
		s.ContextShareGrant = contextShareGrantStatus(context.Background(), g, p, account(context.Background()))
	}))
	led := contextlib.NewLedger(contextLedgerIn(filepath.Dir(opt.StateDir)))
	return func() {
		m.RegisterAutoApprover(contexttools.ShareFileOperation, contexttools.AutoApproveShareFile(contexttools.ShareGrantOptions{
			Grant: g, Ledger: led, Provider: p, FailureCode: m.FailureCode,
		}))
	}, func() { _ = g.Wipe() }
}

func contextShareGrantStatus(ctx context.Context, g contextlib.ShareGrant, p fleetapi.Provider, acct contextlib.Account) *daemon.ContextShareGrant {
	_, on := g.On(acct)
	out := &daemon.ContextShareGrant{On: on && contexttools.OrgAllowsShareGrant(ctx, p), HourlyCap: contextlib.MaxGrantedSharesPerHour, RefusedCap: contextlib.MaxRefusedGrantSharesPerHour}
	if out.On {
		out.UsedThisHour = g.UsedThisHour(acct, time.Now())
		out.RefusedThisHour = g.RefusedThisHour(acct, time.Now())
	}
	return out
}

// contextShareGrantLine is the grant's line in `kenaz status`.
func contextShareGrantLine(g *daemon.ContextShareGrant) string {
	if g == nil {
		return ""
	}
	if g.On {
		return fmt.Sprintf("context share grant: on; your AI tool shares context files with your teams without asking, up to %d per clock hour (turn off: `%s context grant off`)", g.HourlyCap, variant.Current.Binary)
	}
	return "context share grant: off; sharing a context file asks each time"
}
