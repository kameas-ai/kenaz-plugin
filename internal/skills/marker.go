package skills

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"time"
)

// MarkerName is the ownership marker file. The relay only creates, updates
// or deletes skill directories that contain a valid marker.
const MarkerName = ".kameas-managed"

// Source is why a skill is installed.
type Source string

const (
	SourceMandated   Source = "mandated"
	SourceSubscribed Source = "subscribed"
)

// Marker is the content of .kameas-managed. SHA256 covers the
// canonical file map as written, after any stripping.
type Marker struct {
	Schema         int       `json:"schema"`
	CatalogID      string    `json:"catalog_id"`
	Kind           string    `json:"kind"`
	Slug           string    `json:"slug"`
	Version        string    `json:"version"`
	SHA256         string    `json:"sha256"`
	Source         Source    `json:"source"`
	InstalledAt    time.Time `json:"installed_at"`
	TranslatedFrom string    `json:"translated_from,omitempty"`
	Stripped       []string  `json:"stripped,omitempty"`
}

const markerSchema = 1

var errNoMarker = errors.New("skills: no valid marker")

// ReadMarker returns dir's marker, or errNoMarker if dir has none or it is
// not a regular file with a valid body. A symlinked marker is not trusted.
func ReadMarker(dir string) (Marker, error) {
	p := filepath.Join(dir, MarkerName)
	fi, err := os.Lstat(p)
	if err != nil || !fi.Mode().IsRegular() || fi.Size() > 64*1024 {
		return Marker{}, errNoMarker
	}
	b, err := os.ReadFile(p)
	if err != nil {
		return Marker{}, errNoMarker
	}
	var m Marker
	if err := json.Unmarshal(b, &m); err != nil || m.Schema != markerSchema || m.CatalogID == "" {
		return Marker{}, errNoMarker
	}
	return m, nil
}

func writeMarker(dir string, m Marker) error {
	m.Schema = markerSchema
	b, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(dir, MarkerName), append(b, '\n'), 0o644)
}
