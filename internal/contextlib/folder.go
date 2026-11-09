package contextlib

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path"
	"strings"
)

// A team's folder belongs to one environment, organization, account and team. A small
// hidden record says so, which keeps the folder's name readable: two teams called
// Platform get two folders. Text in a folder never reaches any other owner, and a file
// in one bound to someone else can't be read, listed or written through a Set with a viewer.

// FolderRecord is the record's name inside a team's folder.
const FolderRecord = ".kenaz-folder.json"

// ErrOtherOwner refuses a file in a folder that belongs to another account or team, and
// ErrFolderNeedsSignIn one in a folder with an owner when nobody is signed in.
var (
	ErrOtherOwner        = errors.New("contextlib: this folder belongs to another account")
	ErrFolderNeedsSignIn = errors.New("contextlib: sign in to use a team's folder")
)

// FolderOwner is who a team's folder belongs to. TeamID is "" for organization files.
type FolderOwner struct {
	Env    string `json:"env"`
	OrgID  string `json:"org_id"`
	UserID string `json:"user_id"`
	TeamID string `json:"team_id,omitempty"`
	// Unassigned says a file was saved here before a team was known: the folder is this
	// account's, and the first sync of the team of that name takes it.
	Unassigned bool `json:"unassigned,omitempty"`
}

// Readable reports whether the record names an owner at all.
func (o FolderOwner) Readable() bool { return o.Env != "" && o.OrgID != "" && o.UserID != "" }

// Account is the owner's account.
func (o FolderOwner) Account() Account { return Account{Env: o.Env, OrgID: o.OrgID, UserID: o.UserID} }

// FolderOwner reads a folder's record. bound is false when there is none. A record
// that can't be read counts as bound, to nobody.
func (l *Library) FolderOwner(folder string) (owner FolderOwner, bound bool) {
	root, err := l.open()
	if err != nil || root == nil {
		return FolderOwner{}, false
	}
	defer root.Close()
	fi, err := root.Lstat(folder)
	if err != nil {
		return FolderOwner{}, false
	}
	if !fi.IsDir() {
		return FolderOwner{}, fi.Mode()&fs.ModeSymlink != 0
	}
	b, err := root.ReadFile(path.Join(folder, FolderRecord))
	if errors.Is(err, fs.ErrNotExist) {
		return FolderOwner{}, false
	}
	if err != nil || json.Unmarshal(b, &owner) != nil {
		return FolderOwner{}, true
	}
	return owner, true
}

// FindFolder returns the folder bound to owner.
func (l *Library) FindFolder(owner FolderOwner) (string, bool) {
	root, err := l.open()
	if err != nil || root == nil {
		return "", false
	}
	entries, err := fs.ReadDir(root.FS(), ".")
	root.Close()
	if err != nil {
		return "", false
	}
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		if o, bound := l.FolderOwner(e.Name()); bound && o == owner {
			return e.Name(), true
		}
	}
	return "", false
}

// FolderHasFiles reports whether a folder holds anything but hidden files, and exists
// says whether it's there at all.
func (l *Library) FolderHasFiles(folder string) (exists, hasFiles bool) {
	root, err := l.open()
	if err != nil || root == nil {
		return false, false
	}
	defer root.Close()
	if _, err := root.Lstat(folder); err != nil {
		return false, false
	}
	entries, err := fs.ReadDir(root.FS(), folder)
	if err != nil {
		return true, true
	}
	for _, e := range entries {
		if !strings.HasPrefix(e.Name(), ".") {
			return true, true
		}
	}
	return true, false
}

// BindFolder makes folder, if needed, and records its owner. A folder that already has
// another owner, or is a link, is refused.
func (l *Library) BindFolder(folder string, owner FolderOwner) error {
	if o, bound := l.FolderOwner(folder); bound && o != owner && !(o.Unassigned && o.Account() == owner.Account()) {
		return ErrOtherOwner
	}
	if err := os.MkdirAll(l.dir, 0o700); err != nil {
		return err
	}
	root, err := l.open()
	if err != nil {
		return err
	}
	if root == nil {
		return fmt.Errorf("%w: no folder", ErrInvalidPath)
	}
	defer root.Close()
	if err := root.Mkdir(folder, 0o700); err != nil && !errors.Is(err, fs.ErrExist) {
		return err
	}
	b, err := json.Marshal(owner)
	if err != nil {
		return err
	}
	return root.WriteFile(path.Join(folder, FolderRecord), b, 0o600)
}

// RestoreFolderRecord puts a folder's record back as it was before this run bound it. nil
// removes it.
func (l *Library) RestoreFolderRecord(folder string, prior *FolderOwner) error {
	root, err := l.open()
	if err != nil {
		return err
	}
	if root == nil {
		return nil
	}
	defer root.Close()
	rec := path.Join(folder, FolderRecord)
	if prior == nil {
		return root.Remove(rec)
	}
	b, err := json.Marshal(*prior)
	if err != nil {
		return err
	}
	return root.WriteFile(rec, b, 0o600)
}

// UnassignedFolders lists account a's folders with files and no team yet.
func (l *Library) UnassignedFolders(a Account) []string {
	root, err := l.open()
	if err != nil || root == nil {
		return nil
	}
	entries, err := fs.ReadDir(root.FS(), ".")
	root.Close()
	if err != nil {
		return nil
	}
	var out []string
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		o, bound := l.FolderOwner(e.Name())
		if !bound || !o.Unassigned || o.Account() != a {
			continue
		}
		if _, has := l.FolderHasFiles(e.Name()); has {
			out = append(out, e.Name())
		}
	}
	return out
}

// IsLink reports whether rel is a link. A link in a team's folder could point at another
// team's copy, so sync neither reads nor writes through one.
func (l *Library) IsLink(rel string) bool {
	root, err := l.open()
	if err != nil || root == nil {
		return false
	}
	defer root.Close()
	fi, err := root.Lstat(rel)
	return err == nil && fi.Mode()&fs.ModeSymlink != 0
}

// Move renames a file inside the library. A file already at newRel stops it.
func (l *Library) Move(rel, newRel string) error {
	root, err := l.open()
	if err != nil {
		return err
	}
	if root == nil {
		return ErrNotFound
	}
	defer root.Close()
	if _, err := root.Lstat(newRel); err == nil {
		return ErrExists
	}
	return root.Rename(rel, newRel)
}

// WithViewer limits the set's team files to what account a may see: its own folders,
// and older folders with no record. nil means nobody is signed in, who sees no bound folder.
func (s Set) WithViewer(a *Account) Set {
	s.scoped, s.viewer = true, a
	return s
}

// folderAccess is why a folder can't be used, or nil.
func (s Set) folderAccess(folder string) error {
	if !s.scoped || s.Team == nil {
		return nil
	}
	owner, bound := s.Team.FolderOwner(folder)
	switch {
	case !bound:
		return nil
	case s.viewer == nil:
		return ErrFolderNeedsSignIn
	case owner.Account() != *s.viewer:
		return ErrOtherOwner
	}
	return nil
}

// teamVisible checks a path as List reports it, by what it comes to once cleaned, which
// is what a read or write then uses. A path that doesn't clean is refused there.
func (s Set) teamVisible(ref string) error {
	source, rel, ok := strings.Cut(strings.ReplaceAll(ref, "\\", "/"), "/")
	if !ok || Source(source) != SourceTeam {
		return nil
	}
	clean, err := cleanRel(rel)
	if err != nil {
		return nil
	}
	folder, _, inFolder := strings.Cut(clean, "/")
	if !inFolder {
		return nil
	}
	return s.folderAccess(folder)
}

// folderToClaim names the new folder a save at ref would start, for a signed-in person.
func (s Set) folderToClaim(ref string) string {
	if !s.scoped || s.viewer == nil || s.Team == nil {
		return ""
	}
	source, rel, ok := strings.Cut(strings.ReplaceAll(ref, "\\", "/"), "/")
	if !ok || Source(source) != SourceTeam {
		return ""
	}
	clean, err := cleanRel(rel)
	if err != nil {
		return ""
	}
	folder, _, inFolder := strings.Cut(clean, "/")
	if !inFolder {
		return ""
	}
	if _, bound := s.Team.FolderOwner(folder); bound {
		return ""
	}
	if _, has := s.Team.FolderHasFiles(folder); has {
		return ""
	}
	return folder
}

// claimFolder records who a new folder belongs to, once a save has made it. Which team
// isn't known here, so the first sync of the team of that name adds that. If this fails
// the file is still saved, and the first sync can claim the folder.
func (s Set) claimFolder(folder string) {
	if folder != "" {
		_ = s.Team.BindFolder(folder, FolderOwner{Env: s.viewer.Env, OrgID: s.viewer.OrgID, UserID: s.viewer.UserID, Unassigned: true})
	}
}
