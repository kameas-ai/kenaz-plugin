package skills

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
)

// ErrUnsupportedToolSkill marks a legacy kind=tool skill. It names a
// kenaz-harness tool, which neither Claude Code nor Codex has, so it is
// skipped and acked failed/unsupported_tool_skill.
var ErrUnsupportedToolSkill = errors.New("skills: legacy tool skill is not supported outside kenaz-harness")

// legacySkill is the subset of kenaz-harness slashcmd.Skill
// (core/slashcmd/store.go) that translation reads.
type legacySkill struct {
	Trigger     string        `json:"trigger"`
	Kind        string        `json:"kind"`
	Description string        `json:"description"`
	Body        string        `json:"body"`
	Inputs      []legacyInput `json:"inputs"`
}

type legacyInput struct {
	Name       string   `json:"name"`
	Kind       string   `json:"kind"`
	Required   bool     `json:"required"`
	EnumValues []string `json:"enumValues"`
	Default    string   `json:"default"`
	Hint       string   `json:"hint"`
}

// TranslateLegacy turns a legacy catalog "skill" payload of kind text or
// prompt into an agent_skill: name from trigger, description from
// description, body as markdown, inputs as an "Inputs" section.
func TranslateLegacy(payload []byte) (*AgentSkill, error) {
	if len(payload) > MaxBytes {
		return nil, invalid("payload", "exceeds %d bytes", MaxBytes)
	}
	var l legacySkill
	if err := json.NewDecoder(bytes.NewReader(payload)).Decode(&l); err != nil {
		return nil, invalid("payload", "not a legacy skill object")
	}
	switch l.Kind {
	case "tool":
		return nil, ErrUnsupportedToolSkill
	case "text", "prompt":
	default:
		return nil, invalid("kind", "unknown legacy skill kind %q", l.Kind)
	}
	name := strings.ReplaceAll(strings.ToLower(strings.TrimSpace(l.Trigger)), "_", "-")
	desc := strings.Join(strings.Fields(l.Description), " ")
	if desc == "" {
		desc = "Shared /" + l.Trigger + " command from Kameas Fleet."
	}
	var b strings.Builder
	fmt.Fprintf(&b, "---\nname: %s\ndescription: %s\n---\n\n", name, yamlScalar(desc))
	fmt.Fprintf(&b, "# /%s\n\n", l.Trigger)
	if l.Kind == "text" {
		b.WriteString("Insert the following text verbatim:\n\n")
	}
	b.WriteString(strings.TrimSpace(l.Body))
	b.WriteString("\n")
	if len(l.Inputs) > 0 {
		b.WriteString("\n## Inputs\n\n")
		for _, in := range l.Inputs {
			fmt.Fprintf(&b, "- `%s`", in.Name)
			var notes []string
			if in.Required {
				notes = append(notes, "required")
			}
			if len(in.EnumValues) > 0 {
				notes = append(notes, "one of: "+strings.Join(in.EnumValues, ", "))
			}
			if in.Default != "" {
				notes = append(notes, "default: "+in.Default)
			}
			if len(notes) > 0 {
				b.WriteString(" (" + strings.Join(notes, "; ") + ")")
			}
			if in.Hint != "" {
				b.WriteString(": " + in.Hint)
			}
			b.WriteString("\n")
		}
	}
	s := &AgentSkill{Schema: SchemaV1, Name: name, Description: desc, Files: map[string]string{ManifestName: b.String()}}
	if err := s.Validate(); err != nil {
		return nil, err
	}
	return s, nil
}

// yamlScalar quotes s when a plain YAML scalar would be misread.
func yamlScalar(s string) string {
	if s == "" || strings.ContainsAny(s, ":#'\"{}[],&*!|>%@`") || strings.HasPrefix(s, "-") || strings.HasPrefix(s, "?") {
		return `"` + strings.ReplaceAll(strings.ReplaceAll(s, `\`, `\\`), `"`, `\"`) + `"`
	}
	return s
}
