// Package enroll builds the public EnrollBody and runs enroll plus the 6 h
// heartbeat through fleetapi.Enroller.
//
// The body is built entirely here, in public code. The device
// display name replaces the OS hostname by default.
package enroll

import (
	"crypto/rand"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"github.com/kameas-ai/kenaz-plugin/fleetapi"
	"github.com/kameas-ai/kenaz-plugin/internal/device"
)

const nodeIDFile = "node_id"

// crockford is the ULID alphabet.
const crockford = "0123456789ABCDEFGHJKMNPQRSTVWXYZ"

// NewULID returns a ULID string (48-bit ms timestamp + 80 random bits).
func NewULID(now time.Time) (string, error) {
	var b [16]byte
	ms := uint64(now.UnixMilli())
	for i := 5; i >= 0; i-- {
		b[i] = byte(ms)
		ms >>= 8
	}
	if _, err := rand.Read(b[6:]); err != nil {
		return "", err
	}
	// 128 bits → 26 base32 chars, most significant first.
	var out [26]byte
	var acc uint64
	bits := 0
	idx := 0
	// Prepend 2 zero bits so 130 bits divide into 26 groups of 5.
	acc, bits = 0, 2
	for _, v := range b {
		acc = acc<<8 | uint64(v)
		bits += 8
		for bits >= 5 {
			bits -= 5
			out[idx] = crockford[(acc>>uint(bits))&31]
			idx++
		}
	}
	return string(out[:]), nil
}

// LoadOrCreateNodeID returns the per-env node id, creating
// stateDir/node_id (0600) on first use. One node per OS user, machine and
// env.
func LoadOrCreateNodeID(stateDir string, now time.Time) (string, error) {
	p := filepath.Join(stateDir, nodeIDFile)
	b, err := os.ReadFile(p)
	if err == nil {
		id := strings.TrimSpace(string(b))
		if id != "" {
			return id, nil
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return "", err
	}
	id, err := NewULID(now)
	if err != nil {
		return "", err
	}
	f, err := os.OpenFile(p, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if errors.Is(err, os.ErrExist) {
		// Lost a creation race: use the winner's id.
		return LoadOrCreateNodeID(stateDir, now)
	}
	if err != nil {
		return "", err
	}
	defer f.Close()
	if _, err := f.WriteString(id + "\n"); err != nil {
		return "", err
	}
	return id, nil
}

// Host describes the machine. Tests replace it; Hostname is consulted only
// when the member opted in to sending it (device.Settings.UseComputerName).
type Host struct {
	GOOS     string
	GOARCH   string
	Hostname func() (string, error)
}

// LocalHost returns the running machine's Host.
func LocalHost() Host {
	return Host{GOOS: runtime.GOOS, GOARCH: runtime.GOARCH, Hostname: os.Hostname}
}

// BuildBody assembles the complete enroll/heartbeat body. It is the only
// constructor of fleetapi.EnrollBody in the plugin.
func BuildBody(stateDir, nodeID, version string, h Host) (fleetapi.EnrollBody, error) {
	s, err := device.Load(stateDir)
	if err != nil {
		return fleetapi.EnrollBody{}, fmt.Errorf("device settings: %w", err)
	}
	arch := h.GOARCH
	if arch != "amd64" && arch != "arm64" {
		arch = ""
	}
	b := fleetapi.EnrollBody{
		NodeID:     nodeID,
		Platform:   h.GOOS,
		Version:    version,
		Hostname:   device.Resolve(s, h.GOOS, nodeID, h.Hostname),
		Arch:       arch,
		ClientKind: fleetapi.ClientKindRelay,
	}
	return b, b.Validate()
}
