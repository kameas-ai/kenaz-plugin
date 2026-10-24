package memory

import "github.com/kameas-ai/kenaz-plugin/pkg/secretscan"

// LooksLikeSecret reports whether s obviously contains a credential. The
// patterns live in fleetapi/secretscan, shared with the official client.
func LooksLikeSecret(s string) bool { return secretscan.LooksLikeSecret(s) }

// LooksLikeCredentialForSync is the stricter scan applied before a note is
// queued for Fleet (Fleet scans again and rejects per record). A hit keeps
// the note on the device as StateRejected/secret_detected; nothing is
// queued.
func LooksLikeCredentialForSync(parts ...string) bool {
	return secretscan.LooksLikeCredentialForSync(parts...)
}
