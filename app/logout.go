package app

import (
	"errors"
	"flag"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"time"

	"github.com/kameas-ai/kenaz-plugin/fleetapi"
	"github.com/kameas-ai/kenaz-plugin/internal/approvals"
	"github.com/kameas-ai/kenaz-plugin/internal/consent"
	"github.com/kameas-ai/kenaz-plugin/internal/credstore"
	"github.com/kameas-ai/kenaz-plugin/internal/daemon"
	"github.com/kameas-ai/kenaz-plugin/internal/memory/capture"
	"github.com/kameas-ai/kenaz-plugin/internal/spool"
	"github.com/kameas-ai/kenaz-plugin/internal/variant"
)

// How long logout waits for the relay it asked to stop to exit, and how
// long it keeps trying to remove the spool's files after that.
var (
	relayExitWait  = 5 * time.Second
	spoolPurgeWait = 3 * time.Second
)

// waitRelayExit waits until no relay holds stateDir's lock, which a relay
// keeps until it exits, or until wait has passed. It reports whether the
// relay is gone.
func waitRelayExit(stateDir string, wait time.Duration) bool {
	lock := filepath.Join(stateDir, daemon.LockFile)
	deadline := time.Now().Add(wait)
	for {
		if _, err := os.Stat(lock); err != nil {
			return true // no relay ever ran here
		}
		if release, err := daemon.TryLock(lock); err == nil {
			release()
			return true
		}
		if !time.Now().Before(deadline) {
			return false
		}
		time.Sleep(50 * time.Millisecond)
	}
}

// runLogout signs out, stops the daemon after purging lanes, deletes the
// spool and its key. Local purge happens even on an
// unofficial build or when Fleet is unreachable.
func runLogout(env Env, args []string) int {
	fs := flag.NewFlagSet("logout", flag.ContinueOnError)
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
	// For a retired environment the state folder is not created (resolveState).
	// Where there was none, the purges below find nothing; the empty folders
	// they make on the way are removed again, so nothing is left behind.
	for _, dir := range []string{filepath.Dir(r.Paths.StateRoot), r.Paths.StateRoot, r.StateDir} {
		if _, err := os.Stat(dir); os.IsNotExist(err) {
			defer removeEmptyDirs(dir)
			break
		}
	}
	// Withdrawal on logout: forget the member's notice acceptance
	// so a later sign-in re-shows the notice before anything is sent.
	sess, sessErr := env.Provider.Auth().Session(env.Ctx)
	authErr := env.Provider.Auth().Logout(env.Ctx)
	if c := connectDaemon(env.Ctx, r.StateDir); c != nil {
		c.Shutdown(env.Ctx, true)
		// The request is answered once the relay has purged, or not at all
		// when its lanes take longer to stop than the request waits; it never
		// waits for the relay to exit. Until it has exited it may hold a
		// spool segment open, and Windows refuses to remove an open file, so
		// give it a bounded time to go before purging here.
		waitRelayExit(r.StateDir, relayExitWait)
	}
	var errs []error
	// Cancel and purge every write still waiting for approval, even when
	// no daemon was running to do it.
	if err := approvals.CancelAllOffline(approvalsDir(r.StateDir), approvalsStoreKey(env, r.Env, false), env.now()); err != nil {
		errs = append(errs, fmt.Errorf("cancel pending approvals: %w", err))
	}
	// The context share grant goes with the session, and so does its history.
	if err := env.shareGrant(r.StateDir, r.Env, false).Wipe(); err != nil {
		errs = append(errs, fmt.Errorf("end the context share grant: %w", err))
	}
	// Retried: a relay that is still stopping, or one this command could not
	// reach, may still hold a segment. What cannot be removed is said plainly,
	// because it is the signed-out account's queued telemetry.
	if err := spool.PurgeDirWait(filepath.Join(r.StateDir, "spool"), spoolPurgeWait); err != nil {
		errs = append(errs, fmt.Errorf("queued telemetry could not be deleted and is still on this device, most likely because a relay still has it open; quit the apps that run %s and run `%s logout` again: %w", variant.Current.Binary, variant.Current.Binary, err))
	}
	if sessErr == nil && sess.UserID != "" {
		if err := consent.NewStore(filepath.Join(r.StateDir, "consent.json")).Forget(sess.UserID); err != nil {
			errs = append(errs, fmt.Errorf("forget consent: %w", err))
		}
	}
	// managed skill directories go with the session, even when the
	// daemon was not running to purge its lane.
	if err := purgeSkills(env.Provider, r.Env, r.StateDir); err != nil {
		errs = append(errs, fmt.Errorf("remove managed skills: %w", err))
	}
	if err := env.keyStore().Delete(string(r.Env)); err != nil {
		errs = append(errs, fmt.Errorf("delete spool key: %w", err))
	}
	// Forget the recorded credential store, so the next sign-in chooses
	// again. This is also the reset for a recorded keychain that is gone.
	if env.cred != nil && env.cred.dir != "" {
		if err := credstore.RemoveMarker(env.cred.dir); err != nil {
			errs = append(errs, fmt.Errorf("remove credential store marker: %w", err))
		}
	}
	// What capture queued is still unprocessed session data of the account
	// signing out. It goes, and so does Fleet's answer for that account: capture
	// doesn't carry on signed out unless it is turned on signed out. Memory itself
	// is local user data, not Fleet data, and logout keeps it.
	if mp, err := env.memoryPaths(); err == nil {
		if err := capture.ResetOffer(mp.captureRoot()); err != nil {
			errs = append(errs, fmt.Errorf("purge memory capture queue: %w", err))
		}
	}
	if l := env.keptMemoryLine(); l != "" {
		defer fmt.Fprintln(env.Stdout, l)
	}
	if err := errors.Join(errs...); err != nil {
		fmt.Fprintln(env.Stderr, variant.Current.Binary+": "+err.Error())
		return ExitError
	}
	if authErr != nil && !errors.Is(authErr, fleetapi.ErrNotSignedIn) {
		fmt.Fprintln(env.Stdout, "Local data purged.")
		return providerErr(env, authErr)
	}
	fmt.Fprintf(env.Stdout, "Signed out of Kameas Fleet (%s). Queued data on this device was deleted.\n", r.Env)
	return ExitOK
}

// removeEmptyDirs removes dir and the folders under it that hold nothing.
// A folder with a file anywhere in it stays, with that file.
func removeEmptyDirs(dir string) {
	var dirs []string
	_ = filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
		if err == nil && d.IsDir() {
			dirs = append(dirs, p)
		}
		return nil
	})
	for i := len(dirs) - 1; i >= 0; i-- {
		_ = os.Remove(dirs[i]) // fails, as it should, when not empty
	}
}
