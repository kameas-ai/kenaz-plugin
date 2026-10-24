package setup

import (
	"errors"
	"os"

	"github.com/kameas-ai/kenaz-plugin/internal/features"
)

// Host setup states reported by HostStates.
const (
	HostConfigured    = "configured"     // the file still holds what setup wrote
	HostModified      = "modified"       // setup configured it; the file changed since
	HostNotConfigured = "not_configured" // setup has not configured this host
	HostUnknown       = "unknown"        // the setup record or file could not be read
)

// HostState is one host's setup state, for status and doctor. It carries
// no paths or file content.
type HostState struct {
	Host  string
	State string
}

// HostStates reads the setup record in stateDir and compares each
// configured host file with what setup last wrote. It never writes.
func HostStates(stateDir string) []HostState {
	st, err := loadState(stateDir)
	out := []HostState{{Host: "claude-code"}, {Host: "codex"}}
	if features.OpenCode {
		out = append(out, HostState{Host: "opencode"})
	}
	if err != nil {
		for i := range out {
			out[i].State = HostUnknown
		}
		return out
	}
	out[0].State = fileHostState(st.Claude)
	out[1].State = fileHostState(st.Codex)
	if features.OpenCode {
		out[2].State = fileHostState(st.OpenCode)
	}
	return out
}

func fileHostState(fs *FileState) string {
	if fs == nil || fs.Path == "" {
		return HostNotConfigured
	}
	b, err := os.ReadFile(fs.Path)
	if errors.Is(err, os.ErrNotExist) {
		return HostModified // setup wrote it; it is gone now
	}
	if err != nil {
		return HostUnknown
	}
	if sha(b) == fs.WrittenSHA256 {
		return HostConfigured
	}
	return HostModified
}
