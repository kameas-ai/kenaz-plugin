package setup

import (
	"encoding/json"
	"fmt"
	"net"
	"net/url"
	"sort"
	"strings"
)

// Claude Code configuration: the "env" object of settings.json only.
// Every other key, including any existing hooks, is
// preserved in value.

const claudeEndpointKey = "OTEL_EXPORTER_OTLP_ENDPOINT"

// Content-logging switches the relay drops anyway. Setup turns them off
// so the harness does not collect content in the first place.
var claudeContentFlags = []string{
	"OTEL_LOG_USER_PROMPTS",
	"OTEL_LOG_ASSISTANT_RESPONSES",
	"OTEL_LOG_TOOL_DETAILS",
	"OTEL_LOG_TOOL_CONTENT",
	"OTEL_LOG_RAW_API_BODIES",
}

// Per-signal endpoints override OTEL_EXPORTER_OTLP_ENDPOINT, so a user who
// set one would silently bypass the relay.
var claudePerSignalEndpoints = []string{
	"OTEL_EXPORTER_OTLP_LOGS_ENDPOINT",
	"OTEL_EXPORTER_OTLP_METRICS_ENDPOINT",
	"OTEL_EXPORTER_OTLP_TRACES_ENDPOINT",
}

const claudeTracesBetaKey = "CLAUDE_CODE_ENHANCED_TELEMETRY_BETA"

type kv struct{ k, v string }

func claudeBaseEnv(port int) []kv {
	return []kv{
		{"CLAUDE_CODE_ENABLE_TELEMETRY", "1"},
		{"OTEL_LOGS_EXPORTER", "otlp"},
		{"OTEL_METRICS_EXPORTER", "otlp"},
		{"OTEL_EXPORTER_OTLP_PROTOCOL", "http/protobuf"},
		{claudeEndpointKey, fmt.Sprintf("http://127.0.0.1:%d", port)},
	}
}

func claudeTraceEnv() []kv {
	return []kv{{claudeTracesBetaKey, "1"}, {"OTEL_TRACES_EXPORTER", "otlp"}}
}

func claudeApply(cur []byte, port int, traces bool, prev *FileState) ([]byte, *FileState, bool, error) {
	obj, err := parseObject(cur)
	if err != nil {
		return nil, nil, false, fmt.Errorf("settings.json is not valid JSON (%v); nothing was changed", err)
	}
	fs := prev.clone()
	if fs == nil {
		fs = &FileState{Keys: map[string]KeyState{}}
	}
	env := &object{}
	if raw, ok := obj.get("env"); ok {
		if env, err = parseObject(raw); err != nil {
			return nil, nil, false, fmt.Errorf(`"env" in settings.json is not an object; nothing was changed`)
		}
	} else if prev == nil {
		fs.CreatedContainer = true
	}

	for _, k := range claudePerSignalEndpoints {
		if v, ok := env.get(k); ok {
			s, _ := rawString(v)
			return nil, nil, false, fmt.Errorf("env.%s is set (%q) and would override the relay endpoint; remove it, then re-run setup", k, s)
		}
	}
	if v, ok := env.get(claudeEndpointKey); ok {
		s, _ := rawString(v)
		if ks, owned := fs.Keys[claudeEndpointKey]; !(owned && sameJSON(v, json.RawMessage(ks.Written))) && !isLoopbackURL(s) {
			return nil, nil, false, fmt.Errorf("env.%s already exports to %q, which is not this machine; setup will not redirect another collector (remove it, then re-run setup)", claudeEndpointKey, s)
		}
	}

	changed := false
	set := func(k, val string) {
		want, _ := encodeString(val)
		curV, present := env.get(k)
		if !present || !sameJSON(curV, want) {
			env.set(k, want)
			changed = true
		}
		fs.record(k, present, string(curV), string(want))
	}
	for _, e := range claudeBaseEnv(port) {
		set(e.k, e.v)
	}
	if traces || fs.owns(claudeTracesBetaKey) {
		for _, e := range claudeTraceEnv() {
			set(e.k, e.v)
		}
	}
	for _, k := range claudeContentFlags {
		curV, present := env.get(k)
		if fs.owns(k) || !present || truthy(curV) {
			set(k, "0")
		}
	}
	if !changed {
		return cur, prev, false, nil
	}
	return renderWithEnv(obj, env, fs)
}

func renderWithEnv(obj, env *object, fs *FileState) ([]byte, *FileState, bool, error) {
	envRaw, err := env.render("")
	if err != nil {
		return nil, nil, false, err
	}
	obj.set("env", envRaw)
	out, err := obj.render("")
	if err != nil {
		return nil, nil, false, err
	}
	return append(out, '\n'), fs, true, nil
}

// claudeUndo restores each owned key that still holds the value setup
// wrote, and warns about (and keeps) any the user changed since.
func claudeUndo(cur []byte, fs *FileState) ([]byte, []string, error) {
	obj, err := parseObject(cur)
	if err != nil {
		return nil, nil, fmt.Errorf("settings.json is not valid JSON (%v); nothing was changed", err)
	}
	raw, ok := obj.get("env")
	if !ok {
		return cur, []string{`"env" was removed from settings.json since setup; nothing to restore`}, nil
	}
	env, err := parseObject(raw)
	if err != nil {
		return nil, nil, fmt.Errorf(`"env" in settings.json is not an object; nothing was changed`)
	}
	var warnings []string
	changed := false
	for _, k := range sortedKeys(fs.Keys) {
		ks := fs.Keys[k]
		curV, present := env.get(k)
		switch {
		case !present:
			// Already gone; nothing of ours to remove.
		case !sameJSON(curV, json.RawMessage(ks.Written)):
			warnings = append(warnings, fmt.Sprintf("env.%s changed since setup; left as is", k))
		case ks.PriorPresent:
			if !sameJSON(curV, json.RawMessage(ks.Prior)) {
				env.set(k, json.RawMessage(ks.Prior))
				changed = true
			}
		default:
			env.del(k)
			changed = true
		}
	}
	if !changed {
		return cur, warnings, nil
	}
	if fs.CreatedContainer && env.len() == 0 {
		obj.del("env")
		out, err := obj.render("")
		if err != nil {
			return nil, nil, err
		}
		return append(out, '\n'), warnings, nil
	}
	out, _, _, err := renderWithEnv(obj, env, fs)
	return out, warnings, err
}

func truthy(raw json.RawMessage) bool {
	s, ok := rawString(raw)
	if !ok {
		s = string(raw)
	}
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "", "0", "false", "no", "off", "null":
		return false
	}
	return true
}

func isLoopbackURL(s string) bool {
	if strings.TrimSpace(s) == "" {
		return true
	}
	u, err := url.Parse(s)
	if err != nil {
		return false
	}
	host := u.Hostname()
	if host == "localhost" {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

func sortedKeys(m map[string]KeyState) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}
