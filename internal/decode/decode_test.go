package decode

import (
	"testing"
	"time"

	commonv1 "go.opentelemetry.io/proto/otlp/common/v1"
	logsv1 "go.opentelemetry.io/proto/otlp/logs/v1"
	resourcev1 "go.opentelemetry.io/proto/otlp/resource/v1"

	"github.com/kameas-ai/kenaz-plugin/internal/canon"
	"github.com/kameas-ai/kenaz-plugin/internal/fixtures"
)

func kv(k, v string) *commonv1.KeyValue {
	return &commonv1.KeyValue{Key: k, Value: &commonv1.AnyValue{Value: &commonv1.AnyValue_StringValue{StringValue: v}}}
}

func logs(svc string, recs ...*logsv1.LogRecord) *logsv1.LogsData {
	return &logsv1.LogsData{ResourceLogs: []*logsv1.ResourceLogs{{
		Resource:  &resourcev1.Resource{Attributes: []*commonv1.KeyValue{kv("service.name", svc)}},
		ScopeLogs: []*logsv1.ScopeLogs{{LogRecords: recs}},
	}}}
}

func kinds(evs []canon.Event) map[canon.Kind]int {
	m := map[canon.Kind]int{}
	for _, e := range evs {
		m[e.Kind]++
	}
	return m
}

func TestUnknownSourceDropped(t *testing.T) {
	r := Decoder{}.Logs(logs("cursor", &logsv1.LogRecord{Attributes: []*commonv1.KeyValue{kv("event.name", "tool_result"), kv("session.id", "s")}}))
	if r.UnknownSource != 1 || len(r.Events) != 0 {
		t.Fatalf("%+v", r)
	}
}

func TestClaudeIdentifiedByServiceNameOrPrefix(t *testing.T) {
	// Unprefixed event.name: identified by service.name.
	r := Decoder{}.Logs(logs("claude-code", &logsv1.LogRecord{Attributes: []*commonv1.KeyValue{
		kv("event.name", "tool_result"), kv("session.id", "s"), kv("tool_name", "Read"), kv("success", "true")}}))
	if kinds(r.Events)[canon.KindToolCall] != 1 {
		t.Fatalf("unprefixed: %+v", r)
	}
	// Prefixed name in the body only: identified by prefix.
	r = Decoder{}.Logs(logs("", &logsv1.LogRecord{
		Body:       &commonv1.AnyValue{Value: &commonv1.AnyValue_StringValue{StringValue: "claude_code.api_error"}},
		Attributes: []*commonv1.KeyValue{kv("session.id", "s"), kv("status_code", "429")}}))
	if kinds(r.Events)[canon.KindAPIError] != 1 || r.Events[1].StatusCode != 429 {
		t.Fatalf("body name: %+v", r)
	}
}

func TestCodexSuccessfulAPIRequestDropped(t *testing.T) {
	r := Decoder{}.Logs(logs("codex_cli_rs", &logsv1.LogRecord{Attributes: []*commonv1.KeyValue{
		kv("event.name", "codex.api_request"), kv("conversation.id", "c"), kv("http.response.status_code", "200")}}))
	if kinds(r.Events)[canon.KindAPIError] != 0 {
		t.Fatal("2xx produced an error")
	}
}

func TestContentSeenAndVersion(t *testing.T) {
	r := Decoder{}.Logs(fixtures.CodexLogs(false))
	if r.ContentSeen == 0 {
		t.Fatal("content-bearing attributes not detected")
	}
	if r.ClientVersions[canon.Codex] != "0.147.0" {
		t.Fatalf("version = %q", r.ClientVersions[canon.Codex])
	}
	// Redacted prompts do not count as content.
	r = Decoder{}.Logs(logs("codex_cli_rs", &logsv1.LogRecord{Attributes: []*commonv1.KeyValue{
		kv("event.name", "codex.user_prompt"), kv("conversation.id", "c"), kv("prompt", "[REDACTED]")}}))
	if r.ContentSeen != 0 {
		t.Fatal("redacted prompt counted as content")
	}
}

func TestTimestampFallback(t *testing.T) {
	at := time.Date(2030, 1, 1, 0, 0, 0, 0, time.UTC)
	r := Decoder{Now: func() time.Time { return at }}.Logs(logs("claude-code", &logsv1.LogRecord{Attributes: []*commonv1.KeyValue{
		kv("event.name", "user_prompt"), kv("session.id", "s")}}))
	if !r.Events[0].Time.Equal(at) {
		t.Fatalf("time = %v", r.Events[0].Time)
	}
}

func TestVersionRe(t *testing.T) {
	for _, ok := range []string{"2.1.217", "0.147.0", "0.147.0-alpha.3", "1.0", "0.145.0-rc1"} {
		if !versionRe.MatchString(ok) {
			t.Errorf("%q rejected", ok)
		}
	}
	for _, bad := range []string{"devbox.local", "1.2.3-acme-secret-repo", "v1.2.3", "repo", "1.2.3+host", "1"} {
		if versionRe.MatchString(bad) {
			t.Errorf("%q accepted", bad)
		}
	}
}
