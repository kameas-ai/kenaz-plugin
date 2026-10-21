package setup

import (
	"fmt"
	"regexp"
	"strings"
)

// Codex configuration: keys directly under [otel] in config.toml only.
// Codex reads no OTEL_* endpoint variables, so [otel] is
// the only switch. Project configs cannot set [otel]; managed_config.toml
// overrides it (see print-managed).

// codexOwned is the closed set of [otel] keys setup may write, in the
// order it writes them.
var codexOwned = []string{"exporter", "trace_exporter", "metrics_exporter", "log_user_prompt", "tool_result"}

var codexExporters = map[string]string{
	"exporter":         "logs",
	"trace_exporter":   "traces",
	"metrics_exporter": "metrics",
}

// codexValues are the desired [otel] lines. tool_result.max_bytes = 0
// stops Codex putting tool output in its own logs at all; the relay drops
// that field regardless. Codex accepts 0 (ToolResultLogConfig
// max_bytes is a plain usize, codex-rs protocol/src/config_types.rs).
func codexValues(port int) []kv {
	exp := func(signal string) string {
		return fmt.Sprintf(`{ otlp-http = { endpoint = "http://127.0.0.1:%d/v1/%s", protocol = "binary" } }`, port, signal)
	}
	return []kv{
		{"exporter", exp("logs")},
		{"trace_exporter", exp("traces")},
		{"metrics_exporter", exp("metrics")},
		{"log_user_prompt", "false"},
		{"tool_result", "{ max_bytes = 0 }"},
	}
}

var tomlEndpointRe = regexp.MustCompile(`endpoint\s*=\s*"([^"]*)"`)

func nonLoopbackEndpoint(text string) (string, bool) {
	for _, m := range tomlEndpointRe.FindAllStringSubmatch(text, -1) {
		if !isLoopbackURL(m[1]) {
			return m[1], true
		}
	}
	return "", false
}

func codexApply(cur []byte, port int, prev *FileState) ([]byte, *FileState, bool, error) {
	lines := splitLines(string(cur))
	sec, err := scanOtel(lines, codexOwned)
	if err != nil {
		return nil, nil, false, fmt.Errorf("config.toml: %v; nothing was changed", err)
	}
	fs := prev.clone()
	if fs == nil {
		fs = &FileState{Keys: map[string]KeyState{}}
	}
	eol := lineEnding(lines)

	for k := range codexExporters {
		if e, ok := sec.find(k); ok {
			if ep, bad := nonLoopbackEndpoint(joinLines(lines[e.start:e.end])); bad {
				return nil, nil, false, fmt.Errorf("[otel] %s already exports to %q, which is not this machine; setup will not redirect another collector (remove it, then re-run setup)", k, ep)
			}
		}
	}

	replace := map[int]tomlEntry{} // start line → entry being replaced
	newText := map[string]string{}
	var missing []string
	changed := false
	for _, want := range codexValues(port) {
		line := want.k + " = " + want.v
		e, present := sec.find(want.k)
		prior := ""
		if present {
			prior = joinLines(lines[e.start:e.end])
			if want.k == "log_user_prompt" && !fs.owns(want.k) && normTOML(prior) == normTOML(line) {
				continue // already off and not ours: leave it alone
			}
		}
		fs.record(want.k, present, prior, line)
		if present && normTOML(prior) == normTOML(line) {
			continue
		}
		changed = true
		newText[want.k] = line
		if present {
			replace[e.start] = e
		} else {
			missing = append(missing, want.k)
		}
	}
	if !changed {
		return cur, prev, false, nil
	}

	var out []string
	if sec.header < 0 {
		if prev == nil || prev.CreatedContainer {
			fs.CreatedContainer = true
		}
		out = append(out, lines...)
		if n := len(out); n > 0 {
			if !strings.HasSuffix(out[n-1], "\n") {
				out[n-1] += eol
			}
			if strings.TrimSpace(out[n-1]) != "" {
				out = append(out, eol)
			}
		}
		out = append(out, "[otel]"+eol)
		for _, k := range missing {
			out = append(out, newText[k]+eol)
		}
		return []byte(joinLines(out)), fs, true, nil
	}

	insertAt := sec.header + 1
	for _, e := range sec.entries {
		insertAt = max(insertAt, e.end)
	}
	for i := 0; i <= len(lines); i++ {
		if i == insertAt {
			if n := len(out); n > 0 && !strings.HasSuffix(out[n-1], "\n") {
				out[n-1] += eol
			}
			for _, k := range missing {
				out = append(out, newText[k]+eol)
			}
		}
		if i == len(lines) {
			break
		}
		if e, ok := replace[i]; ok {
			out = append(out, newText[e.key]+trailingEOL(lines[e.end-1], eol))
			i = e.end - 1
			continue
		}
		out = append(out, lines[i])
	}
	return []byte(joinLines(out)), fs, true, nil
}

// codexUndo restores owned [otel] keys that still hold what setup wrote.
func codexUndo(cur []byte, fs *FileState) ([]byte, []string, error) {
	lines := splitLines(string(cur))
	sec, err := scanOtel(lines, codexOwned)
	if err != nil {
		return nil, nil, fmt.Errorf("config.toml: %v; nothing was changed", err)
	}
	var warnings []string
	edits := map[int]struct {
		end  int
		text string
	}{}
	for _, k := range codexOwned {
		ks, owned := fs.Keys[k]
		if !owned {
			continue
		}
		e, present := sec.find(k)
		switch {
		case !present:
		case normTOML(joinLines(lines[e.start:e.end])) != normTOML(ks.Written):
			warnings = append(warnings, fmt.Sprintf("[otel] %s changed since setup; left as is", k))
		case ks.PriorPresent:
			edits[e.start] = struct {
				end  int
				text string
			}{e.end, ks.Prior}
		default:
			edits[e.start] = struct {
				end  int
				text string
			}{e.end, ""}
		}
	}
	if len(edits) == 0 {
		return cur, warnings, nil
	}
	var out []string
	for i := 0; i < len(lines); i++ {
		if ed, ok := edits[i]; ok {
			if ed.text != "" {
				out = append(out, ed.text)
			}
			i = ed.end - 1
			continue
		}
		out = append(out, lines[i])
	}
	if fs.CreatedContainer {
		out = dropEmptyOtel(out)
	}
	return []byte(joinLines(out)), warnings, nil
}

// dropEmptyOtel removes an [otel] table that setup created and that is now
// empty, together with the blank separator line setup added before it.
func dropEmptyOtel(lines []string) []string {
	sec, err := scanOtel(lines, codexOwned)
	if err != nil || sec.header < 0 || len(sec.entries) > 0 {
		return lines
	}
	for _, l := range lines[sec.header+1 : sec.end] {
		if strings.TrimSpace(l) != "" {
			return lines
		}
	}
	start := sec.header
	if start > 0 && strings.TrimSpace(lines[start-1]) == "" && sec.end == len(lines) {
		start--
	}
	return append(append([]string{}, lines[:start]...), lines[sec.end:]...)
}

func trailingEOL(line, eol string) string {
	if strings.HasSuffix(line, "\n") {
		return eol
	}
	return ""
}
