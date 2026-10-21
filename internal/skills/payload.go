package skills

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"path"
	"regexp"
	"sort"
	"strings"
	"unicode"
	"unicode/utf8"
)

// agent_skill/v1 limits. They mirror Fleet's catalog validation:
// Fleet validates at publish, and the relay validates again
// before writing anything, so neither side trusts the other.
const (
	SchemaV1     = "agent_skill/v1"
	MaxBytes     = 256 * 1024
	MaxFiles     = 64
	maxDepth     = 4
	maxPathLen   = 200
	maxDescLen   = 1024
	ManifestName = "SKILL.md"
)

var (
	nameRE      = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,63}$`)
	allowedExts = map[string]bool{
		".md": true, ".txt": true, ".json": true, ".yaml": true, ".yml": true,
		".toml": true, ".sh": true, ".py": true, ".js": true, ".ts": true,
		".mjs": true, ".rb": true, ".go": true, ".sql": true, ".csv": true,
		".html": true, ".css": true, ".xml": true,
	}
	bareNames = map[string]bool{"LICENSE": true, "README": true}
)

// ValidationError is a payload rejection. Field and Reason are fixed
// strings or catalog-provided paths, never file bodies.
type ValidationError struct {
	Field  string
	Reason string
}

func (e *ValidationError) Error() string { return "agent_skill: " + e.Field + ": " + e.Reason }

func invalid(field, format string, a ...any) error {
	return &ValidationError{Field: field, Reason: fmt.Sprintf(format, a...)}
}

// AgentSkill is a decoded agent_skill/v1 payload: one Agent Skills
// directory as a path → text map. Exec is informational only: the relay
// never sets executable bits; whether a harness runs a script is its
// own permission model.
type AgentSkill struct {
	Schema      string            `json:"schema"`
	Name        string            `json:"name"`
	Description string            `json:"description"`
	Files       map[string]string `json:"files"`
	Exec        []string          `json:"exec,omitempty"`
}

// ParseAgentSkill strictly decodes and validates an agent_skill/v1 payload.
func ParseAgentSkill(payload []byte) (*AgentSkill, error) {
	if len(payload) > MaxBytes {
		return nil, invalid("payload", "exceeds %d bytes", MaxBytes)
	}
	if !utf8.Valid(payload) {
		return nil, invalid("payload", "not valid UTF-8")
	}
	var s AgentSkill
	dec := json.NewDecoder(bytes.NewReader(payload))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&s); err != nil {
		return nil, invalid("payload", "not an agent_skill/v1 object")
	}
	if _, err := dec.Token(); err != io.EOF {
		return nil, invalid("payload", "trailing data")
	}
	if s.Schema != SchemaV1 {
		return nil, invalid("schema", "must be %q", SchemaV1)
	}
	if err := checkDuplicateFileKeys(payload); err != nil {
		return nil, err
	}
	if err := s.Validate(); err != nil {
		return nil, err
	}
	return &s, nil
}

// Validate applies the on-device rules to s. It is also used for
// translated legacy skills and for directories packaged for publish.
func (s *AgentSkill) Validate() error {
	if !nameRE.MatchString(s.Name) {
		return invalid("name", "must match %s", nameRE)
	}
	if err := checkDescription("description", s.Description); err != nil {
		return err
	}
	if len(s.Files) == 0 {
		return invalid("files", "required")
	}
	if len(s.Files) > MaxFiles {
		return invalid("files", "at most %d files", MaxFiles)
	}
	folded := make(map[string]string, len(s.Files))
	for p, body := range s.Files {
		if err := CheckPath(p); err != nil {
			return err
		}
		k := strings.ToLower(p)
		if prev, dup := folded[k]; dup {
			return invalid("files", "%q and %q differ only by case", prev, p)
		}
		folded[k] = p
		if !utf8.ValidString(body) {
			return invalid("files", "body of %q is not valid UTF-8", p)
		}
	}
	manifest, ok := s.Files[ManifestName]
	if !ok {
		return invalid("files", "%s is required", ManifestName)
	}
	fm, err := ParseFrontmatter(manifest)
	if err != nil {
		return err
	}
	if fm.Get("name") != s.Name {
		return invalid("SKILL.md", "frontmatter name must equal the payload name")
	}
	if err := checkDescription("SKILL.md.description", fm.Get("description")); err != nil {
		return err
	}
	for _, e := range s.Exec {
		if _, ok := s.Files[e]; !ok {
			return invalid("exec", "%q is not a file", e)
		}
	}
	return nil
}

// CheckPath enforces the relative-path rules for one files key: no empty,
// absolute, drive-letter, backslash, control-character, '.' or '..'
// segments, bounded depth, and an allowed extension. Combined with writing
// only regular files under a fresh temp directory, this makes escaping the
// skill directory and creating symlinks impossible.
func CheckPath(p string) error {
	switch {
	case p == "":
		return invalid("files", "empty path")
	case len(p) > maxPathLen:
		return invalid("files", "path %q exceeds %d bytes", p, maxPathLen)
	case !utf8.ValidString(p):
		return invalid("files", "path is not valid UTF-8")
	case strings.HasPrefix(p, "/"):
		return invalid("files", "path %q is absolute", p)
	case strings.ContainsRune(p, '\\'):
		return invalid("files", "path %q contains a backslash", p)
	case len(p) >= 2 && p[1] == ':':
		return invalid("files", "path %q looks like a drive letter", p)
	}
	for _, r := range p {
		if r < 0x20 || r == 0x7f || unicode.IsControl(r) {
			return invalid("files", "path %q contains control characters", p)
		}
	}
	segs := strings.Split(p, "/")
	if len(segs) > maxDepth {
		return invalid("files", "path %q is deeper than %d", p, maxDepth)
	}
	for _, seg := range segs {
		if seg == "" || seg == "." || seg == ".." {
			return invalid("files", "path %q has an empty, '.' or '..' segment", p)
		}
		if seg == MarkerName {
			return invalid("files", "path %q uses the reserved marker name", p)
		}
	}
	base := segs[len(segs)-1]
	ext := path.Ext(base)
	if ext == "" {
		if !bareNames[base] {
			return invalid("files", "path %q has no allowed extension", p)
		}
		return nil
	}
	if !allowedExts[strings.ToLower(ext)] {
		return invalid("files", "extension %q of %q is not allowed", ext, p)
	}
	return nil
}

func checkDescription(field, d string) error {
	d = strings.TrimSpace(d)
	if d == "" {
		return invalid(field, "required")
	}
	if utf8.RuneCountInString(d) > maxDescLen {
		return invalid(field, "at most %d characters", maxDescLen)
	}
	return nil
}

// checkDuplicateFileKeys rejects repeated keys in files, which encoding/json
// would silently collapse.
func checkDuplicateFileKeys(payload []byte) error {
	var top map[string]json.RawMessage
	if err := json.Unmarshal(payload, &top); err != nil {
		return invalid("payload", "not a JSON object")
	}
	raw, ok := top["files"]
	if !ok {
		return nil
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	if t, err := dec.Token(); err != nil || t != json.Delim('{') {
		return invalid("files", "must be an object of path to text")
	}
	seen := map[string]bool{}
	for dec.More() {
		t, err := dec.Token()
		if err != nil {
			return invalid("files", "malformed")
		}
		k, _ := t.(string)
		if seen[k] {
			return invalid("files", "duplicate key %q", k)
		}
		seen[k] = true
		var skip json.RawMessage
		if err := dec.Decode(&skip); err != nil {
			return invalid("files", "malformed")
		}
	}
	return nil
}

// FileMapSHA256 is the canonical hash of a file map: sorted paths,
// each followed by NUL, its body, and NUL.
func FileMapSHA256(files map[string]string) string {
	keys := make([]string, 0, len(files))
	for k := range files {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	h := sha256.New()
	for _, k := range keys {
		io.WriteString(h, k)
		h.Write([]byte{0})
		io.WriteString(h, files[k])
		h.Write([]byte{0})
	}
	return hex.EncodeToString(h.Sum(nil))
}

// PayloadSHA256 is the hash of the payload bytes as received. Fleet
// records a mandate's reviewed_sha256 over exactly these bytes.
func PayloadSHA256(payload []byte) string {
	sum := sha256.Sum256(payload)
	return hex.EncodeToString(sum[:])
}
