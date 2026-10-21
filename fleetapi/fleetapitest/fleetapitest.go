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
	return errs
}
