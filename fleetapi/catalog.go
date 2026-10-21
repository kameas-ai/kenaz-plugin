package fleetapi

import (
	"context"
	"encoding/json"
)

// CatalogKind values this plugin reads.
const (
	CatalogKindAgentSkill = "agent_skill" // Agent Skills directory file-map
	CatalogKindSkill      = "skill"       // legacy kenaz-harness slashcmd JSON (translated best-effort)
)

// Visibility is a catalog item's sharing scope.
type Visibility string

const (
	VisibilityPrivate   Visibility = "private"
	VisibilityTeam      Visibility = "team"
	VisibilityOrgPublic Visibility = "org_public"
)

// CatalogQuery filters List.
type CatalogQuery struct {
	Kind       string
	Query      string
	Visibility Visibility // "" = any visible to the caller
	Limit      int        // 0 = implementation default
}

// CatalogEntry is catalog metadata.
type CatalogEntry struct {
	ID          string
	Slug        string
	Kind        string
	Version     string
	Description string
	Visibility  Visibility
	Mandated    bool
	Revoked     bool
}

// CatalogItem is an entry plus its payload.
type CatalogItem struct {
	CatalogEntry
	Payload json.RawMessage
}

// PublishRequest is POST /catalog/publish. Payload is built and linted in
// public code. The publisher signature is advisory and left to
// the implementation.
type PublishRequest struct {
	Kind        string
	Slug        string
	Version     string
	Description string
	Visibility  Visibility
	Payload     json.RawMessage
}

// Catalog browses and publishes catalog items.
type Catalog interface {
	List(ctx context.Context, q CatalogQuery) ([]CatalogEntry, error)
	// Get fetches id at version ("" = latest).
	Get(ctx context.Context, id, version string) (CatalogItem, error)
	// Publish is only ever called after explicit user confirmation
	// (requiresUserInteraction).
	Publish(ctx context.Context, req PublishRequest) (CatalogEntry, error)
}
