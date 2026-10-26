package skills

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/kameas-ai/kenaz-plugin/fleetapi"
	"github.com/kameas-ai/kenaz-plugin/internal/daemon"
)

// LaneName is the daemon lane name.
const LaneName = "skills"

// DefaultInterval is the config-poll interval (one interval is the
// bound for a newly mandated skill to appear).
const DefaultInterval = 5 * time.Minute

// Lane runs skills sync inside the daemon. It is a download lane: Hold
// pauses polling (org paused, lockdown) and Purge removes every managed
// skill directory and the member's subscriptions (logout, node removed).
type Lane struct {
	Syncer   *Syncer
	Interval time.Duration

	mu      sync.Mutex
	cancel  context.CancelFunc
	held    bool
	reason  string
	applied int64
	failed  int64
	last    *Report
	kick    chan struct{}
}

var (
	_ daemon.Lane              = (*Lane)(nil)
	_ daemon.StatusContributor = (*Lane)(nil)
)

// Name implements daemon.Lane.
func (l *Lane) Name() string { return LaneName }

// Start implements daemon.Lane. It syncs once immediately, then on every
// interval or kick.
func (l *Lane) Start(ctx context.Context) error {
	l.mu.Lock()
	if l.cancel != nil {
		l.mu.Unlock()
		return nil
	}
	ctx, l.cancel = context.WithCancel(ctx)
	l.kick = make(chan struct{}, 1)
	if st, err := l.Syncer.Store.LoadSyncState(); err == nil && st.Report != nil {
		l.last = st.Report
	}
	l.mu.Unlock()
	iv := l.Interval
	if iv <= 0 {
		iv = DefaultInterval
	}
	go func() {
		t := time.NewTicker(iv)
		defer t.Stop()
		for {
			l.runOnce(ctx)
			select {
			case <-ctx.Done():
				return
			case <-t.C:
			case <-l.kick:
			}
		}
	}()
	return nil
}

// Kick requests an immediate sync.
func (l *Lane) Kick() {
	l.mu.Lock()
	k := l.kick
	l.mu.Unlock()
	if k != nil {
		select {
		case k <- struct{}{}:
		default:
		}
	}
}

func (l *Lane) runOnce(ctx context.Context) {
	l.mu.Lock()
	held := l.held
	l.mu.Unlock()
	if held {
		return
	}
	rep, err := l.Syncer.Sync(ctx)
	if err != nil {
		if !errors.Is(err, fleetapi.ErrNotSignedIn) && !errors.Is(err, fleetapi.ErrUnofficialBuild) && ctx.Err() == nil {
			l.Syncer.log().Warn("skills: sync failed", "err", err.Error())
		}
		return
	}
	l.mu.Lock()
	l.applied += int64(rep.Applied)
	l.failed += int64(rep.Failed)
	l.last = &rep
	l.mu.Unlock()
}

// Stop implements daemon.Lane.
func (l *Lane) Stop() error {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.cancel != nil {
		l.cancel()
		l.cancel = nil
	}
	return nil
}

// Hold implements daemon.Lane.
func (l *Lane) Hold(reason string) {
	l.mu.Lock()
	l.held, l.reason = true, reason
	l.mu.Unlock()
}

// Resume implements daemon.Lane.
func (l *Lane) Resume() {
	l.mu.Lock()
	l.held, l.reason = false, ""
	l.mu.Unlock()
	l.Kick()
}

// Purge implements daemon.Lane: org content does not outlive the session.
func (l *Lane) Purge() error {
	unlock, err := l.Syncer.Store.Lock(10 * time.Second)
	if err != nil {
		return err
	}
	defer unlock()
	owned, err := l.Syncer.Mat.Owned()
	if err != nil {
		return err
	}
	var errs []error
	for _, o := range owned {
		errs = append(errs, l.Syncer.Mat.removeOwned(o.Dir))
	}
	errs = append(errs, l.Syncer.Store.Reset())
	if len(owned) > 0 {
		errs = append(errs, l.Syncer.Store.BumpGeneration())
	}
	l.mu.Lock()
	l.last = nil
	l.mu.Unlock()
	return errors.Join(errs...)
}

// Status implements daemon.Lane.
func (l *Lane) Status() daemon.LaneStatus {
	l.mu.Lock()
	defer l.mu.Unlock()
	st := daemon.LaneStatus{Name: LaneName, State: "running", Uploaded: l.applied, Dropped: l.failed}
	switch {
	case l.cancel == nil:
		st.State = "stopped"
	case l.held:
		st.State, st.Reason = "held", l.reason
	}
	return st
}

// ContributeStatus adds the strip / translation / skip notes
// to status and fleet_status.
func (l *Lane) ContributeStatus(s *daemon.Status) {
	l.mu.Lock()
	rep := l.last
	l.mu.Unlock()
	s.Notes = append(s.Notes, StatusNotes(rep)...)
}

// StatusNotes renders a report as short status lines.
func StatusNotes(rep *Report) []string {
	if rep == nil {
		return nil
	}
	var notes []string
	if len(rep.Stripped) > 0 {
		notes = append(notes, fmt.Sprintf("%d shared skill(s) were installed without their tool pre-approvals or hooks (ask your admin to mandate them if needed): %s",
			len(rep.Stripped), strings.Join(rep.Stripped, "; ")))
	}
	if len(rep.Translated) > 0 {
		notes = append(notes, "Translated from older Kenaz skills: "+strings.Join(rep.Translated, ", "))
	}
	if len(rep.Skipped) > 0 {
		notes = append(notes, "Skills not installed: "+strings.Join(rep.Skipped, "; "))
	}
	return notes
}
