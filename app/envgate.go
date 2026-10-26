package app

import (
	"errors"
	"fmt"
	"strings"

	"github.com/kameas-ai/kenaz-plugin/fleetapi"
	"github.com/kameas-ai/kenaz-plugin/internal/config"
	"github.com/kameas-ai/kenaz-plugin/internal/variant"
)

// envGateSkip are the commands the env gate leaves alone: the hook must
// never fail a harness session, and help-like commands select no env.
var envGateSkip = map[string]bool{"hook": true, "licenses": true, "version": true, "migrate": true}

// refuseEnv stops a command, with a usage error, when the Fleet
// environment it would use (its --env flag, the env variables or
// config.toml) is one this build does not connect to: the staff dev build
// refuses prod. Every env the public build knows is allowed, so it never
// refuses there. Unknown env names are left to the command to report.
func (e Env) refuseEnv(cmd string, args []string) (int, bool) {
	if envGateSkip[cmd] || allEnvsAllowed() {
		return 0, false
	}
	paths, err := e.paths()
	if err != nil {
		return 0, false
	}
	file, err := config.LoadFile(paths.ConfigFile())
	if err != nil {
		return 0, false
	}
	_, _, err = config.ResolveEnv(envFlag(args), e.getenv, file)
	if !errors.Is(err, config.ErrEnvNotAllowed) {
		return 0, false
	}
	fmt.Fprintln(e.Stderr, err.Error())
	return ExitUsage, true
}

// allEnvsAllowed reports whether this build connects to every Fleet
// environment (the public build), so there is nothing to refuse.
func allEnvsAllowed() bool {
	for _, env := range []fleetapi.Env{fleetapi.EnvProd, fleetapi.EnvStage, fleetapi.EnvDev} {
		if !variant.Current.Allowed(string(env)) {
			return false
		}
	}
	return true
}

// envFlag returns the value of the last --env (or -env) flag in args.
func envFlag(args []string) string {
	v := ""
	for i, a := range args {
		if a == "--" {
			break
		}
		name, val, hasVal := strings.Cut(strings.TrimLeft(a, "-"), "=")
		if !strings.HasPrefix(a, "-") || name != "env" {
			continue
		}
		switch {
		case hasVal:
			v = val
		case i+1 < len(args):
			v = args[i+1]
		}
	}
	return v
}
