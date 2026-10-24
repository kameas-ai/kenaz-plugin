// Package config holds env selection, the config file and the
// config/state directory layout.
package config

import (
	"bufio"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"

	"github.com/kameas-ai/kenaz-plugin/fleetapi"
)

// Environment variables that select the Fleet env. KENAZ_ENV is what
// the plugin's .mcp.json sets from the plugin's fleet_env user option.
// Hook commands get no .mcp.json env block: Claude Code exports plugin
// user options to every plugin subprocess as CLAUDE_PLUGIN_OPTION_<KEY>,
// so hooks (SessionStart banner, approval links, SessionEnd) read the same
// fleet_env through it and agree with the MCP server on the environment.
// FLEET_PLUGIN_ENV is the original name. The first non-empty,
// non-placeholder value wins.
var EnvVars = []string{"KENAZ_ENV", "CLAUDE_PLUGIN_OPTION_FLEET_ENV", "FLEET_PLUGIN_ENV"}

// Source records where the resolved env came from (shown by doctor/status).
type Source string

const (
	SourceFlag    Source = "flag"
	SourceEnvVar  Source = "env"
	SourceFile    Source = "config"
	SourceDefault Source = "default"
)

// Paths is the on-disk layout. ConfigDir holds config.toml; StateRoot holds
// one subdirectory per Fleet env.
type Paths struct {
	ConfigDir string
	StateRoot string
	// LegacyStateRoot is where an earlier release kept StateRoot (Windows:
	// %AppData%\kameas\kenaz\state); "" when it never moved.
	LegacyStateRoot string
}

// ConfigFile returns the config.toml path.
func (p Paths) ConfigFile() string { return filepath.Join(p.ConfigDir, "config.toml") }

// State returns the per-env state directory.
func (p Paths) State(env fleetapi.Env) string { return filepath.Join(p.StateRoot, string(env)) }

// DefaultPaths returns the platform layout:
//
//	macOS:   ~/Library/Application Support/kameas/kenaz/{,state/}
//	Linux:   $XDG_CONFIG_HOME/kameas/kenaz, $XDG_STATE_HOME/kameas/kenaz
//	Windows: %AppData%\kameas\kenaz (config.toml), %LocalAppData%\kameas\kenaz\state
//
// On Windows the state (queued uploads, sign-in files, memory, the relay's
// daemon.json) is machine-local, so it lives in %LocalAppData% and never
// roams with a roaming profile; config.toml (the env choice) may roam.
// Releases up to 0.4.x kept the state in %AppData%; DefaultPaths moves it
// on first use (see MigrateLegacyState).
//
// KENAZ_HOME overrides both roots (tests, in-VM mode).
func DefaultPaths() (Paths, error) {
	p, err := pathsFor(runtime.GOOS, os.Getenv, os.UserHomeDir, os.UserConfigDir)
	if err != nil {
		return p, err
	}
	return p.MigrateLegacyState(), nil
}

// MigrateLegacyState moves LegacyStateRoot to StateRoot when only the
// legacy one exists, and returns the paths to use:
//
//   - no legacy state: p unchanged;
//   - legacy state only: it is renamed to StateRoot (one atomic rename on
//     the same volume). If the rename fails because another process moved
//     it first, StateRoot is used; if it fails while the legacy state is
//     still there (a running relay holds files open), this process keeps
//     using the legacy root and the move is retried on the next run;
//   - both exist: StateRoot wins and the legacy root is left untouched.
func (p Paths) MigrateLegacyState() Paths {
	if p.LegacyStateRoot == "" || p.LegacyStateRoot == p.StateRoot {
		return p
	}
	if !isDir(p.LegacyStateRoot) || exists(p.StateRoot) {
		return p
	}
	if err := os.MkdirAll(filepath.Dir(p.StateRoot), 0o700); err == nil {
		if err := os.Rename(p.LegacyStateRoot, p.StateRoot); err == nil {
			return p
		}
	}
	// The rename failed. If another kenaz process (kenaz mcp and the
	// SessionStart hook start together after an upgrade) moved it first,
	// StateRoot exists now and is the one to use. Only when the legacy
	// state is still there (its files are open somewhere) does this process
	// keep using it, and the move is retried next time.
	if exists(p.StateRoot) || !isDir(p.LegacyStateRoot) {
		return p
	}
	p.StateRoot = p.LegacyStateRoot
	return p
}

func isDir(p string) bool {
	fi, err := os.Stat(p)
	return err == nil && fi.IsDir()
}

func exists(p string) bool {
	_, err := os.Lstat(p)
	return err == nil
}

func pathsFor(goos string, getenv func(string) string, home, userConfig func() (string, error)) (Paths, error) {
	if h := getenv("KENAZ_HOME"); h != "" {
		return Paths{ConfigDir: filepath.Join(h, "config"), StateRoot: filepath.Join(h, "state")}, nil
	}
	const sub = "kameas/kenaz"
	switch goos {
	case "linux":
		hd, err := home()
		if err != nil {
			return Paths{}, err
		}
		cfg := getenv("XDG_CONFIG_HOME")
		if cfg == "" {
			cfg = filepath.Join(hd, ".config")
		}
		st := getenv("XDG_STATE_HOME")
		if st == "" {
			st = filepath.Join(hd, ".local", "state")
		}
		return Paths{ConfigDir: filepath.Join(cfg, sub), StateRoot: filepath.Join(st, sub)}, nil
	case "windows":
		base, err := userConfig() // %AppData%
		if err != nil {
			return Paths{}, err
		}
		local := getenv("LocalAppData")
		if local == "" {
			hd, err := home()
			if err != nil {
				return Paths{}, err
			}
			local = filepath.Join(hd, "AppData", "Local")
		}
		root := filepath.Join(base, sub)
		return Paths{ConfigDir: root, StateRoot: filepath.Join(local, sub, "state"), LegacyStateRoot: filepath.Join(root, "state")}, nil
	default:
		base, err := userConfig()
		if err != nil {
			return Paths{}, err
		}
		root := filepath.Join(base, sub)
		return Paths{ConfigDir: root, StateRoot: filepath.Join(root, "state")}, nil
	}
}

// EnsureState creates the per-env state directory with mode 0700.
func (p Paths) EnsureState(env fleetapi.Env) (string, error) {
	dir := p.State(env)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", err
	}
	return dir, nil
}

// File is the parsed config.toml. Only flat `key = "value"` string pairs
// are supported; that is all the plugin writes or reads.
type File struct {
	Values map[string]string
}

// Get returns a config value.
func (f File) Get(k string) string { return f.Values[k] }

// LoadFile reads path. A missing file is an empty config, not an error.
func LoadFile(path string) (File, error) {
	f := File{Values: map[string]string{}}
	fh, err := os.Open(path)
	if errors.Is(err, os.ErrNotExist) {
		return f, nil
	}
	if err != nil {
		return f, err
	}
	defer fh.Close()
	sc := bufio.NewScanner(fh)
	for n := 1; sc.Scan(); n++ {
		line := strings.TrimSpace(sc.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		k, v, ok := strings.Cut(line, "=")
		if !ok {
			return f, fmt.Errorf("%s:%d: expected key = \"value\"", path, n)
		}
		k = strings.TrimSpace(k)
		v = strings.TrimSpace(v)
		if i := strings.Index(v, " #"); i >= 0 && !strings.HasPrefix(v, "\"") {
			v = strings.TrimSpace(v[:i])
		}
		if len(v) >= 2 && v[0] == '"' {
			end := strings.LastIndexByte(v, '"')
			if end <= 0 {
				return f, fmt.Errorf("%s:%d: unterminated string", path, n)
			}
			v = v[1:end]
		}
		f.Values[k] = v
	}
	return f, sc.Err()
}

// placeholder reports values a harness left unexpanded, e.g. Codex passes
// "${user_config.env}" through literally from a Claude-format .mcp.json.
func placeholder(v string) bool { return strings.HasPrefix(strings.TrimSpace(v), "${") }

// ResolveEnv applies env precedence: --env flag > env var > config file >
// prod. Empty and unexpanded placeholder values are skipped at every level.
// An explicit but unknown value is an error, never a silent fallback.
func ResolveEnv(flag string, getenv func(string) string, file File) (fleetapi.Env, Source, error) {
	try := func(v string, src Source) (fleetapi.Env, Source, bool, error) {
		v = strings.TrimSpace(v)
		if v == "" || placeholder(v) {
			return "", "", false, nil
		}
		e := fleetapi.Env(strings.ToLower(v))
		if !e.Valid() {
			return "", "", true, fmt.Errorf("unknown Fleet env %q from %s (want prod|stage|dev)", v, src)
		}
		return e, src, true, nil
	}
	if e, s, ok, err := try(flag, SourceFlag); ok || err != nil {
		return e, s, err
	}
	for _, name := range EnvVars {
		if e, s, ok, err := try(getenv(name), SourceEnvVar); ok || err != nil {
			return e, s, err
		}
	}
	if e, s, ok, err := try(file.Get("env"), SourceFile); ok || err != nil {
		return e, s, err
	}
	return fleetapi.EnvProd, SourceDefault, nil
}

// SetValue writes key = "value" into the config file at path, replacing an
// existing line for key and keeping every other line (comments included).
// The directory is created 0700 and the file written 0600.
func SetValue(path, key, value string) error {
	if strings.ContainsAny(key, "=\n\"") || strings.ContainsAny(value, "\n\"") {
		return fmt.Errorf("config: invalid key or value for %q", key)
	}
	b, err := os.ReadFile(path)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	line := fmt.Sprintf("%s = %q", key, value)
	var out []string
	replaced := false
	for _, l := range strings.Split(strings.TrimRight(string(b), "\n"), "\n") {
		if k, _, ok := strings.Cut(strings.TrimSpace(l), "="); ok && strings.TrimSpace(k) == key && !strings.HasPrefix(strings.TrimSpace(l), "#") {
			if !replaced {
				out = append(out, line)
				replaced = true
			}
			continue
		}
		if l != "" || len(out) > 0 {
			out = append(out, l)
		}
	}
	if !replaced {
		out = append(out, line)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, []byte(strings.Join(out, "\n")+"\n"), 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}
