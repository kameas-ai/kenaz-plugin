package fleetapi

import (
	"context"
	"time"
)

// Classification is a context-graph sharing tier. Personal content never
// leaves the device, so there is no "personal" value here.
type Classification string

const (
	ClassificationTeamShared Classification = "team_shared" // Team+
	ClassificationOrgShared  Classification = "org_shared"  // Enterprise
)

// ContextNode is context-graph node metadata.
type ContextNode struct {
	ID             string
	Title          string
	Kind           string
	Classification Classification
	TeamID         string
	UpdatedAt      time.Time
}

// SearchRequest is POST /api/v1/context/search. With TeamID empty, Fleet
// searches org scope, which requires Enterprise.
type SearchRequest struct {
	Query  string
	TeamID string
	Limit  int
}

// SearchResult is one hit.
type SearchResult struct {
	Node    ContextNode
	Snippet string
	Rank    float64
}

// PullRequest fetches node bodies by id.
type PullRequest struct {
	IDs    []string
	TeamID string
}

// NodeContent is a node with its body.
type NodeContent struct {
	Node ContextNode
	Body string
}

// PushRequest shares a note the user authored to a team (team_shared,
// Team+). It is only called after explicit user confirmation.
//
// It is the complete node in Fleet's ContextNodeInput wire shape, built
// entirely in public code (BuildShareNode): the
// private implementation sends these fields as is and adds nothing.
type PushRequest struct {
	ID             string // client-generated UUIDv7
	Kind           string // guidance | procedure | fact | glossary_term | doc
	Title          string
	Body           string
	Metadata       map[string]any // always non-nil; empty for user shares
	Classification Classification // team_shared only; widening is Promote's job
	TeamID         string
	Version        int    // 1 for a new node
	UnitKind       string // "doc"
	UnitScope      string // "global"
	LoadPolicy     string // "on_demand"; never "always" (admin-only on Fleet)
}

// PromoteRequest is POST /api/v1/context/promote (admin, Enterprise for
// org_shared).
type PromoteRequest struct {
	NodeID string
	To     Classification
	TeamID string
}

// PromoteResult carries the promoted node and Fleet warnings.
type PromoteResult struct {
	Node     ContextNode
	Warnings []string
}

// Context is the team/org context graph.
type Context interface {
	Search(ctx context.Context, req SearchRequest) ([]SearchResult, error)
	Pull(ctx context.Context, req PullRequest) ([]NodeContent, error)
	Push(ctx context.Context, req PushRequest) (ContextNode, error)
	Promote(ctx context.Context, req PromoteRequest) (PromoteResult, error)
}
