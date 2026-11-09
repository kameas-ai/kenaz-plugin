package approvals

import (
	"io/fs"
	"os"
	"path/filepath"

	"bytes"
	"context"
	"encoding/json"
	"github.com/kameas-ai/kenaz-plugin/internal/usertext"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strings"
	"testing"
)

const testNonce = "test-nonce"

// pageServer serves m like the daemon does: page routes open, control
// routes behind the nonce bearer.
func pageServer(t *testing.T, m *Manager) (*httptest.Server, string) {
	t.Helper()
	mux := http.NewServeMux()
	m.Mount(func(p string, h http.Handler) { mux.Handle(p, h) }, func(p string, h http.HandlerFunc) {
		mux.HandleFunc(p, func(w http.ResponseWriter, r *http.Request) {
			if r.Header.Get("Authorization") != "Bearer "+testNonce {
				http.Error(w, "unauthorized", http.StatusUnauthorized)
				return
			}
			h(w, r)
		})
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	_, port, _ := net.SplitHostPort(strings.TrimPrefix(srv.URL, "http://"))
	m.opt.BaseURL = func() string { return "http://localhost:" + port }
	return srv, port
}

type browser struct {
	t      *testing.T
	srv    *httptest.Server
	host   string
	origin string
	csrf   string
	cookie *http.Cookie
	jar    map[string]*http.Cookie // other cookies
}

func newBrowser(t *testing.T, srv *httptest.Server, port string) *browser {
	return &browser{t: t, srv: srv, host: "localhost:" + port, origin: "http://localhost:" + port}
}

var csrfMeta = regexp.MustCompile(`name="kenaz-csrf" content="([^"]+)"`)

func (b *browser) do(method, path string, body []byte, mut func(*http.Request)) (*http.Response, []byte) {
	b.t.Helper()
	req, _ := http.NewRequest(method, b.srv.URL+path, bytes.NewReader(body))
	req.Host = b.host
	if method == http.MethodPost {
		req.Header.Set("Origin", b.origin)
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set(csrfHeader, b.csrf)
		req.Header.Set("Sec-Fetch-Site", "same-origin")
		req.Header.Set("Sec-Fetch-Mode", "cors")
	}
	if b.cookie != nil {
		req.AddCookie(b.cookie)
	}
	for _, c := range b.jar {
		req.AddCookie(c)
	}
	if mut != nil {
		mut(req)
	}
	client := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	resp, err := client.Do(req)
	if err != nil {
		b.t.Fatal(err)
	}
	defer resp.Body.Close()
	for _, c := range resp.Cookies() {
		if c.Name != csrfCookie {
			if b.jar == nil {
				b.jar = map[string]*http.Cookie{}
			}
			b.jar[c.Name] = c
		}
	}
	out, _ := io.ReadAll(resp.Body)
	return resp, out
}

// load opens a page, keeping its CSRF token and cookie.
func (b *browser) load(path string) []byte {
	b.t.Helper()
	resp, body := b.do(http.MethodGet, path, nil, func(r *http.Request) { r.Header.Set("Sec-Fetch-Site", "none") })
	if resp.StatusCode != 200 {
		b.t.Fatalf("load %s: %d %s", path, resp.StatusCode, body)
	}
	m := csrfMeta.FindSubmatch(body)
	if m == nil {
		b.t.Fatalf("no csrf token in page")
	}
	b.csrf = string(m[1])
	for _, c := range resp.Cookies() {
		if c.Name == csrfCookie {
			b.cookie = c
		}
	}
	if b.cookie == nil || !b.cookie.HttpOnly || b.cookie.SameSite != http.SameSiteStrictMode {
		b.t.Fatalf("csrf cookie: %+v", b.cookie)
	}
	return body
}

func TestPageFlowOverHTTP(t *testing.T) {
	h := newHarness(t)
	srv, port := pageServer(t, h.m)
	br := newBrowser(t, srv, port)

	// The tool side prepares over the control API.
	r := req(alice, "k1", "<script>alert(1)</script>")
	wire, _ := json.Marshal(WireRequest{Operation: r.Operation, Env: r.Env, OrgID: r.OrgID, UserID: r.UserID, TargetIDs: r.TargetIDs, Payload: r.Payload, IdempotencyKey: r.IdempotencyKey, Summary: r.Summary})
	bearer := func(q *http.Request) { q.Header.Set("Authorization", "Bearer "+testNonce) }
	resp, body := br.do(http.MethodPost, ControlPrepare, wire, bearer)
	var ws WireStatus
	json.Unmarshal(body, &ws)
	if resp.StatusCode != 200 || ws.Status != StatusPending || !strings.HasPrefix(ws.ApproveURL, "http://localhost:"+port+"/") {
		t.Fatalf("prepare: %d %s", resp.StatusCode, body)
	}
	id := ws.RequestID

	// The page renders the payload as data, and says nothing about a device.
	page := br.load(PagePrefix + "r/" + id)
	if bytes.Contains(page, []byte("alert(1)")) {
		t.Fatal("payload rendered into HTML")
	}
	for _, want := range []string{usertext.PageLede, ">" + usertext.PageApprove + "<", ">" + usertext.PageDecline + "<"} {
		if !bytes.Contains(page, []byte(want)) {
			t.Errorf("page lacks %q", want)
		}
	}
	for _, not := range []string{"security key", "unlock", "enrol", "Enrol", "authenticator", "passkey"} {
		if bytes.Contains(page, []byte(not)) {
			t.Errorf("page says %q", not)
		}
	}
	_, body = br.do(http.MethodGet, PagePrefix+"api/requests/"+id, nil, func(r *http.Request) { r.Header.Set("Sec-Fetch-Site", "same-origin") })
	var v View
	json.Unmarshal(body, &v)
	if !strings.Contains(v.PayloadText, "alert(1)") || v.PayloadSHA256 != payloadHash(r.Payload) {
		t.Fatalf("view: %s", body)
	}

	// One press.
	approve := PagePrefix + "api/requests/" + id + "/approve"
	resp, body = br.do(http.MethodPost, approve, []byte(`{}`), nil)
	if resp.StatusCode != 200 || !strings.Contains(string(body), `"done"`) || h.execs.Load() != 1 {
		t.Fatalf("approve: %d %s", resp.StatusCode, body)
	}
	// A second press writes nothing more.
	if _, body = br.do(http.MethodPost, approve, []byte(`{}`), nil); !strings.Contains(string(body), `"done"`) || h.execs.Load() != 1 {
		t.Fatalf("second press: %s execs=%d", body, h.execs.Load())
	}
	// Status over the control API: no payload.
	_, body = br.do(http.MethodGet, ControlStatus+id, nil, bearer)
	if !strings.Contains(string(body), `"done"`) || strings.Contains(string(body), "alert") {
		t.Fatalf("status: %s", body)
	}
	_, body = br.do(http.MethodGet, ControlList, nil, bearer)
	if strings.Contains(string(body), "alert") || strings.Contains(string(body), "Share a note") {
		t.Fatalf("list leaks content: %s", body)
	}
}

// What the approve route stops, each with nothing else wrong: a tool or a plain HTTP call
// that hasn't loaded the page, a web page in the person's browser, and a rebound host.
func TestApproveRouteStops(t *testing.T) {
	h := newHarness(t)
	srv, port := pageServer(t, h.m)
	id := h.prepare(req(alice, "k1", "x"))
	br := newBrowser(t, srv, port)
	br.load(PagePrefix + "r/" + id)
	approve := PagePrefix + "api/requests/" + id + "/approve"
	for name, mut := range map[string]func(*http.Request){
		"curl, nothing added": func(r *http.Request) {
			for k := range r.Header {
				r.Header.Del(k)
			}
		},
		"control bearer, no browser state": func(r *http.Request) {
			for k := range r.Header {
				r.Header.Del(k)
			}
			r.Header.Set("Authorization", "Bearer "+testNonce)
		},
		"token and cookie, no fetch metadata": func(r *http.Request) {
			r.Header.Del("Sec-Fetch-Site")
			r.Header.Del("Sec-Fetch-Mode")
		},
		"all browser headers, no token": func(r *http.Request) { r.Header.Del(csrfHeader) },
		"cross-site page in the browser": func(r *http.Request) {
			r.Header.Set("Origin", "http://evil.example")
			r.Header.Set("Sec-Fetch-Site", "cross-site")
		},
		"same-site other port": func(r *http.Request) {
			r.Header.Set("Origin", "http://localhost:9999")
			r.Header.Set("Sec-Fetch-Site", "same-site")
		},
		"dns rebinding": func(r *http.Request) { r.Host = "attacker.example:" + port },
	} {
		if resp, body := br.do(http.MethodPost, approve, []byte(`{}`), mut); resp.StatusCode < 400 {
			t.Errorf("%s: accepted: %d %s", name, resp.StatusCode, body)
		}
	}
	if h.status(id) != StatusPending || h.execs.Load() != 0 {
		t.Fatalf("%s, %d writes", h.status(id), h.execs.Load())
	}
}

// What it doesn't stop, on purpose, so that no text promises it: a program running as
// this user that loads the page and sends what a browser sends. PageClient is exactly
// that, and is how `kenaz approve` works. Only the terminal check guards the command.
func TestApproveRouteDoesNotStopAForgedBrowser(t *testing.T) {
	h := newHarness(t)
	srv, port := pageServer(t, h.m)
	id := h.prepare(req(alice, "k1", "x"))
	c := &PageClient{PageURL: "http://localhost:" + port + PagePrefix, HTTP: redirectToServer(srv)}
	st, err := c.Approve(context.Background(), id)
	if err != nil || st != StatusDone || h.execs.Load() != 1 {
		t.Fatalf("%s %v %d", st, err, h.execs.Load())
	}
}

// redirectToServer sends localhost:<port> to the test server, which is on 127.0.0.1.
func redirectToServer(srv *httptest.Server) *http.Client {
	return &http.Client{Transport: &http.Transport{DialContext: func(ctx context.Context, network, _ string) (net.Conn, error) {
		return (&net.Dialer{}).DialContext(ctx, network, srv.Listener.Addr().String())
	}}}
}

// PageClient sees what the page shows, and can decline.
func TestPageClientViewAndDeny(t *testing.T) {
	h := newHarness(t)
	srv, port := pageServer(t, h.m)
	id := h.prepare(req(alice, "k1", "the exact body"))
	c := &PageClient{PageURL: "http://localhost:" + port + PagePrefix, HTTP: redirectToServer(srv)}
	v, err := c.View(context.Background(), id)
	if err != nil || v.Status != StatusPending || !strings.Contains(v.PayloadText, "the exact body") {
		t.Fatalf("%+v %v", v, err)
	}
	if err := c.Deny(context.Background(), id); err != nil {
		t.Fatal(err)
	}
	if h.status(id) != StatusDenied || h.execs.Load() != 0 {
		t.Fatal(h.status(id))
	}
}

// Nothing a tool can reach approves: no MCP code names the page client or the route.
func TestNoToolCanApprove(t *testing.T) {
	root := filepath.Join("..", "mcp")
	err := filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() || !strings.HasSuffix(p, ".go") {
			return err
		}
		b, err := os.ReadFile(p)
		if err != nil {
			return err
		}
		for _, bad := range []string{"PageClient", "/approve\"", ".Approve(", "api/requests"} {
			if bytes.Contains(b, []byte(bad)) {
				t.Errorf("%s names %s", p, bad)
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

func TestPageGuards(t *testing.T) {
	h := newHarness(t)
	srv, port := pageServer(t, h.m)
	id := h.prepare(req(alice, "k1", "x"))
	br := newBrowser(t, srv, port)
	br.load(PagePrefix + "r/" + id)
	begin := PagePrefix + "api/requests/" + id + "/approve"
	deny := PagePrefix + "api/requests/" + id + "/deny"

	cases := map[string]func(*http.Request){
		"rebinding host":      func(r *http.Request) { r.Host = "attacker.example:" + port },
		"wrong port host":     func(r *http.Request) { r.Host = "localhost:1" },
		"ip host on api":      func(r *http.Request) { r.Host = "127.0.0.1:" + port },
		"cross origin":        func(r *http.Request) { r.Header.Set("Origin", "http://evil.example") },
		"other local origin":  func(r *http.Request) { r.Header.Set("Origin", "http://localhost:9999") },
		"no origin":           func(r *http.Request) { r.Header.Del("Origin") },
		"cross site fetch":    func(r *http.Request) { r.Header.Set("Sec-Fetch-Site", "cross-site") },
		"no fetch metadata":   func(r *http.Request) { r.Header.Del("Sec-Fetch-Site") },
		"no csrf header":      func(r *http.Request) { r.Header.Del(csrfHeader) },
		"wrong csrf":          func(r *http.Request) { r.Header.Set(csrfHeader, "forged") },
		"no csrf cookie":      func(r *http.Request) { r.Header.Del("Cookie") },
		"form content type":   func(r *http.Request) { r.Header.Set("Content-Type", "application/x-www-form-urlencoded") },
		"text plain (simple)": func(r *http.Request) { r.Header.Set("Content-Type", "text/plain") },
	}
	for name, mut := range cases {
		for _, path := range []string{begin, deny} {
			resp, body := br.do(http.MethodPost, path, []byte(`{}`), mut)
			if resp.StatusCode < 400 {
				t.Errorf("%s %s: accepted: %d %s", name, path, resp.StatusCode, body)
			}
		}
	}
	// A token not issued by this daemon fails even if cookie and header
	// agree (cookies are shared across localhost ports).
	resp, _ := br.do(http.MethodPost, deny, []byte(`{}`), func(r *http.Request) {
		r.Header.Set(csrfHeader, "planted")
		r.Header.Del("Cookie")
		r.AddCookie(&http.Cookie{Name: csrfCookie, Value: "planted"})
	})
	if resp.StatusCode != http.StatusForbidden {
		t.Errorf("planted token: %d", resp.StatusCode)
	}
	if h.status(id) != StatusPending {
		t.Fatal("guarded request changed state")
	}
	// An approval link followed from another site or app is refused, and
	// the page says how to open it.
	resp, body := br.do(http.MethodGet, PagePrefix+"r/"+id, nil, func(r *http.Request) {
		r.Header.Set("Sec-Fetch-Site", "cross-site")
		r.Header.Set("Sec-Fetch-Mode", "navigate")
	})
	if resp.StatusCode != http.StatusForbidden || !strings.Contains(string(body), "address bar") || !strings.Contains(string(body), PagePrefix+"r/"+id) {
		t.Fatalf("cross-site page: %d %s", resp.StatusCode, body)
	}
	// GET of the page on 127.0.0.1 is redirected to localhost.
	resp, _ = br.do(http.MethodGet, PagePrefix+"r/"+id, nil, func(r *http.Request) { r.Host = "127.0.0.1:" + port })
	if resp.StatusCode != http.StatusSeeOther || !strings.HasPrefix(resp.Header.Get("Location"), "http://localhost:"+port+"/") {
		t.Errorf("ip redirect: %d %s", resp.StatusCode, resp.Header.Get("Location"))
	}
	// Security headers.
	resp, _ = br.do(http.MethodGet, PagePrefix+"r/"+id, nil, nil)
	if !strings.Contains(resp.Header.Get("Content-Security-Policy"), "script-src 'self'") || resp.Header.Get("X-Frame-Options") != "DENY" || resp.Header.Get("Cache-Control") != "no-store" {
		t.Errorf("headers: %v", resp.Header)
	}
	// Cross-site reads of the request view are refused.
	resp, _ = br.do(http.MethodGet, PagePrefix+"api/requests/"+id, nil, func(r *http.Request) { r.Header.Set("Sec-Fetch-Site", "cross-site") })
	if resp.StatusCode != http.StatusForbidden {
		t.Errorf("cross-site read: %d", resp.StatusCode)
	}
	// A valid deny works.
	resp, _ = br.do(http.MethodPost, deny, []byte(`{}`), nil)
	if resp.StatusCode != 200 || h.status(id) != StatusDenied || h.execs.Load() != 0 {
		t.Fatalf("deny: %d %s", resp.StatusCode, h.status(id))
	}
}

// Nothing on the control API approves, enrolls or exports anything; the
// control API needs the daemon nonce.
func TestControlAPIHasNoApprove(t *testing.T) {
	h := newHarness(t)
	srv, port := pageServer(t, h.m)
	id := h.prepare(req(alice, "k1", "x"))
	br := newBrowser(t, srv, port)
	auth := func(q *http.Request) {
		q.Header.Set("Authorization", "Bearer "+testNonce)
		q.Header.Del("Origin")
		q.Header.Del(csrfHeader)
	}
	for _, p := range []string{
		"/control/approvals/" + id + "/approve", "/control/approvals/" + id,
		"/control/approvals/approve", "/control/approvals/enroll", "/control/approvals/register", "/control/approvals/receipts",
		"/control/approvals/keys",
	} {
		resp, body := br.do(http.MethodPost, p, []byte(`{"request_id":"`+id+`","user_confirmed":true}`), auth)
		if resp.StatusCode < 400 {
			t.Errorf("POST %s accepted: %d %s", p, resp.StatusCode, body)
		}
	}
	if h.status(id) != StatusPending || h.execs.Load() != 0 {
		t.Fatal("control call changed the request")
	}
	// Without the nonce nothing is served.
	resp, _ := br.do(http.MethodGet, ControlList, nil, nil)
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("control without nonce: %d", resp.StatusCode)
	}
	// The page's approve route can't be driven by a bearer-holding client without the
	// browser's origin and CSRF state either.
	resp, _ = br.do(http.MethodPost, PagePrefix+"api/requests/"+id+"/approve", []byte(`{}`), auth)
	if resp.StatusCode < 400 {
		t.Fatal("approve without origin/csrf accepted")
	}
}

func TestRemoteClient(t *testing.T) {
	h := newHarness(t)
	srv, port := pageServer(t, h.m)
	rc := RemoteClient{Conn: func(context.Context) (Doer, error) { return testDoer{srv.URL}, nil }}
	p, err := Submit(context.Background(), rc, req(alice, "k1", "x"))
	if err != nil || p.Status != StatusPending || !ValidID(p.RequestID) || p.ApproveURL != "http://localhost:"+port+"/approvals/r/"+p.RequestID {
		t.Fatalf("submit: %+v %v", p, err)
	}
	st, _, err := rc.Status(context.Background(), p.RequestID)
	if err != nil || st != StatusPending {
		t.Fatalf("status: %s %v", st, err)
	}
	if _, _, err := rc.Prepare(context.Background(), req(bob, "k1", "x")); err == nil {
		t.Fatal("wrong account accepted")
	}
	// No relay: approval_unavailable, nothing written.
	down := RemoteClient{Conn: func(context.Context) (Doer, error) { return nil, io.EOF }}
	p, err = Submit(context.Background(), down, req(alice, "k2", "x"))
	if err != nil || p.Status != StatusApprovalUnavailable || p.RequestID != "" {
		t.Fatalf("down: %+v %v", p, err)
	}
}

type testDoer struct{ base string }

func (d testDoer) DoJSON(ctx context.Context, method, path string, in, out any) error {
	var body io.Reader
	if in != nil {
		b, _ := json.Marshal(in)
		body = bytes.NewReader(b)
	}
	req, _ := http.NewRequestWithContext(ctx, method, d.base+path, body)
	req.Header.Set("Authorization", "Bearer "+testNonce)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		var e struct{ Code string }
		json.NewDecoder(resp.Body).Decode(&e)
		return &HTTPStatusError{Status: resp.StatusCode, Code: e.Code}
	}
	return json.NewDecoder(resp.Body).Decode(out)
}
