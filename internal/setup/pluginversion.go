package setup

import (
	"fmt"
	"strings"

	"github.com/kameas-ai/kenaz-plugin/internal/variant"
)

// StalePluginFiles says, in one sentence, which Claude Code or Codex plugin
// registrations in stateDir's setup state still use the plugin files of
// another version than version, this kenaz's ("" when none do). The files
// are the copy setup made at the last `kenaz init`; an upgrade replaces
// only the binary, so what the files carry (for example Codex's list of
// tools that ask before writing) stays as it was until init runs again.
// It only reads the setup state.
func StalePluginFiles(stateDir, version string) string {
	state, err := loadState(stateDir)
	if err != nil {
		return ""
	}
	cur := strings.TrimPrefix(version, "v")
	var hosts, from []string
	same := true
	for _, h := range []harness{harnessClaude, harnessCodex} {
		fs := state.get(h)
		if fs == nil || fs.Bundle == nil {
			continue
		}
		v := strings.TrimPrefix(fs.Bundle.Version, "v")
		if v == "" || v == cur {
			continue
		}
		if len(from) > 0 && from[0] != v {
			same = false
		}
		hosts, from = append(hosts, h.label()), append(from, v)
	}
	if len(hosts) == 0 {
		return ""
	}
	bin := variant.Current.Binary
	where := hosts[0] + " are from " + bin + " " + from[0]
	if len(hosts) == 2 {
		if same {
			where = hosts[0] + " and " + hosts[1] + " are from " + bin + " " + from[0]
		} else {
			where += fmt.Sprintf(", those in %s from %s %s,", hosts[1], bin, from[1])
		}
	}
	return fmt.Sprintf("the plugin files registered in %[1]s and this %[2]s is %[3]s; run `%[4]s` to refresh them", where, bin, cur, Options{StateDir: stateDir}.initCmd())
}
