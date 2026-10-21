package skills

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/kameas-ai/kenaz-plugin/fleetapi"
)

// Harness names a skill root's consumer.
const (
	HarnessClaude = "claude"
	HarnessCodex  = "codex"
)

// DirPrefix prefixes every managed skill directory name.
const DirPrefix = "kameas-"

// Root is one harness's user-scope skills directory.
type Root struct {
	Harness string
	Dir     string
}

// DetectRoots returns the user-scope roots for harnesses present under
// home: ~/.claude/skills when ~/.claude exists, ~/.agents/skills
// when ~/.codex or ~/.agents exists. force names harnesses to include
// regardless (config skills.targets).
func DetectRoots(home string, force ...string) []Root {
	want := map[string]bool{}
	for _, f := range force {
		want[f] = true
	}
	exists := func(p string) bool { fi, err := os.Stat(p); return err == nil && fi.IsDir() }
	var roots []Root
	if want[HarnessClaude] || exists(filepath.Join(home, ".claude")) {
		roots = append(roots, Root{HarnessClaude, filepath.Join(home, ".claude", "skills")})
	}
	if want[HarnessCodex] || exists(filepath.Join(home, ".codex")) || exists(filepath.Join(home, ".agents")) {
		roots = append(roots, Root{HarnessCodex, filepath.Join(home, ".agents", "skills")})
	}
	return roots
}

// Item is one catalog item to materialize.
type Item struct {
	CatalogID      string
	Kind           string // fleetapi.CatalogKindAgentSkill or CatalogKindSkill (legacy)
	Slug           string // catalog slug; "" = derive from the payload
	Version        string
	Payload        json.RawMessage
	Source         Source
	ReviewedSHA256 string // mandated items only
}

// Ack details. Fixed strings only; never user or catalog content.
const (
	DetailNameConflict       = "name_conflict"
	DetailInvalidPayload     = "invalid_payload"
	DetailUnsupportedTool    = "unsupported_tool_skill"
	DetailUnsupportedKind    = "unsupported_kind"
	DetailStrippedUnreviewed = "stripped_unreviewed"
	DetailWriteFailed        = "write_failed"
	DetailNoRoots            = "no_harness_detected"
	DetailRevoked            = "revoked"
	DetailNoLongerMandated   = "no_longer_mandated"
)

// Result is the outcome of materializing one item in every root.
type Result struct {
	CatalogID  string
	Version    string
	Slug       string
	Status     fleetapi.AckStatus // applied | failed
	Detail     string
	Changed    bool     // something was written
	Dirs       []string // managed directories now holding the item
	Stripped   []string // permission keys removed
	Translated bool     // legacy slashcmd skill translated
}

// Materializer writes skills into harness roots under the ownership rules.
type Materializer struct {
	Roots []Root
	Now   func() time.Time
}

func (m *Materializer) now() time.Time {
	if m.Now != nil {
		return m.Now()
	}
	return time.Now()
}

type prepared struct {
	skill      *AgentSkill
	slug       string
	stripped   []string
	translated bool
	sha        string
}

// prepare decodes, validates, translates and strips an item. Nothing is
// written; any error means nothing will be.
func prepare(it Item) (*prepared, error) {
	var (
		s          *AgentSkill
		err        error
		translated bool
	)
	switch it.Kind {
	case fleetapi.CatalogKindAgentSkill:
		s, err = ParseAgentSkill(it.Payload)
	case fleetapi.CatalogKindSkill:
		s, err = TranslateLegacy(it.Payload)
		translated = true
	default:
		return nil, fmt.Errorf("skills: %w", errUnsupportedKind)
	}
	if err != nil {
		return nil, err
	}
	p := &prepared{skill: s, translated: translated}
	p.slug = SanitizeSlug(it.Slug)
	if p.slug == "" {
		p.slug = SanitizeSlug(s.Name)
	}
	if p.slug == "" {
		return nil, invalid("slug", "empty after sanitizing")
	}
	// Keep permission keys only on a mandated item whose payload
	// bytes are exactly what an admin reviewed.
	reviewed := it.Source == SourceMandated && it.ReviewedSHA256 != "" &&
		strings.EqualFold(PayloadSHA256(it.Payload), it.ReviewedSHA256)
	if !reviewed {
		doc, removed, err := StripKeys(s.Files[ManifestName], PermissionKeys)
		if err != nil {
			return nil, err
		}
		if len(removed) > 0 {
			files := make(map[string]string, len(s.Files))
			for k, v := range s.Files {
				files[k] = v
			}
			files[ManifestName] = doc
			s = &AgentSkill{Schema: s.Schema, Name: s.Name, Description: s.Description, Files: files, Exec: s.Exec}
			p.skill = s
			p.stripped = removed
		}
		if err := CheckNoPermissionKeys(s.Files[ManifestName]); err != nil {
			return nil, err
		}
	}
	p.sha = FileMapSHA256(p.skill.Files)
	return p, nil
}

var errUnsupportedKind = errors.New("unsupported catalog kind")

// SanitizeSlug maps a catalog slug to [a-z0-9-]{1,48}.
func SanitizeSlug(s string) string {
	var b strings.Builder
	for _, r := range strings.ToLower(s) {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9':
			b.WriteRune(r)
		default:
			b.WriteByte('-')
		}
	}
	out := strings.Trim(b.String(), "-")
	for strings.Contains(out, "--") {
		out = strings.ReplaceAll(out, "--", "-")
	}
	if len(out) > 48 {
		out = strings.Trim(out[:48], "-")
	}
	return out
}

// Install materializes it into every root. It never writes into, renames
// or deletes a directory without a valid marker, and swaps each
// directory atomically.
func (m *Materializer) Install(it Item) Result {
	res := Result{CatalogID: it.CatalogID, Version: it.Version, Status: fleetapi.AckFailed}
	if len(m.Roots) == 0 {
		res.Detail = DetailNoRoots
		return res
	}
	p, err := prepare(it)
	if err != nil {
		switch {
		case errors.Is(err, ErrUnsupportedToolSkill):
			res.Detail = DetailUnsupportedTool
		case errors.Is(err, errUnsupportedKind):
			res.Detail = DetailUnsupportedKind
		default:
			res.Detail = DetailInvalidPayload
		}
		return res
	}
	res.Slug, res.Stripped, res.Translated = p.slug, p.stripped, p.translated
	marker := Marker{
		CatalogID: it.CatalogID, Kind: it.Kind, Slug: p.slug, Version: it.Version,
		SHA256: p.sha, Source: it.Source, InstalledAt: m.now().UTC(), Stripped: p.stripped,
	}
	if p.translated {
		marker.TranslatedFrom = "slashcmd"
	}
	// Resolve every target before writing anything, so a conflict in one
	// root never leaves the item half-installed.
	dirs := make([]string, len(m.Roots))
	for i, r := range m.Roots {
		dir, ok, err := targetDir(r.Dir, p.slug, it.CatalogID)
		if err != nil {
			res.Detail = DetailWriteFailed
			return res
		}
		if !ok {
			res.Detail = DetailNameConflict
			return res
		}
		dirs[i] = dir
	}
	for i, r := range m.Roots {
		dir := dirs[i]
		if cur, err := ReadMarker(dir); err == nil && cur.SHA256 == p.sha && cur.Version == it.Version && cur.Source == it.Source {
			res.Dirs = append(res.Dirs, dir)
			continue // already current
		}
		if err := writeAtomic(r.Dir, dir, p.skill.Files, marker); err != nil {
			res.Detail = DetailWriteFailed
			return res
		}
		res.Changed = true
		res.Dirs = append(res.Dirs, dir)
	}
	res.Status = fleetapi.AckApplied
	if it.Source == SourceMandated && len(p.stripped) > 0 {
		res.Detail = DetailStrippedUnreviewed
	}
	return res
}

// targetDir picks the directory for slug in root. ok=false means an
// unmarked directory occupies the name (name_conflict). A marked directory
// owned by another catalog item gets a -<6 hex of catalog id> suffix.
func targetDir(root, slug, catalogID string) (string, bool, error) {
	base := filepath.Join(root, DirPrefix+slug)
	fi, err := os.Lstat(base)
	if errors.Is(err, os.ErrNotExist) {
		return base, true, nil
	}
	if err != nil {
		return "", false, err
	}
	if !fi.IsDir() {
		return "", false, nil
	}
	cur, err := ReadMarker(base)
	if err != nil {
		return "", false, nil // unmarked: never touch
	}
	if cur.CatalogID == catalogID {
		return base, true, nil
	}
	alt := base + "-" + shortID(catalogID)
	fi, err = os.Lstat(alt)
	if errors.Is(err, os.ErrNotExist) {
		return alt, true, nil
	}
	if err != nil || !fi.IsDir() {
		return "", false, err
	}
	if cur, err := ReadMarker(alt); err == nil && cur.CatalogID == catalogID {
		return alt, true, nil
	}
	return "", false, nil
}

func shortID(id string) string {
	h := PayloadSHA256([]byte(id))
	return h[:6]
}

func randSuffix() string {
	var b [6]byte
	_, _ = rand.Read(b[:])
	return hex.EncodeToString(b[:])
}

// writeAtomic builds the skill in a temp sibling and swaps it into dir.
// Only regular files are created (mode 0644; never executable).
func writeAtomic(root, dir string, files map[string]string, mk Marker) error {
	if err := os.MkdirAll(root, 0o755); err != nil {
		return err
	}
	tmp := filepath.Join(root, ".kameas-tmp-"+randSuffix())
	if err := os.Mkdir(tmp, 0o755); err != nil {
		return err
	}
	cleanup := func() { _ = os.RemoveAll(tmp) }
	paths := make([]string, 0, len(files))
	for p := range files {
		paths = append(paths, p)
	}
	sort.Strings(paths)
	for _, p := range paths {
		if err := CheckPath(p); err != nil {
			cleanup()
			return err
		}
		dst := filepath.Join(tmp, filepath.FromSlash(p))
		if rel, err := filepath.Rel(tmp, dst); err != nil || strings.HasPrefix(rel, "..") {
			cleanup()
			return invalid("files", "path %q escapes the skill directory", p)
		}
		if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
			cleanup()
			return err
		}
		f, err := os.OpenFile(dst, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o644)
		if err != nil {
			cleanup()
			return err
		}
		_, werr := f.WriteString(files[p])
		cerr := f.Close()
		if werr != nil || cerr != nil {
			cleanup()
			return errors.Join(werr, cerr)
		}
	}
	if err := writeMarker(tmp, mk); err != nil {
		cleanup()
		return err
	}
	var old string
	if _, err := os.Lstat(dir); err == nil {
		if _, err := ReadMarker(dir); err != nil {
			cleanup()
			return errNoMarker // re-checked: never replace an unmarked dir
		}
		old = filepath.Join(root, ".kameas-old-"+randSuffix())
		if err := os.Rename(dir, old); err != nil {
			cleanup()
			return err
		}
	}
	if err := os.Rename(tmp, dir); err != nil {
		if old != "" {
			_ = os.Rename(old, dir)
		}
		cleanup()
		return err
	}
	if old != "" {
		_ = os.RemoveAll(old)
	}
	return nil
}

// Owned is a managed directory found on disk.
type Owned struct {
	Root   Root
	Dir    string
	Marker Marker
}

// Owned lists every marked kameas-* directory in the roots and removes
// leftover temp/old directories from interrupted swaps.
func (m *Materializer) Owned() ([]Owned, error) {
	var out []Owned
	for _, r := range m.Roots {
		ents, err := os.ReadDir(r.Dir)
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			return nil, err
		}
		for _, e := range ents {
			name := e.Name()
			if strings.HasPrefix(name, ".kameas-tmp-") || strings.HasPrefix(name, ".kameas-old-") {
				_ = os.RemoveAll(filepath.Join(r.Dir, name))
				continue
			}
			if !e.IsDir() || !strings.HasPrefix(name, DirPrefix) {
				continue
			}
			dir := filepath.Join(r.Dir, name)
			mk, err := ReadMarker(dir)
			if err != nil {
				continue
			}
			out = append(out, Owned{Root: r, Dir: dir, Marker: mk})
		}
	}
	return out, nil
}

// Remove deletes every marked directory holding catalogID. It returns the
// number of directories removed.
func (m *Materializer) Remove(catalogID string) (int, error) {
	owned, err := m.Owned()
	if err != nil {
		return 0, err
	}
	n := 0
	for _, o := range owned {
		if o.Marker.CatalogID != catalogID {
			continue
		}
		if err := removeOwned(o.Dir); err != nil {
			return n, err
		}
		n++
	}
	return n, nil
}

// removeOwned deletes dir only if it still carries a valid marker.
func removeOwned(dir string) error {
	if _, err := ReadMarker(dir); err != nil {
		return errNoMarker
	}
	tomb := filepath.Join(filepath.Dir(dir), ".kameas-old-"+randSuffix())
	if err := os.Rename(dir, tomb); err != nil {
		return err
	}
	return os.RemoveAll(tomb)
}
