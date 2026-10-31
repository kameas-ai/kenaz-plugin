package app

import (
	"context"
	"errors"
	"testing"

	"github.com/kameas-ai/kenaz-plugin/fleetapi"
	"github.com/kameas-ai/kenaz-plugin/fleetapi/stub"
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
// #34 F2). The public build connects to every environment and is left
// alone. Optional interfaces (identity verification, memory) survive.
func TestSessionOutsideAllowedEnvsIsSignedOut(t *testing.T) {
	for _, env := range []fleetapi.Env{fleetapi.EnvProd, fleetapi.EnvStage, fleetapi.EnvDev} {
		inner := sessionProvider{stub.New(), env}
		p := guardSessionEnv(inner)
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
	if allEnvsAllowed() {
		inner := sessionProvider{stub.New(), fleetapi.EnvProd}
		if guardSessionEnv(inner) != fleetapi.Provider(inner) {
			t.Error("the public build's provider was wrapped")
		}
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
	a := envAuth{inner}
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
	only := envAuth{periodicOnlyAuth{sessionAuth{stub.New().Auth(), fleetapi.EnvDev}, &periodic}}
	if err := fleetapi.ReloadSessionNow(ctx, only); err != nil || periodic != 1 {
		t.Fatalf("fallback: %v periodic=%d", err, periodic)
	}

	none := envAuth{sessionAuth{stub.New().Auth(), fleetapi.EnvDev}}
	if err := fleetapi.ReloadSessionNow(ctx, none); !errors.Is(err, fleetapi.ErrReloadUnsupported) {
		t.Fatalf("no reload: %v", err)
	}

	// Through guardSessionEnv, in a build that wraps the provider.
	if !allEnvsAllowed() {
		now = 0
		p := guardSessionEnv(authProvider{stub.New(), inner})
		if err := fleetapi.ReloadSessionNow(ctx, p.Auth()); err != nil || now != 1 {
			t.Fatalf("guarded ReloadSessionNow: %v now=%d", err, now)
		}
	}
}
