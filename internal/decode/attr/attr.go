// Package attr reads named OTLP attributes. Decoders use it to read only
// the attributes they name; there is no iteration or copying API, by design
// ("never copies a source attribute by key pattern").
package attr

import (
	"strconv"
	"strings"

	commonv1 "go.opentelemetry.io/proto/otlp/common/v1"
)

// Set is a read-only view over one or more OTLP attribute lists. Earlier
// lists take precedence (record attributes before resource attributes).
type Set struct {
	lists [][]*commonv1.KeyValue
}

// Of builds a Set. Pass record attributes first, then resource attributes.
func Of(lists ...[]*commonv1.KeyValue) Set { return Set{lists: lists} }

func (s Set) get(key string) *commonv1.AnyValue {
	for _, l := range s.lists {
		for _, kv := range l {
			if kv.GetKey() == key {
				return kv.GetValue()
			}
		}
	}
	return nil
}

// Has reports whether key is present.
func (s Set) Has(key string) bool { return s.get(key) != nil }

// String returns key as a string. Numbers and bools are formatted.
func (s Set) String(key string) string {
	v := s.get(key)
	if v == nil {
		return ""
	}
	switch x := v.GetValue().(type) {
	case *commonv1.AnyValue_StringValue:
		return x.StringValue
	case *commonv1.AnyValue_IntValue:
		return strconv.FormatInt(x.IntValue, 10)
	case *commonv1.AnyValue_DoubleValue:
		return strconv.FormatFloat(x.DoubleValue, 'f', -1, 64)
	case *commonv1.AnyValue_BoolValue:
		return strconv.FormatBool(x.BoolValue)
	}
	return ""
}

// Int returns key as an int64. String-encoded integers are parsed (Codex
// records most numbers via Display). ok is false when absent or invalid.
func (s Set) Int(key string) (int64, bool) {
	v := s.get(key)
	if v == nil {
		return 0, false
	}
	switch x := v.GetValue().(type) {
	case *commonv1.AnyValue_IntValue:
		return x.IntValue, true
	case *commonv1.AnyValue_DoubleValue:
		return int64(x.DoubleValue), true
	case *commonv1.AnyValue_StringValue:
		str := strings.TrimSpace(x.StringValue)
		if n, err := strconv.ParseInt(str, 10, 64); err == nil {
			return n, true
		}
		if f, err := strconv.ParseFloat(str, 64); err == nil {
			return int64(f), true
		}
	}
	return 0, false
}

// NonNeg returns Int clamped at 0 (token counts, durations).
func (s Set) NonNeg(key string) int64 {
	n, _ := s.Int(key)
	if n < 0 {
		return 0
	}
	return n
}

// Float returns key as a float64.
func (s Set) Float(key string) (float64, bool) {
	v := s.get(key)
	if v == nil {
		return 0, false
	}
	switch x := v.GetValue().(type) {
	case *commonv1.AnyValue_DoubleValue:
		return x.DoubleValue, true
	case *commonv1.AnyValue_IntValue:
		return float64(x.IntValue), true
	case *commonv1.AnyValue_StringValue:
		if f, err := strconv.ParseFloat(strings.TrimSpace(x.StringValue), 64); err == nil {
			return f, true
		}
	}
	return 0, false
}

// Bool returns key as a bool; "true"/"false" strings are accepted.
func (s Set) Bool(key string) (bool, bool) {
	v := s.get(key)
	if v == nil {
		return false, false
	}
	switch x := v.GetValue().(type) {
	case *commonv1.AnyValue_BoolValue:
		return x.BoolValue, true
	case *commonv1.AnyValue_StringValue:
		b, err := strconv.ParseBool(strings.TrimSpace(x.StringValue))
		return b, err == nil
	}
	return false, false
}

// ContentKeys are attribute keys that carry user content in either
// harness when content logging is enabled. Error-message keys are left out
// on purpose: both harnesses send them by default on API errors, so they
// say nothing about the content flags (they are still never read). Decoders never read them; their
// presence only increments the content_seen counter.
var ContentKeys = []string{
	"prompt", "prompt_text", "tool_input", "tool_parameters", "arguments", "output",
	"response", "new_context", "system_reminders",
	"system_prompt_preview", "user_system_prompt", "response.model_output",
	"body", "request_body", "response_body",
}

// ContentSeen reports whether any content-bearing key is present with a
// non-redacted value. It looks only at presence, never at the value beyond
// the "[REDACTED]" sentinel comparison.
func (s Set) ContentSeen() bool {
	for _, k := range ContentKeys {
		v := s.get(k)
		if v == nil {
			continue
		}
		if sv, ok := v.GetValue().(*commonv1.AnyValue_StringValue); ok {
			if sv.StringValue == "" || sv.StringValue == "[REDACTED]" {
				continue
			}
		}
		return true
	}
	return false
}
