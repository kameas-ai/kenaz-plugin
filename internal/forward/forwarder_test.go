package forward

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/kameas-ai/kenaz-plugin/fleetapi"
)

type poster struct{ err error }

func (p poster) Post(context.Context, fleetapi.ProjectedBatch) (fleetapi.ExportResult, error) {
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
		{&fleetapi.APIError{Status: 403, Code: fleetapi.CodeEarlyAccessRequired}, Refused},
		{&fleetapi.APIError{Status: 400, Code: "invalid_request"}, Quarantine},
		{&fleetapi.APIError{Status: 413}, Quarantine},
		{&fleetapi.APIError{Status: 429}, Retry},
		{&fleetapi.APIError{Status: 401}, Retry},
		{&fleetapi.APIError{Status: 503}, Retry},
		{errors.New("dial tcp: refused"), Retry},
	}
	for _, c := range cases {
		f := &Forwarder{Poster: poster{c.err}}
		if got := f.Send(context.Background(), fleetapi.ProjectedBatch{}); got.Disposition != c.want {
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
		f.Send(context.Background(), fleetapi.ProjectedBatch{})
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
