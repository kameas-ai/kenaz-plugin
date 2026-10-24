package fleetapi

import "context"

// Signal is an OTLP signal type, i.e. the /otlp/v1/<signal> path.
type Signal string

const (
	SignalLogs    Signal = "logs"
	SignalMetrics Signal = "metrics"
	SignalTraces  Signal = "traces"
)

// ContentTypeProtobuf is the only content type the projector emits.
const ContentTypeProtobuf = "application/x-protobuf"

// ProjectedBatch is a serialized OTLP Export*ServiceRequest that the public
// projector has already projected onto Fleet's closed schema,
// with the kameas.* identity resource attributes already stamped. The
// implementation posts Body unchanged.
type ProjectedBatch struct {
	Signal      Signal
	ContentType string
	Body        []byte
	// Records is the record or span count, for rate limiting and metrics
	// only. Fleet caps at 1000 per request.
	Records int
}

// ExportResult mirrors Fleet's ingest response counters.
type ExportResult struct {
	Accepted       int
	DroppedClasses int
	DroppedPII     int
	DroppedInvalid int
}

// OTLPPoster posts projected batches to Fleet's OTLP/HTTP ingest.
type OTLPPoster interface {
	// Post sends one batch. A retryable failure (network, 5xx, 429)
	// returns an error that is not an *APIError with a 4xx status.
	// An org-pause refusal is detectable with OrgPaused.
	//
	// ctx carries the account the batch was stamped for
	// (WithExpectedSession). The implementation MUST pick the credentials
	// for the request first and compare them with ExpectedSessionFrom(ctx);
	// when ctx has none, or it does not match, it MUST refuse, sending
	// nothing, with an *APIError whose Code is CodeIdentityMismatch.
	Post(ctx context.Context, batch ProjectedBatch) (ExportResult, error)
}
