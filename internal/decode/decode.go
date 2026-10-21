// Package decode turns OTLP export requests from Claude Code and Codex into
// canonical events. Source identification is
// per record by event-name prefix, falling back to the resource's
// service.name. Records from anything else are dropped and counted.
//
// Requests are decoded as LogsData / TracesData / MetricsData, which the
// OTLP specification defines as wire- and JSON-compatible with the
// Export*ServiceRequest messages. That keeps gRPC out of the dependency
// tree.
package decode

import (
	"regexp"
	"strings"
	"time"

	commonv1 "go.opentelemetry.io/proto/otlp/common/v1"
	logsv1 "go.opentelemetry.io/proto/otlp/logs/v1"
	metricsv1 "go.opentelemetry.io/proto/otlp/metrics/v1"
	tracev1 "go.opentelemetry.io/proto/otlp/trace/v1"

	"github.com/kameas-ai/kenaz-plugin/internal/canon"
	"github.com/kameas-ai/kenaz-plugin/internal/decode/attr"
	"github.com/kameas-ai/kenaz-plugin/internal/decode/claude"
	"github.com/kameas-ai/kenaz-plugin/internal/decode/codex"
)

// Result is the outcome of decoding one export request.
type Result struct {
	Events []canon.Event
	// ClientVersions maps client kind to the harness version seen, when it
	// matches versionRe. Used for kameas.client.version only.
	ClientVersions map[canon.ClientKind]string
	// UnknownSource counts records from an unidentified producer.
	UnknownSource int
	// ContentSeen counts records carrying a content-bearing attribute
	// (harness content logging is on). Such content is never read.
	ContentSeen int
}

// versionRe accepts only numeric release versions with an optional
// well-known pre-release tag (e.g. "2.1.217", "0.147.0-alpha.3"). The
// version comes from harness resource attributes that users can set via
// OTEL_RESOURCE_ATTRIBUTES, so free text (hostnames, repo names) must not
// pass: anything else is dropped and kameas.client.version is omitted.
var versionRe = regexp.MustCompile(`^[0-9]{1,4}(\.[0-9]{1,6}){1,3}(-(alpha|beta|rc|dev|nightly|canary|preview|next)(\.?[0-9]{1,6})?)?$`)

// Decoder decodes export requests. Now supplies a timestamp when a record
// has none.
type Decoder struct {
	Now func() time.Time
}

func (d Decoder) now() time.Time {
	if d.Now != nil {
		return d.Now()
	}
	return time.Now()
}

func (d Decoder) ts(nanos ...uint64) time.Time {
	for _, n := range nanos {
		if n != 0 {
			return time.Unix(0, int64(n))
		}
	}
	return d.now()
}

// identify returns the producing harness, or "" when unknown.
func identify(eventName string, res attr.Set) canon.ClientKind {
	switch {
	case strings.HasPrefix(eventName, claude.EventPrefix):
		return canon.ClaudeCode
	case strings.HasPrefix(eventName, codex.EventPrefix):
		return canon.Codex
	}
	svc := res.String("service.name")
	switch {
	case claude.ServiceNames[svc]:
		return canon.ClaudeCode
	case codex.IsServiceName(svc):
		return canon.Codex
	}
	return ""
}

func (r *Result) noteVersion(kind canon.ClientKind, a attr.Set) {
	v := a.String("app.version")
	if v == "" {
		v = a.String("service.version")
	}
	if v == "" || !versionRe.MatchString(v) {
		return
	}
	if r.ClientVersions == nil {
		r.ClientVersions = map[canon.ClientKind]string{}
	}
	r.ClientVersions[kind] = v
}

// logEventName extracts the event name from a log record: the OTLP
// event_name field, the event.name attribute, or a string body that looks
// like a dotted event name.
func logEventName(lrEventName string, a attr.Set, body *commonv1.AnyValue) string {
	if lrEventName != "" {
		return lrEventName
	}
	if n := a.String("event.name"); n != "" {
		return n
	}
	if sv, ok := body.GetValue().(*commonv1.AnyValue_StringValue); ok {
		s := sv.StringValue
		if (strings.HasPrefix(s, claude.EventPrefix) || strings.HasPrefix(s, codex.EventPrefix)) && len(s) < 96 && !strings.ContainsAny(s, " \n\t") {
			return s
		}
	}
	return ""
}

// Logs decodes an OTLP logs export.
func (d Decoder) Logs(req *logsv1.LogsData) Result {
	var r Result
	for _, rl := range req.GetResourceLogs() {
		resAttrs := rl.GetResource().GetAttributes()
		for _, sl := range rl.GetScopeLogs() {
			for _, lr := range sl.GetLogRecords() {
				a := attr.Of(lr.GetAttributes(), resAttrs)
				res := attr.Of(resAttrs)
				name := logEventName(lr.GetEventName(), a, lr.GetBody())
				kind := identify(name, res)
				if kind == "" {
					r.UnknownSource++
					continue
				}
				if a.ContentSeen() {
					r.ContentSeen++
				}
				r.noteVersion(kind, a)
				t := d.ts(lr.GetTimeUnixNano(), lr.GetObservedTimeUnixNano())
				switch kind {
				case canon.ClaudeCode:
					r.Events = append(r.Events, claude.LogRecord(a, name, t)...)
				case canon.Codex:
					r.Events = append(r.Events, codex.LogRecord(a, name, t)...)
				}
			}
		}
	}
	return r
}

// Traces decodes an OTLP traces export. Harness spans are never
// forwarded; they only feed session activity and, for Codex turn
// spans, fallback usage.
func (d Decoder) Traces(req *tracev1.TracesData) Result {
	var r Result
	for _, rs := range req.GetResourceSpans() {
		resAttrs := rs.GetResource().GetAttributes()
		for _, ss := range rs.GetScopeSpans() {
			for _, sp := range ss.GetSpans() {
				a := attr.Of(sp.GetAttributes(), resAttrs)
				name := sp.GetName()
				kind := identify(name, attr.Of(resAttrs))
				if kind == "" {
					r.UnknownSource++
					continue
				}
				if a.ContentSeen() {
					r.ContentSeen++
				}
				r.noteVersion(kind, a)
				start := d.ts(sp.GetStartTimeUnixNano())
				end := d.ts(sp.GetEndTimeUnixNano(), sp.GetStartTimeUnixNano())
				switch kind {
				case canon.ClaudeCode:
					r.Events = append(r.Events, claude.Span(a, name, end)...)
				case canon.Codex:
					r.Events = append(r.Events, codex.Span(a, name, start, end)...)
				}
			}
		}
	}
	return r
}

// Metrics decodes an OTLP metrics export. Metrics are never
// forwarded as metrics; only Claude Code's session-start counter is read.
func (d Decoder) Metrics(req *metricsv1.MetricsData) Result {
	var r Result
	for _, rm := range req.GetResourceMetrics() {
		resAttrs := rm.GetResource().GetAttributes()
		for _, sm := range rm.GetScopeMetrics() {
			for _, m := range sm.GetMetrics() {
				kind := identify(m.GetName(), attr.Of(resAttrs))
				if kind == "" {
					r.UnknownSource++
					continue
				}
				if kind != canon.ClaudeCode {
					continue
				}
				for _, dp := range numberPoints(m) {
					a := attr.Of(dp.GetAttributes(), resAttrs)
					r.noteVersion(kind, a)
					t := d.ts(dp.GetTimeUnixNano())
					r.Events = append(r.Events, claude.MetricPoint(a, m.GetName(), t)...)
				}
			}
		}
	}
	return r
}

func numberPoints(m *metricsv1.Metric) []*metricsv1.NumberDataPoint {
	switch x := m.GetData().(type) {
	case *metricsv1.Metric_Sum:
		return x.Sum.GetDataPoints()
	case *metricsv1.Metric_Gauge:
		return x.Gauge.GetDataPoints()
	}
	return nil
}
