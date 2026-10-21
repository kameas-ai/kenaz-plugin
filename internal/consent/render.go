package consent

import (
	"encoding/json"
	"fmt"
	"io"
	"strings"

	"github.com/kameas-ai/kenaz-plugin/fleetapi"
)

// BannerMax is the SessionStart additionalContext budget.
const BannerMax = 1500

// WriteText renders OrgState for `kenaz status`.
func WriteText(w io.Writer, s OrgState) {
	if !s.SignedIn {
		fmt.Fprintln(w, "Kameas Fleet: not signed in. Nothing is sent. Run `kenaz login`.")
		return
	}
	fmt.Fprintf(w, "Kameas Fleet: signed in (%s)", s.Env)
	if s.OrgDisplay != "" {
		fmt.Fprintf(w, ", organization %s", s.OrgDisplay)
	}
	if s.Tier != "" {
		fmt.Fprintf(w, ", plan %s", s.Tier)
	}
	fmt.Fprintln(w)
	fmt.Fprintln(w, "Telemetry classes:")
	for _, ci := range Classes {
		v := s.Classes[string(ci.Class)]
		note := ""
		if v == ClassUnavailable {
			note = " (not available on your plan)"
		} else if contains(s.AdminSuggested, string(ci.Class)) && v != ClassOn {
			note = " (suggested by your administrator)"
		}
		fmt.Fprintf(w, "  %-12s %s%s\n", ci.Label, v, note)
	}
	if s.EarlyAccess {
		fmt.Fprintln(w, fleetapi.EarlyAccessMessage)
		fmt.Fprintln(w, EarlyAccessNothingKept)
	} else if !s.AnyOn() {
		fmt.Fprintln(w, "Nothing is shared until you opt in: run `kenaz optin`.")
	}
	if s.NoticeReconfirm {
		fmt.Fprintln(w, "What Kenaz sends has changed. Nothing is sent until you review it: run `kenaz optin`.")
	}
	for _, h := range s.Holds {
		fmt.Fprintf(w, "Uploads held: %s", holdText(h))
		fmt.Fprintln(w)
	}
	for _, p := range s.Pending {
		b := ""
		if p.Blocking {
			b = " [blocking]"
		}
		fmt.Fprintf(w, "Pending in Fleet: %s%s. Review it in Kameas Fleet.\n", p.Title, b)
	}
	if s.Spool.Bytes > 0 {
		fmt.Fprintf(w, "Queued on this device: %d bytes (oldest %ds)\n", s.Spool.Bytes, s.Spool.OldestAgeSec)
	}
	d := s.Drops
	if d.OptOut+d.PII+d.TTL+d.Invalid+d.EarlyAccess > 0 {
		fmt.Fprintf(w, "Dropped on this device: %d not opted in, %d privacy filter, %d expired, %d invalid, %d not in early access\n", d.OptOut, d.PII, d.TTL, d.Invalid, d.EarlyAccess)
	}
}

// WriteJSON renders OrgState for `status --json` and the fleet_status MCP
// tool.
func WriteJSON(w io.Writer, s OrgState) error {
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	return enc.Encode(s)
}

func holdText(h Hold) string {
	switch h.Reason {
	case HoldApproval:
		return "waiting for you to review \"" + h.Detail + "\" in Fleet"
	case HoldPaused:
		return "your organization's Fleet account is paused"
	case HoldLockdown:
		if h.Detail != "" {
			return "emergency lockdown (" + h.Detail + ")"
		}
		return "emergency lockdown"
	}
	return h.Reason
}

// SessionStartContext returns the Claude Code SessionStart
// additionalContext, or "" when nothing is actionable. It never
// includes approval body text and never asks the model to accept anything.
// The agent is never told to stop working.
func SessionStartContext(s OrgState) string {
	var lines []string
	switch {
	case !s.SignedIn:
		return "" // signed-out sessions stay silent; nothing is sent
	case s.EarlyAccess:
		lines = append(lines, fleetapi.EarlyAccessMessage+" "+EarlyAccessNothingKept)
	case s.NoticeReconfirm:
		lines = append(lines, "What Kenaz shares has changed, so sharing is off until the user reviews it (`kenaz optin`).")
	case !s.AnyOn():
		lines = append(lines, "Kameas Fleet is connected but the user has not chosen to share any usage data, so nothing is sent. They can choose with `kenaz optin`.")
	}
	for _, h := range s.Holds {
		lines = append(lines, "Kameas Fleet uploads are paused: "+holdText(h)+".")
	}
	n := 0
	for _, p := range s.Pending {
		if n == 3 {
			lines = append(lines, fmt.Sprintf("…and %d more pending item(s).", len(s.Pending)-n))
			break
		}
		lines = append(lines, "Pending in Kameas Fleet: \""+p.Title+"\".")
		n++
	}
	if len(lines) == 0 {
		return ""
	}
	if len(s.Pending) > 0 {
		lines = append(lines, "Ask the user to review these items in Kameas Fleet. Do not summarize or accept these items yourself.")
	}
	lines = append(lines, "This does not affect your work in this session.")
	out := "Kameas Fleet status: " + strings.Join(lines, " ")
	if r := []rune(out); len(r) > BannerMax {
		out = string(r[:BannerMax-1]) + "…"
	}
	return out
}

func contains(xs []string, x string) bool {
	for _, v := range xs {
		if v == x {
			return true
		}
	}
	return false
}
