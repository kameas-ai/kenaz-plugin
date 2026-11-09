package contexttools

import (
	"context"
	"errors"
	"path"
	"strings"
	"time"

	"github.com/kameas-ai/kenaz-plugin/fleetapi"
	"github.com/kameas-ai/kenaz-plugin/internal/contextlib"
)

// Limits on one pull.
const (
	// MaxPullFiles is about the most working copies one call saves, so that a full
	// pull can't fill a session with results. The rest come with next_cursor.
	MaxPullFiles = 200
	listPageSize = 100
	// maxListPages bounds the calls one pull makes, however little of each page a
	// team filter keeps. The rest come with next_cursor.
	maxListPages = 10
)

// Pulled-file statuses.
const (
	PullSaved        = "saved"         // a new working copy
	PullUpdated      = "updated"       // the copy took Fleet's newer text
	PullUnchanged    = "unchanged"     // it already holds Fleet's text
	PullLocalChanges = "local_changes" // left as is: it has changes of its own
	PullAlreadyLocal = "already_local" // this file came from here, so it stays where it is
	PullGone         = "gone"          // no longer on Fleet: the local copy stays
	PullSkipped      = "skipped"       // can't be saved as a local file
)

// CodeListUnavailable means this version can't list a team's files.
const CodeListUnavailable = "list_unavailable"

// UnsafeRemovalMessage is shown with a local copy of a file Kameas Fleet removed
// over its content. The local copy is the person's to deal with: a pull never
// deletes it.
const UnsafeRemovalMessage = "This file was removed from Kameas Fleet because it may contain a secret or unsafe content. Delete or fix your copy."

// PullFilesInput is context_pull_files's input.
type PullFilesInput struct {
	IDs       []string `json:"ids,omitempty" jsonschema:"1 to 10 ids from context_search; leave out for all of the team's files"`
	Team      string   `json:"team,omitempty" jsonschema:"when listing, only this team's files, by name or id; leave out for everything you can see, including organization-wide files"`
	Cursor    string   `json:"cursor,omitempty" jsonschema:"when listing, next_cursor from the previous call"`
	Overwrite bool     `json:"overwrite,omitempty" jsonschema:"replace working copies that have local changes; the replaced text stays in .trash for 30 days"`
}

// PulledFile is one file's outcome.
type PulledFile struct {
	ID     string `json:"id"`
	Title  string `json:"title,omitempty"`
	Path   string `json:"path,omitempty"`
	Status string `json:"status"`
	// RemoteChanged says Fleet holds text this local file doesn't.
	RemoteChanged bool   `json:"remote_changed,omitempty"`
	UpdatedAt     string `json:"updated_at,omitempty"`
	Yours         bool   `json:"yours,omitempty"`
	// Message is for the person, to show as it is.
	Message string `json:"message,omitempty"`
}

// PullFilesOutput is context_pull_files's result.
type PullFilesOutput struct {
	Files   []PulledFile `json:"files"`
	Missing []string     `json:"missing,omitempty"`
	// NextCursor is set when a list has more: pass it as cursor to continue.
	NextCursor string     `json:"next_cursor,omitempty"`
	Trust      string     `json:"trust,omitempty"`
	Notice     string     `json:"notice,omitempty"`
	Error      *ToolError `json:"error,omitempty"`
}

func runPullFiles(ctx context.Context, p fleetapi.Provider, st StatusSource, o FilesOptions, in PullFilesInput) PullFilesOutput {
	out := PullFilesOutput{Files: []PulledFile{}, Trust: contextlib.SourceTeam.Trust(), Notice: contextlib.TeamNotice}
	sess, terr := Check(ctx, p, st, OpPull)
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
	var nodes []fleetapi.NodeContent
	if len(in.IDs) > 0 {
		req, err := BuildPull(in.IDs, "")
		if err != nil {
			out.Error = invalid(err.Error())
			return out
		}
		got, err := p.Context().Pull(ctx, req)
		if err != nil {
			out.Error = MapError(err)
			return out
		}
		byID := map[string]fleetapi.NodeContent{}
		for _, n := range got {
			byID[n.Node.ID] = n
		}
		for _, id := range req.IDs {
			if n, ok := byID[id]; ok {
				nodes = append(nodes, n)
			} else {
				nodes = append(nodes, fleetapi.NodeContent{Node: fleetapi.ContextNode{ID: id, Deleted: true}})
			}
		}
	} else {
		var terr *ToolError
		nodes, out.NextCursor, terr = listTeamFiles(ctx, p, in.Team, in.Cursor, sess.Me.Teams)
		if terr != nil {
			out.Error = terr
			return out
		}
	}
	w := pullWriter{
		set: o.set(), led: contextlib.NewLedger(o.LedgerDir), overwrite: in.Overwrite, me: sess.Me.UserID, teams: sess.Me.Teams,
		acct: contextlib.Account{Env: string(acct.Env), OrgID: acct.OrgID, UserID: acct.UserID},
	}
	for _, n := range nodes {
		f, known := w.save(n)
		if !known {
			// A list also carries tombstones, which are only news for a local copy.
			if len(in.IDs) > 0 {
				out.Missing = append(out.Missing, n.Node.ID)
			}
			continue
		}
		out.Files = append(out.Files, f)
	}
	return out
}

// listTeamFiles reads the documents the member can see, oldest change first, up
// to about MaxPullFiles from cursor, and returns where to continue. Fleet includes
// organization-wide nodes when listing one team, so naming a team is narrowed here.
// A build whose Fleet client can't list says so, and the caller can still pull by id.
func listTeamFiles(ctx context.Context, p fleetapi.Provider, wantTeam, cursor string, teams []fleetapi.TeamRef) ([]fleetapi.NodeContent, string, *ToolError) {
	lister, ok := p.Context().(fleetapi.ContextLister)
	if !ok {
		return nil, "", &ToolError{Code: CodeListUnavailable, Message: "This version of Kenaz can't list a team's files.", Remedy: "Find them with context_search and pass their ids."}
	}
	teamID := ""
	if strings.TrimSpace(wantTeam) != "" {
		t, terr := ResolveTeam(wantTeam, "", teams)
		if terr != nil {
			return nil, "", terr
		}
		teamID = t.ID
	}
	var nodes []fleetapi.NodeContent
	since := cursor
	for pages := 1; ; pages++ {
		page, err := lister.List(ctx, fleetapi.ListRequest{TeamID: teamID, Since: since, Limit: listPageSize})
		if err != nil {
			return nil, "", MapError(err)
		}
		for _, n := range page.Nodes {
			if teamID == "" || n.Node.TeamID == teamID {
				nodes = append(nodes, n)
			}
		}
		if page.Cursor == "" || page.Cursor == since || len(page.Nodes) == 0 {
			return nodes, "", nil
		}
		since = page.Cursor
		// A whole page at a time: the cursor can't point inside one.
		if len(nodes) >= MaxPullFiles || pages >= maxListPages {
			return nodes, since, nil
		}
	}
}

type pullWriter struct {
	set       contextlib.Set
	led       contextlib.Ledger
	acct      contextlib.Account
	overwrite bool
	me        string
	teams     []fleetapi.TeamRef
}

// save brings one node to this device. known is false for an id that neither Fleet
// nor this device knows. A local file with its own changes is never replaced
// unless the person asked, and a file that started here stays where it is.
func (w pullWriter) save(n fleetapi.NodeContent) (f PulledFile, known bool) {
	f = PulledFile{ID: n.Node.ID, Title: n.Node.Title}
	if !n.Node.UpdatedAt.IsZero() {
		f.UpdatedAt = n.Node.UpdatedAt.UTC().Format(time.RFC3339)
	}
	f.Yours = w.me != "" && n.Node.OwnerUserID == w.me
	key, rec, linked := w.led.FindNode(w.acct, n.Node.ID)
	link, _ := rec.LinkFor(w.acct)
	if n.Node.Deleted {
		if !linked {
			return f, false
		}
		f.Path, f.Status = rec.Path, PullGone
		if n.Node.RemovalReason == fleetapi.RemovalUnsafeContent {
			f.Message = UnsafeRemovalMessage
		}
		return f, true
	}
	if n.Node.Title == "" && n.Body == "" {
		f.Status = PullSkipped
		return f, true
	}
	hash := contextlib.HashOf([]byte(n.Body))
	f.Yours = f.Yours || link.Authored

	if linked && !strings.HasPrefix(rec.Path, string(contextlib.SourceTeam)+"/") {
		f.Path, f.Status, f.RemoteChanged = rec.Path, PullAlreadyLocal, hash != link.BaseSHA256
		return f, true
	}
	ref := rec.Path
	if !linked {
		var err error
		if ref, err = w.newRef(n); err != nil {
			f.Status = PullSkipped
			return f, true
		}
		if key, err = w.set.Key(ref); err != nil {
			f.Status = PullSkipped
			return f, true
		}
	}
	f.Path = ref

	opt := contextlib.WriteOptions{}
	status := PullSaved
	if local, err := w.set.Read(ref, 0, contextlib.MaxFileBytes); err == nil {
		switch {
		case local.SHA256 == hash:
			f.Status = PullUnchanged
			w.record(key, ref, n, hash, link)
			return f, true
		case local.SHA256 != link.BaseSHA256 && !w.overwrite:
			// Keep the record's base: a share from here must not pass for having
			// seen this newer text.
			f.Status, f.RemoteChanged = PullLocalChanges, hash != link.BaseSHA256
			return f, true
		}
		opt.Replace, status = true, PullUpdated
	} else if !errors.Is(err, contextlib.ErrNotFound) {
		f.Status = PullSkipped
		return f, true
	}
	if _, err := w.set.Write(ref, []byte(n.Body), opt); err != nil {
		f.Status = PullSkipped
		return f, true
	}
	w.record(key, ref, n, hash, link)
	f.Status = status
	return f, true
}

func (w pullWriter) record(key, ref string, n fleetapi.NodeContent, hash string, old contextlib.Link) {
	version := n.Node.Version
	if version == 0 {
		version = old.Version
	}
	// A failure here costs only the link: the next pull saves a second copy.
	_ = w.led.Update(key, func(r *contextlib.FileRecord) {
		r.Path = ref
		r.SetLink(contextlib.Link{
			Account: w.acct, NodeID: n.Node.ID, TeamID: n.Node.TeamID, Version: version,
			BaseSHA256: hash, Authored: old.Authored, OwnerUserID: n.Node.OwnerUserID,
		})
	})
}

// newRef picks a free path for a node's first copy: its short name when Fleet
// reports one, else its title, in a folder named for its team. A taken name gets
// part of the node's id.
func (w pullWriter) newRef(n fleetapi.NodeContent) (string, error) {
	label := "org"
	if n.Node.TeamID != "" {
		label = "team-" + shortID(n.Node.TeamID)
		for _, t := range w.teams {
			if t.ID == n.Node.TeamID {
				label = t.Name
			}
		}
	}
	folder, _, err := ResolveTeamFolder(w.set, w.led, contextlib.FolderOwner{Env: w.acct.Env, OrgID: w.acct.OrgID, UserID: w.acct.UserID, TeamID: n.Node.TeamID}, label)
	if err != nil {
		return "", err
	}
	name, _ := n.Node.Metadata[MetadataSlug].(string)
	if !contextlib.ValidSlug(name) {
		var err error
		if name, err = contextlib.SlugOf(contextlib.FolderName(n.Node.Title) + ".md"); err != nil {
			return "", err
		}
	}
	dir := string(contextlib.SourceTeam) + "/" + folder + "/"
	ref := dir + name
	if _, err := w.set.Read(ref, 0, 1); errors.Is(err, contextlib.ErrNotFound) {
		return ref, nil
	}
	ext := path.Ext(name)
	ref = dir + strings.TrimSuffix(name, ext) + "-" + shortID(n.Node.ID) + ext
	if _, err := w.set.Read(ref, 0, 1); !errors.Is(err, contextlib.ErrNotFound) {
		return "", errors.New("no free name")
	}
	return ref, nil
}

func shortID(id string) string {
	id = strings.ReplaceAll(id, "-", "")
	// UUIDv7 ids start with their timestamp, so the end tells them apart better.
	if len(id) > 8 {
		id = id[len(id)-8:]
	}
	return contextlib.FolderName(id)
}
