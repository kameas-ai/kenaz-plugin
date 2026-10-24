// Package memsync is the opt-in memory sync lane: the only memory code
// that talks to Kameas Fleet. It moves one account profile's store
// (internal/memory) to and from Fleet's private memory routes through the
// fleetapi.Memory seam.
//
// Order of every cycle (storage-sync contract): validate the profile
// (signed-in identity matches, lease held, consent current) → deliver a
// pending forget-all erase → pull, handling resets → push. A lease and
// the profile generation make sure one sender per profile. The identity
// is checked before and after every request, and every request carries
// the profile's account as a fleetapi.ExpectedSession, which the client
// compares with the credentials it actually sends: a sign-in change
// between the check and the send is refused (identity_mismatch) with
// nothing sent, so queued work is never sent with another account's
// token.
//
// Logs and status carry counts and codes only, never note text.
package memsync

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"math/rand/v2"
	"sync"
	"sync/atomic"
	"time"

	"github.com/kameas-ai/kenaz-plugin/fleetapi"
	"github.com/kameas-ai/kenaz-plugin/internal/memory"
)

// Errors that stop a worker.
var (
	// ErrSignedOut: no session; the worker stops.
	ErrSignedOut = errors.New("memory sync stopped: signed out")
	// ErrAccountMismatch: the session belongs to another account than the
	// profile; the worker stops without sending.
	ErrAccountMismatch = errors.New("memory sync stopped: the signed-in account changed")
	errDisabledOnFleet = errors.New("memory sync was turned off for this account")
	// ErrNotInTier: the plan has no memory_sync capability.
	ErrNotInTier = errors.New("memory sync is not included in your organization's Kameas Fleet plan")
)

// Status codes the worker records in the store (and status shows).
const (
	CodeConsentReconfirm = "consent_reconfirm"
	CodeNotInTier        = fleetapi.CodeCapabilityNotInTier
	CodeDisabledOnFleet  = "disabled_on_fleet"
	CodeInvalidPage      = "invalid_page"
	CodeRateLimited      = fleetapi.CodeRateLimited
	CodeUnavailable      = "fleet_unavailable"
	CodeErasePending     = "erase_pending"
	CodeOK               = ""
	defaultInterval      = 30 * time.Second
	leaseTTL             = 2 * time.Minute
	maxBackoff           = 5 * time.Minute
	minBackoff           = 2 * time.Second
	maxResetRestarts     = 3
	defaultPullLimit     = 200
)

// Options configure a Worker.
type Options struct {
	Store   *memory.Store
	Memory  fleetapi.Memory
	Session func(ctx context.Context) (fleetapi.Session, error)
	// Capabilities reports the account's tier capabilities (nil: assume
	// entitled; Fleet's 403 decides).
	Capabilities func(ctx context.Context) (fleetapi.Capabilities, error)
	// ConsentVersion is the current memory-sync notice edition. A store
	// whose sync was enabled under another edition sends no new notes
	// until the user re-enables (re-consents).
	ConsentVersion string
	// Holder names this worker for the lease (unique per process).
	Holder   string
	Log      *slog.Logger
	Interval time.Duration
	// PullLimit is the page size (default 200).
	PullLimit int
	Now       func() time.Time
}

// Worker syncs one account profile.
type Worker struct {
	o    Options
	kick chan struct{}

	mu        sync.Mutex
	lease     memory.Lease
	held      bool
	backoff   time.Duration
	notBefore time.Time

	pushed, pulled, cycles atomic.Int64
	lastCode               atomic.Value
}

// New returns a worker.
func New(o Options) *Worker {
	if o.Interval <= 0 {
		o.Interval = defaultInterval
	}
	if o.PullLimit <= 0 {
		o.PullLimit = defaultPullLimit
	}
	if o.Now == nil {
		o.Now = time.Now
	}
	if o.Log == nil {
		o.Log = slog.New(slog.DiscardHandler)
	}
	w := &Worker{o: o, kick: make(chan struct{}, 1)}
	w.lastCode.Store("")
	return w
}

// Kick asks for a cycle now.
func (w *Worker) Kick() {
	select {
	case w.kick <- struct{}{}:
	default:
	}
}

// Stats are the worker's counters (no content).
type Stats struct {
	Cycles, Pushed, Pulled int64
	LastCode               string
}

// Stats returns the counters.
func (w *Worker) Stats() Stats {
	return Stats{Cycles: w.cycles.Load(), Pushed: w.pushed.Load(), Pulled: w.pulled.Load(), LastCode: w.lastCode.Load().(string)}
}

// Run cycles until ctx ends or the identity no longer matches. It returns
// the stop reason.
func (w *Worker) Run(ctx context.Context) error {
	defer w.release()
	for {
		wait := w.o.Interval
		err := w.RunOnce(ctx)
		switch {
		case errors.Is(err, ErrSignedOut), errors.Is(err, ErrAccountMismatch):
			return err
		case ctx.Err() != nil:
			return ctx.Err()
		case err != nil:
			wait = w.nextBackoff(err)
		default:
			w.mu.Lock()
			w.backoff = 0
			w.mu.Unlock()
		}
		t := time.NewTimer(wait)
		select {
		case <-ctx.Done():
			t.Stop()
			return ctx.Err()
		case <-w.kick:
			t.Stop()
		case <-t.C:
		}
	}
}

// nextBackoff doubles from 2 s to 5 min with jitter, and honours a 429's
// Retry-After when that is longer.
func (w *Worker) nextBackoff(err error) time.Duration {
	w.mu.Lock()
	defer w.mu.Unlock()
	switch {
	case w.backoff == 0:
		w.backoff = minBackoff
	case w.backoff < maxBackoff:
		w.backoff *= 2
	}
	if w.backoff > maxBackoff {
		w.backoff = maxBackoff
	}
	d := w.backoff/2 + rand.N(w.backoff/2+1)
	if ra, ok := fleetapi.RetryAfter(err); ok && ra > d {
		d = ra
	}
	return d
}

func (w *Worker) release() {
	w.mu.Lock()
	l, held := w.lease, w.held
	w.held = false
	w.mu.Unlock()
	if held {
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		_ = w.o.Store.ReleaseLease(ctx, l)
	}
}

// checkIdentity verifies the active session is the profile's account. It
// runs before every Fleet request.
func (w *Worker) checkIdentity(ctx context.Context) error {
	s, err := w.o.Session(ctx)
	if err != nil {
		return ErrSignedOut
	}
	p := w.o.Store.Profile()
	if string(s.Env) != p.Env || s.OrgID != p.OrgID || s.UserID != p.UserID {
		return ErrAccountMismatch
	}
	return nil
}

// bound returns ctx carrying the profile's account as the request's
// required identity (see fleetapi.Memory).
func (w *Worker) bound(ctx context.Context) context.Context {
	return bindProfile(ctx, w.o.Store.Profile())
}

func bindProfile(ctx context.Context, p memory.Profile) context.Context {
	return fleetapi.WithExpectedSession(ctx, fleetapi.ExpectedSession{Env: fleetapi.Env(p.Env), OrgID: p.OrgID, Subject: p.UserID})
}

// mismatch maps the client's identity_mismatch refusal (nothing was sent)
// to ErrAccountMismatch.
func mismatch(err error) error {
	if fleetapi.APICode(err) == fleetapi.CodeIdentityMismatch {
		return ErrAccountMismatch
	}
	return err
}

func (w *Worker) setCode(ctx context.Context, code string) {
	if w.lastCode.Load().(string) != code {
		w.lastCode.Store(code)
		_ = w.o.Store.SetStatusCode(ctx, code)
		if code != "" {
			w.o.Log.Info("memory sync", "status", code)
		}
	}
}

// RunOnce runs one cycle: validate → erase → pull/reset → push.
func (w *Worker) RunOnce(ctx context.Context) error {
	w.cycles.Add(1)
	st := w.o.Store
	if st.Profile().IsLocal() {
		return memory.ErrLocalProfile
	}
	if err := w.checkIdentity(ctx); err != nil {
		return err
	}
	l, err := st.AcquireLease(ctx, w.o.Holder, leaseTTL)
	if err != nil {
		return err
	}
	w.mu.Lock()
	w.lease, w.held = l, true
	w.mu.Unlock()

	set, err := st.SyncSettings(ctx)
	if err != nil {
		return err
	}
	if set.ErasePending {
		if err := w.erase(ctx, l); err != nil {
			return err
		}
	}
	active := set.Enabled
	if active && set.ConsentVersion != w.o.ConsentVersion {
		// Accepted under another notice edition: nothing new is sent until
		// the user re-confirms. Deletions still go.
		w.setCode(ctx, CodeConsentReconfirm)
		active = false
	}
	entitled := true
	if active && w.o.Capabilities != nil {
		if caps, err := w.o.Capabilities(ctx); err == nil && !caps.Has(fleetapi.CapMemorySync) {
			entitled = false
		}
	}
	if active && !entitled {
		w.setCode(ctx, CodeNotInTier)
	}
	if active && entitled {
		err := w.pullAll(ctx, l)
		if err == nil {
			err = w.pushAll(ctx, l)
		}
		if errors.Is(err, errDisabledOnFleet) {
			return nil
		}
		if _, paused := fleetapi.OrgPaused(err); paused {
			// A paused organization refuses any batch with an upsert, but
			// deletions are data-rights requests: send them on their own.
			if ferr := w.forgetViaRoute(ctx, l); ferr != nil {
				return w.classify(ctx, ferr)
			}
		}
		if err != nil {
			return w.classify(ctx, err)
		}
		if w.lastCode.Load().(string) != CodeOK {
			w.setCode(ctx, CodeOK)
		}
		return nil
	}
	// Not syncing new notes: still deliver deletions of notes Fleet
	// holds, through the data-rights forget route (no capability needed).
	if set.EverEnabled {
		if err := w.forgetViaRoute(ctx, l); err != nil {
			return w.classify(ctx, err)
		}
	}
	return nil
}

// classify records the condition behind err and returns it.
func (w *Worker) classify(ctx context.Context, err error) error {
	switch {
	case errors.Is(err, ErrSignedOut), errors.Is(err, ErrAccountMismatch), errors.Is(err, memory.ErrLeaseLost):
	case errors.Is(err, memory.ErrInvalidPage):
		w.setCode(ctx, CodeInvalidPage)
	case fleetapi.APICode(err) == fleetapi.CodeCapabilityNotInTier:
		w.setCode(ctx, CodeNotInTier)
	case fleetapi.APICode(err) != "":
		w.setCode(ctx, fleetapi.APICode(err))
	default:
		if ctx.Err() == nil {
			w.setCode(ctx, CodeUnavailable)
		}
	}
	w.o.Log.Info("memory sync cycle failed", "code", fleetapi.APICode(err))
	return err
}

// erase delivers a confirmed forget-all. Only Fleet's answer completes
// it; a failure leaves the barrier up (nothing new is sent).
func (w *Worker) erase(ctx context.Context, l memory.Lease) error {
	w.setCode(ctx, CodeErasePending)
	if err := w.checkIdentity(ctx); err != nil {
		return err
	}
	res, err := w.o.Memory.ForgetAll(w.bound(ctx), fleetapi.MemoryForgetAllRequest{Confirm: fleetapi.MemoryForgetAllConfirm})
	if err = mismatch(err); errors.Is(err, ErrAccountMismatch) {
		return err // refused before sending; the barrier stays
	}
	if ierr := w.checkIdentity(ctx); ierr != nil {
		// The session changed during the call: whose data was erased is
		// uncertain, so the barrier stays and the erase is repeated
		// (idempotent) under the right account.
		return ierr
	}
	if err != nil {
		return w.classify(ctx, err)
	}
	if err := w.o.Store.CompleteErase(ctx, l, res.ErasedBefore); err != nil {
		return err
	}
	w.o.Log.Info("memory sync erase complete", "erased", res.Erased)
	w.setCode(ctx, CodeOK)
	return nil
}

func toPulled(rs []fleetapi.MemoryRecord) []memory.PulledRecord {
	out := make([]memory.PulledRecord, 0, len(rs))
	for _, r := range rs {
		out = append(out, memory.PulledRecord{ID: r.ID, Seq: r.Seq, State: r.State, Content: r.Content, ContentHash: r.ContentHash,
			Kind: r.Kind, Source: r.Source, CreatedAt: r.CreatedAt, Title: r.Title, TitleHLC: r.TitleHLC, Pinned: r.Pinned,
			PinnedHLC: r.PinnedHLC, ScopeKind: r.ScopeKind, ScopeHLC: r.ScopeHLC, Aliases: r.Aliases, Reason: r.Reason,
			SupersededBy: r.SupersededBy})
	}
	return out
}

// pull fetches one page. The identity is checked right before the call
// and again after the answer, and a page fetched across a sign-in change
// is discarded. A page from an account whose sync is off (enabled:false,
// no records, the cursor echoed) is returned as errDisabledOnFleet so no
// caller ever treats it as data or as a complete snapshot.
func (w *Worker) pull(ctx context.Context, cursor string) (fleetapi.MemoryPullResponse, error) {
	if err := w.checkIdentity(ctx); err != nil {
		return fleetapi.MemoryPullResponse{}, err
	}
	resp, err := w.o.Memory.Pull(w.bound(ctx), fleetapi.MemoryPullRequest{Cursor: cursor, Limit: w.o.PullLimit, DeviceID: w.o.Store.DeviceID()})
	if err = mismatch(err); errors.Is(err, ErrAccountMismatch) {
		return fleetapi.MemoryPullResponse{}, err
	}
	if ierr := w.checkIdentity(ctx); ierr != nil {
		return fleetapi.MemoryPullResponse{}, ierr
	}
	if err != nil {
		return resp, err
	}
	if !resp.Enabled {
		w.setCode(ctx, CodeDisabledOnFleet)
		if err := w.o.Store.DisableSync(ctx, CodeDisabledOnFleet); err != nil {
			return fleetapi.MemoryPullResponse{}, err
		}
		return fleetapi.MemoryPullResponse{}, errDisabledOnFleet
	}
	return resp, nil
}

// pullAll pulls to exhaustion, resuming an interrupted reset first.
func (w *Worker) pullAll(ctx context.Context, l memory.Lease) error {
	st := w.o.Store
	if rs, ok, err := st.PendingReset(ctx); err != nil {
		return err
	} else if ok {
		if err := w.reset(ctx, l, rs.Reason, rs.ErasedBefore); err != nil {
			return err
		}
	}
	for {
		cur, err := st.Cursor(ctx)
		if err != nil {
			return err
		}
		resp, err := w.pull(ctx, cur)
		if err != nil {
			return err
		}
		if resp.Reset {
			if err := w.reset(ctx, l, resp.ResetReason, resp.ErasedBefore); err != nil {
				return err
			}
			continue
		}
		out, err := st.ApplyPullPage(ctx, l, memory.PullPage{Records: toPulled(resp.Records), Cursor: resp.Cursor, HasMore: resp.HasMore})
		if err != nil {
			return err
		}
		w.pulled.Add(int64(out.Applied + out.Removed))
		if !resp.HasMore {
			return nil
		}
	}
}

// reset stages a full snapshot from the empty cursor and commits it only
// when complete. A reset that itself resets (forget-all mid-snapshot)
// restarts.
func (w *Worker) reset(ctx context.Context, l memory.Lease, reason, erasedBefore string) error {
	st := w.o.Store
	w.o.Log.Info("memory sync reset", "reason", reason)
	for attempt := 0; attempt < maxResetRestarts; attempt++ {
		gen, err := st.BeginReset(ctx, l, reason, erasedBefore)
		if err != nil {
			return err
		}
		cursor := ""
		restarted := false
		for {
			resp, err := w.pull(ctx, cursor)
			if err != nil {
				return err
			}
			if resp.Reset {
				reason, erasedBefore, restarted = resp.ResetReason, resp.ErasedBefore, true
				break
			}
			if err := st.StageSnapshot(ctx, l, gen, memory.PullPage{Records: toPulled(resp.Records), Cursor: resp.Cursor, HasMore: resp.HasMore}); err != nil {
				return err
			}
			cursor = resp.Cursor
			if !resp.HasMore {
				break
			}
		}
		if restarted {
			continue
		}
		out, err := st.CommitReset(ctx, l, gen, cursor)
		if err != nil {
			return err
		}
		w.pulled.Add(int64(out.Applied + out.Removed))
		return nil
	}
	return errors.New("memory sync: reset kept restarting")
}

func toItems(ops []memory.OutboxOp) ([]fleetapi.MemoryPushItem, error) {
	items := make([]fleetapi.MemoryPushItem, len(ops))
	for i, op := range ops {
		if err := json.Unmarshal(op.Payload, &items[i]); err != nil {
			return nil, err
		}
	}
	return items, nil
}

func toResults(rs []fleetapi.MemoryPushResult) []memory.PushResult {
	out := make([]memory.PushResult, len(rs))
	for i, r := range rs {
		out[i] = memory.PushResult{ID: r.ID, Status: r.Status, Seq: r.Seq, CanonicalID: r.CanonicalID, SupersededBy: r.SupersededBy,
			Code: r.Code, Field: r.Field, Retry: r.Retry}
	}
	return out
}

// pushAll sends leased batches until the queue has nothing ready.
func (w *Worker) pushAll(ctx context.Context, l memory.Lease) error {
	st := w.o.Store
	for {
		b, err := st.LeasePushBatch(ctx, l)
		if err != nil {
			return err
		}
		if len(b.Ops) == 0 {
			return nil
		}
		items, err := toItems(b.Ops)
		if err != nil {
			return err
		}
		if err := w.checkIdentity(ctx); err != nil {
			_ = st.ReturnUnknown(context.Background(), l, b.Ops, 0)
			return err
		}
		resp, err := w.o.Memory.Push(w.bound(ctx), fleetapi.MemoryPushRequest{DeviceID: st.DeviceID(), BaseCursor: b.BaseCursor, Items: items})
		if err = mismatch(err); errors.Is(err, ErrAccountMismatch) {
			// Refused by the client before sending: nothing reached Fleet.
			_ = st.ReturnUnknown(context.Background(), l, b.Ops, 0)
			return err
		}
		if ierr := w.checkIdentity(ctx); ierr != nil {
			// The session changed during the call: discard the answer;
			// the ops' outcome is unknown and they are replayed (same
			// payloads, idempotent) only under this profile's account.
			_ = st.ReturnUnknown(context.Background(), l, b.Ops, 0)
			return ierr
		}
		if err != nil {
			// Unknown outcome: the server may have applied the batch. The
			// same immutable payloads are replayed later (idempotent).
			delay := memory.OpBackoff(b.Ops[0].Attempt)
			if ra, ok := fleetapi.RetryAfter(err); ok && ra > delay {
				delay = ra
			}
			if _, paused := fleetapi.OrgPaused(err); paused {
				// Fleet applied nothing; deletions go next on their own.
				var forgets, upserts []memory.OutboxOp
				for _, op := range b.Ops {
					if op.Op == memory.OpForget {
						forgets = append(forgets, op)
					} else {
						upserts = append(upserts, op)
					}
				}
				_ = st.ReturnUnknown(context.Background(), l, forgets, 0)
				_ = st.ReturnUnknown(context.Background(), l, upserts, delay)
				return err
			}
			_ = st.ReturnUnknown(context.Background(), l, b.Ops, delay)
			return err
		}
		out, err := st.ApplyPushResults(ctx, l, b.Ops, toResults(resp.Results))
		if err != nil {
			return err
		}
		w.pushed.Add(int64(out.Synced + out.Merged + out.Forgotten + out.Removed))
		if out.Blocked > 0 {
			w.o.Log.Info("memory sync rejected items", "count", out.Blocked)
		}
		if out.SyncDisabled {
			if s, err := w.o.Memory.Settings(w.bound(ctx)); err == nil && w.checkIdentity(ctx) == nil && !s.Enabled {
				w.setCode(ctx, CodeDisabledOnFleet)
				if err := st.DisableSync(ctx, CodeDisabledOnFleet); err != nil {
					return err
				}
				return errDisabledOnFleet
			}
		}
		if out.ResyncRequired {
			// Our base cursor is behind Fleet's floor: pull (reset) first,
			// then the retried items go with a fresh base cursor.
			return w.pullAll(ctx, l)
		}
		if out.Retry > 0 && out.Synced+out.Merged+out.Forgotten+out.Removed == 0 {
			return nil // everything is backing off
		}
	}
}

// forgetViaRoute sends queued forgets through POST /memory/forget, which
// needs no plan capability, so a user whose sync is off or whose plan
// lapsed can still delete notes Fleet holds.
func (w *Worker) forgetViaRoute(ctx context.Context, l memory.Lease) error {
	st := w.o.Store
	for {
		b, err := st.LeaseForgetBatch(ctx, l)
		if err != nil {
			return err
		}
		if len(b.Ops) == 0 {
			return nil
		}
		forgets := b.Ops
		ids := make([]string, len(forgets))
		for i, op := range forgets {
			ids[i] = op.RecordID
		}
		if err := w.checkIdentity(ctx); err != nil {
			_ = st.ReturnUnknown(context.Background(), l, forgets, 0)
			return err
		}
		_, err = w.o.Memory.Forget(w.bound(ctx), fleetapi.MemoryForgetRequest{IDs: ids})
		if err = mismatch(err); errors.Is(err, ErrAccountMismatch) {
			_ = st.ReturnUnknown(context.Background(), l, forgets, 0)
			return err
		}
		if ierr := w.checkIdentity(ctx); ierr != nil {
			_ = st.ReturnUnknown(context.Background(), l, forgets, 0)
			return ierr
		}
		if err != nil {
			_ = st.ReturnUnknown(context.Background(), l, forgets, memory.OpBackoff(forgets[0].Attempt))
			return err
		}
		res := make([]memory.PushResult, len(forgets))
		for i, op := range forgets {
			res[i] = memory.PushResult{ID: op.RecordID, Status: fleetapi.MemoryForgotten}
		}
		if _, err := st.ApplyPushResults(ctx, l, forgets, res); err != nil {
			return err
		}
		w.pushed.Add(int64(len(forgets)))
	}
}

// Remote implements memory.Remote over the seam: entitlement check and
// the settings opt-in/out.
type Remote struct {
	Memory       fleetapi.Memory
	Session      func(ctx context.Context) (fleetapi.Session, error)
	Capabilities func(ctx context.Context) (fleetapi.Capabilities, error)
}

func (r Remote) check(ctx context.Context, p memory.Profile) error {
	s, err := r.Session(ctx)
	if err != nil {
		return ErrSignedOut
	}
	if string(s.Env) != p.Env || s.OrgID != p.OrgID || s.UserID != p.UserID {
		return ErrAccountMismatch
	}
	return nil
}

// EnableSync implements memory.Remote.
func (r Remote) EnableSync(ctx context.Context, p memory.Profile, consentVersion string) error {
	if err := r.check(ctx, p); err != nil {
		return err
	}
	if r.Capabilities != nil {
		caps, err := r.Capabilities(ctx)
		if err != nil {
			return err
		}
		if !caps.Has(fleetapi.CapMemorySync) {
			return ErrNotInTier
		}
	}
	on := true
	scopes := []string{memory.ScopeGlobal, memory.ScopeLongTerm}
	_, err := r.Memory.PutSettings(bindProfile(ctx, p), fleetapi.MemorySettingsUpdate{Enabled: &on, Scopes: &scopes, ConsentVersion: consentVersion})
	err = mismatch(err)
	if cerr := r.check(ctx, p); cerr != nil {
		return cerr
	}
	if fleetapi.APICode(err) == fleetapi.CodeCapabilityNotInTier {
		return ErrNotInTier
	}
	return err
}

// DisableSync implements memory.Remote.
func (r Remote) DisableSync(ctx context.Context, p memory.Profile) error {
	if err := r.check(ctx, p); err != nil {
		return err
	}
	off := false
	_, err := r.Memory.PutSettings(bindProfile(ctx, p), fleetapi.MemorySettingsUpdate{Enabled: &off})
	err = mismatch(err)
	if cerr := r.check(ctx, p); cerr != nil {
		return cerr
	}
	return err
}
