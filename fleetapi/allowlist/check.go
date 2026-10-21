package allowlist

import (
	"errors"
	"fmt"

	commonv1 "go.opentelemetry.io/proto/otlp/common/v1"
	logsv1 "go.opentelemetry.io/proto/otlp/logs/v1"
	tracev1 "go.opentelemetry.io/proto/otlp/trace/v1"
	"google.golang.org/protobuf/proto"

	"github.com/kameas-ai/kenaz-plugin/fleetapi"
)

// LogKindAttribute is the log attribute that carries the event kind.
const LogKindAttribute = "kameas.event.kind"

// Check verifies that a projected protobuf batch carries only allowlisted
// keys: resource attributes, log attributes, per-kind body keys, and
// per-span attributes, with no span events, links or status messages. The
// public end-to-end test and the official build's CI gate
// both use it.
func (a TelemetryAllowlist) Check(b fleetapi.ProjectedBatch) error {
	var errs []error
	res := func(kvs []*commonv1.KeyValue) {
		for _, kv := range kvs {
			if !contains(a.ResourceAttributes, kv.GetKey()) {
				errs = append(errs, fmt.Errorf("resource attribute %q", kv.GetKey()))
			}
		}
	}
	switch b.Signal {
	case fleetapi.SignalLogs:
		var m logsv1.LogsData
		if err := proto.Unmarshal(b.Body, &m); err != nil {
			return fmt.Errorf("allowlist: decode logs: %w", err)
		}
		for _, rl := range m.GetResourceLogs() {
			res(rl.GetResource().GetAttributes())
			for _, sl := range rl.GetScopeLogs() {
				for _, lr := range sl.GetLogRecords() {
					kind := ""
					for _, kv := range lr.GetAttributes() {
						if !contains(a.LogAttributes, kv.GetKey()) {
							errs = append(errs, fmt.Errorf("log attribute %q", kv.GetKey()))
						}
						if kv.GetKey() == LogKindAttribute {
							kind = kv.GetValue().GetStringValue()
						}
					}
					allowed, ok := a.BodyKeys[kind]
					if !ok {
						errs = append(errs, fmt.Errorf("event kind %q", kind))
					}
					for _, kv := range lr.GetBody().GetKvlistValue().GetValues() {
						if !contains(allowed, kv.GetKey()) {
							errs = append(errs, fmt.Errorf("%s body key %q", kind, kv.GetKey()))
						}
					}
				}
			}
		}
	case fleetapi.SignalTraces:
		var m tracev1.TracesData
		if err := proto.Unmarshal(b.Body, &m); err != nil {
			return fmt.Errorf("allowlist: decode traces: %w", err)
		}
		for _, rs := range m.GetResourceSpans() {
			res(rs.GetResource().GetAttributes())
			for _, ss := range rs.GetScopeSpans() {
				for _, sp := range ss.GetSpans() {
					allowed, ok := a.SpanAttributes[sp.GetName()]
					if !ok {
						errs = append(errs, fmt.Errorf("span %q", sp.GetName()))
					}
					for _, kv := range sp.GetAttributes() {
						if !contains(allowed, kv.GetKey()) {
							errs = append(errs, fmt.Errorf("span %s attribute %q", sp.GetName(), kv.GetKey()))
						}
					}
					if len(sp.GetEvents()) > 0 || len(sp.GetLinks()) > 0 || sp.GetStatus().GetMessage() != "" {
						errs = append(errs, fmt.Errorf("span %s carries events, links or a status message", sp.GetName()))
					}
				}
			}
		}
	default:
		return fmt.Errorf("allowlist: signal %q is never sent", b.Signal)
	}
	return errors.Join(errs...)
}

func contains(xs []string, x string) bool {
	for _, v := range xs {
		if v == x {
			return true
		}
	}
	return false
}
