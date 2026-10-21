// Package otlprx is the loopback-only OTLP/HTTP receiver that Claude Code
// and Codex export to, plus the internal SessionEnd endpoint
// used by the plugin hook.
//
// It has no authentication because it binds loopback only. It rejects any
// non-loopback peer and any request carrying an Origin header, which blocks
// browser-originated POSTs from web pages.
package otlprx

import (
	"bytes"
	"compress/gzip"
	"context"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"io"
	"mime"
	"net"
	"net/http"
	"strconv"
	"sync"
	"time"

	logsv1 "go.opentelemetry.io/proto/otlp/logs/v1"
	metricsv1 "go.opentelemetry.io/proto/otlp/metrics/v1"
	tracev1 "go.opentelemetry.io/proto/otlp/trace/v1"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"

	"github.com/kameas-ai/kenaz-plugin/internal/canon"
)

// Sink receives decoded exports. Implementations must not block for long.
type Sink interface {
	Logs(*logsv1.LogsData)
	Traces(*tracev1.TracesData)
	Metrics(*metricsv1.MetricsData)
	SessionEnd(client canon.ClientKind, nativeID string)
}

// Limits.
const (
	DefaultMaxBody    = 4 << 20 // decompressed bytes
	DefaultMaxRecords = 10_000
	MaxGzipRatio      = 100
	maxSessionEndBody = 4 << 10
	maxSessionIDLen   = 256
	// SessionEndPath is the internal hook endpoint.
	SessionEndPath = "/internal/session-end"
	// SessionEndHeader carries the per-boot token.
	SessionEndHeader = "X-Kameas-Session-End-Token"
)

// Config configures a Receiver.
type Config struct {
	Sink Sink
	// SessionEndToken is the per-boot random token written to the 0600
	// relay state file. Empty disables the endpoint.
	SessionEndToken string
	MaxBody         int64
	MaxRecords      int
}

// Receiver is an http.Handler.
type Receiver struct {
	cfg Config

	mu       sync.Mutex
	counters map[string]int
}

// New returns a Receiver.
func New(cfg Config) *Receiver {
	if cfg.MaxBody <= 0 {
		cfg.MaxBody = DefaultMaxBody
	}
	if cfg.MaxRecords <= 0 {
		cfg.MaxRecords = DefaultMaxRecords
	}
	return &Receiver{cfg: cfg, counters: map[string]int{}}
}

func (rx *Receiver) count(k string) {
	rx.mu.Lock()
	rx.counters[k]++
	rx.mu.Unlock()
}

// Counters returns a copy of the receiver's rejection counters.
func (rx *Receiver) Counters() map[string]int {
	rx.mu.Lock()
	defer rx.mu.Unlock()
	out := make(map[string]int, len(rx.counters))
	for k, v := range rx.counters {
		out[k] = v
	}
	return out
}

// ServeHTTP implements http.Handler.
func (rx *Receiver) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if !loopback(r.RemoteAddr) {
		rx.count("rejected_non_loopback")
		http.Error(w, "forbidden", http.StatusForbidden)
		return
	}
	if r.Header.Get("Origin") != "" {
		rx.count("rejected_origin")
		http.Error(w, "forbidden", http.StatusForbidden)
		return
	}
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", http.MethodPost)
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	switch r.URL.Path {
	case "/v1/logs", "/v1/traces", "/v1/metrics":
		rx.export(w, r)
	case SessionEndPath:
		rx.sessionEnd(w, r)
	default:
		http.NotFound(w, r)
	}
}

func loopback(remote string) bool {
	host, _, err := net.SplitHostPort(remote)
	if err != nil {
		host = remote
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

var errTooLarge = errors.New("too large")

// readBody reads at most limit decompressed bytes, enforcing the gzip ratio.
func readBody(r *http.Request, limit int64) ([]byte, error) {
	raw, err := io.ReadAll(io.LimitReader(r.Body, limit+1))
	if err != nil {
		return nil, err
	}
	if int64(len(raw)) > limit {
		return nil, errTooLarge
	}
	switch r.Header.Get("Content-Encoding") {
	case "", "identity":
		return raw, nil
	case "gzip":
		zr, err := gzip.NewReader(bytes.NewReader(raw))
		if err != nil {
			return nil, err
		}
		defer zr.Close()
		maxOut := int64(len(raw)) * MaxGzipRatio
		if maxOut > limit {
			maxOut = limit
		}
		out, err := io.ReadAll(io.LimitReader(zr, maxOut+1))
		if err != nil {
			return nil, err
		}
		if int64(len(out)) > maxOut {
			return nil, errTooLarge
		}
		return out, nil
	default:
		return nil, errors.New("unsupported content-encoding")
	}
}

func (rx *Receiver) export(w http.ResponseWriter, r *http.Request) {
	ct, _, _ := mime.ParseMediaType(r.Header.Get("Content-Type"))
	isJSON := ct == "application/json"
	if !isJSON && ct != "application/x-protobuf" {
		rx.count("rejected_content_type")
		http.Error(w, "unsupported media type", http.StatusUnsupportedMediaType)
		return
	}
	body, err := readBody(r, rx.cfg.MaxBody)
	if errors.Is(err, errTooLarge) {
		rx.count("rejected_too_large")
		http.Error(w, "too large", http.StatusRequestEntityTooLarge)
		return
	}
	if err != nil {
		rx.count("rejected_malformed")
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	unmarshal := func(m proto.Message) error {
		if isJSON {
			return protojson.UnmarshalOptions{DiscardUnknown: true}.Unmarshal(body, m)
		}
		return proto.Unmarshal(body, m)
	}
	var deliver func()
	var n int
	switch r.URL.Path {
	case "/v1/logs":
		m := &logsv1.LogsData{}
		if err = unmarshal(m); err == nil {
			n = countLogs(m)
			deliver = func() { rx.cfg.Sink.Logs(m) }
		}
	case "/v1/traces":
		m := &tracev1.TracesData{}
		if err = unmarshal(m); err == nil {
			n = countSpans(m)
			deliver = func() { rx.cfg.Sink.Traces(m) }
		}
	case "/v1/metrics":
		m := &metricsv1.MetricsData{}
		if err = unmarshal(m); err == nil {
			n = countPoints(m)
			deliver = func() { rx.cfg.Sink.Metrics(m) }
		}
	}
	if err != nil {
		rx.count("rejected_malformed")
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	if n > rx.cfg.MaxRecords {
		rx.count("rejected_too_many_records")
		http.Error(w, "too many records", http.StatusRequestEntityTooLarge)
		return
	}
	if rx.cfg.Sink != nil {
		deliver()
	}
	// An empty Export*ServiceResponse: zero bytes in protobuf, {} in JSON.
	if isJSON {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte("{}"))
		return
	}
	w.Header().Set("Content-Type", "application/x-protobuf")
	w.WriteHeader(http.StatusOK)
}

func countLogs(m *logsv1.LogsData) (n int) {
	for _, rl := range m.GetResourceLogs() {
		for _, sl := range rl.GetScopeLogs() {
			n += len(sl.GetLogRecords())
		}
	}
	return n
}

func countSpans(m *tracev1.TracesData) (n int) {
	for _, rs := range m.GetResourceSpans() {
		for _, ss := range rs.GetScopeSpans() {
			n += len(ss.GetSpans())
		}
	}
	return n
}

func countPoints(m *metricsv1.MetricsData) (n int) {
	for _, rm := range m.GetResourceMetrics() {
		for _, sm := range rm.GetScopeMetrics() {
			n += len(sm.GetMetrics())
		}
	}
	return n
}

// SessionEndRequest is the body of POST /internal/session-end.
type SessionEndRequest struct {
	ClientKind string `json:"client_kind"`
	SessionID  string `json:"session_id"`
}

func (rx *Receiver) sessionEnd(w http.ResponseWriter, r *http.Request) {
	tok := rx.cfg.SessionEndToken
	if tok == "" {
		http.NotFound(w, r)
		return
	}
	got := r.Header.Get(SessionEndHeader)
	if subtle.ConstantTimeCompare([]byte(got), []byte(tok)) != 1 {
		rx.count("rejected_session_end_token")
		http.Error(w, "forbidden", http.StatusForbidden)
		return
	}
	var req SessionEndRequest
	dec := json.NewDecoder(io.LimitReader(r.Body, maxSessionEndBody))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&req); err != nil {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	kind := canon.ClientKind(req.ClientKind)
	if !kind.Valid() || req.SessionID == "" || len(req.SessionID) > maxSessionIDLen {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	if rx.cfg.Sink != nil {
		rx.cfg.Sink.SessionEnd(kind, req.SessionID)
	}
	w.WriteHeader(http.StatusNoContent)
}

// Server binds the receiver on loopback.
type Server struct {
	Port int
	srvs []*http.Server
}

// Listen binds 127.0.0.1:port and, when available, [::1]:port. Port 0
// picks a free port (reported in Server.Port).
func Listen(rx *Receiver, port int) (*Server, error) {
	s := &Server{}
	ln4, err := net.Listen("tcp4", net.JoinHostPort("127.0.0.1", strconv.Itoa(port)))
	if err != nil {
		return nil, err
	}
	s.Port = ln4.Addr().(*net.TCPAddr).Port
	lns := []net.Listener{ln4}
	if ln6, err := net.Listen("tcp6", net.JoinHostPort("::1", strconv.Itoa(s.Port))); err == nil {
		lns = append(lns, ln6)
	}
	for _, ln := range lns {
		srv := &http.Server{Handler: rx, ReadHeaderTimeout: 10 * time.Second, ReadTimeout: 30 * time.Second}
		s.srvs = append(s.srvs, srv)
		go func(ln net.Listener) { _ = srv.Serve(ln) }(ln)
	}
	return s, nil
}

// Shutdown stops all listeners.
func (s *Server) Shutdown(ctx context.Context) error {
	var first error
	for _, srv := range s.srvs {
		if err := srv.Shutdown(ctx); err != nil && first == nil {
			first = err
		}
	}
	return first
}
