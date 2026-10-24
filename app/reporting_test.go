package app

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/kameas-ai/kenaz-plugin/fleetapi"
	"github.com/kameas-ai/kenaz-plugin/fleetapi/stub"
	"github.com/kameas-ai/kenaz-plugin/internal/daemon"
	"github.com/kameas-ai/kenaz-plugin/internal/fixtures"
	"github.com/kameas-ai/kenaz-plugin/internal/forward"
	"github.com/kameas-ai/kenaz-plugin/internal/setup"
)

// laneReport assembles the status fields the telemetry lane contributes,
// as the daemon does (session first, then contributors).
func laneReport(l *telemetryLane, signedIn bool) daemon.Status {
	s := daemon.Status{PendingApprovals: []string{}, Daemon: daemon.DaemonStatus{Running: true, Port: 4318}}
	if signedIn {
		s.Session = daemon.SessionSignedIn
	}
	l.ContributeStatus(&s)
	return s
}

// assertNoContent fails if a status carries identifiers or planted content.
func assertNoContent(t *testing.T, s daemon.Status) {
	t.Helper()
	b, _ := json.Marshal(s)
	bads := []string{"sub-1", "org-1"}
	for _, c := range fixtures.Canaries {
		bads = append(bads, c)
	}
	for _, bad := range bads {
		if strings.Contains(string(b), bad) {
			t.Fatalf("status carries %q: %s", bad, b)
		}
	}
}

// Status reports truthfully whether reporting works.
func TestReportingStatusStates(t *testing.T) {
	t.Run("not opted in", func(t *testing.T) {
		f := &laneFleet{Provider: stub.New()}
		l, _, _ := newTestLane(t, f)
		waitFor(t, func() bool { return l.cm.State().SignedIn })
		s := laneReport(l, true)
		if r := s.Reporting; r == nil || r.State != daemon.ReportingNoConsent || r.Consent != daemon.ConsentNone || !r.SignedIn || r.ReceiverPort != 4318 {
			t.Fatalf("reporting = %+v", s.Reporting)
		}
	})
	t.Run("signed out", func(t *testing.T) {
		f := &laneFleet{Provider: stub.New(), signedOut: true}
		l, _, _ := newTestLane(t, f)
		s := laneReport(l, false)
		if r := s.Reporting; r.State != daemon.ReportingNotSignedIn || r.SignedIn || r.Consent != daemon.ConsentUnknown {
			t.Fatalf("reporting = %+v", r)
		}
	})
	t.Run("held with queue and approval link", func(t *testing.T) {
		f := &laneFleet{Provider: stub.New(), optedIn: true, approvals: blockingApproval()}
		l, _, _ := newTestLane(t, f)
		queueHeldSession(t, f, l)
		s := laneReport(l, true)
		r := s.Reporting
		if r.State != daemon.ReportingHeld || r.HoldReason != "approval" || r.Queued == 0 || r.Consent != daemon.ConsentGiven || r.LastUploadAt != nil {
			t.Fatalf("reporting = %+v", r)
		}
		if len(s.PendingApprovals) != 1 || s.PendingApprovals[0] != "Terms" || s.DashboardURL != "https://dev.fleet.kameas.ai/" {
			t.Fatalf("approvals %v at %q", s.PendingApprovals, s.DashboardURL)
		}
		assertNoContent(t, s)
	})
	t.Run("working then failing", func(t *testing.T) {
		f := &laneFleet{Provider: stub.New(), optedIn: true}
		l, rx, _ := newTestLane(t, f)
		if !waitFor(t, func() bool { return l.dec.SignedIn() }) {
			t.Fatal("never signed in")
		}
		if r := laneReport(l, true).Reporting; r.State != daemon.ReportingWaitingForFirst {
			t.Fatalf("before any upload: %+v", r)
		}
		postClaudeSession(t, l, rx)
		if !waitFor(t, func() bool { return f.postedCount() > 0 }) {
			t.Fatal("nothing posted")
		}
		s := laneReport(l, true)
		if r := s.Reporting; r.State != daemon.ReportingWorking || r.LastUploadAt == nil || r.LastError != "" {
			t.Fatalf("after upload: %+v", r)
		}
		assertNoContent(t, s)

		f.set(func(f *laneFleet) { f.postErr = &fleetapi.APIError{Status: 503, Message: "secret server text"} })
		b := fleetapi.ProjectedBatch{Signal: fleetapi.SignalLogs, ContentType: fleetapi.ContentTypeProtobuf, Body: []byte{}, Records: 1}
		if l.send(context.Background(), queued{b, fixtures.Base, sub1Owner}) {
			t.Fatal("send reported success while Fleet fails")
		}
		s = laneReport(l, true)
		if r := s.Reporting; r.LastError != daemon.ErrCatServer || r.LastErrorAt == nil || r.Queued == 0 {
			t.Fatalf("failing: %+v", r)
		}
		if b, _ := json.Marshal(s); strings.Contains(string(b), "secret server text") {
			t.Fatal("server message leaked into status")
		}
	})
}

func TestUploadErrCategory(t *testing.T) {
	for _, c := range []struct {
		err  error
		want string
	}{
		{&fleetapi.APIError{Status: 401}, daemon.ErrCatAuth},
		{&fleetapi.APIError{Status: 429}, daemon.ErrCatRateLimited},
		{&fleetapi.APIError{Status: 502}, daemon.ErrCatServer},
		{errors.New("dial tcp: connection refused"), daemon.ErrCatNetwork},
	} {
		if got := uploadErrCategory(forwardRetry(c.err)); got != c.want {
			t.Errorf("%v → %s, want %s", c.err, got, c.want)
		}
	}
}

// Per-host setup state: not configured, configured, modified since setup.
func TestHostSetupStates(t *testing.T) {
	dir := t.TempDir()
	get := func() map[string]string {
		out := map[string]string{}
		for _, h := range hostSetups(dir) {
			out[h.Host] = h.State
		}
		return out
	}
	if g := get(); g["claude-code"] != setup.HostNotConfigured || g["codex"] != setup.HostNotConfigured {
		t.Fatalf("fresh: %v", g)
	}
	settings := filepath.Join(dir, "settings.json")
	os.WriteFile(settings, []byte(`{"env":{}}`+"\n"), 0o600)
	sum := sha256Hex([]byte(`{"env":{}}` + "\n"))
	os.WriteFile(filepath.Join(dir, "setup-state.json"), []byte(`{"version":1,"claude":{"path":`+jsonString(settings)+`,"original_absent":true,"written_sha256":"`+sum+`","created_container":true,"keys":{}}}`), 0o600)
	if g := get(); g["claude-code"] != setup.HostConfigured || g["codex"] != setup.HostNotConfigured {
		t.Fatalf("configured: %v", g)
	}
	os.WriteFile(settings, []byte(`{}`), 0o600)
	if g := get(); g["claude-code"] != setup.HostModified {
		t.Fatalf("modified: %v", g)
	}
}

func forwardRetry(err error) forward.Result {
	return forward.Result{Disposition: forward.Retry, Err: err}
}

func sha256Hex(b []byte) string { h := sha256.Sum256(b); return hex.EncodeToString(h[:]) }

func jsonString(s string) string { b, _ := json.Marshal(s); return string(b) }
