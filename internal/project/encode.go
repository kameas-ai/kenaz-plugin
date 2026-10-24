package project

import (
	"crypto/rand"
	"errors"
	"sort"

	commonv1 "go.opentelemetry.io/proto/otlp/common/v1"
	logsv1 "go.opentelemetry.io/proto/otlp/logs/v1"
	resourcev1 "go.opentelemetry.io/proto/otlp/resource/v1"
	tracev1 "go.opentelemetry.io/proto/otlp/trace/v1"
	"google.golang.org/protobuf/proto"

	"github.com/kameas-ai/kenaz-plugin/fleetapi"
	"github.com/kameas-ai/kenaz-plugin/internal/canon"
	"github.com/kameas-ai/kenaz-plugin/internal/fleetschema"
)

// Identity is stamped onto every resource by the relay.
// It is never read from harness attributes.
type Identity struct {
	// Env is the session's environment. It is not encoded; it lets the
	// emitted batch be bound to the account that stamped it.
	Env       fleetapi.Env
	UserID    string // token sub
	OrgID     string // resourceowner claim
	MachineID string // relay node id
	Tier      string
	// ServiceVersion is the relay's own version.
	ServiceVersion string
	// ClientVersions are harness versions seen (decode.Result), emitted as
	// kameas.client.version at schema rev >= 2.
	ClientVersions map[canon.ClientKind]string
}

// ServiceName is the relay's service.name.
const ServiceName = "kameas-fleet"

// ScopeName is the instrumentation scope on every batch.
const ScopeName = "kameas-fleet/projector"

// ErrIdentity means the identity is incomplete; nothing is encoded.
var ErrIdentity = errors.New("project: identity requires user, org and machine ids")

// Encode serializes records into OTLP protobuf batches of at most
// fleetschema.MaxRecordsPerRequest records each, one resource group per
// client kind. The returned batches are final: the transport adds
// nothing.
func Encode(recs []Record, id Identity, opt Options) ([]fleetapi.ProjectedBatch, error) {
	if id.UserID == "" || id.OrgID == "" || id.MachineID == "" {
		return nil, ErrIdentity
	}
	var logs, spans []Record
	for _, r := range recs {
		if r.IsSpan() {
			spans = append(spans, r)
		} else {
			logs = append(logs, r)
		}
	}
	var out []fleetapi.ProjectedBatch
	for _, chunk := range chunks(logs) {
		b, err := encodeLogs(chunk, id, opt)
		if err != nil {
			return nil, err
		}
		out = append(out, b)
	}
	for _, chunk := range chunks(spans) {
		b, err := encodeSpans(chunk, id, opt)
		if err != nil {
			return nil, err
		}
		out = append(out, b)
	}
	return out, nil
}

func chunks(rs []Record) [][]Record {
	var out [][]Record
	for len(rs) > 0 {
		n := fleetschema.MaxRecordsPerRequest
		if len(rs) < n {
			n = len(rs)
		}
		out = append(out, rs[:n])
		rs = rs[n:]
	}
	return out
}

func byClient(rs []Record) ([]canon.ClientKind, map[canon.ClientKind][]Record) {
	m := map[canon.ClientKind][]Record{}
	for _, r := range rs {
		m[r.Client] = append(m[r.Client], r)
	}
	keys := make([]canon.ClientKind, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Slice(keys, func(i, j int) bool { return keys[i] < keys[j] })
	return keys, m
}

func resource(id Identity, client canon.ClientKind, opt Options) *resourcev1.Resource {
	kv := []*commonv1.KeyValue{
		kvStr("kameas.user.id", id.UserID),
		kvStr("kameas.org.id", id.OrgID),
		kvStr("kameas.machine.id", id.MachineID),
		kvStr("service.name", ServiceName),
	}
	if id.Tier != "" {
		kv = append(kv, kvStr("kameas.tier", id.Tier))
	}
	if id.ServiceVersion != "" {
		kv = append(kv, kvStr("service.version", id.ServiceVersion))
	}
	if opt.v2() && client.Valid() {
		kv = append(kv, kvStr("kameas.client.kind", string(client)))
		if v := id.ClientVersions[client]; v != "" {
			kv = append(kv, kvStr("kameas.client.version", v))
		}
	}
	return &resourcev1.Resource{Attributes: kv}
}

func encodeLogs(rs []Record, id Identity, opt Options) (fleetapi.ProjectedBatch, error) {
	keys, groups := byClient(rs)
	data := &logsv1.LogsData{}
	for _, k := range keys {
		var lrs []*logsv1.LogRecord
		for _, r := range groups[k] {
			ts := uint64(r.Time.UnixNano())
			lrs = append(lrs, &logsv1.LogRecord{
				TimeUnixNano:         ts,
				ObservedTimeUnixNano: ts,
				Attributes:           []*commonv1.KeyValue{kvStr("kameas.event.kind", r.EventKind)},
				Body:                 &commonv1.AnyValue{Value: &commonv1.AnyValue_KvlistValue{KvlistValue: &commonv1.KeyValueList{Values: kvs(r.Body)}}},
			})
		}
		data.ResourceLogs = append(data.ResourceLogs, &logsv1.ResourceLogs{
			Resource:  resource(id, k, opt),
			ScopeLogs: []*logsv1.ScopeLogs{{Scope: &commonv1.InstrumentationScope{Name: ScopeName}, LogRecords: lrs}},
		})
	}
	body, err := proto.Marshal(data)
	if err != nil {
		return fleetapi.ProjectedBatch{}, err
	}
	return fleetapi.ProjectedBatch{Signal: fleetapi.SignalLogs, ContentType: fleetapi.ContentTypeProtobuf, Body: body, Records: len(rs)}, nil
}

func encodeSpans(rs []Record, id Identity, opt Options) (fleetapi.ProjectedBatch, error) {
	keys, groups := byClient(rs)
	data := &tracev1.TracesData{}
	for _, k := range keys {
		var sps []*tracev1.Span
		for _, r := range groups[k] {
			sps = append(sps, &tracev1.Span{
				TraceId:           randBytes(16), // fresh ids: harness trace ids never leave
				SpanId:            randBytes(8),
				Name:              r.SpanName,
				Kind:              tracev1.Span_SPAN_KIND_CLIENT,
				StartTimeUnixNano: uint64(r.Start.UnixNano()),
				EndTimeUnixNano:   uint64(r.Time.UnixNano()),
				Attributes:        kvs(r.Attrs),
			})
		}
		data.ResourceSpans = append(data.ResourceSpans, &tracev1.ResourceSpans{
			Resource:   resource(id, k, opt),
			ScopeSpans: []*tracev1.ScopeSpans{{Scope: &commonv1.InstrumentationScope{Name: ScopeName}, Spans: sps}},
		})
	}
	body, err := proto.Marshal(data)
	if err != nil {
		return fleetapi.ProjectedBatch{}, err
	}
	return fleetapi.ProjectedBatch{Signal: fleetapi.SignalTraces, ContentType: fleetapi.ContentTypeProtobuf, Body: body, Records: len(rs)}, nil
}

func kvStr(k, v string) *commonv1.KeyValue {
	return &commonv1.KeyValue{Key: k, Value: &commonv1.AnyValue{Value: &commonv1.AnyValue_StringValue{StringValue: v}}}
}

func kvs(fs []Field) []*commonv1.KeyValue {
	out := make([]*commonv1.KeyValue, 0, len(fs))
	for _, f := range fs {
		var v *commonv1.AnyValue
		switch f.Type {
		case TString:
			v = &commonv1.AnyValue{Value: &commonv1.AnyValue_StringValue{StringValue: f.Str}}
		case TInt:
			v = &commonv1.AnyValue{Value: &commonv1.AnyValue_IntValue{IntValue: f.Int}}
		case TFloat:
			v = &commonv1.AnyValue{Value: &commonv1.AnyValue_DoubleValue{DoubleValue: f.Float}}
		case TBool:
			v = &commonv1.AnyValue{Value: &commonv1.AnyValue_BoolValue{BoolValue: f.Bool}}
		case TStrings:
			arr := make([]*commonv1.AnyValue, len(f.Strs))
			for i, s := range f.Strs {
				arr[i] = &commonv1.AnyValue{Value: &commonv1.AnyValue_StringValue{StringValue: s}}
			}
			v = &commonv1.AnyValue{Value: &commonv1.AnyValue_ArrayValue{ArrayValue: &commonv1.ArrayValue{Values: arr}}}
		default:
			continue
		}
		out = append(out, &commonv1.KeyValue{Key: f.Key, Value: v})
	}
	return out
}

func randBytes(n int) []byte {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		panic(err) // crypto/rand never fails on supported platforms
	}
	return b
}
