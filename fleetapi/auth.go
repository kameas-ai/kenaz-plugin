package fleetapi

import (
	"context"
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

// TokenSource supplies short-lived access tokens from outside the
// implementation. Nothing in this module provides one; the type is kept so
// existing implementations of this package keep compiling.
type TokenSource interface {
	AccessToken(ctx context.Context) (token string, expiry time.Time, err error)
}
