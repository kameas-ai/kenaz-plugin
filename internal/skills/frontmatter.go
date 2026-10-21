package skills

import (
	"strings"

	"gopkg.in/yaml.v3"
)

// PermissionKeys are the SKILL.md frontmatter keys that pre-approve tools
// or run commands in a harness. They are stripped from every skill that an
// admin has not reviewed at exactly these bytes. The
// relay only ever removes keys; it never adds them.
var PermissionKeys = []string{"allowed-tools", "hooks"}

// Frontmatter is the top-level scalar view of a SKILL.md YAML frontmatter
// block: the same minimal subset Fleet parses. Nested
// values (lists, maps) are recorded as present with an empty value.
type Frontmatter struct {
	vals  map[string]string
	order []string
}

// Get returns the value of top-level key k ("" if absent or nested).
func (f Frontmatter) Get(k string) string { return f.vals[k] }

// Has reports whether top-level key k is present.
func (f Frontmatter) Has(k string) bool { _, ok := f.vals[k]; return ok }

// Keys returns the top-level keys in document order.
func (f Frontmatter) Keys() []string { return append([]string(nil), f.order...) }

// frontmatterBounds returns the doc split into lines and the index of the
// closing fence. The opening fence is line 0.
func frontmatterBounds(doc string) ([]string, int, error) {
	doc = strings.TrimPrefix(doc, "\ufeff")
	lines := strings.Split(doc, "\n")
	if len(lines) == 0 || strings.TrimSpace(lines[0]) != "---" {
		return nil, 0, invalid("SKILL.md", "must begin with a --- frontmatter fence")
	}
	for i := 1; i < len(lines); i++ {
		if strings.TrimRight(lines[i], " \t\r") == "---" {
			return lines, i, nil
		}
	}
	return nil, 0, invalid("SKILL.md", "unterminated frontmatter")
}

// topLevelKey returns the key if line starts a top-level mapping entry.
func topLevelKey(line string) (string, string, bool) {
	l := strings.TrimRight(line, "\r")
	if strings.TrimSpace(l) == "" || strings.HasPrefix(strings.TrimSpace(l), "#") {
		return "", "", false
	}
	if l[0] == ' ' || l[0] == '\t' || strings.HasPrefix(l, "- ") || l == "-" {
		return "", "", false
	}
	k, v, ok := strings.Cut(l, ":")
	if !ok {
		return "", "", false
	}
	return strings.TrimSpace(k), strings.TrimSpace(v), true
}

// ParseFrontmatter parses the top-level keys of doc's frontmatter.
func ParseFrontmatter(doc string) (Frontmatter, error) {
	lines, end, err := frontmatterBounds(doc)
	if err != nil {
		return Frontmatter{}, err
	}
	f := Frontmatter{vals: map[string]string{}}
	body := lines[1:end]
	for i := 0; i < len(body); i++ {
		k, v, ok := topLevelKey(body[i])
		if !ok {
			continue
		}
		if v == "|" || v == ">" || v == "|-" || v == ">-" {
			var parts []string
			for i+1 < len(body) && (strings.HasPrefix(body[i+1], " ") || strings.HasPrefix(body[i+1], "\t") || strings.TrimSpace(body[i+1]) == "") {
				i++
				parts = append(parts, strings.TrimSpace(body[i]))
			}
			sep := "\n"
			if strings.HasPrefix(v, ">") {
				sep = " "
			}
			v = strings.TrimSpace(strings.Join(parts, sep))
		}
		v = strings.Trim(v, `"'`)
		if _, dup := f.vals[k]; !dup {
			f.order = append(f.order, k)
		}
		f.vals[k] = v
	}
	return f, nil
}

// StripKeys removes the given top-level keys (and their nested lines) from
// doc's frontmatter, leaving every other byte of the document unchanged.
// It returns the keys actually removed, in the order of keys.
func StripKeys(doc string, keys []string) (string, []string, error) {
	lines, end, err := frontmatterBounds(doc)
	if err != nil {
		return doc, nil, err
	}
	drop := map[string]bool{}
	for _, k := range keys {
		drop[k] = true
	}
	found := map[string]bool{}
	out := make([]string, 0, len(lines))
	out = append(out, lines[0])
	skipping := false
	for i := 1; i < end; i++ {
		if k, _, ok := topLevelKey(lines[i]); ok {
			skipping = drop[k]
			if skipping {
				found[k] = true
				continue
			}
		} else if skipping {
			continue // nested continuation of a dropped key
		}
		out = append(out, lines[i])
	}
	out = append(out, lines[end:]...)
	var removed []string
	for _, k := range keys {
		if found[k] {
			removed = append(removed, k)
		}
	}
	if len(removed) == 0 {
		return doc, nil, nil
	}
	return strings.Join(out, "\n"), removed, nil
}

// CheckNoPermissionKeys fails closed when doc's frontmatter could still
// carry a PermissionKeys entry after StripKeys. StripKeys only
// sees plain `key:` lines, but YAML has other spellings of the same key:
// quoted or escaped keys ("allowed-tools", "allowed\x2dtools"), explicit
// keys (? allowed-tools), and merge keys (<<: *anchor). An unreviewed
// skill using any of those at the top level is rejected, and a full YAML
// parse of what remains must not yield a permission key.
func CheckNoPermissionKeys(doc string) error {
	lines, end, err := frontmatterBounds(doc)
	if err != nil {
		return err
	}
	body := lines[1:end]
	for _, l := range body {
		l = strings.TrimRight(l, "\r")
		t := strings.TrimSpace(l)
		if t == "" || strings.HasPrefix(t, "#") || l[0] == ' ' || l[0] == '\t' {
			continue
		}
		if strings.ContainsRune(`"'?{[!&*%|>@`+"`", rune(t[0])) {
			return invalid("SKILL.md", "frontmatter uses YAML key syntax the relay cannot verify")
		}
		if k, _, ok := topLevelKey(l); ok && k == "<<" {
			return invalid("SKILL.md", "frontmatter uses a YAML merge key")
		}
	}
	var m map[string]any
	if yaml.Unmarshal([]byte(strings.Join(body, "\n")), &m) == nil {
		for _, k := range PermissionKeys {
			if _, ok := m[k]; ok {
				return invalid("SKILL.md", "frontmatter still carries %s", k)
			}
		}
	}
	return nil
}
