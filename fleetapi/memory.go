package fleetapi

import (
	"context"
	"encoding/json"
	"errors"
	"net/url"
	"strconv"
	"time"
)

// Memory sync seam (Fleet contract docs/contract-harness-memory.md).
//
// Memory sync is opt-in, off by default and strictly private to the
// signed-in user: Fleet takes org and user from the token, never from a
// body, and no admin, staff, team or org route reads it. The types below
// are the wire bodies, field for field (JSON names and omitempty), and are
// built entirely in public code: an implementation marshals them as they
// are, sends no Content-Type requirement of its own, and adds only
// transport concerns (bearer token, user agent, request id). The closed
// key sets are exported by fleetapi/allowlist.
//
// This seam is additive: Provider does not require it. An implementation
// opts in by also implementing MemoryProvider; MemoryOf returns an
// "unavailable" client for one that does not, so older adapters keep
// compiling and simply cannot sync.

// CapMemorySync is the tier capability that enables memory sync (Pro,
// Team, Enterprise). Without it push/pull/settings answer 403
// capability_not_in_tier; forget, forget-all and export still work so a
// user whose plan lapsed can erase or take out their data.
const CapMemorySync = "memory_sync"

// Memory routes. Bodies decode as JSON whatever the Content-Type.
const (
	MemoryRoutePush        = "POST /api/v1/memory/push"
	MemoryRoutePull        = "GET /api/v1/memory/pull"
	MemoryRouteForget      = "POST /api/v1/memory/forget"
	MemoryRouteForgetAll   = "POST /api/v1/memory/forget-all"
	MemoryRouteGetSettings = "GET /api/v1/memory/settings"
	MemoryRoutePutSettings = "PUT /api/v1/memory/settings"
	MemoryRouteExport      = "GET /api/v1/memory/export"
)

// Memory error codes (APIError.Code). Content problems are never errors:
// they are per-item push results in a 200.
const (
	CodeRateLimited            = "rate_limited"           // 429, Details["retry_after_seconds"]
	CodeCapabilityNotInTier    = "capability_not_in_tier" // 403
	CodeStaffNotPermitted      = "staff_not_permitted"    // 403
	CodeUserNotProvisioned     = "user_not_provisioned"   // 403
	CodePermissionMemorySync   = "permission_denied:memory:sync"
	CodeConsentVersionRequired = "consent_version_required" // 422
	CodeConfirmationRequired   = "confirmation_required"    // 422
	CodeScopeNotSupported      = "scope_not_supported"      // 422
	CodeInvalidCursor          = "invalid_cursor"           // 400
	CodeTooManyItems           = "too_many_items"           // 400
	CodeInvalidRequestBody     = "invalid_request_body"     // 400
	CodePayloadTooLarge        = "payload_too_large"        // 413
	// CodeTombstoneQuota is a per-item push reject (inside the 200
	// report) when the user's tombstone quota is full. It is not
	// retryable for that record. Forget is never capped.
	CodeTombstoneQuota = "tombstone_quota"
	// CodeIdentityMismatch is produced by the client, never by Fleet: the
	// request's ExpectedSession is missing or does not match the
	// credentials it would be sent with (see Memory and OTLPPoster). An
	// *APIError carrying it has Status 0, meaning no request was sent.
	CodeIdentityMismatch = "identity_mismatch"
)

// Memory limits Fleet enforces.
const (
	MemoryMaxPushItems    = 100
	MemoryMaxPushBytes    = 2 << 20
	MemoryMaxPullLimit    = 500
	MemoryMaxForgetIDs    = 500
	MemoryMaxContentBytes = 32 << 10
)

// ErrMemoryUnavailable is returned by MemoryOf's fallback for a Provider
// without a memory client.
var ErrMemoryUnavailable = errors.New("memory sync is not available in this build")

// MemoryProvider is implemented by a Provider that can sync memory.
type MemoryProvider interface {
	Memory() Memory
}

// MemoryOf returns p's memory client, or one whose every call fails with
// ErrMemoryUnavailable.
func MemoryOf(p Provider) Memory {
	if mp, ok := p.(MemoryProvider); ok {
		if m := mp.Memory(); m != nil {
			return m
		}
	}
	return unavailableMemory{}
}

// Memory is the user's private memory store on Fleet, for the active
// session. Every method returns ErrNotSignedIn without a session, and an
// *APIError for a non-2xx answer (429 carries Details["retry_after_seconds"]
// from the Retry-After header). Calls must use the active session's token
// only.
//
// Account binding: every call's ctx carries the account the caller queued
// the work for (WithExpectedSession). An implementation MUST pick the
// credentials for the request first and then compare their env, org and
// subject with ExpectedSessionFrom(ctx); when ctx has no ExpectedSession,
// or it does not match those credentials, the implementation MUST refuse
// the call, sending nothing, with an *APIError whose Code is
// CodeIdentityMismatch ("identity_mismatch"). The caller's own identity
// check before the call is not enough on its own: the active account can
// change between that check and the send.
type Memory interface {
	// Settings is GET /api/v1/memory/settings.
	Settings(ctx context.Context) (MemorySettings, error)
	// PutSettings is PUT /api/v1/memory/settings with u as the body.
	PutSettings(ctx context.Context, u MemorySettingsUpdate) (MemorySettings, error)
	// Pull is GET /api/v1/memory/pull?{req.Query()}.
	Pull(ctx context.Context, req MemoryPullRequest) (MemoryPullResponse, error)
	// Push is POST /api/v1/memory/push with req as the body. Per-item
	// outcomes are in the 200 body; never treat the 200 itself as success.
	Push(ctx context.Context, req MemoryPushRequest) (MemoryPushResponse, error)
	// Forget is POST /api/v1/memory/forget with req as the body.
	Forget(ctx context.Context, req MemoryForgetRequest) (MemoryForgetResponse, error)
	// ForgetAll is POST /api/v1/memory/forget-all with req as the body.
	ForgetAll(ctx context.Context, req MemoryForgetAllRequest) (MemoryForgetAllResponse, error)
	// Export is GET /api/v1/memory/export (NDJSON): fn receives each live
	// record, then the trailer is returned. A stream without its trailer
	// is truncated: return an error.
	Export(ctx context.Context, fn func(MemoryRecord) error) (MemoryExportTrailer, error)
}

// MemoryFieldValue is one last-writer-wins register write.
type MemoryFieldValue struct {
	V   json.RawMessage `json:"v"`
	HLC string          `json:"hlc"`
}

// MemoryPushItem is one push item: op "upsert" or "forget".
type MemoryPushItem struct {
	Op              string                      `json:"op"`
	ID              string                      `json:"id"`
	Content         *string                     `json:"content,omitempty"`
	ContentHash     string                      `json:"content_hash,omitempty"`
	Kind            string                      `json:"kind,omitempty"`
	RetrievalWeight *float64                    `json:"retrieval_weight,omitempty"`
	TurnID          string                      `json:"turn_id,omitempty"`
	Source          string                      `json:"source,omitempty"`
	SourceTurn      string                      `json:"source_turn,omitempty"`
	ToolName        string                      `json:"tool_name,omitempty"`
	FilesRead       []string                    `json:"files_read,omitempty"`
	FilesModified   []string                    `json:"files_modified,omitempty"`
	CreatedAt       string                      `json:"created_at,omitempty"`
	Fields          map[string]MemoryFieldValue `json:"fields,omitempty"`
	RecallOwn       *int64                      `json:"recall_own,omitempty"`
	LastAccessed    string                      `json:"last_accessed,omitempty"`
	HLC             string                      `json:"hlc,omitempty"`
}

// MemoryPushRequest is the push body.
type MemoryPushRequest struct {
	DeviceID   string           `json:"device_id"`
	BaseCursor string           `json:"base_cursor"`
	Items      []MemoryPushItem `json:"items"`
}

// Push result statuses.
const (
	MemoryAccepted   = "accepted"
	MemoryUnchanged  = "unchanged"
	MemoryMerged     = "merged"
	MemorySuperseded = "superseded"
	MemoryForgotten  = "forgotten"
	MemoryRejected   = "rejected"
)

// MemoryPushResult is one item's outcome.
type MemoryPushResult struct {
	ID           string `json:"id"`
	Status       string `json:"status"`
	Seq          string `json:"seq,omitempty"`
	CanonicalID  string `json:"canonical_id,omitempty"`
	SupersededBy string `json:"superseded_by,omitempty"`
	Code         string `json:"code,omitempty"`
	Field        string `json:"field,omitempty"`
	Retry        *bool  `json:"retry,omitempty"`
}

// MemoryPushResponse is the push 200 body.
type MemoryPushResponse struct {
	Results     []MemoryPushResult `json:"results"`
	CursorFloor string             `json:"cursor_floor"`
}

// MemoryPullRequest is the pull query.
type MemoryPullRequest struct {
	Cursor   string
	Limit    int
	DeviceID string
}

// Query is the exact query string to send.
func (r MemoryPullRequest) Query() url.Values {
	q := url.Values{}
	q.Set("cursor", r.Cursor)
	if r.Limit > 0 {
		q.Set("limit", strconv.Itoa(r.Limit))
	}
	if r.DeviceID != "" {
		q.Set("device_id", r.DeviceID)
	}
	return q
}

// MemoryRecord is a pulled or exported record: State "live" carries the
// content and registers, State "tombstone" only Reason/SupersededBy.
type MemoryRecord struct {
	ID                string   `json:"id"`
	Seq               string   `json:"seq"`
	State             string   `json:"state"`
	Content           string   `json:"content"`
	ContentHash       string   `json:"content_hash"`
	Kind              string   `json:"kind"`
	RetrievalWeight   float64  `json:"retrieval_weight"`
	TurnID            string   `json:"turn_id"`
	Source            string   `json:"source"`
	SourceTurn        string   `json:"source_turn"`
	ToolName          string   `json:"tool_name"`
	FilesRead         []string `json:"files_read"`
	FilesModified     []string `json:"files_modified"`
	CreatedAt         string   `json:"created_at"`
	Title             string   `json:"title"`
	TitleHLC          string   `json:"title_hlc"`
	Pinned            bool     `json:"pinned"`
	PinnedHLC         string   `json:"pinned_hlc"`
	ScopeKind         string   `json:"scope_kind"`
	ScopeHLC          string   `json:"scope_hlc"`
	RecallCount       int64    `json:"recall_count"`
	RecallCountOthers int64    `json:"recall_count_others"`
	LastAccessed      string   `json:"last_accessed,omitempty"`
	Aliases           []string `json:"aliases"`
	Reason            string   `json:"reason,omitempty"`
	SupersededBy      string   `json:"superseded_by,omitempty"`
}

// MarshalJSON writes a tombstone in Fleet's content-free shape.
func (r MemoryRecord) MarshalJSON() ([]byte, error) {
	type plain MemoryRecord
	if r.State == "tombstone" {
		return json.Marshal(struct {
			ID           string `json:"id"`
			Seq          string `json:"seq"`
			State        string `json:"state"`
			Reason       string `json:"reason"`
			SupersededBy string `json:"superseded_by,omitempty"`
		}{r.ID, r.Seq, r.State, r.Reason, r.SupersededBy})
	}
	return json.Marshal(plain(r))
}

// Reset reasons.
const (
	MemoryResetCursorExpired = "cursor_expired"
	MemoryResetErased        = "erased"
)

// MemoryPullResponse is the pull 200 body.
type MemoryPullResponse struct {
	Enabled      bool           `json:"enabled"`
	Reset        bool           `json:"reset"`
	ResetReason  string         `json:"reset_reason"`
	ErasedBefore string         `json:"erased_before"`
	Records      []MemoryRecord `json:"records"`
	Cursor       string         `json:"cursor"`
	HasMore      bool           `json:"has_more"`
}

// MemoryUsage is the user's quota usage.
type MemoryUsage struct {
	LiveRecords int   `json:"live_records"`
	LiveBytes   int64 `json:"live_bytes"`
	MaxRecords  int   `json:"max_records"`
	MaxBytes    int64 `json:"max_bytes"`
}

// MemorySettings is the settings body.
type MemorySettings struct {
	Enabled        bool        `json:"enabled"`
	Scopes         []string    `json:"scopes"`
	ConsentVersion string      `json:"consent_version"`
	OptedInAt      string      `json:"opted_in_at"`
	Usage          MemoryUsage `json:"usage"`
}

// MemorySettingsUpdate is the PUT body. Nil fields are left unchanged;
// enabling requires ConsentVersion (the notice edition the user accepted).
type MemorySettingsUpdate struct {
	Enabled        *bool     `json:"enabled,omitempty"`
	Scopes         *[]string `json:"scopes,omitempty"`
	ConsentVersion string    `json:"consent_version,omitempty"`
}

// MemoryForgetRequest is the forget body (1..500 ids).
type MemoryForgetRequest struct {
	IDs []string `json:"ids"`
}

// MemoryForgetResponse is the forget 200 body.
type MemoryForgetResponse struct {
	Forgotten int `json:"forgotten"`
}

// MemoryForgetAllRequest is the forget-all body.
type MemoryForgetAllRequest struct {
	Confirm string `json:"confirm"`
}

// MemoryForgetAllConfirm is the only accepted Confirm value.
const MemoryForgetAllConfirm = "forget-all"

// MemoryForgetAllResponse is the forget-all 200 body.
type MemoryForgetAllResponse struct {
	Erased       int    `json:"erased"`
	ErasedBefore string `json:"erased_before"`
}

// MemoryExportTrailer is the export's last NDJSON line.
type MemoryExportTrailer struct {
	ExportedAt string `json:"exported_at"`
	Count      int    `json:"count"`
}

// RetryAfter reports a 429's back-off hint.
func RetryAfter(err error) (time.Duration, bool) {
	var ae *APIError
	if !errors.As(err, &ae) || ae.Status != 429 {
		return 0, false
	}
	if n, e := strconv.Atoi(ae.Details["retry_after_seconds"]); e == nil && n > 0 {
		return time.Duration(n) * time.Second, true
	}
	return 0, true
}

// APICode returns err's Fleet error code, or "".
func APICode(err error) string {
	var ae *APIError
	if errors.As(err, &ae) {
		return ae.Code
	}
	return ""
}

type unavailableMemory struct{}

func (unavailableMemory) Settings(context.Context) (MemorySettings, error) {
	return MemorySettings{}, ErrMemoryUnavailable
}
func (unavailableMemory) PutSettings(context.Context, MemorySettingsUpdate) (MemorySettings, error) {
	return MemorySettings{}, ErrMemoryUnavailable
}
func (unavailableMemory) Pull(context.Context, MemoryPullRequest) (MemoryPullResponse, error) {
	return MemoryPullResponse{}, ErrMemoryUnavailable
}
func (unavailableMemory) Push(context.Context, MemoryPushRequest) (MemoryPushResponse, error) {
	return MemoryPushResponse{}, ErrMemoryUnavailable
}
func (unavailableMemory) Forget(context.Context, MemoryForgetRequest) (MemoryForgetResponse, error) {
	return MemoryForgetResponse{}, ErrMemoryUnavailable
}
func (unavailableMemory) ForgetAll(context.Context, MemoryForgetAllRequest) (MemoryForgetAllResponse, error) {
	return MemoryForgetAllResponse{}, ErrMemoryUnavailable
}
func (unavailableMemory) Export(context.Context, func(MemoryRecord) error) (MemoryExportTrailer, error) {
	return MemoryExportTrailer{}, ErrMemoryUnavailable
}
