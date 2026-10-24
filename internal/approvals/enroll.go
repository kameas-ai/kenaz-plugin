package approvals

import (
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"fmt"
	"time"

	"github.com/go-webauthn/webauthn/protocol"
	"github.com/go-webauthn/webauthn/webauthn"
)

// Enrollment modes.
const (
	// EnrollFirst registers the first authenticator for the signed-in
	// identity; it needs a fresh browser identity verification.
	EnrollFirst = "first"
	// EnrollAdd adds another authenticator; it needs an assertion from an
	// already enrolled one.
	EnrollAdd = "add"
	// EnrollRecover replaces lost authenticators; it repeats browser
	// identity verification and revokes every old credential and open
	// request.
	EnrollRecover = "recover"
)

const enrollTTL = 10 * time.Minute

// enrollment is one in-progress setup ceremony. It lives in memory only.
type enrollment struct {
	id       string
	mode     string
	identity Identity
	origin   string
	expires  time.Time
	authURL  string
	// completeToken is the one-time token in the completion URL the
	// identity provider's redirect lands on in the browser that signed in.
	// It is never returned to an API caller.
	completeToken string
	// bindHash is SHA-256 of the ceremony secret held only in the HttpOnly
	// cookie of the browser that completed sign-in (or the existing-key
	// assertion). Registration requires it.
	bindHash  []byte
	verified  bool
	verifyErr string
	asserted  bool
	assertSes *webauthn.SessionData
	regSes    *webauthn.SessionData
	credGen   uint64
	cancel    context.CancelFunc
}

// EnrollState is what the page sees of a ceremony.
type EnrollState struct {
	EnrollID       string `json:"enroll_id"`
	Mode           string `json:"mode"`
	SignInReady    bool   `json:"signin_ready"`
	Verified       bool   `json:"identity_verified"`
	BrowserBound   bool   `json:"browser_bound"`
	NeedsAssertion bool   `json:"needs_assertion"`
	Asserted       bool   `json:"asserted"`
	Error          string `json:"error,omitempty"`
}

func (e *enrollment) state() EnrollState {
	return EnrollState{
		EnrollID: e.id, Mode: e.mode, SignInReady: e.authURL != "", Verified: e.verified, BrowserBound: e.bindHash != nil,
		NeedsAssertion: e.mode == EnrollAdd, Asserted: e.asserted, Error: e.verifyErr,
	}
}

// StartEnrollment begins a setup ceremony on the page at origin. With no
// enrolled authenticator the mode is always EnrollFirst. First enrollment
// and recovery start a browser identity verification (the Fleet sign-in
// the user completes in this browser); its result must name the signed-in
// account, or the ceremony fails with account_mismatch.
func (m *Manager) StartEnrollment(ctx context.Context, mode, origin string) (EnrollState, error) {
	cur, _, err := m.current(ctx)
	if err != nil {
		return EnrollState{}, err
	}
	m.sweep(ctx)
	n := 0
	m.st.view(func(s *state) { n = len(credentialsFor(s, cur)) })
	switch {
	case n == 0:
		mode = EnrollFirst
	case mode == EnrollAdd || mode == EnrollRecover:
	default:
		return EnrollState{}, fmt.Errorf("%w: mode must be add or recover", ErrInvalidRequest)
	}
	if !m.trusted() || (mode != EnrollAdd && m.opt.Verifier == nil) {
		return EnrollState{}, ErrUnavailable
	}
	id, err := newID("enr_")
	if err != nil {
		return EnrollState{}, err
	}
	tok, err := random32()
	if err != nil {
		return EnrollState{}, err
	}
	e := &enrollment{id: id, mode: mode, identity: cur, origin: origin, expires: m.clk.Now().Add(enrollTTL)}
	if mode != EnrollAdd {
		e.completeToken = base64.RawURLEncoding.EncodeToString(tok)
	}
	var vctx context.Context
	if mode != EnrollAdd {
		// Set before the ceremony is published so dropEphemeral (sign-out,
		// account switch) can always cancel its sign-in.
		vctx, e.cancel = context.WithTimeout(context.WithoutCancel(ctx), enrollTTL)
	}
	m.mu.Lock()
	m.enrolls[id] = e
	m.mu.Unlock()
	if vctx != nil {
		go m.verifyIdentity(vctx, e)
	}
	return m.EnrollmentState(id)
}

func (m *Manager) verifyIdentity(ctx context.Context, e *enrollment) {
	defer e.cancel()
	complete := e.origin + PagePrefix + "enroll/complete?c=" + e.completeToken
	got, err := m.opt.Verifier.VerifyIdentity(ctx, func(u string) error {
		m.mu.Lock()
		e.authURL = u
		m.mu.Unlock()
		return nil
	}, complete)
	m.mu.Lock()
	defer m.mu.Unlock()
	switch {
	case err != nil:
		e.verifyErr = "identity_verification_failed"
	case got != e.identity:
		e.verifyErr = "account_mismatch"
	default:
		e.verified = true
	}
}

// completeWait bounds how long the completion redirect waits for identity
// verification (a variable so tests can shorten it).
var completeWait = 90 * time.Second

// SignInURL returns the identity provider URL for ceremony id, for the
// page's redirect endpoint. Knowing it grants nothing: only the browser the
// provider redirects to the completion URL afterwards gets the ceremony
// cookie.
func (m *Manager) SignInURL(id, origin string) (string, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	e, ok := m.enrolls[id]
	if !ok || e.origin != origin || e.authURL == "" {
		return "", ErrNotFound
	}
	return e.authURL, nil
}

// CompleteEnrollment consumes the one-time completion token the identity
// provider's redirect carried into the browser that signed in, once
// identity verification succeeded, and returns the ceremony id and a
// secret for that browser's HttpOnly cookie.
//
// The browser can arrive here before verification finishes: an identity
// verifier may redirect the browser first and then exchange the code and
// read the account, which a slow identity provider can take a while to do.
// The request waits up to completeWait for the verdict, and returns at once
// if the browser goes away (ctx), the ceremony is dropped, or verification
// fails.
func (m *Manager) CompleteEnrollment(ctx context.Context, token, origin string) (string, string, error) {
	deadline := time.Now().Add(completeWait)
	for {
		m.mu.Lock()
		var e *enrollment
		for _, c := range m.enrolls {
			if c.completeToken != "" && subtle.ConstantTimeCompare([]byte(c.completeToken), []byte(token)) == 1 {
				e = c
			}
		}
		if e == nil || e.origin != origin || e.verifyErr != "" {
			m.mu.Unlock()
			return "", "", ErrNotFound
		}
		if e.verified {
			e.completeToken = "" // one use
			sec, err := m.bindLocked(e)
			m.mu.Unlock()
			return e.id, sec, err
		}
		m.mu.Unlock()
		if time.Now().After(deadline) {
			return "", "", ErrChallenge
		}
		select {
		case <-ctx.Done():
			return "", "", ctx.Err()
		case <-time.After(50 * time.Millisecond):
		}
	}
}

// bindLocked mints the ceremony secret for one browser. m.mu is held.
func (m *Manager) bindLocked(e *enrollment) (string, error) {
	b, err := random32()
	if err != nil {
		return "", err
	}
	sec := base64.RawURLEncoding.EncodeToString(b)
	h := sha256.Sum256([]byte(sec))
	e.bindHash = h[:]
	return sec, nil
}

func (e *enrollment) boundTo(secret string) bool {
	h := sha256.Sum256([]byte(secret))
	return e.bindHash != nil && subtle.ConstantTimeCompare(e.bindHash, h[:]) == 1
}

// EnrollmentState reports a ceremony's progress.
func (m *Manager) EnrollmentState(id string) (EnrollState, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	e, ok := m.enrolls[id]
	if !ok {
		return EnrollState{}, ErrNotFound
	}
	return e.state(), nil
}

// enrollment returns ceremony id if it belongs to origin and the current
// identity.
func (m *Manager) enrollment(ctx context.Context, id, origin string) (*enrollment, Identity, error) {
	cur, _, err := m.current(ctx)
	if err != nil {
		return nil, Identity{}, err
	}
	m.mu.Lock()
	e, ok := m.enrolls[id]
	m.mu.Unlock()
	if !ok || e.origin != origin || !m.clk.Now().Before(e.expires) {
		return nil, Identity{}, ErrNotFound
	}
	if e.identity != cur {
		return nil, Identity{}, ErrIdentityMismatch
	}
	return e, cur, nil
}

// BeginEnrollAssertion asks an enrolled authenticator to authorize adding
// another one (EnrollAdd).
func (m *Manager) BeginEnrollAssertion(ctx context.Context, id, origin string) (*protocol.CredentialAssertion, error) {
	e, cur, err := m.enrollment(ctx, id, origin)
	if err != nil {
		return nil, err
	}
	if e.mode != EnrollAdd {
		return nil, fmt.Errorf("%w: no assertion step in this mode", ErrInvalidRequest)
	}
	var user waUser
	if err := m.st.update(func(s *state) error { user, err = userFor(s, cur); return err }); err != nil {
		return nil, err
	}
	wa, err := m.newWebAuthn(origin)
	if err != nil {
		return nil, err
	}
	chal, err := random32()
	if err != nil {
		return nil, err
	}
	a, ses, err := wa.BeginLogin(user, webauthn.WithChallenge(chal), webauthn.WithUserVerification(protocol.VerificationRequired))
	if err != nil {
		return nil, err
	}
	m.mu.Lock()
	e.assertSes, e.asserted = ses, false
	m.mu.Unlock()
	return a, nil
}

// FinishEnrollAssertion verifies the existing-authenticator assertion.
func (m *Manager) FinishEnrollAssertion(ctx context.Context, id, origin string, body []byte) (string, error) {
	e, cur, err := m.enrollment(ctx, id, origin)
	if err != nil {
		return "", err
	}
	m.mu.Lock()
	ses := e.assertSes
	e.assertSes = nil // one use
	m.mu.Unlock()
	if ses == nil {
		return "", ErrChallenge
	}
	parsed, err := protocol.ParseCredentialRequestResponseBytes(body)
	if err != nil {
		return "", fmt.Errorf("%w: malformed", ErrAssertion)
	}
	wa, err := m.newWebAuthn(origin)
	if err != nil {
		return "", err
	}
	_, gen, err := m.current(ctx)
	if err != nil {
		return "", err
	}
	c := &challenge{identity: cur, gen: gen, origin: origin, session: *ses}
	if err := m.verifyAssertion(wa, c, parsed); err != nil {
		return "", err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	e.asserted = true
	// The browser that produced the assertion is the one bound to the
	// ceremony.
	return m.bindLocked(e)
}

// BeginRegistration returns credential creation options requiring user
// verification. It needs a verified identity (first, recover) or an
// existing-credential assertion (add).
func (m *Manager) BeginRegistration(ctx context.Context, id, origin, bind string) (*protocol.CredentialCreation, error) {
	e, cur, err := m.enrollment(ctx, id, origin)
	if err != nil {
		return nil, err
	}
	m.mu.Lock()
	ok := ((e.mode == EnrollAdd && e.asserted) || (e.mode != EnrollAdd && e.verified)) && e.boundTo(bind)
	m.mu.Unlock()
	if !ok {
		return nil, fmt.Errorf("%w: identity not verified", ErrInvalidRequest)
	}
	var user waUser
	var credGen uint64
	if err := m.st.update(func(s *state) error {
		credGen = s.CredGen
		user, err = userFor(s, cur)
		return err
	}); err != nil {
		return nil, err
	}
	wa, err := m.newWebAuthn(origin)
	if err != nil {
		return nil, err
	}
	var excl []protocol.CredentialDescriptor
	if e.mode == EnrollAdd {
		excl = webauthn.Credentials(user.creds).CredentialDescriptors()
	}
	creation, ses, err := wa.BeginRegistration(user,
		webauthn.WithAuthenticatorSelection(protocol.AuthenticatorSelection{
			UserVerification: protocol.VerificationRequired,
			ResidentKey:      protocol.ResidentKeyRequirementDiscouraged,
		}),
		webauthn.WithConveyancePreference(protocol.PreferNoAttestation),
		webauthn.WithExclusions(excl),
	)
	if err != nil {
		return nil, err
	}
	m.mu.Lock()
	e.regSes, e.credGen = ses, credGen
	m.mu.Unlock()
	return creation, nil
}

// FinishRegistration verifies the new credential (UP and UV required) and
// stores it bound to the signed-in identity. It fails with
// ErrEnrollmentRace if any other credential change happened since
// BeginRegistration. Recovery revokes every older credential of the
// identity and cancels its open requests.
func (m *Manager) FinishRegistration(ctx context.Context, id, origin, bind string, body []byte) error {
	e, cur, err := m.enrollment(ctx, id, origin)
	if err != nil {
		return err
	}
	m.mu.Lock()
	if !e.boundTo(bind) {
		m.mu.Unlock()
		return fmt.Errorf("%w: ceremony not bound to this browser", ErrInvalidRequest)
	}
	ses, credGen, mode := e.regSes, e.credGen, e.mode
	e.regSes = nil // one use
	m.mu.Unlock()
	if ses == nil {
		return ErrChallenge
	}
	parsed, err := protocol.ParseCredentialCreationResponseBytes(body)
	if err != nil {
		return fmt.Errorf("%w: malformed", ErrAssertion)
	}
	wa, err := m.newWebAuthn(origin)
	if err != nil {
		return err
	}
	var user waUser
	m.st.view(func(s *state) {
		user = waUser{handle: append([]byte(nil), s.UserHandles[cur.key()]...), creds: credentialsFor(s, cur)}
	})
	cred, err := wa.CreateCredential(user, *ses, parsed)
	if err != nil {
		return fmt.Errorf("%w: %v", ErrAssertion, err)
	}
	if err := requireFlags(parsed.Response.AttestationObject.AuthData.Flags); err != nil {
		return err
	}
	now := m.clk.Now()
	err = m.st.update(func(s *state) error {
		if s.CredGen != credGen {
			return ErrEnrollmentRace
		}
		if s.Bound == nil || *s.Bound != cur {
			return ErrIdentityMismatch
		}
		if mode == EnrollRecover {
			kept := s.Credentials[:0]
			for _, c := range s.Credentials {
				if c.Identity != cur {
					kept = append(kept, c)
				}
			}
			s.Credentials = kept
			cancelOpen(s, now, func(r *record) bool { return r.Identity == cur })
			s.ProfileGen++
		}
		s.Credentials = append(s.Credentials, storedCredential{Identity: cur, Credential: *cred, CreatedAt: now})
		s.CredGen++
		return nil
	})
	if err != nil {
		return err
	}
	if mode == EnrollRecover {
		m.mu.Lock()
		m.challenges = map[string]*challenge{}
		m.receipts = map[string][]byte{}
		m.mu.Unlock()
		// Rebind the new generation to the same identity.
		_, _, _ = m.current(ctx)
	}
	m.mu.Lock()
	delete(m.enrolls, id)
	m.mu.Unlock()
	m.log.Info("approval authenticator enrolled", "mode", mode)
	return nil
}
