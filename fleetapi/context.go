package fleetapi

import (
	"context"
	"time"
)

// CapContextFiles is the positive rollout key Fleet sends while context files are
// offered to the caller's organization: present and true, or absent, never false.
// A build with the context library serves it, and shares or pulls files, only while
// the key is present and true.
const CapContextFiles = "context_files"

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

	// What follows is optional: a provider that doesn't report one leaves it
	// zero, and callers must work without it.

	// Version is Fleet's version of the node.
	Version int
	// OwnerUserID is the author's Fleet user id. Empty for a former member too.
	OwnerUserID string
	// Metadata is the node's metadata as served.
	Metadata map[string]any
	// Deleted says Fleet took the node down; a tombstone has no title or body.
	Deleted bool
	// RemovalReason says why, on a tombstone, when Fleet reports it.
	RemovalReason string
}

// RemovalUnsafeContent is the RemovalReason of a node taken down because it may
// hold a secret or unsafe content.
const RemovalUnsafeContent = "unsafe_content"

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
	Metadata       map[string]any // always non-nil; empty for a note, a shared file's short name for a file
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

// ListRequest pages the nodes the caller can see, oldest change first.
type ListRequest struct {
	// TeamID narrows the list to one of the caller's teams.
	TeamID string
	// Since is the Cursor of the previous page.
	Since string
	Limit int
}

// ListPage is one page. Cursor is empty when there is nothing more.
type ListPage struct {
	Nodes  []NodeContent
	Cursor string
}

// ContextLister is optional on what Context() returns: GET /api/v1/context/pull
// with unit_kind=doc. Without it a caller finds nodes with Search.
type ContextLister interface {
	List(ctx context.Context, req ListRequest) (ListPage, error)
}

// Context is the team/org context graph.
type Context interface {
	Search(ctx context.Context, req SearchRequest) ([]SearchResult, error)
	Pull(ctx context.Context, req PullRequest) ([]NodeContent, error)
	Push(ctx context.Context, req PushRequest) (ContextNode, error)
	Promote(ctx context.Context, req PromoteRequest) (PromoteResult, error)
}
