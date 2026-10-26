package app

import (
	"errors"
	"flag"
	"fmt"
	"path/filepath"

	"github.com/kameas-ai/kenaz-plugin/fleetapi"
	"github.com/kameas-ai/kenaz-plugin/internal/approvals"
	"github.com/kameas-ai/kenaz-plugin/internal/consent"
	"github.com/kameas-ai/kenaz-plugin/internal/credstore"
	"github.com/kameas-ai/kenaz-plugin/internal/spool"
	"github.com/kameas-ai/kenaz-plugin/internal/variant"
)

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
	// Withdrawal on logout: forget the member's notice acceptance
	// so a later sign-in re-shows the notice before anything is sent.
	sess, sessErr := env.Provider.Auth().Session(env.Ctx)
	authErr := env.Provider.Auth().Logout(env.Ctx)
	if c := connectDaemon(env.Ctx, r.StateDir); c != nil {
		c.Shutdown(env.Ctx, true)
	}
	var errs []error
	// Cancel and purge every write still waiting for approval, even when
	// no daemon was running to do it.
	if err := approvals.CancelAllOffline(approvalsDir(r.StateDir), approvalsStoreKey(env, r.Env, false), env.now()); err != nil {
		errs = append(errs, fmt.Errorf("cancel pending approvals: %w", err))
	}
	if err := spool.PurgeDir(filepath.Join(r.StateDir, "spool")); err != nil {
		errs = append(errs, fmt.Errorf("purge spool: %w", err))
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
	// Memory is local user data, not Fleet data: logout keeps it.
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
