package approvals

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base32"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/go-webauthn/webauthn/webauthn"

	"github.com/kameas-ai/kenaz-plugin/internal/clock"
)

// Request is one write a tool wants to make. Prepare stores it verbatim.
type Request struct {
	Operation      string   // e.g. "memory.save", "memory.forget_all", "context.share", "skills.publish", "memory.sync_enable"
	Env, OrgID     string   // the signed-in Fleet environment and organization
	UserID         string   // the signed-in Fleet subject
	TargetIDs      []string // destination identifiers shown on the page (team, slug@version, memory ids)
	Payload        []byte   // canonical JSON; executed verbatim
	IdempotencyKey string   // stable per logical write; a retry with the same key and payload returns the same request
	Summary        string   // plain text shown on the approval page

	// Impact is optional plain text describing deletion or sharing impact
	// ("Deletes 14 memories", "Visible to every member of Platform").
	Impact string
}

// Status is a request's lifecycle state.
type Status string

const (
	StatusPending             Status = "pending"
	StatusApproved            Status = "approved"
	StatusExecuting           Status = "executing"
	StatusDone                Status = "done"
	StatusDenied              Status = "denied"
	StatusExpired             Status = "expired"
	StatusCanceled            Status = "canceled"
	StatusReconciling         Status = "reconciling"
	StatusApprovalUnavailable Status = "approval_unavailable"
	// StatusFailed means the executor ran and the destination refused the
	// write; the result metadata carries a machine error code.
	StatusFailed Status = "failed"
	// StatusOutcomeUnknown closes a reconciling request at sign-out or
	// account switch: whether the write arrived was never confirmed, it
	// will not be retried, and its content is purged.
	StatusOutcomeUnknown Status = "outcome_unknown"
)

// retryable reports whether a request in s ended without effect, so the
// same idempotency key may be prepared again as a new request.
func (s Status) retryable() bool {
	switch s {
	case StatusDenied, StatusExpired, StatusCanceled, StatusFailed:
		return true
	}
	return false
}

// final reports whether a record in s can never execute again; its content
// is purged.
func (s Status) final() bool {
	switch s {
	case StatusDone, StatusDenied, StatusExpired, StatusCanceled, StatusFailed, StatusOutcomeUnknown:
		return true
	}
	return false
}

// Executor performs one approved write. It receives exactly the payload and
// idempotency key that were prepared and approved. It must be idempotent per
// key, or the operation must also register a Reconciler. Return
// ErrOutcomeUnknown (wrapped) when the destination may or may not have
// applied the write; return ErrorCode(code) for a definite refusal.
type Executor func(ctx context.Context, payload []byte, idempotencyKey string) (result []byte, err error)

// Reconciler looks up whether a write whose outcome is unknown (a crash or
// timeout after submission) was applied. applied=true finishes the request
// with result; applied=false lets the relay retry the idempotent executor
// once with the same key. It must not write.
type Reconciler func(ctx context.Context, payload []byte, idempotencyKey string) (result []byte, applied bool, err error)

// Service is the approval service. The relay daemon's Manager implements
// it; executors are registered there.
type Service interface {
	Register(operation string, exec Executor)
	Prepare(ctx context.Context, r Request) (requestID string, st Status, err error)
	Status(ctx context.Context, requestID string) (Status, []byte /*bounded result metadata*/, error)
}

// Preparer is what a tool process needs: prepare a request, read its
// status, and get the page link to show the user. Manager and RemoteClient
// implement it. It deliberately has no approve method.
type Preparer interface {
	Prepare(ctx context.Context, r Request) (requestID string, st Status, err error)
	Status(ctx context.Context, requestID string) (Status, []byte, error)
	ApprovalURL(ctx context.Context, requestID string) (string, error)
}

// Identity is the signed-in Fleet subject and profile a request and a
// credential are bound to.
type Identity struct {
	Env    string `json:"env"`
	OrgID  string `json:"org_id"`
	UserID string `json:"user_id"`
}

func (i Identity) valid() bool { return i.Env != "" && i.UserID != "" }
func (i Identity) key() string { return i.Env + "\x00" + i.OrgID + "\x00" + i.UserID }

// IdentitySource returns the active session's identity without network
// I/O, or an error when signed out.
type IdentitySource func(ctx context.Context) (Identity, error)

// IdentityVerifier runs a fresh, browser-completed Fleet sign-in (its own
// PKCE flow, forced re-authentication) and returns the verified subject without changing the active
// session. openURL receives the authorization URL; the approval page shows
// it to the user. It is the bootstrap proof for first enrollment and
// recovery; an agent-supplied bearer is never accepted instead.
//
// completeURL is where the provider's redirect must finally send the
// browser that completed sign-in (after the loopback callback). It carries
// a one-time token that binds the enrollment ceremony to that browser; it
// must never be shown, logged or returned anywhere else.
type IdentityVerifier interface {
	VerifyIdentity(ctx context.Context, openURL func(string) error, completeURL string) (Identity, error)
}

// Limits and timings.
const (
	DefaultTTL       = 5 * time.Minute
	MaxPayload       = 1 << 20
	MaxResult        = 4 << 10
	MaxSummary       = 8 << 10
	MaxTargets       = 64
	MaxKey           = 256
	retainFinal      = 24 * time.Hour
	retainUnresolved = 7 * 24 * time.Hour
	execTimeout      = 2 * time.Minute
	sweepPeriod      = 30 * time.Second
)

// Errors.
var (
	ErrNotFound            = errors.New("approval request not found")
	ErrConflict            = errors.New("approval request is not in the expected state")
	ErrUnknownOperation    = errors.New("no executor is registered for this operation")
	ErrInvalidRequest      = errors.New("invalid approval request")
	ErrIdentityMismatch    = errors.New("request identity does not match the signed-in account")
	ErrNotSignedIn         = errors.New("not signed in")
	ErrIdempotencyConflict = errors.New("idempotency key was already used with a different payload")
	ErrOutcomeUnknown      = errors.New("write outcome unknown")
	ErrUnavailable         = errors.New("approval_unavailable: no authenticator is enrolled and this build cannot enroll one")
	ErrEnrollmentRequired  = errors.New("enrollment_required: set up an authenticator first")
	ErrChallenge           = errors.New("approval challenge is missing, used or stale")
	ErrAssertion           = errors.New("authenticator assertion rejected")
	ErrEnrollmentRace      = errors.New("another authenticator was registered at the same time; start again")
	ErrExpired             = errors.New("approval request expired")
)

// codedError carries a machine error code from an executor.
type codedError struct{ code string }

func (e codedError) Error() string { return "write refused: " + e.code }

var codeRe = regexp.MustCompile(`^[a-z][a-z0-9_]{0,63}$`)

// ErrorCode returns an executor error carrying a short machine code
// ([a-z0-9_], at most 64 chars). The code, and nothing else from the error,
// reaches status output.
func ErrorCode(code string) error {
	if !codeRe.MatchString(code) {
		code = "execution_failed"
	}
	return codedError{code}
}

func errCode(err error) string {
	var ce codedError
	if errors.As(err, &ce) {
		return ce.code
	}
	return "execution_failed"
}

// Options configure a Manager.
type Options struct {
	// Dir holds the pending-write store (created 0700).
	Dir string
	// Identity reports the active session (required).
	Identity IdentitySource
	// Verifier performs browser identity verification for enrollment;
	// nil means this build cannot enroll (approval_unavailable unless an
	// authenticator is already enrolled).
	Verifier IdentityVerifier
	// BaseURL returns the page origin, e.g. "http://localhost:4318".
	BaseURL func() string
	Clock   clock.Clock
	Logger  *slog.Logger
	// RPDisplayName is shown by the authenticator ("Kenaz").
	RPDisplayName string
	// TTL overrides DefaultTTL (tests only).
	TTL time.Duration
	// StoreKey authenticates the pending-write store and the enrolled
	// credentials (HMAC-SHA256, at least 32 bytes). It must come from
	// outside user-writable files (the OS keychain; this stops MCP-only and
	// file-writing agents, not a same-user shell agent, which can read the
	// keychain item without a prompt). Empty means no
	// trustworthy storage: every write reports approval_unavailable.
	StoreKey []byte
}

// Rendered is what the approval page shows for a request. The daemon
// derives it from the stored payload with the operation's Renderer; text a
// caller sends with Prepare is never shown.
type Rendered struct {
	Summary string
	Impact  string
	Targets []string
	// Details are the items the request acts on with their full text
	// (for example each note's body). The page shows a short preview of
	// each with a control that reveals the full text. They must be derived
	// from the payload, so the approved payload hash binds them.
	Details []Detail
}

// Detail is one item a request acts on.
type Detail struct {
	Target string `json:"target"`
	Label  string `json:"label"`
	Text   string `json:"text,omitempty"`
}

// Renderer describes a payload for the approval page. It must be a pure
// function of the payload bytes, and must refuse a payload its executor
// would not accept.
type Renderer func(payload []byte) (Rendered, error)

// Manager is the relay-side approval service.
type Manager struct {
	opt Options
	clk clock.Clock
	log *slog.Logger
	st  *store

	mu         sync.Mutex
	execs      map[string]Executor
	recons     map[string]Reconciler
	renderers  map[string]Renderer
	challenges map[string]*challenge  // by request id + page session, one-use
	receipts   map[string][]byte      // by request id, one-use, memory only
	enrolls    map[string]*enrollment // by enrollment id
	csrf       map[string]time.Time   // issued page tokens
	running    map[string]bool        // request ids whose executor is running
}

// New opens the store and recovers interrupted records: approved requests
// whose process died before executing are canceled (their receipt was in
// memory), and executing requests become reconciling.
func New(opt Options) (*Manager, error) {
	if opt.Identity == nil {
		return nil, errors.New("approvals: Identity is required")
	}
	if opt.Clock == nil {
		opt.Clock = clock.Real{}
	}
	if opt.Logger == nil {
		opt.Logger = slog.New(slog.DiscardHandler)
	}
	if opt.RPDisplayName == "" {
		opt.RPDisplayName = "Kenaz"
	}
	if opt.TTL <= 0 {
		opt.TTL = DefaultTTL
	}
	st, err := openStore(opt.Dir, opt.StoreKey)
	if err != nil {
		return nil, err
	}
	if st.rejected {
		opt.Logger.Warn("approvals store failed authentication; set aside and not used")
	}
	m := &Manager{
		opt: opt, clk: opt.Clock, log: opt.Logger, st: st,
		execs: map[string]Executor{}, recons: map[string]Reconciler{}, renderers: map[string]Renderer{},
		challenges: map[string]*challenge{}, receipts: map[string][]byte{},
		enrolls: map[string]*enrollment{}, csrf: map[string]time.Time{},
		running: map[string]bool{},
	}
	now := m.clk.Now()
	err = st.update(func(s *state) error {
		for _, r := range s.Records {
			switch r.Status {
			case StatusApproved:
				r.Status, r.UpdatedAt = StatusCanceled, now
				r.purgeContent()
			case StatusExecuting:
				r.Status, r.UpdatedAt = StatusReconciling, now
				r.ReceiptSHA256 = ""
			}
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return m, nil
}

// Register installs the executor for operation. Call before serving.
func (m *Manager) Register(operation string, exec Executor) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.execs[operation] = exec
}

// RegisterRenderer installs the page renderer for operation. Prepare
// refuses operations without one.
func (m *Manager) RegisterRenderer(operation string, r Renderer) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.renderers[operation] = r
}

func (m *Manager) renderer(op string) Renderer {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.renderers[op]
}

// trusted reports whether this Manager has authenticated storage.
func (m *Manager) trusted() bool { return len(m.st.key) > 0 }

// RegisterReconciler installs the outcome lookup for operation.
func (m *Manager) RegisterReconciler(operation string, rec Reconciler) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.recons[operation] = rec
}

func (m *Manager) executor(op string) (Executor, Reconciler) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.execs[op], m.recons[op]
}

func newID(prefix string) (string, error) {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return prefix + strings.ToLower(base32.StdEncoding.WithPadding(base32.NoPadding).EncodeToString(b)), nil
}

var idRe = regexp.MustCompile(`^apr_[a-z2-7]{26}$`)

// ValidID reports whether s has the shape of a request id.
func ValidID(s string) bool { return idRe.MatchString(s) }

func validate(r Request) error {
	switch {
	case r.Operation == "" || len(r.Operation) > 64 || !opRe.MatchString(r.Operation):
		return fmt.Errorf("%w: operation", ErrInvalidRequest)
	case len(r.Payload) == 0 || len(r.Payload) > MaxPayload || !json.Valid(r.Payload):
		return fmt.Errorf("%w: payload must be JSON of at most %d bytes", ErrInvalidRequest, MaxPayload)
	case strings.TrimSpace(r.IdempotencyKey) == "" || len(r.IdempotencyKey) > MaxKey:
		return fmt.Errorf("%w: idempotency key", ErrInvalidRequest)
	}
	return nil
}

func validRendered(v Rendered) error {
	switch {
	case strings.TrimSpace(v.Summary) == "" || len(v.Summary) > MaxSummary || !utf8.ValidString(v.Summary):
		return fmt.Errorf("%w: rendered summary", ErrInvalidRequest)
	case len(v.Impact) > MaxSummary || !utf8.ValidString(v.Impact):
		return fmt.Errorf("%w: rendered impact", ErrInvalidRequest)
	case len(v.Targets) > MaxTargets:
		return fmt.Errorf("%w: too many targets", ErrInvalidRequest)
	case len(v.Details) > MaxTargets:
		return fmt.Errorf("%w: too many details", ErrInvalidRequest)
	}
	total := 0
	for _, d := range v.Details {
		total += len(d.Target) + len(d.Label) + len(d.Text)
		if len(d.Target) > 256 || len(d.Label) > MaxSummary || !utf8.ValidString(d.Target+d.Label+d.Text) {
			return fmt.Errorf("%w: detail", ErrInvalidRequest)
		}
	}
	if total > MaxPayload {
		return fmt.Errorf("%w: details too large", ErrInvalidRequest)
	}
	for _, t := range v.Targets {
		if len(t) > 256 || !utf8.ValidString(t) {
			return fmt.Errorf("%w: target id", ErrInvalidRequest)
		}
	}
	return nil
}

var opRe = regexp.MustCompile(`^[a-z][a-z0-9_]*(\.[a-z][a-z0-9_]*)+$`)

// current returns the active identity and applies account-switch and
// sign-out invalidation: if the session changed since the generation was
// bound, every open request is canceled and the generation bumped.
func (m *Manager) current(ctx context.Context) (Identity, uint64, error) {
	id, err := m.opt.Identity(ctx)
	if err == nil && !id.valid() {
		err = ErrNotSignedIn
	}
	now := m.clk.Now()
	var gen uint64
	uerr := m.st.update(func(s *state) error {
		switch {
		case err != nil:
			if s.Bound != nil || hasOpen(s) || hasReconciling(s) {
				signOutState(s, now)
			} else {
				return errNoChange
			}
		case s.Bound != nil && *s.Bound == id:
			gen = s.ProfileGen
			return errNoChange
		case s.Bound == nil:
			if hasOpen(s) { // prepared under an unknown profile
				cancelOpen(s, now, nil)
				s.ProfileGen++
			}
			b := id
			s.Bound = &b
		case *s.Bound != id:
			signOutState(s, now)
			b := id
			s.Bound = &b
		}
		gen = s.ProfileGen
		return nil
	})
	if err != nil {
		m.dropEphemeral()
		return Identity{}, 0, fmt.Errorf("%w: %v", ErrNotSignedIn, err)
	}
	if uerr != nil {
		return Identity{}, 0, uerr
	}
	return id, gen, nil
}

func hasReconciling(s *state) bool {
	for _, r := range s.Records {
		if r.Status == StatusReconciling {
			return true
		}
	}
	return false
}

func hasOpen(s *state) bool {
	for _, r := range s.Records {
		if r.Status == StatusPending || r.Status == StatusApproved {
			return true
		}
	}
	return false
}

// dropEphemeral forgets every challenge, receipt and enrollment ceremony.
func (m *Manager) dropEphemeral() {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.challenges = map[string]*challenge{}
	m.receipts = map[string][]byte{}
	for _, e := range m.enrolls {
		if e.cancel != nil {
			e.cancel() // stop its identity sign-in; it can never complete
		}
	}
	m.enrolls = map[string]*enrollment{}
}

func credentialsFor(s *state, id Identity) []webauthn.Credential {
	var out []webauthn.Credential
	for _, c := range s.Credentials {
		if c.Identity == id {
			out = append(out, c.Credential)
		}
	}
	return out
}

// Prepare stores r as a pending write and returns its opaque id. It never
// writes to the destination. A retry with the same operation, identity,
// idempotency key and payload returns the existing request; the same key
// with a different payload is refused. Without an enrolled authenticator
// and without a way to enroll one, it stores nothing and returns
// StatusApprovalUnavailable.
func (m *Manager) Prepare(ctx context.Context, r Request) (string, Status, error) {
	if err := validate(r); err != nil {
		return "", "", err
	}
	if exec, _ := m.executor(r.Operation); exec == nil {
		return "", "", fmt.Errorf("%w: %s", ErrUnknownOperation, r.Operation)
	}
	render := m.renderer(r.Operation)
	if render == nil {
		return "", "", fmt.Errorf("%w: %s", ErrUnknownOperation, r.Operation)
	}
	// The page shows only what the daemon derives from the payload.
	view, err := render(r.Payload)
	if err != nil {
		return "", "", fmt.Errorf("%w: payload: %v", ErrInvalidRequest, err)
	}
	if err := validRendered(view); err != nil {
		return "", "", err
	}
	if !m.trusted() {
		return "", StatusApprovalUnavailable, nil
	}
	cur, gen, err := m.current(ctx)
	if err != nil {
		return "", "", err
	}
	want := Identity{Env: r.Env, OrgID: r.OrgID, UserID: r.UserID}
	if want != cur {
		return "", "", ErrIdentityMismatch
	}
	m.sweep(ctx)
	hash := payloadHash(r.Payload)
	id, err := newID("apr_")
	if err != nil {
		return "", "", err
	}
	now := m.clk.Now()
	var outID string
	var outSt Status
	err = m.st.update(func(s *state) error {
		for _, ex := range s.Records {
			if ex.Operation == r.Operation && ex.Identity == cur && ex.IdempotencyKey == r.IdempotencyKey && !ex.Status.retryable() {
				if ex.PayloadSHA256 != hash {
					return ErrIdempotencyConflict
				}
				outID, outSt = ex.ID, ex.Status
				return nil
			}
		}
		if len(credentialsFor(s, cur)) == 0 && m.opt.Verifier == nil {
			outSt = StatusApprovalUnavailable
			return nil
		}
		s.Records[id] = &record{
			ID: id, Operation: r.Operation, Identity: cur,
			TargetIDs: append([]string(nil), view.Targets...), Payload: append([]byte(nil), r.Payload...),
			PayloadSHA256: hash, IdempotencyKey: r.IdempotencyKey, Summary: view.Summary, Impact: view.Impact,
			Details: append([]Detail(nil), view.Details...),
			Status:  StatusPending, ProfileGen: gen, CreatedAt: now, UpdatedAt: now, ExpiresAt: now.Add(m.opt.TTL),
		}
		outID, outSt = id, StatusPending
		return nil
	})
	if err != nil {
		return "", "", err
	}
	if outSt == StatusApprovalUnavailable {
		return "", outSt, nil
	}
	m.log.Info("approval prepared", "op", r.Operation, "request", outID, "state", string(outSt))
	return outID, outSt, nil
}

// Status returns a request's state and bounded result metadata: the
// executor's result when done, {"error": code} when failed. It never
// returns the payload.
func (m *Manager) Status(ctx context.Context, requestID string) (Status, []byte, error) {
	m.current(ctx) // applies sign-out / account-switch invalidation
	m.sweep(ctx)
	var st Status
	var res []byte
	found := false
	m.st.view(func(s *state) {
		if r, ok := s.Records[requestID]; ok {
			found, st, res = true, r.Status, append([]byte(nil), r.Result...)
		}
	})
	if !found {
		return "", nil, ErrNotFound
	}
	return st, res, nil
}

// ApprovalURL returns the page link for requestID.
func (m *Manager) ApprovalURL(_ context.Context, requestID string) (string, error) {
	if !ValidID(requestID) {
		return "", ErrNotFound
	}
	if m.opt.BaseURL == nil {
		return "", errors.New("approval page is not served")
	}
	return m.opt.BaseURL() + PagePrefix + "r/" + requestID, nil
}

// Info is a request listing entry. It carries no payload or summary text.
type Info struct {
	RequestID string    `json:"request_id"`
	Operation string    `json:"operation"`
	Status    Status    `json:"status"`
	CreatedAt time.Time `json:"created_at"`
	ExpiresAt time.Time `json:"expires_at"`
	UpdatedAt time.Time `json:"updated_at"`
}

// List returns every retained request, newest first.
func (m *Manager) List(ctx context.Context) []Info {
	m.current(ctx)
	m.sweep(ctx)
	var out []Info
	m.st.view(func(s *state) {
		for _, r := range s.Records {
			out = append(out, Info{RequestID: r.ID, Operation: r.Operation, Status: r.Status, CreatedAt: r.CreatedAt, ExpiresAt: r.ExpiresAt, UpdatedAt: r.UpdatedAt})
		}
	})
	sort.Slice(out, func(i, j int) bool { return out[i].CreatedAt.After(out[j].CreatedAt) })
	return out
}

// Enrolled reports whether the signed-in identity has an authenticator.
func (m *Manager) Enrolled(ctx context.Context) bool {
	cur, _, err := m.current(ctx)
	if err != nil {
		return false
	}
	n := 0
	m.st.view(func(s *state) { n = len(credentialsFor(s, cur)) })
	return n > 0
}

// SignOut cancels every request that has not started executing, purges
// their content, forgets challenges, receipts and enrollment ceremonies and
// bumps the profile generation (logout, node removal).
func (m *Manager) SignOut() {
	m.dropEphemeral()
	now := m.clk.Now()
	if err := m.st.update(func(s *state) error {
		signOutState(s, now)
		return nil
	}); err != nil {
		m.log.Warn("approvals sign-out not saved", "err", err.Error())
	}
}

// sweep expires overdue requests, drops old final records and resolves
// reconciling ones.
func (m *Manager) sweep(ctx context.Context) {
	now := m.clk.Now()
	var reconcile []string
	_ = m.st.update(func(s *state) error {
		changed := false
		for id, r := range s.Records {
			switch {
			case (r.Status == StatusPending || r.Status == StatusApproved) && !now.Before(r.ExpiresAt):
				r.Status, r.UpdatedAt = StatusExpired, now
				r.purgeContent()
				changed = true
			case r.Status.final() && now.Sub(r.UpdatedAt) > retainFinal:
				delete(s.Records, id)
				changed = true
			case r.Status == StatusReconciling:
				if now.Sub(r.UpdatedAt) > retainUnresolved {
					// Nothing resolved it in a week (no reconciler, or the
					// destination never answered): it ends as failed with
					// an unknown outcome instead of reconciling forever,
					// its content dropped. It is never re-executed.
					r.Status, r.UpdatedAt, r.Result = StatusFailed, now, errResult("outcome_unknown")
					r.purgeContent()
					changed = true
					continue
				}
				if len(r.Payload) > 0 {
					reconcile = append(reconcile, id)
				}
			}
		}
		if !changed {
			return errNoChange
		}
		return nil
	})
	m.mu.Lock()
	for id, c := range m.challenges {
		if !now.Before(c.expires) {
			delete(m.challenges, id)
		}
	}
	for t, exp := range m.csrf {
		if !now.Before(exp) {
			delete(m.csrf, t)
		}
	}
	for id, e := range m.enrolls {
		if !now.Before(e.expires) {
			delete(m.enrolls, id)
		}
	}
	m.mu.Unlock()
	for _, id := range reconcile {
		m.reconcile(ctx, id)
	}
}

// Run sweeps periodically until ctx ends.
func (m *Manager) Run(ctx context.Context) {
	for {
		m.current(ctx)
		m.sweep(ctx)
		select {
		case <-ctx.Done():
			return
		case <-m.clk.After(sweepPeriod):
		}
	}
}

// claimRun marks id as having an executor or reconciler in flight in this
// process; false means one is already running.
func (m *Manager) claimRun(id string) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.running[id] {
		return false
	}
	m.running[id] = true
	return true
}

func (m *Manager) releaseRun(id string) {
	m.mu.Lock()
	delete(m.running, id)
	m.mu.Unlock()
}

func bounded(b []byte) []byte {
	if len(b) > MaxResult || (len(b) > 0 && !json.Valid(b)) {
		return []byte(`{"truncated":true}`)
	}
	return append([]byte(nil), b...)
}

func errResult(code string) []byte {
	b, _ := json.Marshal(map[string]string{"error": code})
	return b
}

// execute consumes the one-use receipt for an approved request, rechecks
// identity, generation and payload hash, moves it to executing (CAS) and
// runs the executor with the stored payload and key.
func (m *Manager) execute(ctx context.Context, id string, receipt []byte) Status {
	if !m.claimRun(id) {
		return StatusExecuting
	}
	defer m.releaseRun(id)
	m.mu.Lock()
	held, ok := m.receipts[id]
	delete(m.receipts, id)
	m.mu.Unlock()
	if !ok || len(receipt) == 0 || !equalBytes(held, receipt) {
		return m.cancel(id, "receipt")
	}
	rh := sha256.Sum256(receipt)
	cur, gen, err := m.current(ctx)
	if err != nil {
		return m.cancel(id, "signed_out")
	}
	verified, err := m.provenRecord(id, StatusApproved)
	if err != nil {
		return m.cancel(id, "proof")
	}
	now := m.clk.Now()
	rec, err := m.st.transition(id, []Status{StatusApproved}, StatusExecuting, now, func(r *record) error {
		switch {
		case !sameProof(r.Proof, verified.Proof) || r.PayloadSHA256 != verified.PayloadSHA256:
			return ErrAssertion
		case r.ReceiptSHA256 != hex.EncodeToString(rh[:]):
			return ErrChallenge
		case r.Identity != cur || r.ProfileGen != gen:
			return ErrIdentityMismatch
		case payloadHash(r.Payload) != r.PayloadSHA256:
			return ErrAssertion
		case !now.Before(r.ExpiresAt):
			return ErrExpired
		}
		r.ReceiptSHA256 = "" // consumed
		r.Attempts++
		return nil
	})
	if err != nil {
		if errors.Is(err, ErrConflict) {
			st, _, _ := m.Status(ctx, id)
			return st
		}
		return m.cancel(id, "recheck_failed")
	}
	return m.run(ctx, rec)
}

// provenRecord returns a copy of record id in state want after
// re-verifying its approval proof.
func (m *Manager) provenRecord(id string, want Status) (*record, error) {
	var rec *record
	m.st.view(func(s *state) {
		if r, ok := s.Records[id]; ok && r.Status == want {
			rec = r.clone()
		}
	})
	if rec == nil {
		return nil, ErrConflict
	}
	if err := m.verifyProof(rec); err != nil {
		m.log.Warn("approval proof rejected", "request", id)
		return nil, err
	}
	return rec, nil
}

func (m *Manager) cancel(id, why string) Status {
	if _, err := m.st.transition(id, []Status{StatusPending, StatusApproved}, StatusCanceled, m.clk.Now(), nil); err == nil {
		m.log.Warn("approval canceled", "request", id, "reason", why)
		return StatusCanceled
	}
	st := StatusCanceled
	m.st.view(func(s *state) {
		if r, ok := s.Records[id]; ok {
			st = r.Status
		}
	})
	return st
}

// run calls the executor for a record already in executing.
func (m *Manager) run(ctx context.Context, rec *record) Status {
	exec, _ := m.executor(rec.Operation)
	if exec == nil {
		_, _ = m.st.transition(rec.ID, []Status{StatusExecuting}, StatusFailed, m.clk.Now(), func(r *record) error {
			r.Result = errResult("operation_unavailable")
			return nil
		})
		return StatusFailed
	}
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), execTimeout)
	defer cancel()
	ctx = ContextWithIdentity(ctx, rec.Identity)
	res, err := exec(ctx, rec.Payload, rec.IdempotencyKey)
	now := m.clk.Now()
	switch {
	case err == nil:
		_, _ = m.st.transition(rec.ID, []Status{StatusExecuting}, StatusDone, now, func(r *record) error {
			r.Result = bounded(res)
			return nil
		})
		m.log.Info("approval executed", "op", rec.Operation, "request", rec.ID)
		return StatusDone
	case errors.Is(err, ErrOutcomeUnknown) || errors.Is(err, context.DeadlineExceeded) || errors.Is(err, context.Canceled):
		_, _ = m.st.transition(rec.ID, []Status{StatusExecuting}, StatusReconciling, now, nil)
		m.log.Warn("approval outcome unknown", "op", rec.Operation, "request", rec.ID)
		return StatusReconciling
	default:
		code := errCode(err)
		_, _ = m.st.transition(rec.ID, []Status{StatusExecuting}, StatusFailed, now, func(r *record) error {
			r.Result = errResult(code)
			return nil
		})
		m.log.Warn("approval execution refused", "op", rec.Operation, "request", rec.ID, "code", code)
		return StatusFailed
	}
}

// reconcile resolves a reconciling record: it never repeats a write
// blindly. Without a Reconciler the record stays reconciling for the user
// to check. With one, an applied write finishes as done; a write that was
// not applied is retried once through the idempotent executor.
func (m *Manager) reconcile(ctx context.Context, id string) {
	if !m.claimRun(id) {
		return
	}
	defer m.releaseRun(id)
	rec, err := m.provenRecord(id, StatusReconciling)
	if err != nil || len(rec.Payload) == 0 {
		return // never act on a record without a verified approval
	}
	_, recon := m.executor(rec.Operation)
	if recon == nil {
		return
	}
	cur, gen, cerr := m.current(ctx)
	if cerr != nil || cur != rec.Identity || gen != rec.ProfileGen || payloadHash(rec.Payload) != rec.PayloadSHA256 {
		return // the user must sign back in as the same account first
	}
	rctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), execTimeout)
	res, applied, err := recon(rctx, rec.Payload, rec.IdempotencyKey)
	cancel()
	if err != nil {
		return
	}
	now := m.clk.Now()
	if applied {
		_, _ = m.st.transition(id, []Status{StatusReconciling}, StatusDone, now, func(r *record) error {
			if !sameProof(r.Proof, rec.Proof) {
				return ErrAssertion
			}
			r.Result = bounded(res)
			return nil
		})
		return
	}
	next, err := m.st.transition(id, []Status{StatusReconciling}, StatusExecuting, now, func(r *record) error {
		if !sameProof(r.Proof, rec.Proof) || r.PayloadSHA256 != rec.PayloadSHA256 {
			return ErrAssertion
		}
		if r.Attempts >= 3 {
			return ErrConflict
		}
		r.Attempts++
		return nil
	})
	if err != nil {
		return
	}
	m.run(ctx, next)
}

func equalBytes(a, b []byte) bool {
	if len(a) != len(b) {
		return false
	}
	var v byte
	for i := range a {
		v |= a[i] ^ b[i]
	}
	return v == 0
}

type identityKey struct{}

// RecordIdentity returns the identity of the approved request an executor
// is running for. Executors compare it with the provider's current session
// right before writing and refuse on any difference (account switch after
// approval).
func RecordIdentity(ctx context.Context) (Identity, bool) {
	id, ok := ctx.Value(identityKey{}).(Identity)
	return id, ok
}

// ContextWithIdentity returns ctx carrying the approved request's identity,
// as the Manager passes it to an executor (also for executor tests).
func ContextWithIdentity(ctx context.Context, id Identity) context.Context {
	return context.WithValue(ctx, identityKey{}, id)
}

// ErrAccountChanged is the executor refusal when the session no longer
// matches the approved request's identity.
var ErrAccountChanged = ErrorCode("account_changed")

// CheckSession is the executor-side recheck: it fails unless the session
// identity (env, org, subject) equals the approved request's.
func CheckSession(ctx context.Context, env, orgID, userID string) error {
	want, ok := RecordIdentity(ctx)
	if !ok || want != (Identity{Env: env, OrgID: orgID, UserID: userID}) {
		return ErrAccountChanged
	}
	return nil
}
