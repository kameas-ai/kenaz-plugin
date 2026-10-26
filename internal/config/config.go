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
	"github.com/kameas-ai/kenaz-plugin/internal/variant"
)

// EnvVars are the environment variables that select the Fleet env (see
// variant.Identity.EnvVars). Hooks (SessionStart banner, approval links,
// SessionEnd) and the MCP server read the same plugin option through them
// and agree on the environment. The first non-empty, non-placeholder
// value wins.
var EnvVars = variant.Shared.EnvVars

// Source records where the resolved env came from (shown by doctor/status).
type Source string

const (
	SourceFlag    Source = "flag"
	SourceEnvVar  Source = "env"
	SourceFile    Source = "config"
	SourceDefault Source = "default"
)

// Paths is the on-disk layout. ConfigDir holds config.toml; StateRoot holds
// one subdirectory per Fleet env (and the env-independent memory and
// credentials directories). Both are the plugin's root, ~/.kenaz/plugin.
type Paths struct {
	ConfigDir string
	StateRoot string
	// Legacy is where earlier releases kept these files (see Migrate).
	Legacy Legacy
}

// Legacy is an earlier release's layout.
type Legacy struct {
	// ConfigDir held config.toml.
	ConfigDir string
	// StateRoots held the state, in order of preference (on Windows the
	// %LocalAppData% root, then the %AppData% one releases up to 0.4.x
	// used).
	StateRoots []string
}

// ConfigFile returns the config.toml path.
func (p Paths) ConfigFile() string { return filepath.Join(p.ConfigDir, "config.toml") }

// State returns the per-env state directory.
func (p Paths) State(env fleetapi.Env) string { return filepath.Join(p.StateRoot, string(env)) }

// DefaultPaths returns the layout, the same on every platform and for every
// build (the Kenaz harness uses ~/.kenaz/harness/<env> the same way):
//
//	~/.kenaz/plugin/config.toml      the env choice and settings
//	~/.kenaz/plugin/<env>/           one Fleet environment's state
//	~/.kenaz/plugin/{memory,credentials}/
//
// ~ is the user's home directory (%USERPROFILE% on Windows, which never
// roams). KENAZ_HOME replaces ~/.kenaz, as it does for the archive
// installer ($KENAZ_HOME/install).
//
// Files an earlier release left in its own layout (Legacy) keep being used
// where they are until `kenaz migrate` or `kenaz init` moves them (see
// Current and Migrate).
func DefaultPaths() (Paths, error) {
	p, err := pathsFor(runtime.GOOS, os.Getenv, os.UserHomeDir, os.UserConfigDir)
	if err != nil {
		return p, err
	}
	return p.Current(), nil
}

// Current is the layout every command uses. Each component is resolved on
// its own, so a move interrupted between components never hides one:
//
//   - state: the earlier release's state root while it holds files and
//     the root holds no state yet; otherwise the root;
//   - config.toml: the earlier release's while it is a file and the root
//     has none; otherwise the root's.
//
// When both the root and an earlier release's state hold files (the root
// was created by a newer build first), the root wins and the old files are
// not used; Migrate reports where they are.
func (p Paths) Current() Paths {
	out := p
	if r := p.Legacy.stateRoot(); r != "" && !p.rootHasState() {
		out.StateRoot = r
	}
	if p.Legacy.configPresent() && !exists(p.ConfigFile()) {
		out.ConfigDir = p.Legacy.ConfigDir
	}
	return out
}

// Layout returns the layout, reading KENAZ_HOME and the XDG variables
// through getenv, without looking at the disk (Migrate decides which one
// to use).
func Layout(getenv func(string) string) (Paths, error) {
	return pathsFor(runtime.GOOS, getenv, os.UserHomeDir, os.UserConfigDir)
}

func pathsFor(goos string, getenv func(string) string, home, userConfig func() (string, error)) (Paths, error) {
	if h := getenv(variant.Shared.HomeVar); h != "" {
		abs, err := filepath.Abs(h)
		if err != nil {
			return Paths{}, err
		}
		root := filepath.Join(abs, variant.Shared.ProductDir)
		return Paths{ConfigDir: root, StateRoot: root, Legacy: Legacy{
			ConfigDir:  filepath.Join(abs, "config"),
			StateRoots: []string{filepath.Join(abs, "state")},
		}}, nil
	}
	hd, err := home()
	if err != nil {
		return Paths{}, err
	}
	root := filepath.Join(hd, variant.Shared.RootDir, variant.Shared.ProductDir)
	p := Paths{ConfigDir: root, StateRoot: root}
	// Earlier releases kept the files in the platform's config and state
	// locations, under kameas/<binary>. Only the public build shipped
	// those.
	sub := filepath.Join("kameas", variant.Kenaz.Binary)
	switch goos {
	case "linux":
		cfg := getenv("XDG_CONFIG_HOME")
		if cfg == "" {
			cfg = filepath.Join(hd, ".config")
		}
		st := getenv("XDG_STATE_HOME")
		if st == "" {
			st = filepath.Join(hd, ".local", "state")
		}
		p.Legacy = Legacy{ConfigDir: filepath.Join(cfg, sub), StateRoots: []string{filepath.Join(st, sub)}}
	case "windows":
		base, err := userConfig() // %AppData%
		if err != nil {
			return Paths{}, err
		}
		local := getenv("LocalAppData")
		if local == "" {
			local = filepath.Join(hd, "AppData", "Local")
		}
		cfg := filepath.Join(base, sub)
		p.Legacy = Legacy{ConfigDir: cfg, StateRoots: []string{filepath.Join(local, sub, "state"), filepath.Join(cfg, "state")}}
	default:
		base, err := userConfig()
		if err != nil {
			return Paths{}, err
		}
		cfg := filepath.Join(base, sub)
		p.Legacy = Legacy{ConfigDir: cfg, StateRoots: []string{filepath.Join(cfg, "state")}}
	}
	return p, nil
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

// ErrEnvNotAllowed is wrapped by the error ResolveEnv returns for a known
// Fleet environment this build does not connect to (the staff dev build
// refuses prod).
var ErrEnvNotAllowed = errors.New("Fleet environment not allowed in this build")

type envNotAllowed struct {
	env fleetapi.Env
	src Source
}

func (e envNotAllowed) Error() string {
	id := variant.Current
	msg := fmt.Sprintf("%s connects only to %s Fleet", id.Binary, strings.Join(id.AllowedEnvs, " or "))
	if other, ok := id.For(string(e.env)); ok {
		msg += fmt.Sprintf("; use %s for %s", other.Binary, e.env)
	}
	if e.src != SourceFlag {
		msg += fmt.Sprintf(" (%s was selected by %s)", e.env, e.src)
	}
	return msg
}

func (e envNotAllowed) Unwrap() error { return ErrEnvNotAllowed }

// placeholder reports values a harness left unexpanded, e.g. Codex passes
// "${user_config.env}" through literally from a Claude-format .mcp.json.
func placeholder(v string) bool { return strings.HasPrefix(strings.TrimSpace(v), "${") }

// ResolveEnv applies env precedence: --env flag > env var > config file >
// the build's default (prod). Empty and unexpanded placeholder values are
// skipped at every level. An explicit but unknown value, or one the build
// does not connect to (ErrEnvNotAllowed), is an error at every level,
// never a silent fallback.
func ResolveEnv(flag string, getenv func(string) string, file File) (fleetapi.Env, Source, error) {
	try := func(v string, src Source) (fleetapi.Env, Source, bool, error) {
		v = strings.TrimSpace(v)
		if v == "" || placeholder(v) {
			return "", "", false, nil
		}
		e := fleetapi.Env(strings.ToLower(v))
		if !e.Valid() {
			return "", "", true, fmt.Errorf("unknown Fleet env %q from %s (want %s)", v, src, variant.Current.EnvChoices("|"))
		}
		if !variant.Current.Allowed(string(e)) {
			return "", "", true, envNotAllowed{e, src}
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
	return fleetapi.Env(variant.Current.DefaultEnv), SourceDefault, nil
}

// SetValue writes key = "value" into the config file at path, replacing an
// existing line for key and keeping every other line (comments included).
// The directory is created 0700 and the file written 0600.
func SetValue(path, key, value string) error {
	if strings.ContainsAny(key, "=\n\"") || strings.ContainsAny(value, "\n\"") {
		return fmt.Errorf("config: invalid key or value for %q", key)
	}
	// Write the real file: renaming the temp file over a link (the one
	// Migrate leaves at the old config.toml) would replace the link and
	// leave the file in use unchanged.
	if real, err := filepath.EvalSymlinks(path); err == nil {
		path = real
	}
	// Not covered: a SetValue racing a Migrate that moves the root's
	// config.toml out and the root aside (clearRoot) can land its write in
	// the folder put aside (reported by Unused). It needs a settings change
	// in the instant `kenaz migrate`/`init` runs with no state moved yet;
	// taking the migrate lock here is not worth it.
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
