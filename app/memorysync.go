package app

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strconv"
	"sync"
	"time"

	"github.com/kameas-ai/kenaz-plugin/fleetapi"
	"github.com/kameas-ai/kenaz-plugin/internal/approvals"
	"github.com/kameas-ai/kenaz-plugin/internal/daemon"
	contexttools "github.com/kameas-ai/kenaz-plugin/internal/mcp/tools/context"
	"github.com/kameas-ai/kenaz-plugin/internal/memory"
	"github.com/kameas-ai/kenaz-plugin/internal/memory/memsync"
	"github.com/kameas-ai/kenaz-plugin/notices"
)

// memorySyncAvailable reports whether this build can sync memory: an
// official provider that implements the memory seam.
func memorySyncAvailable(p fleetapi.Provider) bool {
	if p == nil || p.Flavor() != fleetapi.FlavorOfficial {
		return false
	}
	_, ok := p.(fleetapi.MemoryProvider)
	return ok
}

// memoryProfileOf maps the active session to its memory profile, without
// network I/O (Auth().Session reads local state). Signed out, or any
// error, is the device notebook.
func memoryProfileOf(ctx context.Context, p fleetapi.Provider) memory.Profile {
	if p == nil {
		return memory.Local
	}
	s, err := p.Auth().Session(ctx)
	if err != nil || s.UserID == "" || s.OrgID == "" {
		return memory.Local
	}
	pr := memory.Profile{Env: string(s.Env), OrgID: s.OrgID, UserID: s.UserID}
	if !pr.Valid() {
		return memory.Local
	}
	return pr
}

// capsCache bounds capability lookups to one per five minutes per
// account: an entry cached for one signed-in account is never used for
// another.
type capsCache struct {
	p  fleetapi.Provider
	mu sync.Mutex
	m  map[string]capsEntry
}

type capsEntry struct {
	at   time.Time
	caps fleetapi.Capabilities
}

func (c *capsCache) get(ctx context.Context) (fleetapi.Capabilities, error) {
	owner := memoryProfileOf(ctx, c.p).Key()
	c.mu.Lock()
	defer c.mu.Unlock()
	if e, ok := c.m[owner]; ok && time.Since(e.at) < 5*time.Minute {
		return e.caps, nil
	}
	caps, err := c.p.Me().Capabilities(ctx)
	if err != nil {
		return caps, err
	}
	// Only cache under the account that is still signed in after the call.
	if memoryProfileOf(ctx, c.p).Key() == owner {
		if c.m == nil {
			c.m = map[string]capsEntry{}
		}
		c.m[owner] = capsEntry{at: time.Now(), caps: caps}
	}
	return caps, nil
}

// memoryRemote is the Fleet side of turning sync on/off, or nil when this
// build cannot sync.
func memoryRemote(p fleetapi.Provider) memory.Remote {
	if !memorySyncAvailable(p) {
		return nil
	}
	return memsync.Remote{Memory: fleetapi.MemoryOf(p), Session: p.Auth().Session, Capabilities: p.Me().Capabilities}
}

// memorySyncHook installs the memory sync lane in the relay daemon.
func memorySyncHook(env Env) DaemonHook {
	return func(opt *daemon.Options) (func(*daemon.Daemon), error) {
		if !memorySyncAvailable(opt.Provider) {
			return nil, nil
		}
		mp, err := env.memoryPaths()
		if err != nil {
			return nil, nil
		}
		b := make([]byte, 6)
		rand.Read(b)
		l := &memorySyncLane{p: opt.Provider, stateRoot: mp.StateRoot, log: opt.Logger,
			holder: fmt.Sprintf("relay-%d-%s", os.Getpid(), hex.EncodeToString(b)), caps: &capsCache{p: opt.Provider},
			kick: make(chan struct{}, 1), period: 30 * time.Second}
		opt.Lanes = append(opt.Lanes, l)
		return nil, nil
	}
}

// memorySyncLane runs one memsync.Worker for the signed-in account's
// profile, if that profile has sync work. It stops the worker on sign-out
// or account switch (the worker also refuses to send under another
// identity) and starts one for the new account. It never deletes notes.
type memorySyncLane struct {
	p         fleetapi.Provider
	stateRoot string
	log       *slog.Logger
	holder    string
	caps      *capsCache
	kick      chan struct{}
	period    time.Duration

	mu      sync.Mutex
	cancel  context.CancelFunc
	profile memory.Profile
	worker  *memsync.Worker
	wcancel context.CancelFunc
	wdone   chan struct{}
	held    string
	stopped bool
	stats   memsync.Stats
}

var _ daemon.Lane = (*memorySyncLane)(nil)

func (l *memorySyncLane) Name() string { return "memory-sync" }

func (l *memorySyncLane) Start(ctx context.Context) error {
	ctx, cancel := context.WithCancel(ctx)
	l.mu.Lock()
	l.cancel, l.stopped = cancel, false
	l.mu.Unlock()
	go l.supervise(ctx)
	return nil
}

func (l *memorySyncLane) supervise(ctx context.Context) {
	for {
		l.reconcile(ctx)
		t := time.NewTimer(l.period)
		select {
		case <-ctx.Done():
			t.Stop()
			l.stopWorker()
			return
		case <-l.kick:
			t.Stop()
		case <-t.C:
		}
	}
}

// reconcile makes the running worker match the signed-in profile.
func (l *memorySyncLane) reconcile(ctx context.Context) {
	want := memoryProfileOf(ctx, l.p)
	l.mu.Lock()
	cur, running, held := l.profile, l.worker != nil, l.held
	l.mu.Unlock()
	if running && (cur != want || held != "") {
		l.stopWorker()
		running = false
	}
	if running || held != "" || want.IsLocal() {
		return
	}
	path := memory.ProfilePath(l.stateRoot, want)
	if _, err := os.Stat(path); err != nil {
		return
	}
	st, err := memory.OpenProfile(path, want)
	if err != nil {
		l.log.Warn("memory sync: store unavailable", "err", err.Error())
		return
	}
	if work, err := st.HasSyncWork(ctx); err != nil || !work {
		st.Close()
		return
	}
	w := memsync.New(memsync.Options{
		Store: st, Memory: fleetapi.MemoryOf(l.p), Session: l.p.Auth().Session, Capabilities: l.caps.get,
		ConsentVersion: notices.MemorySync().ID, AcceptedConsentVersions: notices.MemorySyncAccepted(),
		Holder: l.holder, Log: l.log,
	})
	wctx, wcancel := context.WithCancel(ctx)
	done := make(chan struct{})
	l.mu.Lock()
	l.profile, l.worker, l.wcancel, l.wdone = want, w, wcancel, done
	l.mu.Unlock()
	go func() {
		defer close(done)
		defer st.Close()
		err := w.Run(wctx)
		if errors.Is(err, memsync.ErrAccountMismatch) || errors.Is(err, memsync.ErrSignedOut) {
			// Old identity: no worker may write for it again.
			_ = st.InvalidateLeases(context.Background())
		}
		l.mu.Lock()
		l.stats = w.Stats()
		if l.worker == w {
			l.worker = nil
		}
		l.mu.Unlock()
	}()
}

func (l *memorySyncLane) stopWorker() {
	l.mu.Lock()
	w, cancel, done := l.worker, l.wcancel, l.wdone
	l.mu.Unlock()
	if w == nil {
		return
	}
	cancel()
	<-done
}

func (l *memorySyncLane) Stop() error {
	l.mu.Lock()
	cancel := l.cancel
	l.stopped = true
	l.mu.Unlock()
	if cancel != nil {
		cancel()
	}
	l.stopWorker()
	return nil
}

func (l *memorySyncLane) Hold(reason string) {
	l.mu.Lock()
	l.held = reason
	l.mu.Unlock()
	l.stopWorker()
}

func (l *memorySyncLane) Resume() {
	l.mu.Lock()
	l.held = ""
	l.mu.Unlock()
	select {
	case l.kick <- struct{}{}:
	default:
	}
}

// Purge (logout, node removed) stops syncing and invalidates the account
// profile's lease. Notes are the user's data and stay on the device.
func (l *memorySyncLane) Purge() error {
	l.mu.Lock()
	p := l.profile
	l.mu.Unlock()
	l.stopWorker()
	if p.IsLocal() {
		return nil
	}
	st, err := memory.OpenProfile(memory.ProfilePath(l.stateRoot, p), p)
	if err != nil {
		return nil
	}
	defer st.Close()
	return st.InvalidateLeases(context.Background())
}

func (l *memorySyncLane) Status() daemon.LaneStatus {
	l.mu.Lock()
	defer l.mu.Unlock()
	s := l.stats
	if l.worker != nil {
		s = l.worker.Stats()
	}
	ls := daemon.LaneStatus{Name: l.Name(), State: "stopped", Uploaded: s.Pushed, Reason: s.LastCode}
	switch {
	case l.held != "":
		ls.State, ls.Reason = "held", l.held
	case l.worker != nil:
		ls.State = "running"
	}
	return ls
}

// Memory's approval executors run only in the relay daemon, inside the
// approval service, after it verified the user's WebAuthn assertion. They
// open the payload's profile store here and apply exactly the stored
// payload under its idempotency key.
func init() { approvalRegistrars = append(approvalRegistrars, registerMemoryExecutors) }

func registerMemoryExecutors(m *approvals.Manager, p fleetapi.Provider, stateDir string, _ contexttools.StatusSource) {
	// The per-env state dir is <state root>/<env> (config.Paths.State);
	// memory stores live under the state root, shared by every env.
	root := filepath.Dir(stateDir)
	for _, op := range memory.Operations {
		m.RegisterRenderer(op, renderMemory(op))
	}
	memory.RegisterExecutors(memoryRegistrar{m, p}, memory.ExecOptions{
		StateRoot:      root,
		Active:         func(ctx context.Context) memory.Profile { return memoryProfileOf(ctx, p) },
		Remote:         memoryRemote(p),
		ConsentVersion: notices.MemorySync().ID,
	})
}

// memoryRegistrar adapts the approval Manager to memory.Registrar and maps
// executor errors to the approval service's codes.
type memoryRegistrar struct {
	m *approvals.Manager
	p fleetapi.Provider
}

func (r memoryRegistrar) Register(op string, ex memory.Executor) {
	r.m.Register(op, func(ctx context.Context, payload []byte, key string) ([]byte, error) {
		// The session must still be the account the user approved for,
		// checked right before applying.
		sess, err := r.p.Auth().Session(ctx)
		if err != nil {
			return nil, approvals.ErrorCode("not_signed_in")
		}
		if err := approvals.CheckSession(ctx, string(sess.Env), sess.OrgID, sess.UserID); err != nil {
			return nil, err
		}
		res, err := ex(ctx, payload, key)
		if err != nil {
			return nil, memoryExecError(op, err)
		}
		return res, nil
	})
}

func (r memoryRegistrar) RegisterReconciler(op string, rec memory.Reconciler) {
	r.m.RegisterReconciler(op, approvals.Reconciler(rec))
}

// memoryExecError keeps only a short code of an executor error (no note
// content reaches status output). A sync-enable whose Fleet call failed in
// transit is "outcome unknown": the reconciler then checks and the relay
// retries the idempotent executor.
func memoryExecError(op string, err error) error {
	if c := memory.ErrorCode(err); c != "" {
		return approvals.ErrorCode(c)
	}
	switch {
	case errors.Is(err, memsync.ErrNotInTier):
		return approvals.ErrorCode("capability_not_in_tier")
	case errors.Is(err, memsync.ErrSignedOut):
		return approvals.ErrorCode("not_signed_in")
	case errors.Is(err, memsync.ErrAccountMismatch):
		return approvals.ErrorCode("account_changed")
	}
	var ae *fleetapi.APIError
	if errors.As(err, &ae) && ae.Status < 500 && ae.Status != 429 {
		return approvals.ErrorCode(ae.Code)
	}
	if op == memory.OpSyncEnable || errors.Is(err, memory.ErrOutcomeUnknown) {
		return fmt.Errorf("%w: %s", approvals.ErrOutcomeUnknown, op)
	}
	return approvals.ErrorCode("execution_failed")
}

// memoryPayload is the subset of memory's approval payload the page shows
// (the package's opPayload; NewNote has no JSON tags, so its keys are the
// Go field names, which encoding/json matches case-insensitively).
type memoryPayload struct {
	Profile struct {
		Env    string `json:"env"`
		OrgID  string `json:"org_id"`
		UserID string `json:"user_id"`
	} `json:"profile"`
	Note *struct {
		Title string
		Body  string
		Tags  []string
	} `json:"note"`
	ID    string       `json:"id"`
	Title *string      `json:"title"`
	Body  *string      `json:"body"`
	Ops   []heldOpView `json:"ops"`
	// Records: the exact notes sync-enable uploads or adopt moves.
	Records []heldOpView `json:"records"`
}

type heldOpView struct {
	RecordID string `json:"record_id"`
	Op       string `json:"op"`
	Title    string `json:"title"`
	Hash     string `json:"content_hash"`
	Body     string `json:"body"`
}

// noteDetails lists each note with its full text for the approval page
// (the page previews it and reveals the rest on request). A body that
// does not match the content hash the executor checks is refused, so the
// text shown is the text approved.
func noteDetails(items []heldOpView, label func(heldOpView) string) ([]approvals.Detail, error) {
	if len(items) > approvals.MaxTargets {
		return nil, fmt.Errorf("lists more than %d notes", approvals.MaxTargets)
	}
	out := make([]approvals.Detail, 0, len(items))
	for _, it := range items {
		if it.Body != "" && memory.ContentHash(it.Body) != it.Hash {
			return nil, errors.New("a listed note's text does not match its content hash")
		}
		out = append(out, approvals.Detail{Target: it.RecordID, Label: label(it), Text: it.Body})
	}
	return out, nil
}

// heldLabel names what a held change does.
func heldLabel(op string) string {
	switch op {
	case memory.OpUpsert:
		return "Upload"
	case memory.OpForget, memory.OpDeleteNote:
		return "Delete"
	}
	return op
}

// renderMemory describes a memory payload for the approval page from the
// payload alone; the text a tool sends with the request is never shown.
func renderMemory(op string) approvals.Renderer {
	return func(payload []byte) (approvals.Rendered, error) {
		var pl memoryPayload
		if err := json.Unmarshal(payload, &pl); err != nil {
			return approvals.Rendered{}, err
		}
		if pl.Profile.UserID == "" {
			return approvals.Rendered{}, errors.New("memory approvals need an account profile")
		}
		clip := func(s string) string { return memory.Clip(s, 120) }
		switch op {
		case memory.OpSave:
			if pl.Note == nil {
				return approvals.Rendered{}, errors.New("save without a note")
			}
			return approvals.Rendered{Summary: "Save a memory titled " + strconv.Quote(clip(pl.Note.Title)) + " in your Kameas Fleet account's memory.",
				Details: []approvals.Detail{{Label: pl.Note.Title, Text: pl.Note.Body}}}, nil
		case memory.OpUpdate:
			if pl.ID == "" {
				return approvals.Rendered{}, errors.New("update without an id")
			}
			sum := "Edit memory " + pl.ID
			if pl.Title != nil {
				sum += ": new title " + strconv.Quote(clip(*pl.Title))
			}
			return approvals.Rendered{Summary: sum + ".", Targets: []string{pl.ID}}, nil
		case memory.OpForgetOne:
			if pl.ID == "" {
				return approvals.Rendered{}, errors.New("forget without an id")
			}
			return approvals.Rendered{Summary: "Delete memory " + pl.ID + ".", Targets: []string{pl.ID},
				Impact: "Deletes this memory on this device and, if it was synced, in your Kameas Fleet account."}, nil
		case memory.OpForgetAll:
			return approvals.Rendered{Summary: "Delete every memory of this Kameas Fleet account, on this device and in Kameas Fleet.",
				Impact: "Irreversible: erases all of this account's memories on this device and its memory in Kameas Fleet. " + memory.ForgetAllOtherDevices}, nil
		case memory.OpSyncEnable:
			details, err := noteDetails(pl.Records, func(r heldOpView) string { return "Upload " + strconv.Quote(clip(r.Title)) })
			if err != nil {
				return approvals.Rendered{}, fmt.Errorf("sync enable: %w", err)
			}
			out := approvals.Rendered{Summary: fmt.Sprintf("Turn on memory sync to your Kameas Fleet account under the Memory Sync Notice %s and upload %d notes.", notices.MemorySync().ID, len(pl.Records)),
				Impact: "Keeps this account's synced notes in sync with your other devices. Each note it uploads is listed below with its full text.", Details: details}
			for _, r := range pl.Records {
				out.Targets = append(out.Targets, r.RecordID)
			}
			return out, nil
		case memory.OpSyncApprove:
			if len(pl.Ops) == 0 {
				return approvals.Rendered{}, errors.New("sync approve needs at least one change")
			}
			details, err := noteDetails(pl.Ops, func(o heldOpView) string { return heldLabel(o.Op) + " " + strconv.Quote(clip(o.Title)) })
			if err != nil {
				return approvals.Rendered{}, fmt.Errorf("sync approve: %w", err)
			}
			out := approvals.Rendered{Summary: fmt.Sprintf("Send %d waiting memory changes to your Kameas Fleet account.", len(pl.Ops)),
				Impact: "Each change is listed below with the full text it sends or deletes.", Details: details}
			for _, o := range pl.Ops {
				out.Targets = append(out.Targets, o.RecordID)
			}
			return out, nil
		case memory.OpAdopt:
			if len(pl.Records) == 0 {
				return approvals.Rendered{}, errors.New("adopt needs at least one note")
			}
			details, err := noteDetails(pl.Records, func(r heldOpView) string { return "Move " + strconv.Quote(clip(r.Title)) })
			if err != nil {
				return approvals.Rendered{}, fmt.Errorf("adopt: %w", err)
			}
			out := approvals.Rendered{Summary: fmt.Sprintf("Move %d notes from this device's notebook into your Kameas Fleet account's memory.", len(pl.Records)),
				Impact: "The notes leave the device notebook and can then sync to your account.", Details: details}
			for _, r := range pl.Records {
				out.Targets = append(out.Targets, r.RecordID)
			}
			return out, nil
		}
		return approvals.Rendered{}, errors.New("unknown memory operation")
	}
}
