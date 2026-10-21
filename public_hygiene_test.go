package kenaz

import (
	"bytes"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// The public-repository hygiene gate keeps internal-only references out of
// this repository: names of non-public repositories, paths to non-public
// documents, internal design-document and decision references, legal
// process notes, and personal names or hostnames in fixtures.
//
// Every banned word below is assembled from pieces so that this file does
// not match itself. TestHygieneGateCatches shows one example per category.

// bannedSubstrings are matched case-insensitively anywhere in a file.
var bannedSubstrings = []string{
	"legal" + "-drafts",
	"coun" + "sel",
	"privi" + "leged",
	"kenaz" + "-fleet", // also covers the proprietary client module's repository
	"kenaz-plugin" + "-release",
	"kameas" + "-fleet" + "-release",
	"harness" + "-plugins",
	"private " + "release",
	"private " + "repo",
	"legal " + "review",
	"legal " + "package",
	"work" + "space/specs",
	"../" + "work" + "space",
}

// bannedPatterns are matched against the whole file (case-sensitive unless
// the pattern says otherwise).
var bannedPatterns = []struct {
	why string
	re  *regexp.Regexp
}{
	// Any section sign: legal documents are cited by section, code is not.
	{"section sign", regexp.MustCompile(`\x{00a7}`)},
	// A legal hold, written in capitals.
	{"legal hold", regexp.MustCompile(`\b` + "HO" + `LD\b`)},
	// Citations of a numbered legal document.
	{"document citation", regexp.MustCompile(`(?i)\b(?:le` + `gal|do` + `c) [0-9]{2}\b`)},
	// Decision references in their call form, or qualified by a design
	// document or a decision series.
	{"decision reference", regexp.MustCompile(`\b[D][0-9]{1,3}\(|\b(?:README|plan) [D][0-9]+\b|\b(?:RC|WB)-[D][0-9]+\b`)},
	// Internal design-spec numbers and requirement or criterion ids.
	{"internal spec reference", regexp.MustCompile(`(?i)\bspecs? [0-9]{3}\b|\b[0-9]{3} (?:FR|SC|AC)-[0-9]|\b(?:FR|SC|AC|NFR)-[0-9]+\b`)},
	// Personal machine names (a possessive first name before the model).
	{"personal hostname", regexp.MustCompile(`(?i)\b[a-z]+s-mac` + `book`)},
}

// commentDecisionRe finds a bare decision id inside a comment line. It is
// limited to comments (and Markdown) so test data and identifiers stay free
// to use the letter followed by digits.
var commentDecisionRe = regexp.MustCompile(`\b[D][0-9]{1,3}\b`)

// commentSpecRe finds bare internal spec numbers and spec-style
// phrasing inside comments and Markdown.
var commentSpecRe = regexp.MustCompile(`(?i)\b0(?:0[5-9]|1[0-4])(?:'s)?\b|\bspec-[0-9]{3}\b|\bREADME rule\b|\blater specs?\b|\bspec reference\b`)

// homeDirRe finds macOS home directories in fixtures and examples. Only
// the synthetic users below may appear.
var homeDirRe = regexp.MustCompile(`/Users/([A-Za-z0-9._-]+)`)

var allowedHomeUsers = map[string]bool{"u": true, "alex": true, "me": true, "user": true, "runner": true}

// approvedSentences may appear verbatim even if a future edit of the
// patterns would match them.
var approvedSentences = []string{OfficialBuildsStatement, OpenCoreStatement}

// hygieneSkipFiles are generated or licence files kept byte-identical.
var hygieneSkipFiles = map[string]bool{
	".git": true, "go.mod": true, "go.sum": true,
	"LICENSE": true, "NOTICE": true, "THIRD_PARTY_NOTICES": true,
}

// hygieneSkipPaths are exempt by prefix. notices/ holds served texts whose
// hashes are pinned downstream.
var hygieneSkipPaths = []string{"notices/"}

var hygieneSkipDirs = map[string]bool{".git": true, "bin": true, "dist": true, "node_modules": true}

// hygieneViolations returns every internal reference found in one file.
func hygieneViolations(path, content string) []string {
	for _, s := range approvedSentences {
		content = strings.ReplaceAll(content, s, "")
	}
	var out []string
	lower := strings.ToLower(content)
	for _, w := range bannedSubstrings {
		if strings.Contains(lower, w) {
			out = append(out, "internal reference "+quote(w))
		}
	}
	for _, p := range bannedPatterns {
		if m := p.re.FindString(content); m != "" {
			out = append(out, p.why+" "+quote(m))
		}
	}
	prose := strings.HasSuffix(path, ".md")
	for _, line := range strings.Split(content, "\n") {
		comment := prose
		if i := strings.Index(line, "//"); i >= 0 {
			comment, line = true, line[i:]
		} else if strings.HasPrefix(strings.TrimSpace(line), "#") {
			comment = true
		}
		if !comment {
			continue
		}
		if m := commentDecisionRe.FindString(line); m != "" {
			out = append(out, "decision reference "+quote(m))
		}
		if m := commentSpecRe.FindString(line); m != "" {
			out = append(out, "internal spec reference "+quote(m))
		}
	}
	for _, m := range homeDirRe.FindAllStringSubmatch(content, -1) {
		if !allowedHomeUsers[m[1]] {
			out = append(out, "personal home directory "+quote(m[0]))
		}
	}
	return out
}

func quote(s string) string { return `"` + s + `"` }

// TestNoInternalReferences walks the tree and fails on any internal
// reference outside the exempt paths.
func TestNoInternalReferences(t *testing.T) {
	err := filepath.WalkDir(".", func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			if hygieneSkipDirs[d.Name()] {
				return filepath.SkipDir
			}
			return nil
		}
		if hygieneSkipFiles[filepath.Base(path)] {
			return nil
		}
		slash := filepath.ToSlash(path)
		for _, p := range hygieneSkipPaths {
			if strings.HasPrefix(slash, p) {
				return nil
			}
		}
		b, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		if bytes.IndexByte(b, 0) >= 0 {
			return nil // binary
		}
		for _, v := range hygieneViolations(slash, string(b)) {
			t.Errorf("%s: %s", slash, v)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

// TestHygieneGateCatches seeds one violation per category so the gate
// cannot silently stop matching, and checks the allowed forms pass.
func TestHygieneGateCatches(t *testing.T) {
	bad := map[string]string{
		"decision id in a comment": "// holds uploads (" + "D" + "45).\n",
		"decision id in Markdown":  "Per " + "D" + "88, uploads stop.\n",
		"decision call form":       "see " + "D" + "61(b)\n",
		"design-doc decision":      "x := 1 /* (README " + "D" + "1) */\n",
		"section sign":             "// see 03 \xc2\xa7" + "2\n",
		"document citation":        "// per " + "legal" + " 04\n",
		"doc citation":             "// see " + "doc" + " 10\n",
		"not confirmed note":       "// not confirmed by " + "coun" + "sel\n",
		"legal hold":               "// " + "HO" + "LD until signed\n",
		"confidentiality marking":  "// " + "privi" + "leged and confidential\n",
		"drafts path":              "p := \"../work" + "space/" + "legal" + "-drafts/03.md\"\n",
		"workspace path":           "p := \"work" + "space/specs/x\"\n",
		"spec path":                "// spec " + "harness" + "-plugins/" + "00" + "5\n",
		"spec number":              "// (" + "spec" + " 00" + "9)\n",
		"requirement id":           "// " + "FR" + "-8 order\n",
		"bare spec number":         "// the " + "00" + "6's receiver\n",
		"spec-dash form":           "// the " + "spec" + "-00" + "6 receiver\n",
		"readme rule":              "// (" + "README" + " rule 4)\n",
		"later specs":              "// extended by " + "later" + " specs\n",
		"client repo":              "kenaz" + "-fleet-client\n",
		"release repo":             "kenaz-plugin" + "-release\n",
		"former release repo":      "kameas" + "-fleet" + "-release\n",
		"release pipeline wording": "// built by the " + "private " + "release" + " pipeline\n",
		"home directory":           "p := \"/Users/" + "jordan" + "/project\"\n",
		"hostname":                 "h := \"" + "jordan" + "s-MacBook-Pro.local\"\n",
	}
	for name, s := range bad {
		path := "x.go"
		if strings.Contains(name, "Markdown") {
			path = "x.md"
		}
		if len(hygieneViolations(path, s)) == 0 {
			t.Errorf("%s: not caught: %q", name, s)
		}
	}
	// The wire constants keep the former name and must stay allowed.
	wire := "kameas" + "-fleet"
	good := []string{
		"name := \"" + wire + "\"\n",
		"scope := \"" + wire + "/projector\"\n",
		"label := \"" + wire + "/session-hash/v1\"\n",
		"x := []byte{0xD4, 0x1D}\n",
		"// HTTP 200 OK, 6 h heartbeat\n",
		"p := \"/Users/u/Library/Application Support\"\n",
		"// the hold clears when the item clears\n",
		OfficialBuildsStatement + "\n",
		OpenCoreStatement + "\n",
	}
	for _, s := range good {
		if v := hygieneViolations("x.go", s); len(v) != 0 {
			t.Errorf("false positive on %q: %v", s, v)
		}
	}
}
