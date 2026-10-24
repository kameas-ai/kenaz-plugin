package app

import (
	"fmt"
	"io"
	"time"

	"github.com/kameas-ai/kenaz-plugin/internal/daemon"
	"github.com/kameas-ai/kenaz-plugin/internal/setup"
)

// hostSetupHook adds the per-host setup state to the daemon's status.
func hostSetupHook(opt *daemon.Options) (func(*daemon.Daemon), error) {
	dir := opt.StateDir
	opt.Contributors = append(opt.Contributors, daemon.StatusContributorFunc(func(s *daemon.Status) {
		s.Hosts = hostSetups(dir)
	}))
	return nil, nil
}

// hostSetups converts setup's record into the status shape.
func hostSetups(stateDir string) []daemon.HostSetup {
	var out []daemon.HostSetup
	for _, h := range setup.HostStates(stateDir) {
		out = append(out, daemon.HostSetup{Host: h.Host, State: h.State})
	}
	return out
}

// reportingText is the one-line meaning of each reporting state.
var reportingText = map[string]string{
	daemon.ReportingRelayStopped:    "not running: the relay is stopped (it starts with your next session)",
	daemon.ReportingNotSignedIn:     "off: not signed in (run `kenaz login`)",
	daemon.ReportingConsentUnknown:  "off: signed in, but your sharing choice could not be read from Kameas Fleet yet",
	daemon.ReportingNotAdmitted:     "off: your organization is not in early access",
	daemon.ReportingNoConsent:       "off: you have not opted in (run `kenaz optin`)",
	daemon.ReportingReconfirm:       "off: review what is shared (run `kenaz optin`)",
	daemon.ReportingHeld:            "held",
	daemon.ReportingFailing:         "failing: the last upload attempt failed",
	daemon.ReportingWaitingForFirst: "ready: nothing uploaded yet",
	daemon.ReportingWorking:         "working",
}

// writeReporting renders Status.Reporting and Status.Hosts for `kenaz
// status` and `kenaz doctor`. Counts, times and categories only.
func writeReporting(w io.Writer, prefix string, st daemon.Status) {
	if r := st.Reporting; r != nil {
		line := reportingText[r.State]
		if line == "" {
			line = r.State
		}
		if r.HoldReason != "" {
			line += " (" + r.HoldReason + ")"
		}
		fmt.Fprintf(w, "%sreporting:    %s\n", prefix, line)
		fmt.Fprintf(w, "%s  signed in %s, consent %s, queued %d\n", prefix, yesNo(r.SignedIn), r.Consent, r.Queued)
		if r.LastUploadAt != nil {
			fmt.Fprintf(w, "%s  last successful upload %s\n", prefix, r.LastUploadAt.Format(time.RFC3339))
		} else if r.State != daemon.ReportingRelayStopped {
			fmt.Fprintf(w, "%s  no successful upload since the relay started\n", prefix)
		}
		if r.LastError != "" && r.LastErrorAt != nil {
			fmt.Fprintf(w, "%s  last error %s at %s\n", prefix, r.LastError, r.LastErrorAt.Format(time.RFC3339))
		}
		if r.ReceiverPort != 0 {
			fmt.Fprintf(w, "%s  receiver 127.0.0.1:%d\n", prefix, r.ReceiverPort)
		}
	}
	for _, h := range st.Hosts {
		fmt.Fprintf(w, "%ssetup %-12s %s\n", prefix, h.Host+":", h.State)
	}
}

func yesNo(b bool) string {
	if b {
		return "yes"
	}
	return "no"
}
