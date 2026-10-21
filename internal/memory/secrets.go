package memory

import "regexp"

// credentialPatterns are shapes that are almost always a live credential.
// The check is deliberately small: it catches obvious pastes so they are not
// kept on disk as notes. It is not a scanner, and passing it does not make
// text safe to save.
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

// LooksLikeSecret reports whether s obviously contains a credential.
func LooksLikeSecret(s string) bool {
	for _, re := range credentialPatterns {
		if re.MatchString(s) {
			return true
		}
	}
	return false
}
