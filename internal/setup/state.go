package setup

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
)

// State records exactly what setup changed, so --undo can restore it.
// It lives in the relay state directory with mode
// 0600.
type State struct {
	Version int        `json:"version"`
	Claude  *FileState `json:"claude,omitempty"`
	Codex   *FileState `json:"codex,omitempty"`
}

// FileState is one harness file's setup record.
type FileState struct {
	Path string `json:"path"`
	// OriginalAbsent is true when setup created the file.
	OriginalAbsent bool `json:"original_absent"`
	// Snapshot holds the file's bytes from before the first setup run,
	// used for a byte-identical undo when nothing else changed.
	Snapshot string `json:"snapshot,omitempty"`
	// WrittenSHA256 is the hash of the bytes setup last wrote.
	WrittenSHA256 string `json:"written_sha256"`
	// CreatedContainer is true when setup created the Claude "env" object
	// or the Codex [otel] table.
	CreatedContainer bool                `json:"created_container"`
	Keys             map[string]KeyState `json:"keys"`
}

// KeyState is one owned key: its value before setup first touched it and
// the value setup last wrote. Values are raw JSON (Claude) or raw TOML
// lines (Codex).
type KeyState struct {
	PriorPresent bool   `json:"prior_present"`
	Prior        string `json:"prior,omitempty"`
	Written      string `json:"written"`
}

const stateFile = "setup-state.json"

func loadState(dir string) (*State, error) {
	b, err := os.ReadFile(filepath.Join(dir, stateFile))
	if errors.Is(err, os.ErrNotExist) {
		return &State{Version: 1}, nil
	}
	if err != nil {
		return nil, err
	}
	var s State
	if err := json.Unmarshal(b, &s); err != nil {
		return nil, err
	}
	return &s, nil
}

func saveState(dir string, s *State) error {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	b, err := json.MarshalIndent(s, "", "  ")
	if err != nil {
		return err
	}
	path := filepath.Join(dir, stateFile)
	if _, err := os.Stat(path); errors.Is(err, os.ErrNotExist) {
		if err := os.WriteFile(path, nil, 0o600); err != nil {
			return err
		}
	}
	return writeAtomic(path, append(b, '\n'))
}

func (s *State) get(h harness) *FileState {
	if h == harnessClaude {
		return s.Claude
	}
	return s.Codex
}

func (s *State) put(h harness, fs *FileState) {
	if h == harnessClaude {
		s.Claude = fs
	} else {
		s.Codex = fs
	}
}

func (fs *FileState) clone() *FileState {
	if fs == nil {
		return nil
	}
	c := *fs
	c.Keys = make(map[string]KeyState, len(fs.Keys))
	for k, v := range fs.Keys {
		c.Keys[k] = v
	}
	return &c
}

// record notes key's prior value the first time setup takes ownership,
// and its newly written value every time.
func (fs *FileState) record(key string, priorPresent bool, prior, written string) {
	if fs.Keys == nil {
		fs.Keys = map[string]KeyState{}
	}
	ks, owned := fs.Keys[key]
	if !owned {
		ks = KeyState{PriorPresent: priorPresent, Prior: prior}
	}
	ks.Written = written
	fs.Keys[key] = ks
}

func (fs *FileState) owns(key string) bool {
	if fs == nil {
		return false
	}
	_, ok := fs.Keys[key]
	return ok
}
