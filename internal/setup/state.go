package setup

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/kameas-ai/kenaz-plugin/internal/variant"
)

// State records exactly what setup changed, so --undo can restore it.
// It lives in the relay state directory with mode
// 0600.
type State struct {
	Version int `json:"version"`
	// Variant is the build that made this setup ("" in files written
	// before builds recorded it, which were all kenaz). Every build keeps a
	// Fleet environment's setup in the same state directory, so another
	// build's setup there is refused (see loadState).
	Variant string     `json:"variant,omitempty"`
	Claude  *FileState `json:"claude,omitempty"`
	Codex   *FileState `json:"codex,omitempty"`
	// OpenCode is keyed "opencode" in setup-state.json.
	OpenCode *FileState `json:"opencode,omitempty"`
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
	// Containers lists the top-level OpenCode entries ("mcp", "plugin")
	// that setup created and may remove again when they end up empty.
	Containers []string `json:"containers,omitempty"`
	// Bundle is the plugin registration setup made from a release bundle
	// (Claude Code, Codex), undone with the host CLI.
	Bundle *BundleReg `json:"bundle,omitempty"`
	// Adopted records that setup used the public GitHub marketplace the
	// user had already registered, instead of a bundle (Claude Code, Codex).
	Adopted *Adopted `json:"adopted,omitempty"`
}

// Adopted is an adopted GitHub marketplace registration.
type Adopted struct {
	Source string `json:"source"`
	// InstalledPlugin is true when setup installed (enabled) kenaz@kenaz
	// from it, so --undo uninstalls the plugin. The marketplace itself is
	// the user's and is never removed.
	InstalledPlugin bool `json:"installed_plugin,omitempty"`
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
	if v := s.build(); v != variant.Current.Name && (s.Claude != nil || s.Codex != nil || s.OpenCode != nil) {
		return nil, &OtherBuildError{Variant: v}
	}
	for _, fs := range []*FileState{s.Claude, s.Codex, s.OpenCode} {
		if fs == nil {
			continue
		}
		fs.Snapshot = rebase(dir, fs.Snapshot)
		if fs.Bundle != nil {
			if r := rebase(dir, fs.Bundle.Rendered); r != fs.Bundle.Rendered {
				fs.Bundle.formerRendered, fs.Bundle.Rendered = fs.Bundle.Rendered, r
			}
		}
	}
	return &s, nil
}

// rebase maps a path setup recorded under an earlier location of the state
// directory (it moved, e.g. the Windows state migration) onto dir: a path
// that no longer exists and has a "setup" component becomes the same
// tail under dir when that exists. Anything else is returned unchanged.
//
// A path that still exists through a link the move left at the old
// location (it resolves inside dir but is not spelled under it) is mapped
// onto dir the same way, so pruning and re-registration see the plugin
// copy setup actually uses.
func rebase(dir, p string) string {
	if p == "" {
		return p
	}
	if fileExists(p) {
		if within(dir, p) {
			return p
		}
		rp, err1 := realPath(p)
		rd, err2 := realPath(dir)
		if err1 == nil && err2 == nil && within(rd, rp) {
			if rel, err := filepath.Rel(rd, rp); err == nil {
				return filepath.Join(dir, rel)
			}
		}
		return p
	}
	sep := string(filepath.Separator)
	i := strings.LastIndex(p, sep+"setup"+sep)
	if i < 0 {
		return p
	}
	cand := filepath.Join(dir, p[i+1:])
	if cand != p && fileExists(cand) {
		return cand
	}
	return p
}

func (s *State) build() string {
	if s.Variant == "" {
		return variant.Kenaz.Name
	}
	return s.Variant
}

// recordedVariant is what files record as their build: "" for kenaz, so
// the public build's files read as they always have.
func recordedVariant() string {
	if variant.Current.Name == variant.Kenaz.Name {
		return ""
	}
	return variant.Current.Name
}

// OtherBuildError means another build set up the apps for this Fleet
// environment: one build at a time can (they share its state directory).
type OtherBuildError struct{ Variant string }

func (e *OtherBuildError) Error() string {
	return fmt.Sprintf("%[1]s set up the apps for this Fleet environment on this device, and only one build at a time can. Use %[1]s for it, or run `%[1]s setup --undo` for each app first", e.Variant)
}

func saveState(dir string, s *State) error {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	s.Variant = recordedVariant()
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
	switch h {
	case harnessClaude:
		return s.Claude
	case harnessOpenCode:
		return s.OpenCode
	}
	return s.Codex
}

func (s *State) put(h harness, fs *FileState) {
	switch h {
	case harnessClaude:
		s.Claude = fs
	case harnessOpenCode:
		s.OpenCode = fs
	default:
		s.Codex = fs
	}
}

func (fs *FileState) clone() *FileState {
	if fs == nil {
		return nil
	}
	c := *fs
	c.Containers = append([]string(nil), fs.Containers...)
	if fs.Bundle != nil {
		b := *fs.Bundle
		c.Bundle = &b
	}
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
