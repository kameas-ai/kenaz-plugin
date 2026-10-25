package app

import (
	"flag"
	"fmt"

	"github.com/kameas-ai/kenaz-plugin/internal/device"
	"github.com/kameas-ai/kenaz-plugin/internal/enroll"
	"github.com/kameas-ai/kenaz-plugin/internal/variant"
)

// runDevice shows or changes the device display name sent to Kameas Fleet
// instead of the computer's hostname.
//
//	kenaz device [show]
//	kenaz device rename <name>
//	kenaz device rename --use-computer-name
//	kenaz device reset
func runDevice(env Env, args []string) int {
	fs := flag.NewFlagSet("device", flag.ContinueOnError)
	fs.SetOutput(env.Stderr)
	envName := fs.String("env", "", "Fleet environment")
	if err := fs.Parse(args); err != nil {
		return ExitUsage
	}
	r, err := env.resolve(*envName)
	if err != nil {
		fmt.Fprintln(env.Stderr, variant.Current.Binary+": "+err.Error())
		return ExitUsage
	}
	nodeID, err := enroll.LoadOrCreateNodeID(r.StateDir, env.now())
	if err != nil {
		fmt.Fprintln(env.Stderr, variant.Current.Binary+": "+err.Error())
		return ExitError
	}
	rest := fs.Args()
	sub := "show"
	if len(rest) > 0 {
		sub, rest = rest[0], rest[1:]
	}
	switch sub {
	case "show":
	case "rename":
		rfs := flag.NewFlagSet("device rename", flag.ContinueOnError)
		rfs.SetOutput(env.Stderr)
		useHost := rfs.Bool("use-computer-name", false, "send this computer's hostname (explicit opt-in)")
		if err := rfs.Parse(rest); err != nil {
			return ExitUsage
		}
		switch {
		case *useHost && rfs.NArg() == 0:
			if err := device.UseComputerName(r.StateDir); err != nil {
				fmt.Fprintln(env.Stderr, variant.Current.Binary+": "+err.Error())
				return ExitError
			}
		case !*useHost && rfs.NArg() == 1:
			if _, err := device.Rename(r.StateDir, rfs.Arg(0)); err != nil {
				fmt.Fprintln(env.Stderr, variant.Current.Binary+": "+err.Error())
				return ExitUsage
			}
		default:
			fmt.Fprintln(env.Stderr, "usage: kenaz device rename <name> | --use-computer-name")
			return ExitUsage
		}
	case "reset":
		if err := device.Reset(r.StateDir); err != nil {
			fmt.Fprintln(env.Stderr, variant.Current.Binary+": "+err.Error())
			return ExitError
		}
	default:
		fmt.Fprintln(env.Stderr, "usage: kenaz device [show|rename <name>|rename --use-computer-name|reset]")
		return ExitUsage
	}
	s, _ := device.Load(r.StateDir)
	h := enroll.LocalHost()
	name := device.Resolve(s, h.GOOS, nodeID, h.Hostname)
	fmt.Fprintf(env.Stdout, "Device name shown to your Kameas Fleet admins: %s\n", name)
	if sub != "show" {
		if c := connectDaemon(env.Ctx, r.StateDir); c != nil {
			c.Kick(env.Ctx) // re-sent on the next heartbeat; kick sends it now
		}
	}
	return ExitOK
}
