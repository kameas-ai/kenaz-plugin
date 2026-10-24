package contexttools

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"sort"
	"strings"
	"time"
	"unicode/utf8"

	sdk "github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/kameas-ai/kenaz-plugin/fleetapi"
	"github.com/kameas-ai/kenaz-plugin/internal/approvals"
	"github.com/kameas-ai/kenaz-plugin/internal/config"
	"github.com/kameas-ai/kenaz-plugin/internal/mcp"
)

// MetaRequiresUserInteraction makes Claude Code prompt the human on every
// call, in every permission mode, with no "don't ask again". Codex gets the equivalent from the plugin's MCP entry.
const MetaRequiresUserInteraction = "anthropic/requiresUserInteraction"

// Tool names. They are part of the plugin's public surface.
const (
	ToolSearch      = "context_search"
	ToolPull        = "context_pull"
	ToolShare       = "context_share"
	ToolPromote     = "context_promote"
	ToolTeamMembers = "team_members"
)

// WriteTools are the tools that upload or widen sharing and therefore must
// force a human prompt.
var WriteTools = []string{ToolShare, ToolPromote}

// Options are the group's injectable dependencies.
type Options struct {
	// DefaultTeam returns the configured default team ("" if none).
	DefaultTeam func() string
	Now         func() time.Time
	Rand        io.Reader
}

// Group registers the context tools with production defaults.
var Group = NewGroup(Options{})

// NewGroup returns the tool group with opts (zero fields get defaults).
func NewGroup(opts Options) mcp.Group {
	if opts.DefaultTeam == nil {
		opts.DefaultTeam = configuredDefaultTeam
	}
	if opts.Now == nil {
		opts.Now = time.Now
	}
	if opts.Rand == nil {
		opts.Rand = rand.Reader
	}
	return mcp.GroupFunc(func(s *sdk.Server, d mcp.Deps) { register(s, d, opts) })
}

func configuredDefaultTeam() string {
	p, err := config.DefaultPaths()
	if err != nil {
		return ""
	}
	f, err := config.LoadFile(p.ConfigFile())
	if err != nil {
		return ""
	}
	return f.Get(DefaultTeamKey)
}

// ---- inputs and outputs ----

type SearchInput struct {
	Query string `json:"query" jsonschema:"what to look for in your team's shared context (up to 512 characters)"`
	Scope string `json:"scope,omitempty" jsonschema:"team (default) or org; org scope needs the Enterprise plan"`
	Team  string `json:"team,omitempty" jsonschema:"team name or id; defaults to your configured or only team"`
	Limit int    `json:"limit,omitempty" jsonschema:"maximum results, 1 to 25 (default 8)"`
}

type SearchHit struct {
	ID             string  `json:"id"`
	Title          string  `json:"title"`
	Kind           string  `json:"kind"`
	Classification string  `json:"classification"`
	Snippet        string  `json:"snippet"`
	Rank           float64 `json:"rank"`
}

type SearchOutput struct {
	Scope     string      `json:"scope,omitempty"`
	Team      string      `json:"team,omitempty"`
	Results   []SearchHit `json:"results"`
	Truncated bool        `json:"truncated"`
	Error     *ToolError  `json:"error,omitempty"`
}

type PullInput struct {
	IDs  []string `json:"ids" jsonschema:"1 to 10 node ids from context_search"`
	Team string   `json:"team,omitempty" jsonschema:"team name or id; defaults to your configured or only team"`
}

type PulledNode struct {
	ID             string `json:"id"`
	Title          string `json:"title"`
	Kind           string `json:"kind"`
	Classification string `json:"classification"`
	Body           string `json:"body"`
	UpdatedAt      string `json:"updated_at,omitempty"`
	Truncated      bool   `json:"truncated"`
}

type PullOutput struct {
	Nodes     []PulledNode `json:"nodes"`
	Missing   []string     `json:"missing,omitempty"`
	Truncated bool         `json:"truncated"`
	Error     *ToolError   `json:"error,omitempty"`
}

type ShareArgs struct {
	Title string `json:"title" jsonschema:"short title, up to 200 characters"`
	Body  string `json:"body" jsonschema:"the note text the user wrote or approved, up to 16 KiB"`
	Kind  string `json:"kind" jsonschema:"one of guidance, procedure, fact, glossary_term, doc"`
	Team  string `json:"team,omitempty" jsonschema:"team name or id; defaults to your configured or only team"`
	// UserConfirmed is accepted for compatibility and ignored: a model
	// flag is not proof of human confirmation.
	UserConfirmed bool `json:"user_confirmed,omitempty" jsonschema:"ignored; the user approves on the approval page"`
}

// ShareOutput is context_share's result. Nothing is uploaded when the tool
// returns: status is "pending" until the user approves at approve_url.
type ShareOutput struct {
	RequestID      string     `json:"request_id,omitempty"`
	Status         string     `json:"status,omitempty"`
	ApproveURL     string     `json:"approve_url,omitempty"`
	Message        string     `json:"message,omitempty"`
	ID             string     `json:"id,omitempty"`
	Title          string     `json:"title,omitempty"`
	Team           string     `json:"team,omitempty"`
	Classification string     `json:"classification,omitempty"`
	Error          *ToolError `json:"error,omitempty"`
}

type PromoteInput struct {
	NodeID string `json:"node_id" jsonschema:"id of a team-shared node"`
	// UserConfirmed is accepted for compatibility and ignored.
	UserConfirmed bool `json:"user_confirmed,omitempty" jsonschema:"ignored; the user approves on the approval page"`
}

// PromoteOutput is context_promote's result; like ShareOutput it is
// pending until approved.
type PromoteOutput struct {
	RequestID      string     `json:"request_id,omitempty"`
	Status         string     `json:"status,omitempty"`
	ApproveURL     string     `json:"approve_url,omitempty"`
	Message        string     `json:"message,omitempty"`
	ID             string     `json:"id,omitempty"`
	Classification string     `json:"classification,omitempty"`
	Warnings       []string   `json:"warnings,omitempty"`
	Error          *ToolError `json:"error,omitempty"`
}

type TeamMembersInput struct {
	Team string `json:"team,omitempty" jsonschema:"team name or id; defaults to your configured or only team"`
}

type TeamOut struct {
	ID   string `json:"id"`
	Name string `json:"name"`
}

type MemberOut struct {
	DisplayName string `json:"display_name"`
	Role        string `json:"role,omitempty"`
}

type TeamMembersOutput struct {
	Teams   []TeamOut   `json:"teams"`
	Team    string      `json:"team,omitempty"`
	Members []MemberOut `json:"members"`
	Error   *ToolError  `json:"error,omitempty"`
}

// ---- registration ----

func register(s *sdk.Server, d mcp.Deps, o Options) {
	var st StatusSource
	if d.Status != nil {
		st = d.Status
	}
	touch := func() {
		if d.Touch != nil {
			d.Touch()
		}
	}
	readOnly := &sdk.ToolAnnotations{ReadOnlyHint: true, IdempotentHint: true, OpenWorldHint: ptr(true)}
	writes := sdk.Meta{MetaRequiresUserInteraction: true}

	sdk.AddTool(s, &sdk.Tool{
		Name:        ToolSearch,
		Title:       "Search team context",
		Description: "Search the notes your team has shared in Kameas Fleet (guidance, procedures, facts, glossary terms). Read-only: sends only your query to Kameas Fleet. Use scope=org to search organization-wide notes (Enterprise plan).",
		Annotations: withTitle(readOnly, "Search team context"),
	}, func(ctx context.Context, _ *sdk.CallToolRequest, in SearchInput) (*sdk.CallToolResult, SearchOutput, error) {
		touch()
		out := runSearch(ctx, d.Provider, st, o, in)
		return errResult(out.Error), out, nil
	})

	sdk.AddTool(s, &sdk.Tool{
		Name:        ToolPull,
		Title:       "Read team context",
		Description: "Fetch the full text of shared Kameas Fleet context notes by id (ids come from context_search). Read-only.",
		Annotations: withTitle(readOnly, "Read team context"),
	}, func(ctx context.Context, _ *sdk.CallToolRequest, in PullInput) (*sdk.CallToolResult, PullOutput, error) {
		touch()
		out := runPull(ctx, d.Provider, st, o, in)
		return errResult(out.Error), out, nil
	})

	sdk.AddTool(s, &sdk.Tool{
		Name:  ToolShare,
		Title: "Share a note with your team",
		Description: "Prepares sharing the given title and body to Kameas Fleet as a note visible to every member of the chosen team. " +
			"Only share text the user wrote or asked to share; never share file contents, code, transcripts or anything containing secrets. " +
			"Nothing is uploaded by this call: it returns request_id, status \"pending\" and approve_url. Give the user approve_url; they review the exact note there " +
			"and approve with their security key or device unlock.",
		Annotations: &sdk.ToolAnnotations{Title: "Share a note with your team", DestructiveHint: ptr(false), OpenWorldHint: ptr(true)},
		Meta:        writes,
	}, func(ctx context.Context, _ *sdk.CallToolRequest, in ShareArgs) (*sdk.CallToolResult, ShareOutput, error) {
		touch()
		out := runShare(ctx, d.Provider, d.Approvals, st, o, in)
		return errResult(out.Error), out, nil
	})

	sdk.AddTool(s, &sdk.Tool{
		Name:  ToolPromote,
		Title: "Share a team note with the whole organization",
		Description: "Widens an existing team-shared Kameas Fleet note so the WHOLE ORGANIZATION can see it. " +
			"Organization owners and admins only, Enterprise plan. Nothing changes by this call: it returns request_id, status \"pending\" and approve_url, " +
			"where the user approves with their security key or device unlock.",
		Annotations: &sdk.ToolAnnotations{Title: "Share a team note with the whole organization", DestructiveHint: ptr(false), OpenWorldHint: ptr(true)},
		Meta:        writes,
	}, func(ctx context.Context, _ *sdk.CallToolRequest, in PromoteInput) (*sdk.CallToolResult, PromoteOutput, error) {
		touch()
		out := runPromote(ctx, d.Provider, d.Approvals, st, in)
		return errResult(out.Error), out, nil
	})

	sdk.AddTool(s, &sdk.Tool{
		Name:        ToolTeamMembers,
		Title:       "List your teams and members",
		Description: "List the Kameas Fleet teams you belong to and the display names and roles of a team's members. Read-only.",
		Annotations: withTitle(readOnly, "List your teams and members"),
	}, func(ctx context.Context, _ *sdk.CallToolRequest, in TeamMembersInput) (*sdk.CallToolResult, TeamMembersOutput, error) {
		touch()
		out := runTeamMembers(ctx, d.Provider, st, o, in)
		return errResult(out.Error), out, nil
	})
}

// ---- handlers (exported behaviour is tested through MCP) ----

func runSearch(ctx context.Context, p fleetapi.Provider, st StatusSource, o Options, in SearchInput) SearchOutput {
	out := SearchOutput{Results: []SearchHit{}}
	scope := Scope(strings.ToLower(strings.TrimSpace(in.Scope)))
	if scope == "" {
		scope = ScopeTeam
	}
	op := OpSearchTeam
	if scope == ScopeOrg {
		op = OpSearchOrg
	} else if scope != ScopeTeam {
		out.Error = invalid("scope must be team or org")
		return out
	}
	out.Scope = string(scope)
	sess, terr := Check(ctx, p, st, op)
	if terr != nil {
		out.Error = terr
		return out
	}
	teamID := ""
	if scope == ScopeTeam {
		t, terr := ResolveTeam(in.Team, o.DefaultTeam(), sess.Me.Teams)
		if terr != nil {
			out.Error = terr
			return out
		}
		teamID, out.Team = t.ID, t.Name
	}
	req, err := BuildSearch(in.Query, scope, teamID, in.Limit)
	if err != nil {
		out.Error = invalid(err.Error())
		return out
	}
	res, err := p.Context().Search(ctx, req)
	if err != nil {
		out.Error = MapError(err)
		return out
	}
	sort.SliceStable(res, func(i, j int) bool { return res[i].Rank > res[j].Rank })
	size := 0
	for _, r := range res {
		snippet, _ := clip(r.Snippet, MaxSnippetLen)
		h := SearchHit{ID: r.Node.ID, Title: r.Node.Title, Kind: r.Node.Kind, Classification: string(r.Node.Classification), Snippet: snippet, Rank: r.Rank}
		n := len(h.ID) + len(h.Title) + len(h.Kind) + len(h.Classification) + len(h.Snippet) + 64
		if size+n > MaxSearchResult {
			out.Truncated = true
			break
		}
		size += n
		out.Results = append(out.Results, h)
	}
	return out
}

func runPull(ctx context.Context, p fleetapi.Provider, st StatusSource, o Options, in PullInput) PullOutput {
	out := PullOutput{Nodes: []PulledNode{}}
	sess, terr := Check(ctx, p, st, OpPull)
	if terr != nil {
		out.Error = terr
		return out
	}
	teamID := ""
	if strings.TrimSpace(in.Team) != "" || len(sess.Me.Teams) > 0 {
		t, terr := ResolveTeam(in.Team, o.DefaultTeam(), sess.Me.Teams)
		if terr != nil {
			out.Error = terr
			return out
		}
		teamID = t.ID
	}
	req, err := BuildPull(in.IDs, teamID)
	if err != nil {
		out.Error = invalid(err.Error())
		return out
	}
	nodes, err := p.Context().Pull(ctx, req)
	if err != nil {
		out.Error = MapError(err)
		return out
	}
	byID := map[string]fleetapi.NodeContent{}
	for _, n := range nodes {
		byID[n.Node.ID] = n
	}
	size := 0
	for _, id := range req.IDs {
		n, ok := byID[id]
		if !ok {
			out.Missing = append(out.Missing, id)
			continue
		}
		body, cut := clip(n.Body, MaxPullBodyLen)
		pn := PulledNode{ID: n.Node.ID, Title: n.Node.Title, Kind: n.Node.Kind, Classification: string(n.Node.Classification), Body: body, Truncated: cut}
		if !n.Node.UpdatedAt.IsZero() {
			pn.UpdatedAt = n.Node.UpdatedAt.UTC().Format(time.RFC3339)
		}
		sz := len(pn.ID) + len(pn.Title) + len(pn.Body) + 96
		if size+sz > MaxPullResultLen {
			out.Truncated = true
			break
		}
		size += sz
		out.Nodes = append(out.Nodes, pn)
	}
	return out
}

// Approval operation names for the context write tools.
const (
	ShareOperation   = "context.share"
	PromoteOperation = "context.promote"
)

func runShare(ctx context.Context, p fleetapi.Provider, ap approvals.Preparer, st StatusSource, o Options, in ShareArgs) ShareOutput {
	var out ShareOutput
	// in.UserConfirmed is deliberately ignored: only the user's
	// authenticator on the approval page authorizes the upload.
	// Credentials are not scanned on the device: Kameas Fleet checks every
	// shared note on arrival and rejects one that looks like it holds a
	// credential. The executor maps that rejection to a code.
	sess, terr := Check(ctx, p, st, OpShare)
	if terr != nil {
		out.Error = terr
		return out
	}
	t, terr := ResolveTeam(in.Team, o.DefaultTeam(), sess.Me.Teams)
	if terr != nil {
		out.Error = terr
		return out
	}
	node, err := BuildShareNode(ShareInput{Title: in.Title, Body: in.Body, Kind: in.Kind}, t.ID, o.Now(), o.Rand)
	if err != nil {
		out.Error = invalid(err.Error())
		return out
	}
	payload, err := json.Marshal(node)
	if err != nil {
		out.Error = invalid(err.Error())
		return out
	}
	pend, terr := submit(ctx, p, ap, approvals.Request{
		Operation: ShareOperation, TargetIDs: []string{"team:" + t.Name},
		Payload: payload, IdempotencyKey: ShareOperation + ":" + node.ID,
		Summary: fmt.Sprintf("Share a %s note titled %q with the team %q in Kameas Fleet.", node.Kind, node.Title, t.Name),
		Impact:  "Every member of " + t.Name + " will be able to read this note.",
	})
	if terr != nil {
		out.Error = terr
		return out
	}
	out.RequestID, out.Status, out.ApproveURL, out.Message = pend.RequestID, string(pend.Status), pend.ApproveURL, pend.Message
	out.Title, out.Team = node.Title, t.Name
	return out
}

func runPromote(ctx context.Context, p fleetapi.Provider, ap approvals.Preparer, st StatusSource, in PromoteInput) PromoteOutput {
	var out PromoteOutput
	if _, terr := Check(ctx, p, st, OpPromote); terr != nil {
		out.Error = terr
		return out
	}
	req, err := BuildPromote(in.NodeID)
	if err != nil {
		out.Error = invalid(err.Error())
		return out
	}
	// The approval page shows what is promoted (the note's title and the
	// team it is shared with now), so look the node up first (a read).
	nc, terr := lookupNode(ctx, p, req.NodeID)
	if terr != nil {
		out.Error = terr
		return out
	}
	node := nc.Node
	payload, err := json.Marshal(promotePayload{NodeID: req.NodeID, To: string(req.To), TeamID: node.TeamID, Title: node.Title, Body: nc.Body})
	if err != nil {
		out.Error = invalid(err.Error())
		return out
	}
	pend, terr := submit(ctx, p, ap, approvals.Request{
		Operation: PromoteOperation, TargetIDs: []string{"node:" + req.NodeID, "team:" + node.TeamID, "classification:" + string(req.To)},
		Payload: payload, IdempotencyKey: PromoteOperation + ":" + req.NodeID + ":" + string(req.To),
		Summary: fmt.Sprintf("Share the team note %q with your whole organization.", node.Title),
		Impact:  "Everyone in your organization will be able to read this note.",
	})
	if terr != nil {
		out.Error = terr
		return out
	}
	out.RequestID, out.Status, out.ApproveURL, out.Message = pend.RequestID, string(pend.Status), pend.ApproveURL, pend.Message
	out.ID = req.NodeID
	return out
}

// submit fills in the signed-in identity and prepares r.
func submit(ctx context.Context, p fleetapi.Provider, ap approvals.Preparer, r approvals.Request) (approvals.Pending, *ToolError) {
	sess, err := p.Auth().Session(ctx)
	if err != nil {
		return approvals.Pending{}, MapError(err)
	}
	r.Env, r.OrgID, r.UserID = string(sess.Env), sess.OrgID, sess.UserID
	pend, err := approvals.Submit(ctx, ap, r)
	if err != nil {
		return approvals.Pending{}, &ToolError{Code: CodeApprovalFailed, Message: "The write could not be prepared for approval; nothing was written."}
	}
	return pend, nil
}

// CodeApprovalFailed means the approval request could not be prepared.
const CodeApprovalFailed = "approval_failed"

type promotePayload struct {
	NodeID string `json:"node_id"`
	To     string `json:"to"`
	TeamID string `json:"team_id,omitempty"`
	// Title and Body are the note when the request was prepared, shown on
	// the approval page (bound by the payload hash); the executor refuses
	// if the note changed since.
	Title string `json:"title,omitempty"`
	Body  string `json:"body,omitempty"`
}

// lookupNode reads node id (title, team) from Fleet.
func lookupNode(ctx context.Context, p fleetapi.Provider, id string) (fleetapi.NodeContent, *ToolError) {
	nodes, err := p.Context().Pull(ctx, fleetapi.PullRequest{IDs: []string{id}})
	if err != nil {
		return fleetapi.NodeContent{}, MapError(err)
	}
	for _, n := range nodes {
		if n.Node.ID == id {
			if n.Node.TeamID == "" || n.Node.Classification != fleetapi.ClassificationTeamShared {
				return fleetapi.NodeContent{}, invalid("only a team note can be shared with the whole organization")
			}
			return n, nil
		}
	}
	return fleetapi.NodeContent{}, invalid("no team note with that id is readable")
}

// ExecuteShare returns the approval executor for ShareOperation. The relay
// daemon runs it only after the user approved the exact node; it rechecks
// tier, lockdown, pause and team membership, then pushes the stored node
// as is.
func ExecuteShare(p fleetapi.Provider, st StatusSource) approvals.Executor {
	return func(ctx context.Context, payload []byte, _ string) ([]byte, error) {
		var node NodeInput
		if err := json.Unmarshal(payload, &node); err != nil || node.ID == "" {
			return nil, approvals.ErrorCode("invalid_payload")
		}
		sess, terr := Check(ctx, p, st, OpShare)
		if terr != nil {
			return nil, approvals.ErrorCode(terr.Code)
		}
		if err := recheckSession(ctx, p); err != nil {
			return nil, err
		}
		member := false
		for _, t := range sess.Me.Teams {
			member = member || t.ID == node.TeamID
		}
		if !member || node.Classification != string(fleetapi.ClassificationTeamShared) || node.LoadPolicy != "on_demand" {
			return nil, approvals.ErrorCode("scope_changed")
		}
		got, err := p.Context().Push(ctx, ToPushRequest(node))
		if err != nil {
			return nil, writeErr(err)
		}
		id, cls := got.ID, string(got.Classification)
		if id == "" {
			id = node.ID
		}
		if cls == "" {
			cls = node.Classification
		}
		return json.Marshal(map[string]string{"id": id, "classification": cls})
	}
}

// ReconcileShare reports whether an interrupted share reached Fleet (the
// client-generated node id exists), without writing.
func ReconcileShare(p fleetapi.Provider) approvals.Reconciler {
	return func(ctx context.Context, payload []byte, _ string) ([]byte, bool, error) {
		var node NodeInput
		if err := json.Unmarshal(payload, &node); err != nil {
			return nil, false, err
		}
		nodes, err := p.Context().Pull(ctx, fleetapi.PullRequest{IDs: []string{node.ID}, TeamID: node.TeamID})
		if err != nil {
			return nil, false, err
		}
		for _, n := range nodes {
			if n.Node.ID == node.ID {
				res, err := json.Marshal(map[string]string{"id": n.Node.ID, "classification": string(n.Node.Classification)})
				return res, err == nil, err
			}
		}
		return nil, false, nil
	}
}

// ExecutePromote returns the approval executor for PromoteOperation.
func ExecutePromote(p fleetapi.Provider, st StatusSource) approvals.Executor {
	return func(ctx context.Context, payload []byte, _ string) ([]byte, error) {
		var pp promotePayload
		if err := json.Unmarshal(payload, &pp); err != nil || pp.NodeID == "" {
			return nil, approvals.ErrorCode("invalid_payload")
		}
		if _, terr := Check(ctx, p, st, OpPromote); terr != nil {
			return nil, approvals.ErrorCode(terr.Code)
		}
		if err := recheckSession(ctx, p); err != nil {
			return nil, err
		}
		// The note must still be the one the user saw: same title, same
		// team.
		nc, terr := lookupNode(ctx, p, pp.NodeID)
		if terr != nil {
			if terr.Code == CodeInvalidInput {
				return nil, approvals.ErrorCode("node_changed")
			}
			return nil, approvals.ErrorCode(terr.Code)
		}
		if nc.Node.Title != pp.Title || nc.Node.TeamID != pp.TeamID || nc.Body != pp.Body {
			return nil, approvals.ErrorCode("node_changed")
		}
		res, err := p.Context().Promote(ctx, fleetapi.PromoteRequest{NodeID: pp.NodeID, To: fleetapi.Classification(pp.To), TeamID: pp.TeamID})
		if err != nil {
			return nil, writeErr(err)
		}
		return json.Marshal(map[string]any{"id": res.Node.ID, "classification": string(res.Node.Classification), "warnings": len(res.Warnings)})
	}
}

// recheckSession refuses unless the provider's session is still the
// account the user approved for (checked right before writing).
func recheckSession(ctx context.Context, p fleetapi.Provider) error {
	sess, err := p.Auth().Session(ctx)
	if err != nil {
		return approvals.ErrorCode(CodeNotSignedIn)
	}
	return approvals.CheckSession(ctx, string(sess.Env), sess.OrgID, sess.UserID)
}

// RenderShare describes a share payload for the approval page from the
// payload alone. teamName optionally resolves a team id to its display
// name (the id is shown when it cannot).
func RenderShare(teamName func(id string) string) approvals.Renderer {
	return func(payload []byte) (approvals.Rendered, error) {
		var n NodeInput
		if err := json.Unmarshal(payload, &n); err != nil {
			return approvals.Rendered{}, err
		}
		if n.ID == "" || n.TeamID == "" || n.Classification != string(fleetapi.ClassificationTeamShared) || n.LoadPolicy != "on_demand" || !validShareKind(n.Kind) {
			return approvals.Rendered{}, errors.New("not a team share")
		}
		team := n.TeamID
		if teamName != nil {
			if name := teamName(n.TeamID); name != "" {
				team = name + " (" + n.TeamID + ")"
			}
		}
		return approvals.Rendered{
			Summary: fmt.Sprintf("Share a %s note titled %q with the team %s in Kameas Fleet.", n.Kind, n.Title, team),
			Impact:  "Every member of " + team + " will be able to read this note.",
			Targets: []string{"team:" + n.TeamID, "node:" + n.ID},
		}, nil
	}
}

// RenderPromote describes a promote payload for the approval page: the
// note's title, the team it is shared with now (teamName optionally
// resolves the id to its display name) and the new organization-wide
// scope.
func RenderPromote(teamName func(id string) string) approvals.Renderer {
	return func(payload []byte) (approvals.Rendered, error) {
		var pp promotePayload
		if err := json.Unmarshal(payload, &pp); err != nil {
			return approvals.Rendered{}, err
		}
		if pp.NodeID == "" || pp.TeamID == "" || pp.Title == "" || pp.To != string(fleetapi.ClassificationOrgShared) {
			return approvals.Rendered{}, errors.New("not an organization promote")
		}
		team := pp.TeamID
		if teamName != nil {
			if name := teamName(pp.TeamID); name != "" {
				team = name + " (" + pp.TeamID + ")"
			}
		}
		return approvals.Rendered{
			Summary: fmt.Sprintf("Share the team note %q (now shared with the team %s) with your whole organization.", pp.Title, team),
			Impact:  "Scope changes from team " + team + " to the whole organization: everyone in your organization will be able to read this note.",
			Targets: []string{"node:" + pp.NodeID, "team:" + pp.TeamID, "classification:" + pp.To},
			Details: []approvals.Detail{{Target: "node:" + pp.NodeID, Label: pp.Title, Text: pp.Body}},
		}, nil
	}
}

// ReconcilePromote reports whether an interrupted promote reached Fleet
// (the node is organization-shared now), without writing. Not applied
// means the relay retries the executor, which refuses if the note changed.
func ReconcilePromote(p fleetapi.Provider) approvals.Reconciler {
	return func(ctx context.Context, payload []byte, _ string) ([]byte, bool, error) {
		var pp promotePayload
		if err := json.Unmarshal(payload, &pp); err != nil {
			return nil, false, err
		}
		nodes, err := p.Context().Pull(ctx, fleetapi.PullRequest{IDs: []string{pp.NodeID}})
		if err != nil {
			return nil, false, err
		}
		for _, n := range nodes {
			if n.Node.ID == pp.NodeID && n.Node.Classification == fleetapi.ClassificationOrgShared {
				res, err := json.Marshal(map[string]string{"id": n.Node.ID, "classification": string(n.Node.Classification)})
				return res, err == nil, err
			}
		}
		return nil, false, nil
	}
}

// writeErr separates definite Fleet refusals (coded) from transport
// failures whose outcome is unknown.
func writeErr(err error) error {
	var ae *fleetapi.APIError
	if errors.Is(err, fleetapi.ErrNotSignedIn) || errors.Is(err, fleetapi.ErrUnofficialBuild) || errors.As(err, &ae) {
		return approvals.ErrorCode(MapError(err).Code)
	}
	return fmt.Errorf("%w: %v", approvals.ErrOutcomeUnknown, err)
}

func runTeamMembers(ctx context.Context, p fleetapi.Provider, st StatusSource, o Options, in TeamMembersInput) TeamMembersOutput {
	out := TeamMembersOutput{Teams: []TeamOut{}, Members: []MemberOut{}}
	sess, terr := Check(ctx, p, st, OpTeamMembers)
	if terr != nil {
		out.Error = terr
		return out
	}
	for _, t := range sess.Me.Teams {
		out.Teams = append(out.Teams, TeamOut{ID: t.ID, Name: t.Name})
	}
	t, terr := ResolveTeam(in.Team, o.DefaultTeam(), sess.Me.Teams)
	if terr != nil {
		out.Error = terr
		return out
	}
	out.Team = t.Name
	ms, err := p.Team().Members(ctx, t.ID)
	if err != nil {
		out.Error = MapError(err)
		return out
	}
	for _, m := range ms {
		// Display names and roles only: anything that looks
		// like an email address is dropped rather than shown.
		name := m.DisplayName
		if strings.Contains(name, "@") {
			name = "(member)"
		}
		out.Members = append(out.Members, MemberOut{DisplayName: name, Role: m.Role})
	}
	return out
}

// ---- helpers ----

func invalid(msg string) *ToolError { return &ToolError{Code: CodeInvalidInput, Message: msg} }

func errResult(e *ToolError) *sdk.CallToolResult {
	if e == nil {
		return nil
	}
	return &sdk.CallToolResult{IsError: true}
}

// clip truncates s to at most n bytes on a rune boundary.
func clip(s string, n int) (string, bool) {
	if len(s) <= n {
		return s, false
	}
	cut := n
	for cut > 0 && !utf8.RuneStart(s[cut]) {
		cut--
	}
	return s[:cut], true
}

func ptr[T any](v T) *T { return &v }

func withTitle(a *sdk.ToolAnnotations, title string) *sdk.ToolAnnotations {
	c := *a
	c.Title = title
	return &c
}
