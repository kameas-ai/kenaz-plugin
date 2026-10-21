package app

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/kameas-ai/kenaz-plugin/fleetapi"
	"github.com/kameas-ai/kenaz-plugin/fleetapi/stub"
	"github.com/kameas-ai/kenaz-plugin/internal/skills"
)

// TestSkillsLogoutPurgeAndReload: a managed skill is removed by the logout
// purge path even with no daemon running, an unmarked user skill survives,
// and the Claude reload signal fires once per change.
func TestSkillsLogoutPurgeAndReload(t *testing.T) {
	home, err := skillsHome()
	if err != nil {
		t.Fatal(err)
	}
	for _, d := range []string{".claude", ".agents"} {
		if err := os.MkdirAll(filepath.Join(home, d, "skills"), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	userSkill := filepath.Join(home, ".claude", "skills", "my-own")
	if err := os.MkdirAll(userSkill, 0o755); err != nil {
		t.Fatal(err)
	}
	stateDir := t.TempDir()

	fm := "---\nname: review\ndescription: Review carefully.\n---\n\n# review\n"
	p, err := json.Marshal(skills.AgentSkill{Schema: skills.SchemaV1, Name: "review", Description: "Review carefully.", Files: map[string]string{skills.ManifestName: fm}})
	if err != nil {
		t.Fatal(err)
	}
	mat := &skills.Materializer{Roots: skills.DetectRoots(home)}
	if len(mat.Roots) == 0 {
		t.Fatal("no harness roots detected in test home")
	}
	if res := mat.Install(skills.Item{CatalogID: "cat-1", Kind: "agent_skill", Version: "1", Payload: p, Source: skills.SourceSubscribed}); res.Status != fleetapi.AckApplied {
		t.Fatalf("install: %s %s", res.Status, res.Detail)
	}
	if err := skills.NewStore(stateDir).BumpGeneration(); err != nil {
		t.Fatal(err)
	}

	r := skillsReload{stateDir: stateDir}
	if !r.ChangedSinceLastScan(context.Background()) {
		t.Fatal("reload should be needed after an install")
	}
	if r.ChangedSinceLastScan(context.Background()) {
		t.Fatal("reload should be reported once per change")
	}

	if err := purgeSkills(stub.New(), stateDir); err != nil {
		t.Fatalf("purge: %v", err)
	}
	owned, err := mat.Owned()
	if err != nil {
		t.Fatal(err)
	}
	if len(owned) != 0 {
		t.Fatalf("managed skills left after purge: %d", len(owned))
	}
	if _, err := os.Stat(userSkill); err != nil {
		t.Fatalf("unmarked user skill was touched: %v", err)
	}
}
