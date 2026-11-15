package app

import (
	"context"

	"github.com/kameas-ai/kenaz-plugin/fleetapi"
	"github.com/kameas-ai/kenaz-plugin/internal/variant"
)

// guardSessionEnv wraps p so that a session for an environment this build
// does not use reads as signed out: one the build does not connect to (the
// staff dev build and prod), and one that has been retired (stage, in
// every build). The provider chooses the active session from what it
// stored, not from the environment this build resolved; every
// environment's sign-in shares one keychain service, so a prod session
// there must not reach the dev build, and a stage session an earlier
// release left must not make any build act as signed in. A command that
// resolves no environment (`context sync`) goes by the session alone, so
// this is what keeps it out of a retired one.
//
// cleanup is set for a command that only removes what an earlier sign-in
// left (retiredEnvCleanup): it still sees a session for a retired
// environment, so it can clear it.
func guardSessionEnv(p fleetapi.Provider, cleanup bool) fleetapi.Provider {
	if p == nil {
		return p
	}
	var a fleetapi.Auth = envAuth{p.Auth(), cleanup}
	if v, ok := p.Auth().(fleetapi.IdentityVerifier); ok {
		a = envAuthVerifier{envAuth{p.Auth(), cleanup}, v}
	}
	g := envProvider{p, a}
	return withOptional(g, p, g.turnedAway)
}

type envProvider struct {
	fleetapi.Provider
	auth fleetapi.Auth
}

func (p envProvider) Auth() fleetapi.Auth { return p.auth }

// withOptional returns base, a wrapper around inner, with the optional
// provider interfaces inner implements (fleetapi.MemoryProvider,
// fleetapi.ProjectsProvider), so that fleetapi.MemoryOf, fleetapi.ProjectsOf
// and every `p.(fleetapi.XProvider)` find through a wrapper exactly what
// they find without it. A wrapper that embeds the Provider interface drops
// them otherwise. Their calls answer with out's error while it has one,
// like every other Fleet call of the wrapper: being signed out, or in
// conflict, does not depend on the caller having asked for the session
// first.
//
// Every wrapper of a provider in this package goes through here, and
// TestWrappersForwardEveryOptionalInterface fails when fleetapi gains an
// optional interface this does not forward.
func withOptional(base, inner fleetapi.Provider, out func(context.Context) error) fleetapi.Provider {
	mp, hasMemory := inner.(fleetapi.MemoryProvider)
	pp, hasProjects := inner.(fleetapi.ProjectsProvider)
	m, pr := optionalMemory{mp, out}, optionalProjects{pp, out}
	switch {
	case hasMemory && hasProjects:
		return providerWithMemoryAndProjects{base, m, pr}
	case hasMemory:
		return providerWithMemory{base, m}
	case hasProjects:
		return providerWithProjects{base, pr}
	}
	return base
}

type providerWithMemory struct {
	fleetapi.Provider
	optionalMemory
}

type providerWithProjects struct {
	fleetapi.Provider
	optionalProjects
}

type providerWithMemoryAndProjects struct {
	fleetapi.Provider
	optionalMemory
	optionalProjects
}

type optionalMemory struct {
	mp  fleetapi.MemoryProvider
	out func(context.Context) error
}

// Memory is the wrapped provider's memory client behind out, or nil when
// it has none (fleetapi.MemoryOf then answers as it does without a wrapper).
func (o optionalMemory) Memory() fleetapi.Memory {
	m := o.mp.Memory()
	if m == nil {
		return nil
	}
	return guardedMemory{m, o.out}
}

type optionalProjects struct {
	pp  fleetapi.ProjectsProvider
	out func(context.Context) error
}

func (o optionalProjects) Projects() fleetapi.Projects {
	c := o.pp.Projects()
	if c == nil {
		return nil
	}
	return guardedProjects{c, o.out}
}

type guardedProjects struct {
	fleetapi.Projects
	out func(context.Context) error
}

func (g guardedProjects) Create(ctx context.Context, req fleetapi.ProjectCreateRequest) (fleetapi.Project, error) {
	if err := g.out(ctx); err != nil {
		return fleetapi.Project{}, err
	}
	return g.Projects.Create(ctx, req)
}

type guardedMemory struct {
	fleetapi.Memory
	out func(context.Context) error
}

func (g guardedMemory) Settings(ctx context.Context) (fleetapi.MemorySettings, error) {
	if err := g.out(ctx); err != nil {
		return fleetapi.MemorySettings{}, err
	}
	return g.Memory.Settings(ctx)
}

func (g guardedMemory) PutSettings(ctx context.Context, u fleetapi.MemorySettingsUpdate) (fleetapi.MemorySettings, error) {
	if err := g.out(ctx); err != nil {
		return fleetapi.MemorySettings{}, err
	}
	return g.Memory.PutSettings(ctx, u)
}

func (g guardedMemory) Pull(ctx context.Context, req fleetapi.MemoryPullRequest) (fleetapi.MemoryPullResponse, error) {
	if err := g.out(ctx); err != nil {
		return fleetapi.MemoryPullResponse{}, err
	}
	return g.Memory.Pull(ctx, req)
}

func (g guardedMemory) Push(ctx context.Context, req fleetapi.MemoryPushRequest) (fleetapi.MemoryPushResponse, error) {
	if err := g.out(ctx); err != nil {
		return fleetapi.MemoryPushResponse{}, err
	}
	return g.Memory.Push(ctx, req)
}

func (g guardedMemory) Forget(ctx context.Context, req fleetapi.MemoryForgetRequest) (fleetapi.MemoryForgetResponse, error) {
	if err := g.out(ctx); err != nil {
		return fleetapi.MemoryForgetResponse{}, err
	}
	return g.Memory.Forget(ctx, req)
}

func (g guardedMemory) ForgetAll(ctx context.Context, req fleetapi.MemoryForgetAllRequest) (fleetapi.MemoryForgetAllResponse, error) {
	if err := g.out(ctx); err != nil {
		return fleetapi.MemoryForgetAllResponse{}, err
	}
	return g.Memory.ForgetAll(ctx, req)
}

func (g guardedMemory) Export(ctx context.Context, fn func(fleetapi.MemoryRecord) error) (fleetapi.MemoryExportTrailer, error) {
	if err := g.out(ctx); err != nil {
		return fleetapi.MemoryExportTrailer{}, err
	}
	return g.Memory.Export(ctx, fn)
}

// The Fleet calls a command makes for the person are signed out with the
// session: a command may ask Fleet who the person is before it asks for
// the session (`context sync` does), and the wrapped provider would answer
// with the session it stored. They pass through untouched whenever the
// guard has turned no session away.
func (p envProvider) Me() fleetapi.Me { return guardedMe{p.Provider.Me(), p.turnedAway} }

// Enroller is signed out the same way. The relay's heartbeat enrolls
// without asking for the session (the provider says "not signed in" when
// there is none), so with a session the guard turns away it would enroll
// this device against that session's environment.
func (p envProvider) Enroller() fleetapi.Enroller {
	return guardedEnroller{p.Provider.Enroller(), p.turnedAway}
}
func (p envProvider) Catalog() fleetapi.Catalog {
	return guardedCatalog{p.Provider.Catalog(), p.turnedAway}
}
func (p envProvider) Team() fleetapi.Team { return guardedTeam{p.Provider.Team(), p.turnedAway} }
func (p envProvider) Context() fleetapi.Context {
	c := guardedContext{p.Provider.Context(), p.turnedAway}
	if l, ok := c.Context.(fleetapi.ContextLister); ok {
		return guardedContextLister{c, l}
	}
	return c
}

// turnedAway is ErrNotSignedIn while the stored session is one the guard
// reads as signed out, and nil otherwise (signed in to an environment this
// build uses, or not signed in at all: the wrapped provider says which).
func (p envProvider) turnedAway(ctx context.Context) error {
	if g, ok := p.auth.(interface{ turnedAway(context.Context) bool }); ok && g.turnedAway(ctx) {
		return fleetapi.ErrNotSignedIn
	}
	return nil
}

type guardedEnroller struct {
	fleetapi.Enroller
	out func(context.Context) error
}

func (g guardedEnroller) Enroll(ctx context.Context, body fleetapi.EnrollBody) (fleetapi.EnrollResult, error) {
	if err := g.out(ctx); err != nil {
		return fleetapi.EnrollResult{}, err
	}
	return g.Enroller.Enroll(ctx, body)
}

type guardedMe struct {
	fleetapi.Me
	out func(context.Context) error
}

func (g guardedMe) Get(ctx context.Context) (fleetapi.MeInfo, error) {
	if err := g.out(ctx); err != nil {
		return fleetapi.MeInfo{}, err
	}
	return g.Me.Get(ctx)
}

func (g guardedMe) Capabilities(ctx context.Context) (fleetapi.Capabilities, error) {
	if err := g.out(ctx); err != nil {
		return fleetapi.Capabilities{}, err
	}
	return g.Me.Capabilities(ctx)
}

type guardedCatalog struct {
	fleetapi.Catalog
	out func(context.Context) error
}

func (g guardedCatalog) List(ctx context.Context, q fleetapi.CatalogQuery) ([]fleetapi.CatalogEntry, error) {
	if err := g.out(ctx); err != nil {
		return nil, err
	}
	return g.Catalog.List(ctx, q)
}

func (g guardedCatalog) Get(ctx context.Context, id, version string) (fleetapi.CatalogItem, error) {
	if err := g.out(ctx); err != nil {
		return fleetapi.CatalogItem{}, err
	}
	return g.Catalog.Get(ctx, id, version)
}

func (g guardedCatalog) Publish(ctx context.Context, req fleetapi.PublishRequest) (fleetapi.CatalogEntry, error) {
	if err := g.out(ctx); err != nil {
		return fleetapi.CatalogEntry{}, err
	}
	return g.Catalog.Publish(ctx, req)
}

type guardedTeam struct {
	fleetapi.Team
	out func(context.Context) error
}

func (g guardedTeam) Members(ctx context.Context, teamID string) ([]fleetapi.Member, error) {
	if err := g.out(ctx); err != nil {
		return nil, err
	}
	return g.Team.Members(ctx, teamID)
}

type guardedContext struct {
	fleetapi.Context
	out func(context.Context) error
}

func (g guardedContext) Search(ctx context.Context, req fleetapi.SearchRequest) ([]fleetapi.SearchResult, error) {
	if err := g.out(ctx); err != nil {
		return nil, err
	}
	return g.Context.Search(ctx, req)
}

func (g guardedContext) Pull(ctx context.Context, req fleetapi.PullRequest) ([]fleetapi.NodeContent, error) {
	if err := g.out(ctx); err != nil {
		return nil, err
	}
	return g.Context.Pull(ctx, req)
}

func (g guardedContext) Push(ctx context.Context, req fleetapi.PushRequest) (fleetapi.ContextNode, error) {
	if err := g.out(ctx); err != nil {
		return fleetapi.ContextNode{}, err
	}
	return g.Context.Push(ctx, req)
}

func (g guardedContext) Promote(ctx context.Context, req fleetapi.PromoteRequest) (fleetapi.PromoteResult, error) {
	if err := g.out(ctx); err != nil {
		return fleetapi.PromoteResult{}, err
	}
	return g.Context.Promote(ctx, req)
}

// guardedContextLister is guardedContext for a provider that can list
// (fleetapi.ContextLister is optional).
type guardedContextLister struct {
	guardedContext
	l fleetapi.ContextLister
}

func (g guardedContextLister) List(ctx context.Context, req fleetapi.ListRequest) (fleetapi.ListPage, error) {
	if err := g.out(ctx); err != nil {
		return fleetapi.ListPage{}, err
	}
	return g.l.List(ctx, req)
}

type envAuth struct {
	fleetapi.Auth
	// retiredOK lets a session for a retired environment through (cleanup).
	retiredOK bool
}

func (a envAuth) Session(ctx context.Context) (fleetapi.Session, error) {
	s, err := a.Auth.Session(ctx)
	if err == nil && a.outside(s.Env) {
		return fleetapi.Session{}, fleetapi.ErrNotSignedIn
	}
	return s, err
}

// outside reports whether a session for env reads as signed out.
func (a envAuth) outside(env fleetapi.Env) bool {
	return !variant.Current.Allowed(string(env)) && !(a.retiredOK && variant.Retired(string(env)))
}

// turnedAway reports whether there is a stored session that reads as
// signed out.
func (a envAuth) turnedAway(ctx context.Context) bool {
	s, err := a.Auth.Session(ctx)
	return err == nil && a.outside(s.Env)
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
// teams, memory sync, the project registry) report err, so each Fleet
// tool explains the conflict instead of acting through this build's
// connection. The optional interfaces p implements stay (withOptional).
func conflictProvider(p fleetapi.Provider, err error) fleetapi.Provider {
	var a fleetapi.Auth = conflictAuth{p.Auth(), err}
	if _, ok := p.Auth().(fleetapi.IdentityVerifier); ok {
		a = conflictAuthVerifier{conflictAuth{p.Auth(), err}}
	}
	g := conflicted{envProvider{p, a}, err}
	return withOptional(g, p, func(context.Context) error { return err })
}

type conflicted struct {
	envProvider
	err error
}

func (c conflicted) Catalog() fleetapi.Catalog { return conflictCalls{c.err} }
func (c conflicted) Enroller() fleetapi.Enroller {
	return guardedEnroller{c.Provider.Enroller(), func(context.Context) error { return c.err }}
}
func (c conflicted) Me() fleetapi.Me { return meCalls{c.err} }
func (c conflicted) Context() fleetapi.Context {
	// Listing is optional (fleetapi.ContextLister): it stays when the
	// wrapped provider has it, so the tools that list report the conflict
	// and not that listing is unavailable.
	if _, ok := c.Provider.Context().(fleetapi.ContextLister); ok {
		return conflictContextLister{conflictCalls{c.err}}
	}
	return conflictCalls{c.err}
}
func (c conflicted) Team() fleetapi.Team { return conflictCalls{c.err} }

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

// conflictContextLister is conflictCalls as a fleetapi.Context that lists
// (its List differs from the catalog's).
type conflictContextLister struct{ c conflictCalls }

func (l conflictContextLister) Search(ctx context.Context, r fleetapi.SearchRequest) ([]fleetapi.SearchResult, error) {
	return l.c.Search(ctx, r)
}
func (l conflictContextLister) Pull(ctx context.Context, r fleetapi.PullRequest) ([]fleetapi.NodeContent, error) {
	return l.c.Pull(ctx, r)
}
func (l conflictContextLister) Push(ctx context.Context, r fleetapi.PushRequest) (fleetapi.ContextNode, error) {
	return l.c.Push(ctx, r)
}
func (l conflictContextLister) Promote(ctx context.Context, r fleetapi.PromoteRequest) (fleetapi.PromoteResult, error) {
	return l.c.Promote(ctx, r)
}
func (l conflictContextLister) List(context.Context, fleetapi.ListRequest) (fleetapi.ListPage, error) {
	return fleetapi.ListPage{}, l.c.err
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

// The reloads pass through, as they do in envAuth: a new sign-in is still
// picked up, and Session keeps answering with the conflict.
func (a conflictAuth) ReloadSession(ctx context.Context) error {
	return fleetapi.ReloadSession(ctx, a.Auth)
}

func (a conflictAuth) ReloadSessionNow(ctx context.Context) error {
	return fleetapi.ReloadSessionNow(ctx, a.Auth)
}

// conflictAuthVerifier is conflictAuth for an Auth that verifies identity
// (fleetapi.IdentityVerifier is optional): that is a sign-in, so it
// reports the conflict too.
type conflictAuthVerifier struct{ conflictAuth }

func (a conflictAuthVerifier) VerifyIdentity(context.Context, fleetapi.LoginRequest) (fleetapi.VerifiedIdentity, error) {
	return fleetapi.VerifiedIdentity{}, a.err
}
