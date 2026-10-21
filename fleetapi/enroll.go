package fleetapi

import (
	"context"
	"errors"
	"fmt"
	"unicode"
	"unicode/utf8"
)

// ClientKind is the enroll-time kind of installed software.
// Which harness produced a given telemetry batch is carried per batch by
// the kameas.client.kind resource attribute, not by the node.
type ClientKind string

// ClientKindRelay is the only kind this plugin enrolls as. One node per
// install, regardless of how many harnesses it serves.
const ClientKindRelay ClientKind = "relay"

// MaxDisplayNameLen bounds EnrollBody.Hostname.
const MaxDisplayNameLen = 64

// EnrollBody is the complete POST /api/v1/enroll body, built in public
// code. The implementation sends it byte-for-byte as marshaled
// from these JSON tags, adding nothing.
type EnrollBody struct {
	NodeID   string `json:"node_id"`
	Platform string `json:"platform"` // darwin | linux | windows
	Version  string `json:"version"`  // kenaz version
	// Hostname carries the member-controlled device display name, by
	// default "<OS> device <4 hex>", never the raw OS hostname unless
	// the member explicitly opted in.
	Hostname   string     `json:"hostname"`
	Arch       string     `json:"arch,omitempty"` // amd64 | arm64
	OSVersion  string     `json:"os_version,omitempty"`
	InVM       bool       `json:"in_vm,omitempty"`
	VMID       string     `json:"vm_id,omitempty"`
	ClientKind ClientKind `json:"client_kind"`
}

// Validate checks the invariants Fleet enforces plus the display-name
// bounds. Public callers must validate before calling Enroll.
func (b EnrollBody) Validate() error {
	var errs []error
	if b.NodeID == "" {
		errs = append(errs, errors.New("node_id required"))
	}
	switch b.Platform {
	case "darwin", "linux", "windows":
	default:
		errs = append(errs, fmt.Errorf("platform %q not in darwin|linux|windows", b.Platform))
	}
	switch b.Arch {
	case "", "amd64", "arm64":
	default:
		errs = append(errs, fmt.Errorf("arch %q not in amd64|arm64", b.Arch))
	}
	if b.Version == "" {
		errs = append(errs, errors.New("version required"))
	}
	if b.ClientKind != ClientKindRelay {
		errs = append(errs, fmt.Errorf("client_kind %q must be %q", b.ClientKind, ClientKindRelay))
	}
	if b.Hostname == "" || utf8.RuneCountInString(b.Hostname) > MaxDisplayNameLen {
		errs = append(errs, fmt.Errorf("hostname (display name) must be 1..%d characters", MaxDisplayNameLen))
	}
	for _, r := range b.Hostname {
		if unicode.IsControl(r) {
			errs = append(errs, errors.New("hostname (display name) contains control characters"))
			break
		}
	}
	return errors.Join(errs...)
}

// EnrollResult is Fleet's enroll response, reduced to what public code
// uses.
type EnrollResult struct {
	NodeID string
	OrgID  string
	Tier   string
}

// Enroller registers the device. Re-enrolling with an identical body is the
// heartbeat (every 6 h). Fleet may answer from cache within its
// rate limit.
type Enroller interface {
	// Enroll posts body unchanged. A removed node yields an *APIError
	// with Code CodeNodeRemoved; an org outside the early-access program
	// yields an *APIError with Code CodeEarlyAccessRequired.
	Enroll(ctx context.Context, body EnrollBody) (EnrollResult, error)
}
