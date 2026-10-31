package forward

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/kameas-ai/kenaz-plugin/fleetapi"
)

type poster struct{ err error }

var owner = fleetapi.ExpectedSession{Env: fleetapi.EnvDev, OrgID: "org-1", Subject: "sub-1"}

func (p poster) Post(ctx context.Context, _ fleetapi.ProjectedBatch) (fleetapi.ExportResult, error) {
	if got, ok := fleetapi.ExpectedSessionFrom(ctx); !ok || got != owner {
		return fleetapi.ExportResult{}, fleetapi.IdentityMismatch()
	}
	if p.err != nil {
		return fleetapi.ExportResult{}, p.err
	}
	return fleetapi.ExportResult{Accepted: 1}, nil
}

func TestClassification(t *testing.T) {
	cases := []struct {
		err  error
		want Disposition
	}{
		{nil, Sent},
		{&fleetapi.APIError{Status: 403, Code: fleetapi.CodeOrgPaused}, Hold},
		{&fleetapi.APIError{Status: 403, Code: fleetapi.CodeCapabilityNotInTier, Details: map[string]string{"capability": fleetapi.PluginCapability}}, Refused},
		// A per-feature tier limit is not the whole-plugin refusal.
		{&fleetapi.APIError{Status: 403, Code: fleetapi.CodeCapabilityNotInTier, Details: map[string]string{"capability": "org_graph_sharing"}}, Quarantine},
		{&fleetapi.APIError{Status: 403, Code: fleetapi.CodeLegacyEarlyAccessRequired}, Refused},
		{&fleetapi.APIError{Status: 400, Code: "invalid_request"}, Quarantine},
		{&fleetapi.APIError{Status: 413}, Quarantine},
		{&fleetapi.APIError{Status: 429}, Retry},
		{&fleetapi.APIError{Status: 401}, Retry},
		{&fleetapi.APIError{Status: 503}, Retry},
		{errors.New("dial tcp: refused"), Retry},
		{fleetapi.IdentityMismatch(), Stale},
	}
	for _, c := range cases {
		f := &Forwarder{Poster: poster{c.err}}
		if got := f.Send(context.Background(), owner, fleetapi.ProjectedBatch{}); got.Disposition != c.want {
			t.Errorf("%v: got %v want %v", c.err, got.Disposition, c.want)
		}
	}
}

func TestTokenBucket(t *testing.T) {
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	var slept time.Duration
	f := &Forwarder{Poster: poster{}, PerMinute: 3,
		Now:   func() time.Time { return now },
		Sleep: func(_ context.Context, d time.Duration) error { slept += d; now = now.Add(d); return nil }}
	for i := 0; i < 4; i++ {
		f.Send(context.Background(), owner, fleetapi.ProjectedBatch{})
	}
	// Burst of 3, then one token every 20s.
	if slept < 19*time.Second || slept > 21*time.Second {
		t.Fatalf("slept %v", slept)
	}
}

func TestBackoff(t *testing.T) {
	if Backoff(0) != 5*time.Second || Backoff(1) != 10*time.Second || Backoff(20) != time.Hour {
		t.Fatal("backoff schedule")
	}
}

// The account switches while Send waits for a rate token: the owner is
// re-checked after the wait and nothing is posted.
func TestOwnerRecheckedAfterWait(t *testing.T) {
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	current := owner
	posts := 0
	f := &Forwarder{Poster: countingPoster{&posts}, PerMinute: 1,
		Now: func() time.Time { return now },
		Sleep: func(_ context.Context, d time.Duration) error {
			current = fleetapi.ExpectedSession{Env: fleetapi.EnvDev, OrgID: "org-1", Subject: "sub-2"} // switch mid-wait
			now = now.Add(d)
			return nil
		},
		Current: func(_ context.Context, o fleetapi.ExpectedSession) bool { return o == current },
	}
	if got := f.Send(context.Background(), owner, fleetapi.ProjectedBatch{}); got.Disposition != Sent {
		t.Fatalf("first send: %v", got.Disposition)
	}
	if got := f.Send(context.Background(), owner, fleetapi.ProjectedBatch{}); got.Disposition != Stale {
		t.Fatalf("send across the switch: %v", got.Disposition)
	}
	if posts != 1 {
		t.Fatalf("posted %d batches, want 1", posts)
	}
}

type countingPoster struct{ n *int }

func (p countingPoster) Post(context.Context, fleetapi.ProjectedBatch) (fleetapi.ExportResult, error) {
	*p.n++
	return fleetapi.ExportResult{Accepted: 1}, nil
}
