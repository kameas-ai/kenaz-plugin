package approvals

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"regexp"
	"strconv"
	"time"

	"github.com/go-webauthn/webauthn/protocol"
	"github.com/go-webauthn/webauthn/webauthn"

	"github.com/kameas-ai/kenaz-plugin/internal/variant"
)

// RPID is the WebAuthn relying-party id. Browsers accept "localhost" as a
// secure context and RP ID; an IP literal is not a valid RP ID, so the page
// is always served as http://localhost:<port>.
const RPID = "localhost"

// challenge binds one fresh 32-byte WebAuthn challenge to a request, its
// payload hash, operation, identity, profile generation and expiry. It is
// used at most once.
type challenge struct {
	requestID string
	nonce     []byte
	sessionID string
	hash      string
	op        string
	identity  Identity
	gen       uint64
	origin    string
	expires   time.Time
	session   webauthn.SessionData
}

// waUser adapts an identity's enrolled credentials to webauthn.User.
type waUser struct {
	handle []byte
	creds  []webauthn.Credential
}

func (u waUser) WebAuthnID() []byte                         { return u.handle }
func (u waUser) WebAuthnName() string                       { return variant.Current.ApprovalsName() }
func (u waUser) WebAuthnDisplayName() string                { return variant.Current.ApprovalsName() }
func (u waUser) WebAuthnCredentials() []webauthn.Credential { return u.creds }

func (m *Manager) newWebAuthn(origin string) (*webauthn.WebAuthn, error) {
	return webauthn.New(&webauthn.Config{
		RPID:                  RPID,
		RPDisplayName:         m.opt.RPDisplayName,
		RPOrigins:             []string{origin},
		AttestationPreference: protocol.PreferNoAttestation,
		AuthenticatorSelection: protocol.AuthenticatorSelection{
			UserVerification: protocol.VerificationRequired,
			ResidentKey:      protocol.ResidentKeyRequirementDiscouraged,
		},
		Timeouts: webauthn.TimeoutsConfig{
			Login:        webauthn.TimeoutConfig{Timeout: DefaultTTL, TimeoutUVD: DefaultTTL},
			Registration: webauthn.TimeoutConfig{Timeout: DefaultTTL, TimeoutUVD: DefaultTTL},
		},
	})
}

// userFor returns the WebAuthn user for id, creating its random user
// handle on first use. Call inside a store update.
func userFor(s *state, id Identity) (waUser, error) {
	h, ok := s.UserHandles[id.key()]
	if !ok {
		h = make([]byte, 32)
		if _, err := rand.Read(h); err != nil {
			return waUser{}, err
		}
		s.UserHandles[id.key()] = h
	}
	return waUser{handle: append([]byte(nil), h...), creds: credentialsFor(s, id)}, nil
}

func random32() ([]byte, error) {
	b := make([]byte, 32)
	_, err := rand.Read(b)
	return b, err
}

// requireFlags checks UP and UV on authenticator data, in addition to the
// library's own checks (defense in depth).
func requireFlags(f protocol.AuthenticatorFlags) error {
	if !f.UserPresent() {
		return fmt.Errorf("%w: user presence missing", ErrAssertion)
	}
	if !f.UserVerified() {
		return fmt.Errorf("%w: user verification missing", ErrAssertion)
	}
	return nil
}

// challengeFor derives the WebAuthn challenge from a fresh nonce and the
// record's id, payload hash, operation, identity and generation, so a
// signed assertion is proof for exactly that record and payload.
func challengeFor(nonce []byte, r *record) []byte {
	h := sha256.New()
	h.Write([]byte("kenaz-approval-v1\x00"))
	for _, part := range []string{string(nonce), r.ID, r.PayloadSHA256, r.Operation, r.Identity.key(), strconv.FormatUint(r.ProfileGen, 10)} {
		h.Write([]byte(strconv.Itoa(len(part))))
		h.Write([]byte{0})
		h.Write([]byte(part))
	}
	return h.Sum(nil)
}

// maxSessionsPerRequest bounds concurrent challenges for one request (one
// per page session).
const maxSessionsPerRequest = 1024

func challengeKey(requestID, session string) string { return requestID + "\x00" + session }

// BeginApproval issues the assertion options for requestID, bound to a
// fresh challenge for this page session. origin is the validated page
// origin; session is the page's CSRF session token.
func (m *Manager) BeginApproval(ctx context.Context, requestID, origin, session string) (*protocol.CredentialAssertion, error) {
	if !m.trusted() {
		return nil, ErrUnavailable
	}
	cur, gen, err := m.current(ctx)
	if err != nil {
		return nil, err
	}
	m.sweep(ctx)
	wa, err := m.newWebAuthn(origin)
	if err != nil {
		return nil, err
	}
	nonce, err := random32()
	if err != nil {
		return nil, err
	}
	var rec *record
	var user waUser
	err = m.st.update(func(s *state) error {
		r, ok := s.Records[requestID]
		if !ok {
			return ErrNotFound
		}
		if r.Status != StatusPending {
			return fmt.Errorf("%w: %s", ErrConflict, r.Status)
		}
		if r.Identity != cur || r.ProfileGen != gen {
			return ErrIdentityMismatch
		}
		rec = r.clone()
		user, err = userFor(s, cur)
		return err
	})
	if err != nil {
		return nil, err
	}
	if len(user.creds) == 0 {
		if m.opt.Verifier == nil {
			return nil, ErrUnavailable
		}
		return nil, ErrEnrollmentRequired
	}
	assertion, ses, err := wa.BeginLogin(user,
		webauthn.WithChallenge(challengeFor(nonce, rec)),
		webauthn.WithUserVerification(protocol.VerificationRequired),
	)
	if err != nil {
		return nil, err
	}
	exp := m.clk.Now().Add(m.opt.TTL)
	if rec.ExpiresAt.Before(exp) {
		exp = rec.ExpiresAt
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	// Never evict another session's challenge: a page session's pending
	// approval survives any number of begin calls from other sessions. A
	// flood can only refuse new sessions (a denial of service, never a
	// write).
	if _, mine := m.challenges[challengeKey(requestID, session)]; !mine {
		n := 0
		for _, c := range m.challenges {
			if c.requestID == requestID {
				n++
			}
		}
		if n >= maxSessionsPerRequest {
			return nil, fmt.Errorf("%w: too many approval attempts for this request", ErrConflict)
		}
	}
	m.challenges[challengeKey(requestID, session)] = &challenge{
		requestID: requestID, nonce: nonce, sessionID: session, hash: rec.PayloadSHA256, op: rec.Operation, identity: cur, gen: gen,
		origin: origin, expires: exp, session: *ses,
	}
	return assertion, nil
}

func (m *Manager) takeChallenge(requestID, session string) *challenge {
	m.mu.Lock()
	defer m.mu.Unlock()
	k := challengeKey(requestID, session)
	c := m.challenges[k]
	delete(m.challenges, k)
	return c
}

// dropChallenges forgets every challenge for requestID.
func (m *Manager) dropChallenges(requestID string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	for k, c := range m.challenges {
		if c.requestID == requestID {
			delete(m.challenges, k)
		}
	}
}

// FinishApproval verifies the browser's assertion for requestID and, if it
// passes every check, approves and executes the stored payload. The
// challenge is consumed whether or not verification succeeds, so an
// assertion can never be replayed.
func (m *Manager) FinishApproval(ctx context.Context, requestID, origin, session string, body []byte) (Status, error) {
	c := m.takeChallenge(requestID, session)
	if c == nil {
		return "", ErrChallenge
	}
	now := m.clk.Now()
	if c.origin != origin || !now.Before(c.expires) {
		return "", ErrChallenge
	}
	cur, gen, err := m.current(ctx)
	if err != nil {
		return "", err
	}
	if cur != c.identity || gen != c.gen {
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
	if err := m.verifyAssertion(wa, c, parsed); err != nil {
		m.log.Warn("approval assertion rejected", "request", requestID)
		return "", err
	}
	receipt, err := random32()
	if err != nil {
		return "", err
	}
	rh := sha256.Sum256(receipt)
	now = m.clk.Now()
	_, err = m.st.transition(requestID, []Status{StatusPending}, StatusApproved, now, func(r *record) error {
		switch {
		case r.Identity != cur || r.ProfileGen != gen:
			return ErrIdentityMismatch
		case r.PayloadSHA256 != c.hash || payloadHash(r.Payload) != c.hash || r.Operation != c.op:
			return fmt.Errorf("%w: payload changed", ErrAssertion)
		case !now.Before(r.ExpiresAt):
			return ErrExpired
		}
		r.ReceiptSHA256 = hex.EncodeToString(rh[:])
		r.Proof = &proof{Nonce: append([]byte(nil), c.nonce...), Origin: origin, Assertion: append([]byte(nil), body...)}
		return nil
	})
	if err == nil {
		m.dropChallenges(requestID)
	}
	if err != nil {
		return "", err
	}
	m.mu.Lock()
	m.receipts[requestID] = receipt
	m.mu.Unlock()
	return m.execute(ctx, requestID, receipt), nil
}

// verifyAssertion runs the library verification (challenge, origin, RP ID,
// signature, credential ownership, UV) plus explicit UP/UV and clone checks,
// and records the new signature counter.
func (m *Manager) verifyAssertion(wa *webauthn.WebAuthn, c *challenge, parsed *protocol.ParsedCredentialAssertionData) error {
	var user waUser
	var uerr error
	m.st.view(func(s *state) {
		h, ok := s.UserHandles[c.identity.key()]
		if !ok {
			uerr = ErrAssertion
			return
		}
		user = waUser{handle: append([]byte(nil), h...), creds: credentialsFor(s, c.identity)}
	})
	if uerr != nil {
		return uerr
	}
	cred, err := wa.ValidateLogin(user, c.session, parsed)
	if err != nil {
		return fmt.Errorf("%w: %v", ErrAssertion, err)
	}
	if err := requireFlags(parsed.Response.AuthenticatorData.Flags); err != nil {
		return err
	}
	if cred.Authenticator.CloneWarning {
		return fmt.Errorf("%w: signature counter did not advance", ErrAssertion)
	}
	return m.st.update(func(s *state) error {
		for i := range s.Credentials {
			sc := &s.Credentials[i]
			if sc.Identity == c.identity && equalBytes(sc.Credential.ID, cred.ID) {
				sc.Credential.Authenticator.SignCount = cred.Authenticator.SignCount
				sc.Credential.Flags = cred.Flags
				return nil
			}
		}
		return fmt.Errorf("%w: credential revoked", ErrAssertion)
	})
}

// Deny cancels a pending request from the page. Nothing executes.
func (m *Manager) Deny(ctx context.Context, requestID, session string) error {
	cur, _, err := m.current(ctx)
	if err != nil {
		return err
	}
	m.takeChallenge(requestID, session)
	_, err = m.st.transition(requestID, []Status{StatusPending}, StatusDenied, m.clk.Now(), func(r *record) error {
		if r.Identity != cur {
			return ErrIdentityMismatch
		}
		return nil
	})
	return err
}

var pageOriginRe = regexp.MustCompile(`^http://localhost:[0-9]{1,5}$`)

// verifyProof re-verifies a record's stored approval assertion against the
// identity's enrolled (authenticated) credentials, with the challenge
// recomputed from the record itself. It runs before any transition into
// executing, so a record that was not approved by a real assertion over
// exactly this payload can never execute.
func (m *Manager) verifyProof(r *record) error {
	pf := r.Proof
	if pf == nil || len(pf.Nonce) != 32 || !pageOriginRe.MatchString(pf.Origin) {
		return fmt.Errorf("%w: no approval proof", ErrAssertion)
	}
	if len(r.Payload) == 0 || payloadHash(r.Payload) != r.PayloadSHA256 {
		return fmt.Errorf("%w: payload changed", ErrAssertion)
	}
	parsed, err := protocol.ParseCredentialRequestResponseBytes(pf.Assertion)
	if err != nil {
		return fmt.Errorf("%w: malformed proof", ErrAssertion)
	}
	var user waUser
	ok := false
	m.st.view(func(s *state) {
		h, found := s.UserHandles[r.Identity.key()]
		user = waUser{handle: append([]byte(nil), h...), creds: credentialsFor(s, r.Identity)}
		ok = found && len(user.creds) > 0
	})
	if !ok {
		return fmt.Errorf("%w: no enrolled credential", ErrAssertion)
	}
	wa, err := m.newWebAuthn(pf.Origin)
	if err != nil {
		return err
	}
	var allowed [][]byte
	for _, c := range user.creds {
		allowed = append(allowed, c.ID)
	}
	ses := webauthn.SessionData{
		Challenge:            base64.RawURLEncoding.EncodeToString(challengeFor(pf.Nonce, r)),
		RelyingPartyID:       RPID,
		UserID:               user.handle,
		AllowedCredentialIDs: allowed,
		UserVerification:     protocol.VerificationRequired,
	}
	if _, err := wa.ValidateLogin(user, ses, parsed); err != nil {
		return fmt.Errorf("%w: proof: %v", ErrAssertion, err)
	}
	return requireFlags(parsed.Response.AuthenticatorData.Flags)
}

func sameProof(a, b *proof) bool {
	return a != nil && b != nil && bytes.Equal(a.Nonce, b.Nonce) && a.Origin == b.Origin && bytes.Equal(a.Assertion, b.Assertion)
}
