// Package device owns the member-controlled device display name sent as
// the enroll "hostname" field. The raw OS
// hostname is never used unless the member opts in explicitly.
//
// Persistence lives in store.go ($STATE/device.json); the CLI is
// `kenaz device [show|rename <name>|rename --use-computer-name|reset]`.
package device

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/kameas-ai/kenaz-plugin/fleetapi"
)

// DefaultDisplayName returns "<OS> device <4 hex>", where the 4 hex
// characters come from SHA-256(nodeID). The node id itself is never
// revealed.
func DefaultDisplayName(goos, nodeID string) string {
	sum := sha256.Sum256([]byte(nodeID))
	return osLabel(goos) + " device " + hex.EncodeToString(sum[:2])
}

func osLabel(goos string) string {
	switch goos {
	case "darwin":
		return "macOS"
	case "linux":
		return "Linux"
	case "windows":
		return "Windows"
	}
	return "Unknown OS"
}

// ErrEmptyName is returned when a name is empty after sanitizing.
var ErrEmptyName = errors.New("device name is empty")

// SanitizeDisplayName strips control characters, trims spaces and
// truncates to fleetapi.MaxDisplayNameLen runes.
func SanitizeDisplayName(s string) (string, error) {
	s = strings.Map(func(r rune) rune {
		if unicode.IsControl(r) {
			return -1
		}
		return r
	}, s)
	s = strings.TrimSpace(s)
	if s == "" {
		return "", ErrEmptyName
	}
	if utf8.RuneCountInString(s) > fleetapi.MaxDisplayNameLen {
		s = string([]rune(s)[:fleetapi.MaxDisplayNameLen])
		s = strings.TrimSpace(s)
	}
	return s, nil
}
