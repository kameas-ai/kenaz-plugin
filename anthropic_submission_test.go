package kenaz

import (
	"regexp"
	"slices"
	"strings"
	"testing"
	"unicode/utf8"
)

// docs/directory/ANTHROPIC-SUBMISSION.md prepares the plugin bundle and the
// hosted connector listings for Anthropic's directory. The connector listing
// is typed into the developer portal from that file, so this test holds its
// fields to the portal's limits, the way TestOpenAIPackage holds openai/ to
// OpenAI's.

const anthropicSubmission = "docs/directory/ANTHROPIC-SUBMISSION.md"

// anthropicConnectorTools are the hosted connector's tools in tool list v7:
// the tools the OpenAI package may name, with my_teams (which that list
// gains when the OpenAI package starts using it).
var anthropicConnectorTools = withTool(hostedTools, "my_teams")

func withTool(tools []string, name string) []string {
	if slices.Contains(tools, name) {
		return tools
	}
	return append(slices.Clone(tools), name)
}

var listingFieldRe = regexp.MustCompile("(?s)<!-- listing:([a-z-]+) -->\n```text\n(.*?)\n```")

func TestAnthropicConnectorListing(t *testing.T) {
	doc := readText(t, anthropicSubmission)

	fields := map[string]string{}
	for _, m := range listingFieldRe.FindAllStringSubmatch(doc, -1) {
		if _, dup := fields[m[1]]; dup {
			t.Errorf("listing field %q appears twice", m[1])
		}
		fields[m[1]] = m[2]
	}

	limits := map[string]int{"name": 100, "one-liner": 200, "description": 2000}
	for field, limit := range limits {
		v, ok := fields[field]
		if !ok || strings.TrimSpace(v) == "" {
			t.Errorf("listing field %q is missing", field)
			continue
		}
		if strings.Contains(v, "\n") {
			t.Errorf("listing field %q must be one line", field)
		}
		if n := utf8.RuneCountInString(v); n > limit {
			t.Errorf("listing field %q is %d characters, over the portal's %d", field, n, limit)
		}
		if m := promotion.FindString(v); m != "" {
			t.Errorf("listing field %q has promotion or encryption wording %q", field, m)
		}
	}
	if thirdPartyMark.MatchString(fields["name"]) || thirdPartyMark.MatchString(fields["slug"]) {
		t.Error("the connector name and slug may not contain a third-party mark")
	}
	if fields["slug"] != "kenaz" {
		t.Errorf("slug = %q, want kenaz", fields["slug"])
	}
	if n := len(strings.Split(strings.TrimSpace(fields["categories"]), "\n")); fields["categories"] == "" || n > 5 {
		t.Errorf("the listing needs one to five categories, has %d", n)
	}
	const attribution = "Claude and Cowork are trademarks of Anthropic, PBC. Kenaz and Kameas are trademarks of Kameas AI, Inc. Kenaz is an independent product of Kameas AI, Inc. and is not affiliated with, endorsed or sponsored by Anthropic."
	if !strings.HasSuffix(fields["description"], attribution) {
		t.Error("the connector description must end with its Anthropic attribution")
	}

	// The plugin bundle's attribution is the approved Anthropic-only form,
	// and the listing description the packet quotes is the manifest's, which
	// is what the portal reads.
	if !strings.Contains(doc, DisclaimerGAnthropic) {
		t.Error("the plugin bundle listing must carry the Anthropic attribution verbatim")
	}
	var manifest struct{ Description string }
	readJSON(t, "plugin/.claude-plugin/plugin.json", &manifest)
	if !strings.Contains(doc, manifest.Description) {
		t.Error("the packet must quote plugin/.claude-plugin/plugin.json's description verbatim")
	}

	// Example prompts: at least three, naming only hosted tools, and one
	// that finds a team with my_teams.
	start := strings.Index(doc, "<!-- example-prompts -->")
	end := strings.Index(doc, "<!-- /example-prompts -->")
	if start < 0 || end < start {
		t.Fatal("example prompts table not found")
	}
	rows, named := 0, map[string]bool{}
	for _, line := range strings.Split(doc[start:end], "\n") {
		cells := strings.Split(line, "|")
		if len(cells) < 5 || strings.TrimSpace(cells[1]) == "#" || strings.HasPrefix(strings.TrimSpace(cells[1]), "-") {
			continue
		}
		rows++
		tools := regexp.MustCompile("`([a-z_]+)`").FindAllStringSubmatch(cells[3], -1)
		if len(tools) == 0 {
			t.Errorf("example prompt %s names no tool", strings.TrimSpace(cells[1]))
		}
		for _, m := range tools {
			named[m[1]] = true
			if !slices.Contains(anthropicConnectorTools, m[1]) {
				t.Errorf("example prompt %s names %q, which the connector doesn't have", strings.TrimSpace(cells[1]), m[1])
			}
		}
	}
	if rows < 3 {
		t.Errorf("the directory policy asks for at least three example prompts, found %d", rows)
	}
	if !named["my_teams"] {
		t.Error("an example prompt must use my_teams to find the team to share with")
	}
}
