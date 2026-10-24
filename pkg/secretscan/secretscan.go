// Package secretscan is the on-device credential detector shared by the
// public plugin and the official Fleet client, so both apply exactly the
// same patterns before anything leaves the device. It is deliberately
// small: it catches obvious pastes. It is not a scanner, and passing it
// does not make text safe to store or send; Fleet scans again and
// rejects per record.
package secretscan

import (
	"regexp"
	"strings"
)

// credentialPatterns are shapes that are almost always a live credential.
var credentialPatterns = []*regexp.Regexp{
	// Cloud access key ids (long-term and temporary).
	regexp.MustCompile(`\b(?:AKIA|ASIA|ABIA|ACCA)[0-9A-Z]{16}\b`),
	// GitHub personal, OAuth, app and refresh tokens.
	regexp.MustCompile(`\bgh[pousr]_[A-Za-z0-9]{30,}`),
	regexp.MustCompile(`\bgithub_pat_[A-Za-z0-9_]{20,}`),
	// PEM private keys of any algorithm.
	regexp.MustCompile(`-----BEGIN (?:[A-Z0-9]+ )*PRIVATE KEY-----`),
	// Chat workspace bot, user, app and refresh tokens.
	regexp.MustCompile(`\bxox[abprs]-[A-Za-z0-9-]{10,}`),
	// Payment platform live secret and restricted keys.
	regexp.MustCompile(`\b(?:sk|rk)_live_[A-Za-z0-9]{10,}`),
	// AI provider API keys: the sk-ant- form and the generic sk- form.
	regexp.MustCompile(`\bsk-ant-[A-Za-z0-9_-]{10,}`),
	regexp.MustCompile(`\bsk-[A-Za-z0-9_-]{20,}`),
	// JSON Web Tokens: three base64url segments, the header starting eyJ.
	regexp.MustCompile(`\beyJ[A-Za-z0-9_-]{5,}\.[A-Za-z0-9_-]{5,}\.[A-Za-z0-9_-]{5,}`),
}

// syncCredentialPatterns extend credentialPatterns for anything about to
// leave the device: assignments of passwords, secrets and tokens, bearer
// headers and credentials in URLs.
var syncCredentialPatterns = []*regexp.Regexp{
	regexp.MustCompile(`(?i)\b(?:password|passwd|pwd|secret|token|api[_-]?key|access[_-]?key)\s*[:=]\s*["']?[^\s"']{6,}`),
	regexp.MustCompile(`(?i)\bbearer\s+[A-Za-z0-9._~+/-]{16,}=*`),
	regexp.MustCompile(`(?i)\b[a-z][a-z0-9+.-]*://[^/\s:@]+:[^/\s@]+@`),
	regexp.MustCompile(`\bAIza[0-9A-Za-z_-]{35}\b`),
	regexp.MustCompile(`(?i)\baws_secret_access_key\b`),
}

// Patterns returns the credential shapes LooksLikeSecret checks.
func Patterns() []*regexp.Regexp { return append([]*regexp.Regexp(nil), credentialPatterns...) }

// SyncPatterns returns the extra shapes LooksLikeCredentialForSync checks
// on top of Patterns.
func SyncPatterns() []*regexp.Regexp { return append([]*regexp.Regexp(nil), syncCredentialPatterns...) }

// LooksLikeSecret reports whether s obviously contains a credential (the
// check applied before a note is kept on disk).
func LooksLikeSecret(s string) bool {
	for _, re := range credentialPatterns {
		if re.MatchString(s) {
			return true
		}
	}
	return false
}

// LooksLikeCredentialForSync is the stricter scan applied before anything
// is queued for Fleet. parts are joined with newlines and scanned as one.
func LooksLikeCredentialForSync(parts ...string) bool {
	s := strings.Join(parts, "\n")
	if LooksLikeSecret(s) {
		return true
	}
	for _, re := range syncCredentialPatterns {
		if re.MatchString(s) {
			return true
		}
	}
	return false
}
