// Package installed is the credential-free installed-tier conformance
// journey: it builds this checkout's unofficial kenaz, sets it up for one
// host in throwaway directories, and checks what can be checked with no
// model credentials and no Kameas Fleet sign-in.
//
// It never signs in to anything, never makes a model call and never
// contacts Kameas Fleet (the unofficial build cannot). Everything it writes
// is under one temporary directory; it refuses to start otherwise.
package installed

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"github.com/kameas-ai/kenaz-plugin/internal/credstore"
)

// sandbox is the throwaway machine state of one journey.
type sandbox struct {
	root      string // the temporary directory (symlinks resolved)
	home      string
	kenazHome string
	claudeDir string
	codexDir  string
	work      string // working directory of every child
	tmp       string // children's TMPDIR
	bundle    string // the assembled local bundle
	exe       string // bundle/bin/kenaz[.exe]
	env       []string
	realHome  string
}

// secretLike reports environment names that could authenticate a host, a
// model provider, a cloud or Fleet, or that steer kenaz or a host at real
// state. They never reach a child process.
func secretLike(name string) bool {
	n := strings.ToUpper(name)
	for _, p := range []string{"ANTHROPIC_", "OPENAI_", "CLAUDE", "CODEX_", "KENAZ_", "FLEET_", "AWS_", "AZURE_", "GOOGLE_", "GCP_", "GH_", "GITHUB_", "ACTIONS_", "NPM_", "OPENROUTER_", "GEMINI_"} {
		if strings.HasPrefix(n, p) {
			return true
		}
	}
	for _, s := range []string{"API_KEY", "APIKEY", "TOKEN", "SECRET", "PASSWORD", "CREDENTIAL"} {
		if strings.Contains(n, s) {
			return true
		}
	}
	return false
}

// newSandbox creates the temporary directory and the environment children
// run with. Nothing in it points outside the directory.
func newSandbox() (*sandbox, error) {
	realHome, err := os.UserHomeDir()
	if err != nil {
		return nil, err
	}
	root, err := os.MkdirTemp("", "kenaz-conformance-")
	if err != nil {
		return nil, err
	}
	if r, err := filepath.EvalSymlinks(root); err == nil {
		root = r
	}
	home := filepath.Join(root, "home")
	s := &sandbox{
		root: root, home: home, realHome: realHome,
		kenazHome: filepath.Join(home, ".kenaz"),
		claudeDir: filepath.Join(home, ".claude"),
		codexDir:  filepath.Join(home, ".codex"),
		work:      filepath.Join(root, "work"),
		tmp:       filepath.Join(root, "tmp"),
		bundle:    filepath.Join(root, "bundle"),
	}
	s.exe = filepath.Join(s.bundle, "bin", "kenaz")
	if runtime.GOOS == "windows" {
		s.exe += ".exe"
	}
	over := map[string]string{
		"HOME": home, "USERPROFILE": home,
		"KENAZ_HOME": s.kenazHome, "CLAUDE_CONFIG_DIR": s.claudeDir, "CODEX_HOME": s.codexDir,
		"XDG_CONFIG_HOME": filepath.Join(home, ".config"), "XDG_STATE_HOME": filepath.Join(home, ".local", "state"),
		"XDG_DATA_HOME": filepath.Join(home, ".local", "share"), "XDG_CACHE_HOME": filepath.Join(home, ".cache"),
		"APPDATA": filepath.Join(home, "AppData", "Roaming"), "LOCALAPPDATA": filepath.Join(home, "AppData", "Local"),
		"TMPDIR": s.tmp, "TEMP": s.tmp, "TMP": s.tmp,
	}
	for _, kv := range os.Environ() {
		k, _, _ := strings.Cut(kv, "=")
		if _, replaced := over[strings.ToUpper(k)]; replaced || secretLike(k) || k == "" {
			continue
		}
		s.env = append(s.env, kv)
	}
	for k, v := range over {
		s.env = append(s.env, k+"="+v)
	}
	// Host CLIs: no self-update and no background traffic. Set after the
	// filter, which drops every inherited CLAUDE* variable.
	s.env = append(s.env, "DISABLE_AUTOUPDATER=1", "CLAUDE_CODE_DISABLE_NONESSENTIAL_TRAFFIC=1", "DO_NOT_TRACK=1")
	if err := s.assertContained(over); err != nil {
		_ = os.RemoveAll(root)
		return nil, err
	}
	for _, d := range []string{home, s.claudeDir, s.codexDir, s.work, s.tmp, filepath.Join(s.bundle, "bin")} {
		if err := os.MkdirAll(d, 0o700); err != nil {
			_ = os.RemoveAll(root)
			return nil, err
		}
	}
	return s, nil
}

// assertContained aborts the journey unless every path it (or a child) can
// be told to write is inside the temporary directory, and that directory is
// not this machine's real home or host state.
func (s *sandbox) assertContained(over map[string]string) error {
	root := filepath.Clean(s.root)
	if root == "" || root == string(filepath.Separator) || !filepath.IsAbs(root) {
		return fmt.Errorf("refusing to run: temporary directory %q is not a usable absolute path", root)
	}
	real := filepath.Clean(s.realHome)
	if r, err := filepath.EvalSymlinks(real); err == nil {
		real = r
	}
	if within(real, root) {
		return errors.New("refusing to run: the user's home directory is inside the journey's temporary directory")
	}
	for _, d := range []string{".kenaz", ".claude", ".codex"} {
		if within(root, filepath.Join(real, d)) {
			return fmt.Errorf("refusing to run: the temporary directory is inside the real ~/%s", d)
		}
	}
	paths := map[string]string{"home": s.home, "kenaz home": s.kenazHome, "claude config": s.claudeDir, "codex home": s.codexDir, "work": s.work, "tmp": s.tmp, "bundle": s.bundle, "kenaz binary": s.exe}
	for k, v := range over {
		paths["$"+k] = v
	}
	for name, p := range paths {
		if !filepath.IsAbs(p) || !within(p, root) || filepath.Clean(p) == root {
			return fmt.Errorf("refusing to run: %s (%s) is not inside the journey's temporary directory", name, p)
		}
		for _, d := range []string{".kenaz", ".claude", ".codex"} {
			if within(p, filepath.Join(real, d)) {
				return fmt.Errorf("refusing to run: %s would be inside the real ~/%s", name, d)
			}
		}
	}
	// The environment handed to children must agree with the paths above.
	for _, kv := range s.env {
		k, v, _ := strings.Cut(kv, "=")
		want, own := over[k]
		if own && v != want {
			return fmt.Errorf("refusing to run: $%s is not the sandbox value", k)
		}
		if secretLike(k) && !own && k != "CLAUDE_CODE_DISABLE_NONESSENTIAL_TRAFFIC" {
			return fmt.Errorf("refusing to run: $%s would reach child processes", k)
		}
	}
	return nil
}

// within reports whether p is dir or inside it.
func within(p, dir string) bool {
	rel, err := filepath.Rel(filepath.Clean(dir), filepath.Clean(p))
	return err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)) && !filepath.IsAbs(rel)
}

// samePath compares two paths the way the OS does.
func samePath(a, b string) bool {
	norm := func(p string) string {
		if r, err := filepath.EvalSymlinks(p); err == nil {
			p = r
		}
		return filepath.Clean(p)
	}
	a, b = norm(a), norm(b)
	if runtime.GOOS == "windows" {
		return strings.EqualFold(a, b)
	}
	return a == b
}

// run executes name in the sandbox and returns its combined output and
// exit code (-1 when it could not run).
func (s *sandbox) run(ctx context.Context, timeout time.Duration, name string, args ...string) (string, int) {
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.Dir, cmd.Env = s.work, s.env
	cmd.WaitDelay = 5 * time.Second
	out, err := cmd.CombinedOutput()
	var ee *exec.ExitError
	switch {
	case err == nil:
		return string(out), 0
	case errors.As(err, &ee):
		return string(out), ee.ExitCode()
	default:
		return string(out) + err.Error(), -1
	}
}

// kenaz runs the built binary in the sandbox. It refuses until the file
// credential store is recorded there: without the marker kenaz would probe
// the OS keychain (on a Linux desktop, the Secret Service) on its first run.
func (s *sandbox) kenaz(ctx context.Context, args ...string) (string, int) {
	if err := s.fileStoreRecorded(); err != nil {
		return err.Error(), -1
	}
	return s.run(ctx, 90*time.Second, s.exe, args...)
}

// credentialsDir is where kenaz keeps its credential store marker.
func (s *sandbox) credentialsDir() string {
	return filepath.Join(s.kenazHome, "plugin", "credentials")
}

// useFileCredentialStore records the file credential store inside the
// sandbox. It must run before kenaz does (see kenaz).
func (s *sandbox) useFileCredentialStore() error {
	dir := s.credentialsDir()
	if !within(dir, s.root) {
		return errors.New("refusing to run: the credentials directory is outside the journey's temporary directory")
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	return credstore.WriteMarker(dir, credstore.Files)
}

// fileStoreRecorded is nil once the sandbox's marker says files.
func (s *sandbox) fileStoreRecorded() error {
	b, err := os.ReadFile(filepath.Join(s.credentialsDir(), credstore.MarkerName))
	if err != nil || strings.TrimSpace(string(b)) != string(credstore.Files) {
		return errors.New("refusing to run kenaz: the sandbox's credential store is not recorded as files yet")
	}
	return nil
}

// scrub removes the sandbox's and the user's own paths from text that goes
// into evidence, and bounds it to one short line.
func (s *sandbox) scrub(text string) string {
	for _, p := range []string{s.root, filepath.ToSlash(s.root)} {
		text = strings.ReplaceAll(text, p, "$TMP")
	}
	for _, p := range []string{s.realHome, filepath.ToSlash(s.realHome)} {
		if p != "" && p != "/" {
			text = strings.ReplaceAll(text, p, "~")
		}
	}
	text = strings.Join(strings.Fields(text), " ")
	if len(text) > 600 {
		text = text[:600] + "…"
	}
	return text
}

func (s *sandbox) getenv(k string) string {
	for i := len(s.env) - 1; i >= 0; i-- {
		if name, v, _ := strings.Cut(s.env[i], "="); name == k {
			return v
		}
	}
	return ""
}

func (s *sandbox) close() { _ = os.RemoveAll(s.root) }
