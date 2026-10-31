package fleetapi

import (
	"context"
	"errors"
	"time"
)

// Auth is sign-in and session state. Tokens never cross this seam into
// public code.
type Auth interface {
	// Login runs interactive OIDC sign-in (PKCE, RFC 8252 loopback
	// redirect) for req.Env and makes it the active session. The
	// implementation calls req.OpenURL with the authorization URL and
	// never prints or logs tokens, emails or claims. Login replaces any
	// existing session.
	Login(ctx context.Context, req LoginRequest) (Session, error)

	// Logout revokes the refresh token where supported, deletes stored
	// tokens and clears the active session. It is idempotent: with no
	// session it returns nil.
	Logout(ctx context.Context) error

	// Session returns the active session without network I/O, or
	// ErrNotSignedIn.
	Session(ctx context.Context) (Session, error)

	// TokenStore controls where the implementation keeps credentials.
	TokenStore() TokenStore
}

// LoginRequest parameterizes Login.
type LoginRequest struct {
	Env Env
	// OpenURL opens the authorization URL in the system browser. If it
	// returns an error, the implementation prints nothing itself. The
	// caller is expected to have shown the URL.
	OpenURL func(url string) error
	// Timeout bounds the wait for the loopback callback. Zero means the
	// implementation default (10 minutes).
	Timeout time.Duration
	// CompleteURL is used by IdentityVerifier only (Login ignores it).
	// After a successful callback, the loopback listener must answer the
	// browser with a 303 redirect to exactly this URL, and must never log,
	// print or return it elsewhere: it carries a one-time token that binds
	// the approval enrollment ceremony to the browser that signed in.
	CompleteURL string
}

// Session describes the active sign-in. It contains no secrets.
type Session struct {
	Env       Env
	UserID    string // Zitadel subject; the kameas.user.id telemetry attribute
	OrgID     string // resourceowner claim; the kameas.org.id telemetry attribute
	ExpiresAt time.Time
	// External reports that tokens come from a TokenSource rather than
	// local storage. This module never sets it up, so it is false here.
	External bool
}

// TokenStore selects the credential backing store.
type TokenStore interface {
	// Clear deletes every stored credential for every env.
	Clear(ctx context.Context) error
}

// FileCredentialStore is optionally implemented by a TokenStore that can
// keep credentials in files on a device with no OS keychain (for example a
// Linux VM without a Secret Service). The kenaz CLI checks for it with a
// type assertion and calls UseFiles once, before any other provider call,
// only when no OS keychain is usable. Implementations that always need a
// keychain simply do not implement it.
type FileCredentialStore interface {
	TokenStore
	// UseFiles keeps credentials as owner-only (0600) files in dir, an
	// absolute path to an owner-only (0700) directory, instead of the OS
	// keychain.
	UseFiles(dir string) error
}

// SessionReloader is optionally implemented by an Auth (callers check with
// a type assertion, or call ReloadSession). Session may answer from state
// the implementation loaded once per process; a long-lived process (the
// relay) calls ReloadSession when another process may have signed in or
// out, so it sees the stored session rather than the one it loaded at
// start.
//
// ReloadSession re-reads the stored session without network I/O. When
// the store holds the same sign-in as the active session, the active
// session is kept as it is (no new token client, no lost refresh state);
// when it holds none, the active session is cleared; when it holds a
// different one, that one becomes active. It never sends anything.
type SessionReloader interface {
	ReloadSession(ctx context.Context) error
}

// ErrReloadUnsupported is returned by ReloadSession for an Auth that
// cannot reload its session in place. A caller then restarts the process
// that holds the stale session instead.
var ErrReloadUnsupported = errors.New("fleetapi: session reload not supported")

// ReloadSession calls a's ReloadSession, or returns ErrReloadUnsupported
// when a does not implement SessionReloader.
func ReloadSession(ctx context.Context, a Auth) error {
	if r, ok := a.(SessionReloader); ok {
		return r.ReloadSession(ctx)
	}
	return ErrReloadUnsupported
}

// SessionReloaderNow is optionally implemented by an Auth that also
// implements SessionReloader. ReloadSession is meant for periodic checks
// and may answer cheaply (from a session marker, or with a back-off error
// after the store could not be read); ReloadSessionNow is the reload a
// caller asks for after another process signed in or out (`kenaz login`
// notifying the relay). It always reads the stored session and ignores any
// back-off. Like ReloadSession, it never sends anything.
type SessionReloaderNow interface {
	ReloadSessionNow(ctx context.Context) error
}

// ReloadSessionNow calls a's ReloadSessionNow when a implements
// SessionReloaderNow, otherwise ReloadSession (and so ErrReloadUnsupported
// when a implements neither).
func ReloadSessionNow(ctx context.Context, a Auth) error {
	if r, ok := a.(SessionReloaderNow); ok {
		return r.ReloadSessionNow(ctx)
	}
	return ReloadSession(ctx, a)
}

// TokenSource supplies short-lived access tokens from outside the
// implementation. Nothing in this module provides one; the type is kept so
// existing implementations of this package keep compiling.
type TokenSource interface {
	AccessToken(ctx context.Context) (token string, expiry time.Time, err error)
}

// IdentityVerifier is optionally implemented by an Auth (callers check
// with a type assertion). It runs a fresh, interactive browser sign-in for
// req.Env with its own PKCE flow and loopback redirect, exactly as Login
// does, but returns only the verified subject: it must not replace,
// refresh or read the active session, and must not store the new tokens.
//
// It must force interactive re-authentication (OIDC prompt=login and
// max_age=0) so an existing identity-provider browser session cannot
// complete it silently, must return the ID token's auth_time, and must
// redirect the browser to req.CompleteURL after the callback. An
// implementation that cannot do all three must not implement this
// interface.
//
// The approval enrollment ceremony uses it as the bootstrap proof that the
// person at the browser is the signed-in member. Nothing an agent supplies is accepted instead. An
// Auth that does not implement it makes approval enrollment unavailable.
type IdentityVerifier interface {
	VerifyIdentity(ctx context.Context, req LoginRequest) (VerifiedIdentity, error)
}

// VerifiedIdentity is the subject a fresh browser sign-in proved. It
// contains no secrets.
type VerifiedIdentity struct {
	Env    Env
	UserID string // Zitadel subject (same as Session.UserID)
	OrgID  string // resourceowner (same as Session.OrgID)
	// AuthTime is the identity provider's auth_time for this sign-in.
	AuthTime time.Time
}
