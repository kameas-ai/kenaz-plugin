// Package safetext makes untrusted text safe to show on one line of a block an AI
// tool reads as context.
package safetext

import (
	"fmt"
	"strings"
	"unicode"
)

// OneLine reduces text from a file to one line of visible characters, so it
// cannot start a new line in an index, send escape codes to a terminal or hide
// words from a reader. Angle brackets become look-alikes, so it cannot close or
// open a wrapper tag a host puts around the index and step outside its label.
func OneLine(s string, maxRunes int) string {
	s = strings.ToValidUTF8(s, "")
	var b strings.Builder
	n := 0
	space := false
	for _, r := range s {
		if unicode.IsSpace(r) {
			space = n > 0
			continue
		}
		if Invisible(r) {
			continue
		}
		if n >= maxRunes {
			return b.String() + "…"
		}
		if space {
			b.WriteByte(' ')
			n++
			space = false
		}
		switch r {
		case '<':
			r = '‹'
		case '>':
			r = '›'
		}
		b.WriteRune(r)
		n++
	}
	return b.String()
}

// Invisible reports characters a reader cannot see or that break a line: control,
// format and line-separator characters, private-use and unassigned code points,
// variation selectors, and the fillers and blanks that render as nothing.
func Invisible(r rune) bool {
	return !unicode.IsGraphic(r) || r == 0x2800 || unicode.In(r, unicode.Variation_Selector, unicode.Other_Default_Ignorable_Code_Point)
}

// Terminal makes untrusted text safe to print on a terminal, keeping its lines: every
// character a terminal would act on or a reader can't see shows as a visible \\uXXXX, so
// text can't move the cursor or erase what's above it. Line breaks, tabs and the joiners
// and selectors ordinary writing needs stay.
func Terminal(s string) string {
	s = strings.ToValidUTF8(s, "\uFFFD")
	var b strings.Builder
	for _, r := range s {
		switch {
		case r == '\n', r == '\t', r == 0x200C, r == 0x200D, r == 0xFE0E, r == 0xFE0F, !Invisible(r):
			b.WriteRune(r)
		case r <= 0xFFFF:
			fmt.Fprintf(&b, "\\u%04X", r)
		default:
			fmt.Fprintf(&b, "\\U%08X", r)
		}
	}
	return b.String()
}
