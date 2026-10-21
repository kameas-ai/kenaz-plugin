// Package project projects aggregated harness sessions onto Kameas Fleet's
// closed telemetry schema: the four harness.* log kinds and synthesized
// gen_ai.chat spans. It is an allowlist,
// never a denylist: every value it writes is a hash, an
// enum member, a number, or a model id that matches Fleet's id pattern.
package project

import (
	"time"

	"github.com/kameas-ai/kenaz-plugin/internal/canon"
	"github.com/kameas-ai/kenaz-plugin/internal/fleetschema"
	"github.com/kameas-ai/kenaz-plugin/internal/session"
)

// TableVersion is the projection mapping-table version. Bump it on any
// change to the set of fields that can leave the device: consent folds it
// into the notice version so members re-confirm.
const TableVersion = 1

// FieldType is the value type of a Field.
type FieldType int

const (
	TString FieldType = iota + 1
	TInt
	TFloat
	TBool
	TStrings
)

// Field is one projected key/value.
type Field struct {
	Key   string
	Type  FieldType
	Str   string
	Int   int64
	Float float64
	Bool  bool
	Strs  []string
}

func str(k, v string) Field       { return Field{Key: k, Type: TString, Str: v} }
func i64(k string, v int64) Field { return Field{Key: k, Type: TInt, Int: v} }
func f64(k string, v float64) Field {
	return Field{Key: k, Type: TFloat, Float: v}
}
func boolean(k string, v bool) Field { return Field{Key: k, Type: TBool, Bool: v} }

// Record is one projected log record (EventKind set) or span (SpanName set).
type Record struct {
	Class  fleetschema.Class
	Client canon.ClientKind
	Time   time.Time

	EventKind string
	Body      []Field

	SpanName string
	Attrs    []Field
	Start    time.Time
}

// IsSpan reports whether r is a span.
func (r Record) IsSpan() bool { return r.SpanName != "" }

// Options control schema-revision-dependent output.
type Options struct {
	// SchemaRev is fleetapi.Capabilities.TelemetrySchema. cost_known and the
	// kameas.client.* resource attributes are emitted only at
	// fleetschema.SchemaRevCostKnown or later.
	SchemaRev int
}

func (o Options) v2() bool { return o.SchemaRev >= fleetschema.SchemaRevCostKnown }

// Project maps aggregator outputs to records.
func Project(outs []session.Out, opt Options) []Record {
	var recs []Record
	for _, o := range outs {
		switch o.Kind {
		case session.OutStarted:
			recs = append(recs, logRec(o, fleetschema.KindConversationStarted,
				str("conversation_id", o.Hash),
				str("model_provider", fleetschema.NormalizeProvider(o.Provider))))
		case session.OutEnded:
			cost := o.CostUSD
			if !o.CostKnown || cost < 0 {
				cost = 0
			}
			body := []Field{
				str("conversation_id", o.Hash),
				i64("duration_ms", max64(o.Duration.Milliseconds(), 1)),
				i64("token_in", max64(o.TokenIn, 0)),
				i64("token_out", max64(o.TokenOut, 0)),
				f64("cost_usd", cost),
			}
			if opt.v2() {
				body = append(body, boolean(fleetschema.CostKnownKey, o.CostKnown))
			}
			r := logRec(o, fleetschema.KindConversationEnded, body...)
			// The summary covers the whole session: Start lets the gate
			// tell whether any of it predates an admission moment.
			r.Start = o.Time.Add(-o.Duration)
			recs = append(recs, r)
		case session.OutToolCall:
			recs = append(recs, logRec(o, fleetschema.KindToolInvoked,
				str("tool_name", ToolName(o.Client, o.ToolName, o.ToolIsMCP)),
				i64("latency_ms", max64(o.Latency.Milliseconds(), 0)),
				boolean("success", o.Success)))
			if !o.Success {
				recs = append(recs, logRec(o, fleetschema.KindError,
					str("category", CatToolFailure), boolean("recoverable", true)))
			}
		case session.OutError:
			cat := ErrorCategory(o.StatusCode, o.Timeout)
			recs = append(recs, logRec(o, fleetschema.KindError,
				str("category", cat), boolean("recoverable", Recoverable(cat))))
		case session.OutModelCall:
			recs = append(recs, chatSpan(o))
		}
	}
	return recs
}

func logRec(o session.Out, kind string, body ...Field) Record {
	return Record{Class: fleetschema.KindClass[kind], Client: o.Client, Time: o.Time, EventKind: kind, Body: body}
}

func chatSpan(o session.Out) Record {
	attrs := []Field{
		str("gen_ai.operation.name", "chat"),
		str("gen_ai.provider.name", fleetschema.NormalizeProvider(o.Provider)),
	}
	if o.Model != "" && fleetschema.IDRe.MatchString(o.Model) {
		attrs = append(attrs, str("gen_ai.request.model", o.Model))
	}
	attrs = append(attrs,
		i64("gen_ai.usage.input_tokens", clamp(o.InputTokens, 1_000_000_000)),
		i64("gen_ai.usage.output_tokens", clamp(o.OutputTokens, 1_000_000_000)),
		i64("gen_ai.usage.cache_read.input_tokens", clamp(o.CacheRead, 1_000_000_000)),
		i64("gen_ai.usage.cache_creation.input_tokens", clamp(o.CacheCreation, 1_000_000_000)),
	)
	if o.Reasoning > 0 {
		attrs = append(attrs, i64("kenaz.llm.reasoning_tokens", clamp(o.Reasoning, 1_000_000_000)))
	}
	if o.CallCostKnown && o.CallCostUSD >= 0 && o.CallCostUSD <= 100_000 {
		attrs = append(attrs, f64("kenaz.llm.cost_usd", o.CallCostUSD))
	}
	if o.HasTTFT {
		attrs = append(attrs, i64("kenaz.llm.ttft_ms", clamp(o.TTFT.Milliseconds(), 86_400_000)))
	}
	attrs = append(attrs, str("kenaz.session.hash", o.Hash))
	start := o.Time.Add(-o.CallDuration)
	return Record{Class: fleetschema.ClassModelCallTelemetry, Client: o.Client, Time: o.Time,
		SpanName: fleetschema.SpanGenAIChat, Attrs: attrs, Start: start}
}

func clamp(v, hi int64) int64 {
	if v < 0 {
		return 0
	}
	if v > hi {
		return hi
	}
	return v
}

func max64(a, b int64) int64 {
	if a > b {
		return a
	}
	return b
}
