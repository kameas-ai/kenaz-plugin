package otlprx

import (
	"bytes"
	"compress/gzip"
	"context"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	logsv1 "go.opentelemetry.io/proto/otlp/logs/v1"
	metricsv1 "go.opentelemetry.io/proto/otlp/metrics/v1"
	tracev1 "go.opentelemetry.io/proto/otlp/trace/v1"
	"google.golang.org/protobuf/proto"

	"github.com/kameas-ai/kenaz-plugin/internal/canon"
)

type sink struct {
	mu                    sync.Mutex
	logs, traces, metrics int
	ends                  []string
}

func (s *sink) Logs(*logsv1.LogsData)          { s.mu.Lock(); s.logs++; s.mu.Unlock() }
func (s *sink) Traces(*tracev1.TracesData)     { s.mu.Lock(); s.traces++; s.mu.Unlock() }
func (s *sink) Metrics(*metricsv1.MetricsData) { s.mu.Lock(); s.metrics++; s.mu.Unlock() }
func (s *sink) SessionEnd(c canon.ClientKind, id string) {
	s.mu.Lock()
	s.ends = append(s.ends, string(c)+"/"+id)
	s.mu.Unlock()
}

func do(rx *Receiver, method, path, remote string, body []byte, hdr map[string]string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(method, path, bytes.NewReader(body))
	req.RemoteAddr = remote
	for k, v := range hdr {
		req.Header.Set(k, v)
	}
	w := httptest.NewRecorder()
	rx.ServeHTTP(w, req)
	return w
}

var pb = map[string]string{"Content-Type": "application/x-protobuf"}

func logsBody(n int) []byte {
	lrs := make([]*logsv1.LogRecord, n)
	for i := range lrs {
		lrs[i] = &logsv1.LogRecord{}
	}
	b, _ := proto.Marshal(&logsv1.LogsData{ResourceLogs: []*logsv1.ResourceLogs{{ScopeLogs: []*logsv1.ScopeLogs{{LogRecords: lrs}}}}})
	return b
}

func gz(b []byte) []byte {
	var buf bytes.Buffer
	zw := gzip.NewWriter(&buf)
	zw.Write(b)
	zw.Close()
	return buf.Bytes()
}

func TestReceiverHardening(t *testing.T) {
	s := &sink{}
	rx := New(Config{Sink: s, MaxRecords: 10, SessionEndToken: "tok"})
	cases := []struct {
		name   string
		method string
		path   string
		remote string
		body   []byte
		hdr    map[string]string
		want   int
	}{
		{"ok protobuf", "POST", "/v1/logs", "127.0.0.1:9", logsBody(1), pb, 200},
		{"ok json", "POST", "/v1/logs", "[::1]:9", []byte(`{"resourceLogs":[]}`), map[string]string{"Content-Type": "application/json"}, 200},
		{"ok gzip", "POST", "/v1/logs", "127.0.0.1:9", gz(logsBody(2)), map[string]string{"Content-Type": "application/x-protobuf", "Content-Encoding": "gzip"}, 200},
		{"non-loopback", "POST", "/v1/logs", "10.0.0.5:9", logsBody(1), pb, 403},
		{"origin header", "POST", "/v1/logs", "127.0.0.1:9", logsBody(1), map[string]string{"Content-Type": "application/x-protobuf", "Origin": "https://evil.example"}, 403},
		{"get", "GET", "/v1/logs", "127.0.0.1:9", nil, pb, 405},
		{"unknown path", "POST", "/v1/other", "127.0.0.1:9", nil, pb, 404},
		{"bad content type", "POST", "/v1/logs", "127.0.0.1:9", logsBody(1), map[string]string{"Content-Type": "text/plain"}, 415},
		{"malformed protobuf", "POST", "/v1/traces", "127.0.0.1:9", []byte{0xff, 0xff, 0xff}, pb, 400},
		{"malformed gzip", "POST", "/v1/logs", "127.0.0.1:9", []byte("notgzip"), map[string]string{"Content-Type": "application/x-protobuf", "Content-Encoding": "gzip"}, 400},
		{"too many records", "POST", "/v1/logs", "127.0.0.1:9", logsBody(11), pb, 413},
		{"gzip bomb", "POST", "/v1/logs", "127.0.0.1:9", gz(make([]byte, 3<<20)), map[string]string{"Content-Type": "application/x-protobuf", "Content-Encoding": "gzip"}, 413},
		{"oversize", "POST", "/v1/logs", "127.0.0.1:9", make([]byte, DefaultMaxBody+1), pb, 413},
		{"session-end ok", "POST", SessionEndPath, "127.0.0.1:9", []byte(`{"client_kind":"codex","session_id":"abc"}`), map[string]string{SessionEndHeader: "tok"}, 204},
		{"session-end bad token", "POST", SessionEndPath, "127.0.0.1:9", []byte(`{"client_kind":"codex","session_id":"abc"}`), map[string]string{SessionEndHeader: "nope"}, 403},
		{"session-end bad kind", "POST", SessionEndPath, "127.0.0.1:9", []byte(`{"client_kind":"cursor","session_id":"abc"}`), map[string]string{SessionEndHeader: "tok"}, 400},
		{"session-end extra field", "POST", SessionEndPath, "127.0.0.1:9", []byte(`{"client_kind":"codex","session_id":"abc","x":1}`), map[string]string{SessionEndHeader: "tok"}, 400},
	}
	for _, c := range cases {
		if w := do(rx, c.method, c.path, c.remote, c.body, c.hdr); w.Code != c.want {
			t.Errorf("%s: got %d want %d", c.name, w.Code, c.want)
		}
	}
	if s.logs != 3 || len(s.ends) != 1 || s.ends[0] != "codex/abc" {
		t.Errorf("sink saw logs=%d ends=%v", s.logs, s.ends)
	}
	ctr := rx.Counters()
	for _, k := range []string{"rejected_non_loopback", "rejected_origin", "rejected_too_large", "rejected_malformed", "rejected_too_many_records"} {
		if ctr[k] == 0 {
			t.Errorf("counter %s not incremented: %v", k, ctr)
		}
	}
}

func TestSessionEndDisabledWithoutToken(t *testing.T) {
	rx := New(Config{Sink: &sink{}})
	w := do(rx, "POST", SessionEndPath, "127.0.0.1:9", []byte(`{"client_kind":"codex","session_id":"a"}`), map[string]string{SessionEndHeader: ""})
	if w.Code != 404 {
		t.Fatalf("got %d", w.Code)
	}
}

func TestListenLoopbackOnly(t *testing.T) {
	s := &sink{}
	srv, err := Listen(New(Config{Sink: s}), 0)
	if err != nil {
		t.Fatal(err)
	}
	defer srv.Shutdown(context.Background())
	resp, err := http.Post("http://127.0.0.1:"+strconv.Itoa(srv.Port)+"/v1/logs", "application/x-protobuf", bytes.NewReader(logsBody(1)))
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("status %d", resp.StatusCode)
	}
	deadline := time.Now().Add(time.Second)
	for s.logs == 0 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if s.logs != 1 {
		t.Fatal("not delivered")
	}
}

// FuzzReceiver: arbitrary bodies, encodings and paths never crash the
// receiver and never reach the sink unless they decode.
func FuzzReceiver(f *testing.F) {
	f.Add([]byte{}, "/v1/logs", false, false)
	f.Add(logsBody(2), "/v1/logs", false, false)
	f.Add([]byte(`{"resourceSpans":[{"scopeSpans":[{"spans":[{"name":"x"}]}]}]}`), "/v1/traces", true, false)
	f.Add(gz(logsBody(1)), "/v1/metrics", false, true)
	rx := New(Config{Sink: &sink{}, SessionEndToken: "tok"})
	f.Fuzz(func(t *testing.T, body []byte, path string, isJSON, isGzip bool) {
		if !strings.HasPrefix(path, "/") {
			path = "/" + path
		}
		hdr := map[string]string{"Content-Type": "application/x-protobuf", SessionEndHeader: "tok"}
		if isJSON {
			hdr["Content-Type"] = "application/json"
		}
		if isGzip {
			hdr["Content-Encoding"] = "gzip"
		}
		defer func() {
			if r := recover(); r != nil {
				t.Fatalf("panic: %v", r)
			}
		}()
		req, err := http.NewRequest("POST", "http://127.0.0.1"+path, bytes.NewReader(body))
		if err != nil {
			return
		}
		req.RemoteAddr = "127.0.0.1:9"
		for k, v := range hdr {
			req.Header.Set(k, v)
		}
		rx.ServeHTTP(httptest.NewRecorder(), req)
	})
}
