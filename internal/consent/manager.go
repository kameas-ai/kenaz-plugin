package consent

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"time"
	"unicode"

	"github.com/kameas-ai/kenaz-plugin/fleetapi"
	"github.com/kameas-ai/kenaz-plugin/notices"
)

// Action is a gate outcome for one telemetry record.
type Action int

const (
	// ActionSend: forward now.
	ActionSend Action = iota
	// ActionHold: spool and forward when the hold clears (approval, pause,
	// lockdown). The spool's TTL still applies. An organization
	// whose plan does not include the plugin is never a hold: its records are dropped.
	ActionHold
	// ActionDrop: discard on the device and count it. Never spooled.
	ActionDrop
)

func (a Action) String() string {
	switch a {
	case ActionSend:
		return "send"
	case ActionHold:
		return "hold"
	}
	return "drop"
}

// Drop reasons in Decision.Reason.
const (
	ReasonNotSignedIn     = "not_signed_in"
	ReasonOptOut          = "optout"
	ReasonUnavailable     = "unavailable"
	ReasonUnknownClass    = "unknown_class"
	ReasonNoticeReconfirm = "notice_reconfirm"
	// ReasonPlanRequired: the organization's plan does not include the plugin
	// (or it is not yet known to). Nothing is kept.
	ReasonPlanRequired = "plan_required"
)

// Decision is the gate's answer for one record.
type Decision struct {
	Action Action
	Reason string // drop reason, or the first hold reason
}

// Gate is what the projector and daemon call before spooling
// or forwarding a record. It is the only place upload eligibility is
// decided. It decides about uploads to Fleet, never about agent actions.
type Gate interface {
	// Decide classifies one record of class c.
	Decide(c fleetapi.TelemetryClass) Decision
	// ObserveUploadError feeds Fleet errors from the forwarder back in:
	// org_paused latches a pause hold, node_removed triggers withdrawal.
	ObserveUploadError(err error)
	// CountDrop records an on-device drop decided elsewhere ("pii", "ttl",
	// "invalid").
	CountDrop(kind string)
}

// Lanes is what withdrawal acts on. The daemon implements it over its
// lanes and the spool. Purges must complete within 1 s.
type Lanes interface {
	StopAll()
	PurgeAll() error
	PurgeClass(c fleetapi.TelemetryClass) error
}

// ErrLockedDown refuses Fleet write tools (skills_publish, context_promote)
// during an emergency lockdown. Read tools keep working, and the
// agent itself is never blocked.
var ErrLockedDown = errors.New("your organization has an emergency lockdown in effect: sharing to Kameas Fleet is paused")

// CapEmergencyLockdown is the capability key that enables the lockdown
// long-poll (Team+). Lower tiers skip it.
const CapEmergencyLockdown = "emergency_lockdown"

// Options configures a Manager.
type Options struct {
	Provider fleetapi.Provider
	// Env is the resolved Fleet environment. It selects the dashboard that
	// approval links open; when empty, the signed-in session's env is used.
	Env fleetapi.Env
	// Lanes may be nil (CLI use without a daemon).
	Lanes Lanes
	// ProjectionVersion is the projector's projection-table version. A change
	// bumps NoticeVersion and forces re-confirmation.
	ProjectionVersion int
	// Store persists the locally accepted notice version. It may be nil.
	Store *Store
	// SpoolStats reports spool usage for OrgState. It may be nil.
	SpoolStats func() SpoolStats
	Now        func() time.Time
}

// Manager tracks sign-in, opt-ins, approvals, pause and lockdown, and
// implements Gate. It is safe for concurrent use.
type Manager struct {
	o Options

	mu          sync.Mutex
	signedIn    bool
	session     fleetapi.Session
	me          fleetapi.MeInfo
	caps        fleetapi.Capabilities
	optins      map[fleetapi.TelemetryClass]fleetapi.OptInState
	approvals   []fleetapi.Approval
	pauseLatch  bool
	pauseDetail string
	// planRequired: Fleet last refused this org's plugin requests because
	// its plan does not include the plugin (pending approvals, enroll or an
	// upload). Every record is dropped and nothing is kept on the device.
	planRequired bool
	// admittedOrg / admittedAt: the org Fleet last admitted and the moment
	// this device first saw it do so. Only records generated from then on
	// are sent. Zero while the org is not admitted or not yet known to be.
	admittedOrg string
	admittedAt  time.Time
	lockdown    fleetapi.LockdownState
	drops       Drops
}

var _ Gate = (*Manager)(nil)

// NewManager returns a Manager in the signed-out state. Until Refresh
// observes a session and a member opt-in, every record is dropped.
func NewManager(o Options) *Manager {
	if o.Now == nil {
		o.Now = time.Now
	}
	return &Manager{o: o, optins: map[fleetapi.TelemetryClass]fleetapi.OptInState{}}
}

// CapAnalyticsMemberFloor is the capability key Fleet advertises once the
// analytics floor is enforced. Until then the notice uses
// the v1 edition.
const CapAnalyticsMemberFloor = "analytics_member_floor"

// NoticeEdition is the Agent Telemetry Notice edition that is true for the
// connected Fleet and this org's size.
func (m *Manager) NoticeEdition() notices.Edition {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.noticeEditionLocked()
}

func (m *Manager) noticeEditionLocked() notices.Edition {
	return notices.AgentTelemetry(m.caps.Has(CapAnalyticsMemberFloor), m.me.OrgSizeBand)
}

// NoticeVersion is the current notice version (edition + projection).
func (m *Manager) NoticeVersion() string {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.noticeVersionLocked()
}

func (m *Manager) noticeVersionLocked() string {
	return NoticeVersion(m.noticeEditionLocked().ID, m.o.ProjectionVersion)
}

// Refresh re-reads Fleet state. Call it after login, on every
// capability-poll tick (about 5 min) and on SIGHUP.
func (m *Manager) Refresh(ctx context.Context) error {
	p := m.o.Provider
	sess, err := p.Auth().Session(ctx)
	if errors.Is(err, fleetapi.ErrNotSignedIn) {
		m.withdraw("signed out")
		return nil
	}
	if err != nil {
		return err
	}
	if err := m.claimQueue(sess); err != nil {
		return err
	}

	me, err := p.Me().Get(ctx)
	if err != nil {
		if m.membershipLost(err) {
			m.withdraw("membership lost")
			return nil
		}
		m.ObserveUploadError(err)
		return err
	}
	caps, err := p.Me().Capabilities(ctx)
	if err != nil {
		m.ObserveUploadError(err)
		return err
	}
	opt, err := p.OptIns().Get(ctx)
	if err != nil {
		m.ObserveUploadError(err)
		return err
	}
	// While the plan does not include the plugin, Fleet refuses pending-approvals (it
	// still answers /me, capabilities and the opt-ins). Keep refreshing the
	// opt-ins so a withdrawal is applied and purged locally. Nothing
	// is collected or kept until the org is admitted; a successful answer
	// here is an admitted response.
	planRequired := false
	appr, err := p.Pollers().PendingApprovals(ctx)
	if fleetapi.PlanRequired(err) {
		appr, err, planRequired = nil, nil, true
	}
	if err != nil {
		m.ObserveUploadError(err)
		return err
	}

	next := make(map[fleetapi.TelemetryClass]fleetapi.OptInState, len(opt))
	for _, s := range opt {
		next[s.Class] = s
	}

	m.mu.Lock()
	prevOn := onSet(m.optins)
	m.signedIn = true
	m.session = sess
	m.me = me
	m.caps = caps
	m.optins = next
	m.approvals = appr
	// /me is authoritative for pause and keeps answering while paused.
	m.pauseLatch = me.OrgPaused
	m.pauseDetail = me.PausedCategory
	nowOn := onSet(next)
	m.mu.Unlock()

	if planRequired {
		m.notAdmitted()
	} else {
		m.admitted(sess.OrgID)
	}

	// Withdrawal and per-class opt-out.
	if len(prevOn) > 0 && len(nowOn) == 0 {
		m.stopAndPurge()
		return nil
	}
	for c := range prevOn {
		if !nowOn[c] && m.o.Lanes != nil {
			if err := m.o.Lanes.PurgeClass(c); err != nil {
				return fmt.Errorf("consent: purge %s: %w", c, err)
			}
		}
	}
	return nil
}

// OwnerKey identifies the account a device queue belongs to: Fleet env,
// organization and user. It is a hash, so consent.json never gains an
// identifier it did not already hold.
func OwnerKey(s fleetapi.Session) string {
	h := sha256.Sum256([]byte("kenaz/queue-owner/v1\x00" + string(s.Env) + "\x00" + s.OrgID + "\x00" + s.UserID))
	return hex.EncodeToString(h[:16])
}

// claimQueue makes sure nothing queued for another account can be
// sent under sess. When the signed-in account differs from the one this
// manager last saw, or from the queue owner recorded on disk by an earlier
// daemon run, every lane is stopped and purged before the new identity is
// adopted. A purge failure leaves the manager signed out, so
// everything keeps being dropped until a later refresh succeeds. CLI
// managers (no lanes) never touch the queue or its owner.
func (m *Manager) claimQueue(sess fleetapi.Session) error {
	if m.o.Lanes == nil {
		return nil
	}
	key := OwnerKey(sess)
	m.mu.Lock()
	prev, was := m.session, m.signedIn
	m.mu.Unlock()
	switched := was && OwnerKey(prev) != key
	if !switched && m.o.Store != nil {
		switch owner := m.o.Store.QueueOwner(); owner {
		case key:
		case "":
			// First run (the queue is empty, so this costs nothing), or
			// upgraded from a build that did not record the owner. What
			// such a build queued, batches ('b') and held records ('r'),
			// may belong to another account (a `kenaz login` over an
			// existing session did not purge it), so it is purged before
			// the current account is recorded as the owner.
			if err := m.o.Lanes.PurgeAll(); err != nil {
				return fmt.Errorf("consent: purge ownerless queue: %w", err)
			}
			return m.o.Store.SetQueueOwner(key)
		default:
			switched = true
		}
	}
	if !switched {
		return nil
	}
	m.mu.Lock()
	m.signedIn = false
	m.session = fleetapi.Session{}
	m.optins = map[fleetapi.TelemetryClass]fleetapi.OptInState{}
	m.approvals = nil
	m.admittedOrg, m.admittedAt = "", time.Time{}
	m.mu.Unlock()
	m.o.Lanes.StopAll()
	if err := m.o.Lanes.PurgeAll(); err != nil {
		return fmt.Errorf("consent: purge queue after account change: %w", err)
	}
	if m.o.Store != nil {
		if err := m.o.Store.SetQueueOwner(key); err != nil {
			return fmt.Errorf("consent: record queue owner: %w", err)
		}
	}
	return nil
}

// SessionCurrent reports whether the provider's active session is still
// the account this manager last refreshed for. It reads only the local
// session, so the lanes call it before every upload: `kenaz login` as
// another account takes effect at once, not at the next Refresh. On a
// sign-out the lanes are withdrawn; on an account change they are stopped
// and purged (claimQueue) and false is returned until Refresh adopts the
// new account. Any other error also returns false (nothing is sent).
func (m *Manager) SessionCurrent(ctx context.Context) bool {
	sess, err := m.o.Provider.Auth().Session(ctx)
	if errors.Is(err, fleetapi.ErrNotSignedIn) {
		m.withdraw("signed out")
		return false
	}
	if err != nil {
		return false
	}
	m.mu.Lock()
	cur, signedIn := m.session, m.signedIn
	m.mu.Unlock()
	if !signedIn {
		return false
	}
	if OwnerKey(sess) != OwnerKey(cur) {
		_ = m.claimQueue(sess) // purges; a failure leaves it signed out
		return false
	}
	return true
}

// ObserveEnrolled feeds a successful enroll (heartbeat) in: an admitted
// response for the signed-in org.
func (m *Manager) ObserveEnrolled() {
	m.mu.Lock()
	org, ok := m.session.OrgID, m.signedIn
	m.mu.Unlock()
	if ok {
		m.admitted(org)
	}
}

// ObservePlanRequired feeds a plan-required refusal of the enroll
// (heartbeat) in.
func (m *Manager) ObservePlanRequired() { m.notAdmitted() }

// notAdmitted records that Fleet refuses the org: drop everything from now
// on and discard anything already queued on the device.
func (m *Manager) notAdmitted() {
	m.mu.Lock()
	m.planRequired = true
	org := m.admittedOrg
	if org == "" {
		org = m.session.OrgID
	}
	m.admittedOrg, m.admittedAt = "", time.Time{}
	m.mu.Unlock()
	if m.o.Lanes == nil {
		return // CLI: the daemon owns the queue and its record
	}
	_ = m.o.Store.SetAdmitted(org, time.Time{})
	_ = m.o.Lanes.PurgeAll() // best effort; the spool reports its own failure
}

// admitted records an admitted response for org. The first one after a
// refusal, or the first this device has seen for org, is the admission
// moment: everything queued before it is purged before anything is sent,
// and only records generated from then on are sent.
func (m *Manager) admitted(org string) {
	if org == "" {
		return
	}
	now := m.o.Now()
	m.mu.Lock()
	if m.admittedOrg == org && !m.admittedAt.IsZero() {
		m.planRequired = false
		m.mu.Unlock()
		return
	}
	// A refusal seen by this process ends any earlier admission, even if
	// clearing the stored moment failed.
	refused := m.planRequired
	m.mu.Unlock()

	at := time.Time{}
	if m.o.Lanes != nil {
		// A moment recorded earlier by this build still stands: what is
		// queued now was generated after it. A stored moment in the future
		// (the clock was ahead when it was recorded) would hold back
		// everything until then, so it is replaced like a fresh admission.
		at = m.o.Store.AdmittedAt(org)
		if at.IsZero() || refused || at.After(now) {
			if err := m.o.Lanes.PurgeAll(); err != nil {
				return // stay not admitted; retried on the next answer
			}
			at = now
			_ = m.o.Store.SetAdmitted(org, at)
		}
	} else {
		at = now // CLI: display only
	}
	m.mu.Lock()
	m.planRequired = false
	m.admittedOrg, m.admittedAt = org, at
	m.mu.Unlock()
}

// Admission returns the signed-in organization's admission moment; ok is
// false while it is not admitted (or not yet known to be). It reads live
// state, not a snapshot, so a refusal applies to the next check at once.
func (m *Manager) Admission() (since time.Time, ok bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if !m.signedIn || !m.admittedLocked() {
		return time.Time{}, false
	}
	return m.admittedAt, true
}

// admittedLocked reports whether the signed-in org is admitted.
func (m *Manager) admittedLocked() bool {
	return !m.planRequired && !m.admittedAt.IsZero() && m.admittedOrg == m.session.OrgID
}

func onSet(m map[fleetapi.TelemetryClass]fleetapi.OptInState) map[fleetapi.TelemetryClass]bool {
	out := map[fleetapi.TelemetryClass]bool{}
	for c, s := range m {
		if s.OptedIn && s.Available {
			out[c] = true
		}
	}
	return out
}

func (m *Manager) membershipLost(err error) bool {
	var ae *fleetapi.APIError
	if !errors.As(err, &ae) {
		return false
	}
	// The implementation already retried once after refresh, so a 401 here
	// means the refreshed token was rejected.
	return ae.Status == http.StatusUnauthorized || ae.Status == http.StatusNotFound
}

// Withdraw stops all lanes and purges the spool. Call it on logout.
func (m *Manager) Withdraw(reason string) { m.withdraw(reason) }

func (m *Manager) withdraw(string) {
	m.mu.Lock()
	m.signedIn = false
	m.session = fleetapi.Session{}
	m.optins = map[fleetapi.TelemetryClass]fleetapi.OptInState{}
	m.approvals = nil
	m.mu.Unlock()
	m.stopAndPurge()
}

func (m *Manager) stopAndPurge() {
	if m.o.Lanes == nil {
		return
	}
	m.o.Lanes.StopAll()
	_ = m.o.Lanes.PurgeAll() // best effort; the spool reports its own failure
}

// Decide implements Gate. Order: no member sign-in or opt-in → drop
// (signed in and personally opted in); stale notice → drop; holds → hold; otherwise send.
func (m *Manager) Decide(c fleetapi.TelemetryClass) Decision {
	m.mu.Lock()
	defer m.mu.Unlock()
	if !m.signedIn {
		m.drops.OptOut++
		return Decision{ActionDrop, ReasonNotSignedIn}
	}
	if !knownClass(c) {
		m.drops.Invalid++
		return Decision{ActionDrop, ReasonUnknownClass}
	}
	s, ok := m.optins[c]
	if !ok || !s.Available {
		m.drops.OptOut++
		return Decision{ActionDrop, ReasonUnavailable}
	}
	if !s.OptedIn {
		m.drops.OptOut++
		return Decision{ActionDrop, ReasonOptOut}
	}
	if m.reconfirmLocked() {
		m.drops.OptOut++
		return Decision{ActionDrop, ReasonNoticeReconfirm}
	}
	if !m.admittedLocked() {
		m.drops.PlanRequired++
		return Decision{ActionDrop, ReasonPlanRequired}
	}
	if h := m.holdsLocked(); len(h) > 0 {
		return Decision{ActionHold, h[0].Reason}
	}
	return Decision{ActionSend, ""}
}

func knownClass(c fleetapi.TelemetryClass) bool {
	for _, ci := range Classes {
		if ci.Class == c {
			return true
		}
	}
	return false
}

// reconfirmLocked reports whether any opted-in class was accepted under a
// notice version that does not stand for the current edition (see
// notices.AgentTelemetryAccepted). Fleet's recorded version wins. Before notice_version
// ships, Fleet returns "", so the locally stored acceptance is used.
func (m *Manager) reconfirmLocked() bool {
	accepted := map[string]bool{}
	for _, id := range notices.AgentTelemetryAccepted(m.noticeEditionLocked().ID) {
		accepted[NoticeVersion(id, m.o.ProjectionVersion)] = true
	}
	local := ""
	if m.o.Store != nil {
		local = m.o.Store.Accepted(m.session.UserID)
	}
	for _, s := range m.optins {
		if !s.OptedIn || !s.Available {
			continue
		}
		got := s.NoticeVersion
		if got == "" {
			got = local
		}
		if !accepted[got] {
			return true
		}
	}
	return false
}

func (m *Manager) holdsLocked() []Hold {
	var h []Hold
	now := m.o.Now()
	for _, a := range m.approvals {
		if ClassifyApproval(a, now) == EffectHold {
			h = append(h, Hold{HoldApproval, sanitize(a.Title, 120)})
		}
	}
	if m.pauseLatch {
		h = append(h, Hold{HoldPaused, sanitize(m.pauseDetail, 60)})
	}
	if m.lockdown.Locked {
		h = append(h, Hold{HoldLockdown, sanitize(m.lockdown.Reason, 200)})
	}
	return h
}

// ObserveUploadError implements Gate.
func (m *Manager) ObserveUploadError(err error) {
	if err == nil {
		return
	}
	if cat, ok := fleetapi.OrgPaused(err); ok {
		m.mu.Lock()
		m.pauseLatch = true
		m.pauseDetail = cat
		m.mu.Unlock()
		return
	}
	if fleetapi.PlanRequired(err) {
		m.notAdmitted()
		return
	}
	var ae *fleetapi.APIError
	if errors.As(err, &ae) && ae.Code == fleetapi.CodeNodeRemoved {
		m.withdraw("node removed")
	}
}

// UploadSucceeded clears a pause latch after a successful non-exempt call.
func (m *Manager) UploadSucceeded() {
	m.mu.Lock()
	m.pauseLatch = false
	m.pauseDetail = ""
	m.mu.Unlock()
}

// CountDrop implements Gate.
func (m *Manager) CountDrop(kind string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	switch kind {
	case "pii":
		m.drops.PII++
	case "ttl":
		m.drops.TTL++
	case "optout":
		m.drops.OptOut++
	default:
		m.drops.Invalid++
	}
}

// SetLockdown records the lockdown state. It holds uploads and refuses
// Fleet writes. It never touches the harness or its config.
func (m *Manager) SetLockdown(s fleetapi.LockdownState) {
	m.mu.Lock()
	m.lockdown = s
	m.mu.Unlock()
}

// WriteAllowed returns ErrLockedDown during a lockdown. Write-type MCP
// tools call it first.
func (m *Manager) WriteAllowed() error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.lockdown.Locked {
		return ErrLockedDown
	}
	return nil
}

// RunLockdownWatch long-polls lockdown state until ctx ends. Orgs without
// CapEmergencyLockdown skip it (returns nil immediately).
func (m *Manager) RunLockdownWatch(ctx context.Context) error {
	m.mu.Lock()
	enabled := m.caps.Has(CapEmergencyLockdown)
	m.mu.Unlock()
	if !enabled {
		return nil
	}
	pl := m.o.Provider.Pollers()
	if s, err := pl.LockdownStatus(ctx); err == nil {
		m.SetLockdown(s)
	}
	backoff := time.Second
	for ctx.Err() == nil {
		s, changed, err := pl.LockdownWait(ctx)
		if err != nil {
			var wait time.Duration
			wait, backoff = m.lockdownWaitFailed(err, backoff)
			select {
			case <-ctx.Done():
			case <-time.After(wait):
			}
			continue
		}
		backoff = time.Second
		if changed {
			m.SetLockdown(s)
		}
	}
	return nil
}

// lockdownWaitMaxBackoff caps the wait between failed lockdown polls.
const lockdownWaitMaxBackoff = time.Minute

// lockdownWaitFailed handles a failed lockdown poll and returns how long
// to wait before the next one and the backoff after that. A per-feature tier limit (the plan includes
// the plugin but not the lockdown feature) is not a plan-required refusal:
// it ends no admission and purges nothing; the watch keeps polling at the
// maximum backoff. Every other error is observed as an upload error.
func (m *Manager) lockdownWaitFailed(err error, backoff time.Duration) (wait, next time.Duration) {
	if _, ok := fleetapi.TierLimit(err); ok {
		return lockdownWaitMaxBackoff, lockdownWaitMaxBackoff
	}
	m.ObserveUploadError(err)
	return backoff, min(backoff*2, lockdownWaitMaxBackoff)
}

// SetOptIns writes the member's own selection. It is called only
// from an interactive member action (first-run or `kenaz optin`).
// No code path derives it from managed configuration. Opting in
// records the notice version the member just saw.
func (m *Manager) SetOptIns(ctx context.Context, sel map[fleetapi.TelemetryClass]bool) error {
	m.mu.Lock()
	signedIn := m.signedIn
	userID := m.session.UserID
	states := m.optins
	planRequired := m.planRequired
	m.mu.Unlock()
	if !signedIn {
		return fleetapi.ErrNotSignedIn
	}
	req := fleetapi.OptInPut{}
	anyOn := false
	for _, ci := range Classes {
		on, ok := sel[ci.Class]
		if !ok {
			continue
		}
		if on && !states[ci.Class].Available {
			return fmt.Errorf("consent: %s is not available on your plan", ci.Label)
		}
		req.Updates = append(req.Updates, fleetapi.OptInUpdate{Class: ci.Class, OptedIn: on})
		anyOn = anyOn || on
	}
	if len(req.Updates) == 0 {
		return nil
	}
	// While the plan does not include the plugin, a member may opt out (always) but not in.
	if anyOn && planRequired {
		return &fleetapi.APIError{Status: http.StatusForbidden, Code: fleetapi.CodeCapabilityNotInTier, Message: fleetapi.PlanRequiredMessage, Details: map[string]string{"capability": fleetapi.PluginCapability}}
	}
	if anyOn {
		req.NoticeVersion = m.NoticeVersion()
	}
	if err := m.o.Provider.OptIns().Put(ctx, req); err != nil {
		return err
	}
	if anyOn && m.o.Store != nil {
		if err := m.o.Store.SetAccepted(userID, req.NoticeVersion); err != nil {
			return err
		}
	}
	return m.Refresh(ctx)
}

// State builds the OrgState snapshot.
func (m *Manager) State() OrgState {
	m.mu.Lock()
	defer m.mu.Unlock()
	st := OrgState{
		SignedIn: m.signedIn,
		Classes:  map[string]string{},
		Holds:    []Hold{},
		Pending:  []Pending{},
		Drops:    m.drops,
	}
	if m.o.SpoolStats != nil {
		st.Spool = m.o.SpoolStats()
	}
	for _, ci := range Classes {
		st.Classes[string(ci.Class)] = ClassOff
	}
	env := m.o.Env
	if env == "" {
		env = m.session.Env
	}
	st.DashboardURL = DashboardURL(env, "")
	if !m.signedIn {
		return st
	}
	st.Env = string(m.session.Env)
	st.OrgDisplay = m.me.OrgName
	if st.OrgDisplay == "" {
		st.OrgDisplay = m.me.OrgID
	}
	st.Tier = m.me.Tier
	for _, ci := range Classes {
		s, ok := m.optins[ci.Class]
		switch {
		case !ok || !s.Available:
			st.Classes[string(ci.Class)] = ClassUnavailable
		case s.OptedIn:
			st.Classes[string(ci.Class)] = ClassOn
		}
		if ok && s.AdminSuggested && s.Available {
			st.AdminSuggested = append(st.AdminSuggested, string(ci.Class))
		}
	}
	st.NoticeReconfirm = m.reconfirmLocked()
	st.PlanRequired = m.planRequired
	if m.admittedLocked() {
		st.AdmittedAt = m.admittedAt
	}
	st.Holds = append(st.Holds, m.holdsLocked()...)
	for _, a := range m.approvals {
		st.Pending = append(st.Pending, Pending{ID: a.ID, Kind: a.Kind, Title: sanitize(a.Title, 120), Blocking: a.Blocking, URL: ApprovalURL(env, a)})
	}
	return st
}

// OptInStates returns the cached per-class states in display order.
func (m *Manager) OptInStates() []fleetapi.OptInState {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]fleetapi.OptInState, 0, len(Classes))
	for _, ci := range Classes {
		s, ok := m.optins[ci.Class]
		if !ok {
			s = fleetapi.OptInState{Class: ci.Class}
		}
		out = append(out, s)
	}
	return out
}

// Me returns the cached /me (zero value when signed out).
func (m *Manager) Me() fleetapi.MeInfo {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.me
}

// sanitize strips control characters and truncates server-supplied text
// before it reaches a terminal, a banner or the model.
func sanitize(s string, max int) string {
	s = strings.Map(func(r rune) rune {
		if unicode.IsControl(r) {
			return ' '
		}
		return r
	}, s)
	s = strings.TrimSpace(s)
	if r := []rune(s); len(r) > max {
		s = string(r[:max-1]) + "…"
	}
	return s
}

// Session returns the last observed sign-in session. Its UserID/OrgID are
// the token claims Fleet checks against kameas.user.id / kameas.org.id.
func (m *Manager) Session() (fleetapi.Session, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.session, m.signedIn
}

// Caps returns the last observed capabilities (schema revision, tier keys).
func (m *Manager) Caps() fleetapi.Capabilities {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.caps
}
