package app

import (
	"context"
	"errors"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"testing"

	"github.com/kameas-ai/kenaz-plugin/fleetapi"
	"github.com/kameas-ai/kenaz-plugin/fleetapi/stub"
	"github.com/kameas-ai/kenaz-plugin/internal/enroll"
	"github.com/kameas-ai/kenaz-plugin/internal/variant"
)

// fleetapiInterfaces is every interface the fleetapi package declares, and
// what the provider wrappers in this package (guardSessionEnv,
// conflictProvider) do about it. A wrapper embeds the interface it wraps,
// which keeps that interface's own methods and silently drops any other
// interface the wrapped value also implements. So an optional interface,
// one that callers find with a type assertion or an ...Of helper, has to
// be forwarded by hand, and this table is where a new one is noticed:
// TestWrappersForwardEveryOptionalInterface fails for an interface that is
// not listed, and checks each "optional" one through both wrappers.
var fleetapiInterfaces = map[string]string{
	// What Provider's own methods return: embedded, so always there. (Some
	// are also signed out by the guard: Me, Catalog, Context, Team and
	// Enroller; see TestFleetCallsAreSignedOutWithTheSession and
	// TestEnrollmentIsSignedOutWithTheSession.)
	"Provider": "core", "Auth": "core", "Enroller": "core", "Me": "core", "Pollers": "core",
	"OptIns": "core", "Catalog": "core", "Context": "core", "Team": "core", "OTLPPoster": "core",
	// Reached through Auth, which the wrappers pass on unwrapped.
	"TokenStore": "core", "TokenSource": "core", "FileCredentialStore": "core",
	// What an optional provider interface returns.
	"Memory": "core", "Projects": "core",
	// Optional: found by type assertion on the provider, its Auth or its
	// Context. Each is forwarded and checked below.
	"MemoryProvider": "optional", "ProjectsProvider": "optional",
	"IdentityVerifier": "optional", "SessionReloader": "optional", "SessionReloaderNow": "optional",
	"ContextLister": "optional",
}

func declaredFleetapiInterfaces(t *testing.T) []string {
	t.Helper()
	dir := filepath.Join("..", "fleetapi")
	var names []string
	fset := token.NewFileSet()
	matches, err := filepath.Glob(filepath.Join(dir, "*.go"))
	if err != nil || len(matches) == 0 {
		t.Fatalf("no fleetapi sources: %v", err)
	}
	for _, f := range matches {
		if strings.HasSuffix(f, "_test.go") {
			continue
		}
		file, err := parser.ParseFile(fset, f, nil, 0)
		if err != nil {
			t.Fatal(err)
		}
		ast.Inspect(file, func(n ast.Node) bool {
			if ts, ok := n.(*ast.TypeSpec); ok {
				if _, ok := ts.Type.(*ast.InterfaceType); ok && ts.Name.IsExported() {
					names = append(names, ts.Name.Name)
				}
			}
			return true
		})
	}
	sort.Strings(names)
	return names
}

// everything is an official provider that implements every optional
// interface, with a session for env, counting what reaches it.
type everything struct {
	fleetCalls
	memory, projects bool
	now              *int
}

func (p everything) Auth() fleetapi.Auth {
	return bothReloadsAuth{sessionAuth{orgSessionAuth{p.Provider.Auth(), p.env}, p.env}, new(int), p.now}
}

// withMemory, withProjects and withBoth add the optional provider
// interfaces, so that each combination a provider can have is wrapped.
type withMemory struct{ everything }

func (p withMemory) Memory() fleetapi.Memory { return countedMemory{p.n} }

type withProjects struct{ everything }

func (p withProjects) Projects() fleetapi.Projects { return countedProjects{p.n} }

type withBoth struct{ everything }

func (p withBoth) Memory() fleetapi.Memory     { return countedMemory{p.n} }
func (p withBoth) Projects() fleetapi.Projects { return countedProjects{p.n} }

type countedProjects struct{ n *int }

func (c countedProjects) Create(context.Context, fleetapi.ProjectCreateRequest) (fleetapi.Project, error) {
	*c.n++
	return fleetapi.Project{ID: "p1"}, nil
}

func providerWith(env fleetapi.Env, memory, projects bool, n, now *int) fleetapi.Provider {
	e := everything{fleetCalls{stub.New(), env, true, n}, memory, projects, now}
	switch {
	case memory && projects:
		return withBoth{e}
	case memory:
		return withMemory{e}
	case projects:
		return withProjects{e}
	}
	return e
}

// Every optional interface a provider, its Auth or its Context implements
// is still found through each wrapper, for each combination a provider
// can have: present when the provider has it and absent when it doesn't,
// reaching the real implementation for a session the build uses, and
// signed out (or in conflict) otherwise, whether or not the caller asked
// for the session first.
func TestWrappersForwardEveryOptionalInterface(t *testing.T) {
	declared := declaredFleetapiInterfaces(t)
	for _, name := range declared {
		if fleetapiInterfaces[name] == "" {
			t.Errorf("fleetapi.%s is new: say in fleetapiInterfaces whether the provider wrappers must forward it, and if so forward it in app/envguard.go (withOptional) and check it here", name)
		}
	}
	for name := range fleetapiInterfaces {
		if i := sort.SearchStrings(declared, name); i >= len(declared) || declared[i] != name {
			t.Errorf("fleetapi.%s is listed but no longer declared", name)
		}
	}

	ctx := context.Background()
	allowed := fleetapi.Env(variant.Current.DefaultEnv)
	conflict := errors.New("another build is using this environment")
	for _, c := range []struct{ memory, projects bool }{{false, false}, {true, false}, {false, true}, {true, true}} {
		for _, w := range []struct {
			name string
			env  fleetapi.Env
			wrap func(fleetapi.Provider) fleetapi.Provider
			want error // of every Fleet call; nil: it reaches the provider
		}{
			{"guard, a session the build uses", allowed, func(p fleetapi.Provider) fleetapi.Provider { return guardSessionEnv(p, false) }, nil},
			{"guard, a stage session", fleetapi.EnvStage, func(p fleetapi.Provider) fleetapi.Provider { return guardSessionEnv(p, false) }, fleetapi.ErrNotSignedIn},
			{"guard, a stage session, cleanup", fleetapi.EnvStage, func(p fleetapi.Provider) fleetapi.Provider { return guardSessionEnv(p, true) }, nil},
			{"conflict", allowed, func(p fleetapi.Provider) fleetapi.Provider { return conflictProvider(p, conflict) }, conflict},
			{"conflict over the guard", allowed, func(p fleetapi.Provider) fleetapi.Provider {
				return conflictProvider(guardSessionEnv(p, false), conflict)
			}, conflict},
		} {
			n, now := 0, 0
			inner := providerWith(w.env, c.memory, c.projects, &n, &now)
			p := w.wrap(inner)
			at := func(what string) string {
				return w.name + ", memory " + map[bool]string{true: "yes", false: "no"}[c.memory] + ", projects " + map[bool]string{true: "yes", false: "no"}[c.projects] + ": " + what
			}

			// MemoryProvider and ProjectsProvider: there exactly when the provider has them.
			if _, ok := p.(fleetapi.MemoryProvider); ok != c.memory {
				t.Error(at("MemoryProvider through the wrapper"), ok)
			}
			if _, ok := p.(fleetapi.ProjectsProvider); ok != c.projects {
				t.Error(at("ProjectsProvider through the wrapper"), ok)
			}
			if memorySyncAvailable(p) != c.memory {
				t.Error(at("memorySyncAvailable"))
			}
			before := n
			_, perr := fleetapi.ProjectsOf(p).Create(ctx, fleetapi.ProjectCreateRequest{})
			_, merr := fleetapi.MemoryOf(p).Settings(ctx)
			_, m2 := fleetapi.MemoryOf(p).Push(ctx, fleetapi.MemoryPushRequest{})
			_, m3 := fleetapi.MemoryOf(p).Pull(ctx, fleetapi.MemoryPullRequest{})
			_, m4 := fleetapi.MemoryOf(p).PutSettings(ctx, fleetapi.MemorySettingsUpdate{})
			_, m5 := fleetapi.MemoryOf(p).Forget(ctx, fleetapi.MemoryForgetRequest{})
			_, m6 := fleetapi.MemoryOf(p).ForgetAll(ctx, fleetapi.MemoryForgetAllRequest{})
			_, m7 := fleetapi.MemoryOf(p).Export(ctx, func(fleetapi.MemoryRecord) error { return nil })
			reached := n - before
			wantReached := 0
			switch {
			case !c.projects:
				if !errors.Is(perr, fleetapi.ErrProjectsUnavailable) {
					t.Error(at("ProjectsOf without a registry"), perr)
				}
			case w.want == nil:
				wantReached++
				if perr != nil {
					t.Error(at("project create"), perr)
				}
			case !errors.Is(perr, w.want):
				t.Error(at("project create"), perr)
			}
			for i, err := range []error{merr, m2, m3, m4, m5, m6, m7} {
				switch {
				case !c.memory:
					if err == nil || errors.Is(err, fleetapi.ErrNotSignedIn) || errors.Is(err, conflict) {
						t.Error(at("MemoryOf without memory"), i, err)
					}
				case w.want == nil:
					wantReached++
					if err != nil {
						t.Error(at("memory call"), i, err)
					}
				case !errors.Is(err, w.want):
					t.Error(at("memory call"), i, err)
				}
			}
			if reached != wantReached {
				t.Error(at("calls that reached the provider"), reached, "want", wantReached)
			}

			// Auth: identity verification and both reloads.
			if _, ok := p.Auth().(fleetapi.IdentityVerifier); !ok {
				t.Error(at("IdentityVerifier lost"))
			}
			if _, ok := p.Auth().(fleetapi.SessionReloader); !ok {
				t.Error(at("SessionReloader lost"))
			}
			if _, ok := p.Auth().(fleetapi.SessionReloaderNow); !ok {
				t.Error(at("SessionReloaderNow lost"))
			}
			if err := fleetapi.ReloadSessionNow(ctx, p.Auth()); err != nil || now != 1 {
				t.Error(at("ReloadSessionNow did not reach the provider"), err, now)
			}
			// Context: listing.
			l, ok := p.Context().(fleetapi.ContextLister)
			if !ok {
				t.Error(at("ContextLister lost"))
				continue
			}
			if _, err := l.List(ctx, fleetapi.ListRequest{}); !errors.Is(err, w.want) {
				t.Error(at("list"), err)
			}
		}
	}

	// A provider without listing gains none.
	if _, ok := stub.New().Context().(fleetapi.ContextLister); !ok {
		if _, ok := guardSessionEnv(stub.New(), false).Context().(fleetapi.ContextLister); ok {
			t.Error("the guard invented ContextLister")
		}
		if _, ok := conflictProvider(stub.New(), conflict).Context().(fleetapi.ContextLister); ok {
			t.Error("the conflict provider invented ContextLister")
		}
	}
}

// The project registry through the guard, as the relay's memory sync
// reaches it (fleetapi.ProjectsOf): a prod or dev session this build uses
// creates the project; a session the guard turns away is signed out and
// nothing is sent.
func TestProjectRegistryThroughTheSessionGuard(t *testing.T) {
	ctx := context.Background()
	for _, env := range []fleetapi.Env{fleetapi.EnvProd, fleetapi.EnvDev, fleetapi.EnvStage} {
		n, now := 0, 0
		p := guardSessionEnv(providerWith(env, true, true, &n, &now), false)
		got, err := fleetapi.ProjectsOf(p).Create(ctx, fleetapi.ProjectCreateRequest{})
		if variant.Current.Allowed(string(env)) {
			if err != nil || got.ID != "p1" || n != 1 {
				t.Errorf("%s session in %s: %+v %v (%d reached)", env, variant.Current.Name, got, err, n)
			}
			continue
		}
		if !errors.Is(err, fleetapi.ErrNotSignedIn) || n != 0 {
			t.Errorf("%s session in %s: %v (%d reached)", env, variant.Current.Name, err, n)
		}
	}
}

// The app tests never take where files are, or which Fleet environment to
// use, from the shell that ran them (TestMain): a test that builds an Env
// without Getenv reads the process environment, and would otherwise write
// under an exported KENAZ_HOME.
func TestProcessEnvironmentIsIsolated(t *testing.T) {
	for _, name := range append([]string{variant.Shared.HomeVar, "CLAUDE_CONFIG_DIR", "CODEX_HOME"}, variant.Shared.EnvVars...) {
		if v := os.Getenv(name); v != "" {
			t.Errorf("%s=%q reaches the tests from the environment", name, v)
		}
	}
	home, err := os.UserHomeDir()
	if err != nil || !strings.HasPrefix(filepath.Base(home), "kenaz-app-home-") {
		t.Errorf("the home directory is %q (%v), not the tests' own", home, err)
	}
}

// forwardingProofs holds, for every fleetapi interface classified
// "optional", the proof that a wrapper forwards it: given a wrapped
// provider that implements it, the wrapper exposes it and, when reach is
// set (the wrapper lets calls through), the call arrives at the real
// implementation. Classifying an interface as optional without adding its
// proof here fails TestEveryOptionalInterfaceIsProvedForwarded, so the
// table above cannot say "forwarded" for something that is not.
var forwardingProofs = map[string]func(t *testing.T, p fleetapi.Provider, reach bool, c *proofCounters){
	"MemoryProvider": func(t *testing.T, p fleetapi.Provider, reach bool, c *proofCounters) {
		mp, ok := p.(fleetapi.MemoryProvider)
		if !ok || mp.Memory() == nil {
			t.Fatal("not exposed")
		}
		before := *c.fleet
		_, err := mp.Memory().Settings(context.Background())
		if reach && (err != nil || *c.fleet != before+1) {
			t.Fatalf("did not reach the provider's memory: %v", err)
		}
		if !reach && (err == nil || *c.fleet != before) {
			t.Fatalf("reached the provider's memory: %v", err)
		}
	},
	"ProjectsProvider": func(t *testing.T, p fleetapi.Provider, reach bool, c *proofCounters) {
		pp, ok := p.(fleetapi.ProjectsProvider)
		if !ok || pp.Projects() == nil {
			t.Fatal("not exposed")
		}
		before := *c.fleet
		got, err := pp.Projects().Create(context.Background(), fleetapi.ProjectCreateRequest{})
		if reach && (err != nil || got.ID != "p1" || *c.fleet != before+1) {
			t.Fatalf("did not reach the provider's registry: %+v %v", got, err)
		}
		if !reach && (err == nil || *c.fleet != before) {
			t.Fatalf("reached the provider's registry: %v", err)
		}
	},
	// Verifying an identity is a fresh sign-in at the browser, not a use of
	// the stored session: the guard lets it through whatever session is
	// stored, and only a conflict refuses it.
	"IdentityVerifier": func(t *testing.T, p fleetapi.Provider, _ bool, c *proofCounters) {
		v, ok := p.Auth().(fleetapi.IdentityVerifier)
		if !ok {
			t.Fatal("not exposed")
		}
		got, err := v.VerifyIdentity(context.Background(), fleetapi.LoginRequest{})
		if !c.conflict && (err != nil || got.UserID != "u") {
			t.Fatalf("did not reach the provider's verifier: %+v %v", got, err)
		}
		if c.conflict && err == nil {
			t.Fatal("verified an identity in conflict")
		}
	},
	// The reloads reach the provider through every wrapper: they change no
	// answer of the wrapper, and are how a new sign-in is picked up.
	"SessionReloader": func(t *testing.T, p fleetapi.Provider, _ bool, c *proofCounters) {
		r, ok := p.Auth().(fleetapi.SessionReloader)
		if !ok {
			t.Fatal("not exposed")
		}
		before := *c.periodic
		if err := r.ReloadSession(context.Background()); err != nil || *c.periodic != before+1 {
			t.Fatalf("did not reach the provider's reload: %v", err)
		}
	},
	"SessionReloaderNow": func(t *testing.T, p fleetapi.Provider, _ bool, c *proofCounters) {
		r, ok := p.Auth().(fleetapi.SessionReloaderNow)
		if !ok {
			t.Fatal("not exposed")
		}
		before := *c.now
		if err := r.ReloadSessionNow(context.Background()); err != nil || *c.now != before+1 {
			t.Fatalf("did not reach the provider's reload: %v", err)
		}
	},
	"ContextLister": func(t *testing.T, p fleetapi.Provider, reach bool, c *proofCounters) {
		l, ok := p.Context().(fleetapi.ContextLister)
		if !ok {
			t.Fatal("not exposed")
		}
		before := *c.fleet
		_, err := l.List(context.Background(), fleetapi.ListRequest{})
		if reach && (err != nil || *c.fleet != before+1) {
			t.Fatalf("did not reach the provider's listing: %v", err)
		}
		if !reach && (err == nil || *c.fleet != before) {
			t.Fatalf("reached the provider's listing: %v", err)
		}
	},
}

type proofCounters struct {
	fleet, periodic, now *int
	conflict             bool // the wrapper answers with a conflict
}

// proofProvider implements every optional interface, with a session for
// env, and counts what reaches it.
type proofProvider struct {
	fleetCalls
	c *proofCounters
}

func (p proofProvider) Auth() fleetapi.Auth {
	return bothReloadsAuth{sessionAuth{orgSessionAuth{p.Provider.Auth(), p.env}, p.env}, p.c.periodic, p.c.now}
}
func (p proofProvider) Memory() fleetapi.Memory     { return countedMemory{p.n} }
func (p proofProvider) Projects() fleetapi.Projects { return countedProjects{p.n} }

// Every interface classified optional is proved forwarded, by each wrapper:
// exposed, and reaching the wrapped provider's own implementation when the
// wrapper lets calls through. An interface classified optional with no
// proof, or a proof for one that is not classified optional, fails.
func TestEveryOptionalInterfaceIsProvedForwarded(t *testing.T) {
	for name, kind := range fleetapiInterfaces {
		if _, ok := forwardingProofs[name]; ok != (kind == "optional") {
			t.Errorf("fleetapi.%s is classified %q but has proof=%v in forwardingProofs", name, kind, ok)
		}
	}
	for name := range forwardingProofs {
		if fleetapiInterfaces[name] == "" {
			t.Errorf("forwardingProofs has %s, which fleetapiInterfaces does not list", name)
		}
	}
	conflict := errors.New("another build is using this environment")
	allowed := fleetapi.Env(variant.Current.DefaultEnv)
	for _, w := range []struct {
		name  string
		env   fleetapi.Env
		reach bool
		wrap  func(fleetapi.Provider) fleetapi.Provider
	}{
		{"unwrapped (the proofs themselves hold)", allowed, true, func(p fleetapi.Provider) fleetapi.Provider { return p }},
		{"the session guard, a session the build uses", allowed, true, func(p fleetapi.Provider) fleetapi.Provider { return guardSessionEnv(p, false) }},
		{"the session guard, a stage session", fleetapi.EnvStage, false, func(p fleetapi.Provider) fleetapi.Provider { return guardSessionEnv(p, false) }},
		{"the session guard for a cleanup command, a stage session", fleetapi.EnvStage, true, func(p fleetapi.Provider) fleetapi.Provider { return guardSessionEnv(p, true) }},
		{"the conflict provider", allowed, false, func(p fleetapi.Provider) fleetapi.Provider { return conflictProvider(p, conflict) }},
		{"the conflict provider over the guard", allowed, false, func(p fleetapi.Provider) fleetapi.Provider {
			return conflictProvider(guardSessionEnv(p, false), conflict)
		}},
	} {
		for name, proof := range forwardingProofs {
			t.Run(w.name+"/"+name, func(t *testing.T) {
				c := &proofCounters{new(int), new(int), new(int), strings.Contains(w.name, "conflict")}
				proof(t, w.wrap(proofProvider{fleetCalls{stub.New(), w.env, true, c.fleet}, c}), w.reach, c)
			})
		}
	}
}

// recordedEnroller keeps every body it is given, in order.
type recordedEnroller struct{ bodies *[]fleetapi.EnrollBody }

func (e recordedEnroller) Enroll(_ context.Context, b fleetapi.EnrollBody) (fleetapi.EnrollResult, error) {
	*e.bodies = append(*e.bodies, b)
	return fleetapi.EnrollResult{NodeID: b.NodeID, Tier: "team"}, nil
}

type enrollingProvider struct {
	fleetCalls
	e recordedEnroller
}

func (p enrollingProvider) Enroller() fleetapi.Enroller { return p.e }

// The relay's heartbeat enrolls without asking for the session. Behind the
// guard it is signed out with the session, like the other relay lanes: no
// enroll call is made for a session the guard turns away (a stage session
// in any build; a prod session in the staff dev build), and the heartbeat
// reads as signed out. For a session the build uses nothing changes: the
// same bodies reach the provider in the same order as without the guard.
func TestEnrollmentIsSignedOutWithTheSession(t *testing.T) {
	ctx := context.Background()
	beats := []fleetapi.EnrollBody{{NodeID: "n1"}, {NodeID: "n1"}, {NodeID: "n2"}}
	run := func(p fleetapi.Provider) (states []enroll.State, errs []error) {
		i := 0
		hb := &enroll.Heartbeat{Enroller: p.Enroller(), Body: func() (fleetapi.EnrollBody, error) { b := beats[i]; i++; return b, nil }}
		for range beats {
			s, err := hb.Once(ctx)
			states, errs = append(states, s), append(errs, err)
		}
		return states, errs
	}
	for _, env := range []fleetapi.Env{fleetapi.EnvProd, fleetapi.EnvDev, fleetapi.EnvStage} {
		var direct, guarded []fleetapi.EnrollBody
		n := 0
		run(enrollingProvider{fleetCalls{stub.New(), env, true, &n}, recordedEnroller{&direct}})
		states, errs := run(guardSessionEnv(enrollingProvider{fleetCalls{stub.New(), env, true, &n}, recordedEnroller{&guarded}}, false))
		if variant.Current.Allowed(string(env)) {
			if !reflect.DeepEqual(guarded, direct) || len(guarded) != len(beats) {
				t.Errorf("%s session in %s: the guard changed the enroll calls: %+v, without it %+v", env, variant.Current.Name, guarded, direct)
			}
			for i, s := range states {
				if s != enroll.StateEnrolled || errs[i] != nil {
					t.Errorf("%s session, beat %d: %s %v", env, i, s, errs[i])
				}
			}
			continue
		}
		if len(guarded) != 0 {
			t.Errorf("%s session in %s: %d enroll call(s) reached the provider", env, variant.Current.Name, len(guarded))
		}
		for i, s := range states {
			if s != enroll.StateSignedOut || !errors.Is(errs[i], fleetapi.ErrNotSignedIn) {
				t.Errorf("%s session, beat %d: %s %v", env, i, s, errs[i])
			}
		}
	}
	// In conflict (another build's relay runs the environment) nothing enrolls either.
	var got []fleetapi.EnrollBody
	n := 0
	conflict := errors.New("another build is using this environment")
	p := conflictProvider(enrollingProvider{fleetCalls{stub.New(), fleetapi.Env(variant.Current.DefaultEnv), true, &n}, recordedEnroller{&got}}, conflict)
	if _, err := p.Enroller().Enroll(ctx, beats[0]); !errors.Is(err, conflict) || len(got) != 0 {
		t.Errorf("conflict: %v, %d call(s)", err, len(got))
	}
	// A cleanup command sees a stage session as it is.
	got = nil
	p = guardSessionEnv(enrollingProvider{fleetCalls{stub.New(), fleetapi.EnvStage, true, &n}, recordedEnroller{&got}}, true)
	if _, err := p.Enroller().Enroll(ctx, beats[0]); err != nil || len(got) != 1 {
		t.Errorf("cleanup: %v, %d call(s)", err, len(got))
	}
}
