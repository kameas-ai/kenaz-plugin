package capture

import (
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"time"
)

// Failure codes: a closed set, safe to store and show. The text a model or
// a CLI produced on failure is never kept, since it may quote captured data.
const (
	FailCLIMissing = "cli_not_found"
	FailNeedsKey   = "needs_api_key"
	FailTimeout    = "timeout"
	FailExit       = "cli_failed"
	FailBadOutput  = "bad_output"
	FailStore      = "store_error"
	// FailRateLimited: the provider refused for rate or plan limits.
	FailRateLimited = "rate_limited"
	// FailRefused: the provider or host refused the call (sign-in, access).
	FailRefused = "refused"
	// PauseHostCLIOff: Fleet switched the host CLI writer off.
	PauseHostCLIOff = "host_cli_turned_off"
	// PauseHostCLIUnverified: Fleet has not confirmed lately that this writer is on.
	PauseHostCLIUnverified = "host_cli_unverified"
	// PauseOrgOff: the member's organization turned capture off.
	PauseOrgOff = "org_turned_off"
	// PauseNotOffered: Kameas Fleet answered, and doesn't offer capture to the
	// member's organization (no memory_capture_kinds).
	PauseNotOffered = "not_offered"
	// PauseUnconfirmed: no positive answer from Kameas Fleet lately (offline, an
	// unofficial build, or a failed read), so capture doesn't run.
	PauseUnconfirmed = "not_confirmed"
	// PauseSignedOutWriter: signed out, capture runs only with the api-key or local
	// writer, turned on while signed out.
	PauseSignedOutWriter = "signed_out_writer"
)

// SignedOutWriter reports a writer that may run while signed out: one that uses
// the member's own key or a model on this device. Neither reaches Kameas Fleet.
func SignedOutWriter(writer string) bool { return writer == WriterAPIKey || writer == WriterLocal }

// offerFresh is how old the positive answer that capture is offered may be. Without
// one this recent, hooks queue nothing and the worker processes nothing.
const offerFresh = 30 * time.Minute

// DayUsage is one day's model use.
type DayUsage struct {
	TokensIn  int64   `json:"tokens_in"`
	TokensOut int64   `json:"tokens_out"`
	USD       float64 `json:"usd"`
	Calls     int64   `json:"calls"`
}

// Tokens is the day's total.
func (d DayUsage) Tokens() int64 { return d.TokensIn + d.TokensOut }

// State is the worker's record of what it did: counters and codes only.
// Only the worker writes it.
type State struct {
	Writer string `json:"writer,omitempty"`
	// Usage holds the last 30 days by UTC date.
	Usage              map[string]DayUsage `json:"usage,omitempty"`
	Batches            int64               `json:"batches"`
	FailedBatches      int64               `json:"failed_batches"`
	Saved              int64               `json:"saved"`
	DroppedSecret      int64               `json:"dropped_secret"`
	DroppedOverCap     int64               `json:"dropped_over_cap"`
	Redactions         int64               `json:"redactions"`
	ConsecutiveFailure int64               `json:"consecutive_failures"`
	LastFailure        string              `json:"last_failure,omitempty"`
	LastFailureAt      int64               `json:"last_failure_at,omitempty"`
	LastSuccessAt      int64               `json:"last_success_at,omitempty"`
	PausedUntil        int64               `json:"paused_until,omitempty"`
	PauseReason        string              `json:"pause_reason,omitempty"`
	ProfileKey         string              `json:"profile_key,omitempty"`
	OrgOff             bool                `json:"org_off,omitempty"`
	HostCLIOff         bool                `json:"host_cli_off,omitempty"`
	HostCLIVerifiedAt  int64               `json:"host_cli_verified_at,omitempty"`
	SwitchesCheckedAt  int64               `json:"switches_checked_at,omitempty"`
	// OfferedAt is when Kameas Fleet last said it offers capture to the member's
	// organization; 0 once it didn't. OfferCode says why not.
	OfferedAt int64  `json:"offered_at,omitempty"`
	OfferCode string `json:"offer_code,omitempty"`
	// OfferedFor is the memory profile that answer is for ("local" while signed
	// out). Nothing queued is processed under another profile.
	OfferedFor string `json:"offered_for,omitempty"`
	// EnabledFor is the profile capture was turned on under. Capture runs signed out
	// only if it was turned on signed out.
	EnabledFor string `json:"enabled_for,omitempty"`
}

// NotOfferedText says in words why capture isn't running for want of Fleet's answer.
func NotOfferedText(reason string) string {
	switch reason {
	case PauseNotOffered:
		return "automatic memory capture isn't available for your organization yet."
	case PauseSignedOutWriter:
		return "while you're signed out, capture runs only with your own API key or a model on this device, turned on while signed out (`kenaz memory capture enable --writer api-key` or `--writer local`)."
	}
	return "Kameas Fleet hasn't confirmed lately that automatic memory capture is available for your organization. It starts again once it does."
}

// Offered reports a positive answer, no older than offerFresh, that Kameas Fleet
// offers capture to the member's organization. Capture runs only while it holds.
func (s State) Offered(now time.Time) bool {
	t := now.UnixMilli()
	return s.OfferedAt > 0 && s.OfferedAt <= t && t-s.OfferedAt <= offerFresh.Milliseconds()
}

// NotOfferedReason is why capture isn't running for want of Fleet's answer: a
// PauseNotOffered or PauseUnconfirmed code, or "" while it is offered.
func (s State) NotOfferedReason(now time.Time) string {
	if s.Offered(now) {
		return ""
	}
	if s.OfferCode == PauseNotOffered || s.OfferCode == PauseSignedOutWriter {
		return s.OfferCode
	}
	return PauseUnconfirmed
}

// LoadState reads the state; a missing or damaged file is a fresh state.
func LoadState(p Paths) State {
	var s State
	if b, err := os.ReadFile(p.state()); err == nil {
		_ = json.Unmarshal(b, &s)
	}
	if s.Usage == nil {
		s.Usage = map[string]DayUsage{}
	}
	return s
}

func saveState(p Paths, s State) error {
	if err := p.ensure(); err != nil {
		return err
	}
	b, err := json.MarshalIndent(s, "", "  ")
	if err != nil {
		return err
	}
	tmp := p.state() + ".tmp"
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, p.state())
}

func dayKey(t time.Time) string { return t.UTC().Format("2006-01-02") }

func (s *State) addUsage(now time.Time, u Usage) {
	k := dayKey(now)
	d := s.Usage[k]
	d.TokensIn += u.In
	d.TokensOut += u.Out
	d.USD += u.USD
	d.Calls++
	s.Usage[k] = d
	cutoff := dayKey(now.AddDate(0, 0, -30))
	for day := range s.Usage {
		if day < cutoff {
			delete(s.Usage, day)
		}
	}
}

// Paused reports whether capture is paused at now.
func (s State) Paused(now time.Time) bool { return s.PausedUntil > now.UnixMilli() }

// Report is capture's status, shared by the CLI and status output. It holds
// counters and codes only, never note or event content.
type Report struct {
	Available      bool     `json:"available"`
	On             bool     `json:"on"`
	Writer         string   `json:"writer,omitempty"`
	Model          string   `json:"model,omitempty"`
	Queued         int      `json:"queued"`
	Dropped        int64    `json:"dropped"`
	Batches        int64    `json:"batches"`
	FailedBatches  int64    `json:"failed_batches"`
	Saved          int64    `json:"saved"`
	DroppedSecret  int64    `json:"dropped_secret"`
	DroppedOverCap int64    `json:"dropped_over_cap"`
	Redactions     int64    `json:"redactions"`
	LastFailure    string   `json:"last_failure,omitempty"`
	LastFailureAt  string   `json:"last_failure_at,omitempty"`
	PausedUntil    string   `json:"paused_until,omitempty"`
	PauseReason    string   `json:"pause_reason,omitempty"`
	Today          DayUsage `json:"today"`
	Last30Days     DayUsage `json:"last_30_days"`
	DailyTokens    int64    `json:"daily_token_cap,omitempty"`
	DailyUSD       float64  `json:"daily_usd_cap,omitempty"`
	Excluded       []string `json:"excluded,omitempty"`
	// NotOffered is set while capture is on but not running because Kameas Fleet
	// hasn't said lately that it offers capture to the member's organization:
	// PauseNotOffered or PauseUnconfirmed.
	NotOffered string `json:"not_offered,omitempty"`
}

// BuildReport gathers the status.
func BuildReport(p Paths, s Settings, now time.Time) Report {
	st := LoadState(p)
	r := Report{
		Available: true, On: s.Active(), Writer: s.Writer, Model: s.Model,
		Queued: QueueLen(p), Dropped: droppedCount(p), Batches: st.Batches, FailedBatches: st.FailedBatches,
		Saved: st.Saved, DroppedSecret: st.DroppedSecret, DroppedOverCap: st.DroppedOverCap, Redactions: st.Redactions,
		LastFailure: st.LastFailure, Today: st.Usage[dayKey(now)], DailyTokens: s.DailyTokens, DailyUSD: s.DailyUSD,
		Excluded: s.Exclude,
	}
	if s.Writer == WriterLocal || s.Writer == WriterSessionAgent {
		r.Model = ""
	}
	if st.LastFailureAt != 0 {
		r.LastFailureAt = time.UnixMilli(st.LastFailureAt).UTC().Format(time.RFC3339)
	}
	if st.Paused(now) {
		r.PausedUntil = time.UnixMilli(st.PausedUntil).UTC().Format(time.RFC3339)
		r.PauseReason = st.PauseReason
	}
	if r.On {
		r.NotOffered = st.NotOfferedReason(now)
	}
	for _, d := range st.Usage {
		r.Last30Days.TokensIn += d.TokensIn
		r.Last30Days.TokensOut += d.TokensOut
		r.Last30Days.USD += d.USD
		r.Last30Days.Calls += d.Calls
	}
	return r
}

// Line is the one-line form for `kenaz status`.
func (r Report) Line() string {
	if !r.On {
		return "memory capture: off"
	}
	if r.NotOffered != "" {
		return "memory capture: turned on (" + r.Writer + "), but not running: " + NotOfferedText(r.NotOffered)
	}
	var b strings.Builder
	if r.Writer == WriterSessionAgent {
		b.WriteString("memory capture: on (" + r.Writer + ")")
		if r.PausedUntil != "" {
			fmt.Fprintf(&b, "; paused until %s (%s)", r.PausedUntil, r.PauseReason)
		}
		return b.String()
	}
	fmt.Fprintf(&b, "memory capture: on (%s); %d queued, %d notes saved; today %d tokens", r.Writer, r.Queued, r.Saved, r.Today.Tokens())
	if r.DailyTokens > 0 {
		fmt.Fprintf(&b, " of %d", r.DailyTokens)
	}
	if r.PausedUntil != "" {
		fmt.Fprintf(&b, "; paused until %s (%s)", r.PausedUntil, r.PauseReason)
	}
	if r.LastFailure != "" {
		fmt.Fprintf(&b, "; last failure %s, %d batches failed", r.LastFailure, r.FailedBatches)
	}
	if r.Dropped > 0 {
		fmt.Fprintf(&b, "; %d events dropped", r.Dropped)
	}
	return b.String()
}
