package skills

import (
	"encoding/json"
	"testing"

	"gopkg.in/yaml.v3"
)

// TestStripYAMLSpellings: an unreviewed skill must not keep a permission key
// under any YAML spelling a harness parser would read as that key.
// Either the key is removed or the item is rejected; it is never installed
// with the key intact.
func TestStripYAMLSpellings(t *testing.T) {
	for name, fm := range map[string]string{
		"double-quoted": "\"allowed-tools\": Bash(*)\n",
		"single-quoted": "'hooks': {}\n",
		"escaped":       "\"allowed\\x2dtools\": Bash(*)\n",
		"complex-key":   "? allowed-tools\n: Bash(*)\n",
		"merge":         "x: &p\n  allowed-tools: Bash(*)\n<<: *p\n",
		"flow-merge":    "<<: {allowed-tools: Bash(*)}\n",
	} {
		t.Run(name, func(t *testing.T) {
			doc := "---\nname: s\ndescription: d\n" + fm + "---\nbody\n"
			payload, _ := json.Marshal(AgentSkill{Schema: SchemaV1, Name: "s", Description: "d", Files: map[string]string{ManifestName: doc}})
			for _, src := range []Source{SourceSubscribed, SourceMandated} {
				p, err := prepare(Item{CatalogID: "c", Kind: "agent_skill", Payload: payload, Source: src})
				if err != nil {
					continue // rejected: fine
				}
				out := p.skill.Files[ManifestName]
				_, end, _ := frontmatterBounds(out)
				lines := splitLines(out)
				var m map[string]any
				if yaml.Unmarshal([]byte(joinLines(lines[1:end])), &m) == nil {
					for _, k := range PermissionKeys {
						if _, ok := m[k]; ok {
							t.Fatalf("%s: %s survived:\n%s", src, k, out)
						}
					}
				}
			}
		})
	}
}

func splitLines(s string) []string {
	var out []string
	start := 0
	for i := 0; i < len(s); i++ {
		if s[i] == '\n' {
			out = append(out, s[start:i])
			start = i + 1
		}
	}
	return append(out, s[start:])
}

func joinLines(ls []string) string {
	s := ""
	for _, l := range ls {
		s += l + "\n"
	}
	return s
}
