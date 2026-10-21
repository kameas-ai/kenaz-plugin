package contexttools

import (
	"context"
	"crypto/rand"
	"io"
	"sort"
	"strings"
	"time"
	"unicode/utf8"

	sdk "github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/kameas-ai/kenaz-plugin/fleetapi"
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
	Title         string `json:"title" jsonschema:"short title, up to 200 characters"`
	Body          string `json:"body" jsonschema:"the note text the user wrote or approved, up to 16 KiB"`
	Kind          string `json:"kind" jsonschema:"one of guidance, procedure, fact, glossary_term, doc"`
	Team          string `json:"team,omitempty" jsonschema:"team name or id; defaults to your configured or only team"`
	UserConfirmed bool   `json:"user_confirmed,omitempty" jsonschema:"true only after the user has seen this exact title and body and said to share it"`
}

type ShareOutput struct {
	ID             string     `json:"id,omitempty"`
	Title          string     `json:"title,omitempty"`
	Team           string     `json:"team,omitempty"`
	Classification string     `json:"classification,omitempty"`
	Error          *ToolError `json:"error,omitempty"`
}

type PromoteInput struct {
	NodeID        string `json:"node_id" jsonschema:"id of a team-shared node"`
	UserConfirmed bool   `json:"user_confirmed,omitempty" jsonschema:"true only after the user said to share this node with the whole organization"`
}

type PromoteOutput struct {
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
		Description: "UPLOADS the given title and body to Kameas Fleet as a note visible to every member of the chosen team. " +
			"Only share text the user wrote or explicitly approved; never share file contents, code, transcripts or anything containing secrets. " +
			"Show the user the exact title, body and team first, and set user_confirmed=true only after they say to share it. " +
			"Your harness will also ask the user to approve this call.",
		Annotations: &sdk.ToolAnnotations{Title: "Share a note with your team", DestructiveHint: ptr(false), OpenWorldHint: ptr(true)},
		Meta:        writes,
	}, func(ctx context.Context, _ *sdk.CallToolRequest, in ShareArgs) (*sdk.CallToolResult, ShareOutput, error) {
		touch()
		out := runShare(ctx, d.Provider, st, o, in)
		return errResult(out.Error), out, nil
	})

	sdk.AddTool(s, &sdk.Tool{
		Name:  ToolPromote,
		Title: "Share a team note with the whole organization",
		Description: "Widens an existing team-shared Kameas Fleet note so the WHOLE ORGANIZATION can see it. " +
			"Organization owners and admins only, Enterprise plan. Set user_confirmed=true only after the user says to share it organization-wide. " +
			"Your harness will also ask the user to approve this call.",
		Annotations: &sdk.ToolAnnotations{Title: "Share a team note with the whole organization", DestructiveHint: ptr(false), OpenWorldHint: ptr(true)},
		Meta:        writes,
	}, func(ctx context.Context, _ *sdk.CallToolRequest, in PromoteInput) (*sdk.CallToolResult, PromoteOutput, error) {
		touch()
		out := runPromote(ctx, d.Provider, st, in)
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

func runShare(ctx context.Context, p fleetapi.Provider, st StatusSource, o Options, in ShareArgs) ShareOutput {
	var out ShareOutput
	if !in.UserConfirmed {
		out.Error = &ToolError{Code: CodeNotConfirmed, Message: "Nothing was shared. Show the user the exact title, body and team, and call again with user_confirmed=true only if they say to share it."}
		return out
	}
	// Credentials are not scanned on the device: Kameas Fleet checks every
	// shared note on arrival and rejects one that looks like it holds a
	// credential. MapError surfaces that rejection.
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
	got, err := p.Context().Push(ctx, ToPushRequest(node))
	if err != nil {
		out.Error = MapError(err)
		return out
	}
	out.ID, out.Title, out.Team = got.ID, got.Title, t.Name
	out.Classification = string(got.Classification)
	if out.ID == "" {
		out.ID = node.ID
	}
	if out.Classification == "" {
		out.Classification = node.Classification
	}
	return out
}

func runPromote(ctx context.Context, p fleetapi.Provider, st StatusSource, in PromoteInput) PromoteOutput {
	var out PromoteOutput
	if !in.UserConfirmed {
		out.Error = &ToolError{Code: CodeNotConfirmed, Message: "Nothing was changed. Call again with user_confirmed=true only if the user says to share this note with the whole organization."}
		return out
	}
	if _, terr := Check(ctx, p, st, OpPromote); terr != nil {
		out.Error = terr
		return out
	}
	req, err := BuildPromote(in.NodeID)
	if err != nil {
		out.Error = invalid(err.Error())
		return out
	}
	res, err := p.Context().Promote(ctx, req)
	if err != nil {
		out.Error = MapError(err)
		return out
	}
	out.ID, out.Classification, out.Warnings = res.Node.ID, string(res.Node.Classification), res.Warnings
	return out
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
