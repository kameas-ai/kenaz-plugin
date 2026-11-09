package safetext

import (
	"strings"
	"testing"
	"unicode"
)

func TestTerminalEveryRune(t *testing.T) {
	var kept []rune
	for r := rune(0); r <= 0x10FFFF; r++ {
		if r >= 0xD800 && r <= 0xDFFF {
			continue
		}
		out := Terminal(string(r))
		if out != string(r) {
			continue
		}
		if r == '\n' || r == '\t' {
			continue
		}
		if unicode.IsControl(r) || unicode.Is(unicode.Cf, r) || unicode.Is(unicode.Zl, r) || unicode.Is(unicode.Zp, r) {
			kept = append(kept, r)
		}
	}
	for _, r := range kept {
		if r != 0x200C && r != 0x200D {
			t.Errorf("%U kept", r)
		}
	}
	for _, raw := range []string{"\x9b1A", "\xc2\x9b1A", "\xed\xa0\x80", "\xf4\x90\x80\x80", "\x1b]52;c;aGk=\x07"} {
		out := Terminal(raw)
		if strings.ContainsAny(out, "\x1b\x07\u009b") {
			t.Errorf("%q -> %q", raw, out)
		}
	}
}
