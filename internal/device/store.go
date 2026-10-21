package device

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
)

// Settings is $STATE/device.json. UseComputerName is an explicit member
// opt-in to send the OS hostname; managed configuration never sets it.
type Settings struct {
	DisplayName     string `json:"display_name,omitempty"`
	UseComputerName bool   `json:"use_computer_name,omitempty"`
}

const fileName = "device.json"

// Load reads stateDir/device.json. A missing file is the zero Settings.
func Load(stateDir string) (Settings, error) {
	var s Settings
	b, err := os.ReadFile(filepath.Join(stateDir, fileName))
	if errors.Is(err, os.ErrNotExist) {
		return s, nil
	}
	if err != nil {
		return s, err
	}
	err = json.Unmarshal(b, &s)
	return s, err
}

// Save writes stateDir/device.json atomically with mode 0600.
func Save(stateDir string, s Settings) error {
	b, err := json.MarshalIndent(s, "", "  ")
	if err != nil {
		return err
	}
	tmp := filepath.Join(stateDir, fileName+".tmp")
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, filepath.Join(stateDir, fileName))
}

// Rename sets a member-chosen display name and clears the computer-name
// opt-in.
func Rename(stateDir, name string) (string, error) {
	clean, err := SanitizeDisplayName(name)
	if err != nil {
		return "", err
	}
	return clean, Save(stateDir, Settings{DisplayName: clean})
}

// UseComputerName records the member's explicit opt-in to send the OS
// hostname.
func UseComputerName(stateDir string) error {
	return Save(stateDir, Settings{UseComputerName: true})
}

// Reset returns to the default "<OS> device <4 hex>" name.
func Reset(stateDir string) error { return Save(stateDir, Settings{}) }

// Resolve returns the display name to send: the member's name, the OS
// hostname only under explicit opt-in, otherwise DefaultDisplayName.
// hostname is called only in the opt-in case.
func Resolve(s Settings, goos, nodeID string, hostname func() (string, error)) string {
	if s.UseComputerName && hostname != nil {
		if h, err := hostname(); err == nil {
			if clean, err := SanitizeDisplayName(h); err == nil {
				return clean
			}
		}
	}
	if s.DisplayName != "" {
		if clean, err := SanitizeDisplayName(s.DisplayName); err == nil {
			return clean
		}
	}
	return DefaultDisplayName(goos, nodeID)
}
