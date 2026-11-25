package contexttools

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/kameas-ai/kenaz-plugin/fleetapi"
	"github.com/kameas-ai/kenaz-plugin/internal/approvals"
	"github.com/kameas-ai/kenaz-plugin/internal/contextlib"
)

// grantCheck drives the relay's grant check directly: each take is a request the
// tool really prepared (so the ledger holds what grantCovers requires), offered to
// the check, and settled when and how the test says. The clock is the test's.
type grantCheck struct {
	t     *testing.T
	d     *grantDesk
	auto  approvals.AutoApprover
	mu    sync.Mutex
	now   time.Time
	codes map[string]string
	n     int
}

func newGrantCheck(t *testing.T) *grantCheck {
	t.Helper()
	c := &grantCheck{t: t, d: newGrantDesk(t, newGraph(), "u1", platform), now: fixedNow, codes: map[string]string{}}
	c.restart()
	c.d.on(t)
	return c
}

// restart is the relay started again: its own counts are gone, and the history is
// all it has.
func (c *grantCheck) restart() {
	c.auto = AutoApproveShareFile(ShareGrantOptions{
		Grant: c.d.grant, Ledger: c.d.led, Provider: c.d.desk.m,
		Now:         func() time.Time { c.mu.Lock(); defer c.mu.Unlock(); return c.now },
		FailureCode: func(id string) string { c.mu.Lock(); defer c.mu.Unlock(); return c.codes[id] },
	})
}

func (c *grantCheck) at(now time.Time) { c.mu.Lock(); c.now = now; c.mu.Unlock() }

// take prepares a new file's share and offers it to the grant check. settle is nil
// when the grant didn't take it.
func (c *grantCheck) take() (settle func(st approvals.Status, code string)) {
	c.t.Helper()
	c.mu.Lock()
	c.n++
	n := c.n
	c.mu.Unlock()
	name := fmt.Sprintf("g%03d.md", n)
	c.d.put(c.t, name, fmt.Sprintf("text %d\n", n))
	o := c.d.opts
	seededOpts(&o, name)
	// Prepared with a recorder, which never runs it: the check below decides.
	out := runShareFile(context.Background(), c.d.desk.m, c.d.desk.m.approvals, nil, o, ShareFileInput{Path: "personal/" + name})
	if out.Error != nil {
		c.t.Fatalf("%+v", out.Error)
	}
	id := fmt.Sprintf("apr_%026d", n)
	who := approvals.Identity{Env: c.d.acct.Env, OrgID: c.d.acct.OrgID, UserID: c.d.acct.UserID}
	settled, covered := c.auto(context.Background(), who, id, c.d.desk.m.approvals.last().Payload)
	if !covered {
		return nil
	}
	return func(st approvals.Status, code string) {
		c.mu.Lock()
		c.codes[id] = code
		c.mu.Unlock()
		settled(st)
	}
}

// room counts how many more shares the grant takes before it stops, settling
// each as done.
func (c *grantCheck) room() int {
	c.t.Helper()
	for n := 0; ; n++ {
		settle := c.take()
		if settle == nil {
			return n
		}
		settle(approvals.StatusDone, "")
		if n > 3*contextlib.MaxGrantedSharesPerHour {
			c.t.Fatal("the grant never stopped")
		}
	}
}

// loseHistory is the history file gone, with the grant still on: only the relay's
// own count is left to hold the hour.
func (c *grantCheck) loseHistory() {
	c.t.Helper()
	if err := c.d.grant.Wipe(); err != nil {
		c.t.Fatal(err)
	}
	c.d.on(c.t)
}

func seededOpts(o *FilesOptions, seed string) {
	d := &desk{opts: *o}
	seeded(d, seed)
	*o = d.opts
}

const hourCap = contextlib.MaxGrantedSharesPerHour

// Only a not_team_member failure hands its slot back. Any other failure keeps it
// in the relay's own count too, which is all that's left when the history is lost
// after the failures.
func TestGrantRefundsOnlyNotTeamMember(t *testing.T) {
	for code, back := range map[string]int{
		"not_team_member":     1,
		"node_changed":        0,
		"credential_detected": 0,
		"scope_changed":       0,
		"fleet_error":         0,
		"":                    0,
	} {
		c := newGrantCheck(t)
		for i := 0; i < 3; i++ {
			settle := c.take()
			if settle == nil {
				t.Fatalf("%q: not taken", code)
			}
			settle(approvals.StatusFailed, code)
		}
		c.loseHistory()
		if got, want := c.room(), hourCap-3+3*back; got != want {
			t.Errorf("after three shares failed as %q and the history was lost, the grant took %d more, want %d", code, got, want)
		}
	}
	// A not_team_member code on a request that didn't fail hands nothing back.
	for _, st := range []approvals.Status{approvals.StatusDone, approvals.StatusCanceled, approvals.StatusReconciling} {
		c := newGrantCheck(t)
		c.take()(st, "not_team_member")
		c.loseHistory()
		if got := c.room(); got != hourCap-1 {
			t.Errorf("settled %s: the grant took %d more, want %d", st, got, hourCap-1)
		}
	}
}

// A share taken in one clock hour and refused in the next hands nothing back to
// the new hour: its slot was the old hour's.
func TestGrantRefundStaysInItsHour(t *testing.T) {
	c := newGrantCheck(t)
	late := c.take()
	c.at(fixedNow.Add(time.Hour))
	if settle := c.take(); settle == nil {
		t.Fatal("the new hour's first share wasn't taken")
	} else {
		settle(approvals.StatusDone, "")
	}
	late(approvals.StatusFailed, "not_team_member")
	c.loseHistory()
	if got := c.room(); got != hourCap-1 {
		t.Fatalf("after last hour's share was refused, this hour took %d more, want %d", got, hourCap-1)
	}
}

// The relay's count never goes below zero, whatever order refusals settle in and
// whatever the clock does: a count below zero would be a share past the cap.
func TestGrantRefundNeverGoesBelowZero(t *testing.T) {
	c := newGrantCheck(t)
	first := c.take()
	// The clock steps into the next hour and back (a correction), with a share
	// taken in each: the count for this hour starts again at the step back.
	c.at(fixedNow.Add(time.Hour))
	c.take()(approvals.StatusDone, "")
	c.loseHistory()
	c.at(fixedNow)
	second := c.take()
	first(approvals.StatusFailed, "not_team_member")
	second(approvals.StatusFailed, "not_team_member")
	c.loseHistory()
	if got := c.room(); got != hourCap {
		t.Fatalf("the grant took %d more, want exactly the cap of %d", got, hourCap)
	}
}

const refusedCap = contextlib.MaxRefusedGrantSharesPerHour

// Refusals for membership are free only up to their own small bound: after that
// many in a clock hour the grant takes nothing more until the next hour, with or
// without its history.
func TestGrantStopsAfterTooManyMembershipRefusals(t *testing.T) {
	c := newGrantCheck(t)
	for i := 0; i < refusedCap; i++ {
		settle := c.take()
		if settle == nil {
			t.Fatalf("refusal %d wasn't taken", i+1)
		}
		settle(approvals.StatusFailed, "not_team_member")
	}
	if used, refused := c.d.grant.UsedThisHour(c.d.acct, fixedNow), c.d.grant.RefusedThisHour(c.d.acct, fixedNow); used != 0 || refused != refusedCap {
		t.Fatalf("%d used, %d refused", used, refused)
	}
	if c.take() != nil {
		t.Fatal("the grant took a share after the hour's refusals were spent")
	}
	// A relay started again reads them from the history.
	c.restart()
	if c.take() != nil {
		t.Fatal("restarting the relay handed the refusals back")
	}
	c.loseHistory()
	if c.take() != nil {
		t.Fatal("losing the history handed the refusals back")
	}
	// The next hour starts again.
	c.at(fixedNow.Add(time.Hour))
	if got := c.room(); got != hourCap {
		t.Fatalf("the next hour took %d", got)
	}
}

// One short of the bound, the grant still works, and the slots are exact.
func TestGrantWorksBelowTheRefusalBound(t *testing.T) {
	c := newGrantCheck(t)
	for i := 0; i < refusedCap-1; i++ {
		c.take()(approvals.StatusFailed, "not_team_member")
	}
	c.loseHistory()
	if got := c.room(); got != hourCap {
		t.Fatalf("after %d free refusals the grant took %d, want the whole cap of %d", refusedCap-1, got, hourCap)
	}
}

// All at once: a full hour's worth of shares is in flight when every one of them
// is refused. Exactly the bound's worth are free; the rest keep their slots, so
// refusals can never be more than the cap plus the bound, and the grant stops.
func TestGrantRefusalBoundHoldsUnderConcurrency(t *testing.T) {
	c := newGrantCheck(t)
	var settles []func(approvals.Status, string)
	for {
		settle := c.take()
		if settle == nil {
			break
		}
		settles = append(settles, settle)
	}
	if len(settles) != hourCap {
		t.Fatalf("%d in flight, want the cap of %d", len(settles), hourCap)
	}
	var wg sync.WaitGroup
	for _, settle := range settles {
		wg.Add(1)
		go func() { defer wg.Done(); settle(approvals.StatusFailed, "not_team_member") }()
	}
	wg.Wait()
	free, counted := 0, 0
	for _, e := range c.d.grant.History(c.d.acct) {
		switch {
		case !e.RefusedNotMember():
			t.Fatalf("%+v", e)
		case e.Counts():
			counted++
		default:
			free++
		}
	}
	if free != refusedCap || counted != hourCap-refusedCap {
		t.Fatalf("%d free and %d counted refusals, want %d and %d", free, counted, refusedCap, hourCap-refusedCap)
	}
	if used := c.d.grant.UsedThisHour(c.d.acct, fixedNow); used != hourCap-refusedCap {
		t.Fatalf("%d used", used)
	}
	if c.take() != nil {
		t.Fatal("the grant took a share after the hour's refusals were spent")
	}
	c.loseHistory()
	if c.take() != nil {
		t.Fatal("losing the history handed the refusals back")
	}
}

// Through the tool, the share that meets the stopped grant waits for the person
// and says why.
func TestShareSaysTheGrantStoppedForRefusals(t *testing.T) {
	g := newGraph()
	d := newGrantDesk(t, g, "u1", platform)
	var refuse error = &fleetapi.APIError{Status: 403, Code: "not_team_member"}
	p := &refusing{member: d.desk.m, err: func() error { return refuse }}
	d.mgr.Register(ShareFileOperation, ExecuteShareFile(p, nil, d.led))
	d.mgr.RegisterAutoApprover(ShareFileOperation, AutoApproveShareFile(ShareGrantOptions{Grant: d.grant, Ledger: d.led, Provider: p, Now: d.opts.Now, FailureCode: d.mgr.FailureCode}))
	d.on(t)
	for i := 0; i < refusedCap; i++ {
		name := fmt.Sprintf("r%02d.md", i)
		d.put(t, name, fmt.Sprintf("refused %d\n", i))
		seeded(d.desk, name)
		if out := d.share(ShareFileInput{Path: "personal/" + name}); out.Status != string(approvals.StatusFailed) || !out.SharedUnderGrant {
			t.Fatalf("%+v", out)
		}
	}
	refuse = nil
	d.put(t, "fine.md", "fine\n")
	seeded(d.desk, "fine")
	out := d.share(ShareFileInput{Path: "personal/fine.md"})
	want := fmt.Sprintf("The context share grant is on, but %d of its shares this clock hour were refused because the user is not a member of the team, so it shares nothing more without approval until the next hour, so this one waits for the user's approval as usual.", refusedCap)
	if out.Error != nil || out.Status != string(approvals.StatusPending) || out.SharedUnderGrant || out.ApproveURL == "" || !strings.HasSuffix(out.Message, want) {
		t.Fatalf("%+v", out)
	}
	if len(g.nodes) != 0 {
		t.Fatal("something was shared")
	}
}
