package allowlist

import (
	"strings"
	"testing"
)

func TestTelemetryClosedSet(t *testing.T) {
	al := Telemetry()
	want := map[string]string{
		"harness.conversation_started": "conversation_id,model_provider",
		"harness.conversation_ended":   "conversation_id,cost_known,cost_usd,duration_ms,token_in,token_out",
		"harness.tool_invoked":         "latency_ms,success,tool_name",
		"harness.error":                "category,recoverable",
	}
	if len(al.BodyKeys) != len(want) {
		t.Fatalf("kinds = %v", al.BodyKeys)
	}
	for kind, keys := range want {
		if got := strings.Join(al.BodyKeys[kind], ","); got != keys {
			t.Errorf("%s = %s, want %s", kind, got, keys)
		}
	}
	if len(al.SpanAttributes) != 1 || len(al.SpanAttributes["gen_ai.chat"]) == 0 {
		t.Fatalf("spans = %v", al.SpanAttributes)
	}
	for _, k := range al.SpanAttributes["gen_ai.chat"] {
		if k == "gen_ai.response.id" {
			t.Fatal("gen_ai.response.id must never be allowlisted")
		}
	}
	if strings.Join(al.LogAttributes, ",") != "kameas.event.kind" {
		t.Fatalf("log attrs = %v", al.LogAttributes)
	}
}

// TestMemoryClosedSet pins every key the memory lane can send. Changing
// one changes what is sent: it needs review and a memory-sync notice
// update.
func TestMemoryClosedSet(t *testing.T) {
	m := Memory()
	for name, c := range map[string][2]string{
		"push":     {strings.Join(m.PushRequest, ","), "base_cursor,device_id,items"},
		"item":     {strings.Join(m.PushItem, ","), "content,content_hash,created_at,fields,files_modified,files_read,hlc,id,kind,last_accessed,op,recall_own,retrieval_weight,source,source_turn,tool_name,turn_id"},
		"fields":   {strings.Join(m.Fields, ","), "pinned,scope_kind,title"},
		"register": {strings.Join(m.FieldValue, ","), "hlc,v"},
		"settings": {strings.Join(m.SettingsUpdate, ","), "consent_version,enabled,scopes"},
		"forget":   {strings.Join(m.Forget, ","), "ids"},
		"all":      {strings.Join(m.ForgetAll, ","), "confirm"},
		"pull":     {strings.Join(m.PullQuery, ","), "cursor,device_id,limit"},
	} {
		if c[0] != c[1] {
			t.Errorf("%s = %s, want %s", name, c[0], c[1])
		}
	}
	ok := []byte(`{"device_id":"d","base_cursor":"","items":[{"op":"upsert","id":"mem-1","content":"x","fields":{"title":{"v":"t","hlc":"h"}}}]}`)
	if err := m.CheckPushBody(ok); err != nil {
		t.Fatal(err)
	}
	for _, bad := range []string{
		`{"device_id":"d","user_id":"u","items":[]}`,
		`{"items":[{"op":"upsert","id":"x","email":"a@b"}]}`,
		`{"items":[{"op":"upsert","id":"x","fields":{"tags":{"v":[],"hlc":"h"}}}]}`,
		`{"items":[{"op":"upsert","id":"x","fields":{"title":{"v":"t","hlc":"h","who":"me"}}}]}`,
	} {
		if m.CheckPushBody([]byte(bad)) == nil {
			t.Errorf("accepted %s", bad)
		}
	}
}
