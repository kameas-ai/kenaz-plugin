package consent

import (
	"errors"
	"fmt"
	"html"
	"regexp"
	"strings"

	"github.com/kameas-ai/kenaz-plugin/notices"
)

// NoticeVars fills the Agent Telemetry Notice placeholders.
type NoticeVars struct {
	OrgName           string   // {Organization name}
	Harnesses         []string // e.g. ["Claude Code", "Codex"]; filled as "A", "B" or "A and B"
	DeviceDisplayName string   // {device display name}
	HarnessVersions   []string // e.g. ["Claude Code 2.1.219", "Codex 0.147.0"]
	// Edition is the notice revision to render (Manager.NoticeEdition).
	// The zero value renders v1.
	Edition notices.Edition
}

// placeholder matches any brace token left in a notice template, including
// per-plan build-state markers such as "{30 / 90 / 365}" or
// "{Enterprise only: ...}" that RenderNotice cannot fill.
var placeholder = regexp.MustCompile(`\{[^{}\n]*\}`)

// ErrUnfilledNotice means a placeholder was left empty. The notice is never
// shown half-filled.
var ErrUnfilledNotice = errors.New("consent: notice has unfilled placeholders")

// NoticeVersion is the version string stored with an opt-in. It
// changes when either the notice edition or the projection field list
// changes, which forces re-display and re-confirmation.
func NoticeVersion(editionID string, projectionVersion int) string {
	if editionID == "" {
		editionID = notices.AgentTelemetryV1ID
	}
	return fmt.Sprintf("%s+p%d", editionID, projectionVersion)
}

// RenderNotice returns the notice text with placeholders filled. The
// terminal prints it as-is. The browser page embeds the same bytes (see
// RenderNoticeHTML), so both surfaces match word for word.
func RenderNotice(v NoticeVars) (string, error) {
	repl := map[string]string{
		"{Organization name}":   v.OrgName,
		"{harnesses}":           joinAnd(v.Harnesses),
		"{device display name}": v.DeviceDisplayName,
		"{harness versions}":    joinAnd(v.HarnessVersions),
	}
	for k, val := range repl {
		if strings.TrimSpace(val) == "" {
			return "", fmt.Errorf("%w: %s", ErrUnfilledNotice, k)
		}
	}
	out := v.Edition.Text
	if v.Edition.ID == "" {
		out = notices.AgentTelemetryV1
	}
	// Check the template, not the filled text: an unresolved marker must
	// never reach the member, and a brace in an organization or device
	// name must not stop the notice from rendering.
	rest := out
	for k := range repl {
		rest = strings.ReplaceAll(rest, k, "")
	}
	if m := placeholder.FindString(rest); m != "" {
		return "", fmt.Errorf("%w: %s", ErrUnfilledNotice, m)
	}
	for k, val := range repl {
		out = strings.ReplaceAll(out, k, val)
	}
	return out, nil
}

// RenderNoticeHTML wraps RenderNotice's exact text for the browser page
// that login opens. The text is escaped, never reformatted, so unescaping
// the <pre> body yields the terminal bytes.
func RenderNoticeHTML(v NoticeVars) (string, error) {
	text, err := RenderNotice(v)
	if err != nil {
		return "", err
	}
	return "<!doctype html><meta charset=\"utf-8\"><title>Kenaz for Claude Code and Codex: agent telemetry</title>" +
		"<pre id=\"notice\" style=\"white-space:pre-wrap;font:15px/1.5 system-ui,sans-serif;max-width:46rem;margin:2rem auto\">" +
		html.EscapeString(text) + "</pre>", nil
}

func joinAnd(xs []string) string {
	var clean []string
	for _, x := range xs {
		if s := strings.TrimSpace(x); s != "" {
			clean = append(clean, s)
		}
	}
	switch len(clean) {
	case 0:
		return ""
	case 1:
		return clean[0]
	}
	return strings.Join(clean[:len(clean)-1], ", ") + " and " + clean[len(clean)-1]
}
