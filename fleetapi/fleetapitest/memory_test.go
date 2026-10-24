package fleetapitest_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"

	"github.com/kameas-ai/kenaz-plugin/fleetapi"
	"github.com/kameas-ai/kenaz-plugin/fleetapi/allowlist"
	"github.com/kameas-ai/kenaz-plugin/fleetapi/fleetapitest"
)

// The push example from Fleet's docs/contract-harness-memory.md push section,
// verbatim apart from a real hash. It must round-trip through the public
// types without losing or adding a key, and pass the allowlist.
const contractPush = `{
  "device_id": "mach-7f3c",
  "base_cursor": "1842",
  "items": [
    {"op":"upsert","id":"mem-0123456789abcdef0123456789abcdef",
     "content":"Build uses make","content_hash":"HASH",
     "kind":"raw","retrieval_weight":1.0,"source":"post-llm","source_turn":"assistant",
     "files_read":["~/src/app/main.go"],"created_at":"2026-10-06T21:04:05.123Z",
     "fields":{
       "title":{"v":"Build uses make","hlc":"0001759784645123:000002:mach-7f3c"},
       "pinned":{"v":true,"hlc":"0001759784645123:000003:mach-7f3c"},
       "scope_kind":{"v":"long_term","hlc":"0001759784645123:000004:mach-7f3c"}},
     "recall_own":7,"last_accessed":"2026-10-06T21:30:00Z"},
    {"op":"forget","id":"mem-1","hlc":"0001759784645123:000005:mach-7f3c"}
  ]
}`

func canon(t *testing.T, b []byte) any {
	t.Helper()
	var v any
	if err := json.Unmarshal(b, &v); err != nil {
		t.Fatal(err)
	}
	return v
}

func TestPushWireRoundTrip(t *testing.T) {
	body := strings.Replace(contractPush, "HASH", strings.Repeat("a", 64), 1)
	var req fleetapi.MemoryPushRequest
	if err := json.Unmarshal([]byte(body), &req); err != nil {
		t.Fatal(err)
	}
	out, err := json.Marshal(req)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(canon(t, []byte(body)), canon(t, out)) {
		t.Fatalf("round trip changed the body:\n%s", out)
	}
	if err := allowlist.Memory().CheckPushBody(out); err != nil {
		t.Fatal(err)
	}
}

// The pull example (pull section): live records and content-free tombstones.
const contractPull = `{"enabled":true,"reset":false,"reset_reason":"","erased_before":"",
 "records":[
  {"id":"mem-1","seq":"1851","state":"live","content":"x","content_hash":"h","kind":"raw","retrieval_weight":1,
   "turn_id":"","source":"","source_turn":"","tool_name":"","files_read":[],"files_modified":[],"created_at":"2026-10-06T21:04:05.123Z",
   "title":"t","title_hlc":"a","pinned":true,"pinned_hlc":"b","scope_kind":"long_term","scope_hlc":"c",
   "recall_count":19,"recall_count_others":12,"last_accessed":"2026-10-06T21:30:00Z","aliases":["mem-aaaa"]},
  {"id":"mem-9","seq":"1853","state":"tombstone","reason":"forgotten"},
  {"id":"mem-c","seq":"1854","state":"tombstone","reason":"superseded","superseded_by":"mem-d"}],
 "cursor":"1854","has_more":false}`

func TestPullWireRoundTrip(t *testing.T) {
	var resp fleetapi.MemoryPullResponse
	if err := json.Unmarshal([]byte(contractPull), &resp); err != nil {
		t.Fatal(err)
	}
	out, _ := json.Marshal(resp)
	if !reflect.DeepEqual(canon(t, []byte(contractPull)), canon(t, out)) {
		t.Fatalf("round trip changed the body:\n%s", out)
	}
	if resp.Records[0].Aliases[0] != "mem-aaaa" || resp.Records[2].SupersededBy != "mem-d" {
		t.Fatalf("decoded = %+v", resp.Records)
	}
	q := fleetapi.MemoryPullRequest{Cursor: "s5.0.0", Limit: 200, DeviceID: "kz-1"}.Query().Encode()
	if q != "cursor=s5.0.0&device_id=kz-1&limit=200" {
		t.Fatalf("query = %s", q)
	}
}

func TestSettingsWire(t *testing.T) {
	on := true
	b, _ := json.Marshal(fleetapi.MemorySettingsUpdate{Enabled: &on, ConsentVersion: "memory-sync/v1"})
	if string(b) != `{"enabled":true,"consent_version":"memory-sync/v1"}` {
		t.Fatalf("settings body = %s", b)
	}
	b, _ = json.Marshal(fleetapi.MemoryForgetAllRequest{Confirm: fleetapi.MemoryForgetAllConfirm})
	if string(b) != `{"confirm":"forget-all"}` {
		t.Fatalf("forget-all body = %s", b)
	}
}

// The HTTP fake answers like Fleet: JSON without a Content-Type, per-item
// results in a 200, envelopes for 4xx, Retry-After on 429.
func TestMemoryHandler(t *testing.T) {
	f := fleetapitest.NewMemoryFleet()
	srv := httptest.NewServer(f.Handler(func(r *http.Request) (fleetapitest.Identity, bool) {
		switch r.Header.Get("Authorization") {
		case "Bearer alice":
			return fleetapitest.Identity{OrgID: "o", UserID: "alice"}, true
		case "Bearer staff":
			return fleetapitest.Identity{Staff: true}, true
		}
		return fleetapitest.Identity{}, false
	}))
	defer srv.Close()
	do := func(method, path, token, body string) (*http.Response, map[string]any) {
		req, _ := http.NewRequest(method, srv.URL+path, strings.NewReader(body))
		req.Header.Set("Authorization", "Bearer "+token)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		var m map[string]any
		json.NewDecoder(resp.Body).Decode(&m)
		return resp, m
	}
	if r, m := do("PUT", "/api/v1/memory/settings", "alice", `{"enabled":true}`); r.StatusCode != 422 || m["code"] != "consent_version_required" {
		t.Fatalf("enable without consent = %d %v", r.StatusCode, m)
	}
	if r, _ := do("PUT", "/api/v1/memory/settings", "alice", `{"enabled":true,"consent_version":"memory-sync/v1"}`); r.StatusCode != 200 {
		t.Fatal(r.StatusCode)
	}
	body := `{"device_id":"d1","base_cursor":"","items":[{"op":"upsert","id":"mem-1","content":"hello","kind":"raw","fields":{"scope_kind":{"v":"global","hlc":"0000000000001:000000:d1"}}},{"op":"upsert","id":"mem-2","content":"key AKIAABCDEFGHIJKLMNOP","kind":"raw","fields":{"scope_kind":{"v":"global","hlc":"0000000000000001:000000:d1"}}}]}`
	r, m := do("POST", "/api/v1/memory/push", "alice", body)
	if r.StatusCode != 200 {
		t.Fatalf("push = %d %v", r.StatusCode, m)
	}
	res := m["results"].([]any)
	if res[0].(map[string]any)["code"] != "invalid_field" || res[1].(map[string]any)["code"] != "secret_detected" || res[1].(map[string]any)["retry"] != false {
		t.Fatalf("results = %v", res)
	}
	if r, m := do("POST", "/api/v1/memory/forget-all", "alice", `{}`); r.StatusCode != 422 || m["code"] != "confirmation_required" {
		t.Fatalf("forget-all = %d %v", r.StatusCode, m)
	}
	if r, m := do("GET", "/api/v1/memory/pull", "staff", ""); r.StatusCode != 403 || m["code"] != "staff_not_permitted" {
		t.Fatalf("staff = %d %v", r.StatusCode, m)
	}
	if r, m := do("GET", "/api/v1/memory/pull?cursor=s1.2", "alice", ""); r.StatusCode != 400 || m["code"] != "invalid_cursor" {
		t.Fatalf("bad cursor = %d %v", r.StatusCode, m)
	}
	f.Fault = func(c string, after bool) error {
		if !after {
			return fleetapitest.RateLimited(30)
		}
		return nil
	}
	r, m = do("GET", "/api/v1/memory/pull", "alice", "")
	if r.StatusCode != 429 || r.Header.Get("Retry-After") != "30" || m["details"].(map[string]any)["retry_after_seconds"] != float64(30) {
		t.Fatalf("429 = %d %v %v", r.StatusCode, r.Header, m)
	}
	f.Fault = nil
	// Export: NDJSON lines then the trailer.
	req, _ := http.NewRequest("GET", srv.URL+"/api/v1/memory/export", nil)
	req.Header.Set("Authorization", "Bearer alice")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.Header.Get("Content-Type") != "application/x-ndjson" {
		t.Fatal(resp.Header)
	}
}

// The fake enforces the Memory account binding like a real adapter: no
// ExpectedSession, or one for another account, is refused with
// identity_mismatch and nothing reaches the server.
func TestMemoryClientEnforcesExpectedSession(t *testing.T) {
	ctx := context.Background()
	f := fleetapitest.NewMemoryFleet()
	c := f.Client("org-1", "alice")
	item := fleetapi.MemoryPushItem{Op: "forget", ID: "01JABCDEFGHJKMNPQRSTVWXYZ0"}
	for name, cctx := range map[string]context.Context{
		"none":      ctx,
		"other":     f.ContextFor(ctx, "org-1", "bob"),
		"other-org": f.ContextFor(ctx, "org-2", "alice"),
		"other-env": fleetapi.WithExpectedSession(ctx, fleetapi.ExpectedSession{Env: fleetapi.EnvProd, OrgID: "org-1", Subject: "alice"}),
	} {
		if _, err := c.Push(cctx, fleetapi.MemoryPushRequest{DeviceID: "d", Items: []fleetapi.MemoryPushItem{item}}); fleetapi.APICode(err) != fleetapi.CodeIdentityMismatch {
			t.Fatalf("%s: err = %v", name, err)
		}
	}
	if f.Calls("push") != 0 || f.Refused() != 4 {
		t.Fatalf("calls=%d refused=%d", f.Calls("push"), f.Refused())
	}
	if _, err := c.Settings(f.ContextFor(ctx, "org-1", "alice")); err != nil {
		t.Fatal(err)
	}
	// A session-following client checks the session it picks per request.
	who := "alice"
	sc := f.SessionClient(func(context.Context) (fleetapi.Session, error) {
		return fleetapi.Session{Env: fleetapi.EnvDev, OrgID: "org-1", UserID: who}, nil
	})
	actx := f.ContextFor(ctx, "org-1", "alice")
	if _, err := sc.Settings(actx); err != nil {
		t.Fatal(err)
	}
	who = "bob"
	if _, err := sc.Settings(actx); fleetapi.APICode(err) != fleetapi.CodeIdentityMismatch {
		t.Fatalf("switched session: err = %v", err)
	}
}
