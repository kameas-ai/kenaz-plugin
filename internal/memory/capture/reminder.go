package capture

import (
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// reminderEvery sets how often the session-agent writer's reminder is shown:
// on the first prompt of a session and then on every Nth. Each costs about 60
// input tokens, on the member's own plan.
const reminderEvery = 4

// reminderStale is when a session's counter counts as abandoned.
const reminderStale = 7 * 24 * time.Hour

// sessionAgentReminder asks for an inline save, with no fuss. It must stay
// short: it is paid for on every showing.
const sessionAgentReminder = "Kenaz memory: if this work produces something a later session would need (a decision and why, a root cause and fix, a new capability, a non-obvious fact), call memory_save with a short title and body as you go, tagged type:decision, type:bugfix, type:feature, type:discovery or type:change. Don't announce it. Never save credentials or personal data."

func (p Paths) reminders() string { return filepath.Join(p.Dir, "reminders") }

// nextReminder counts a prompt and returns the reminder when it is due. first
// reports the session's first prompt. A file per session keeps concurrent
// sessions from sharing a lock; a lost count only moves a reminder by one.
func nextReminder(p Paths, session string, now time.Time) (text string, first bool) {
	if err := os.MkdirAll(p.reminders(), 0o700); err != nil {
		return "", false
	}
	path := filepath.Join(p.reminders(), session)
	n := 0
	if b, err := os.ReadFile(path); err == nil {
		n, _ = strconv.Atoi(strings.TrimSpace(string(b)))
	}
	first = n == 0
	if first {
		pruneReminders(p, now)
	}
	if err := os.WriteFile(path, []byte(strconv.Itoa(n+1)), 0o600); err != nil {
		return "", first
	}
	if n%reminderEvery == 0 {
		return sessionAgentReminder, first
	}
	return "", first
}

func pruneReminders(p Paths, now time.Time) {
	ents, err := os.ReadDir(p.reminders())
	if err != nil {
		return
	}
	for _, e := range ents {
		if info, err := e.Info(); err == nil && now.Sub(info.ModTime()) > reminderStale {
			os.Remove(filepath.Join(p.reminders(), e.Name()))
		}
	}
}
