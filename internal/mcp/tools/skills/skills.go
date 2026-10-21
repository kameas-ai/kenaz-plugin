// Package skills is the skills_* MCP tool group:
// search the Kameas Fleet catalog, install and uninstall shared
// Agent Skills, and publish a local skill. Installs go through the same
// materializer as the daemon's sync, under the same cross-process lock.
// Publishing always needs an explicit user confirmation in the harness.
package skills

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	sdk "github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/kameas-ai/kenaz-plugin/fleetapi"
	"github.com/kameas-ai/kenaz-plugin/internal/mcp"
	sk "github.com/kameas-ai/kenaz-plugin/internal/skills"
)

// Group registers skills_search, skills_install, skills_uninstall and
// skills_publish.
var Group = mcp.GroupFunc(register)

// HomeDir returns the user's home directory (tests override it).
var HomeDir = os.UserHomeDir

// capTeamShare gates team/org publishing.
const capTeamShare = "catalog_team_share"

// requiresInteraction makes the harness confirm every call with the user
// (Claude Code: _meta["anthropic/requiresUserInteraction"]).
var requiresInteraction = sdk.Meta{"anthropic/requiresUserInteraction": true}

// SearchInput is skills_search's arguments.
type SearchInput struct {
	Query string `json:"query,omitempty" jsonschema:"words to match in the skill name or description"`
	Scope string `json:"scope,omitempty" jsonschema:"private, team or org_public; empty means everything you can see"`
}

// SearchItem is one catalog result.
type SearchItem struct {
	CatalogID   string `json:"catalog_id"`
	Slug        string `json:"slug"`
	Description string `json:"description"`
	Version     string `json:"version"`
	Visibility  string `json:"visibility"`
	Kind        string `json:"kind"`
	Mandated    bool   `json:"mandated"`
	Installed   bool   `json:"installed"`
	Note        string `json:"note,omitempty"`
}

// SearchOutput is skills_search's result.
type SearchOutput struct {
	Items []SearchItem `json:"items"`
}

// InstallInput is skills_install's arguments.
type InstallInput struct {
	CatalogID string `json:"catalog_id" jsonschema:"the catalog_id from skills_search"`
	Version   string `json:"version,omitempty" jsonschema:"pin a specific version; empty follows the latest"`
}

// InstallOutput is skills_install's result.
type InstallOutput struct {
	Slug        string   `json:"slug"`
	Version     string   `json:"version"`
	Directories []string `json:"directories"`
	Stripped    []string `json:"stripped,omitempty"`
	Translated  bool     `json:"translated,omitempty"`
	Message     string   `json:"message"`
}

// UninstallInput is skills_uninstall's arguments.
type UninstallInput struct {
	CatalogID string `json:"catalog_id" jsonschema:"the catalog_id of an installed skill"`
}

// UninstallOutput is skills_uninstall's result.
type UninstallOutput struct {
	Removed int    `json:"removed"`
	Message string `json:"message"`
}

// PublishInput is skills_publish's arguments.
type PublishInput struct {
	Path       string `json:"path" jsonschema:"absolute path of a local skill directory containing SKILL.md"`
	Version    string `json:"version" jsonschema:"version to publish, e.g. 1.0.0"`
	Visibility string `json:"visibility,omitempty" jsonschema:"private (default), team or org_public"`
}

// PublishOutput is skills_publish's result.
type PublishOutput struct {
	CatalogID  string `json:"catalog_id"`
	Slug       string `json:"slug"`
	Version    string `json:"version"`
	Visibility string `json:"visibility"`
	Files      int    `json:"files"`
	Bytes      int    `json:"bytes"`
	Message    string `json:"message"`
}

func register(s *sdk.Server, d mcp.Deps) {
	sdk.AddTool(s, &sdk.Tool{
		Name:        "skills_search",
		Description: "Search the Kameas Fleet catalog for shared Agent Skills your organization or team has published. Returns each skill's catalog_id, name, description, version, visibility, and whether it is already installed on this machine. Read-only.",
		Annotations: &sdk.ToolAnnotations{Title: "Search shared skills", ReadOnlyHint: true, IdempotentHint: true},
	}, func(ctx context.Context, _ *sdk.CallToolRequest, in SearchInput) (*sdk.CallToolResult, SearchOutput, error) {
		touch(d)
		out, err := Search(ctx, d, in)
		return nil, out, err
	})
	sdk.AddTool(s, &sdk.Tool{
		Name:        "skills_install",
		Description: "Install a shared skill from the Kameas Fleet catalog into this machine's skill folders, so it is available in your next session. Shared skills are installed without any tool pre-approvals or hooks they declare; the result says what was removed.",
		Annotations: &sdk.ToolAnnotations{Title: "Install a shared skill", IdempotentHint: true, DestructiveHint: ptr(false)},
	}, func(ctx context.Context, _ *sdk.CallToolRequest, in InstallInput) (*sdk.CallToolResult, InstallOutput, error) {
		touch(d)
		out, err := Install(ctx, d, in)
		return nil, out, err
	})
	sdk.AddTool(s, &sdk.Tool{
		Name:        "skills_uninstall",
		Description: "Remove a shared skill you installed from the Kameas Fleet catalog. Skills your organization requires cannot be removed this way.",
		Annotations: &sdk.ToolAnnotations{Title: "Uninstall a shared skill", IdempotentHint: true},
	}, func(ctx context.Context, _ *sdk.CallToolRequest, in UninstallInput) (*sdk.CallToolResult, UninstallOutput, error) {
		touch(d)
		out, err := Uninstall(ctx, d, in)
		return nil, out, err
	})
	sdk.AddTool(s, &sdk.Tool{
		Name:        "skills_publish",
		Description: "Publish a local Agent Skill directory (with SKILL.md) to your Kameas Fleet catalog. This uploads the skill's files. You will be asked to confirm. Visibility defaults to private (only you); team or org_public share it with colleagues and need a Team plan.",
		Annotations: &sdk.ToolAnnotations{Title: "Publish a skill", DestructiveHint: ptr(false), OpenWorldHint: ptr(true)},
		Meta:        requiresInteraction,
	}, func(ctx context.Context, _ *sdk.CallToolRequest, in PublishInput) (*sdk.CallToolResult, PublishOutput, error) {
		touch(d)
		out, err := Publish(ctx, d, in)
		return nil, out, err
	})
}

func ptr[T any](v T) *T { return &v }

func touch(d mcp.Deps) {
	if d.Touch != nil {
		d.Touch()
	}
}

func materializer() (*sk.Materializer, error) {
	home, err := HomeDir()
	if err != nil {
		return nil, errors.New("cannot find your home directory")
	}
	return &sk.Materializer{Roots: sk.DetectRoots(home)}, nil
}

func store(d mcp.Deps) (sk.Store, error) {
	if d.StateDir == "" {
		return sk.Store{}, errors.New("kenaz state directory is not configured")
	}
	return sk.NewStore(d.StateDir), nil
}

// Search implements skills_search.
func Search(ctx context.Context, d mcp.Deps, in SearchInput) (SearchOutput, error) {
	vis := fleetapi.Visibility(in.Scope)
	switch vis {
	case "", fleetapi.VisibilityPrivate, fleetapi.VisibilityTeam, fleetapi.VisibilityOrgPublic:
	default:
		return SearchOutput{}, fmt.Errorf("scope must be private, team or org_public")
	}
	installed := map[string]bool{}
	if m, err := materializer(); err == nil {
		if owned, err := m.Owned(); err == nil {
			for _, o := range owned {
				installed[o.Marker.CatalogID] = true
			}
		}
	}
	out := SearchOutput{Items: []SearchItem{}}
	for _, kind := range []string{fleetapi.CatalogKindAgentSkill, fleetapi.CatalogKindSkill} {
		entries, err := d.Provider.Catalog().List(ctx, fleetapi.CatalogQuery{Kind: kind, Query: in.Query, Visibility: vis})
		if err != nil {
			return SearchOutput{}, friendly(err)
		}
		for _, e := range entries {
			if e.Revoked {
				continue
			}
			it := SearchItem{
				CatalogID: e.ID, Slug: e.Slug, Description: e.Description, Version: e.Version,
				Visibility: string(e.Visibility), Kind: e.Kind, Mandated: e.Mandated, Installed: installed[e.ID],
			}
			if e.Kind == fleetapi.CatalogKindSkill {
				it.Note = "older Kenaz skill: text and prompt skills are translated on install; tool skills cannot be installed"
			}
			out.Items = append(out.Items, it)
		}
	}
	return out, nil
}

// Install implements skills_install.
func Install(ctx context.Context, d mcp.Deps, in InstallInput) (InstallOutput, error) {
	if strings.TrimSpace(in.CatalogID) == "" {
		return InstallOutput{}, errors.New("catalog_id is required")
	}
	st, err := store(d)
	if err != nil {
		return InstallOutput{}, err
	}
	m, err := materializer()
	if err != nil {
		return InstallOutput{}, err
	}
	if len(m.Roots) == 0 {
		return InstallOutput{}, errors.New("no supported agent harness was found in your home directory")
	}
	item, err := d.Provider.Catalog().Get(ctx, in.CatalogID, in.Version)
	if err != nil {
		return InstallOutput{}, friendly(err)
	}
	if item.Revoked {
		return InstallOutput{}, errors.New("this skill was revoked by its publisher or your organization")
	}
	unlock, err := st.Lock(10 * time.Second)
	if err != nil {
		return InstallOutput{}, err
	}
	defer unlock()
	res := m.Install(sk.Item{CatalogID: item.ID, Kind: item.Kind, Slug: item.Slug, Version: item.Version, Payload: item.Payload, Source: sk.SourceSubscribed})
	if res.Status != fleetapi.AckApplied {
		return InstallOutput{}, errors.New(installFailure(res.Detail))
	}
	subs, err := st.Subscriptions()
	if err != nil {
		return InstallOutput{}, err
	}
	subs[item.ID] = sk.Subscription{CatalogID: item.ID, Kind: item.Kind, Slug: res.Slug, Version: in.Version}
	if err := st.SaveSubscriptions(subs); err != nil {
		return InstallOutput{}, err
	}
	if res.Changed {
		_ = st.BumpGeneration()
	}
	out := InstallOutput{Slug: res.Slug, Version: res.Version, Directories: res.Dirs, Stripped: res.Stripped, Translated: res.Translated}
	out.Message = fmt.Sprintf("Installed %s %s. It will be available in your next session.", res.Slug, res.Version)
	if len(res.Stripped) > 0 {
		out.Message += " It was installed without its " + strings.Join(res.Stripped, " and ") + " settings, because shared skills cannot pre-approve tools or run hooks. Ask your admin to mandate it if your team needs them."
	}
	if res.Translated {
		out.Message += " It was converted from an older Kenaz skill."
	}
	return out, nil
}

// Uninstall implements skills_uninstall.
func Uninstall(_ context.Context, d mcp.Deps, in UninstallInput) (UninstallOutput, error) {
	st, err := store(d)
	if err != nil {
		return UninstallOutput{}, err
	}
	m, err := materializer()
	if err != nil {
		return UninstallOutput{}, err
	}
	unlock, err := st.Lock(10 * time.Second)
	if err != nil {
		return UninstallOutput{}, err
	}
	defer unlock()
	owned, err := m.Owned()
	if err != nil {
		return UninstallOutput{}, err
	}
	for _, o := range owned {
		if o.Marker.CatalogID == in.CatalogID && o.Marker.Source == sk.SourceMandated {
			return UninstallOutput{}, errors.New("your organization requires this skill, so it can't be uninstalled here. Ask your admin if it should be removed")
		}
	}
	subs, err := st.Subscriptions()
	if err != nil {
		return UninstallOutput{}, err
	}
	_, wasSub := subs[in.CatalogID]
	delete(subs, in.CatalogID)
	if err := st.SaveSubscriptions(subs); err != nil {
		return UninstallOutput{}, err
	}
	n, err := m.Remove(in.CatalogID)
	if err != nil {
		return UninstallOutput{}, err
	}
	if n > 0 {
		_ = st.BumpGeneration()
	}
	if n == 0 && !wasSub {
		return UninstallOutput{Message: "That skill is not installed."}, nil
	}
	return UninstallOutput{Removed: n, Message: "Uninstalled. It will be gone in your next session."}, nil
}

// Publish implements skills_publish. The harness has already asked the
// user to confirm (requiresUserInteraction); this validates, enforces the
// tier rule for shared visibility, and uploads.
func Publish(ctx context.Context, d mcp.Deps, in PublishInput) (PublishOutput, error) {
	if !filepath.IsAbs(in.Path) {
		return PublishOutput{}, errors.New("path must be an absolute path to a skill directory")
	}
	if strings.TrimSpace(in.Version) == "" {
		return PublishOutput{}, errors.New("version is required")
	}
	vis := fleetapi.Visibility(in.Visibility)
	switch vis {
	case "":
		vis = fleetapi.VisibilityPrivate
	case fleetapi.VisibilityPrivate, fleetapi.VisibilityTeam, fleetapi.VisibilityOrgPublic:
	default:
		return PublishOutput{}, errors.New("visibility must be private, team or org_public")
	}
	s, payload, err := sk.Package(in.Path)
	if err != nil {
		return PublishOutput{}, err
	}
	if vis != fleetapi.VisibilityPrivate {
		caps, err := d.Provider.Me().Capabilities(ctx)
		if err != nil {
			return PublishOutput{}, friendly(err)
		}
		if !caps.Has(capTeamShare) {
			return PublishOutput{}, errors.New("sharing skills with your team needs a Team plan; publish as private instead")
		}
	}
	e, err := d.Provider.Catalog().Publish(ctx, fleetapi.PublishRequest{
		Kind: fleetapi.CatalogKindAgentSkill, Slug: s.Name, Version: in.Version,
		Description: s.Description, Visibility: vis, Payload: payload,
	})
	if err != nil {
		return PublishOutput{}, friendly(err)
	}
	return PublishOutput{
		CatalogID: e.ID, Slug: s.Name, Version: in.Version, Visibility: string(vis),
		Files: len(s.Files), Bytes: len(payload),
		Message: fmt.Sprintf("Published %s %s as %s.", s.Name, in.Version, vis),
	}, nil
}

func installFailure(detail string) string {
	switch detail {
	case sk.DetailNameConflict:
		return "a folder with this skill's name already exists in your skills directory and was not created by Kenaz, so it was left untouched"
	case sk.DetailUnsupportedTool:
		return "this older Kenaz skill runs a Kenaz tool, which other harnesses don't have, so it can't be installed"
	case sk.DetailInvalidPayload:
		return "this skill's files failed validation, so nothing was written"
	case sk.DetailNoRoots:
		return "no supported agent harness was found in your home directory"
	default:
		return "the skill could not be installed (" + detail + ")"
	}
}

func friendly(err error) error {
	switch {
	case errors.Is(err, fleetapi.ErrNotSignedIn):
		return errors.New("you are not signed in to Kameas Fleet; run `kenaz login`")
	case errors.Is(err, fleetapi.ErrUnofficialBuild):
		return fleetapi.ErrUnofficialBuild
	}
	if cat, ok := fleetapi.OrgPaused(err); ok {
		return fmt.Errorf("your organization's Kameas Fleet access is paused (%s); try again later", cat)
	}
	return err
}
