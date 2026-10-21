package memory

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"time"
)

// Notice is the user-facing statement about where memories go. It appears
// verbatim in the READMEs and in `kenaz memory --help`.
const Notice = "Memories are stored only on this device and are never sent to Kameas. When your AI tool uses a memory in a session, it becomes part of that session and is sent to your AI provider (Anthropic or OpenAI) like anything else in the conversation."

// OffMessage is returned by every memory tool while memory is turned off.
const OffMessage = "Kenaz memory is turned off. Turn it on with `kenaz memory enable`."

// Disclosure is the one-time notice shown when memory is first used.
func Disclosure(path string) string {
	return fmt.Sprintf("Kenaz memory is on: your AI tool can save notes to %s on this device. Kameas never receives them. Turn off: `kenaz memory disable`. Clear: `kenaz memory purge`.", path)
}

// KeptLine is printed by `kenaz logout` and `kenaz setup --undo`, which
// leave memory in place.
func KeptLine(path string) string {
	return fmt.Sprintf("Your Kenaz memory (%s) was kept. Delete it with `kenaz memory purge`.", path)
}

// DisclosureMarker is the state file that records the disclosure was shown.
const DisclosureMarker = "memory-disclosure-shown"

// ClaimDisclosure returns true exactly once per marker file: the first
// caller creates it and should show the disclosure. Any error (including
// an unwritable state directory) returns false rather than repeat it.
func ClaimDisclosure(markerPath string, now time.Time) bool {
	f, err := os.OpenFile(markerPath, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return false
	}
	defer f.Close()
	_, _ = f.WriteString(now.UTC().Format(time.RFC3339) + "\n")
	return true
}

// Index budget for the session-start block.
const (
	IndexMaxEntries = 10
	IndexMaxBytes   = 1536
	indexTimeout    = time.Second
)

const (
	indexHeader = "Kenaz memory index for this project (notes stored on this device; titles only):"
	indexFooter = "Use memory_search or memory_get for details, and memory_save to record durable learnings (never secrets or personal data)."
)

// SessionIndex returns the session-start "Memory index" block for project p:
// the most recent titles and ids, at most IndexMaxBytes, never bodies. It
// opens the store read-only with a short timeout and returns "" on any
// error or when the project has no notes.
func SessionIndex(ctx context.Context, path string, p Project) string {
	ctx, cancel := context.WithTimeout(ctx, indexTimeout)
	defer cancel()
	s, err := OpenReadOnly(path, 250*time.Millisecond)
	if err != nil {
		return ""
	}
	defer s.Close()
	entries, err := s.Recent(ctx, ProjectScope(p.Key), IndexMaxEntries)
	if err != nil || len(entries) == 0 {
		return ""
	}
	return FormatIndex(entries)
}

// FormatIndex renders entries as the index block within IndexMaxBytes,
// dropping the oldest lines that do not fit.
func FormatIndex(entries []Entry) string {
	var b strings.Builder
	b.WriteString(indexHeader)
	budget := IndexMaxBytes - len(indexHeader) - len(indexFooter) - 2
	n := 0
	for _, e := range entries {
		line := fmt.Sprintf("\n- %s [%s, %s]", Clip(e.Title, 120), e.ID, e.CreatedAt.Format("2006-01-02"))
		if len(line) > budget {
			break
		}
		budget -= len(line)
		b.WriteString(line)
		n++
	}
	if n == 0 {
		return ""
	}
	b.WriteString("\n")
	b.WriteString(indexFooter)
	return b.String()
}

// IsUserError reports errors whose message is meant for the user as is.
func IsUserError(err error) bool {
	return errors.Is(err, ErrInvalidInput) || errors.Is(err, ErrFull) || errors.Is(err, ErrLooksSecret) ||
		errors.Is(err, ErrNotFound) || errors.Is(err, ErrEmptyQuery)
}
