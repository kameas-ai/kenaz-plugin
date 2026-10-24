package fleetapitest

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/kameas-ai/kenaz-plugin/fleetapi"
)

// MemoryFleet is an in-memory Kameas Fleet memory store that implements
// the server contract (docs/contract-harness-memory.md and the Fleet
// service's memory package): per-user isolation keyed by the token's org
// and user only, capability gate, per-item push outcomes with the merge
// rules (aliases, absorbing tombstones, content dedupe, narrative
// precedence, per-field LWW, left_sync_scope, quotas, credential scan,
// clock skew), exclusive cursors with the snapshot "s<seq>.<floor>.<erase>"
// form, cursor floor resets (cursor_expired, erased), forget, forget-all,
// settings and export.
//
// Clients are per identity (Client) or staff (StaffClient). Fault injects
// failures before or after the server applies a call, which is how tests
// model "server committed, client never heard back". Handler serves the
// same store over HTTP with Fleet's exact routes and envelopes, for an
// adapter's transport contract tests.
type MemoryFleet struct {
	mu sync.Mutex
	// Now is the server clock (default time.Now).
	Now func() time.Time
	// Entitled reports whether org's tier has memory_sync (default: yes).
	Entitled func(org string) bool
	// MaxRecords / MaxBytes / MaxTombstones are the per-user quotas
	// (defaults 20000 / 50 MiB / 100000).
	MaxRecords    int
	MaxBytes      int64
	MaxTombstones int
	// Fault, when set, is called before (after=false) and after
	// (after=true) the server applies call ("push", "pull", "settings",
	// "put_settings", "forget", "forget_all", "export"); a non-nil error is
	// returned to the client. An error after=true means the server state
	// changed but the client sees a failure.
	Fault func(call string, after bool) error

	// Env is the environment Client's credentials belong to (default
	// dev).
	Env fleetapi.Env

	users   map[string]*memUser
	calls   map[string]int
	refused int
}

// NewMemoryFleet returns an empty store.
func NewMemoryFleet() *MemoryFleet {
	return &MemoryFleet{users: map[string]*memUser{}, calls: map[string]int{}}
}

// Calls returns how many times call reached the server (after auth).
func (f *MemoryFleet) Calls(call string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.calls[call]
}

type memRec struct {
	seq                                   int64
	state                                 string
	content, hash, kind, turnID           string
	source, sourceTurn, toolName          string
	filesRead, filesModified              []string
	createdAt                             string
	weight                                float64
	size                                  int
	title, titleHLC                       string
	pinned                                bool
	pinnedHLC, scope, scopeHLC            string
	lastAccessed                          string
	reason, supersededBy                  string
	recall                                map[string]int64
	hasTitle, hasPinned, hasScope, hasAcc bool
}

type memUser struct {
	enabled      bool
	scopes       []string
	consent      string
	optedInAt    string
	nextSeq      int64
	floor        int64
	erasedAtSeq  int64
	erasedBefore string
	records      map[string]*memRec
	aliases      map[string]string
	liveRecords  int
	liveBytes    int64
}

func (u *memUser) scopeEnabled(s string) bool {
	if s != "long_term" && s != "global" {
		return false
	}
	for _, x := range u.scopes {
		if x == s {
			return true
		}
	}
	return false
}

func (f *MemoryFleet) now() time.Time {
	if f.Now != nil {
		return f.Now().UTC()
	}
	return time.Now().UTC()
}

func (f *MemoryFleet) user(org, user string) *memUser {
	k := org + "\x00" + user
	u := f.users[k]
	if u == nil {
		u = &memUser{scopes: []string{"long_term", "global"}, nextSeq: 1, records: map[string]*memRec{}, aliases: map[string]string{}}
		f.users[k] = u
	}
	return u
}

func apiErr(status int, code, msg string) *fleetapi.APIError {
	return &fleetapi.APIError{Status: status, Code: code, Message: msg}
}

// RateLimited is the error Fleet returns for 429 (for Fault).
func RateLimited(retryAfterSeconds int) error {
	return &fleetapi.APIError{Status: 429, Code: fleetapi.CodeRateLimited, Message: "too many requests; back off and retry later",
		Details: map[string]string{"retry_after_seconds": strconv.Itoa(retryAfterSeconds)}}
}

// Client returns the memory client of one signed-in member, in env
// f.Env (default dev). Like a real adapter it enforces the account
// binding: a call whose ctx carries no fleetapi.ExpectedSession, or one
// that is not this member, is refused with identity_mismatch and reaches
// nothing. Use ContextFor to build a ctx for direct test calls.
func (f *MemoryFleet) Client(org, user string) fleetapi.Memory {
	env := f.env()
	return &boundClient{f: f, creds: func(context.Context) (fleetapi.Session, error) {
		return fleetapi.Session{Env: env, OrgID: org, UserID: user}, nil
	}}
}

// SessionClient returns a client that, like the official adapter, sends
// each request with whichever session is active when the request is made
// (session is called once per request to pick the credentials). It
// enforces the account binding against that session.
func (f *MemoryFleet) SessionClient(session func(context.Context) (fleetapi.Session, error)) fleetapi.Memory {
	return &boundClient{f: f, creds: session}
}

// ContextFor returns ctx bound to the member (org, user) in f.Env, for
// direct calls on Client in tests.
func (f *MemoryFleet) ContextFor(ctx context.Context, org, user string) context.Context {
	return fleetapi.WithExpectedSession(ctx, fleetapi.ExpectedSession{Env: f.env(), OrgID: org, Subject: user})
}

// Refused returns how many calls the account binding refused (nothing
// was sent for them).
func (f *MemoryFleet) Refused() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.refused
}

func (f *MemoryFleet) env() fleetapi.Env {
	if f.Env != "" {
		return f.Env
	}
	return fleetapi.EnvDev
}

// server is Fleet's own view of a member: no client-side binding (the
// HTTP handler uses it; the binding is a client concern).
func (f *MemoryFleet) server(org, user string) *memClient {
	return &memClient{f: f, org: org, user: user}
}

// boundClient is the client side: it picks credentials, checks the
// ExpectedSession, and only then lets the request reach the server.
type boundClient struct {
	f     *MemoryFleet
	creds func(context.Context) (fleetapi.Session, error)
}

func (b *boundClient) bind(ctx context.Context) (*memClient, error) {
	s, err := b.creds(ctx)
	if err != nil {
		return nil, fleetapi.ErrNotSignedIn
	}
	want, ok := fleetapi.ExpectedSessionFrom(ctx)
	if !ok || !want.Matches(s) {
		b.f.mu.Lock()
		b.f.refused++
		b.f.mu.Unlock()
		return nil, fleetapi.IdentityMismatch()
	}
	return b.f.server(s.OrgID, s.UserID), nil
}

func (b *boundClient) Settings(ctx context.Context) (fleetapi.MemorySettings, error) {
	c, err := b.bind(ctx)
	if err != nil {
		return fleetapi.MemorySettings{}, err
	}
	return c.Settings(ctx)
}

func (b *boundClient) PutSettings(ctx context.Context, u fleetapi.MemorySettingsUpdate) (fleetapi.MemorySettings, error) {
	c, err := b.bind(ctx)
	if err != nil {
		return fleetapi.MemorySettings{}, err
	}
	return c.PutSettings(ctx, u)
}

func (b *boundClient) Pull(ctx context.Context, r fleetapi.MemoryPullRequest) (fleetapi.MemoryPullResponse, error) {
	c, err := b.bind(ctx)
	if err != nil {
		return fleetapi.MemoryPullResponse{}, err
	}
	return c.Pull(ctx, r)
}

func (b *boundClient) Push(ctx context.Context, r fleetapi.MemoryPushRequest) (fleetapi.MemoryPushResponse, error) {
	c, err := b.bind(ctx)
	if err != nil {
		return fleetapi.MemoryPushResponse{}, err
	}
	return c.Push(ctx, r)
}

func (b *boundClient) Forget(ctx context.Context, r fleetapi.MemoryForgetRequest) (fleetapi.MemoryForgetResponse, error) {
	c, err := b.bind(ctx)
	if err != nil {
		return fleetapi.MemoryForgetResponse{}, err
	}
	return c.Forget(ctx, r)
}

func (b *boundClient) ForgetAll(ctx context.Context, r fleetapi.MemoryForgetAllRequest) (fleetapi.MemoryForgetAllResponse, error) {
	c, err := b.bind(ctx)
	if err != nil {
		return fleetapi.MemoryForgetAllResponse{}, err
	}
	return c.ForgetAll(ctx, r)
}

func (b *boundClient) Export(ctx context.Context, fn func(fleetapi.MemoryRecord) error) (fleetapi.MemoryExportTrailer, error) {
	c, err := b.bind(ctx)
	if err != nil {
		return fleetapi.MemoryExportTrailer{}, err
	}
	return c.Export(ctx, fn)
}

// StaffClient returns a client with a staff identity (always refused).
func (f *MemoryFleet) StaffClient() fleetapi.Memory { return &memClient{f: f, staff: true} }

type memClient struct {
	f         *MemoryFleet
	org, user string
	staff     bool
}

// gate applies the route gates. dataRights routes (forget, forget-all,
// export) need the permission but not the capability.
func (c *memClient) gate(call string, dataRights bool) error {
	if c.staff {
		return apiErr(403, fleetapi.CodeStaffNotPermitted, "learned memory is private to its owner; staff have no access")
	}
	if c.user == "" || c.org == "" {
		return apiErr(403, fleetapi.CodeUserNotProvisioned, "customer context required")
	}
	if !dataRights && c.f.Entitled != nil && !c.f.Entitled(c.org) {
		return apiErr(403, fleetapi.CodeCapabilityNotInTier, "memory sync is not in your plan")
	}
	if c.f.Fault != nil {
		if err := c.f.Fault(call, false); err != nil {
			return err
		}
	}
	return nil
}

func (c *memClient) after(call string) error {
	c.f.calls[call]++
	if c.f.Fault != nil {
		return c.f.Fault(call, true)
	}
	return nil
}

// ---- cursors ----

type cursor struct {
	seq, floor, erased int64
	given, snapshot    bool
}

func parseCursor(s string) (cursor, bool) {
	if s == "" {
		return cursor{}, true
	}
	if !strings.HasPrefix(s, "s") {
		n, err := strconv.ParseInt(s, 10, 64)
		return cursor{seq: n, given: true}, err == nil && n >= 0
	}
	parts := strings.Split(s[1:], ".")
	if len(parts) != 3 {
		return cursor{}, false
	}
	var v [3]int64
	for i, p := range parts {
		n, err := strconv.ParseInt(p, 10, 64)
		if err != nil || n < 0 {
			return cursor{}, false
		}
		v[i] = n
	}
	return cursor{seq: v[0], snapshot: true, floor: v[1], erased: v[2]}, true
}

func (u *memUser) stale(c cursor) (string, error) {
	switch {
	case c.given:
		if c.seq < u.floor {
			if c.seq < u.erasedAtSeq {
				return "erased", nil
			}
			return "cursor_expired", nil
		}
	case c.snapshot:
		if c.erased > u.erasedAtSeq || c.floor > u.floor {
			return "", apiErr(400, fleetapi.CodeInvalidCursor, "cursor epoch is newer than the server's")
		}
		if c.erased != u.erasedAtSeq {
			return "erased", nil
		}
		if u.floor > c.floor {
			return "cursor_expired", nil
		}
	}
	return "", nil
}

// ---- validation ----

var (
	memIDRe   = regexp.MustCompile(`^[A-Za-z0-9._-]{1,64}$`)
	memHLCRe  = regexp.MustCompile(`^(\d{16}):(\d{6}):([A-Za-z0-9._-]{1,64})$`)
	memHashRe = regexp.MustCompile(`^[0-9a-f]{64}$`)
	// A representative subset of Fleet's credential shapes.
	memSecretRes = []*regexp.Regexp{
		regexp.MustCompile(`\b(?:AKIA|ASIA)[0-9A-Z]{16}\b`),
		regexp.MustCompile(`-----BEGIN (?:[A-Z0-9]+ )*PRIVATE KEY-----`),
		regexp.MustCompile(`\beyJ[A-Za-z0-9_-]{5,}\.[A-Za-z0-9_-]{5,}\.[A-Za-z0-9_-]{5,}`),
		regexp.MustCompile(`(?i)\bbearer\s+[A-Za-z0-9._~+/-]{16,}`),
		regexp.MustCompile(`\bsk-(?:ant-)?[A-Za-z0-9_-]{20,}`),
		regexp.MustCompile(`\bgh[pousr]_[A-Za-z0-9]{30,}`),
		regexp.MustCompile(`\bxox[abprs]-[A-Za-z0-9-]{10,}`),
		regexp.MustCompile(`(?i)\b(?:password|secret|token)\s*[:=]\s*\S{6,}`),
		regexp.MustCompile(`(?i)\b[a-z][a-z0-9+.-]*://[^/\s:@]+:[^/\s@]+@`),
	}
)

func memHash(s string) string {
	h := sha256.Sum256([]byte(s))
	return hex.EncodeToString(h[:])
}

func hasSecret(s string) bool {
	for _, re := range memSecretRes {
		if s != "" && re.MatchString(s) {
			return true
		}
	}
	return false
}

func badText(s string) bool { return !utf8.ValidString(s) || strings.ContainsRune(s, 0) }

type reject struct {
	code, field string
	retry       bool
}

type lwwS struct{ v, hlc string }
type lwwB struct {
	v   bool
	hlc string
}

type prepared struct {
	it     fleetapi.MemoryPushItem
	hash   string
	title  *lwwS
	pinned *lwwB
	scope  *lwwS
}

func validKind(k string) bool {
	switch k {
	case "raw", "narrative_extractive", "narrative_synthesised", "narrative_extractive_fallback":
		return true
	}
	return false
}

func validateUpsert(it fleetapi.MemoryPushItem, now time.Time) (*prepared, *reject) {
	p := &prepared{it: it}
	if it.Content != nil {
		c := *it.Content
		switch {
		case len(c) == 0, badText(c):
			return nil, &reject{code: "invalid_field", field: "content"}
		case len(c) > fleetapi.MemoryMaxContentBytes:
			return nil, &reject{code: "too_large", field: "content"}
		}
	}
	if it.ContentHash != "" && !memHashRe.MatchString(it.ContentHash) {
		return nil, &reject{code: "hash_mismatch", field: "content_hash"}
	}
	if it.Content != nil {
		h := memHash(*it.Content)
		if it.ContentHash != "" && it.ContentHash != h {
			return nil, &reject{code: "hash_mismatch", field: "content_hash"}
		}
		p.hash = h
	} else {
		p.hash = it.ContentHash
	}
	if it.Kind != "" && !validKind(it.Kind) {
		return nil, &reject{code: "invalid_field", field: "kind"}
	}
	if it.RetrievalWeight != nil && !(*it.RetrievalWeight > 0 && *it.RetrievalWeight <= 10) {
		return nil, &reject{code: "invalid_field", field: "retrieval_weight"}
	}
	for name, v := range map[string]string{"turn_id": it.TurnID, "source": it.Source, "source_turn": it.SourceTurn, "tool_name": it.ToolName} {
		if len(v) > 128 || badText(v) {
			return nil, &reject{code: "invalid_field", field: name}
		}
	}
	for name, fs := range map[string][]string{"files_read": it.FilesRead, "files_modified": it.FilesModified} {
		if len(fs) > 64 {
			return nil, &reject{code: "invalid_field", field: name}
		}
		for _, x := range fs {
			if len(x) > 1024 || badText(x) {
				return nil, &reject{code: "invalid_field", field: name}
			}
		}
	}
	if it.RecallOwn != nil && (*it.RecallOwn < 0 || *it.RecallOwn > 1<<31) {
		return nil, &reject{code: "invalid_field", field: "recall_own"}
	}
	for _, ts := range []struct{ name, v string }{{"created_at", it.CreatedAt}, {"last_accessed", it.LastAccessed}} {
		if ts.v != "" {
			if _, err := time.Parse(time.RFC3339Nano, ts.v); err != nil {
				return nil, &reject{code: "invalid_field", field: ts.name}
			}
		}
	}
	names := make([]string, 0, len(it.Fields))
	for n := range it.Fields {
		names = append(names, n)
	}
	sort.Strings(names)
	for _, name := range names {
		fv := it.Fields[name]
		switch name {
		case "title", "pinned", "scope_kind":
		default:
			return nil, &reject{code: "invalid_field", field: "fields." + name}
		}
		m := memHLCRe.FindStringSubmatch(fv.HLC)
		if m == nil {
			return nil, &reject{code: "invalid_field", field: "fields." + name + ".hlc"}
		}
		ms, _ := strconv.ParseInt(m[1], 10, 64)
		if ms > now.Add(5*time.Minute).UnixMilli() {
			return nil, &reject{code: "clock_in_future", field: name}
		}
		switch name {
		case "title":
			var s string
			if json.Unmarshal(fv.V, &s) != nil || utf8.RuneCountInString(s) > 256 || badText(s) {
				return nil, &reject{code: "invalid_field", field: "title"}
			}
			p.title = &lwwS{s, fv.HLC}
		case "pinned":
			var b bool
			if json.Unmarshal(fv.V, &b) != nil {
				return nil, &reject{code: "invalid_field", field: "pinned"}
			}
			p.pinned = &lwwB{b, fv.HLC}
		case "scope_kind":
			var s string
			if json.Unmarshal(fv.V, &s) != nil || (s != "global" && s != "project" && s != "session" && s != "long_term") {
				return nil, &reject{code: "invalid_field", field: "scope_kind"}
			}
			p.scope = &lwwS{s, fv.HLC}
		}
	}
	title := ""
	if p.title != nil {
		title = p.title.v
	}
	content := ""
	if it.Content != nil {
		content = *it.Content
	}
	switch {
	case hasSecret(content):
		return nil, &reject{code: "secret_detected", field: "content"}
	case hasSecret(title):
		return nil, &reject{code: "secret_detected", field: "title"}
	case hasSecret(it.ToolName):
		return nil, &reject{code: "secret_detected", field: "tool_name"}
	case hasSecret(it.Source):
		return nil, &reject{code: "secret_detected", field: "source"}
	case hasSecret(it.SourceTurn):
		return nil, &reject{code: "secret_detected", field: "source_turn"}
	}
	for _, fs := range [][]string{it.FilesRead, it.FilesModified} {
		for _, x := range fs {
			if hasSecret(x) {
				return nil, &reject{code: "secret_detected", field: "files"}
			}
		}
	}
	return p, nil
}

func rejected(id string, r *reject) fleetapi.MemoryPushResult {
	b := r.retry
	return fleetapi.MemoryPushResult{ID: id, Status: fleetapi.MemoryRejected, Code: r.code, Field: r.field, Retry: &b}
}

func (f *MemoryFleet) quotas() (int, int64, int) {
	r, b, t := f.MaxRecords, f.MaxBytes, f.MaxTombstones
	if r <= 0 {
		r = 20000
	}
	if b <= 0 {
		b = 50 << 20
	}
	if t <= 0 {
		t = 100000
	}
	return r, b, t
}

// ---- push ----

func (c *memClient) Push(ctx context.Context, req fleetapi.MemoryPushRequest) (fleetapi.MemoryPushResponse, error) {
	if err := ctx.Err(); err != nil {
		return fleetapi.MemoryPushResponse{}, err
	}
	f := c.f
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := c.gate("push", false); err != nil {
		return fleetapi.MemoryPushResponse{}, err
	}
	if body, _ := json.Marshal(req); len(body) > fleetapi.MemoryMaxPushBytes {
		return fleetapi.MemoryPushResponse{}, apiErr(413, fleetapi.CodePayloadTooLarge, "request body exceeds the limit")
	}
	if len(req.Items) > fleetapi.MemoryMaxPushItems {
		return fleetapi.MemoryPushResponse{}, apiErr(400, fleetapi.CodeTooManyItems, "a push holds at most 100 items")
	}
	if !memIDRe.MatchString(req.DeviceID) {
		return fleetapi.MemoryPushResponse{}, apiErr(400, fleetapi.CodeInvalidRequestBody, "device_id must match [A-Za-z0-9._-]{1,64}")
	}
	base, ok := parseCursor(req.BaseCursor)
	if !ok {
		return fleetapi.MemoryPushResponse{}, apiErr(400, fleetapi.CodeInvalidCursor, "base_cursor must be empty or a non-negative integer")
	}
	u := f.user(c.org, c.user)
	resp := fleetapi.MemoryPushResponse{Results: []fleetapi.MemoryPushResult{}, CursorFloor: strconv.FormatInt(u.floor, 10)}
	blanket := ""
	if !u.enabled {
		blanket = "sync_disabled"
	} else if reason, err := u.stale(base); err != nil {
		return fleetapi.MemoryPushResponse{}, err
	} else if reason != "" {
		blanket = "resync_required"
	}
	pt := &pushTx{f: f, u: u, device: req.DeviceID, now: f.now()}
	for _, it := range req.Items {
		if blanket != "" && it.Op != "forget" {
			t := true
			resp.Results = append(resp.Results, fleetapi.MemoryPushResult{ID: it.ID, Status: fleetapi.MemoryRejected, Code: blanket, Retry: &t})
			continue
		}
		resp.Results = append(resp.Results, pt.item(it))
	}
	if err := c.after("push"); err != nil {
		return fleetapi.MemoryPushResponse{}, err
	}
	return resp, nil
}

type pushTx struct {
	f      *MemoryFleet
	u      *memUser
	device string
	now    time.Time
}

func (pt *pushTx) alloc() int64 {
	n := pt.u.nextSeq
	pt.u.nextSeq++
	return n
}

func seqStr(n int64) string { return strconv.FormatInt(n, 10) }

func (pt *pushTx) tombs() int {
	n := 0
	for _, r := range pt.u.records {
		if r.state == "tombstone" {
			n++
		}
	}
	return n
}

func (pt *pushTx) item(it fleetapi.MemoryPushItem) fleetapi.MemoryPushResult {
	if it.Op != "upsert" && it.Op != "forget" {
		return rejected(it.ID, &reject{code: "invalid_field", field: "op"})
	}
	if !memIDRe.MatchString(it.ID) {
		return rejected(it.ID, &reject{code: "invalid_field", field: "id"})
	}
	if it.Op == "forget" {
		seq := pt.forgetID(it.ID)
		res := fleetapi.MemoryPushResult{ID: it.ID, Status: fleetapi.MemoryForgotten}
		if seq > 0 {
			res.Seq = seqStr(seq)
		}
		return res
	}
	p, rj := validateUpsert(it, pt.now)
	if rj != nil {
		return rejected(it.ID, rj)
	}
	if p.scope != nil && p.scope.v == "project" {
		return rejected(it.ID, &reject{code: "scope_not_supported_yet", field: "scope_kind"})
	}
	target, isAlias := it.ID, false
	if c, ok := pt.u.aliases[it.ID]; ok {
		target, isAlias = c, true
	}
	r0 := pt.u.records[target]
	if r0 == nil && isAlias {
		delete(pt.u.aliases, it.ID)
		target, isAlias = it.ID, false
		r0 = pt.u.records[target]
	}
	switch {
	case r0 != nil && r0.state == "tombstone":
		return pt.onTombstone(r0, p, it.ID)
	case r0 != nil:
		return pt.onLive(r0, p, it.ID, isAlias)
	default:
		return pt.create(p)
	}
}

func (pt *pushTx) onTombstone(r *memRec, p *prepared, sent string) fleetapi.MemoryPushResult {
	switch r.reason {
	case "forgotten":
		return fleetapi.MemoryPushResult{ID: sent, Status: fleetapi.MemoryForgotten}
	case "superseded":
		return fleetapi.MemoryPushResult{ID: sent, Status: fleetapi.MemorySuperseded, SupersededBy: r.supersededBy}
	}
	if p.scope != nil && r.scopeHLC != "" && p.scope.hlc > r.scopeHLC && pt.u.scopeEnabled(p.scope.v) {
		if p.it.Content == nil {
			return rejected(sent, &reject{code: "invalid_field", field: "content"})
		}
		return pt.create(p)
	}
	return fleetapi.MemoryPushResult{ID: sent, Status: fleetapi.MemoryUnchanged}
}

func (pt *pushTx) onLive(r *memRec, p *prepared, sent string, isAlias bool) fleetapi.MemoryPushResult {
	changed, tomb, rj := pt.applyToLive(r, p)
	if rj != nil {
		return rejected(sent, rj)
	}
	switch {
	case isAlias:
		return fleetapi.MemoryPushResult{ID: sent, Status: fleetapi.MemoryMerged, CanonicalID: pt.idOf(r), Seq: seqStr(r.seq)}
	case changed || tomb:
		return fleetapi.MemoryPushResult{ID: sent, Status: fleetapi.MemoryAccepted, Seq: seqStr(r.seq)}
	}
	return fleetapi.MemoryPushResult{ID: sent, Status: fleetapi.MemoryUnchanged}
}

func (pt *pushTx) idOf(r *memRec) string {
	for id, x := range pt.u.records {
		if x == r {
			return id
		}
	}
	return ""
}

func (pt *pushTx) applyToLive(r *memRec, p *prepared) (changed, tomb bool, rj *reject) {
	_, maxBytes, _ := pt.f.quotas()
	if p.hash != "" && p.hash != r.hash {
		return false, false, &reject{code: "content_mismatch", field: "content"}
	}
	if p.title != nil && (r.titleHLC == "" || p.title.hlc > r.titleHLC) {
		if d := int64(len(p.title.v) - len(r.title)); d > 0 && pt.u.liveBytes+d > maxBytes {
			return false, false, &reject{code: "quota_exceeded", retry: true}
		}
	}
	newSize := r.size
	scopeChanged := false
	if p.title != nil && (r.titleHLC == "" || p.title.hlc > r.titleHLC) {
		newSize += len(p.title.v) - len(r.title)
		r.title, r.titleHLC, r.hasTitle = p.title.v, p.title.hlc, true
		changed = true
	}
	if p.pinned != nil && (r.pinnedHLC == "" || p.pinned.hlc > r.pinnedHLC) {
		r.pinned, r.pinnedHLC, r.hasPinned = p.pinned.v, p.pinned.hlc, true
		changed = true
	}
	if p.scope != nil && (r.scopeHLC == "" || p.scope.hlc > r.scopeHLC) {
		r.scope, r.scopeHLC = p.scope.v, p.scope.hlc
		changed, scopeChanged = true, true
	}
	if p.it.LastAccessed != "" && p.it.LastAccessed > r.lastAccessed {
		r.lastAccessed = p.it.LastAccessed
		changed = true
	}
	if p.it.RecallOwn != nil {
		if r.recall == nil {
			r.recall = map[string]int64{}
		}
		if r.recall[pt.device] < *p.it.RecallOwn {
			r.recall[pt.device] = *p.it.RecallOwn
			changed = true
		}
	}
	if scopeChanged && !pt.u.scopeEnabled(r.scope) {
		pt.tombstone(r, "left_sync_scope", "", true)
		return true, true, nil
	}
	if !changed {
		return false, false, nil
	}
	r.seq = pt.alloc()
	pt.u.liveBytes += int64(newSize - r.size)
	r.size = newSize
	return true, false, nil
}

func kindRank(k string) int {
	switch k {
	case "narrative_synthesised":
		return 2
	case "narrative_extractive_fallback":
		return 1
	}
	return 0
}

func (pt *pushTx) create(p *prepared) fleetapi.MemoryPushResult {
	maxRecords, maxBytes, maxTombs := pt.f.quotas()
	it := p.it
	id := it.ID
	if it.Content == nil {
		return rejected(id, &reject{code: "invalid_field", field: "content"})
	}
	if it.Kind == "" {
		return rejected(id, &reject{code: "invalid_field", field: "kind"})
	}
	if p.scope == nil {
		return rejected(id, &reject{code: "invalid_field", field: "fields.scope_kind"})
	}
	if !pt.u.scopeEnabled(p.scope.v) {
		return rejected(id, &reject{code: "scope_not_enabled", field: "scope_kind"})
	}
	// Exact-content dedupe: alias to the live record with the same
	// (content_hash, kind).
	var canonIDs []string
	for cid, r := range pt.u.records {
		if r.state == "live" && r.hash == p.hash && r.kind == it.Kind && cid != id {
			canonIDs = append(canonIDs, cid)
		}
	}
	if len(canonIDs) > 0 {
		sort.Strings(canonIDs)
		canon := canonIDs[0]
		pt.u.aliases[id] = canon
		c := pt.u.records[canon]
		changed, tomb, rj := pt.applyToLive(c, p)
		if rj != nil {
			return rejected(id, rj)
		}
		if !changed && !tomb {
			c.seq = pt.alloc()
		}
		return fleetapi.MemoryPushResult{ID: id, Status: fleetapi.MemoryMerged, CanonicalID: canon, Seq: seqStr(c.seq)}
	}
	var loser *memRec
	if rk := kindRank(it.Kind); rk > 0 && it.TurnID != "" {
		var best *memRec
		for cid, r := range pt.u.records {
			if r.state == "live" && r.turnID == it.TurnID && cid != id && kindRank(r.kind) > 0 && (best == nil || r.seq < best.seq) {
				best = r
			}
		}
		if best != nil {
			if rk > kindRank(best.kind) {
				loser = best
			} else {
				if pt.tombs() >= maxTombs {
					return rejected(id, &reject{code: "tombstone_quota"})
				}
				seq := pt.alloc()
				by := pt.idOf(best)
				pt.u.records[id] = &memRec{seq: seq, state: "tombstone", reason: "superseded", supersededBy: by}
				return fleetapi.MemoryPushResult{ID: id, Status: fleetapi.MemorySuperseded, SupersededBy: by, Seq: seqStr(seq)}
			}
		}
	}
	title := ""
	if p.title != nil {
		title = p.title.v
	}
	size := len(*it.Content) + len(title)
	for _, x := range append(append([]string{}, it.FilesRead...), it.FilesModified...) {
		size += len(x)
	}
	netR, netB := 1, int64(size)
	if loser != nil {
		netR--
		netB -= int64(loser.size)
	}
	if pt.u.liveRecords+netR > maxRecords || pt.u.liveBytes+netB > maxBytes {
		return rejected(id, &reject{code: "quota_exceeded", retry: true})
	}
	if loser != nil {
		pt.tombstone(loser, "superseded", id, false)
	}
	w := 1.0
	if it.RetrievalWeight != nil {
		w = *it.RetrievalWeight
	}
	created := it.CreatedAt
	if created == "" {
		created = pt.now.Format(time.RFC3339Nano)
	}
	r := &memRec{seq: pt.alloc(), state: "live", content: *it.Content, hash: p.hash, kind: it.Kind, turnID: it.TurnID,
		source: it.Source, sourceTurn: it.SourceTurn, toolName: it.ToolName, filesRead: it.FilesRead, filesModified: it.FilesModified,
		createdAt: created, weight: w, size: size, scope: p.scope.v, scopeHLC: p.scope.hlc, hasScope: true, lastAccessed: it.LastAccessed}
	if p.title != nil {
		r.title, r.titleHLC, r.hasTitle = p.title.v, p.title.hlc, true
	}
	if p.pinned != nil {
		r.pinned, r.pinnedHLC, r.hasPinned = p.pinned.v, p.pinned.hlc, true
	}
	if it.RecallOwn != nil && *it.RecallOwn > 0 {
		r.recall = map[string]int64{pt.device: *it.RecallOwn}
	}
	pt.u.records[id] = r
	pt.u.liveRecords++
	pt.u.liveBytes += int64(size)
	return fleetapi.MemoryPushResult{ID: id, Status: fleetapi.MemoryAccepted, Seq: seqStr(r.seq)}
}

func (pt *pushTx) tombstone(r *memRec, reason, by string, keepScope bool) {
	pt.u.liveRecords--
	pt.u.liveBytes -= int64(r.size)
	scope, scopeHLC := "", ""
	if keepScope {
		scope, scopeHLC = r.scope, r.scopeHLC
	}
	*r = memRec{seq: pt.alloc(), state: "tombstone", reason: reason, supersededBy: by, scope: scope, scopeHLC: scopeHLC}
}

// forgetID tombstones id (resolving aliases); an id Fleet never saw gets
// no row (the server's behaviour) and is reported forgotten.
func (pt *pushTx) forgetID(id string) int64 {
	target := id
	if c, ok := pt.u.aliases[id]; ok {
		target = c
	}
	r := pt.u.records[target]
	if r == nil {
		return 0
	}
	if r.state == "live" {
		pt.tombstone(r, "forgotten", "", false)
		return r.seq
	}
	if r.reason != "forgotten" {
		r.reason, r.supersededBy, r.scope, r.scopeHLC = "forgotten", "", "", ""
	}
	return 0
}

// ---- pull ----

func (pt *pushTx) wire(id string, r *memRec, device string) fleetapi.MemoryRecord {
	if r.state == "tombstone" {
		return fleetapi.MemoryRecord{ID: id, Seq: seqStr(r.seq), State: "tombstone", Reason: r.reason, SupersededBy: r.supersededBy}
	}
	var total, mine int64
	for d, n := range r.recall {
		total += n
		if d == device {
			mine += n
		}
	}
	aliases := []string{}
	for a, c := range pt.u.aliases {
		if c == id {
			aliases = append(aliases, a)
		}
	}
	sort.Strings(aliases)
	nz := func(s []string) []string {
		if s == nil {
			return []string{}
		}
		return s
	}
	return fleetapi.MemoryRecord{ID: id, Seq: seqStr(r.seq), State: "live", Content: r.content, ContentHash: r.hash, Kind: r.kind,
		RetrievalWeight: r.weight, TurnID: r.turnID, Source: r.source, SourceTurn: r.sourceTurn, ToolName: r.toolName,
		FilesRead: nz(r.filesRead), FilesModified: nz(r.filesModified), CreatedAt: r.createdAt, Title: r.title, TitleHLC: r.titleHLC,
		Pinned: r.pinned, PinnedHLC: r.pinnedHLC, ScopeKind: r.scope, ScopeHLC: r.scopeHLC, RecallCount: total,
		RecallCountOthers: total - mine, LastAccessed: r.lastAccessed, Aliases: aliases}
}

func (c *memClient) Pull(ctx context.Context, req fleetapi.MemoryPullRequest) (fleetapi.MemoryPullResponse, error) {
	if err := ctx.Err(); err != nil {
		return fleetapi.MemoryPullResponse{}, err
	}
	f := c.f
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := c.gate("pull", false); err != nil {
		return fleetapi.MemoryPullResponse{}, err
	}
	cu, ok := parseCursor(req.Cursor)
	if !ok {
		return fleetapi.MemoryPullResponse{}, apiErr(400, fleetapi.CodeInvalidCursor, "cursor must be empty or a non-negative integer")
	}
	if req.Limit < 0 {
		return fleetapi.MemoryPullResponse{}, apiErr(400, fleetapi.CodeInvalidRequestBody, "limit must be a positive integer")
	}
	limit := req.Limit
	if limit == 0 {
		limit = 200
	}
	if limit > fleetapi.MemoryMaxPullLimit {
		limit = fleetapi.MemoryMaxPullLimit
	}
	if req.DeviceID != "" && !memIDRe.MatchString(req.DeviceID) {
		return fleetapi.MemoryPullResponse{}, apiErr(400, fleetapi.CodeInvalidRequestBody, "device_id must match [A-Za-z0-9._-]{1,64}")
	}
	resp := fleetapi.MemoryPullResponse{Records: []fleetapi.MemoryRecord{}, Cursor: req.Cursor}
	u := f.users[c.org+"\x00"+c.user]
	if u == nil || !u.enabled {
		return resp, c.after("pull")
	}
	resp.Enabled = true
	reason, err := u.stale(cu)
	if err != nil {
		return fleetapi.MemoryPullResponse{}, err
	}
	if reason != "" {
		resp.Reset, resp.ResetReason, resp.Cursor = true, reason, ""
		if reason == "erased" {
			resp.ErasedBefore = u.erasedBefore
		}
		return resp, c.after("pull")
	}
	type row struct {
		id string
		r  *memRec
	}
	var rows []row
	for id, r := range u.records {
		if r.seq > cu.seq {
			rows = append(rows, row{id, r})
		}
	}
	sort.Slice(rows, func(i, j int) bool { return rows[i].r.seq < rows[j].r.seq })
	if len(rows) > limit {
		rows, resp.HasMore = rows[:limit], true
	}
	pt := &pushTx{f: f, u: u}
	var last int64
	for _, rw := range rows {
		resp.Records = append(resp.Records, pt.wire(rw.id, rw.r, req.DeviceID))
		last = rw.r.seq
	}
	if len(rows) > 0 {
		resp.Cursor = seqStr(last)
	}
	if !cu.given {
		switch {
		case resp.HasMore:
			resp.Cursor = fmt.Sprintf("s%d.%d.%d", last, u.floor, u.erasedAtSeq)
		default:
			final := last
			if u.floor > final {
				final = u.floor
			}
			if cu.seq > final {
				final = cu.seq
			}
			if final == 0 {
				resp.Cursor = req.Cursor
			} else {
				resp.Cursor = seqStr(final)
			}
		}
	}
	return resp, c.after("pull")
}

// ---- settings, forget, export ----

func (u *memUser) settings(f *MemoryFleet) fleetapi.MemorySettings {
	mr, mb, _ := f.quotas()
	return fleetapi.MemorySettings{Enabled: u.enabled, Scopes: append([]string(nil), u.scopes...), ConsentVersion: u.consent,
		OptedInAt: u.optedInAt, Usage: fleetapi.MemoryUsage{LiveRecords: u.liveRecords, LiveBytes: u.liveBytes, MaxRecords: mr, MaxBytes: mb}}
}

func (c *memClient) Settings(ctx context.Context) (fleetapi.MemorySettings, error) {
	f := c.f
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := c.gate("settings", false); err != nil {
		return fleetapi.MemorySettings{}, err
	}
	u := f.users[c.org+"\x00"+c.user]
	if u == nil {
		u = &memUser{scopes: []string{"long_term", "global"}}
	}
	return u.settings(f), c.after("settings")
}

func (c *memClient) PutSettings(ctx context.Context, upd fleetapi.MemorySettingsUpdate) (fleetapi.MemorySettings, error) {
	f := c.f
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := c.gate("put_settings", false); err != nil {
		return fleetapi.MemorySettings{}, err
	}
	if len(upd.ConsentVersion) > 64 || strings.ContainsRune(upd.ConsentVersion, 0) {
		return fleetapi.MemorySettings{}, apiErr(400, fleetapi.CodeInvalidRequestBody, "consent_version too long")
	}
	var scopes []string
	if upd.Scopes != nil {
		seen := map[string]bool{}
		for _, s := range *upd.Scopes {
			switch s {
			case "long_term", "global":
				if !seen[s] {
					seen[s] = true
					scopes = append(scopes, s)
				}
			case "project", "session":
				return fleetapi.MemorySettings{}, apiErr(422, fleetapi.CodeScopeNotSupported, "only long_term and global memory can sync in v1")
			default:
				return fleetapi.MemorySettings{}, apiErr(400, fleetapi.CodeInvalidRequestBody, "unknown scope")
			}
		}
		if len(scopes) == 0 {
			return fleetapi.MemorySettings{}, apiErr(400, fleetapi.CodeInvalidRequestBody, "scopes must not be empty")
		}
	}
	u := f.user(c.org, c.user)
	if upd.Enabled != nil {
		if *upd.Enabled && !u.enabled {
			if upd.ConsentVersion == "" {
				return fleetapi.MemorySettings{}, apiErr(422, fleetapi.CodeConsentVersionRequired, "enabling memory sync requires the consent_version the user accepted")
			}
			u.optedInAt, u.consent = f.now().Format(time.RFC3339Nano), upd.ConsentVersion
		} else if *upd.Enabled && upd.ConsentVersion != "" {
			u.consent = upd.ConsentVersion
		}
		u.enabled = *upd.Enabled
	}
	if scopes != nil {
		u.scopes = scopes
	}
	return u.settings(f), c.after("put_settings")
}

func (c *memClient) Forget(ctx context.Context, req fleetapi.MemoryForgetRequest) (fleetapi.MemoryForgetResponse, error) {
	f := c.f
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := c.gate("forget", true); err != nil {
		return fleetapi.MemoryForgetResponse{}, err
	}
	if len(req.IDs) == 0 || len(req.IDs) > fleetapi.MemoryMaxForgetIDs {
		return fleetapi.MemoryForgetResponse{}, apiErr(400, fleetapi.CodeInvalidRequestBody, "ids must hold 1..500 entries")
	}
	for _, id := range req.IDs {
		if !memIDRe.MatchString(id) {
			return fleetapi.MemoryForgetResponse{}, apiErr(400, fleetapi.CodeInvalidRequestBody, "ids must match [A-Za-z0-9._-]{1,64}")
		}
	}
	pt := &pushTx{f: f, u: f.user(c.org, c.user), now: f.now()}
	seen := map[string]bool{}
	n := 0
	for _, id := range req.IDs {
		if !seen[id] {
			seen[id] = true
			pt.forgetID(id)
			n++
		}
	}
	return fleetapi.MemoryForgetResponse{Forgotten: n}, c.after("forget")
}

func (c *memClient) ForgetAll(ctx context.Context, req fleetapi.MemoryForgetAllRequest) (fleetapi.MemoryForgetAllResponse, error) {
	f := c.f
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := c.gate("forget_all", true); err != nil {
		return fleetapi.MemoryForgetAllResponse{}, err
	}
	if req.Confirm != fleetapi.MemoryForgetAllConfirm {
		return fleetapi.MemoryForgetAllResponse{}, apiErr(422, fleetapi.CodeConfirmationRequired, `send {"confirm":"forget-all"} to erase all memory`)
	}
	u := f.user(c.org, c.user)
	n := u.liveRecords
	u.records, u.aliases = map[string]*memRec{}, map[string]string{}
	floor := u.nextSeq
	u.floor, u.erasedAtSeq, u.nextSeq = floor, floor, floor+1
	u.erasedBefore = fmt.Sprintf("%016d:%06d:fleet", f.now().UnixMilli(), 0)
	u.liveRecords, u.liveBytes = 0, 0
	return fleetapi.MemoryForgetAllResponse{Erased: n, ErasedBefore: u.erasedBefore}, c.after("forget_all")
}

func (c *memClient) Export(ctx context.Context, fn func(fleetapi.MemoryRecord) error) (fleetapi.MemoryExportTrailer, error) {
	f := c.f
	f.mu.Lock()
	if err := c.gate("export", true); err != nil {
		f.mu.Unlock()
		return fleetapi.MemoryExportTrailer{}, err
	}
	u := f.user(c.org, c.user)
	pt := &pushTx{f: f, u: u}
	var recs []fleetapi.MemoryRecord
	for id, r := range u.records {
		if r.state == "live" {
			w := pt.wire(id, r, "")
			w.RecallCountOthers = 0
			recs = append(recs, w)
		}
	}
	err := c.after("export")
	f.mu.Unlock()
	if err != nil {
		return fleetapi.MemoryExportTrailer{}, err
	}
	sort.Slice(recs, func(i, j int) bool { return recs[i].Seq < recs[j].Seq })
	for _, r := range recs {
		if err := fn(r); err != nil {
			return fleetapi.MemoryExportTrailer{}, err
		}
	}
	return fleetapi.MemoryExportTrailer{ExportedAt: f.now().Format(time.RFC3339Nano), Count: len(recs)}, nil
}

// SweepTombstones deletes the user's tombstones and raises the cursor
// floor to the highest swept seq, as Fleet's 180-day sweep does. Devices
// with an older cursor then get reset cursor_expired.
func (f *MemoryFleet) SweepTombstones(org, user string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	u := f.user(org, user)
	n := 0
	for id, r := range u.records {
		if r.state == "tombstone" {
			if r.seq > u.floor {
				u.floor = r.seq
			}
			delete(u.records, id)
			n++
		}
	}
	return n
}

// LiveContents returns the user's live record contents by id (tests).
func (f *MemoryFleet) LiveContents(org, user string) map[string]string {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := map[string]string{}
	if u := f.users[org+"\x00"+user]; u != nil {
		for id, r := range u.records {
			if r.state == "live" {
				out[id] = r.content
			}
		}
	}
	return out
}

// AllContents returns every live content held for anyone (tests that
// look for a canary anywhere on the server).
func (f *MemoryFleet) AllContents() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []string
	for _, u := range f.users {
		for _, r := range u.records {
			if r.state == "live" {
				out = append(out, r.content, r.title)
			}
		}
	}
	return out
}
