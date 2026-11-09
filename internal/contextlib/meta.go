package contextlib

import (
	"bytes"
	"path"
	"strings"

	"github.com/kameas-ai/kenaz-plugin/internal/safetext"
)

const (
	maxTitleRunes       = 80
	maxDescriptionRunes = 120
)

type meta struct {
	Title, Description string
	// DescriptionFromBody says the description is the file's first line of
	// text rather than one the file declares.
	DescriptionFromBody bool
	Always              bool
}

// parseMeta reads the title, description and always flag from the start of a
// file. Frontmatter is a first line of ---, simple "key: value" lines and a
// closing ---. Nothing else in it is interpreted. A missing title or description
// falls back to the first heading and the first line of text.
func parseMeta(head []byte, partial bool) meta {
	if partial {
		// The last line may be cut off mid-way.
		if i := bytes.LastIndexByte(head, '\n'); i >= 0 {
			head = head[:i]
		}
	}
	fields, body := splitFrontmatter(head)
	m := meta{Title: oneLine(fields["title"], maxTitleRunes), Description: oneLine(fields["description"], maxDescriptionRunes)}
	if v, ok := fields["always"]; ok {
		switch strings.ToLower(v) {
		case "", "true", "yes", "on":
			m.Always = true
		}
	}
	for _, line := range strings.Split(string(body), "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		if h, ok := strings.CutPrefix(line, "# "); ok && m.Title == "" {
			m.Title = oneLine(h, maxTitleRunes)
			continue
		}
		if strings.HasPrefix(line, "#") {
			continue
		}
		if m.Description == "" {
			m.Description = oneLine(line, maxDescriptionRunes)
			m.DescriptionFromBody = true
		}
		break
	}
	return m
}

// splitFrontmatter returns the frontmatter fields and everything after them.
// With no complete frontmatter the whole input is the body.
func splitFrontmatter(b []byte) (map[string]string, []byte) {
	s := bytes.TrimPrefix(b, []byte("\xef\xbb\xbf"))
	first, rest, ok := bytes.Cut(s, []byte("\n"))
	if !ok || string(bytes.TrimRight(first, " \t\r")) != "---" {
		return nil, s
	}
	fields := map[string]string{}
	for len(rest) > 0 {
		var line []byte
		line, rest, _ = bytes.Cut(rest, []byte("\n"))
		text := strings.TrimSpace(string(line))
		if text == "---" {
			return fields, rest
		}
		k, v, ok := strings.Cut(text, ":")
		if !ok || strings.HasPrefix(text, "#") {
			continue
		}
		fields[strings.ToLower(strings.TrimSpace(k))] = unquote(strings.TrimSpace(v))
	}
	return nil, s
}

func unquote(v string) string {
	if len(v) >= 2 && (v[0] == '"' || v[0] == '\'') && v[len(v)-1] == v[0] {
		return v[1 : len(v)-1]
	}
	return v
}

func oneLine(s string, maxRunes int) string { return safetext.OneLine(s, maxRunes) }

func fallbackTitle(rel string) string {
	base := path.Base(rel)
	return oneLine(strings.TrimSuffix(base, path.Ext(base)), maxTitleRunes)
}

// TitleOf returns the title a file with this content is listed under.
func TitleOf(rel string, content []byte) string {
	if t := parseMeta(content, false).Title; t != "" {
		return t
	}
	return fallbackTitle(rel)
}
