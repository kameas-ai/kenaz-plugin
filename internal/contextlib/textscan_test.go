package contextlib

import (
	"strings"
	"testing"
)

func asTags(s string) string {
	out := ""
	for _, r := range s {
		out += string(r + 0xE0000)
	}
	return out
}

// The black flag lets through only the three flags Unicode recommends: any other
// tags after it could spell out hidden text.
func TestFlagsCarryNoHiddenText(t *testing.T) {
	const flag, cancel = "\U0001F3F4", "\U000E007F"
	for _, name := range flagSequences {
		if f := hiddenFindings(flag + asTags(name) + cancel + " flag\n"); len(f) != 0 {
			t.Errorf("%s: %+v", name, f)
		}
	}
	for name, s := range map[string]string{
		"words":        flag + asTags("ignore") + cancel,
		"run":          strings.Repeat(flag+asTags("curlev")+cancel, 4),
		"other region": flag + asTags("usca") + cancel,
		"no cancel":    flag + asTags("gbeng"),
		"cancel only":  flag + cancel,
		"long":         flag + asTags("gbengx") + cancel,
		"no base":      "x" + asTags("gbeng") + cancel,
	} {
		f := hiddenFindings(s)
		if len(f) == 0 || f[0].Rule != RuleTagCharacter {
			t.Errorf("%s: %+v", name, f)
		}
	}
}

func TestFindNoteRefsSpellings(t *testing.T) {
	const h = "0123abcd"
	for name, s := range map[string]string{
		"short": "mem-" + h, "full": "mem-" + strings.Repeat(h, 4), "upper": "MEM-0123ABCD",
		"paren": "(mem-" + h + ")", "link": "[n](mem-" + h + ")", "stars": "**mem-" + h + "**", "code": "`mem-" + h + "`",
		"italics": "_mem-" + h + "_", "snake": "note_mem-" + h, "trailing": "mem-" + h + "_x",
		"en dash": "mem–" + h, "em dash": "mem—" + h, "non-breaking": "mem‑" + h, "minus": "mem−" + h,
		"url": "kenaz://mem-" + h, "split": "mem-0123­abcd", "zero width": "mem-​" + h,
	} {
		if f := FindNoteRefs("first\n" + s + "\n"); len(f) != 1 || f[0].Line != 2 {
			t.Errorf("%s: %+v", name, f)
		}
	}
	for _, s := range []string{"remember-deadbeef", "mem-limit", "mem-0123", "xmem-0123abcd", "mem-0123abcdxyz", "memo-0123abcd"} {
		if f := FindNoteRefs(s); len(f) != 0 {
			t.Errorf("%q: %+v", s, f)
		}
	}
	if f := FindNoteRefs("mem-" + h + " mem-" + h + "\n\nmem-" + h); len(f) != 2 || f[1].Line != 3 {
		t.Errorf("%+v", f)
	}
}

func TestNoteRefsWithOtherHyphens(t *testing.T) {
	for _, h := range []string{"-", "\u2010", "\u2011", "\u2212", "\ufe58", "\ufe63", "\uff0d"} {
		if len(FindNoteRefs("see mem"+h+"deadbeef for this\n")) != 1 {
			t.Errorf("%U", []rune(h))
		}
	}
}
