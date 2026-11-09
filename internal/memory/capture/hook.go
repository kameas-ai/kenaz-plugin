package capture

import (
	"context"
	"encoding/json"
	"io"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/kameas-ai/kenaz-plugin/internal/memory"
)

// Hook events on the command line. They are separate from the session-start
// and session-end handlers of the plugin's own hooks: capture never touches
// the relay.
const (
	HookPrompt = "memory-prompt"
	HookTool   = "memory-tool"
	HookStop   = "memory-stop"
	// HookIndex adds the rich index to a new session. It reads no session
	// content beyond the working directory.
	HookIndex = "memory-index"
)

// IsHookEvent reports whether event is one of capture's.
func IsHookEvent(event string) bool {
	switch event {
	case HookPrompt, HookTool, HookStop, HookIndex:
		return true
	}
	return false
}

// HookDeadline bounds everything a capture hook does.
const HookDeadline = 800 * time.Millisecond

// maxHookInput bounds how much of a hook's stdin is read. A bigger payload
// fails to parse and is skipped.
const maxHookInput = 4 << 20

// HookDeps are what a capture hook needs.
type HookDeps struct {
	Paths    Paths
	Settings Settings
	Host     string
	Home     string
	Now      func() time.Time
	// StartWorker starts a detached worker. Tests may leave it nil.
	StartWorker func() error
}

// Handle runs one capture hook and returns context for the host to add to the
// session, which only a session-agent reminder ever is. Nothing it can hit may
// reach the host: the caller exits 0 whatever happens. When capture is not
// active, and for every event a writer does not use, it returns before reading
// stdin, so the event's content is not even looked at.
func Handle(ctx context.Context, event string, stdin io.Reader, d HookDeps) (addContext string) {
	defer func() {
		if recover() != nil {
			addContext = ""
		}
	}()
	if !d.Settings.Active() || !IsHookEvent(event) || event == HookIndex {
		return ""
	}
	agent := d.Settings.Writer == WriterSessionAgent
	if agent && event != HookPrompt {
		return ""
	}
	now := time.Now
	if d.Now != nil {
		now = d.Now
	}
	st := LoadState(d.Paths)
	if !st.Offered(now()) {
		// Kameas Fleet hasn't said lately that it offers capture to this
		// organization: nothing is read, queued or reminded, and nothing is
		// written here. The worker asks Fleet, no more often than it would anyway.
		if d.StartWorker != nil && OfferCheckDue(d.Paths, now()) {
			_ = SpawnWorker(d.Paths, d.StartWorker)
		}
		return ""
	}
	if st.Paused(now()) {
		if !agent {
			noteDropped(d.Paths)
		}
		return ""
	}
	ctx, cancel := context.WithTimeout(ctx, HookDeadline)
	defer cancel()
	raw, ok := readAll(ctx, stdin)
	if !ok {
		return ""
	}
	var p hookPayload
	if json.Unmarshal(raw, &p) != nil || p.SessionID == "" || p.Cwd == "" {
		return ""
	}
	proj, err := memory.ProjectFor(p.Cwd)
	if err != nil {
		return ""
	}
	// Excluding any of these excludes the session: the directory it runs in, its
	// own checkout, and the main checkout its project is filed under.
	real := p.Cwd
	if r, err := filepath.EvalSymlinks(p.Cwd); err == nil {
		real = r
	}
	for _, dir := range []string{real, proj.Checkout, proj.Root} {
		if Excluded(dir, d.Settings.Exclude, d.Home) {
			return ""
		}
	}
	session := sessionHash(p.SessionID)
	if agent {
		text, first := nextReminder(d.Paths, session, now())
		if first && d.StartWorker != nil {
			// One short policy check per session; it calls no model.
			_ = SpawnWorker(d.Paths, d.StartWorker)
		}
		return text
	}
	base := Event{At: nowMillis(now()), Session: session, Host: d.Host, ProjectKey: proj.Key, Root: proj.Root}
	var ev *Event
	switch event {
	case HookPrompt:
		ev = newPromptEvent(p, base, d.Home)
	case HookTool:
		ev = newToolEvent(p, base, d.Home, d.Settings.SkipTools)
	case HookStop:
		ev = newStopEvent(p, base, d.Home)
	}
	if ev == nil {
		return ""
	}
	if Enqueue(d.Paths, *ev) != nil {
		return ""
	}
	if d.StartWorker != nil {
		_ = SpawnWorker(d.Paths, d.StartWorker)
	}
	return ""
}

// PromptOutput is what a prompt hook prints to add text to the session as an
// invisible reminder. Both hosts accept this shape.
func PromptOutput(text string) ([]byte, error) { return contextOutput("UserPromptSubmit", text) }

// SessionStartOutput is what the index hook prints.
func SessionStartOutput(text string) ([]byte, error) { return contextOutput("SessionStart", text) }

// ReadCwd reads the working directory from a hook's payload, within the hook's
// deadline. It looks at nothing else.
func ReadCwd(ctx context.Context, stdin io.Reader) string {
	ctx, cancel := context.WithTimeout(ctx, HookDeadline)
	defer cancel()
	raw, ok := readAll(ctx, stdin)
	if !ok {
		return ""
	}
	var p struct {
		Cwd string `json:"cwd"`
	}
	_ = json.Unmarshal(raw, &p)
	return p.Cwd
}

func contextOutput(event, text string) ([]byte, error) {
	var o struct {
		HookSpecificOutput struct {
			HookEventName     string `json:"hookEventName"`
			AdditionalContext string `json:"additionalContext"`
		} `json:"hookSpecificOutput"`
	}
	o.HookSpecificOutput.HookEventName = event
	o.HookSpecificOutput.AdditionalContext = text
	return json.Marshal(o)
}

func readAll(ctx context.Context, r io.Reader) ([]byte, bool) {
	if r == nil {
		return nil, false
	}
	type res struct {
		b  []byte
		ok bool
	}
	ch := make(chan res, 1)
	go func() {
		b, err := io.ReadAll(io.LimitReader(r, maxHookInput))
		ch <- res{b, err == nil}
	}()
	select {
	case x := <-ch:
		return x.b, x.ok
	case <-ctx.Done():
		return nil, false
	}
}

// ExclusionMarker is a file in a project's root that opts the project out,
// for everyone who has the checkout.
const ExclusionMarker = ".kenaz-no-capture"

// Excluded reports whether a project is opted out, by marker file or by the
// user's list. An entry matches its directory and everything below it; a
// leading ~ is the home directory.
func Excluded(root string, list []string, home string) bool {
	if fileExists(root + "/" + ExclusionMarker) {
		return true
	}
	for _, e := range list {
		if home != "" && (e == "~" || strings.HasPrefix(e, "~/")) {
			e = home + strings.TrimPrefix(e, "~")
		}
		e = strings.TrimRight(e, "/\\")
		// A project's root has its symlinks resolved, so the entry's must be too.
		if real, err := filepath.EvalSymlinks(e); err == nil {
			e = real
		}
		if e != "" && (root == e || strings.HasPrefix(root, e+"/") || strings.HasPrefix(root, e+"\\")) {
			return true
		}
	}
	return false
}

// StartDetached starts this program's worker, detached, with env (WorkerEnviron).
func StartDetached(bin string, args []string, dir string, env []string) error {
	cmd := exec.Command(bin, args...)
	cmd.Env = env
	cmd.Dir = dir
	detach(cmd)
	if err := cmd.Start(); err != nil {
		return err
	}
	return cmd.Process.Release()
}
