package setup

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"

	"github.com/kameas-ai/kenaz-plugin/internal/daemonstate"
	"github.com/kameas-ai/kenaz-plugin/internal/variant"
)

// otherEnvSetup is the setup record another Fleet environment on this
// device holds for a host. Claude Code and Codex have one plugin
// marketplace of this name and one set of OTEL settings, while setup state
// is kept per environment, so a host set up for one environment looks
// foreign from every other one.
type otherEnvSetup struct {
	Env    string // the environment that set the host up
	Binary string // the build that did
	dir    string
	fs     *FileState
}

// undo is the command that removes s's setup of h.
func (s otherEnvSetup) undo(h harness) string {
	return fmt.Sprintf("%s setup --undo %s --env %s", s.Binary, hostCLI(h), s.Env)
}

// envName is the Fleet environment o.StateDir belongs to: state
// directories are <root>/<env>. It is "" for any other directory.
func (o Options) envName() string {
	if env := filepath.Base(o.StateDir); slices.Contains(daemonstate.Environments, env) {
		return env
	}
	return ""
}

// initCmd is the `init` command that registers the apps again for
// o.StateDir's environment: with --env unless that is the build's default,
// which a plain init acts on.
func (o Options) initCmd() string {
	return variant.Current.Command("init", o.envName())
}

// otherEnvSetups returns the setup records for h in the other
// environments' state directories. It only reads files, and reads them
// whichever build wrote them.
func (o Options) otherEnvSetups(h harness) []otherEnvSetup {
	env := o.envName()
	if env == "" {
		return nil
	}
	var out []otherEnvSetup
	for _, e := range daemonstate.Environments {
		if e == env {
			continue
		}
		dir := filepath.Join(filepath.Dir(o.StateDir), e)
		b, err := os.ReadFile(filepath.Join(dir, stateFile))
		if err != nil {
			continue
		}
		var s State
		if json.Unmarshal(b, &s) != nil {
			continue
		}
		fs := s.get(h)
		if fs == nil {
			continue
		}
		bin := s.build()
		if v, ok := variant.Named(s.Variant); ok {
			bin = v.Binary
		}
		out = append(out, otherEnvSetup{Env: e, Binary: bin, dir: dir, fs: fs})
	}
	return out
}

// otherEnvRegistered returns the other environment whose setup registered
// the plugin marketplace h currently has at path cur.
func (o Options) otherEnvRegistered(h harness, cur string) (otherEnvSetup, bool) {
	for _, s := range o.otherEnvSetups(h) {
		if reg := s.fs.Bundle; reg != nil && reg.Rendered != "" && (cur == reg.Rendered || cur == rebase(s.dir, reg.Rendered)) {
			return s, true
		}
	}
	return otherEnvSetup{}, false
}

// otherEnvEndpoint returns the other environment whose setup wrote
// endpoint, a loopback OTLP endpoint, into h's config file.
func (o Options) otherEnvEndpoint(h harness, endpoint string) (otherEnvSetup, bool) {
	for _, s := range o.otherEnvSetups(h) {
		if s.fs.Path != o.path(h) {
			continue
		}
		for _, k := range s.fs.Keys {
			if strings.Contains(k.Written, endpoint) {
				return s, true
			}
		}
	}
	return otherEnvSetup{}, false
}

// checkUserEndpoint is checkEndpoint for the endpoint in h's own config
// file. An endpoint on another port than this environment's relay that
// another environment's setup wrote is reported as that, with the command
// that undoes it: re-running setup here is refused while the host is set
// up there.
func (o Options) checkUserEndpoint(h harness, add func(status, format string, a ...any), endpoint string, port int) {
	if port != 0 && isLoopbackURL(endpoint) && !strings.Contains(endpoint, ":"+strconv.Itoa(port)) {
		if s, ok := o.otherEnvEndpoint(h, endpoint); ok {
			add("warn", "exports to %s, the relay of the %s environment, which it was set up for; the %s relay listens on port %d. To use %s instead, run `%s`, then set it up again",
				endpoint, s.Env, o.envName(), port, o.envName(), s.undo(h))
			return
		}
	}
	checkEndpoint(add, endpoint, port)
}
