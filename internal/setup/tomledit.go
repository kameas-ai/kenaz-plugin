package setup

import (
	"fmt"
	"regexp"
	"strings"
)

// A minimal, format-preserving editor for one TOML table. Setup only ever
// touches keys directly under [otel], so instead of
// round-tripping the whole document through a TOML library (which loses
// comments and ordering) it edits those lines in place and leaves every
// other byte alone.

const otelTable = "otel"

type tomlEntry struct {
	key        string
	start, end int // line indices, end exclusive
}

type tomlSection struct {
	header  int // line index of [otel], -1 when absent
	end     int // line index after the section
	entries []tomlEntry
}

func (s tomlSection) find(key string) (tomlEntry, bool) {
	for _, e := range s.entries {
		if e.key == key {
			return e, true
		}
	}
	return tomlEntry{}, false
}

var (
	tomlKeyRe    = regexp.MustCompile(`^\s*([A-Za-z0-9_.\-]+)\s*=`)
	tomlHeaderRe = regexp.MustCompile(`^\s*(\[\[?)\s*([^\]]*?)\s*\]\]?\s*(#.*)?$`)
)

// splitLines splits s into lines that keep their line endings.
func splitLines(s string) []string {
	if s == "" {
		return nil
	}
	lines := strings.SplitAfter(s, "\n")
	if lines[len(lines)-1] == "" {
		lines = lines[:len(lines)-1]
	}
	return lines
}

// scanOtel locates [otel] and the keys directly inside it. It refuses
// layouts it cannot edit safely: dotted otel keys at the root, [otel.*]
// sub-tables, array-of-tables, a duplicate [otel], or dotted keys that
// address one of the keys setup owns.
func scanOtel(lines []string, owned []string) (tomlSection, error) {
	sec := tomlSection{header: -1, end: -1}
	table := ""
	inML := ""
	isOwned := func(k string) bool {
		for _, o := range owned {
			if k == o {
				return true
			}
		}
		return false
	}
	for i := 0; i < len(lines); i++ {
		line := lines[i]
		if inML != "" {
			if strings.Count(line, inML)%2 == 1 {
				inML = ""
			}
			continue
		}
		trimmed := strings.TrimSpace(line)
		if trimmed == "" || strings.HasPrefix(trimmed, "#") {
			continue
		}
		if strings.HasPrefix(trimmed, "[") {
			m := tomlHeaderRe.FindStringSubmatch(trimmed)
			if m == nil {
				return sec, fmt.Errorf("line %d: unrecognized table header", i+1)
			}
			name := strings.ReplaceAll(m[2], " ", "")
			if table == otelTable && sec.end < 0 {
				sec.end = i
			}
			table = name
			isOtel := name == otelTable || strings.HasPrefix(name, otelTable+".") || strings.HasPrefix(name, `"otel"`)
			switch {
			case !isOtel:
			case m[1] == "[[":
				return sec, fmt.Errorf("line %d: [[%s]] is not supported; edit [otel] by hand", i+1, name)
			case name != otelTable:
				return sec, fmt.Errorf("line %d: [%s] sub-tables are not supported; edit [otel] by hand", i+1, name)
			case sec.header >= 0:
				return sec, fmt.Errorf("line %d: duplicate [otel] table", i+1)
			default:
				sec.header = i
			}
			continue
		}
		m := tomlKeyRe.FindStringSubmatch(line)
		if m == nil {
			continue
		}
		key := m[1]
		value := line[len(m[0]):]
		end := i + 1
		depth := bracketDepth(value)
		for depth > 0 && end < len(lines) {
			depth += bracketDepth(lines[end])
			end++
		}
		if c := strings.Count(value, `"""`); c%2 == 1 {
			inML = `"""`
		} else if c := strings.Count(value, `'''`); c%2 == 1 {
			inML = `'''`
		}
		switch table {
		case "":
			if key == otelTable || strings.HasPrefix(key, otelTable+".") {
				return sec, fmt.Errorf("line %d: root-level %q is not supported; use an [otel] table", i+1, key)
			}
		case otelTable:
			if j := strings.IndexByte(key, '.'); j >= 0 {
				if isOwned(key[:j]) {
					return sec, fmt.Errorf("line %d: dotted key %q is not supported; edit [otel] by hand", i+1, key)
				}
			} else {
				sec.entries = append(sec.entries, tomlEntry{key, i, end})
			}
		}
		i = end - 1
	}
	if sec.header >= 0 && sec.end < 0 {
		sec.end = len(lines)
	}
	return sec, nil
}

// bracketDepth returns the net count of opening brackets/braces in s,
// ignoring strings and comments.
func bracketDepth(s string) int {
	depth := 0
	for i := 0; i < len(s); i++ {
		switch c := s[i]; c {
		case '#':
			return depth
		case '"', '\'':
			j := i + 1
			for j < len(s) && s[j] != c {
				if c == '"' && s[j] == '\\' {
					j++
				}
				j++
			}
			i = j
		case '[', '{':
			depth++
		case ']', '}':
			depth--
		}
	}
	return depth
}

func joinLines(lines []string) string { return strings.Join(lines, "") }

// normTOML normalizes a TOML snippet for comparison: whitespace and
// trailing comments do not matter.
func normTOML(s string) string {
	var b strings.Builder
	for _, line := range strings.Split(s, "\n") {
		if i := commentStart(line); i >= 0 {
			line = line[:i]
		}
		b.WriteString(strings.Join(strings.Fields(line), ""))
	}
	return b.String()
}

func commentStart(s string) int {
	for i := 0; i < len(s); i++ {
		switch c := s[i]; c {
		case '#':
			return i
		case '"', '\'':
			j := i + 1
			for j < len(s) && s[j] != c {
				if c == '"' && s[j] == '\\' {
					j++
				}
				j++
			}
			i = j
		}
	}
	return -1
}

func lineEnding(lines []string) string {
	for _, l := range lines {
		if strings.HasSuffix(l, "\r\n") {
			return "\r\n"
		}
	}
	return "\n"
}
