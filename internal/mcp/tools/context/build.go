// Package contexttools is the team/org context-graph MCP tool group:
// context_search, context_pull, context_share,
// context_promote and team_members.
//
// Request construction lives here as pure functions so that every byte of a
// push or promote body is built in public code. The private
// fleetapi implementation only adds auth and transport.
//
// Nothing local is uploaded except a note the user authored and explicitly
// confirmed through context_share. No tool
// here makes a permission decision for the harness; tier, role
// and org-state refusals only describe what Fleet would refuse.
package contexttools

import (
	"crypto/rand"
	"encoding/binary"
	"encoding/hex"
	"fmt"
	"io"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/kameas-ai/kenaz-plugin/fleetapi"
)

// Input limits.
const (
	MaxQueryLen      = 512
	MaxTitleLen      = 200
	MaxShareBodyLen  = 16 << 10
	DefaultLimit     = 8
	MaxSearchLimit   = 25
	MaxPullIDs       = 10
	MaxSnippetLen    = 500
	MaxSearchResult  = 16 << 10
	MaxPullBodyLen   = 8 << 10
	MaxPullResultLen = 48 << 10
)

// ShareKinds are the knowledge kinds a user may share. All
// ride unit_kind=doc, which Fleet accepts for curated kinds (guidance,
// procedure) and for fact/glossary_term/doc (context_kinds.go lane mapping).
var ShareKinds = []string{"guidance", "procedure", "fact", "glossary_term", "doc"}

func validShareKind(k string) bool {
	for _, s := range ShareKinds {
		if s == k {
			return true
		}
	}
	return false
}

// NodeInput is the complete context node a share pushes, in Fleet's
// ContextNodeInput wire shape. It
// is built entirely here; the fleetapi implementation must send it as is.
type NodeInput struct {
	ID             string         `json:"id"`
	Kind           string         `json:"kind"`
	Title          string         `json:"title"`
	Body           string         `json:"body"`
	Metadata       map[string]any `json:"metadata"`
	Classification string         `json:"classification"`
	TeamID         string         `json:"team_id,omitempty"`
	Version        int            `json:"version"`
	UnitKind       string         `json:"unit_kind"`
	UnitScope      string         `json:"unit_scope"`
	LoadPolicy     string         `json:"load_policy"`
}

// ShareInput is the user-facing share request.
type ShareInput struct {
	Title string
	Body  string
	Kind  string
}

// BuildShareNode validates in and returns the node to push to teamID. It
// never sets load_policy=always (that is admin-only on Fleet and would make
// the note load into every session), and never classifies above
// team_shared; widening to org_shared is context_promote's job.
func BuildShareNode(in ShareInput, teamID string, now time.Time, rnd io.Reader) (NodeInput, error) {
	title := strings.TrimSpace(in.Title)
	switch {
	case title == "":
		return NodeInput{}, fmt.Errorf("title is required")
	case utf8.RuneCountInString(title) > MaxTitleLen:
		return NodeInput{}, fmt.Errorf("title is longer than %d characters", MaxTitleLen)
	case strings.TrimSpace(in.Body) == "":
		return NodeInput{}, fmt.Errorf("body is required")
	case len(in.Body) > MaxShareBodyLen:
		return NodeInput{}, fmt.Errorf("body is larger than %d KiB", MaxShareBodyLen>>10)
	case !utf8.ValidString(title) || !utf8.ValidString(in.Body):
		return NodeInput{}, fmt.Errorf("title and body must be valid UTF-8")
	case !validShareKind(in.Kind):
		return NodeInput{}, fmt.Errorf("kind must be one of %s", strings.Join(ShareKinds, ", "))
	case teamID == "":
		return NodeInput{}, fmt.Errorf("team is required")
	}
	id, err := NewUUIDv7(now, rnd)
	if err != nil {
		return NodeInput{}, err
	}
	return NodeInput{
		ID:             id,
		Kind:           in.Kind,
		Title:          title,
		Body:           in.Body,
		Metadata:       map[string]any{},
		Classification: string(fleetapi.ClassificationTeamShared),
		TeamID:         teamID,
		Version:        1,
		UnitKind:       "doc",
		UnitScope:      "global",
		LoadPolicy:     "on_demand",
	}, nil
}

// ToPushRequest maps a built node onto fleetapi.PushRequest, which carries
// the complete node; the private implementation sends it as is.
func ToPushRequest(n NodeInput) fleetapi.PushRequest {
	return fleetapi.PushRequest{
		ID:             n.ID,
		Kind:           n.Kind,
		Title:          n.Title,
		Body:           n.Body,
		Metadata:       n.Metadata,
		Classification: fleetapi.Classification(n.Classification),
		TeamID:         n.TeamID,
		Version:        n.Version,
		UnitKind:       n.UnitKind,
		UnitScope:      n.UnitScope,
		LoadPolicy:     n.LoadPolicy,
	}
}

// BuildPromote returns the promote request for nodeID. Only team_shared to
// org_shared is supported by Fleet (handlers_context.go).
func BuildPromote(nodeID string) (fleetapi.PromoteRequest, error) {
	nodeID = strings.TrimSpace(nodeID)
	if nodeID == "" {
		return fleetapi.PromoteRequest{}, fmt.Errorf("node_id is required")
	}
	if len(nodeID) > 128 {
		return fleetapi.PromoteRequest{}, fmt.Errorf("node_id is too long")
	}
	return fleetapi.PromoteRequest{NodeID: nodeID, To: fleetapi.ClassificationOrgShared}, nil
}

// Scope is a search scope.
type Scope string

const (
	ScopeTeam Scope = "team"
	ScopeOrg  Scope = "org"
)

// BuildSearch validates and builds a search request. teamID must be set for
// team scope and is dropped for org scope (Fleet treats an empty team_id as
// org scope, Enterprise only).
func BuildSearch(query string, scope Scope, teamID string, limit int) (fleetapi.SearchRequest, error) {
	q := strings.TrimSpace(query)
	switch {
	case q == "":
		return fleetapi.SearchRequest{}, fmt.Errorf("query is required")
	case utf8.RuneCountInString(q) > MaxQueryLen:
		return fleetapi.SearchRequest{}, fmt.Errorf("query is longer than %d characters", MaxQueryLen)
	}
	if limit == 0 {
		limit = DefaultLimit
	}
	if limit < 1 || limit > MaxSearchLimit {
		return fleetapi.SearchRequest{}, fmt.Errorf("limit must be between 1 and %d", MaxSearchLimit)
	}
	switch scope {
	case "", ScopeTeam:
		if teamID == "" {
			return fleetapi.SearchRequest{}, fmt.Errorf("team is required for team scope")
		}
		return fleetapi.SearchRequest{Query: q, TeamID: teamID, Limit: limit}, nil
	case ScopeOrg:
		return fleetapi.SearchRequest{Query: q, Limit: limit}, nil
	}
	return fleetapi.SearchRequest{}, fmt.Errorf("scope must be team or org")
}

// BuildPull validates ids and builds a pull request.
func BuildPull(ids []string, teamID string) (fleetapi.PullRequest, error) {
	if len(ids) == 0 || len(ids) > MaxPullIDs {
		return fleetapi.PullRequest{}, fmt.Errorf("ids must contain 1 to %d node ids", MaxPullIDs)
	}
	seen := map[string]bool{}
	out := make([]string, 0, len(ids))
	for _, id := range ids {
		id = strings.TrimSpace(id)
		if id == "" || len(id) > 128 {
			return fleetapi.PullRequest{}, fmt.Errorf("invalid node id")
		}
		if !seen[id] {
			seen[id] = true
			out = append(out, id)
		}
	}
	return fleetapi.PullRequest{IDs: out, TeamID: teamID}, nil
}

// NewUUIDv7 returns an RFC 9562 version 7 UUID for now, using rnd for the
// random bits.
func NewUUIDv7(now time.Time, rnd io.Reader) (string, error) {
	if rnd == nil {
		rnd = rand.Reader
	}
	var b [16]byte
	if _, err := io.ReadFull(rnd, b[6:]); err != nil {
		return "", fmt.Errorf("uuid: %w", err)
	}
	ms := uint64(now.UnixMilli())
	var ts [8]byte
	binary.BigEndian.PutUint64(ts[:], ms)
	copy(b[0:6], ts[2:8])
	b[6] = 0x70 | (b[6] & 0x0f) // version 7
	b[8] = 0x80 | (b[8] & 0x3f) // variant 10
	h := hex.EncodeToString(b[:])
	return h[0:8] + "-" + h[8:12] + "-" + h[12:16] + "-" + h[16:20] + "-" + h[20:32], nil
}
