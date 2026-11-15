// Package fleetapitest is the public conformance suite every
// fleetapi.Provider must pass: the stub here, and the official adapter in
// the proprietary Fleet connection module.
//
// Live checks against a real Fleet run only when Options.Live is set, which
// happens only in the official builds' end-to-end job.
package fleetapitest

import (
	"context"
	"encoding/json"
	"errors"
	"slices"
	"testing"

	"github.com/kameas-ai/kenaz-plugin/fleetapi"
	"github.com/kameas-ai/kenaz-plugin/fleetapi/allowlist"
)

// Options configures Run.
type Options struct {
	// Live enables checks that talk to a real Fleet (official flavor
	// only).
	Live bool
}

// SampleEnrollBody is the canonical public fixture both flavors receive.
// The official adapter is driven with it through a capturing
// transport.
func SampleEnrollBody() fleetapi.EnrollBody {
	return fleetapi.EnrollBody{
		NodeID:     "01JABCDEFGHJKMNPQRSTVWXYZ0",
		Platform:   "darwin",
		Version:    "0.0.0-test",
		Hostname:   "macOS device 3f2a",
		Arch:       "arm64",
		ClientKind: fleetapi.ClientKindRelay,
	}
}

// SampleTeams is the canonical public fixture for MeInfo.Teams with Fleet's
// stored team tree: the default (all-members) team at the root, a team
// directly under it, and that team's sub-team. The default team is first, as
// Fleet lists it, and is never a parent or in a path. Slug and Membership are
// left empty, as Fleet does not send them yet. The unofficial stub refuses
// Me.Get, so it has no teams to fill; fakes and the official adapter's tests use
// this.
func SampleTeams() []fleetapi.TeamRef {
	const (
		everyone = "0b8f6f0e-0000-4000-8000-000000000001"
		platform = "0b8f6f0e-0000-4000-8000-000000000002"
		payments = "0b8f6f0e-0000-4000-8000-000000000003"
	)
	return []fleetapi.TeamRef{
		{ID: everyone, Name: "Everyone", IsDefault: true, Role: fleetapi.TeamRoleMember, Depth: 0, Path: []string{}},
		{ID: platform, Name: "Platform", Role: fleetapi.TeamRoleLead, Depth: 1, Path: []string{}},
		{ID: payments, Name: "Payments", Role: fleetapi.TeamRoleMember, ParentTeamID: platform, Depth: 2, Path: []string{platform}},
	}
}

// Run executes the suite against p.
func Run(t *testing.T, p fleetapi.Provider, opts Options) {
	t.Helper()

	t.Run("flavor", func(t *testing.T) {
		switch p.Flavor() {
		case fleetapi.FlavorOfficial, fleetapi.FlavorUnofficial:
		default:
			t.Fatalf("unknown flavor %q", p.Flavor())
		}
	})

	t.Run("sample enroll body is valid and fully allowlisted", func(t *testing.T) {
		b := SampleEnrollBody()
		if err := b.Validate(); err != nil {
			t.Fatalf("sample body invalid: %v", err)
		}
		raw, err := json.Marshal(b)
		if err != nil {
			t.Fatal(err)
		}
		var m map[string]any
		if err := json.Unmarshal(raw, &m); err != nil {
			t.Fatal(err)
		}
		allowed := allowlist.EnrollBodyKeys()
		for k := range m {
			if !slices.Contains(allowed, k) {
				t.Errorf("enroll key %q not in allowlist", k)
			}
		}
	})

	t.Run("sample teams form a tree under the default team", func(t *testing.T) {
		teams := SampleTeams()
		root, ok := fleetapi.DefaultTeam(teams)
		if !ok || !fleetapi.TreeKnown(teams) || teams[0].ID != root.ID {
			t.Fatalf("no default team first: %+v", teams)
		}
		byID := map[string]fleetapi.TeamRef{}
		for _, tm := range teams {
			byID[tm.ID] = tm
		}
		for _, tm := range teams {
			if tm.Role != fleetapi.TeamRoleMember && tm.Role != fleetapi.TeamRoleLead {
				t.Errorf("%s: role %q", tm.Name, tm.Role)
			}
			if tm.ParentTeamID == root.ID || slices.Contains(tm.Path, root.ID) {
				t.Errorf("%s: the default team is never a parent or in a path", tm.Name)
			}
			switch {
			case tm.IsDefault:
				if tm.Depth != 0 || tm.ParentTeamID != "" || len(tm.Path) != 0 {
					t.Errorf("default team: %+v", tm)
				}
			case tm.ParentTeamID == "":
				if tm.Depth != 1 || len(tm.Path) != 0 {
					t.Errorf("top-level team: %+v", tm)
				}
			default:
				parent, ok := byID[tm.ParentTeamID]
				if !ok || tm.Depth != parent.Depth+1 || len(tm.Path) != tm.Depth-1 || tm.Path[len(tm.Path)-1] != parent.ID {
					t.Errorf("sub-team: %+v", tm)
				}
			}
		}
	})

	if p.Flavor() == fleetapi.FlavorUnofficial {
		t.Run("unofficial build refuses every call", func(t *testing.T) {
			for name, err := range everyCall(p) {
				if !errors.Is(err, fleetapi.ErrUnofficialBuild) {
					t.Errorf("%s: got %v, want ErrUnofficialBuild", name, err)
				}
			}
		})
		return
	}

	if !opts.Live {
		t.Log("official flavor: live checks skipped (Options.Live=false)")
		return
	}
	// TODO: live checks against dev Fleet, e.g. Session
	// after login, Enroll(SampleEnrollBody()) and Capabilities.
}

// everyCall invokes each seam method once with zero-value arguments and
// returns its error.
func everyCall(p fleetapi.Provider) map[string]error {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	errs := map[string]error{}
	_, errs["Auth.Login"] = p.Auth().Login(ctx, fleetapi.LoginRequest{Env: fleetapi.EnvDev})
	errs["Auth.Logout"] = p.Auth().Logout(ctx)
	_, errs["Auth.Session"] = p.Auth().Session(ctx)
	errs["TokenStore.Clear"] = p.Auth().TokenStore().Clear(ctx)
	if v, ok := p.Auth().(fleetapi.IdentityVerifier); ok {
		_, errs["Auth.VerifyIdentity"] = v.VerifyIdentity(ctx, fleetapi.LoginRequest{Env: fleetapi.EnvDev})
	}
	_, errs["Enroller.Enroll"] = p.Enroller().Enroll(ctx, SampleEnrollBody())
	_, errs["Me.Get"] = p.Me().Get(ctx)
	_, errs["Me.Capabilities"] = p.Me().Capabilities(ctx)
	_, errs["Pollers.PendingApprovals"] = p.Pollers().PendingApprovals(ctx)
	_, errs["Pollers.LockdownStatus"] = p.Pollers().LockdownStatus(ctx)
	_, _, errs["Pollers.LockdownWait"] = p.Pollers().LockdownWait(ctx)
	_, errs["Pollers.OrgPause"] = p.Pollers().OrgPause(ctx)
	_, errs["Pollers.Configs"] = p.Pollers().Configs(ctx, fleetapi.ConfigsRequest{})
	errs["Pollers.AckConfigs"] = p.Pollers().AckConfigs(ctx, "", nil)
	_, errs["OptIns.Get"] = p.OptIns().Get(ctx)
	errs["OptIns.Put"] = p.OptIns().Put(ctx, fleetapi.OptInPut{})
	_, errs["Catalog.List"] = p.Catalog().List(ctx, fleetapi.CatalogQuery{})
	_, errs["Catalog.Get"] = p.Catalog().Get(ctx, "", "")
	_, errs["Catalog.Publish"] = p.Catalog().Publish(ctx, fleetapi.PublishRequest{})
	_, errs["Context.Search"] = p.Context().Search(ctx, fleetapi.SearchRequest{})
	_, errs["Context.Pull"] = p.Context().Pull(ctx, fleetapi.PullRequest{})
	_, errs["Context.Push"] = p.Context().Push(ctx, fleetapi.PushRequest{})
	_, errs["Context.Promote"] = p.Context().Promote(ctx, fleetapi.PromoteRequest{})
	_, errs["Team.Members"] = p.Team().Members(ctx, "")
	_, errs["OTLP.Post"] = p.OTLP().Post(ctx, fleetapi.ProjectedBatch{Signal: fleetapi.SignalLogs})
	m := fleetapi.MemoryOf(p)
	_, errs["Memory.Settings"] = m.Settings(ctx)
	_, errs["Memory.PutSettings"] = m.PutSettings(ctx, fleetapi.MemorySettingsUpdate{})
	_, errs["Memory.Pull"] = m.Pull(ctx, fleetapi.MemoryPullRequest{})
	_, errs["Memory.Push"] = m.Push(ctx, fleetapi.MemoryPushRequest{})
	_, errs["Memory.Forget"] = m.Forget(ctx, fleetapi.MemoryForgetRequest{})
	_, errs["Memory.ForgetAll"] = m.ForgetAll(ctx, fleetapi.MemoryForgetAllRequest{})
	_, errs["Memory.Export"] = m.Export(ctx, func(fleetapi.MemoryRecord) error { return nil })
	return errs
}

// IdentityVerifier is a fake fleetapi.IdentityVerifier for tests of the
// approval enrollment ceremony. It calls OpenURL with URL (if set) and
// returns Identity or Err. It never touches a session.
type IdentityVerifier struct {
	URL      string
	Identity fleetapi.VerifiedIdentity
	Err      error
	Calls    int
	// CompleteURL is the last request's completion URL (the browser
	// redirect target after the callback).
	CompleteURL string
}

// VerifyIdentity implements fleetapi.IdentityVerifier.
func (f *IdentityVerifier) VerifyIdentity(ctx context.Context, req fleetapi.LoginRequest) (fleetapi.VerifiedIdentity, error) {
	f.Calls++
	f.CompleteURL = req.CompleteURL
	if f.URL != "" && req.OpenURL != nil {
		if err := req.OpenURL(f.URL); err != nil {
			return fleetapi.VerifiedIdentity{}, err
		}
	}
	if err := ctx.Err(); err != nil {
		return fleetapi.VerifiedIdentity{}, err
	}
	return f.Identity, f.Err
}

var _ fleetapi.IdentityVerifier = (*IdentityVerifier)(nil)
