// Package allowlist exports the closed set of keys the public layer can
// send to Kameas Fleet.
//
// The proprietary Fleet connection module's CI imports this package and fails if
// its transport ever emits a key outside these sets. Only public code may
// extend them, and every extension changes what is sent, so it needs review
// by Kameas and an update to the published telemetry disclosure.
package allowlist

import (
	"encoding/json"
	"errors"
	"fmt"
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

func jsonKeys(t reflect.Type) []string {
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

// MemoryAllowlist is the closed set of keys the memory sync lane can send.
// Memory sync is a separate, opt-in lane: it carries the note text the
// user chose to sync, never telemetry, and only these keys.
type MemoryAllowlist struct {
	PushRequest    []string // push envelope
	PushItem       []string // one push item
	Fields         []string // names allowed in an item's "fields"
	FieldValue     []string // one field register
	SettingsUpdate []string // PUT settings body
	Forget         []string // forget body
	ForgetAll      []string // forget-all body
	PullQuery      []string // pull query parameters
}

// Memory returns the memory lane's closed key sets, derived from the
// fleetapi wire types' struct tags.
func Memory() MemoryAllowlist {
	return MemoryAllowlist{
		PushRequest:    jsonKeys(reflect.TypeOf(fleetapi.MemoryPushRequest{})),
		PushItem:       jsonKeys(reflect.TypeOf(fleetapi.MemoryPushItem{})),
		Fields:         []string{"pinned", "scope_kind", "title"},
		FieldValue:     jsonKeys(reflect.TypeOf(fleetapi.MemoryFieldValue{})),
		SettingsUpdate: jsonKeys(reflect.TypeOf(fleetapi.MemorySettingsUpdate{})),
		Forget:         jsonKeys(reflect.TypeOf(fleetapi.MemoryForgetRequest{})),
		ForgetAll:      jsonKeys(reflect.TypeOf(fleetapi.MemoryForgetAllRequest{})),
		PullQuery:      []string{"cursor", "device_id", "limit"},
	}
}

// CheckPushBody verifies an encoded push body carries only allowlisted
// keys (envelope, items, field names and field registers).
func (a MemoryAllowlist) CheckPushBody(body []byte) error {
	var env map[string]json.RawMessage
	if err := json.Unmarshal(body, &env); err != nil {
		return fmt.Errorf("allowlist: decode memory push: %w", err)
	}
	var errs []error
	for k := range env {
		if !contains(a.PushRequest, k) {
			errs = append(errs, fmt.Errorf("memory push key %q", k))
		}
	}
	var items []map[string]json.RawMessage
	if raw, ok := env["items"]; ok {
		if err := json.Unmarshal(raw, &items); err != nil {
			return fmt.Errorf("allowlist: decode memory items: %w", err)
		}
	}
	for _, it := range items {
		for k, v := range it {
			if !contains(a.PushItem, k) {
				errs = append(errs, fmt.Errorf("memory item key %q", k))
			}
			if k != "fields" {
				continue
			}
			var fields map[string]map[string]json.RawMessage
			if err := json.Unmarshal(v, &fields); err != nil {
				errs = append(errs, fmt.Errorf("memory fields: %v", err))
				continue
			}
			for name, reg := range fields {
				if !contains(a.Fields, name) {
					errs = append(errs, fmt.Errorf("memory field %q", name))
				}
				for rk := range reg {
					if !contains(a.FieldValue, rk) {
						errs = append(errs, fmt.Errorf("memory field register key %q", rk))
					}
				}
			}
		}
	}
	return errors.Join(errs...)
}
