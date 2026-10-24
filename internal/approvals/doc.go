// Package approvals is the human-authorized write protocol.
//
// A tool that wants to write (publish a skill, share context, save or forget
// memory) never writes directly. It calls Prepare with the exact canonical
// payload it would send. The relay daemon stores that payload in a
// protected pending-write record and returns an opaque request id with
// status "pending". Preparation performs no write.
//
// The user approves on a local page the daemon serves on
// http://localhost:<port>/approvals/. The page renders the stored payload as
// text (never caller-supplied HTML) and asks the browser for a WebAuthn
// assertion with user presence and user verification from an authenticator
// the user enrolled earlier. Only after the relay has verified that assertion
// (challenge, origin, RP ID, signature, credential ownership, UP and UV)
// does the record move pending → approved → executing, by compare-and-swap,
// and the registered Executor run with the stored payload and idempotency
// key.
//
// There is no other way to approve: no MCP tool, CLI command or control
// endpoint approves a request, registers an authenticator or exports a
// receipt. A machine without an enrolled authenticator and without a way to
// enroll one reports approval_unavailable; there is no software fallback.
//
// WebAuthn verification is done by github.com/go-webauthn/webauthn; this
// package implements no cryptography itself.
package approvals
