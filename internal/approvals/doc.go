// Package approvals is the human-approved write protocol.
//
// A tool that wants to write (publish a skill, share context, change or forget memory)
// never writes directly. It calls Prepare with the exact payload it would send. The relay
// daemon stores that payload in a protected pending-write record and returns an opaque
// request id with status "pending". Preparation performs no write.
//
// The person approves in one of two ways: the Approve button on a local page the daemon
// serves on http://localhost:<port>/approvals/, or `kenaz approve` in a real terminal.
// Both show the stored payload as text. Only then does the record move pending →
// executing, by compare-and-swap, once, within its expiry and for the same account, and
// the registered Executor run with the stored payload and idempotency key.
//
// No MCP tool or control endpoint approves a request. The page's approve route needs what
// a browser's same-origin fetch sends and a token only the page hands out, which stops a
// tool, a cross-site page and a plain HTTP call. It can't stop a program running as this
// user that loads the page and sends the same things: that is the limit of an approval
// with no credential behind it.
package approvals
