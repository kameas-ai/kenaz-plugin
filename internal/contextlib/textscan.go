package contextlib

import (
	"regexp"
	"sort"
	"strings"
	"unicode/utf8"

	"github.com/kameas-ai/kenaz-plugin/internal/safetext"
	"github.com/kameas-ai/kenaz-plugin/pkg/secretscan"
)

// Rules of the hidden-character scan.
const (
	RuleTagCharacter       = "tag_character"
	RuleInvisibleCharacter = "invisible_character"
)

const maxHiddenFindings = 20

// ScanText is the check on text about to be saved or shared: characters a reader
// can't see, by rule and line, then credentials. Credentials are looked for with
// the invisible characters taken out, so one can't hide behind a zero-width space.
// forSync adds the stricter patterns for text about to leave the device. A finding
// never holds the text.
//
// Look-alike letters aren't folded, so a credential spelled with them isn't found.
func ScanText(text string, forSync bool) []secretscan.Finding {
	out := hiddenFindings(text)
	clean := stripHidden(text)
	if forSync {
		return append(out, secretscan.FindForSync(clean)...)
	}
	return append(out, secretscan.Find(clean)...)
}

// HiddenOnly reports whether findings are all about hidden characters.
func HiddenOnly(findings []secretscan.Finding) bool {
	for _, f := range findings {
		if f.Rule != RuleTagCharacter && f.Rule != RuleInvisibleCharacter {
			return false
		}
	}
	return true
}

// ScanName looks for a credential in a file's path. It reports no line, because a
// name has none.
func ScanName(rel string) []secretscan.Finding {
	var out []secretscan.Finding
	seen := map[string]bool{}
	for _, f := range secretscan.FindForSync(rel) {
		if !seen[f.Rule] {
			seen[f.Rule] = true
			out = append(out, secretscan.Finding{Rule: f.Rule})
		}
	}
	return out
}

// noteRefRe matches a memory note's id, whole or as the short prefix the session
// index shows and memory_get accepts. Letters and digits around it mean it's part of
// a longer word; an underscore doesn't, so Markdown italics and note_mem-... count,
// and so do hyphens other than the plain one, because editors swap those in.
var noteRefRe = regexp.MustCompile(`(?i)(?:^|[^\p{L}\p{N}])mem[-\x{00AD}\x{2010}-\x{2015}\x{2212}\x{FE58}\x{FE63}\x{FF0D}]([0-9a-f]{8,})(?:$|[^\p{L}\p{N}])`)

// RuleNoteReference names a pointer back to a memory note.
const RuleNoteReference = "note_reference"

// FindNoteRefs reports the lines that name a memory note. A file drawn from memory
// must hold no way back to its notes.
func FindNoteRefs(text string) []secretscan.Finding {
	var out []secretscan.Finding
	clean := stripHidden(text)
	seen := map[int]bool{}
	for _, m := range noteRefRe.FindAllStringSubmatchIndex(clean, -1) {
		line := strings.Count(clean[:m[2]], "\n") + 1
		if !seen[line] && len(out) < maxHiddenFindings {
			seen[line] = true
			out = append(out, secretscan.Finding{Rule: RuleNoteReference, Line: line})
		}
	}
	return out
}

func hiddenFindings(text string) []secretscan.Finding {
	if !strings.ContainsFunc(text, safetext.Invisible) {
		return nil // the common case: most files have only spaces and line ends
	}
	rs := []rune(text)
	var out []secretscan.Finding
	seen := map[secretscan.Finding]bool{}
	line := 1
	for i := 0; i < len(rs); i++ {
		r := rs[i]
		if r == '\n' {
			line++
			continue
		}
		if !safetext.Invisible(r) || allowedHidden(rs, i) {
			continue
		}
		if end := flagSequenceEnd(rs, i); end > i {
			i = end
			continue
		}
		rule := RuleInvisibleCharacter
		if isTag(r) {
			rule = RuleTagCharacter
		}
		f := secretscan.Finding{Rule: rule, Line: line}
		if !seen[f] {
			seen[f] = true
			out = append(out, f)
		}
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].Line < out[j].Line })
	if len(out) > maxHiddenFindings {
		out = out[:maxHiddenFindings]
	}
	return out
}

func isTag(r rune) bool { return r >= 0xE0000 && r <= 0xE007F }

// flagSequences are the only subdivision flags Unicode recommends: England, Scotland
// and Wales. A black flag followed by any other tags could carry hidden text.
var flagSequences = []string{"gbeng", "gbsct", "gbwls"}

// flagSequenceEnd returns the index of the cancel tag closing one of those flags
// (black flag, the name as tags, then cancel) when the tag at i starts one, else -1.
func flagSequenceEnd(rs []rune, i int) int {
	if i == 0 || rs[i-1] != 0x1F3F4 {
		return -1
	}
next:
	for _, name := range flagSequences {
		end := i + len(name)
		if end >= len(rs) || rs[end] != 0xE007F {
			continue
		}
		for j, c := range name {
			if rs[i+j] != 0xE0000+c {
				continue next
			}
		}
		return end
	}
	return -1
}

// allowedHidden reports an invisible character at i that ordinary text needs, and
// which can't join or split the ASCII a credential or a hidden instruction is
// written in:
//   - tab and carriage return;
//   - a byte order mark at the very start, which some editors add;
//   - zero-width joiner and non-joiner between non-ASCII characters: emoji sequences,
//     and Persian and Indic writing;
//   - left-to-right and right-to-left marks beside a non-ASCII character, for
//     Hebrew and Arabic text;
//   - one text or emoji variation selector after a non-ASCII character, or on a
//     keycap (1, #, * and the like).
//
// Everything else invisible is refused, including bidirectional overrides and the
// other variation selectors, which can carry hidden data.
func allowedHidden(rs []rune, i int) bool {
	r := rs[i]
	wide := func(j int) bool { return j >= 0 && j < len(rs) && rs[j] >= utf8.RuneSelf }
	switch r {
	case '\t', '\r':
		return true
	case 0xFEFF:
		return i == 0
	case 0x200C, 0x200D:
		return wide(i-1) && wide(i+1)
	case 0x200E, 0x200F:
		return wide(i-1) || wide(i+1)
	case 0xFE0E, 0xFE0F:
		if i == 0 || rs[i-1] == 0xFE0E || rs[i-1] == 0xFE0F {
			return false
		}
		if rs[i-1] >= utf8.RuneSelf {
			return true
		}
		return i+1 < len(rs) && rs[i+1] == 0x20E3
	}
	return false
}

// stripHidden removes every invisible character, allowed or not, keeping line
// breaks and so line numbers.
func stripHidden(text string) string {
	if !strings.ContainsFunc(text, safetext.Invisible) {
		return text
	}
	return strings.Map(func(r rune) rune {
		if r != '\n' && r != '\t' && safetext.Invisible(r) {
			return -1
		}
		return r
	}, text)
}
