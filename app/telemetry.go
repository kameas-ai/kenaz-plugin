package app

import (
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/kameas-ai/kenaz-plugin/fleetapi"
	"github.com/kameas-ai/kenaz-plugin/internal/canon"
	"github.com/kameas-ai/kenaz-plugin/internal/consent"
	"github.com/kameas-ai/kenaz-plugin/internal/daemon"
	"github.com/kameas-ai/kenaz-plugin/internal/decode"
	"github.com/kameas-ai/kenaz-plugin/internal/enroll"
	"github.com/kameas-ai/kenaz-plugin/internal/fleetschema"
	"github.com/kameas-ai/kenaz-plugin/internal/forward"
	"github.com/kameas-ai/kenaz-plugin/internal/gate"
	"github.com/kameas-ai/kenaz-plugin/internal/otlprx"
	"github.com/kameas-ai/kenaz-plugin/internal/pipeline"
	"github.com/kameas-ai/kenaz-plugin/internal/project"
	"github.com/kameas-ai/kenaz-plugin/internal/session"
	"github.com/kameas-ai/kenaz-plugin/internal/spool"
)

// telemetryLaneName is the spool lane and status name for telemetry.
const telemetryLaneName = "telemetry"

// Spool record tags inside the telemetry lane. Earlier builds wrote 'B'
// and 'R' records, which could hold data Fleet refused for an organization
// whose plan did not include the plugin; those are discarded unsent.
const (
	// tagBatch: u64be(earliest event unix nanos) || JSON fleetapi.ProjectedBatch,
	// written by earlier builds without an owner: dropped unsent.
	tagBatch byte = 'b'
	// tagHeld: JSON []project.Record held by a consent hold; re-gated on drain
	tagHeld byte = 'r'
	// tagOwnedBatch: u64be(earliest event unix nanos) || JSON ownedBatch,
	// a batch with the account (ExpectedSession) that stamped it.
	tagOwnedBatch byte = 'o'

	tagLegacyBatch byte = 'B'
	tagLegacyHeld  byte = 'R'
)

// laneNow is the lane's clock for the admission moment (tests override it).
var laneNow = time.Now

// queued is a batch with the earliest event time among its records and
// the account whose session stamped it. The owner travels with the batch
// through the queue and the spool, is re-checked after the forwarder's
// rate-limit wait, and is attached to the Post as the ExpectedSession.
type queued struct {
	b        fleetapi.ProjectedBatch
	earliest time.Time
	owner    fleetapi.ExpectedSession
}

// ownedBatch is the spooled form of a queued batch.
type ownedBatch struct {
	Owner fleetapi.ExpectedSession `json:"owner"`
	Batch fleetapi.ProjectedBatch  `json:"batch"`
}

// Telemetry lane timing. Variables so tests can shorten them.
var (
	consentRefreshEvery = 5 * time.Minute
	sessionTickEvery    = time.Minute
	drainEvery          = 30 * time.Second
)

// telemetryHook wires the receiver and pipeline, the consent gate
// (lockdown, withdrawal) and the spool and daemon lifecycle into
// one daemon lane. It is the only place these packages meet.
func telemetryHook(env Env) DaemonHook {
	return func(opt *daemon.Options) (func(*daemon.Daemon), error) {
		l, err := newTelemetryLane(env, opt)
		if err != nil {
			return nil, err
		}
		opt.Lanes = append(opt.Lanes, l)
		opt.Contributors = append(opt.Contributors, l)
		// A kick (sent by `kenaz login`) re-reads the session at once.
		opt.KickObservers = append(opt.KickObservers, l.requestRefresh)
		return l.mount, nil
	}
}

// telemetryLane implements daemon.Lane and consent.Lanes.
type telemetryLane struct {
	log     *slog.Logger
	sp      *spool.Spool
	cm      *consent.Manager
	dec     *consentDecider
	pipe    *pipeline.Pipeline
	rx      *otlprx.Receiver
	fwd     *forward.Forwarder
	nodeID  string
	version string

	queue   chan queued
	wake    chan struct{}
	refresh chan struct{} // re-read consent and session now

	// spMu orders spool appends against purges: an append checks
	// admission and writes under it, and a purge runs under it, so nothing
	// checked before a refusal can land on disk after the refusal's purge.
	spMu sync.Mutex

	mu       sync.Mutex
	hold     string // lane-level hold (withdrawal, daemon request)
	stopped  bool
	cancel   context.CancelFunc
	wg       sync.WaitGroup
	lockdown atomic.Bool

	uploaded, held, dropped atomic.Int64

	// Delivery outcome for status: the last accepted upload and the last
	// failure category (never a server message or payload).
	outMu      sync.Mutex
	lastUpload time.Time
	lastErr    string
	lastErrAt  time.Time
}

var _ daemon.Lane = (*telemetryLane)(nil)
var _ consent.Lanes = (*telemetryLane)(nil)
var _ daemon.StatusContributor = (*telemetryLane)(nil)

func newTelemetryLane(env Env, opt *daemon.Options) (*telemetryLane, error) {
	if opt.Spool == nil {
		return nil, errors.New("telemetry lane: no spool")
	}
	key, err := spool.GetOrCreateKey(env.keyStore(), string(opt.Env))
	if err != nil {
		return nil, err
	}
	nodeID, err := enroll.LoadOrCreateNodeID(opt.StateDir, time.Now())
	if err != nil {
		return nil, err
	}
	log := opt.Logger
	if log == nil {
		log = slog.New(slog.NewTextHandler(io.Discard, nil))
	}
	l := &telemetryLane{
		log:     log,
		sp:      opt.Spool,
		nodeID:  nodeID,
		version: opt.Version,
		queue:   make(chan queued, 64),
		wake:    make(chan struct{}, 1),
		refresh: make(chan struct{}, 1),
		fwd:     &forward.Forwarder{Poster: opt.Provider.OTLP(), PerMinute: 100},
	}
	l.fwd.Current = l.ownerIs
	l.cm = consent.NewManager(consent.Options{
		Provider:          opt.Provider,
		Env:               opt.Env,
		Lanes:             l,
		ProjectionVersion: project.TableVersion,
		Store:             consent.NewStore(filepath.Join(opt.StateDir, "consent.json")),
		SpoolStats: func() consent.SpoolStats {
			return consent.SpoolStats{Bytes: opt.Spool.Stats().Bytes}
		},
		Now: func() time.Time { return laneNow() },
	})
	// Enroll outcomes are admission signals too: enrolled is an admitted
	// response, plan_required a refusal.
	opt.EnrollObservers = append(opt.EnrollObservers, func(s enroll.State) {
		switch s {
		case enroll.StateEnrolled:
			l.cm.ObserveEnrolled()
		case enroll.StatePlanRequired:
			l.cm.ObservePlanRequired()
		default:
			return
		}
		l.dec.invalidate()
		l.kick()
	})
	l.dec = &consentDecider{m: l.cm}
	l.pipe = pipeline.New(pipeline.Config{
		Decoder: decode.Decoder{},
		Aggregator: session.New(session.Config{
			// The session-hash key is derived from the keychain-held spool
			// key, so it rotates when logout deletes that key.
			Key: sessionKey(key),
		}),
		Decider: l.dec,
		Options: func() project.Options {
			return project.Options{SchemaRev: l.cm.Caps().TelemetrySchema}
		},
		Identity: l.identity,
		Emit:     l.emit,
		Hold:     l.holdRecords,
	})
	// The receiver's own session-end endpoint stays disabled (empty token): the
	// SessionEnd hook goes through the daemon's nonce-bearer control API.
	l.rx = otlprx.New(otlprx.Config{Sink: l.pipe})
	return l, nil
}

func sessionKey(spoolKey []byte) []byte {
	h := sha256.New()
	h.Write([]byte("kameas-fleet/session-hash/v1\x00"))
	h.Write(spoolKey)
	return h.Sum(nil)
}

// mount registers the OTLP ingest routes and the session-end control route.
func (l *telemetryLane) mount(d *daemon.Daemon) {
	for _, p := range []string{"/v1/logs", "/v1/traces", "/v1/metrics"} {
		d.Handle("POST "+p, d.RequireSession(l.rx))
	}
	d.HandleControl("POST "+daemon.SessionEndPath, l.handleSessionEnd)
}

func (l *telemetryLane) handleSessionEnd(w http.ResponseWriter, r *http.Request) {
	var req struct {
		ClientKind  string `json:"client_kind"`
		SessionHash string `json:"session_hash"`
	}
	dec := json.NewDecoder(io.LimitReader(r.Body, 4096))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&req); err != nil {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	kind := canon.ClientKind(req.ClientKind)
	if !kind.Valid() || len(req.SessionHash) != 32 {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	l.pipe.SessionEndLocal(kind, req.SessionHash)
	w.WriteHeader(http.StatusNoContent)
}

func (l *telemetryLane) identity() (project.Identity, bool) {
	sess, ok := l.cm.Session()
	if !ok || sess.UserID == "" || sess.OrgID == "" {
		return project.Identity{}, false
	}
	return project.Identity{
		Env:            sess.Env,
		UserID:         sess.UserID,
		OrgID:          sess.OrgID,
		MachineID:      l.nodeID,
		Tier:           l.cm.Me().Tier,
		ServiceVersion: l.version,
	}, true
}

// --- daemon.Lane ---

func (l *telemetryLane) Name() string { return telemetryLaneName }

func (l *telemetryLane) Start(ctx context.Context) error {
	ctx, cancel := context.WithCancel(ctx)
	l.mu.Lock()
	l.cancel, l.stopped = cancel, false
	l.mu.Unlock()

	l.wg.Add(3)
	go func() { defer l.wg.Done(); l.runConsent(ctx) }()
	go func() { defer l.wg.Done(); l.runTicks(ctx) }()
	go func() { defer l.wg.Done(); l.runSender(ctx) }()
	return nil
}

func (l *telemetryLane) Stop() error {
	l.mu.Lock()
	cancel := l.cancel
	l.stopped = true
	l.mu.Unlock()
	if cancel != nil {
		cancel()
	}
	l.wg.Wait()
	// Flush anything still queued in memory to the spool.
	for {
		select {
		case q := <-l.queue:
			l.spoolBatch(q)
		default:
			return nil
		}
	}
}

func (l *telemetryLane) Hold(reason string) {
	l.mu.Lock()
	l.hold = reason
	l.mu.Unlock()
}

func (l *telemetryLane) Resume() {
	l.mu.Lock()
	l.hold = ""
	l.mu.Unlock()
	l.kick()
}

func (l *telemetryLane) Purge() error {
	l.spMu.Lock()
	defer l.spMu.Unlock()
	// Open sessions are aggregated in memory; drop them too so nothing
	// collected before the purge is sent later as part of a summary.
	l.pipe.Reset()
	for {
		select {
		case <-l.queue:
		default:
			return l.sp.Purge()
		}
	}
}

func (l *telemetryLane) Status() daemon.LaneStatus {
	st := daemon.LaneStatus{
		Name:     telemetryLaneName,
		State:    "running",
		Uploaded: l.uploaded.Load(),
		Held:     l.held.Load(),
		Dropped:  l.dropped.Load(),
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	switch {
	case l.stopped:
		st.State = "stopped"
	case l.hold != "":
		st.State, st.Reason = "held", l.hold
	default:
		if r := l.dec.HoldReason(); r != "" {
			st.State, st.Reason = "held", r
		}
	}
	return st
}

// --- consent.Lanes ---

// StopAll holds uploads after withdrawal; the daemon keeps running so a
// later opt-in resumes without a restart.
func (l *telemetryLane) StopAll() { l.Hold("withdrawn") }

// PurgeAll drops everything queued for upload (within 1 s).
func (l *telemetryLane) PurgeAll() error { return l.Purge() }

// PurgeClass drops queued data for an opted-out class. Spooled batches mix
// classes, so this purges the whole lane: a superset that never keeps data
// the member withdrew (the conservative choice).
func (l *telemetryLane) PurgeClass(fleetapi.TelemetryClass) error { return l.Purge() }

// --- loops ---

func (l *telemetryLane) runConsent(ctx context.Context) {
	hup := make(chan os.Signal, 1)
	signal.Notify(hup, syscall.SIGHUP)
	defer signal.Stop(hup)
	t := time.NewTicker(consentRefreshEvery)
	defer t.Stop()
	for {
		if err := l.cm.Refresh(ctx); err != nil && ctx.Err() == nil {
			l.log.Warn("consent refresh failed", "err", err.Error())
		}
		l.dec.invalidate()
		l.ensureLockdownWatch(ctx)
		if st := l.cm.State(); st.SignedIn && st.AnyOn() {
			l.mu.Lock()
			if l.hold == "withdrawn" {
				l.hold = ""
			}
			l.mu.Unlock()
		}
		l.kick()
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		case <-hup:
		case <-l.refresh:
		}
	}
}

// requestRefresh asks runConsent to refresh now (non-blocking).
func (l *telemetryLane) requestRefresh() {
	select {
	case l.refresh <- struct{}{}:
	default:
	}
}

// ownerCurrent is checked immediately before every upload: the batch was
// stamped for the account the consent manager last refreshed for, and that
// must still be the signed-in account. On a mismatch the manager purges
// the lane and a refresh adopts the new account.
func (l *telemetryLane) ownerCurrent(ctx context.Context) bool {
	if l.cm.SessionCurrent(ctx) {
		return true
	}
	l.dec.invalidate()
	l.requestRefresh()
	return false
}

// ownerIs reports whether owner is the signed-in account right now (the
// forwarder calls it after its rate-limit wait, just before posting).
func (l *telemetryLane) ownerIs(ctx context.Context, owner fleetapi.ExpectedSession) bool {
	if !l.ownerCurrent(ctx) {
		return false
	}
	sess, ok := l.cm.Session()
	return ok && owner.Matches(sess)
}

// ensureLockdownWatch starts the consent long-poll once the org has the
// capability; RunLockdownWatch returns immediately for lower tiers.
func (l *telemetryLane) ensureLockdownWatch(ctx context.Context) {
	if !l.cm.Caps().Has(consent.CapEmergencyLockdown) || !l.lockdown.CompareAndSwap(false, true) {
		return
	}
	l.wg.Add(1)
	go func() {
		defer l.wg.Done()
		defer l.lockdown.Store(false)
		_ = l.cm.RunLockdownWatch(ctx)
	}()
}

func (l *telemetryLane) runTicks(ctx context.Context) {
	t := time.NewTicker(sessionTickEvery)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			l.pipe.Tick()
		}
	}
}

func (l *telemetryLane) runSender(ctx context.Context) {
	t := time.NewTicker(drainEvery)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case q := <-l.queue:
			l.send(ctx, q)
		case <-l.wake:
			l.drain(ctx)
		case <-t.C:
			l.drain(ctx)
		}
	}
}

func (l *telemetryLane) kick() {
	select {
	case l.wake <- struct{}{}:
	default:
	}
}

func (l *telemetryLane) laneHeld() bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.hold != ""
}

// uploadHeld reports whether nothing may be uploaded right now: a
// lane-level hold, or a consent hold (lockdown, org pause, blocking
// approval). It is checked before every upload, not only when a batch is
// gated, because batches can wait in the queue or the spool (and the
// forwarder can sleep for rate tokens) while a lockdown arrives (lockdown stops uploads).
func (l *telemetryLane) uploadHeld() bool {
	return l.laneHeld() || l.dec.HoldReason() != ""
}

// admittedSince returns the organization's admission moment; ok is false
// while it is not admitted (or not yet known to be).
func (l *telemetryLane) admittedSince() (time.Time, bool) { return l.dec.AdmittedSince() }

// sendable reports whether a batch may be uploaded or kept: the
// organization is admitted and every event in it was generated after the
// admission moment. Anything else is dropped, never kept.
func (l *telemetryLane) sendable(earliest time.Time) bool {
	since, ok := l.admittedSince()
	return ok && !earliest.IsZero() && !earliest.Before(since)
}

// emit receives final batches from the pipeline; it never blocks.
func (l *telemetryLane) emit(bs []fleetapi.ProjectedBatch, earliest time.Time, id project.Identity) {
	owner := fleetapi.ExpectedSession{Env: id.Env, OrgID: id.OrgID, Subject: id.UserID}
	for _, b := range bs {
		q := queued{b, earliest, owner}
		select {
		case l.queue <- q:
		default:
			l.spoolBatch(q)
		}
	}
}

// send uploads one batch, spooling it on hold or transient failure. While
// the organization is not admitted it is dropped, not kept.
func (l *telemetryLane) send(ctx context.Context, q queued) bool {
	b := q.b
	if !l.sendable(q.earliest) {
		l.dropped.Add(int64(b.Records))
		return true
	}
	if l.uploadHeld() {
		l.spoolBatch(q)
		return false
	}
	if !l.ownerCurrent(ctx) {
		// Stamped for an account that is no longer signed in (the lane
		// was purged), or the session cannot be read: never upload it.
		l.dropped.Add(int64(b.Records))
		return true
	}
	res := l.fwd.Send(ctx, q.owner, b)
	l.noteResult(ctx, res)
	switch res.Disposition {
	case forward.Sent:
		l.uploaded.Add(int64(b.Records))
		l.cm.UploadSucceeded()
		return true
	case forward.Stale:
		// The account changed while the batch waited for a rate token:
		// nothing was sent, and it is never sent as another account.
		l.dropped.Add(int64(b.Records))
		l.dec.invalidate()
		l.requestRefresh()
		return true
	case forward.Quarantine:
		l.dropped.Add(int64(b.Records))
		l.cm.ObserveUploadError(res.Err)
		l.cm.CountDrop("invalid")
		return true // nothing to retry
	case forward.Refused:
		// Plan does not include the plugin: drop it and everything queued (the consent
		// manager purges the lane).
		l.dropped.Add(int64(b.Records))
		l.cm.ObserveUploadError(res.Err)
		l.dec.invalidate()
		return true
	default: // Hold (org paused) or Retry
		if res.Err != nil {
			l.cm.ObserveUploadError(res.Err)
		}
		l.dec.invalidate()
		if !l.sendable(q.earliest) {
			l.dropped.Add(int64(b.Records))
			return true
		}
		l.spoolBatch(q)
		return false
	}
}

// spoolBatch keeps a batch for a later retry. Only batches the
// organization may send are kept.
func (l *telemetryLane) spoolBatch(q queued) {
	b := q.b
	l.spMu.Lock()
	defer l.spMu.Unlock()
	if !l.sendable(q.earliest) {
		l.dropped.Add(int64(b.Records))
		return
	}
	payload, err := json.Marshal(ownedBatch{Owner: q.owner, Batch: b})
	if err != nil {
		l.dropped.Add(int64(b.Records))
		return
	}
	rec := make([]byte, 9, 9+len(payload))
	rec[0] = tagOwnedBatch
	binary.BigEndian.PutUint64(rec[1:], uint64(q.earliest.UnixNano()))
	rec = append(rec, payload...)
	if err := l.sp.Append(telemetryLaneName, rec); err != nil {
		l.dropped.Add(int64(b.Records))
		l.log.Warn("spool append failed", "err", err.Error())
	}
}

// holdRecords keeps records held by a consent hold (pause, lockdown,
// blocking approval) until it clears; drain re-gates them.
func (l *telemetryLane) holdRecords(recs []project.Record, _ string) {
	l.spMu.Lock()
	defer l.spMu.Unlock()
	if since, ok := l.admittedSince(); !ok || gate.Earliest(recs).Before(since) {
		l.dropped.Add(int64(len(recs))) // not admitted: nothing is kept
		return
	}
	payload, err := json.Marshal(recs)
	if err != nil {
		l.dropped.Add(int64(len(recs)))
		return
	}
	if err := l.sp.Append(telemetryLaneName, append([]byte{tagHeld}, payload...)); err != nil {
		l.dropped.Add(int64(len(recs)))
		return
	}
	l.held.Add(int64(len(recs)))
}

var errStopDrain = errors.New("stop drain")

// drain replays spooled batches and held records once nothing holds. It
// does nothing while the organization is not admitted.
func (l *telemetryLane) drain(ctx context.Context) {
	if _, ok := l.admittedSince(); !ok || l.uploadHeld() || l.sp.Empty() {
		return
	}
	_, err := l.sp.Drain(func(r spool.Record) error {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if r.Lane != telemetryLaneName || len(r.Payload) < 2 {
			return nil
		}
		switch r.Payload[0] {
		case tagLegacyBatch, tagLegacyHeld, tagBatch:
			// From an earlier build, with no owner stamp. Whose batch it is
			// can't be known: earlier builds recorded no queue owner (the
			// first claim adopts whoever is signed in) and a `kenaz login`
			// over an existing session did not purge the spool, so it may
			// belong to another account. Never sent.
			l.dropped.Add(1)
			return nil
		case tagOwnedBatch:
			if len(r.Payload) < 10 {
				return nil
			}
			if _, ok := l.admittedSince(); !ok || l.uploadHeld() {
				return errStopDrain // a refusal or hold arrived mid-drain
			}
			var ob ownedBatch
			if json.Unmarshal(r.Payload[9:], &ob) != nil {
				return nil
			}
			b, owner := ob.Batch, ob.Owner
			earliest := time.Unix(0, int64(binary.BigEndian.Uint64(r.Payload[1:9])))
			if !l.sendable(earliest) {
				l.dropped.Add(int64(b.Records)) // generated before admission
				return nil
			}
			if !l.ownerCurrent(ctx) {
				return errStopDrain // purged, or the session is unreadable
			}
			if !l.sendDirect(ctx, owner, b) {
				return errStopDrain // keep the segment; retry later
			}
		case tagHeld:
			var recs []project.Record
			if json.Unmarshal(r.Payload[1:], &recs) != nil {
				return nil
			}
			l.held.Add(-int64(len(recs)))
			l.pipe.Regate(recs) // re-checks consent; may hold again
		}
		return nil
	})
	if err != nil && !errors.Is(err, errStopDrain) && ctx.Err() == nil {
		l.log.Warn("spool drain failed", "err", err.Error())
	}
}

// sendDirect is send without re-spooling (the record is still in the
// segment being drained).
func (l *telemetryLane) sendDirect(ctx context.Context, owner fleetapi.ExpectedSession, b fleetapi.ProjectedBatch) bool {
	res := l.fwd.Send(ctx, owner, b)
	l.noteResult(ctx, res)
	switch res.Disposition {
	case forward.Sent:
		l.uploaded.Add(int64(b.Records))
		l.cm.UploadSucceeded()
		return true
	case forward.Stale:
		// Owned by an account that is not the signed-in one: never sent,
		// dropped (an owner change purges the lane's spool anyway).
		l.dropped.Add(int64(b.Records))
		l.dec.invalidate()
		l.requestRefresh()
		return true
	case forward.Quarantine:
		l.dropped.Add(int64(b.Records))
		l.cm.ObserveUploadError(res.Err)
		l.cm.CountDrop("invalid")
		return true
	case forward.Refused:
		l.dropped.Add(int64(b.Records))
		l.cm.ObserveUploadError(res.Err) // purges the spool
		l.dec.invalidate()
		return false
	default:
		if res.Err != nil {
			l.cm.ObserveUploadError(res.Err)
		}
		return false
	}
}

// consentDecider adapts consent.Manager to the pipeline's
// gate.Decider. It reads one cached OrgState snapshot (refreshed at most
// once a second or after each consent refresh), so the gate never does I/O.
type consentDecider struct {
	m *consent.Manager

	mu   sync.Mutex
	at   time.Time
	snap consent.OrgState
}

func (d *consentDecider) invalidate() {
	d.mu.Lock()
	d.at = time.Time{}
	d.mu.Unlock()
}

func (d *consentDecider) state() consent.OrgState {
	d.mu.Lock()
	defer d.mu.Unlock()
	if time.Since(d.at) > time.Second {
		d.snap, d.at = d.m.State(), time.Now()
	}
	return d.snap
}

// SignedIn: a member session exists AND they personally opted in to at
// least one class under the current notice.
func (d *consentDecider) SignedIn() bool {
	s := d.state()
	return s.SignedIn && s.AnyOn() && !s.NoticeReconfirm
}

func (d *consentDecider) HoldReason() string {
	s := d.state()
	if len(s.Holds) > 0 {
		return s.Holds[0].Reason
	}
	return ""
}

// AdmittedSince implements gate.Admission. It reads the manager's live
// state rather than the cached snapshot, so a refusal stops collection,
// spooling and sending at once.
func (d *consentDecider) AdmittedSince() (time.Time, bool) { return d.m.Admission() }

func (d *consentDecider) TierAllows(c fleetschema.Class) bool {
	v, ok := d.state().Classes[string(c)]
	return ok && v != consent.ClassUnavailable
}

func (d *consentDecider) OptedIn(c fleetschema.Class) bool {
	return d.state().Classes[string(c)] == consent.ClassOn
}

// noteResult records an upload outcome for status.
func (l *telemetryLane) noteResult(ctx context.Context, res forward.Result) {
	if res.Disposition == forward.Retry && ctx.Err() != nil {
		return // shutting down: not a delivery failure
	}
	now := laneNow()
	l.outMu.Lock()
	defer l.outMu.Unlock()
	if res.Disposition == forward.Sent {
		l.lastUpload = now
		return
	}
	l.lastErr, l.lastErrAt = uploadErrCategory(res), now
}

// uploadErrCategory maps a failed upload to a fixed category.
func uploadErrCategory(res forward.Result) string {
	switch res.Disposition {
	case forward.Hold:
		return daemon.ErrCatOrgPaused
	case forward.Refused:
		return daemon.ErrCatPlanRequired
	case forward.Quarantine:
		return daemon.ErrCatRejected
	}
	var ae *fleetapi.APIError
	if errors.As(res.Err, &ae) {
		switch {
		case ae.Status == http.StatusUnauthorized || ae.Status == http.StatusForbidden:
			return daemon.ErrCatAuth
		case ae.Status == http.StatusTooManyRequests:
			return daemon.ErrCatRateLimited
		case ae.Status >= 500:
			return daemon.ErrCatServer
		case ae.Status >= 400:
			return daemon.ErrCatRejected
		}
	}
	return daemon.ErrCatNetwork
}

// ContributeStatus adds the reporting summary, the pending approval titles
// and the dashboard link to status and fleet_status. It reads local state
// only.
func (l *telemetryLane) ContributeStatus(s *daemon.Status) {
	st := l.cm.State()
	s.DashboardURL = st.DashboardURL
	for _, p := range st.Pending {
		s.PendingApprovals = append(s.PendingApprovals, p.Title)
	}
	r := &daemon.Reporting{
		SignedIn:     s.Session == daemon.SessionSignedIn,
		ReceiverPort: s.Daemon.Port,
		Queued:       int64(l.sp.Stats().Records) + int64(len(l.queue)),
		Consent:      daemon.ConsentNone,
	}
	l.outMu.Lock()
	lastUpload, lastErr, lastErrAt := l.lastUpload, l.lastErr, l.lastErrAt
	l.outMu.Unlock()
	if !lastUpload.IsZero() {
		t := lastUpload.UTC()
		r.LastUploadAt = &t
	}
	if lastErr != "" {
		t := lastErrAt.UTC()
		r.LastError, r.LastErrorAt = lastErr, &t
	}
	switch {
	case !st.SignedIn || !r.SignedIn:
		r.Consent = daemon.ConsentUnknown
	case st.NoticeReconfirm:
		r.Consent = daemon.ConsentReconfirm
	case st.AnyOn():
		r.Consent = daemon.ConsentGiven
	}
	hold := ""
	if len(st.Holds) > 0 {
		hold = st.Holds[0].Reason
	} else {
		l.mu.Lock()
		hold = l.hold
		l.mu.Unlock()
	}
	switch {
	case !r.SignedIn:
		r.State = daemon.ReportingNotSignedIn
	case !st.SignedIn:
		// Signed in, but consent could not be read from Fleet yet
		// (starting up, offline): nothing is sent until it is.
		r.State = daemon.ReportingConsentUnknown
	case st.PlanRequired:
		r.State = daemon.ReportingNotAdmitted
	case r.Consent == daemon.ConsentReconfirm:
		r.State = daemon.ReportingReconfirm
	case r.Consent != daemon.ConsentGiven:
		r.State = daemon.ReportingNoConsent
	case hold != "":
		r.State, r.HoldReason = daemon.ReportingHeld, hold
	case lastErr != "" && !lastErrAt.Before(lastUpload):
		r.State = daemon.ReportingFailing
	case lastUpload.IsZero():
		r.State = daemon.ReportingWaitingForFirst
	default:
		r.State = daemon.ReportingWorking
	}
	s.Reporting = r
}
