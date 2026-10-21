// Package memorytools is the memory_* MCP tool group: save, search, read,
// list and forget short notes in Kenaz's on-device memory store.
//
// Memory is local user data. These tools read and write one SQLite file
// on this device and never call Kameas Fleet: the group does not use the
// Fleet provider in mcp.Deps, works signed out, and is not affected by
// telemetry consent. A test pins that.
package memorytools

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"strings"
	"time"

	sdk "github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/kameas-ai/kenaz-plugin/internal/mcp"
	"github.com/kameas-ai/kenaz-plugin/internal/memory"
)

// Tool names. They are part of the plugin's public surface.
const (
	ToolSave   = "memory_save"
	ToolSearch = "memory_search"
	ToolGet    = "memory_get"
	ToolForget = "memory_forget"
	ToolRecent = "memory_recent"
)

// MaxResult bounds the JSON a single tool call returns (~16 KiB).
const MaxResult = 15 << 10

// onDevice is the sentence every tool description carries.
const onDevice = "Kenaz memory is stored only on this device and is never sent to Kameas Fleet. "

// Options are the group's dependencies. Path is required.
type Options struct {
	// Path is the memory database file.
	Path string
	// Enabled reports whether memory is turned on (nil: always on).
	Enabled func() bool
	// Disclose returns the one-time disclosure the first time it is
	// called across the machine, and "" afterwards (nil: never).
	Disclose func() string
	// Workdir is the project directory (nil: the process working
	// directory).
	Workdir func() string
	// Busy bounds how long a call waits for another writer.
	Busy time.Duration
}

// NewGroup returns the memory tool group.
func NewGroup(o Options) mcp.Group {
	if o.Enabled == nil {
		o.Enabled = func() bool { return true }
	}
	if o.Disclose == nil {
		o.Disclose = func() string { return "" }
	}
	if o.Workdir == nil {
		o.Workdir = defaultWorkdir
	}
	if o.Busy <= 0 {
		o.Busy = 2 * time.Second
	}
	return mcp.GroupFunc(func(s *sdk.Server, d mcp.Deps) { register(s, d, o) })
}

// defaultWorkdir prefers the project directory Claude Code exports, then
// the working directory both harnesses start MCP servers in.
func defaultWorkdir() string {
	if d := os.Getenv("CLAUDE_PROJECT_DIR"); d != "" {
		return d
	}
	d, _ := os.Getwd()
	return d
}

// ToolError is a tool-level error with a message for the user.
type ToolError struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

// ---- inputs and outputs ----

type SaveInput struct {
	Title   string   `json:"title" jsonschema:"short title, up to 200 characters"`
	Body    string   `json:"body" jsonschema:"the note, up to 8 KiB: a durable fact, decision, convention or fix"`
	Tags    []string `json:"tags,omitempty" jsonschema:"up to 10 tags, each up to 40 characters"`
	Project string   `json:"project,omitempty" jsonschema:"current (default): this project only; global: every project"`
}

type SaveOutput struct {
	ID      string     `json:"id,omitempty"`
	Project string     `json:"project,omitempty"`
	Notice  string     `json:"notice,omitempty"`
	Error   *ToolError `json:"error,omitempty"`
}

type SearchInput struct {
	Query   string `json:"query" jsonschema:"words to find in titles, bodies and tags"`
	Project string `json:"project,omitempty" jsonschema:"current (default): this project and global notes; all: every project"`
	Limit   int    `json:"limit,omitempty" jsonschema:"maximum results, 1 to 20 (default 10)"`
}

type Hit struct {
	ID        string   `json:"id"`
	Title     string   `json:"title"`
	Tags      []string `json:"tags"`
	CreatedAt string   `json:"created_at"`
	Snippet   string   `json:"snippet"`
}

type SearchOutput struct {
	Results   []Hit      `json:"results"`
	Truncated bool       `json:"truncated"`
	Error     *ToolError `json:"error,omitempty"`
}

type GetInput struct {
	IDs []string `json:"ids" jsonschema:"1 to 10 memory ids from memory_search or memory_recent"`
}

type Record struct {
	ID        string   `json:"id"`
	CreatedAt string   `json:"created_at"`
	UpdatedAt string   `json:"updated_at"`
	Project   string   `json:"project"`
	Title     string   `json:"title"`
	Body      string   `json:"body"`
	Tags      []string `json:"tags"`
	Source    string   `json:"source"`
}

type GetOutput struct {
	Notes   []Record   `json:"notes"`
	Missing []string   `json:"missing,omitempty"`
	Omitted []string   `json:"omitted,omitempty"`
	Error   *ToolError `json:"error,omitempty"`
}

type ForgetInput struct {
	ID string `json:"id" jsonschema:"the memory id to delete"`
}

type ForgetOutput struct {
	Deleted bool       `json:"deleted"`
	Error   *ToolError `json:"error,omitempty"`
}

type RecentInput struct {
	Limit   int    `json:"limit,omitempty" jsonschema:"maximum entries, 1 to 20 (default 10)"`
	Project string `json:"project,omitempty" jsonschema:"current (default): this project and global notes; all: every project"`
}

type IndexEntry struct {
	ID        string `json:"id"`
	Title     string `json:"title"`
	CreatedAt string `json:"created_at"`
}

type RecentOutput struct {
	Entries   []IndexEntry `json:"entries"`
	Truncated bool         `json:"truncated"`
	Error     *ToolError   `json:"error,omitempty"`
}

// ---- registration ----

func ptr[T any](v T) *T { return &v }

func register(s *sdk.Server, _ mcp.Deps, o Options) {
	readOnly := func(title string) *sdk.ToolAnnotations {
		return &sdk.ToolAnnotations{Title: title, ReadOnlyHint: true, IdempotentHint: true, OpenWorldHint: ptr(false)}
	}

	sdk.AddTool(s, &sdk.Tool{
		Name:  ToolSave,
		Title: "Save a memory",
		Description: onDevice + "Save a short note for later sessions: a durable fact, decision, convention or fix worth remembering. " +
			"Never save secrets, credentials or personal data; ask the user first before saving anything sensitive. " +
			"Notes are scoped to the current project unless project=global. Returns the new id. " +
			"If the result has a notice, show it to the user.",
		Annotations: &sdk.ToolAnnotations{Title: "Save a memory", DestructiveHint: ptr(false), OpenWorldHint: ptr(false)},
	}, func(ctx context.Context, req *sdk.CallToolRequest, in SaveInput) (*sdk.CallToolResult, SaveOutput, error) {
		out := runSave(ctx, o, sourceOf(req), in)
		return errResult(out.Error), out, nil
	})

	sdk.AddTool(s, &sdk.Tool{
		Name:  ToolSearch,
		Title: "Search memories",
		Description: onDevice + "Full-text search of saved memories. Returns ids, titles, tags, dates and short snippets; " +
			"use memory_get for the full text. Searches this project and global notes unless project=all.",
		Annotations: readOnly("Search memories"),
	}, func(ctx context.Context, _ *sdk.CallToolRequest, in SearchInput) (*sdk.CallToolResult, SearchOutput, error) {
		out := runSearch(ctx, o, in)
		return errResult(out.Error), out, nil
	})

	sdk.AddTool(s, &sdk.Tool{
		Name:        ToolGet,
		Title:       "Read memories",
		Description: onDevice + "Read the full text of up to 10 saved memories by id (ids come from memory_search, memory_recent or the session's memory index).",
		Annotations: readOnly("Read memories"),
	}, func(ctx context.Context, _ *sdk.CallToolRequest, in GetInput) (*sdk.CallToolResult, GetOutput, error) {
		out := runGet(ctx, o, in)
		return errResult(out.Error), out, nil
	})

	sdk.AddTool(s, &sdk.Tool{
		Name:        ToolForget,
		Title:       "Delete a memory",
		Description: onDevice + "Permanently delete one saved memory by id. This cannot be undone.",
		Annotations: &sdk.ToolAnnotations{Title: "Delete a memory", DestructiveHint: ptr(true), IdempotentHint: true, OpenWorldHint: ptr(false)},
	}, func(ctx context.Context, _ *sdk.CallToolRequest, in ForgetInput) (*sdk.CallToolResult, ForgetOutput, error) {
		out := runForget(ctx, o, in)
		return errResult(out.Error), out, nil
	})

	sdk.AddTool(s, &sdk.Tool{
		Name:        ToolRecent,
		Title:       "List recent memories",
		Description: onDevice + "List the most recently saved memories (ids, titles and dates only) for this project and global notes, or every project with project=all.",
		Annotations: readOnly("List recent memories"),
	}, func(ctx context.Context, _ *sdk.CallToolRequest, in RecentInput) (*sdk.CallToolResult, RecentOutput, error) {
		out := runRecent(ctx, o, in)
		return errResult(out.Error), out, nil
	})
}

func errResult(e *ToolError) *sdk.CallToolResult {
	if e == nil {
		return nil
	}
	return &sdk.CallToolResult{IsError: true}
}

// sourceOf names the harness from the MCP client's self-reported name.
func sourceOf(req *sdk.CallToolRequest) string {
	if req != nil && req.Session != nil {
		if p := req.Session.InitializeParams(); p != nil && p.ClientInfo != nil &&
			strings.Contains(strings.ToLower(p.ClientInfo.Name), "codex") {
			return memory.SourceCodex
		}
	}
	return memory.SourceClaudeCode
}

var errOff = &ToolError{Code: "memory_off", Message: memory.OffMessage}

func userErr(err error) *ToolError {
	switch {
	case errors.Is(err, memory.ErrFull):
		return &ToolError{Code: "memory_full", Message: err.Error()}
	case errors.Is(err, memory.ErrLooksSecret):
		return &ToolError{Code: "looks_like_secret", Message: err.Error()}
	case errors.Is(err, memory.ErrNotFound):
		return &ToolError{Code: "not_found", Message: err.Error()}
	case memory.IsUserError(err):
		return &ToolError{Code: "invalid_argument", Message: err.Error()}
	default:
		return &ToolError{Code: "memory_unavailable", Message: "The memory store on this device could not be used: " + err.Error()}
	}
}

func invalid(msg string) *ToolError { return &ToolError{Code: "invalid_argument", Message: msg} }

func (o Options) project() (memory.Project, error) { return memory.ProjectFor(o.Workdir()) }

// scope maps a project argument to a store scope.
func (o Options) scope(arg string) (memory.Scope, *ToolError) {
	switch strings.ToLower(strings.TrimSpace(arg)) {
	case "", "current":
		p, err := o.project()
		if err != nil {
			return memory.Scope{}, userErr(err)
		}
		return memory.ProjectScope(p.Key), nil
	case "all":
		return memory.AllProjects, nil
	default:
		return memory.Scope{}, invalid("project must be current or all")
	}
}

// reader opens the store read-only; ok=false with no error means there is
// no store yet (an empty result).
func (o Options) reader() (*memory.Store, bool, *ToolError) {
	s, err := memory.OpenReadOnly(o.Path, o.Busy)
	if errors.Is(err, memory.ErrNoStore) {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, userErr(err)
	}
	return s, true, nil
}

func stamp(t time.Time) string { return t.UTC().Format(time.RFC3339) }

func runSave(ctx context.Context, o Options, source string, in SaveInput) SaveOutput {
	if !o.Enabled() {
		return SaveOutput{Error: errOff}
	}
	n := memory.NewNote{Title: in.Title, Body: in.Body, Tags: in.Tags, Source: source}
	switch strings.ToLower(strings.TrimSpace(in.Project)) {
	case "", "current":
		p, err := o.project()
		if err != nil {
			return SaveOutput{Error: userErr(err)}
		}
		n.ProjectKey, n.ProjectRoot = p.Key, p.Root
	case "global":
		n.ProjectKey = memory.GlobalProject
	default:
		return SaveOutput{Error: invalid("project must be current or global")}
	}
	// Validate before creating the store so a refused first note leaves
	// nothing on disk.
	if err := n.Validate(); err != nil {
		return SaveOutput{Error: userErr(err)}
	}
	s, err := memory.Open(o.Path)
	if err != nil {
		return SaveOutput{Error: userErr(err)}
	}
	defer s.Close()
	id, err := s.Save(ctx, n)
	if err != nil {
		return SaveOutput{Error: userErr(err)}
	}
	out := SaveOutput{ID: id, Project: "current"}
	if n.ProjectKey == memory.GlobalProject {
		out.Project = "global"
	}
	if d := o.Disclose(); d != "" {
		out.Notice = d
	}
	return out
}

func runSearch(ctx context.Context, o Options, in SearchInput) SearchOutput {
	out := SearchOutput{Results: []Hit{}}
	if !o.Enabled() {
		out.Error = errOff
		return out
	}
	if strings.TrimSpace(in.Query) == "" {
		out.Error = invalid("query is required")
		return out
	}
	if len(in.Query) > 512 {
		out.Error = invalid("query is longer than 512 characters")
		return out
	}
	sc, terr := o.scope(in.Project)
	if terr != nil {
		out.Error = terr
		return out
	}
	s, ok, terr := o.reader()
	if !ok {
		out.Error = terr
		return out
	}
	defer s.Close()
	hits, err := s.Search(ctx, in.Query, sc, in.Limit)
	if err != nil {
		out.Error = userErr(err)
		return out
	}
	size := 0
	for _, h := range hits {
		tags := h.Tags
		if tags == nil {
			tags = []string{}
		}
		hit := Hit{ID: h.ID, Title: h.Title, Tags: tags, CreatedAt: stamp(h.CreatedAt), Snippet: h.Snippet}
		if size += jsonSize(hit); size > MaxResult {
			out.Truncated = true
			break
		}
		out.Results = append(out.Results, hit)
	}
	return out
}

func runGet(ctx context.Context, o Options, in GetInput) GetOutput {
	out := GetOutput{Notes: []Record{}}
	if !o.Enabled() {
		out.Error = errOff
		return out
	}
	if len(in.IDs) == 0 || len(in.IDs) > memory.MaxGetIDs {
		out.Error = invalid("ids must list 1 to 10 memory ids")
		return out
	}
	s, ok, terr := o.reader()
	if !ok {
		if terr == nil {
			out.Missing = in.IDs
		}
		out.Error = terr
		return out
	}
	defer s.Close()
	notes, missing, err := s.Get(ctx, in.IDs)
	if err != nil {
		out.Error = userErr(err)
		return out
	}
	out.Missing = missing
	size := 0
	for _, n := range notes {
		r := Record{ID: n.ID, CreatedAt: stamp(n.CreatedAt), UpdatedAt: stamp(n.UpdatedAt), Project: "current",
			Title: n.Title, Body: n.Body, Tags: n.Tags, Source: n.Source}
		if n.ProjectKey == memory.GlobalProject {
			r.Project = "global"
		} else if p, err := o.project(); err != nil || p.Key != n.ProjectKey {
			r.Project = "other"
		}
		if r.Tags == nil {
			r.Tags = []string{}
		}
		if size+jsonSize(r) > MaxResult {
			out.Omitted = append(out.Omitted, n.ID)
			continue
		}
		size += jsonSize(r)
		out.Notes = append(out.Notes, r)
	}
	return out
}

func runForget(ctx context.Context, o Options, in ForgetInput) ForgetOutput {
	if !o.Enabled() {
		return ForgetOutput{Error: errOff}
	}
	if strings.TrimSpace(in.ID) == "" {
		return ForgetOutput{Error: invalid("id is required")}
	}
	if _, err := os.Stat(o.Path); err != nil {
		return ForgetOutput{Error: userErr(memory.ErrNotFound)}
	}
	s, err := memory.Open(o.Path)
	if err != nil {
		return ForgetOutput{Error: userErr(err)}
	}
	defer s.Close()
	if err := s.Forget(ctx, strings.TrimSpace(in.ID)); err != nil {
		return ForgetOutput{Error: userErr(err)}
	}
	return ForgetOutput{Deleted: true}
}

func runRecent(ctx context.Context, o Options, in RecentInput) RecentOutput {
	out := RecentOutput{Entries: []IndexEntry{}}
	if !o.Enabled() {
		out.Error = errOff
		return out
	}
	sc, terr := o.scope(in.Project)
	if terr != nil {
		out.Error = terr
		return out
	}
	s, ok, terr := o.reader()
	if !ok {
		out.Error = terr
		return out
	}
	defer s.Close()
	es, err := s.Recent(ctx, sc, in.Limit)
	if err != nil {
		out.Error = userErr(err)
		return out
	}
	size := 0
	for _, e := range es {
		ie := IndexEntry{ID: e.ID, Title: e.Title, CreatedAt: stamp(e.CreatedAt)}
		if size += jsonSize(ie); size > MaxResult {
			out.Truncated = true
			break
		}
		out.Entries = append(out.Entries, ie)
	}
	return out
}

func jsonSize(v any) int {
	b, _ := json.Marshal(v)
	return len(b) + 1
}
