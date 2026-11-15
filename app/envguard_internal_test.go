package app

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/kameas-ai/kenaz-plugin/fleetapi"
	"github.com/kameas-ai/kenaz-plugin/fleetapi/stub"
	"github.com/kameas-ai/kenaz-plugin/internal/features"
	"github.com/kameas-ai/kenaz-plugin/internal/spool"
	"github.com/kameas-ai/kenaz-plugin/internal/variant"
)

type sessionAuth struct {
	fleetapi.Auth
	env fleetapi.Env
}

func (a sessionAuth) Session(context.Context) (fleetapi.Session, error) {
	return fleetapi.Session{Env: a.env, UserID: "u"}, nil
}

func (a sessionAuth) VerifyIdentity(context.Context, fleetapi.LoginRequest) (fleetapi.VerifiedIdentity, error) {
	return fleetapi.VerifiedIdentity{UserID: "u"}, nil
}

type sessionProvider struct {
	fleetapi.Provider
	env fleetapi.Env
}

func (p sessionProvider) Auth() fleetapi.Auth     { return sessionAuth{p.Provider.Auth(), p.env} }
func (p sessionProvider) Memory() fleetapi.Memory { return nil }

// The provider picks the active session from what it stored, and every
// environment's sign-in shares one keychain service, so a build reads a
// session for an environment it does not connect to as signed out (issue
// #34 F2), and every build reads one for the retired stage as signed out:
// the public build too, which is otherwise allowed every environment.
// Optional interfaces (identity verification, memory) survive.
func TestSessionOutsideAllowedEnvsIsSignedOut(t *testing.T) {
	for _, env := range []fleetapi.Env{fleetapi.EnvProd, fleetapi.EnvStage, fleetapi.EnvDev} {
		inner := sessionProvider{stub.New(), env}
		p := guardSessionEnv(inner, false)
		_, err := p.Auth().Session(context.Background())
		switch allowed := variant.Current.Allowed(string(env)); {
		case allowed && err != nil:
			t.Errorf("%s session: %v", env, err)
		case !allowed && !errors.Is(err, fleetapi.ErrNotSignedIn):
			t.Errorf("%s session in %s: %v", env, variant.Current.Name, err)
		}
		if _, ok := p.Auth().(fleetapi.IdentityVerifier); !ok {
			t.Errorf("%s: identity verification lost", env)
		}
		if _, ok := p.(fleetapi.MemoryProvider); !ok {
			t.Errorf("%s: memory lost", env)
		}
	}
	// No build is exempt: stage is signed out in each, and only a cleanup
	// command sees the session.
	stage := sessionProvider{stub.New(), fleetapi.EnvStage}
	if _, err := guardSessionEnv(stage, false).Auth().Session(context.Background()); !errors.Is(err, fleetapi.ErrNotSignedIn) {
		t.Errorf("stage session in %s: %v", variant.Current.Name, err)
	}
	if s, err := guardSessionEnv(stage, true).Auth().Session(context.Background()); err != nil || s.Env != fleetapi.EnvStage {
		t.Errorf("stage session for a cleanup command in %s: %+v %v", variant.Current.Name, s, err)
	}
	if guardSessionEnv(nil, false) != nil {
		t.Error("no provider became one")
	}
}

// Every Fleet call of the MCP server's provider reports the conflict when
// another build uses the environment.
func TestConflictProviderAnswersEveryCall(t *testing.T) {
	want := errors.New("kenaz-dev is using this Fleet environment")
	p := conflictProvider(sessionProvider{stub.New(), fleetapi.EnvDev}, want)
	ctx := context.Background()
	_, e1 := p.Auth().Session(ctx)
	_, e2 := p.Catalog().List(ctx, fleetapi.CatalogQuery{})
	_, e3 := p.Catalog().Get(ctx, "c", "1")
	_, e4 := p.Me().Get(ctx)
	_, e5 := p.Me().Capabilities(ctx)
	_, e6 := p.Context().Search(ctx, fleetapi.SearchRequest{})
	_, e7 := p.Team().Members(ctx, "t")
	_, e8 := p.Auth().Login(ctx, fleetapi.LoginRequest{})
	for i, err := range []error{e1, e2, e3, e4, e5, e6, e7, e8} {
		if !errors.Is(err, want) {
			t.Errorf("call %d: %v", i+1, err)
		}
	}
	if _, ok := p.(fleetapi.MemoryProvider); !ok {
		t.Error("memory lost")
	}
}

// bothReloadsAuth counts each reload, to check what the guard passes on.
type bothReloadsAuth struct {
	sessionAuth
	periodic, now *int
}

func (a bothReloadsAuth) ReloadSession(context.Context) error    { *a.periodic++; return nil }
func (a bothReloadsAuth) ReloadSessionNow(context.Context) error { *a.now++; return nil }

type periodicOnlyAuth struct {
	sessionAuth
	periodic *int
}

func (a periodicOnlyAuth) ReloadSession(context.Context) error { *a.periodic++; return nil }

type authProvider struct {
	fleetapi.Provider
	a fleetapi.Auth
}

func (p authProvider) Auth() fleetapi.Auth { return p.a }

// The guard passes both reloads through to the wrapped Auth, so the relay
// finds ReloadSessionNow by type assertion and calls the wrapped one; an
// Auth with only ReloadSession gets that from the explicit path too, and
// one with neither still reports ErrReloadUnsupported.
func TestEnvAuthPassesReloadsThrough(t *testing.T) {
	ctx := context.Background()
	var periodic, now int
	inner := bothReloadsAuth{sessionAuth{stub.New().Auth(), fleetapi.EnvDev}, &periodic, &now}
	a := envAuth{inner, false}
	if _, ok := fleetapi.Auth(a).(fleetapi.SessionReloaderNow); !ok {
		t.Fatal("envAuth does not implement SessionReloaderNow")
	}
	if err := fleetapi.ReloadSession(ctx, a); err != nil || periodic != 1 || now != 0 {
		t.Fatalf("ReloadSession: %v periodic=%d now=%d", err, periodic, now)
	}
	if err := fleetapi.ReloadSessionNow(ctx, a); err != nil || periodic != 1 || now != 1 {
		t.Fatalf("ReloadSessionNow: %v periodic=%d now=%d", err, periodic, now)
	}

	periodic = 0
	only := envAuth{periodicOnlyAuth{sessionAuth{stub.New().Auth(), fleetapi.EnvDev}, &periodic}, false}
	if err := fleetapi.ReloadSessionNow(ctx, only); err != nil || periodic != 1 {
		t.Fatalf("fallback: %v periodic=%d", err, periodic)
	}

	none := envAuth{sessionAuth{stub.New().Auth(), fleetapi.EnvDev}, false}
	if err := fleetapi.ReloadSessionNow(ctx, none); !errors.Is(err, fleetapi.ErrReloadUnsupported) {
		t.Fatalf("no reload: %v", err)
	}

	// Through guardSessionEnv, which wraps the provider in every build.
	now = 0
	p := guardSessionEnv(authProvider{stub.New(), inner}, false)
	if err := fleetapi.ReloadSessionNow(ctx, p.Auth()); err != nil || now != 1 {
		t.Fatalf("guarded ReloadSessionNow: %v now=%d", err, now)
	}
}

// fleetCalls is a signed-in provider that counts every Fleet call made
// for the person.
type fleetCalls struct {
	fleetapi.Provider
	env    fleetapi.Env
	lister bool
	n      *int
}

func (p fleetCalls) Flavor() fleetapi.Flavor   { return fleetapi.FlavorOfficial }
func (p fleetCalls) Auth() fleetapi.Auth       { return orgSessionAuth{p.Provider.Auth(), p.env} }
func (p fleetCalls) Me() fleetapi.Me           { return countedCalls{p.n} }
func (p fleetCalls) Catalog() fleetapi.Catalog { return countedCatalog{p.n} }
func (p fleetCalls) Team() fleetapi.Team       { return countedCalls{p.n} }
func (p fleetCalls) Context() fleetapi.Context {
	if p.lister {
		return countedLister{countedContext{p.n}}
	}
	return countedContext{p.n}
}

type orgSessionAuth struct {
	fleetapi.Auth
	env fleetapi.Env
}

func (a orgSessionAuth) Session(context.Context) (fleetapi.Session, error) {
	return fleetapi.Session{Env: a.env, OrgID: "org-1", UserID: "alice"}, nil
}

type countedCalls struct{ n *int }

func (c countedCalls) Get(context.Context) (fleetapi.MeInfo, error) {
	*c.n++
	return fleetapi.MeInfo{UserID: "alice", OrgID: "org-1", Teams: []fleetapi.TeamRef{{ID: "team-1", Name: "Platform"}}}, nil
}
func (c countedCalls) Capabilities(context.Context) (fleetapi.Capabilities, error) {
	*c.n++
	return fleetapi.Capabilities{Keys: map[string]bool{"team_graph_sharing": true, fleetapi.CapContextFiles: true}}, nil
}
func (c countedCalls) Members(context.Context, string) ([]fleetapi.Member, error) {
	*c.n++
	return nil, nil
}

type countedCatalog struct{ n *int }

func (c countedCatalog) List(context.Context, fleetapi.CatalogQuery) ([]fleetapi.CatalogEntry, error) {
	*c.n++
	return nil, nil
}
func (c countedCatalog) Get(context.Context, string, string) (fleetapi.CatalogItem, error) {
	*c.n++
	return fleetapi.CatalogItem{}, nil
}
func (c countedCatalog) Publish(context.Context, fleetapi.PublishRequest) (fleetapi.CatalogEntry, error) {
	*c.n++
	return fleetapi.CatalogEntry{}, nil
}

type countedContext struct{ n *int }

func (c countedContext) Search(context.Context, fleetapi.SearchRequest) ([]fleetapi.SearchResult, error) {
	*c.n++
	return nil, nil
}
func (c countedContext) Pull(context.Context, fleetapi.PullRequest) ([]fleetapi.NodeContent, error) {
	*c.n++
	return nil, nil
}
func (c countedContext) Push(context.Context, fleetapi.PushRequest) (fleetapi.ContextNode, error) {
	*c.n++
	return fleetapi.ContextNode{}, nil
}
func (c countedContext) Promote(context.Context, fleetapi.PromoteRequest) (fleetapi.PromoteResult, error) {
	*c.n++
	return fleetapi.PromoteResult{}, nil
}

type countedLister struct{ countedContext }

func (c countedLister) List(context.Context, fleetapi.ListRequest) (fleetapi.ListPage, error) {
	*c.n++
	return fleetapi.ListPage{}, nil
}

// everyFleetCall makes each call a command makes for the person, and
// returns their errors.
func everyFleetCall(p fleetapi.Provider) []error {
	ctx := context.Background()
	_, e1 := p.Me().Get(ctx)
	_, e2 := p.Me().Capabilities(ctx)
	_, e3 := p.Catalog().List(ctx, fleetapi.CatalogQuery{})
	_, e4 := p.Catalog().Get(ctx, "c", "1")
	_, e5 := p.Catalog().Publish(ctx, fleetapi.PublishRequest{})
	_, e6 := p.Team().Members(ctx, "t")
	_, e7 := p.Context().Search(ctx, fleetapi.SearchRequest{})
	_, e8 := p.Context().Pull(ctx, fleetapi.PullRequest{})
	_, e9 := p.Context().Push(ctx, fleetapi.PushRequest{})
	_, e10 := p.Context().Promote(ctx, fleetapi.PromoteRequest{})
	errs := []error{e1, e2, e3, e4, e5, e6, e7, e8, e9, e10}
	if l, ok := p.Context().(fleetapi.ContextLister); ok {
		_, e11 := l.List(ctx, fleetapi.ListRequest{})
		errs = append(errs, e11)
	}
	return errs
}

// A session that reads as signed out is signed out for every Fleet call
// too, not only for Auth().Session: a command may ask Fleet who the person
// is first. Nothing reaches the provider with a stage session, in any
// build; a session the build uses passes through; a cleanup command still
// sees stage.
func TestFleetCallsAreSignedOutWithTheSession(t *testing.T) {
	for _, lister := range []bool{false, true} {
		n := 0
		p := guardSessionEnv(fleetCalls{stub.New(), fleetapi.EnvStage, lister, &n}, false)
		errs := everyFleetCall(p)
		if want := map[bool]int{false: 10, true: 11}[lister]; len(errs) != want {
			t.Fatalf("lister %v: %d calls, want %d (the optional List was lost or invented)", lister, len(errs), want)
		}
		for i, err := range errs {
			if !errors.Is(err, fleetapi.ErrNotSignedIn) {
				t.Errorf("stage session, call %d: %v", i+1, err)
			}
		}
		if n != 0 {
			t.Errorf("%d call(s) reached the provider with a stage session", n)
		}

		// A session this build uses passes through.
		n = 0
		env := fleetapi.Env(variant.Current.DefaultEnv)
		errs = everyFleetCall(guardSessionEnv(fleetCalls{stub.New(), env, lister, &n}, false))
		for i, err := range errs {
			if err != nil {
				t.Errorf("%s session, call %d: %v", env, i+1, err)
			}
		}
		if n != len(errs) {
			t.Errorf("%s session: %d of %d calls reached the provider", env, n, len(errs))
		}

		// The cleanup commands see the stage session as it is.
		n = 0
		errs = everyFleetCall(guardSessionEnv(fleetCalls{stub.New(), fleetapi.EnvStage, lister, &n}, true))
		if n != len(errs) {
			t.Errorf("cleanup: %d of %d calls reached the provider", n, len(errs))
		}
	}
	// Not signed in at all: the provider's own answer stands.
	if _, err := guardSessionEnv(stub.New(), false).Me().Get(context.Background()); !errors.Is(err, fleetapi.ErrUnofficialBuild) {
		t.Errorf("stub: %v", err)
	}
}

// `kenaz context sync` resolves no environment and skips the env gate: it
// goes by the session. With a session for the retired stage it is signed
// out, in every build, and asks Fleet nothing.
func TestContextSyncWithAStageSessionIsSignedOut(t *testing.T) {
	old := features.ContextLibrary
	features.ContextLibrary = true
	t.Cleanup(func() { features.ContextLibrary = old })
	oldT, oldR := memoryStdinTerminal, memoryStdin
	memoryStdinTerminal, memoryStdin = func() bool { return true }, strings.NewReader("y\n")
	t.Cleanup(func() { memoryStdinTerminal, memoryStdin = oldT, oldR })

	for _, c := range []struct {
		env  fleetapi.Env
		want bool // signed in
	}{
		{fleetapi.EnvStage, false},
		{fleetapi.Env(variant.Current.DefaultEnv), true},
	} {
		n := 0
		home := t.TempDir()
		var out, errb bytes.Buffer
		env := Env{Provider: fleetCalls{stub.New(), c.env, true, &n}, Stdout: &out, Stderr: &errb, Ctx: context.Background(),
			KeyStore: &spool.Memory{}, Getenv: func(k string) string {
				if k == variant.Shared.HomeVar {
					return home
				}
				return ""
			}}
		code := RunEnv(env, []string{"context", "sync"})
		signedOut := strings.Contains(errb.String(), "You are not signed in to Kameas Fleet.")
		switch {
		case c.want && (signedOut || n == 0):
			t.Errorf("%s session: exit %d, %d Fleet calls\n%s%s", c.env, code, n, out.String(), errb.String())
		case !c.want && (code != ExitError || !signedOut || n != 0 || out.Len() != 0):
			t.Errorf("%s session: exit %d, %d Fleet calls\nstdout %q\nstderr %q", c.env, code, n, out.String(), errb.String())
		}
	}
}
