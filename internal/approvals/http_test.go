package approvals

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strings"
	"testing"

	"github.com/go-webauthn/webauthn/protocol"
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
	jar    map[string]*http.Cookie // other cookies (enrollment ceremony)
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
	h := newHarness(t, true)
	srv, port := pageServer(t, h.m)
	a := newVAuth(t)
	a.origin = "http://localhost:" + port
	br := newBrowser(t, srv, port)

	// Enroll through the page API.
	br.load(PagePrefix + "enroll")
	resp, body := br.do(http.MethodPost, PagePrefix+"api/enroll", []byte(`{"mode":"first"}`), nil)
	if resp.StatusCode != 200 {
		t.Fatalf("enroll start: %d %s", resp.StatusCode, body)
	}
	var es EnrollState
	json.Unmarshal(body, &es)
	if strings.Contains(string(body), "id.example.test") {
		t.Fatalf("enrollment start leaks the sign-in URL: %s", body)
	}
	h.waitVerified(es.EnrollID)
	// Without the ceremony cookie (any API caller) registration is refused.
	if resp, _ := br.do(http.MethodPost, PagePrefix+"api/enroll/"+es.EnrollID+"/register/begin", []byte(`{}`), nil); resp.StatusCode < 400 {
		t.Fatal("registration began without the browser binding")
	}
	// The page's sign-in link redirects to the provider.
	resp, _ = br.do(http.MethodGet, PagePrefix+"enroll/signin/"+es.EnrollID, nil, nil)
	if resp.StatusCode != http.StatusSeeOther || !strings.HasPrefix(resp.Header.Get("Location"), "https://id.example.test/") {
		t.Fatalf("signin redirect: %d %v", resp.StatusCode, resp.Header)
	}
	// The provider's final redirect lands in the browser that signed in.
	resp, _ = br.do(http.MethodGet, PagePrefix+"enroll/complete?c="+completeToken(h.m, es.EnrollID), nil, func(r *http.Request) {
		r.Header.Set("Sec-Fetch-Site", "cross-site")
		r.Header.Set("Sec-Fetch-Mode", "navigate")
		r.Header.Set("Sec-Fetch-Dest", "document")
	})
	if resp.StatusCode != http.StatusSeeOther || br.jar[enrollCookiePrefix+es.EnrollID] == nil || !br.jar[enrollCookiePrefix+es.EnrollID].HttpOnly {
		t.Fatalf("complete: %d %v", resp.StatusCode, br.jar)
	}
	_, body = br.do(http.MethodPost, PagePrefix+"api/enroll/"+es.EnrollID+"/register/begin", []byte(`{}`), nil)
	var creation protocol.CredentialCreation
	if err := json.Unmarshal(body, &creation); err != nil {
		t.Fatalf("creation: %v %s", err, body)
	}
	if creation.Response.AuthenticatorSelection.UserVerification != protocol.VerificationRequired || creation.Response.RelyingParty.ID != RPID {
		t.Fatalf("creation options: %+v", creation.Response)
	}
	resp, body = br.do(http.MethodPost, PagePrefix+"api/enroll/"+es.EnrollID+"/register/finish", a.register(t, &creation), nil)
	if resp.StatusCode != 200 {
		t.Fatalf("register finish: %d %s", resp.StatusCode, body)
	}

	// The tool side prepares over the control API.
	r := req(alice, "k1", "<script>alert(1)</script>")
	wire, _ := json.Marshal(WireRequest{Operation: r.Operation, Env: r.Env, OrgID: r.OrgID, UserID: r.UserID, TargetIDs: r.TargetIDs, Payload: r.Payload, IdempotencyKey: r.IdempotencyKey, Summary: r.Summary})
	resp, body = br.do(http.MethodPost, ControlPrepare, wire, func(q *http.Request) { q.Header.Set("Authorization", "Bearer "+testNonce) })
	var ws WireStatus
	json.Unmarshal(body, &ws)
	if resp.StatusCode != 200 || ws.Status != StatusPending || !strings.HasPrefix(ws.ApproveURL, "http://localhost:"+port+"/approvals/r/apr_") {
		t.Fatalf("prepare: %d %s", resp.StatusCode, body)
	}
	id := ws.RequestID

	// The page renders the payload as data. The HTML template never
	// contains it; the API returns it as a JSON string.
	page := br.load(PagePrefix + "r/" + id)
	if bytes.Contains(page, []byte("alert(1)")) {
		t.Fatal("payload rendered into HTML")
	}
	_, body = br.do(http.MethodGet, PagePrefix+"api/requests/"+id, nil, func(r *http.Request) { r.Header.Set("Sec-Fetch-Site", "same-origin") })
	var v View
	json.Unmarshal(body, &v)
	if !strings.Contains(v.PayloadText, "alert(1)") || v.PayloadSHA256 != payloadHash(r.Payload) || !v.Enrolled {
		t.Fatalf("view: %s", body)
	}

	// Approve.
	_, body = br.do(http.MethodPost, PagePrefix+"api/requests/"+id+"/begin", []byte(`{}`), nil)
	var assertion protocol.CredentialAssertion
	json.Unmarshal(body, &assertion)
	if assertion.Response.UserVerification != protocol.VerificationRequired || len(assertion.Response.Challenge) != 32 {
		t.Fatalf("assertion options: %s", body)
	}
	resp, body = br.do(http.MethodPost, PagePrefix+"api/requests/"+id+"/finish", a.assert(t, &assertion), nil)
	if resp.StatusCode != 200 || !strings.Contains(string(body), `"done"`) {
		t.Fatalf("finish: %d %s", resp.StatusCode, body)
	}
	// Status over the control API: no payload, no receipt.
	_, body = br.do(http.MethodGet, ControlStatus+id, nil, func(q *http.Request) { q.Header.Set("Authorization", "Bearer "+testNonce) })
	if !strings.Contains(string(body), `"done"`) || strings.Contains(string(body), "receipt") || strings.Contains(string(body), "alert") {
		t.Fatalf("status: %s", body)
	}
	_, body = br.do(http.MethodGet, ControlList, nil, func(q *http.Request) { q.Header.Set("Authorization", "Bearer "+testNonce) })
	if strings.Contains(string(body), "alert") || strings.Contains(string(body), "Share a note") || strings.Contains(string(body), "receipt") {
		t.Fatalf("list leaks content: %s", body)
	}
}

func TestPageGuards(t *testing.T) {
	h := newHarness(t, true)
	a := newVAuth(t)
	srv, port := pageServer(t, h.m)
	h.enroll(a)
	a.origin = "http://localhost:" + port
	id := h.prepare(req(alice, "k1", "x"))
	br := newBrowser(t, srv, port)
	br.load(PagePrefix + "r/" + id)
	begin := PagePrefix + "api/requests/" + id + "/begin"
	deny := PagePrefix + "api/requests/" + id + "/deny"

	cases := map[string]func(*http.Request){
		"rebinding host":      func(r *http.Request) { r.Host = "attacker.example:" + port },
		"wrong port host":     func(r *http.Request) { r.Host = "localhost:1" },
		"ip host on api":      func(r *http.Request) { r.Host = "127.0.0.1:" + port },
		"cross origin":        func(r *http.Request) { r.Header.Set("Origin", "http://evil.example") },
		"other local origin":  func(r *http.Request) { r.Header.Set("Origin", "http://localhost:9999") },
		"no origin":           func(r *http.Request) { r.Header.Del("Origin") },
		"cross site fetch":    func(r *http.Request) { r.Header.Set("Sec-Fetch-Site", "cross-site") },
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
func TestControlAPIHasNoApproveOrEnroll(t *testing.T) {
	h := newHarness(t, true)
	a := newVAuth(t)
	srv, port := pageServer(t, h.m)
	h.enroll(a)
	a.origin = "http://localhost:" + port
	id := h.prepare(req(alice, "k1", "x"))
	br := newBrowser(t, srv, port)
	auth := func(q *http.Request) {
		q.Header.Set("Authorization", "Bearer "+testNonce)
		q.Header.Del("Origin")
		q.Header.Del(csrfHeader)
	}
	for _, p := range []string{
		"/control/approvals/" + id + "/approve", "/control/approvals/" + id + "/finish", "/control/approvals/" + id,
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
	// The page's begin/finish endpoints cannot be driven by a bearer-holding
	// client without the browser's origin and CSRF state either.
	resp, _ = br.do(http.MethodPost, PagePrefix+"api/requests/"+id+"/finish", []byte(`{}`), auth)
	if resp.StatusCode < 400 {
		t.Fatal("finish without origin/csrf accepted")
	}
}

func TestRemoteClient(t *testing.T) {
	h := newHarness(t, true)
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

func completeToken(m *Manager, eid string) string {
	m.mu.Lock()
	defer m.mu.Unlock()
	if e := m.enrolls[eid]; e != nil {
		return e.completeToken
	}
	return ""
}
