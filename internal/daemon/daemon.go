// Package daemon is the per-user, per-env singleton relay daemon: lockfile, loopback listener
// with a nonce-proved health check, a nonce-authenticated control API for
// the MCP process and CLI, lane lifecycle, the enroll heartbeat, the
// capability poller, and idle exit.
//
// One daemon serves every harness session on the machine: it owns
// the session, pollers and spool, so there is a single token refresher.
package daemon

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"log/slog"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"time"

	"github.com/kameas-ai/kenaz-plugin/fleetapi"
	"github.com/kameas-ai/kenaz-plugin/internal/clock"
	"github.com/kameas-ai/kenaz-plugin/internal/consent"
	"github.com/kameas-ai/kenaz-plugin/internal/enroll"
	"github.com/kameas-ai/kenaz-plugin/internal/spool"
	"github.com/kameas-ai/kenaz-plugin/internal/variant"
)

// ErrAlreadyRunning is returned by Run when another daemon holds the lock.
var ErrAlreadyRunning = errors.New("kenaz daemon already running")

// DefaultPorts is the bind preference: the OTLP/HTTP default, a
// fixed fallback, then ephemeral.
var DefaultPorts = variant.Shared.Ports()

// Timing defaults.
const (
	DefaultIdleTimeout      = 15 * time.Minute
	DefaultCapabilityPeriod = 5 * time.Minute
	clientTTL               = 3 * time.Minute
	idleCheckPeriod         = time.Minute
)

// Options configures a daemon.
type Options struct {
	Provider fleetapi.Provider
	Env      fleetapi.Env
	StateDir string
	Version  string
	Logger   *slog.Logger
	Clock    clock.Clock

	IdleTimeout time.Duration
	Ports       []int

	// Host is the enroll host description; zero means enroll.LocalHost().
	Host enroll.Host
	// HeartbeatInterval overrides 6 h (tests).
	HeartbeatInterval time.Duration
	// CapabilityPeriod overrides 5 min (tests).
	CapabilityPeriod time.Duration

	Spool        *spool.Spool
	Lanes        []Lane
	Contributors []StatusContributor
	// EnrollObservers are told each new enroll state (for example, the
	// telemetry lane treats enrolled as an admitted response and
	// early_access_required as a refusal).
	EnrollObservers []func(enroll.State)
	// KickObservers run on every Kick (for example after `kenaz login`),
	// so lanes re-read the session at once instead of on their next tick.
	KickObservers []func()
}

// Daemon is a running relay daemon.
type Daemon struct {
	opt  Options
	log  *slog.Logger
	clk  clock.Clock
	mux  *http.ServeMux
	kick chan struct{}

	mu           sync.Mutex
	info         Info
	clients      map[string]time.Time
	lastActivity time.Time
	hb           *enroll.Heartbeat
	lastBeat     *time.Time
	caps         fleetapi.Capabilities
	me           *fleetapi.MeInfo
	removed      bool
	started      bool
	shutdown     chan string // reason
}

// New prepares a daemon. Mount extra handlers with Handle before Run.
func New(opt Options) *Daemon {
	if opt.Clock == nil {
		opt.Clock = clock.Real{}
	}
	if opt.Logger == nil {
		opt.Logger = slog.New(slog.NewTextHandler(os.Stderr, nil))
	}
	if opt.IdleTimeout <= 0 {
		opt.IdleTimeout = DefaultIdleTimeout
	}
	if len(opt.Ports) == 0 {
		opt.Ports = DefaultPorts
	}
	if opt.CapabilityPeriod <= 0 {
		opt.CapabilityPeriod = DefaultCapabilityPeriod
	}
	if opt.Host.GOOS == "" {
		opt.Host = enroll.LocalHost()
	}
	d := &Daemon{
		opt:      opt,
		log:      opt.Logger,
		clk:      opt.Clock,
		mux:      http.NewServeMux(),
		kick:     make(chan struct{}, 1),
		clients:  map[string]time.Time{},
		shutdown: make(chan string, 1),
	}
	d.mux.HandleFunc("GET /healthz", d.handleHealth)
	d.mux.Handle("GET /control/status", d.authed(d.handleStatus))
	d.mux.Handle("POST /control/clients/{id}", d.authed(d.handleClientPing))
	d.mux.Handle("DELETE /control/clients/{id}", d.authed(d.handleClientBye))
	d.mux.Handle("POST /control/kick", d.authed(d.handleKick))
	d.mux.Handle("POST /control/shutdown", d.authed(d.handleShutdown))
	return d
}

// RequireSession wraps h so it answers 503 while signed out: the
// harness exporter retries or drops, and nothing is buffered before
// consent. Pass it to Handle for ingest endpoints.
func (d *Daemon) RequireSession(h http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if _, err := d.opt.Provider.Auth().Session(r.Context()); err != nil || d.isRemoved() {
			http.Error(w, "kenaz: not signed in", http.StatusServiceUnavailable)
			return
		}
		h.ServeHTTP(w, r)
	})
}

// Handle mounts h (e.g. the OTLP receiver at "POST /v1/logs").
// Every request counts as activity for idle exit. Call before Run.
func (d *Daemon) Handle(pattern string, h http.Handler) {
	d.mux.Handle(pattern, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		d.Touch()
		h.ServeHTTP(w, r)
	}))
}

// Touch records activity (OTLP traffic, tool calls).
func (d *Daemon) Touch() {
	d.mu.Lock()
	d.lastActivity = d.clk.Now()
	d.mu.Unlock()
}

// Kick triggers an immediate heartbeat and capability refresh.
func (d *Daemon) Kick() {
	select {
	case d.kick <- struct{}{}:
	default:
	}
	for _, o := range d.opt.KickObservers {
		o()
	}
}

// Info returns the running daemon's info (zero before Run binds).
func (d *Daemon) Info() Info {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.info
}

func (d *Daemon) isRemoved() bool {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.removed
}

// Run holds the singleton lock, binds, writes daemon.json, starts the
// heartbeat, capability poller and lanes, and serves until ctx ends, a
// shutdown request arrives, or the daemon is idle. It returns
// ErrAlreadyRunning without side effects when another daemon owns the
// state directory.
func (d *Daemon) Run(ctx context.Context) error {
	if err := os.MkdirAll(d.opt.StateDir, 0o700); err != nil {
		return err
	}
	release, err := TryLock(filepath.Join(d.opt.StateDir, LockFile))
	if err != nil {
		return err
	}
	defer release()

	ln, err := listen(d.opt.Ports)
	if err != nil {
		return err
	}
	nonce, err := newNonce()
	if err != nil {
		ln.Close()
		return err
	}
	now := d.clk.Now()
	d.mu.Lock()
	d.info = Info{
		PID:       os.Getpid(),
		Port:      ln.Addr().(*net.TCPAddr).Port,
		Version:   d.opt.Version,
		StartedAt: now,
		Nonce:     nonce,
		Variant:   recordedVariant(),
	}
	d.lastActivity = now
	d.started = true
	info := d.info
	d.mu.Unlock()
	if err := WriteInfo(d.opt.StateDir, info); err != nil {
		ln.Close()
		return err
	}
	defer os.Remove(filepath.Join(d.opt.StateDir, InfoFile))
	d.log.Info("daemon started", "port", info.Port, "flavor", string(d.opt.Provider.Flavor()))

	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	srv := &http.Server{Handler: d.mux, ReadHeaderTimeout: 10 * time.Second}
	serveErr := make(chan error, 1)
	go func() { serveErr <- srv.Serve(ln) }()

	var wg sync.WaitGroup
	wg.Add(2)
	go func() { defer wg.Done(); d.runHeartbeat(ctx) }()
	go func() { defer wg.Done(); d.runCapabilities(ctx) }()
	for _, l := range d.opt.Lanes {
		if err := l.Start(ctx); err != nil {
			d.log.Warn("lane start failed", "lane", l.Name(), "err", err.Error())
		}
	}

	reason := d.wait(ctx, serveErr)
	d.log.Info("daemon stopping", "reason", reason)
	for _, l := range d.opt.Lanes {
		if err := l.Stop(); err != nil {
			d.log.Warn("lane stop failed", "lane", l.Name(), "err", err.Error())
		}
	}
	cancel()
	shCtx, shCancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer shCancel()
	srv.Shutdown(shCtx)
	wg.Wait()
	if d.opt.Spool != nil {
		d.opt.Spool.Close()
	}
	return nil
}

// wait blocks until a stop condition and returns its reason.
func (d *Daemon) wait(ctx context.Context, serveErr <-chan error) string {
	for {
		select {
		case <-ctx.Done():
			return "context"
		case err := <-serveErr:
			if err != nil && !errors.Is(err, http.ErrServerClosed) {
				return "serve_error"
			}
			return "server_closed"
		case r := <-d.shutdown:
			return r
		case <-d.clk.After(idleCheckPeriod):
			if d.idle() {
				return "idle"
			}
		}
	}
}

// idle implements the idle rule: no live MCP client, no activity for IdleTimeout,
// and an empty spool.
func (d *Daemon) idle() bool {
	now := d.clk.Now()
	d.mu.Lock()
	for id, seen := range d.clients {
		if now.Sub(seen) > clientTTL {
			delete(d.clients, id)
		}
	}
	live := len(d.clients)
	quiet := now.Sub(d.lastActivity) >= d.opt.IdleTimeout
	d.mu.Unlock()
	if live > 0 || !quiet {
		return false
	}
	return d.opt.Spool == nil || d.opt.Spool.Empty()
}

func (d *Daemon) runHeartbeat(ctx context.Context) {
	nodeID, err := enroll.LoadOrCreateNodeID(d.opt.StateDir, d.clk.Now())
	if err != nil {
		d.log.Error("node id", "err", err.Error())
		return
	}
	hb := &enroll.Heartbeat{
		Enroller: d.opt.Provider.Enroller(),
		Body: func() (fleetapi.EnrollBody, error) {
			return enroll.BuildBody(d.opt.StateDir, nodeID, d.opt.Version, d.opt.Host)
		},
		Clock:    d.clk,
		Interval: d.opt.HeartbeatInterval,
		Kick:     d.kick,
		OnState: func(s enroll.State, _ fleetapi.EnrollResult) {
			d.log.Info("enroll state", "state", string(s))
			if s == enroll.StateRemoved {
				d.onRemoved()
			}
			for _, o := range d.opt.EnrollObservers {
				o(s)
			}
		},
	}
	d.mu.Lock()
	d.hb = hb
	d.mu.Unlock()
	if err := hb.Run(ctx); err != nil && !errors.Is(err, context.Canceled) && !errors.Is(err, fleetapi.ErrUnofficialBuild) {
		d.log.Warn("heartbeat stopped", "err", err.Error())
	}
}

// onRemoved stops and purges every lane and the spool.
func (d *Daemon) onRemoved() {
	d.mu.Lock()
	d.removed = true
	d.mu.Unlock()
	for _, l := range d.opt.Lanes {
		l.Stop()
		l.Purge()
	}
	if d.opt.Spool != nil {
		d.opt.Spool.Purge()
	}
}

func (d *Daemon) runCapabilities(ctx context.Context) {
	for {
		caps, err := d.opt.Provider.Me().Capabilities(ctx)
		if errors.Is(err, fleetapi.ErrUnofficialBuild) {
			return
		}
		if err == nil {
			me, merr := d.opt.Provider.Me().Get(ctx)
			d.mu.Lock()
			d.caps = caps
			if merr == nil {
				d.me = &me
			}
			d.mu.Unlock()
		}
		select {
		case <-ctx.Done():
			return
		case <-d.clk.After(d.opt.CapabilityPeriod):
		}
	}
}

// Status assembles the current status (also used in-process by tests).
func (d *Daemon) Status(ctx context.Context) Status {
	s := Status{
		SchemaVersion:    StatusSchemaVersion,
		Flavor:           string(d.opt.Provider.Flavor()),
		Env:              string(d.opt.Env),
		Session:          SessionSignedOut,
		Enroll:           string(enroll.StatePending),
		Lanes:            []LaneStatus{},
		PendingApprovals: []string{},
	}
	if d.opt.Provider.Flavor() == fleetapi.FlavorUnofficial {
		s.Session = SessionUnofficial
	} else if _, err := d.opt.Provider.Auth().Session(ctx); err == nil {
		s.Session = SessionSignedIn
	}
	d.mu.Lock()
	in := d.info
	if d.removed {
		s.Session = SessionRemoved
	}
	if d.hb != nil {
		st, _ := d.hb.State()
		s.Enroll = string(st)
		if st == enroll.StateEarlyAccess {
			// The plugin's own text, never Fleet's message.
			s.Notes = append(s.Notes, fleetapi.EarlyAccessMessage, consent.EarlyAccessNothingKept)
		}
	}
	if d.me != nil {
		s.Tier = d.me.Tier
		s.OrgName = d.me.OrgName
		s.OrgPaused = d.me.OrgPaused
		s.PausedCategory = d.me.PausedCategory
	}
	for k, v := range d.caps.Keys {
		if v {
			s.Capabilities = append(s.Capabilities, k)
		}
	}
	d.mu.Unlock()
	sort.Strings(s.Capabilities)
	started := in.StartedAt
	s.Daemon = DaemonStatus{Running: true, PID: in.PID, Port: in.Port, Version: in.Version, StartedAt: &started}
	for _, l := range d.opt.Lanes {
		s.Lanes = append(s.Lanes, l.Status())
	}
	if d.opt.Spool != nil {
		st := d.opt.Spool.Stats()
		s.Spool = &st
	}
	for _, c := range d.opt.Contributors {
		c.ContributeStatus(&s)
	}
	return s
}

// HandleControl registers a local control endpoint behind the daemon's
// nonce bearer (the same check as /control/status). It is used for
// hook → daemon calls such as POST /control/session-end.
func (d *Daemon) HandleControl(pattern string, h http.HandlerFunc) {
	d.mux.Handle(pattern, d.authed(h))
}

// --- HTTP handlers ---

func (d *Daemon) handleHealth(w http.ResponseWriter, r *http.Request) {
	ch := r.URL.Query().Get("challenge")
	if len(ch) < 16 || len(ch) > 128 {
		http.Error(w, "challenge required", http.StatusBadRequest)
		return
	}
	in := d.Info()
	writeJSON(w, healthResponse{Version: in.Version, Proof: proof(in.Nonce, ch)})
}

func (d *Daemon) authed(h http.HandlerFunc) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		want := "Bearer " + d.Info().Nonce
		if subtle.ConstantTimeCompare([]byte(r.Header.Get("Authorization")), []byte(want)) != 1 {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		h(w, r)
	})
}

func (d *Daemon) handleStatus(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, d.Status(r.Context()))
}

func (d *Daemon) handleClientPing(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if id == "" || len(id) > 64 {
		http.Error(w, "bad client id", http.StatusBadRequest)
		return
	}
	now := d.clk.Now()
	d.mu.Lock()
	d.clients[id] = now
	d.lastActivity = now
	d.mu.Unlock()
	w.WriteHeader(http.StatusNoContent)
}

func (d *Daemon) handleClientBye(w http.ResponseWriter, r *http.Request) {
	d.mu.Lock()
	delete(d.clients, r.PathValue("id"))
	d.lastActivity = d.clk.Now()
	d.mu.Unlock()
	w.WriteHeader(http.StatusNoContent)
}

func (d *Daemon) handleKick(w http.ResponseWriter, _ *http.Request) {
	d.Kick()
	w.WriteHeader(http.StatusNoContent)
}

// handleShutdown stops the daemon; ?purge=1 (logout) purges lanes and
// the spool first.
func (d *Daemon) handleShutdown(w http.ResponseWriter, r *http.Request) {
	reason := "requested"
	if r.URL.Query().Get("purge") == "1" {
		reason = "logout"
		for _, l := range d.opt.Lanes {
			l.Stop()
			l.Purge()
		}
		if d.opt.Spool != nil {
			d.opt.Spool.Purge()
		}
	}
	w.WriteHeader(http.StatusNoContent)
	select {
	case d.shutdown <- reason:
	default:
	}
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(v)
}
