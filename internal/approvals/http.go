package approvals

import (
	"bytes"
	"context"
	"crypto/subtle"
	"encoding/base64"
	"encoding/json"
	"errors"
	"html/template"
	"io"
	"net"
	"net/http"
	"strings"
	"time"

	"github.com/kameas-ai/kenaz-plugin/internal/approvals/web"
	"github.com/kameas-ai/kenaz-plugin/internal/variant"
)

// PagePrefix is where the approval page is served on the daemon's
// loopback listener.
const PagePrefix = "/approvals/"

// Control API paths (behind the daemon's nonce bearer). There is no
// approve, enroll or receipt endpoint here, by design.
const (
	ControlPrepare = "/control/approvals/prepare"
	ControlList    = "/control/approvals"
	ControlStatus  = "/control/approvals/" // + request id
)

var csrfCookie = variant.Current.ApprovalsCSRFCookie

const (
	csrfHeader = "X-Kenaz-CSRF"
	csrfTTL    = 30 * time.Minute
	maxBody    = 64 << 10
)

// Mount registers the page routes with page (no bearer: the browser has
// none; every state change is guarded by Host/Origin/CSRF checks and,
// for approval, a verified WebAuthn assertion) and the control routes with
// control (the daemon's nonce-authenticated mux).
func (m *Manager) Mount(page func(pattern string, h http.Handler), control func(pattern string, h http.HandlerFunc)) {
	pg := func(p string, f func(http.ResponseWriter, *http.Request, string)) { page(p, m.guard(f, false, false)) }
	api := func(p string, f func(http.ResponseWriter, *http.Request, string)) { page(p, m.guard(f, true, false)) }
	post := func(p string, f func(http.ResponseWriter, *http.Request, string)) { page(p, m.guard(f, true, true)) }

	pg("GET "+PagePrefix+"{$}", m.servePage)
	pg("GET "+PagePrefix+"r/{id}", m.servePage)
	pg("GET "+PagePrefix+"enroll", m.servePage)
	pg("GET "+PagePrefix+"enroll/signin/{eid}", m.serveSignIn)
	// The identity provider's final redirect is a cross-site top-level
	// navigation; its one-time token is the credential.
	page("GET "+PagePrefix+"enroll/complete", m.guardNav(m.serveComplete))
	pg("GET "+PagePrefix+"static/{file}", m.serveStatic)

	api("GET "+PagePrefix+"api/requests/{id}", m.apiView)
	post("POST "+PagePrefix+"api/requests/{id}/begin", m.apiBegin)
	post("POST "+PagePrefix+"api/requests/{id}/finish", m.apiFinish)
	post("POST "+PagePrefix+"api/requests/{id}/deny", m.apiDeny)

	api("GET "+PagePrefix+"api/enroll", m.apiEnrollInfo)
	post("POST "+PagePrefix+"api/enroll", m.apiEnrollStart)
	api("GET "+PagePrefix+"api/enroll/{eid}", m.apiEnrollState)
	post("POST "+PagePrefix+"api/enroll/{eid}/assert/begin", m.apiEnrollAssertBegin)
	post("POST "+PagePrefix+"api/enroll/{eid}/assert/finish", m.apiEnrollAssertFinish)
	post("POST "+PagePrefix+"api/enroll/{eid}/register/begin", m.apiRegisterBegin)
	post("POST "+PagePrefix+"api/enroll/{eid}/register/finish", m.apiRegisterFinish)

	control("POST "+ControlPrepare, m.ctlPrepare)
	control("GET "+ControlList, m.ctlList)
	control("GET "+ControlStatus+"{id}", m.ctlStatus)
}

// ---- request guard ----

func localPort(r *http.Request) string {
	if a, ok := r.Context().Value(http.LocalAddrContextKey).(net.Addr); ok {
		if _, p, err := net.SplitHostPort(a.String()); err == nil {
			return p
		}
	}
	return ""
}

func securityHeaders(w http.ResponseWriter) {
	h := w.Header()
	h.Set("Content-Security-Policy", "default-src 'none'; script-src 'self'; style-src 'self'; connect-src 'self'; img-src 'self'; base-uri 'none'; form-action 'none'; frame-ancestors 'none'")
	h.Set("X-Frame-Options", "DENY")
	h.Set("X-Content-Type-Options", "nosniff")
	h.Set("Referrer-Policy", "no-referrer")
	h.Set("Cache-Control", "no-store")
	h.Set("Cross-Origin-Opener-Policy", "same-origin")
	h.Set("Cross-Origin-Resource-Policy", "same-origin")
}

// guard enforces the loopback page's request rules:
//   - Host must be exactly localhost:<listener port> (defeats DNS
//     rebinding); a GET of a page on 127.0.0.1 is redirected to localhost.
//   - A present Origin must be the page origin; a present Sec-Fetch-Site
//     must be same-origin (or none, for a top-level page load).
//   - State-changing calls must be POST with an exact Origin, a JSON body
//     and a CSRF token matching both the cookie and an issued token.
func (m *Manager) guard(f func(http.ResponseWriter, *http.Request, string), api, post bool) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		securityHeaders(w)
		port := localPort(r)
		host := "localhost:" + port
		if port == "" || r.Host != host {
			if !api && r.Method == http.MethodGet && port != "" && r.Host == "127.0.0.1:"+port {
				http.Redirect(w, r, "http://"+host+r.URL.RequestURI(), http.StatusSeeOther)
				return
			}
			jsonErr(w, http.StatusMisdirectedRequest, "bad_host", "open this page at http://"+host)
			return
		}
		origin := "http://" + host
		if o := r.Header.Get("Origin"); o != "" && o != origin {
			jsonErr(w, http.StatusForbidden, "bad_origin", "cross-origin request refused")
			return
		}
		if sfs := r.Header.Get("Sec-Fetch-Site"); sfs != "" && sfs != "same-origin" && (api || sfs != "none") {
			if !api && r.Method == http.MethodGet {
				// A link followed from another site or app (chat, email,
				// a web page) is refused on purpose: tell the person how
				// to open it.
				crossSitePage(w, origin+r.URL.RequestURI())
				return
			}
			jsonErr(w, http.StatusForbidden, "bad_origin", "cross-site request refused")
			return
		}
		if post {
			if r.Header.Get("Origin") != origin {
				jsonErr(w, http.StatusForbidden, "bad_origin", "origin required")
				return
			}
			if ct := r.Header.Get("Content-Type"); !strings.HasPrefix(ct, "application/json") {
				jsonErr(w, http.StatusUnsupportedMediaType, "bad_content_type", "JSON required")
				return
			}
			if !m.checkCSRF(r) {
				jsonErr(w, http.StatusForbidden, "bad_csrf", "reload the page and try again")
				return
			}
		}
		f(w, r, origin)
	})
}

func (m *Manager) issueCSRF() (string, error) {
	b, err := random32()
	if err != nil {
		return "", err
	}
	t := base64.RawURLEncoding.EncodeToString(b)
	m.mu.Lock()
	m.csrf[t] = m.clk.Now().Add(csrfTTL)
	m.mu.Unlock()
	return t, nil
}

func (m *Manager) checkCSRF(r *http.Request) bool {
	h := r.Header.Get(csrfHeader)
	c, err := r.Cookie(csrfCookie)
	if h == "" || err != nil || subtle.ConstantTimeCompare([]byte(h), []byte(c.Value)) != 1 {
		return false
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	exp, ok := m.csrf[h]
	return ok && m.clk.Now().Before(exp)
}

// ---- page and static files ----

var pageTmpl = template.Must(template.ParseFS(web.Files, "index.html"))

func (m *Manager) servePage(w http.ResponseWriter, r *http.Request, _ string) {
	if id := r.PathValue("id"); id != "" && !ValidID(id) {
		http.NotFound(w, r)
		return
	}
	tok, err := m.issueCSRF()
	if err != nil {
		http.Error(w, "unavailable", http.StatusInternalServerError)
		return
	}
	http.SetCookie(w, &http.Cookie{Name: csrfCookie, Value: tok, Path: PagePrefix, HttpOnly: true, SameSite: http.SameSiteStrictMode, MaxAge: int(csrfTTL / time.Second)})
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	var buf bytes.Buffer
	if err := pageTmpl.Execute(&buf, struct{ CSRF string }{tok}); err != nil {
		http.Error(w, "unavailable", http.StatusInternalServerError)
		return
	}
	w.Write(buf.Bytes())
}

func (m *Manager) serveStatic(w http.ResponseWriter, r *http.Request, _ string) {
	name := r.PathValue("file")
	var ct string
	switch name {
	case "app.js":
		ct = "text/javascript; charset=utf-8"
	case "style.css":
		ct = "text/css; charset=utf-8"
	default:
		http.NotFound(w, r)
		return
	}
	b, err := web.Files.ReadFile(name)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Content-Type", ct)
	w.Write(b)
}

// ---- page API ----

// View is the page's rendering of one request. The payload is shown as
// text; the page inserts every field with textContent, never as HTML.
type View struct {
	RequestID     string          `json:"request_id"`
	Operation     string          `json:"operation"`
	Status        Status          `json:"status"`
	Summary       string          `json:"summary,omitempty"`
	Impact        string          `json:"impact,omitempty"`
	Targets       []string        `json:"targets,omitempty"`
	Details       []Detail        `json:"details,omitempty"`
	Env           string          `json:"env"`
	OrgID         string          `json:"org_id,omitempty"`
	PayloadText   string          `json:"payload_text,omitempty"`
	PayloadSHA256 string          `json:"payload_sha256"`
	CreatedAt     time.Time       `json:"created_at"`
	ExpiresAt     time.Time       `json:"expires_at"`
	Enrolled      bool            `json:"enrolled"`
	CanEnroll     bool            `json:"can_enroll"`
	Result        json.RawMessage `json:"result,omitempty"`
}

// ViewRequest returns the page view of requestID for the signed-in user.
func (m *Manager) ViewRequest(ctx context.Context, requestID string) (View, error) {
	cur, _, err := m.current(ctx)
	if err != nil {
		return View{}, err
	}
	m.sweep(ctx)
	var v View
	found := false
	m.st.view(func(s *state) {
		r, ok := s.Records[requestID]
		if !ok || r.Identity != cur {
			return
		}
		found = true
		v = View{
			RequestID: r.ID, Operation: r.Operation, Status: r.Status, Summary: r.Summary, Impact: r.Impact,
			Targets: append([]string(nil), r.TargetIDs...), Details: append([]Detail(nil), r.Details...),
			Env: r.Identity.Env, OrgID: r.Identity.OrgID,
			PayloadSHA256: r.PayloadSHA256, CreatedAt: r.CreatedAt, ExpiresAt: r.ExpiresAt,
			Enrolled: len(credentialsFor(s, cur)) > 0, CanEnroll: m.opt.Verifier != nil,
		}
		if len(r.Result) > 0 {
			v.Result = append(json.RawMessage(nil), r.Result...)
		}
		if len(r.Payload) > 0 {
			var buf bytes.Buffer
			if json.Indent(&buf, r.Payload, "", "  ") == nil {
				v.PayloadText = buf.String()
			} else {
				v.PayloadText = string(r.Payload)
			}
		}
	})
	if !found {
		return View{}, ErrNotFound
	}
	return v, nil
}

func (m *Manager) apiView(w http.ResponseWriter, r *http.Request, _ string) {
	v, err := m.ViewRequest(r.Context(), r.PathValue("id"))
	if err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, v)
}

func (m *Manager) apiBegin(w http.ResponseWriter, r *http.Request, origin string) {
	a, err := m.BeginApproval(r.Context(), r.PathValue("id"), origin, r.Header.Get(csrfHeader))
	if err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, a)
}

func readBody(r *http.Request) ([]byte, error) {
	b, err := io.ReadAll(io.LimitReader(r.Body, maxBody+1))
	if err != nil || len(b) > maxBody {
		return nil, ErrInvalidRequest
	}
	return b, nil
}

func (m *Manager) apiFinish(w http.ResponseWriter, r *http.Request, origin string) {
	b, err := readBody(r)
	if err != nil {
		writeErr(w, err)
		return
	}
	st, err := m.FinishApproval(r.Context(), r.PathValue("id"), origin, r.Header.Get(csrfHeader), b)
	if err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, map[string]Status{"status": st})
}

func (m *Manager) apiDeny(w http.ResponseWriter, r *http.Request, _ string) {
	if err := m.Deny(r.Context(), r.PathValue("id"), r.Header.Get(csrfHeader)); err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, map[string]Status{"status": StatusDenied})
}

func (m *Manager) apiEnrollInfo(w http.ResponseWriter, r *http.Request, _ string) {
	if _, _, err := m.current(r.Context()); err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, map[string]bool{"enrolled": m.Enrolled(r.Context()), "can_enroll": m.opt.Verifier != nil})
}

func (m *Manager) apiEnrollStart(w http.ResponseWriter, r *http.Request, origin string) {
	b, err := readBody(r)
	if err != nil {
		writeErr(w, err)
		return
	}
	var in struct {
		Mode string `json:"mode"`
	}
	if len(b) > 0 && json.Unmarshal(b, &in) != nil {
		writeErr(w, ErrInvalidRequest)
		return
	}
	st, err := m.StartEnrollment(r.Context(), in.Mode, origin)
	if err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, st)
}

func (m *Manager) apiEnrollState(w http.ResponseWriter, r *http.Request, origin string) {
	if _, _, err := m.enrollment(r.Context(), r.PathValue("eid"), origin); err != nil {
		writeErr(w, err)
		return
	}
	st, err := m.EnrollmentState(r.PathValue("eid"))
	if err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, st)
}

func (m *Manager) apiEnrollAssertBegin(w http.ResponseWriter, r *http.Request, origin string) {
	a, err := m.BeginEnrollAssertion(r.Context(), r.PathValue("eid"), origin)
	if err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, a)
}

func (m *Manager) apiEnrollAssertFinish(w http.ResponseWriter, r *http.Request, origin string) {
	b, err := readBody(r)
	sec := ""
	if err == nil {
		sec, err = m.FinishEnrollAssertion(r.Context(), r.PathValue("eid"), origin, b)
	}
	if err != nil {
		writeErr(w, err)
		return
	}
	setEnrollCookie(w, r.PathValue("eid"), sec)
	writeJSON(w, map[string]bool{"asserted": true})
}

func (m *Manager) apiRegisterBegin(w http.ResponseWriter, r *http.Request, origin string) {
	c, err := m.BeginRegistration(r.Context(), r.PathValue("eid"), origin, enrollCookie(r, r.PathValue("eid")))
	if err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, c)
}

func (m *Manager) apiRegisterFinish(w http.ResponseWriter, r *http.Request, origin string) {
	b, err := readBody(r)
	if err == nil {
		err = m.FinishRegistration(r.Context(), r.PathValue("eid"), origin, enrollCookie(r, r.PathValue("eid")), b)
	}
	if err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, map[string]bool{"enrolled": true})
}

// ---- control API (nonce bearer; MCP process and CLI) ----

// WireRequest is the control-API form of Request.
type WireRequest struct {
	Operation      string          `json:"operation"`
	Env            string          `json:"env"`
	OrgID          string          `json:"org_id"`
	UserID         string          `json:"user_id"`
	TargetIDs      []string        `json:"target_ids,omitempty"`
	Payload        json.RawMessage `json:"payload"`
	IdempotencyKey string          `json:"idempotency_key"`
	Summary        string          `json:"summary"`
	Impact         string          `json:"impact,omitempty"`
}

// WireStatus is the control-API status of one request. It never carries a
// payload or a receipt.
type WireStatus struct {
	RequestID  string          `json:"request_id,omitempty"`
	Status     Status          `json:"status"`
	ApproveURL string          `json:"approve_url,omitempty"`
	Result     json.RawMessage `json:"result,omitempty"`
	Code       string          `json:"code,omitempty"`
}

// WireList is the control-API listing.
type WireList struct {
	PageURL  string `json:"page_url"`
	Enrolled bool   `json:"enrolled"`
	Requests []Info `json:"requests"`
}

func (m *Manager) ctlPrepare(w http.ResponseWriter, r *http.Request) {
	var in WireRequest
	dec := json.NewDecoder(io.LimitReader(r.Body, MaxPayload+MaxSummary*2+64<<10))
	if err := dec.Decode(&in); err != nil {
		writeErr(w, ErrInvalidRequest)
		return
	}
	id, st, err := m.Prepare(r.Context(), Request{
		Operation: in.Operation, Env: in.Env, OrgID: in.OrgID, UserID: in.UserID, TargetIDs: in.TargetIDs,
		Payload: in.Payload, IdempotencyKey: in.IdempotencyKey, Summary: in.Summary, Impact: in.Impact,
	})
	if err != nil {
		writeErr(w, err)
		return
	}
	out := WireStatus{RequestID: id, Status: st}
	if id != "" {
		out.ApproveURL, _ = m.ApprovalURL(r.Context(), id)
	}
	writeJSON(w, out)
}

func (m *Manager) ctlStatus(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	st, res, err := m.Status(r.Context(), id)
	if err != nil {
		writeErr(w, err)
		return
	}
	out := WireStatus{RequestID: id, Status: st, Result: res}
	if st == StatusPending {
		out.ApproveURL, _ = m.ApprovalURL(r.Context(), id)
	}
	writeJSON(w, out)
}

func (m *Manager) ctlList(w http.ResponseWriter, r *http.Request) {
	out := WireList{Requests: m.List(r.Context()), Enrolled: m.Enrolled(r.Context())}
	if out.Requests == nil {
		out.Requests = []Info{}
	}
	if m.opt.BaseURL != nil {
		out.PageURL = m.opt.BaseURL() + PagePrefix
	}
	writeJSON(w, out)
}

// ---- helpers ----

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	enc := json.NewEncoder(w)
	enc.Encode(v)
}

// CrossSiteMessage is the refusal page for an approval link opened from
// another site or app.
var CrossSiteMessage = variant.Current.ApprovalsName() + ": this page was not opened.\n\n" +
	"For your safety, " + variant.Current.DisplayName + " approval pages only open when you go to them directly, not from a link on another web page, chat or email.\n\n" +
	"Copy the approval link from your terminal (or run `" + variant.Current.Binary + " approvals`) and paste it into your browser's address bar:\n\n"

func crossSitePage(w http.ResponseWriter, link string) {
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.WriteHeader(http.StatusForbidden)
	io.WriteString(w, CrossSiteMessage+link+"\n")
}

func jsonErr(w http.ResponseWriter, status int, code, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(map[string]string{"code": code, "message": msg})
}

// ErrCode maps an error to its stable wire code.
func ErrCode(err error) (int, string) {
	switch {
	case errors.Is(err, ErrNotFound):
		return http.StatusNotFound, "not_found"
	case errors.Is(err, ErrUnavailable):
		return http.StatusConflict, string(StatusApprovalUnavailable)
	case errors.Is(err, ErrEnrollmentRequired):
		return http.StatusConflict, "enrollment_required"
	case errors.Is(err, ErrEnrollmentRace):
		return http.StatusConflict, "enrollment_race"
	case errors.Is(err, ErrConflict):
		return http.StatusConflict, "conflict"
	case errors.Is(err, ErrIdempotencyConflict):
		return http.StatusConflict, "idempotency_conflict"
	case errors.Is(err, ErrChallenge):
		return http.StatusBadRequest, "stale_challenge"
	case errors.Is(err, ErrAssertion):
		return http.StatusForbidden, "assertion_rejected"
	case errors.Is(err, ErrIdentityMismatch):
		return http.StatusForbidden, "account_mismatch"
	case errors.Is(err, ErrNotSignedIn):
		return http.StatusUnauthorized, "signed_out"
	case errors.Is(err, ErrExpired):
		return http.StatusGone, "expired"
	case errors.Is(err, ErrUnknownOperation):
		return http.StatusBadRequest, "unknown_operation"
	case errors.Is(err, ErrInvalidRequest):
		return http.StatusBadRequest, "invalid_request"
	}
	return http.StatusInternalServerError, "internal"
}

// codeErrors maps wire codes back to errors (RemoteClient).
var codeErrors = map[string]error{
	"not_found": ErrNotFound, string(StatusApprovalUnavailable): ErrUnavailable,
	"enrollment_required": ErrEnrollmentRequired, "conflict": ErrConflict,
	"idempotency_conflict": ErrIdempotencyConflict, "account_mismatch": ErrIdentityMismatch,
	"signed_out": ErrNotSignedIn, "unknown_operation": ErrUnknownOperation, "invalid_request": ErrInvalidRequest,
}

func writeErr(w http.ResponseWriter, err error) {
	status, code := ErrCode(err)
	msg := code
	switch code {
	case "not_found":
		msg = "This request does not exist, has been cleaned up, or belongs to another account."
	case string(StatusApprovalUnavailable):
		msg = "No authenticator is set up and this build cannot set one up, so this request cannot be approved."
	case "enrollment_required":
		msg = "Set up a security key or this device's unlock first."
	case "stale_challenge":
		msg = "This approval attempt is no longer valid. Reload the page and try again."
	case "assertion_rejected":
		msg = "Your authenticator's response could not be verified. Nothing was written."
	case "account_mismatch":
		msg = "This request belongs to a different signed-in account."
	case "signed_out":
		msg = "You are signed out, so pending requests were canceled."
	case "expired":
		msg = "This request expired. Ask your agent to prepare it again."
	case "enrollment_race":
		msg = "Another authenticator was set up at the same time. Start again."
	}
	jsonErr(w, status, code, msg)
}

// ---- enrollment browser binding ----

var enrollCookiePrefix = variant.Current.EnrollCookiePrefix

func setEnrollCookie(w http.ResponseWriter, eid, secret string) {
	http.SetCookie(w, &http.Cookie{Name: enrollCookiePrefix + eid, Value: secret, Path: PagePrefix, HttpOnly: true,
		SameSite: http.SameSiteStrictMode, MaxAge: int(enrollTTL / time.Second)})
}

func enrollCookie(r *http.Request, eid string) string {
	c, err := r.Cookie(enrollCookiePrefix + eid)
	if err != nil {
		return ""
	}
	return c.Value
}

// serveSignIn sends the page's browser to the identity provider for
// ceremony eid.
func (m *Manager) serveSignIn(w http.ResponseWriter, r *http.Request, origin string) {
	u, err := m.SignInURL(r.PathValue("eid"), origin)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	http.Redirect(w, r, u, http.StatusSeeOther)
}

// guardNav is guard for the identity provider's completion redirect: exact
// Host, GET only, and a top-level navigation (never a subresource or
// fetch).
func (m *Manager) guardNav(f func(http.ResponseWriter, *http.Request, string)) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		securityHeaders(w)
		port := localPort(r)
		if port == "" || r.Host != "localhost:"+port {
			jsonErr(w, http.StatusMisdirectedRequest, "bad_host", "bad host")
			return
		}
		if mode := r.Header.Get("Sec-Fetch-Mode"); mode != "" && mode != "navigate" {
			jsonErr(w, http.StatusForbidden, "bad_origin", "navigation required")
			return
		}
		if d := r.Header.Get("Sec-Fetch-Dest"); d != "" && d != "document" {
			jsonErr(w, http.StatusForbidden, "bad_origin", "navigation required")
			return
		}
		f(w, r, "http://localhost:"+port)
	})
}

// serveComplete binds the ceremony to the browser the identity provider
// redirected here after sign-in: it sets the HttpOnly ceremony cookie that
// registration requires, then shows the setup page.
func (m *Manager) serveComplete(w http.ResponseWriter, r *http.Request, origin string) {
	eid, sec, err := m.CompleteEnrollment(r.Context(), r.URL.Query().Get("c"), origin)
	if err != nil {
		http.Error(w, "This sign-in link is not valid any more. Start setup again.", http.StatusBadRequest)
		return
	}
	setEnrollCookie(w, eid, sec)
	http.Redirect(w, r, PagePrefix+"enroll?e="+eid, http.StatusSeeOther)
}
