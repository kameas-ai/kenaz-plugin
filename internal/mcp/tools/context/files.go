package contexttools

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"time"
	"unicode/utf8"

	sdk "github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/kameas-ai/kenaz-plugin/fleetapi"
	"github.com/kameas-ai/kenaz-plugin/internal/approvals"
	"github.com/kameas-ai/kenaz-plugin/internal/contextlib"
	"github.com/kameas-ai/kenaz-plugin/internal/mcp"
	"github.com/kameas-ai/kenaz-plugin/internal/usertext"
	"github.com/kameas-ai/kenaz-plugin/pkg/secretscan"
)

// The file tools. They are part of the plugin's public surface.
const (
	ToolShareFile = "context_share_file"
	ToolPullFiles = "context_pull_files"
)

// ShareFileOperation is the approval operation for sharing a local file.
const ShareFileOperation = "context.share_file"

// Size limits. A file reaches Kameas Fleet as one JSON string and Fleet refuses a
// node over 1 MiB, so what counts is the encoded size: a file of quotes and line
// breaks takes twice its size, and < or > six times.
const (
	// MaxShareFileBytes is the largest file that can be shared.
	MaxShareFileBytes = 512 << 10
	// maxEncodedNodeBytes keeps a little under Fleet's limit.
	maxEncodedNodeBytes = 1<<20 - 1<<10
)

// Refusals of a file share, on top of the codes in gate.go.
const (
	CodeNotYours        = "not_yours"
	CodeTeamMismatch    = "team_mismatch"
	CodeFileTooLarge    = "too_large"
	CodeFileNotReadable = "not_readable"
	CodeLedger          = "ledger_failed"
	CodeHiddenText      = "hidden_text"
	CodeOtherAccount    = "other_account"
	CodeNoteReference   = contextlib.RuleNoteReference
)

// Machine codes an approved file share can fail with. Each leaves Fleet as it was.
const (
	failNodeChanged  = "node_changed"
	failNodeGone     = "node_gone"
	failNodePromoted = "node_promoted"
	failScopeChanged = "scope_changed"
)

// FilesOptions are the file tools' dependencies.
type FilesOptions struct {
	PersonalDir string
	// LedgerDir keeps which Fleet node each local file is linked to.
	LedgerDir   string
	Workdir     func() string
	DefaultTeam func() string
	Now         func() time.Time
	Rand        io.Reader
	// Viewer is the signed-in account, or nil, whose team folders can be used.
	Viewer func() *contextlib.Account
}

func (o FilesOptions) set() contextlib.Set {
	set := contextlib.Open(o.PersonalDir, o.Workdir())
	if o.Viewer != nil {
		set = set.WithViewer(o.Viewer())
	}
	return set
}

// NewFilesGroup returns the tools that move context files between this device
// and Kameas Fleet. They are a separate group so that a build without the context
// library does not serve them.
func NewFilesGroup(o FilesOptions) mcp.Group {
	base := Options{DefaultTeam: o.DefaultTeam, Now: o.Now, Rand: o.Rand}
	if base.DefaultTeam == nil {
		base.DefaultTeam = configuredDefaultTeam
	}
	if base.Now == nil {
		base.Now = time.Now
	}
	if base.Rand == nil {
		base.Rand = defaultRand()
	}
	o.DefaultTeam, o.Now, o.Rand = base.DefaultTeam, base.Now, base.Rand
	if o.Workdir == nil {
		o.Workdir = func() string {
			if d := os.Getenv("CLAUDE_PROJECT_DIR"); d != "" {
				return d
			}
			d, _ := os.Getwd()
			return d
		}
	}
	return mcp.GroupFunc(func(s *sdk.Server, d mcp.Deps) { registerFiles(s, d, o) })
}

// ShareFileInput is context_share_file's input.
type ShareFileInput struct {
	Path string `json:"path" jsonschema:"the file to share, a path from context_list such as personal/company.md"`
	Team string `json:"team,omitempty" jsonschema:"team name or id; defaults to your configured or only team. A file already shared stays with its team."`
	Kind string `json:"kind,omitempty" jsonschema:"one of guidance, procedure, fact, glossary_term, doc (default)"`
	// AsNew shares a copy under this person's own name.
	AsNew bool `json:"as_new,omitempty" jsonschema:"share as a new file of your own instead of updating the one this copy came from"`
	// FromMemory also shows the approval page's warning for a file context_write
	// did not mark.
	FromMemory bool `json:"from_memory,omitempty" jsonschema:"true when any of the text was drafted from the user's memory notes"`
}

// ShareFileOutput is context_share_file's result. Nothing is uploaded when the
// tool returns: status is "pending" until the user approves at approve_url.
type ShareFileOutput struct {
	RequestID  string `json:"request_id,omitempty"`
	Status     string `json:"status,omitempty"`
	ApproveURL string `json:"approve_url,omitempty"`
	Message    string `json:"message,omitempty"`
	ID         string `json:"id,omitempty"`
	Path       string `json:"path,omitempty"`
	Slug       string `json:"slug,omitempty"`
	Team       string `json:"team,omitempty"`
	Update     bool   `json:"update,omitempty"`
	// Findings say where to look when a credential stopped the share, in this
	// result only: the kind and the line, never the text.
	Findings []secretscan.Finding `json:"findings,omitempty"`
	Error    *ToolError           `json:"error,omitempty"`
}

// shareFilePayload is what the user approves and the relay carries out. Only
// Node reaches Fleet; the rest stays on this device.
type shareFilePayload struct {
	Node NodeInput `json:"node"`
	// Path and LedgerKey say which local file this is, for the ledger.
	Path      string `json:"path"`
	LedgerKey string `json:"ledger_key"`
	// Update says Node replaces a file already on Fleet, which must still hold
	// the text with BaseSHA256.
	Update     bool   `json:"update,omitempty"`
	BaseSHA256 string `json:"base_sha256,omitempty"`
	// FromMemory shows the approval page's warning.
	FromMemory bool `json:"from_memory,omitempty"`
}

func registerFiles(s *sdk.Server, d mcp.Deps, o FilesOptions) {
	var st StatusSource
	if d.Status != nil {
		st = d.Status
	}
	touch := func() {
		if d.Touch != nil {
			d.Touch()
		}
	}
	sdk.AddTool(s, &sdk.Tool{
		Name:  ToolShareFile,
		Title: "Share a context file with your team",
		Description: "Prepares sharing one local context file, picked by the user, with a team in Kameas Fleet, or updating the copy already shared. " +
			"Only share files the user asked to share. Never include secrets. " +
			usertext.ToolApproval + " " +
			"The file reaches Fleet under a short name shown on that page, never its path on this device.",
		Annotations: &sdk.ToolAnnotations{Title: "Share a context file with your team", DestructiveHint: ptr(true), OpenWorldHint: ptr(true)},
		Meta:        sdk.Meta{MetaRequiresUserInteraction: true},
	}, func(ctx context.Context, _ *sdk.CallToolRequest, in ShareFileInput) (*sdk.CallToolResult, ShareFileOutput, error) {
		touch()
		out := runShareFile(ctx, d.Provider, d.Approvals, st, o, in)
		return errResult(out.Error), out, nil
	})

	sdk.AddTool(s, &sdk.Tool{
		Name:  ToolPullFiles,
		Title: "Save team files to this device",
		Description: "Save files your team shared in Kameas Fleet as local working copies under ~/.kenaz/team-contexts, when the user asks. " +
			"Give ids from context_search, or no ids for all the team and organization files you can see, where this version supports it; a long list comes in parts, so pass next_cursor as cursor to continue. Changes nothing on Fleet. " +
			"A copy with local changes is left as it is unless overwrite is set; what overwrite replaces stays in the folder's .trash for 30 days. " +
			"Read a copy with context_read. " + contextlib.TeamNotice,
		Annotations: &sdk.ToolAnnotations{Title: "Save team files to this device", DestructiveHint: ptr(true), OpenWorldHint: ptr(true)},
	}, func(ctx context.Context, _ *sdk.CallToolRequest, in PullFilesInput) (*sdk.CallToolResult, PullFilesOutput, error) {
		touch()
		out := runPullFiles(ctx, d.Provider, st, o, in)
		return errResult(out.Error), out, nil
	})
}

func runShareFile(ctx context.Context, p fleetapi.Provider, ap approvals.Preparer, st StatusSource, o FilesOptions, in ShareFileInput) ShareFileOutput {
	var out ShareFileOutput
	sess, terr := Check(ctx, p, st, OpShare)
	if terr == nil {
		terr = RequireContextFiles(sess.Caps)
	}
	if terr != nil {
		out.Error = terr
		return out
	}
	acct, err := p.Auth().Session(ctx)
	if err != nil {
		out.Error = MapError(err)
		return out
	}
	set := o.set()
	loc, err := set.Locate(in.Path)
	if err != nil {
		out.Error = fileError(err)
		return out
	}
	pg, err := loc.Library.Read(loc.Rel, 0, contextlib.MaxFileBytes)
	if err != nil {
		out.Error = fileError(err)
		return out
	}
	switch {
	case pg.Size > MaxShareFileBytes:
		out.Error = &ToolError{Code: CodeFileTooLarge, Message: fmt.Sprintf("Only files up to %d KiB can be shared.", MaxShareFileBytes>>10)}
		return out
	case strings.TrimSpace(pg.Content) == "":
		out.Error = invalid("this file is empty")
		return out
	case contextlib.HashOf([]byte(pg.Content)) != pg.SHA256 || strings.IndexByte(pg.Content, 0) >= 0:
		// Read cleans up what isn't text, so what would be shared isn't the file.
		out.Error = &ToolError{Code: CodeFileNotReadable, Message: "This file isn't plain text, so it can't be shared."}
		return out
	}
	if f, terr := checkShareText(loc.Rel, pg.Content); terr != nil {
		out.Findings, out.Error = f, terr
		return out
	}
	slug, err := contextlib.SlugOf(loc.Rel)
	if err != nil {
		out.Error = invalid("this file's name can't be turned into a short name; rename it and try again")
		return out
	}
	kind := in.Kind
	if kind == "" {
		kind = "doc"
	}
	if !validShareKind(kind) {
		out.Error = invalid("kind must be one of " + strings.Join(ShareKinds, ", "))
		return out
	}
	led := contextlib.NewLedger(o.LedgerDir)
	key, err := loc.Library.Key(loc.Rel)
	if err != nil {
		out.Error = fileError(err)
		return out
	}
	rec, err := led.Get(key)
	if err != nil {
		out.Error = ledgerError()
		return out
	}
	acctKey := contextlib.Account{Env: string(acct.Env), OrgID: acct.OrgID, UserID: acct.UserID}
	link, linked := rec.LinkFor(acctKey)
	if !linked && loc.Library.Source() == contextlib.SourceTeam {
		// A working copy pulled by another account or environment, whose team text
		// must not reach this account's teams, even as a new file.
		out.Error = &ToolError{Code: CodeOtherAccount, Message: "This copy belongs to another account on this device, so it can't be shared from this one."}
		return out
	}
	if in.AsNew {
		linked, link = false, contextlib.Link{}
	}
	update := linked
	if update && !link.Mine(sess.Me.UserID) {
		// Fleet lets only a file's author change it, and refuses anyone else.
		out.Error = &ToolError{Code: CodeNotYours, Message: "This copy came from a file on your team's Kameas Fleet that isn't known to be yours, and Kameas Fleet lets only the author change a file. Nothing was shared.",
			Remedy: "To share your version as a new file of your own, call this again with as_new. The original stays as it is."}
		return out
	}

	held, reserved := rec.ReservationFor(acctKey)
	team, terr := pickTeam(in.Team, o.DefaultTeam(), sess.Me.Teams, link, held, update)
	if terr != nil {
		out.Error = terr
		return out
	}
	nodeID := link.NodeID
	if !update {
		nodeID = held.NodeID
		// A held id can be on Fleet already: the relay pushed and then couldn't write
		// the ledger. A new copy then needs its own id, and so does one that can't be
		// checked. The worst of guessing wrong is a second file.
		if reserved && in.AsNew && mayBeOnFleet(ctx, p, held.NodeID) {
			reserved = false
		}
		if !reserved {
			if nodeID, err = NewUUIDv7(o.Now(), o.Rand); err != nil {
				out.Error = invalid(err.Error())
				return out
			}
		}
	}
	version := 1
	if update {
		version = link.Version + 1
	}
	node, err := BuildFileNode(ShareInput{Title: contextlib.TitleOf(loc.Rel, []byte(pg.Content)), Body: pg.Content, Kind: kind}, slug, team.ID, nodeID, version)
	if err != nil {
		out.Error = invalid(err.Error())
		return out
	}
	encoded, err := json.Marshal(node)
	if err != nil {
		out.Error = invalid(err.Error())
		return out
	}
	if len(encoded) > maxEncodedNodeBytes {
		out.Error = &ToolError{Code: CodeFileTooLarge, Message: "This file is too large to share once encoded for Kameas Fleet, usually because it has many quotes, line breaks or angle brackets."}
		return out
	}
	payload := shareFilePayload{Node: node, Path: pg.Path, LedgerKey: key, Update: update, FromMemory: in.FromMemory || rec.DraftedFromMemory}
	if update {
		payload.BaseSHA256 = link.BaseSHA256
	}
	raw, err := json.Marshal(payload)
	if err != nil {
		out.Error = invalid(err.Error())
		return out
	}
	// The new file's id is held before the request exists, so that asking twice
	// can't make two files, whichever request the user approves. Links stay as they
	// are until a share has happened: an abandoned request must change nothing.
	if !update {
		err := led.Update(key, func(r *contextlib.FileRecord) {
			r.Path = pg.Path
			r.Reserve(contextlib.Reservation{Account: acctKey, NodeID: nodeID, TeamID: team.ID})
		})
		if err != nil {
			out.Error = ledgerError()
			return out
		}
	}
	pend, terr := submit(ctx, p, ap, approvals.Request{
		Operation: ShareFileOperation, TargetIDs: []string{"team:" + team.Name, "file:" + slug},
		Payload: raw, IdempotencyKey: ShareFileOperation + ":" + nodeID + ":" + contextlib.HashOf(raw)[:32],
		Summary: fmt.Sprintf("Share the file %q with the team %q in Kameas Fleet.", slug, team.Name),
		Impact:  "Every member of " + team.Name + " will be able to read this file.",
	})
	if terr != nil {
		out.Error = terr
		return out
	}
	out.RequestID, out.Status, out.ApproveURL, out.Message = pend.RequestID, string(pend.Status), pend.ApproveURL, pend.Message
	out.ID, out.Path, out.Slug, out.Team, out.Update = nodeID, pg.Path, slug, team.Name, update
	return out
}

// checkShareText is every check a file's text passes before it can leave the device:
// the name, credentials, hidden characters and note ids. It gives the findings, with
// kind and line only, and the refusal.
func checkShareText(rel, content string) ([]secretscan.Finding, *ToolError) {
	if f := contextlib.ScanName(rel); len(f) > 0 {
		return f, &ToolError{Code: CodeCredentialDetected, Message: "Nothing was shared: this file's name looks like it holds a credential.", Remedy: "Rename it and try again."}
	}
	// This scan only shows the person where to look, in this result. Fleet scans
	// again on arrival and decides; lines never go to Fleet, telemetry or logs.
	if f := contextlib.ScanText(content, true); len(f) > 0 {
		if contextlib.HiddenOnly(f) {
			// The approval page would show these as nothing.
			return f, &ToolError{Code: CodeHiddenText, Message: "Nothing was shared: this has characters a reader can't see on the lines listed.", Remedy: "Remove them and try again."}
		}
		return f, &ToolError{Code: CodeCredentialDetected, Message: "Nothing was shared: this looks like it holds a credential on the lines listed.", Remedy: "Remove it and try again."}
	}
	// A note id added since the draft, or to a file never marked, must not reach the
	// approval page either: it means nothing to a teammate and points back to a note.
	if f := contextlib.FindNoteRefs(content); len(f) > 0 {
		return f, &ToolError{Code: CodeNoteReference, Message: "Nothing was shared: this names a memory note on the lines listed.", Remedy: "Remove the note ids and try again."}
	}
	return nil, nil
}

// mayBeOnFleet reports whether node id exists on Fleet, or whether that can't be told.
func mayBeOnFleet(ctx context.Context, p fleetapi.Provider, id string) bool {
	nodes, err := p.Context().Pull(ctx, fleetapi.PullRequest{IDs: []string{id}})
	return err != nil || len(nodes) > 0
}

// pickTeam returns the team a file goes to. A file already shared stays with its
// team: moving it would change who can read it, which needs its own decision.
func pickTeam(want, def string, teams []fleetapi.TeamRef, link contextlib.Link, held contextlib.Reservation, shared bool) (fleetapi.TeamRef, *ToolError) {
	if !shared {
		// Asked before and not approved yet: the same team, unless they say otherwise.
		if want == "" && held.TeamID != "" {
			want = held.TeamID
		}
		return ResolveTeam(want, def, teams)
	}
	for _, t := range teams {
		if t.ID != link.TeamID {
			continue
		}
		if want != "" && !strings.EqualFold(want, t.Name) && want != t.ID {
			return fleetapi.TeamRef{}, &ToolError{Code: CodeTeamMismatch, Message: "This file is already shared with " + t.Name + ", so it stays with that team.", Remedy: "To share it with another team too, call this again with as_new."}
		}
		return t, nil
	}
	return fleetapi.TeamRef{}, &ToolError{Code: CodeTeamMismatch, Message: "This file is shared with a team you aren't in any more, so it can't be updated.", Remedy: "To share it with one of your teams, call this again with as_new."}
}

// BuildFileNode is the node for a shared file: the same fixed shape as a shared
// note, plus the file's short name in metadata. It carries no path. A file keeps
// its id across updates, and only the version moves.
func BuildFileNode(in ShareInput, slug, teamID, id string, version int) (NodeInput, error) {
	title := strings.TrimSpace(in.Title)
	switch {
	case title == "":
		return NodeInput{}, errors.New("title is required")
	case utf8.RuneCountInString(title) > MaxTitleLen:
		return NodeInput{}, fmt.Errorf("title is longer than %d characters", MaxTitleLen)
	case strings.TrimSpace(in.Body) == "":
		return NodeInput{}, errors.New("body is required")
	case len(in.Body) > MaxShareFileBytes:
		return NodeInput{}, fmt.Errorf("body is larger than %d KiB", MaxShareFileBytes>>10)
	case !utf8.ValidString(title) || !utf8.ValidString(in.Body):
		return NodeInput{}, errors.New("title and body must be valid UTF-8")
	case !validShareKind(in.Kind):
		return NodeInput{}, fmt.Errorf("kind must be one of %s", strings.Join(ShareKinds, ", "))
	case teamID == "" || id == "":
		return NodeInput{}, errors.New("team and id are required")
	case !contextlib.ValidSlug(slug):
		return NodeInput{}, errors.New("invalid short name")
	}
	return NodeInput{
		ID: id, Kind: in.Kind, Title: title, Body: in.Body,
		Metadata:       map[string]any{MetadataSlug: slug},
		Classification: string(fleetapi.ClassificationTeamShared),
		TeamID:         teamID, Version: version,
		UnitKind: "doc", UnitScope: "global", LoadPolicy: "on_demand",
	}, nil
}

// MetadataSlug is the node metadata key for a file's short name.
const MetadataSlug = "slug"

// slugOf returns the short name of a shared-file node, refusing any other
// metadata: this is what the approval page shows and all that may be sent.
func slugOf(n NodeInput) (string, error) {
	s, _ := n.Metadata[MetadataSlug].(string)
	if len(n.Metadata) != 1 || !contextlib.ValidSlug(s) {
		return "", errors.New("not a shared file")
	}
	return s, nil
}

func decodeShareFile(payload []byte) (shareFilePayload, string, error) {
	var sp shareFilePayload
	if err := json.Unmarshal(payload, &sp); err != nil {
		return sp, "", err
	}
	n := sp.Node
	slug, err := slugOf(n)
	if err != nil {
		return sp, "", err
	}
	if n.ID == "" || n.TeamID == "" || n.Title == "" || n.Version < 1 || n.Classification != string(fleetapi.ClassificationTeamShared) ||
		n.LoadPolicy != "on_demand" || n.UnitKind != "doc" || n.UnitScope != "global" || !validShareKind(n.Kind) || sp.LedgerKey == "" || sp.Path == "" {
		return sp, "", errors.New("not a shared file")
	}
	return sp, slug, nil
}

// FromMemoryBanner is shown on the approval page only, for text drawn from the
// person's private notes. Teammates never see it.
func FromMemoryBanner(team string) string {
	return "Drafted from your private memory. Approving makes this text visible to " + team +
		", including anything in it that came from your private notes. Check it for personal details, other people's information and anything shared with you in confidence. " +
		"Your notes stay private, but deleting one later won't remove this text."
}

// RenderShareFile describes a file share for the approval page from the payload
// alone: the short name, who can read it and the exact text.
func RenderShareFile(teamName func(id string) string) approvals.Renderer {
	return func(payload []byte) (approvals.Rendered, error) {
		sp, slug, err := decodeShareFile(payload)
		if err != nil {
			return approvals.Rendered{}, err
		}
		n := sp.Node
		name := ""
		if teamName != nil {
			name = teamName(n.TeamID)
		}
		team, shown := n.TeamID, "this team"
		if name != "" {
			team, shown = name+" ("+n.TeamID+")", name
		}
		verb := "Share the file"
		if sp.Update {
			verb = "Update the shared file"
		}
		impact := "Every member of " + team + " will be able to read this file. Kameas Fleet stores it under the short name " + slug + ", never its full path on your device."
		if sp.FromMemory {
			impact = FromMemoryBanner(shown) + "\n\n" + impact
		}
		return approvals.Rendered{
			Summary: fmt.Sprintf("%s %q, titled %q, with the team %s in Kameas Fleet.", verb, slug, n.Title, team),
			Impact:  impact,
			Targets: []string{"team:" + n.TeamID, "node:" + n.ID, "file:" + slug},
			Details: []approvals.Detail{{Target: "node:" + n.ID, Label: n.Title, Text: n.Body}},
		}, nil
	}
}

// ExecuteShareFile returns the approval executor for ShareFileOperation. The
// relay runs it only after the user approved this exact text. It rechecks the
// user's standing, then pushes only if Fleet still holds what the user saw, so
// that a newer version from another device is never overwritten.
func ExecuteShareFile(p fleetapi.Provider, st StatusSource, led contextlib.Ledger) approvals.Executor {
	return func(ctx context.Context, payload []byte, _ string) ([]byte, error) {
		sp, _, err := decodeShareFile(payload)
		if err != nil {
			return nil, approvals.ErrorCode("invalid_payload")
		}
		sess, terr := Check(ctx, p, st, OpShare)
		if terr == nil {
			terr = RequireContextFiles(sess.Caps)
		}
		if terr != nil {
			return nil, approvals.ErrorCode(terr.Code)
		}
		if err := recheckSession(ctx, p); err != nil {
			return nil, err
		}
		member := false
		for _, t := range sess.Me.Teams {
			member = member || t.ID == sp.Node.TeamID
		}
		if !member {
			return nil, approvals.ErrorCode(failScopeChanged)
		}
		cur, found, err := fileNode(ctx, p, sp.Node)
		if err != nil {
			// Nothing has been sent, so this is a plain failure and the person can try
			// again, not an outcome to go on checking.
			return nil, approvals.ErrorCode(MapError(err).Code)
		}
		switch {
		case found && cur.Node.Deleted:
			// Taken down: a push would bring it back.
			return nil, approvals.ErrorCode(failNodeGone)
		case found && cur.Node.Classification != fleetapi.ClassificationTeamShared:
			// A push would ask to move it back to the team.
			return nil, approvals.ErrorCode(failNodePromoted)
		case found && sameFile(cur, sp.Node):
			// An earlier try reached Fleet: nothing left to do.
			return recordShared(ctx, p, led, sp), nil
		case sp.Update && !found:
			return nil, approvals.ErrorCode(failNodeGone)
		case found && (!sp.Update || contextlib.HashOf([]byte(cur.Body)) != sp.BaseSHA256):
			return nil, approvals.ErrorCode(failNodeChanged)
		}
		if _, err := p.Context().Push(ctx, ToPushRequest(sp.Node)); err != nil {
			return nil, writeErr(err)
		}
		return recordShared(ctx, p, led, sp), nil
	}
}

// ReconcileShareFile reports whether an interrupted file share reached Fleet: the
// node holds the approved text. An older text means it did not, so the relay
// retries, and the executor decides again.
func ReconcileShareFile(p fleetapi.Provider, led contextlib.Ledger) approvals.Reconciler {
	return func(ctx context.Context, payload []byte, _ string) ([]byte, bool, error) {
		sp, _, err := decodeShareFile(payload)
		if err != nil {
			return nil, false, err
		}
		cur, found, err := fileNode(ctx, p, sp.Node)
		if err != nil {
			return nil, false, err
		}
		if !found || cur.Node.Deleted || !sameFile(cur, sp.Node) {
			return nil, false, nil
		}
		return recordShared(ctx, p, led, sp), true, nil
	}
}

func fileNode(ctx context.Context, p fleetapi.Provider, n NodeInput) (fleetapi.NodeContent, bool, error) {
	nodes, err := p.Context().Pull(ctx, fleetapi.PullRequest{IDs: []string{n.ID}})
	if err != nil {
		return fleetapi.NodeContent{}, false, err
	}
	for _, c := range nodes {
		if c.Node.ID == n.ID {
			return c, true, nil
		}
	}
	return fleetapi.NodeContent{}, false, nil
}

func sameFile(cur fleetapi.NodeContent, n NodeInput) bool {
	return cur.Body == n.Body && cur.Node.Title == n.Title && cur.Node.TeamID == n.TeamID
}

// recordShared links the file to what is now on Fleet. The memory mark goes only
// if the text just approved, with the warning, is the marked text: an older request
// approved late says nothing about what the file holds now. A failure here is not a failed share, and the
// result says so; the next share of the file then stops at node_changed rather
// than overwrite anything.
func recordShared(ctx context.Context, p fleetapi.Provider, led contextlib.Ledger, sp shareFilePayload) []byte {
	res := map[string]any{"id": sp.Node.ID, "classification": sp.Node.Classification}
	acct, err := p.Auth().Session(ctx)
	if err == nil {
		err = led.Update(sp.LedgerKey, func(r *contextlib.FileRecord) {
			r.Path = sp.Path
			if sp.FromMemory && r.DraftedSHA256 != "" && r.DraftedSHA256 == contextlib.HashOf([]byte(sp.Node.Body)) {
				r.DraftedFromMemory, r.DraftedSHA256 = false, ""
			}
			who := contextlib.Account{Env: string(acct.Env), OrgID: acct.OrgID, UserID: acct.UserID}
			r.Release(who, sp.Node.ID)
			r.SetLink(contextlib.Link{
				Account: who,
				NodeID:  sp.Node.ID, TeamID: sp.Node.TeamID, Version: sp.Node.Version,
				BaseSHA256: contextlib.HashOf([]byte(sp.Node.Body)), Authored: true,
			})
		})
	}
	if err != nil {
		res["local_record"] = "failed"
	}
	b, err := json.Marshal(res)
	if err != nil {
		return nil
	}
	return b
}

func ledgerError() *ToolError {
	return &ToolError{Code: CodeLedger, Message: "Kenaz couldn't keep track of this file on this device, so nothing was shared."}
}

// fileError gives a stable code and a fixed message: never an absolute path.
func fileError(err error) *ToolError {
	for _, c := range []struct {
		err       error
		code, msg string
	}{
		{contextlib.ErrInvalidPath, "invalid_path", "Use a path from context_list, such as personal/company.md."},
		{contextlib.ErrOtherOwner, CodeOtherAccount, "This folder belongs to another account on this device."},
		{contextlib.ErrFolderNeedsSignIn, CodeNotSignedIn, "Sign in with `kenaz login` to use your team's files."},
		{contextlib.ErrNoProject, "no_project", "This session is not in a repository, so there are no project files."},
		{contextlib.ErrUnsupportedExt, "unsupported_type", "Only .md, .markdown and .txt files can be shared."},
		{contextlib.ErrFileTooLarge, CodeFileTooLarge, "This file is over the 1 MiB limit."},
		{contextlib.ErrSymlinkEscape, "outside_library", "This path leads outside the context folder, so it was not read."},
		{contextlib.ErrNotFound, "not_found", "No such context file. Use context_list to see what exists."},
		{contextlib.ErrIsDirectory, "is_directory", "That path is a folder."},
		{contextlib.ErrNotRegular, "not_a_file", "That path is not a regular file."},
	} {
		if errors.Is(err, c.err) {
			return &ToolError{Code: c.code, Message: c.msg}
		}
	}
	return &ToolError{Code: CodeFileNotReadable, Message: "The file could not be read."}
}
