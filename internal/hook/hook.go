// Package hook implements the SessionStart/SessionEnd hook handlers that
// the plugin registers in both harnesses. It never emits permission decisions, rewrites tool input or stops
// a session; `make check-no-policy` enforces that on this
// package's sources.
//
// A hook must never fail or slow a harness session. Run always finishes
// within its deadline, writes either nothing or one valid JSON object to
// stdout, and the caller always exits 0.
package hook

import (
	"context"
	"encoding/json"
	"io"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/kameas-ai/kenaz-plugin/internal/daemonstate"
)

// Events, as passed on the command line by plugin/hooks/hooks.json.
const (
	EventSessionStart = "session-start"
	EventSessionEnd   = "session-end"
)

// Harness identifies which agent harness invoked the hook.
type Harness string

const (
	HarnessUnknown Harness = "unknown"
	HarnessClaude  Harness = "claude-code"
	HarnessCodex   Harness = "codex"
)

// DetectHarness tells the harnesses apart by the environment they give
// plugin hooks. Codex sets PLUGIN_ROOT (plus CLAUDE_PLUGIN_ROOT for
// compatibility, codex-rs hooks/src/engine/discovery.rs); Claude Code sets
// only CLAUDE_PLUGIN_ROOT.
func DetectHarness(getenv func(string) string) Harness {
	switch {
	case getenv("PLUGIN_ROOT") != "":
		return HarnessCodex
	case getenv("CLAUDE_PLUGIN_ROOT") != "":
		return HarnessClaude
	default:
		return HarnessUnknown
	}
}

// MaxContext caps additionalContext.
const MaxContext = 600

// Default deadlines. SessionStart is registered with a 5 s timeout and
// SessionEnd with 1 s (Claude's own SessionEnd default is 1.5 s), so both
// leave headroom for process start-up.
const (
	DefaultStartDeadline = 4 * time.Second
	DefaultEndDeadline   = 900 * time.Millisecond
)

// Daemon is the hook's view of the local relay daemon.
type Daemon interface {
	// Ensure starts the daemon if it is not running. It should return
	// promptly when ctx is done.
	Ensure(ctx context.Context) error
	// SessionEnd tells the daemon a harness session ended.
	SessionEnd(ctx context.Context, end daemonstate.SessionEnd) error
}

// Banners supplies the consent and org-state banners shown at session
// start (not signed in, pending blocking notice, org paused, lockdown,
// opt-ins not configured). Implemented by internal/consent.
type Banners interface {
	Banners(ctx context.Context) []string
}

// Skills reports whether Fleet skills were materialized or changed since
// the harness last scanned them.
type Skills interface {
	ChangedSinceLastScan(ctx context.Context) bool
}

// Memory supplies the on-device memory index for the session's project
// (titles and ids only). It must return "" when there is nothing to show
// or on any error, and return promptly when ctx is done.
type Memory interface {
	Index(ctx context.Context, cwd string) string
}

// MaxMemoryContext caps the memory index block, separately from the
// Fleet banners.
const MaxMemoryContext = 1536

// Deps are the hook's collaborators. Nil interfaces are treated as "no
// daemon / no banners / no skill changes / no memory".
type Deps struct {
	Daemon        Daemon
	Banners       Banners
	Skills        Skills
	Memory        Memory
	Getenv        func(string) string
	StartDeadline time.Duration
	EndDeadline   time.Duration
	// Tag, when set, prefixes the session-start context (the staff dev
	// build's "[kenaz-dev · dev]").
	Tag string
}

// input is the subset of the hook stdin payload both harnesses send.
type input struct {
	SessionID string `json:"session_id"`
	Reason    string `json:"reason"`
	Cwd       string `json:"cwd"`
}

// claudeStartOutput is Claude Code's SessionStart output shape.
type claudeStartOutput struct {
	HookSpecificOutput struct {
		HookEventName     string `json:"hookEventName"`
		AdditionalContext string `json:"additionalContext,omitempty"`
		ReloadSkills      bool   `json:"reloadSkills,omitempty"`
	} `json:"hookSpecificOutput"`
}

// codexStartOutput is Codex's SessionStart output shape. Codex rejects
// unknown fields (codex-rs hooks/src/schema.rs, deny_unknown_fields), so
// it has no reloadSkills; Codex hot-reloads skill directories itself.
type codexStartOutput struct {
	HookSpecificOutput struct {
		HookEventName     string `json:"hookEventName"`
		AdditionalContext string `json:"additionalContext,omitempty"`
	} `json:"hookSpecificOutput"`
}

// Run handles one hook invocation. It never returns an error: every
// failure degrades to writing nothing.
func Run(ctx context.Context, event string, stdin io.Reader, stdout io.Writer, deps Deps) {
	if deps.Getenv == nil {
		deps.Getenv = func(string) string { return "" }
	}
	switch event {
	case EventSessionStart:
		d := deps.StartDeadline
		if d <= 0 {
			d = DefaultStartDeadline
		}
		ctx, cancel := context.WithTimeout(ctx, d)
		defer cancel()
		sessionStart(ctx, stdin, stdout, deps)
	case EventSessionEnd:
		d := deps.EndDeadline
		if d <= 0 {
			d = DefaultEndDeadline
		}
		ctx, cancel := context.WithTimeout(ctx, d)
		defer cancel()
		sessionEnd(ctx, stdin, deps)
	}
}

func sessionStart(ctx context.Context, stdin io.Reader, stdout io.Writer, deps Deps) {
	harness := DetectHarness(deps.Getenv)

	// The memory index runs alongside the daemon start so a slow relay
	// never starves it. It is best-effort in both harnesses.
	mem := make(chan string, 1)
	go func() {
		if deps.Memory == nil {
			mem <- ""
			return
		}
		// A stdin that never closes costs at most stdinWait; the
		// index then falls back to the hook's working directory.
		rctx, cancel := context.WithTimeout(ctx, stdinWait)
		in, _ := readInput(rctx, stdin)
		cancel()
		mem <- deps.Memory.Index(ctx, in.Cwd)
	}()

	type result struct {
		banners []string
		reload  bool
	}
	done := make(chan result, 1)
	go func() {
		var r result
		if deps.Daemon != nil {
			_ = deps.Daemon.Ensure(ctx)
		}
		if deps.Banners != nil && ctx.Err() == nil {
			r.banners = deps.Banners.Banners(ctx)
		}
		if deps.Skills != nil && ctx.Err() == nil {
			r.reload = deps.Skills.ChangedSinceLastScan(ctx)
		}
		done <- r
	}()

	var r result
	var index string
	select {
	case r = <-done:
	case <-ctx.Done():
		return // out of time: say nothing rather than something partial
	}
	select {
	case index = <-mem:
	case <-ctx.Done():
		return
	}

	text := contextText(r.banners)
	if index = strings.TrimSpace(index); index != "" && len(index) <= MaxMemoryContext {
		if text != "" {
			text += "\n\n"
		}
		text += index
	}
	if text != "" && deps.Tag != "" {
		text = deps.Tag + " " + text
	}
	// reloadSkills is a Claude Code field; only send it when we are sure
	// we are talking to Claude Code.
	reload := r.reload && harness == HarnessClaude
	if text == "" && !reload {
		return
	}

	var v any
	if harness == HarnessClaude {
		var o claudeStartOutput
		o.HookSpecificOutput.HookEventName = "SessionStart"
		o.HookSpecificOutput.AdditionalContext = text
		o.HookSpecificOutput.ReloadSkills = reload
		v = o
	} else {
		var o codexStartOutput
		o.HookSpecificOutput.HookEventName = "SessionStart"
		o.HookSpecificOutput.AdditionalContext = text
		v = o
	}
	b, err := json.Marshal(v)
	if err != nil {
		return
	}
	_, _ = stdout.Write(append(b, '\n'))
}

func sessionEnd(ctx context.Context, stdin io.Reader, deps Deps) {
	in, ok := readInput(ctx, stdin)
	if !ok || in.SessionID == "" || deps.Daemon == nil {
		return
	}
	_ = deps.Daemon.SessionEnd(ctx, daemonstate.SessionEnd{
		SessionHash: daemonstate.SessionHash(in.SessionID),
		Reason:      boundedReason(in.Reason),
		Harness:     string(DetectHarness(deps.Getenv)),
	})
}

// stdinWait bounds how long session-start waits for its stdin payload.
const stdinWait = 500 * time.Millisecond

// maxInput bounds how much hook stdin is read; the payloads are small.
const maxInput = 1 << 20

func readInput(ctx context.Context, r io.Reader) (input, bool) {
	if r == nil {
		return input{}, false
	}
	type res struct {
		in input
		ok bool
	}
	ch := make(chan res, 1)
	go func() {
		var in input
		err := json.NewDecoder(io.LimitReader(r, maxInput)).Decode(&in)
		ch <- res{in, err == nil}
	}()
	select {
	case x := <-ch:
		return x.in, x.ok
	case <-ctx.Done():
		return input{}, false
	}
}

// Reasons are closed vocabularies in both harnesses (Claude: clear,
// resume, logout, prompt_input_exit, other; Codex: other). Anything else
// is reported as "other" so free text never reaches the daemon.
func boundedReason(r string) string {
	switch r {
	case "clear", "resume", "logout", "prompt_input_exit", "other":
		return r
	default:
		return "other"
	}
}

func contextText(banners []string) string {
	var lines []string
	for _, b := range banners {
		if b = strings.TrimSpace(b); b != "" {
			lines = append(lines, b)
		}
	}
	if len(lines) == 0 {
		return ""
	}
	return truncate("Kameas Fleet: "+strings.Join(lines, "\n"), MaxContext)
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	s = s[:n-len("…")]
	for !utf8.ValidString(s) {
		s = s[:len(s)-1]
	}
	return s + "…"
}
