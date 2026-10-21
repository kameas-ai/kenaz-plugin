// Package allowlist exports the closed set of keys the public layer can
// send to Kameas Fleet.
//
// The proprietary Fleet connection module's CI imports this package and fails if
// its transport ever emits a key outside these sets. Only public code may
// extend them, and every extension changes what is sent, so it needs review
// by Kameas and an update to the published telemetry disclosure.
package allowlist

import (
	"reflect"
	"sort"
	"strings"

	"github.com/kameas-ai/kenaz-plugin/fleetapi"
	"github.com/kameas-ai/kenaz-plugin/internal/fleetschema"
)

// EnrollBodyKeys returns the JSON keys of fleetapi.EnrollBody, derived from
// its struct tags so the list cannot drift from the type.
func EnrollBodyKeys() []string {
	t := reflect.TypeOf(fleetapi.EnrollBody{})
	keys := make([]string, 0, t.NumField())
	for i := 0; i < t.NumField(); i++ {
		name, _, _ := strings.Cut(t.Field(i).Tag.Get("json"), ",")
		if name != "" && name != "-" {
			keys = append(keys, name)
		}
	}
	sort.Strings(keys)
	return keys
}

// TelemetryAllowlist is the projector's closed output set.
type TelemetryAllowlist struct {
	// ResourceAttributes are stamped by the relay, never copied from the
	// harness.
	ResourceAttributes []string
	// LogAttributes are per-record log attributes.
	LogAttributes []string
	// BodyKeys maps kameas.event.kind to its permitted body keys.
	BodyKeys map[string][]string
	// SpanAttributes maps span name to its permitted attributes.
	SpanAttributes map[string][]string
}

// Telemetry returns the projector's closed output set,
// derived from internal/fleetschema.ClosedOutput, which the projector's
// tests prove covers every key it emits. Keys are sorted.
func Telemetry() TelemetryAllowlist {
	o := fleetschema.ClosedOutput()
	body := make(map[string][]string, len(o.BodyKeys))
	for k, v := range o.BodyKeys {
		body[k] = sorted(v)
	}
	spans := make(map[string][]string, len(o.SpanAttributes))
	for k, v := range o.SpanAttributes {
		spans[k] = sorted(v)
	}
	return TelemetryAllowlist{
		ResourceAttributes: sorted(o.ResourceAttributes),
		LogAttributes:      sorted(o.LogAttributes),
		BodyKeys:           body,
		SpanAttributes:     spans,
	}
}

func sorted(v []string) []string {
	out := append([]string(nil), v...)
	sort.Strings(out)
	return out
}
