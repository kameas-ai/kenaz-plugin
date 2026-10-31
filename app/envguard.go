package app

import (
	"context"

	"github.com/kameas-ai/kenaz-plugin/fleetapi"
	"github.com/kameas-ai/kenaz-plugin/internal/variant"
)

// guardSessionEnv wraps p, in a build that does not connect to every Fleet
// environment (the staff dev build), so that a session for an environment
// the build does not allow reads as signed out. The provider chooses the
// active session from what it stored, not from the environment this build
// resolved; every environment's sign-in shares one keychain service, so a
// prod session there must not reach the dev build. The public build's
// provider is returned unchanged.
func guardSessionEnv(p fleetapi.Provider) fleetapi.Provider {
	if p == nil || allEnvsAllowed() {
		return p
	}
	var a fleetapi.Auth = envAuth{p.Auth()}
	if v, ok := p.Auth().(fleetapi.IdentityVerifier); ok {
		a = envAuthVerifier{envAuth{p.Auth()}, v}
	}
	g := envProvider{p, a}
	if mp, ok := p.(fleetapi.MemoryProvider); ok {
		return envMemoryProvider{g, mp}
	}
	return g
}

type envProvider struct {
	fleetapi.Provider
	auth fleetapi.Auth
}

func (p envProvider) Auth() fleetapi.Auth { return p.auth }

type envMemoryProvider struct {
	envProvider
	mp fleetapi.MemoryProvider
}

func (p envMemoryProvider) Memory() fleetapi.Memory { return p.mp.Memory() }

type envAuth struct{ fleetapi.Auth }

func (a envAuth) Session(ctx context.Context) (fleetapi.Session, error) {
	s, err := a.Auth.Session(ctx)
	if err == nil && !variant.Current.Allowed(string(s.Env)) {
		return fleetapi.Session{}, fleetapi.ErrNotSignedIn
	}
	return s, err
}

// ReloadSession passes the reload through to the wrapped Auth, so the
// relay of a build with the guard can still pick up a new sign-in.
func (a envAuth) ReloadSession(ctx context.Context) error {
	return fleetapi.ReloadSession(ctx, a.Auth)
}

// ReloadSessionNow passes the explicit reload through the same way, so the
// relay's session-changed notice reaches the wrapped Auth's
// ReloadSessionNow (or its ReloadSession when it has none).
func (a envAuth) ReloadSessionNow(ctx context.Context) error {
	return fleetapi.ReloadSessionNow(ctx, a.Auth)
}

type envAuthVerifier struct {
	envAuth
	v fleetapi.IdentityVerifier
}

func (a envAuthVerifier) VerifyIdentity(ctx context.Context, req fleetapi.LoginRequest) (fleetapi.VerifiedIdentity, error) {
	return a.v.VerifyIdentity(ctx, req)
}

// conflictProvider is p for an MCP server whose environment another build
// is using: sign-in and every Fleet call (catalog, identity, context,
// teams) report err, so each Fleet tool explains the conflict instead of
// acting through this build's connection.
func conflictProvider(p fleetapi.Provider, err error) fleetapi.Provider {
	g := conflicted{envProvider{p, conflictAuth{p.Auth(), err}}, err}
	if mp, ok := p.(fleetapi.MemoryProvider); ok {
		return conflictedMemory{g, mp}
	}
	return g
}

type conflicted struct {
	envProvider
	err error
}

func (c conflicted) Catalog() fleetapi.Catalog { return conflictCalls{c.err} }
func (c conflicted) Me() fleetapi.Me           { return meCalls{c.err} }
func (c conflicted) Context() fleetapi.Context { return conflictCalls{c.err} }
func (c conflicted) Team() fleetapi.Team       { return conflictCalls{c.err} }

type conflictedMemory struct {
	conflicted
	mp fleetapi.MemoryProvider
}

func (p conflictedMemory) Memory() fleetapi.Memory { return p.mp.Memory() }

// conflictCalls answers every Fleet call with the conflict.
type conflictCalls struct{ err error }

func (c conflictCalls) List(context.Context, fleetapi.CatalogQuery) ([]fleetapi.CatalogEntry, error) {
	return nil, c.err
}
func (c conflictCalls) Get(context.Context, string, string) (fleetapi.CatalogItem, error) {
	return fleetapi.CatalogItem{}, c.err
}
func (c conflictCalls) Publish(context.Context, fleetapi.PublishRequest) (fleetapi.CatalogEntry, error) {
	return fleetapi.CatalogEntry{}, c.err
}
func (c conflictCalls) Members(context.Context, string) ([]fleetapi.Member, error) { return nil, c.err }
func (c conflictCalls) Search(context.Context, fleetapi.SearchRequest) ([]fleetapi.SearchResult, error) {
	return nil, c.err
}
func (c conflictCalls) Pull(context.Context, fleetapi.PullRequest) ([]fleetapi.NodeContent, error) {
	return nil, c.err
}
func (c conflictCalls) Push(context.Context, fleetapi.PushRequest) (fleetapi.ContextNode, error) {
	return fleetapi.ContextNode{}, c.err
}
func (c conflictCalls) Promote(context.Context, fleetapi.PromoteRequest) (fleetapi.PromoteResult, error) {
	return fleetapi.PromoteResult{}, c.err
}

// meCalls is conflictCalls as a fleetapi.Me (its Get differs from the
// catalog's).
type meCalls struct{ err error }

func (c meCalls) Get(context.Context) (fleetapi.MeInfo, error) { return fleetapi.MeInfo{}, c.err }
func (c meCalls) Capabilities(context.Context) (fleetapi.Capabilities, error) {
	return fleetapi.Capabilities{}, c.err
}

type conflictAuth struct {
	fleetapi.Auth
	err error
}

func (a conflictAuth) Session(context.Context) (fleetapi.Session, error) {
	return fleetapi.Session{}, a.err
}

func (a conflictAuth) Login(context.Context, fleetapi.LoginRequest) (fleetapi.Session, error) {
	return fleetapi.Session{}, a.err
}
