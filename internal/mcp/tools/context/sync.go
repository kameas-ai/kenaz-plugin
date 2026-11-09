package contexttools

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"path"
	"sort"
	"strings"

	"github.com/kameas-ai/kenaz-plugin/fleetapi"
	"github.com/kameas-ai/kenaz-plugin/internal/approvals"
	"github.com/kameas-ai/kenaz-plugin/internal/contextlib"
	"github.com/kameas-ai/kenaz-plugin/pkg/secretscan"
)

// `kenaz context sync` keeps one team's folder, ~/.kenaz/team-contexts/<team>/, and
// Kameas Fleet the same. It only runs from a person's terminal: no tool reaches it.

// SyncFile is a file on Kameas Fleet. Path is its name in the team's folder. Today's
// routes keep only a short name, so a file in a subfolder doesn't sync yet.
type SyncFile struct {
	ID, TeamID, Path, Title, Body string
	Version                       int
	Deleted                       bool
	RemovalReason                 string
}

// SyncPut writes a file. ExpectedVersion 0 means create.
type SyncPut struct {
	TeamID, Path, Title, Body string
	ExpectedVersion           int
	Local                     SyncLocal
}

// SyncLocal is what today's routes and this device's records need besides.
type SyncLocal struct {
	NodeID, BaseSHA256, Ref, LedgerKey string
	FromMemory                         bool
}

// SyncStore is what sync needs from Kameas Fleet. It follows the shape of the planned
// file routes, so that they can replace today's without touching the rest.
type SyncStore interface {
	// Files lists one team's files, with removals, from cursor. next is "" at the end.
	Files(ctx context.Context, teamID, cursor string) (files []SyncFile, next string, err error)
	// File reads one file, and found says whether it's there.
	File(ctx context.Context, id string) (f SyncFile, found bool, err error)
	// Put writes one file. Its errors carry an approvals code, or are of unknown outcome.
	Put(ctx context.Context, p SyncPut) error
}

// NodeStore is SyncStore over today's context routes, writing through the same
// executor as an approved context_share_file: same checks, same records.
func NodeStore(p fleetapi.Provider, st StatusSource, led contextlib.Ledger) SyncStore {
	return nodeStore{p: p, exec: ExecuteShareFile(p, st, led)}
}

type nodeStore struct {
	p    fleetapi.Provider
	exec approvals.Executor
}

func syncFileOf(n fleetapi.NodeContent) SyncFile {
	name, _ := n.Node.Metadata[MetadataSlug].(string)
	return SyncFile{ID: n.Node.ID, TeamID: n.Node.TeamID, Path: name, Title: n.Node.Title, Body: n.Body,
		Version: n.Node.Version, Deleted: n.Node.Deleted, RemovalReason: n.Node.RemovalReason}
}

func (s nodeStore) Files(ctx context.Context, teamID, cursor string) ([]SyncFile, string, error) {
	lister, ok := s.p.Context().(fleetapi.ContextLister)
	if !ok {
		return nil, "", &ToolError{Code: CodeListUnavailable, Message: "This version of Kenaz can't list a team's files."}
	}
	page, err := lister.List(ctx, fleetapi.ListRequest{TeamID: teamID, Since: cursor, Limit: listPageSize})
	if err != nil {
		return nil, "", MapError(err)
	}
	var out []SyncFile
	for _, n := range page.Nodes {
		// Fleet includes organization-wide files when listing one team.
		if n.Node.TeamID == teamID {
			out = append(out, syncFileOf(n))
		}
	}
	next := page.Cursor
	if next == cursor || len(page.Nodes) == 0 {
		next = ""
	}
	return out, next, nil
}

func (s nodeStore) File(ctx context.Context, id string) (SyncFile, bool, error) {
	nodes, err := s.p.Context().Pull(ctx, fleetapi.PullRequest{IDs: []string{id}})
	if err != nil {
		return SyncFile{}, false, MapError(err)
	}
	for _, n := range nodes {
		if n.Node.ID == id {
			return syncFileOf(n), true, nil
		}
	}
	return SyncFile{}, false, nil
}

func (s nodeStore) Put(ctx context.Context, p SyncPut) error {
	version := p.ExpectedVersion + 1
	node, err := BuildFileNode(ShareInput{Title: p.Title, Body: p.Body, Kind: "doc"}, p.Path, p.TeamID, p.Local.NodeID, version)
	if err != nil {
		return approvals.ErrorCode("invalid_payload")
	}
	raw, err := json.Marshal(shareFilePayload{Node: node, Path: p.Local.Ref, LedgerKey: p.Local.LedgerKey,
		Update: p.Local.BaseSHA256 != "", BaseSHA256: p.Local.BaseSHA256, FromMemory: p.Local.FromMemory})
	if err != nil {
		return approvals.ErrorCode("invalid_payload")
	}
	_, err = s.exec(ctx, raw, "")
	return err
}

// StableFileID is the id of a new file: the same for everyone in a team who adds a file
// of that name, so that two people's first syncs meet rather than make two files. It
// has the shape of a UUID, marked as custom, and names nothing.
func StableFileID(orgID, teamID, name string) string {
	sum := sha256.Sum256([]byte("kenaz-context-file\x00" + orgID + "\x00" + teamID + "\x00" + name))
	b := sum[:16]
	b[6] = 0x80 | (b[6] & 0x0f)
	b[8] = 0x80 | (b[8] & 0x3f)
	h := hex.EncodeToString(b)
	return h[0:8] + "-" + h[8:12] + "-" + h[12:16] + "-" + h[16:20] + "-" + h[20:32]
}

// Syncer is one sync of one team's folder.
type Syncer struct {
	Store SyncStore
	Set   contextlib.Set
	Led   contextlib.Ledger
	Acct  contextlib.Account
	Team  fleetapi.TeamRef

	folder string
	links  map[string]contextlib.LinkedFile
	// What Fleet holds, without the text: only the file being worked on is in memory.
	byID, byName map[string]syncSeen
	claimed      map[string]string
	gone         map[string]bool
	// touched says this run wrote a link.
	touched bool
}

type syncSeen struct {
	ID, Path, Hash string
	Version        int
}

// Folder is the team's folder name.
func (s *Syncer) Folder() string { return s.folder }

// SyncLegacy is what's left of an older folder: files that aren't sent, because they can't
// be claimed, and whether its owner's record can't be read.
type SyncLegacy struct {
	Folder     string
	Files      int
	Unreadable bool
	// Claim is set when this run made the folder this owner's.
	Claim *FolderClaim
}

// FolderClaim is a folder this run bound. Prior is its record before, if any, and HadFiles
// says files were already there.
type FolderClaim struct {
	Folder   string
	Prior    *contextlib.FolderOwner
	HadFiles bool
	Moved    bool
}

// ResolveTeamFolder finds the folder of one owner's team, making it if needed. A folder
// bound to anyone else is never used. An older folder with no record is taken unless a
// file in it is linked to someone else: then this owner's own files move to a new folder,
// and the rest stay where they are, reported and not sent.
func ResolveTeamFolder(set contextlib.Set, led contextlib.Ledger, owner contextlib.FolderOwner, name string) (string, SyncLegacy, error) {
	lib := set.Team
	want := contextlib.FolderName(name)
	if f, ok := lib.FindFolder(owner); ok {
		// An older folder at the team's name may have been reopened since: bring this
		// owner's files across, as when the folder was first made.
		moveProvable(set, led, owner, want, f)
		return f, legacyOf(set, led, owner, want), nil
	}
	candidates := []string{want, want + "-" + shortID(owner.TeamID), want + "-" + contextlib.FolderName(owner.TeamID)}
	for _, folder := range candidates {
		cur, bound := lib.FolderOwner(folder)
		if bound && !(cur.Unassigned && cur.Account() == owner.Account()) {
			continue
		}
		if !bound {
			if _, _, foreign := unbound(set, led, owner, folder); len(foreign) > 0 {
				continue
			}
		}
		claim := &FolderClaim{Folder: folder}
		if bound {
			prior := cur
			claim.Prior = &prior
		}
		_, claim.HadFiles = lib.FolderHasFiles(folder)
		if err := lib.BindFolder(folder, owner); err != nil {
			continue
		}
		claim.Moved = moveProvable(set, led, owner, want, folder)
		legacy := legacyOf(set, led, owner, want)
		legacy.Claim = claim
		return folder, legacy, nil
	}
	return "", SyncLegacy{}, errors.New("no free folder")
}

// unbound sorts the files in a folder with no record: this owner's by their links, those
// with no link at all, and those whose links say they're someone else's.
func unbound(set contextlib.Set, led contextlib.Ledger, owner contextlib.FolderOwner, folder string) (mine, free, foreign []contextlib.Entry) {
	if _, bound := set.Team.FolderOwner(folder); bound {
		return nil, nil, nil
	}
	files, _, _, err := set.Team.List(context.Background())
	if err != nil {
		return nil, nil, nil
	}
	for _, e := range files {
		if !strings.HasPrefix(e.Rel(), folder+"/") {
			continue
		}
		key, kerr := set.Team.Key(e.Rel())
		rec, rerr := led.Get(key)
		if kerr != nil || rerr != nil {
			foreign = append(foreign, e)
			continue
		}
		link, ok := rec.LinkFor(owner.Account())
		switch {
		case ok && link.TeamID == owner.TeamID:
			mine = append(mine, e)
		case ok || len(rec.Links) > 0:
			foreign = append(foreign, e)
		default:
			free = append(free, e)
		}
	}
	return mine, free, foreign
}

func legacyOf(set contextlib.Set, led contextlib.Ledger, owner contextlib.FolderOwner, folder string) SyncLegacy {
	if cur, bound := set.Team.FolderOwner(folder); bound {
		return SyncLegacy{Folder: folder, Unreadable: !cur.Readable()}
	}
	_, free, foreign := unbound(set, led, owner, folder)
	return SyncLegacy{Folder: folder, Files: len(free) + len(foreign)}
}

// moveProvable brings this owner's files out of an older folder into its own. A failure
// leaves the file where it was, and it's reported with the others.
func moveProvable(set contextlib.Set, led contextlib.Ledger, owner contextlib.FolderOwner, from, to string) (moved bool) {
	if from == to {
		return false
	}
	mine, _, _ := unbound(set, led, owner, from)
	source := string(contextlib.SourceTeam) + "/"
	for _, e := range mine {
		rel := to + "/" + strings.TrimPrefix(e.Rel(), from+"/")
		oldKey, err := set.Team.Key(e.Rel())
		if err != nil {
			continue
		}
		newKey, err := set.Team.Key(rel)
		if err != nil {
			continue
		}
		rec, err := led.Get(oldKey)
		if err != nil || set.Team.Move(e.Rel(), rel) != nil {
			continue
		}
		_ = led.Update(newKey, func(r *contextlib.FileRecord) {
			*r = rec
			r.Path = source + rel
		})
		_ = led.Update(oldKey, func(r *contextlib.FileRecord) { *r = contextlib.FileRecord{Path: r.Path} })
		moved = true
	}
	return moved
}

func (s *Syncer) ref(name string) string {
	return string(contextlib.SourceTeam) + "/" + s.Folder() + "/" + name
}

// SyncConflict says both sides changed a file: Fleet's text is at Name, and the
// person's is in Yours.
type SyncConflict struct{ Name, Yours string }

// SyncGone is a file removed on Kameas Fleet. The copy stays.
type SyncGone struct{ Name, Message string }

// PullResult is what coming down changed.
type PullResult struct {
	Saved, Updated int
	Conflicts      []SyncConflict
	Gone           []SyncGone
	// Removed are files deleted here that Fleet still holds.
	Removed []string
	// Failed are files that couldn't be brought down.
	Failed []string
	// Notes counts shared notes, which aren't files and stay on Fleet.
	Notes int
	// Legacy is an older folder with files nobody can be shown to own.
	Legacy SyncLegacy
}

// Pull brings Fleet's changes into the folder. It never loses text: a replaced file stays
// in the folder's .trash, and a file both sides changed is kept beside Fleet's.
func (s *Syncer) Pull(ctx context.Context) (PullResult, error) {
	var res PullResult
	acct := contextlib.FolderOwner{Env: s.Acct.Env, OrgID: s.Acct.OrgID, UserID: s.Acct.UserID, TeamID: s.Team.ID}
	folder, legacy, err := ResolveTeamFolder(s.Set, s.Led, acct, s.Team.Name)
	if err != nil {
		return res, err
	}
	s.folder, res.Legacy = folder, legacy
	s.byID, s.byName, s.claimed, s.gone = map[string]syncSeen{}, map[string]syncSeen{}, map[string]string{}, map[string]bool{}
	s.links = s.Led.LinksFor(s.Acct)
	// A page at a time: only a page's text is in memory.
	for cursor := ""; ; {
		page, next, err := s.Store.Files(ctx, s.Team.ID, cursor)
		if err != nil {
			return res, err
		}
		for _, f := range page {
			s.pullOne(f, &res)
		}
		if next == "" {
			break
		}
		cursor = next
	}
	sort.Strings(res.Removed)
	return res, nil
}

// freeName picks where a node's first copy goes: its short name, unless another file
// holds that, and then a name from its id, which then stays, by its link.
func (s *Syncer) freeName(f SyncFile) string {
	name := f.Path
	taken := s.claimed[name] != "" && s.claimed[name] != f.ID
	if !taken {
		if key, err := s.Set.Key(s.ref(name)); err == nil {
			if rec, err := s.Led.Get(key); err == nil {
				link, mine := rec.LinkFor(s.Acct)
				taken = (mine && link.NodeID != f.ID) || (!mine && len(rec.Links) > 0)
			}
		}
	}
	if taken {
		ext := path.Ext(name)
		name = strings.TrimSuffix(name, ext) + "-" + shortID(f.ID) + ext
	}
	s.claimed[name] = f.ID
	return name
}

func (s *Syncer) pullOne(f SyncFile, res *PullResult) {
	found, linked := s.links[f.ID]
	key, rec := found.Key, found.Rec
	link, _ := rec.LinkFor(s.Acct)
	if f.Deleted {
		if linked {
			s.gone[rec.Path] = true
			g := SyncGone{Name: path.Base(rec.Path)}
			if f.RemovalReason == fleetapi.RemovalUnsafeContent {
				g.Message = UnsafeRemovalMessage
			}
			res.Gone = append(res.Gone, g)
		}
		return
	}
	hash := contextlib.HashOf([]byte(f.Body))
	seen := syncSeen{ID: f.ID, Path: f.Path, Hash: hash, Version: f.Version}
	s.byID[f.ID] = seen
	// Shared from your own folder, or linked anywhere else: it stays there, and it isn't
	// part of this folder.
	if linked && !strings.HasPrefix(rec.Path, s.ref("")) {
		return
	}
	ref := rec.Path
	if !linked {
		if !contextlib.ValidSlug(f.Path) {
			res.Notes++
			return
		}
		name := s.freeName(f)
		if name == f.Path {
			s.byName[f.Path] = seen
		}
		ref = s.ref(name)
		var err error
		if key, err = s.Set.Key(ref); err != nil {
			res.Failed = append(res.Failed, f.Path)
			return
		}
	}
	name := strings.TrimPrefix(ref, s.ref(""))
	if s.Set.Team.IsLink(s.folder + "/" + name) {
		res.Failed = append(res.Failed, name)
		return
	}
	local, err := s.Set.Read(ref, 0, contextlib.MaxFileBytes)
	switch {
	case errors.Is(err, contextlib.ErrNotFound):
		if linked && hash == link.BaseSHA256 {
			res.Removed = append(res.Removed, name)
			return
		}
		if _, err := s.Set.Write(ref, []byte(f.Body), contextlib.WriteOptions{}); err != nil {
			res.Failed = append(res.Failed, name)
			return
		}
		res.Saved++
	case err != nil:
		res.Failed = append(res.Failed, name)
		return
	case local.SHA256 == hash:
	case linked && hash == link.BaseSHA256:
		return // changed only here: it goes up
	case linked && local.SHA256 == link.BaseSHA256:
		if _, err := s.Set.Write(ref, []byte(f.Body), contextlib.WriteOptions{Replace: true}); err != nil {
			res.Failed = append(res.Failed, name)
			return
		}
		res.Updated++
	default:
		c, err := s.keepBoth(ref, key, f.Body, local)
		if err != nil {
			res.Failed = append(res.Failed, name)
			return
		}
		res.Conflicts = append(res.Conflicts, c)
	}
	s.record(key, ref, f, hash, link)
}

// UndoClaim puts back a folder this run bound, if nothing has come to depend on it: no
// file was pulled or linked into it, and none moved. It says whether it did.
func (s *Syncer) UndoClaim(c *FolderClaim) bool {
	if c == nil || c.Moved || s.touched {
		return false
	}
	return s.Set.Team.RestoreFolderRecord(c.Folder, c.Prior) == nil
}

func (s *Syncer) record(key, ref string, f SyncFile, hash string, old contextlib.Link) {
	s.touched = true
	version := f.Version
	if version == 0 {
		version = old.Version
	}
	// Failing here costs only the link: the next sync finds the file by its name.
	_ = s.Led.Update(key, func(r *contextlib.FileRecord) {
		r.Path = ref
		r.SetLink(contextlib.Link{Account: s.Acct, NodeID: f.ID, TeamID: f.TeamID, Version: version, BaseSHA256: hash, Authored: old.Authored, OwnerUserID: old.OwnerUserID})
	})
}

// keepBoth saves the person's text beside the file, then puts Fleet's at its name.
func (s *Syncer) keepBoth(ref, key, fleetBody string, local contextlib.Page) (SyncConflict, error) {
	if contextlib.HashOf([]byte(local.Content)) != local.SHA256 {
		return SyncConflict{}, errors.New("not plain text")
	}
	dir, name := path.Split(ref)
	ext := path.Ext(name)
	stem := strings.TrimSuffix(name, ext)
	yours := ""
	for i := 1; ; i++ {
		yours = stem + " (your version)" + ext
		if i > 1 {
			yours = fmt.Sprintf("%s (your version %d)%s", stem, i, ext)
		}
		if _, err := s.Set.Read(dir+yours, 0, 1); errors.Is(err, contextlib.ErrNotFound) {
			break
		}
		if i > 99 {
			return SyncConflict{}, errors.New("no free name")
		}
	}
	if _, err := s.Set.Write(dir+yours, []byte(local.Content), contextlib.WriteOptions{}); err != nil {
		return SyncConflict{}, err
	}
	// Text drawn from private notes keeps that mark under its new name.
	if rec, err := s.Led.Get(key); err == nil && rec.DraftedFromMemory {
		if k, err := s.Set.Key(dir + yours); err == nil {
			_ = s.Led.Update(k, func(r *contextlib.FileRecord) {
				r.Path, r.DraftedFromMemory, r.DraftedSHA256 = dir+yours, true, rec.DraftedSHA256
			})
		}
	}
	if _, err := s.Set.Write(ref, []byte(fleetBody), contextlib.WriteOptions{Replace: true}); err != nil {
		return SyncConflict{}, err
	}
	return SyncConflict{Name: name, Yours: yours}, nil
}

// SyncItem is a file to send.
type SyncItem struct {
	Name  string
	Text  string
	Size  int64
	New   bool
	Drawn bool // drawn from private memory
	put   SyncPut
}

// SyncRefusal is a file the checks stopped, with where to look: kinds and lines only.
type SyncRefusal struct {
	Name     string
	Reason   string
	Findings []secretscan.Finding
}

// SyncPlan is what would go up.
type SyncPlan struct {
	Items     []SyncItem
	Unchanged int
	Refused   []SyncRefusal
	// InSubfolders counts files that don't sync yet, and OtherAccount those whose
	// copies belong to another account on this device.
	InSubfolders int
	OtherAccount []string
	// OtherTeam are copies linked to another of this account's teams.
	OtherTeam []string
	// RemovedOnFleet counts files here whose Fleet copy is gone, and which aren't sent.
	RemovedOnFleet int
	Truncated      bool
}

// FromMemory counts the files drawn from private memory.
func (p SyncPlan) FromMemory() int {
	n := 0
	for _, it := range p.Items {
		if it.Drawn {
			n++
		}
	}
	return n
}

// Plan reads the folder, after Pull, and works out what would go up. Only this team's
// folder is read: your own files, and a repository's, are never part of it.
func (s *Syncer) Plan(ctx context.Context) (SyncPlan, error) {
	var plan SyncPlan
	files, _, truncated, err := s.Set.Team.List(ctx)
	if err != nil {
		return plan, err
	}
	plan.Truncated = truncated
	prefix := s.Folder() + "/"
	names := map[string]string{}
	for _, e := range files {
		rel, ok := strings.CutPrefix(e.Rel(), prefix)
		if !ok {
			continue
		}
		if strings.Contains(rel, "/") {
			plan.InSubfolders++
			continue
		}
		s.planOne(e, rel, names, &plan)
	}
	sort.Slice(plan.Items, func(i, j int) bool { return plan.Items[i].Name < plan.Items[j].Name })
	return plan, nil
}

func (s *Syncer) planOne(e contextlib.Entry, name string, names map[string]string, plan *SyncPlan) {
	ref := s.ref(name)
	refuse := func(reason string, f []secretscan.Finding) {
		plan.Refused = append(plan.Refused, SyncRefusal{Name: name, Reason: reason, Findings: f})
	}
	if s.gone[ref] {
		plan.RemovedOnFleet++
		return
	}
	key, err := s.Set.Key(ref)
	if err != nil {
		refuse("this file's name can't be used", nil)
		return
	}
	rec, err := s.Led.Get(key)
	if err != nil {
		refuse("Kenaz couldn't keep track of this file", nil)
		return
	}
	link, linked := rec.LinkFor(s.Acct)
	if !linked && len(rec.Links) > 0 {
		plan.OtherAccount = append(plan.OtherAccount, name)
		return
	}
	if linked && link.TeamID != s.Team.ID {
		plan.OtherTeam = append(plan.OtherTeam, name)
		return
	}
	if s.Set.Team.IsLink(e.Rel()) {
		refuse("it's a link, and links aren't shared", nil)
		return
	}
	if e.TooLarge {
		refuse(fmt.Sprintf("only files up to %d KiB can be shared", MaxShareFileBytes>>10), nil)
		return
	}
	pg, err := s.Set.Read(ref, 0, contextlib.MaxFileBytes)
	switch {
	case err != nil:
		refuse("it couldn't be read", nil)
		return
	case pg.Size > MaxShareFileBytes:
		refuse(fmt.Sprintf("only files up to %d KiB can be shared", MaxShareFileBytes>>10), nil)
		return
	case strings.TrimSpace(pg.Content) == "":
		refuse("it's empty", nil)
		return
	case contextlib.HashOf([]byte(pg.Content)) != pg.SHA256 || strings.IndexByte(pg.Content, 0) >= 0:
		refuse("it isn't plain text", nil)
		return
	}
	if f, terr := checkShareText(name, pg.Content); terr != nil {
		refuse(syncRefusalReason(terr), f)
		return
	}
	cur, found := s.byID[link.NodeID]
	if !linked {
		found = false
	}
	slug, err := contextlib.SlugOf(name)
	if err != nil {
		refuse("this file's name can't be turned into a short name", nil)
		return
	}
	if !found && !linked {
		cur, found = s.byName[slug]
	}
	// A file on Fleet keeps its short name, whatever it's called here.
	if found && contextlib.ValidSlug(cur.Path) {
		slug = cur.Path
	}
	if !found && !linked {
		if other, taken := names[slug]; taken {
			refuse("it has the same short name as "+other, nil)
			return
		}
		names[slug] = name
	}

	nodeID, base, expected := "", "", 0
	switch {
	case found:
		nodeID, base, expected = cur.ID, cur.Hash, cur.Version
	case linked:
		nodeID, base, expected = link.NodeID, link.BaseSHA256, link.Version
	default:
		nodeID = StableFileID(s.Acct.OrgID, s.Team.ID, slug)
	}
	if base == pg.SHA256 {
		plan.Unchanged++
		return
	}
	plan.Items = append(plan.Items, SyncItem{Name: name, Text: pg.Content, Size: pg.Size, New: base == "", Drawn: rec.DraftedFromMemory,
		put: SyncPut{TeamID: s.Team.ID, Path: slug, Title: contextlib.TitleOf(name, []byte(pg.Content)), Body: pg.Content, ExpectedVersion: expected,
			Local: SyncLocal{NodeID: nodeID, BaseSHA256: base, Ref: ref, LedgerKey: key, FromMemory: rec.DraftedFromMemory}}})
}

func syncRefusalReason(terr *ToolError) string {
	switch terr.Code {
	case CodeHiddenText:
		return "it has characters a reader can't see:"
	case CodeNoteReference:
		return "it names a memory note:"
	}
	return "it looks like it holds a credential:"
}

// Outcomes of sending one file.
const (
	SyncSent        = "sent"
	SyncNotYours    = "not_yours"
	SyncConflicted  = "conflict"
	SyncRemoved     = "removed"
	SyncPromoted    = "promoted"
	SyncBlocked     = "blocked"
	SyncUnreachable = "unreachable"
	SyncRefusedElse = "refused"
)

// PushResult is one file's outcome.
type PushResult struct {
	Outcome  string
	Conflict SyncConflict
}

// Retryable says trying the same file again may work: Fleet is busy or out of reach. The
// write is safe to repeat, since it checks what Fleet holds first.
func Retryable(err error) bool {
	return errors.Is(err, approvals.ErrOutcomeUnknown) || approvals.CodeOf(err) == CodeFleetError
}

// Send writes one file.
func (s *Syncer) Send(ctx context.Context, it SyncItem) (PushResult, error) {
	return s.result(ctx, it, s.Store.Put(ctx, it.put))
}

func (s *Syncer) result(ctx context.Context, it SyncItem, err error) (PushResult, error) {
	if err == nil {
		return PushResult{Outcome: SyncSent}, nil
	}
	if Retryable(err) {
		return PushResult{Outcome: SyncUnreachable}, err
	}
	switch approvals.CodeOf(err) {
	case CodeNotYours:
		return PushResult{Outcome: SyncNotYours}, nil
	case failNodeGone:
		return PushResult{Outcome: SyncRemoved}, nil
	case failNodePromoted:
		return PushResult{Outcome: SyncPromoted}, nil
	case CodeCredentialDetected:
		return PushResult{Outcome: SyncBlocked}, nil
	case failNodeChanged:
		// Someone got there first: theirs at the name, and this text beside it.
		cur, found, rerr := s.Store.File(ctx, it.put.Local.NodeID)
		if rerr != nil || !found || cur.Deleted {
			return PushResult{Outcome: SyncUnreachable}, nil
		}
		ref := it.put.Local.Ref
		local, rerr := s.Set.Read(ref, 0, contextlib.MaxFileBytes)
		if rerr != nil {
			return PushResult{Outcome: SyncUnreachable}, nil
		}
		c, rerr := s.keepBoth(ref, it.put.Local.LedgerKey, cur.Body, local)
		if rerr != nil {
			return PushResult{Outcome: SyncUnreachable}, nil
		}
		rec, _ := s.Led.Get(it.put.Local.LedgerKey)
		link, _ := rec.LinkFor(s.Acct)
		s.record(it.put.Local.LedgerKey, ref, cur, contextlib.HashOf([]byte(cur.Body)), link)
		return PushResult{Outcome: SyncConflicted, Conflict: c}, nil
	}
	return PushResult{Outcome: SyncRefusedElse}, nil
}
