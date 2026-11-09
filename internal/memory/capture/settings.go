// Package capture is Kenaz's automatic memory. A member picks who writes the
// notes. By default it is the session's own agent, reminded now and then to
// save what is worth keeping. With a background writer, hooks observe the
// session and queue what happened, and a worker summarises the queue into typed
// notes in the local memory store.
//
// It is off for everyone by default, in two ways: release builds do not
// include it (features.MemoryCapture), and a build that does still needs the
// user to turn it on. It keeps three promises:
//
//   - It never affects the host session. Hooks write nothing to stdout,
//     always exit 0 and only queue. The worker is a separate process.
//   - Nothing captured enters telemetry. This package imports nothing that
//     talks to Kameas Fleet or queues data for it, a test pins that, and its
//     model calls turn the host's telemetry off.
//   - Raw tool output does not linger. The queue, which only a background
//     writer uses, holds only scrubbed,
//     truncated text, is bounded, expires, and each event is deleted once
//     processed, whether or not that succeeded.
//
// Nothing here sends a note to Fleet.
package capture

import (
	"strconv"
	"strings"
)

// Config keys in config.toml.
const (
	KeySwitch      = "memory_capture"
	KeyAck         = "memory_capture_ack"
	KeyWriter      = "memory_capture_writer"
	KeyModel       = "memory_capture_model"
	KeyHostCLI     = "memory_capture_host_cli"
	KeyCommand     = "memory_capture_command"
	KeyAPIKeyEnv   = "memory_capture_api_key_env"
	KeyDailyTokens = "memory_capture_daily_tokens"
	KeyDailyUSD    = "memory_capture_daily_usd"
	KeyExclude     = "memory_capture_exclude"
	KeySkipTools   = "memory_capture_skip_tools"
)

// Note writers: who turns a session into notes. The primary design is a
// background observer: hooks queue what happens, and a worker calls a model. A
// member picks the writer after seeing its line in the notice. There is no
// default, capture stays off until one is picked, and none falls back to
// another: a writer that cannot run pauses capture and says so. The session
// agent is a secondary fallback.
const (
	// WriterSessionAgent is the secondary, no-key fallback: the session's own
	// agent saves notes inline with memory_save, nudged by a short invisible
	// reminder. It reads no session content and writes far fewer notes.
	WriterSessionAgent = "session-agent"
	// WriterAPIKey runs the host CLI in its bare mode in the background, with
	// the member's own API key or cloud credentials. It never falls back to a
	// signed-in account: without a key, it stays off.
	WriterAPIKey = "api-key"
	// WriterLocal pipes each request to a command the member names, a model on
	// this device.
	WriterLocal = "local"
	// WriterHostCLI runs the vendor's unmodified CLI in the background under that
	// member's own login on this machine, drawing on their plan's limits. It is
	// never a default: a member selects it after seeing its model line. Kenaz
	// never reads, stores, copies or forwards the vendor's credentials or session
	// tokens: the CLI finds its own login. Fleet can switch it off per vendor with
	// no release (HostCLIAllowed).
	WriterHostCLI = "host-cli"
)

// Defaults for background writers. There is no default spending cap: usage is
// shown in status, and a daily cap is optional (KeyDailyTokens, KeyDailyUSD).
const (
	DefaultModel     = "claude-sonnet-5-5"
	DefaultAPIKeyEnv = "ANTHROPIC_API_KEY"
)

// AckVersion is the notice edition a member turned capture on under
// (notices.MemoryCaptureV1ID). A new edition needs a new value, so that nobody
// stays opted in to wording they never saw.
const AckVersion = "memory-capture/v1"

// The vendor behind the CLI this writer runs today, for the notice and for its
// off switch.
const (
	DefaultProvider = "Anthropic"
	DefaultTool     = "Claude Code"
	DefaultVendor   = "anthropic"
)

// Settings are the user's capture settings.
type Settings struct {
	On        bool
	Acked     bool
	Writer    string
	Model     string
	HostCLI   string
	Command   string
	APIKeyEnv string
	// DailyTokens and DailyUSD are optional caps; zero means none.
	DailyTokens int64
	DailyUSD    float64
	Exclude     []string
	// SkipTools adds tool names to the ones never captured.
	SkipTools []string
}

// LoadSettings reads settings through get (the config file's lookup). This
// package does not read config itself: the config package imports the Fleet
// interfaces, and this one must not.
func LoadSettings(get func(string) string) Settings {
	s := Settings{
		On:     strings.EqualFold(strings.TrimSpace(get(KeySwitch)), "on"),
		Acked:  strings.TrimSpace(get(KeyAck)) == AckVersion,
		Writer: strings.TrimSpace(get(KeyWriter)),
		Model:  strings.TrimSpace(get(KeyModel)), HostCLI: strings.TrimSpace(get(KeyHostCLI)),
		Command: strings.TrimSpace(get(KeyCommand)), APIKeyEnv: strings.TrimSpace(get(KeyAPIKeyEnv)),
		Exclude: splitList(get(KeyExclude)), SkipTools: splitList(get(KeySkipTools)),
	}
	if s.Model == "" {
		s.Model = DefaultModel
	}
	if s.APIKeyEnv == "" {
		s.APIKeyEnv = DefaultAPIKeyEnv
	}
	if !ValidKeyEnv(s.APIKeyEnv) {
		// Not a key: the writer finds none and stays paused.
		s.APIKeyEnv = "UNUSABLE_KEY_VARIABLE"
	}
	if n, err := strconv.ParseInt(strings.TrimSpace(get(KeyDailyTokens)), 10, 64); err == nil && n > 0 {
		s.DailyTokens = n
	}
	if f, err := strconv.ParseFloat(strings.TrimSpace(get(KeyDailyUSD)), 64); err == nil && f > 0 {
		s.DailyUSD = f
	}
	return s
}

// Active reports whether capture may run: turned on, with the current notice
// acknowledged and a known writer.
func (s Settings) Active() bool {
	if !s.On || !s.Acked {
		return false
	}
	switch s.Writer {
	case WriterSessionAgent, WriterAPIKey, WriterHostCLI:
		return true
	case WriterLocal:
		return s.Command != ""
	}
	return false
}

// Background reports whether this writer reads session content through hooks
// and calls a model from the worker.
func (s Settings) Background() bool {
	return s.Active() && s.Writer != WriterSessionAgent
}

func splitList(v string) []string {
	var out []string
	for _, p := range strings.Split(v, ",") {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}

// JoinList is the config form of a list. A comma cannot be part of an entry.
func JoinList(items []string) string { return strings.Join(items, ",") }
