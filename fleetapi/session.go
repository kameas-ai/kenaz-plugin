package fleetapi

import "context"

// ExpectedSession is the account a caller requires a request to be sent
// as. Callers that queue work for one account (memory sync, telemetry)
// attach it to the request context with WithExpectedSession; the
// implementation compares it with the credentials it actually picked for
// the request, at the moment it picks them, so a sign-in change between
// the caller's own identity check and the send can never put one
// account's data on another account's token.
type ExpectedSession struct {
	Env     Env
	OrgID   string // Session.OrgID
	Subject string // Session.UserID (the token's subject)
}

// Matches reports whether s is exactly the identity of sess.
func (s ExpectedSession) Matches(sess Session) bool {
	return s.Env == sess.Env && s.OrgID == sess.OrgID && s.Subject == sess.UserID && s.Subject != "" && s.OrgID != ""
}

type expectedSessionKey struct{}

// WithExpectedSession returns ctx carrying s.
func WithExpectedSession(ctx context.Context, s ExpectedSession) context.Context {
	return context.WithValue(ctx, expectedSessionKey{}, s)
}

// ExpectedSessionFrom returns the ExpectedSession attached to ctx.
func ExpectedSessionFrom(ctx context.Context) (ExpectedSession, bool) {
	s, ok := ctx.Value(expectedSessionKey{}).(ExpectedSession)
	return s, ok
}

// IdentityMismatch returns the error an implementation answers with when
// ExpectedSession is missing or does not match.
func IdentityMismatch() error {
	return &APIError{Status: 0, Code: CodeIdentityMismatch, Message: "the request was not sent: the signed-in account is not the one it was queued for"}
}
