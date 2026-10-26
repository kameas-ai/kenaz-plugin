package skills

import (
	"os"
	"path/filepath"
	"sort"
	"testing"
	"time"

	"github.com/kameas-ai/kenaz-plugin/fleetapi"
)

// envSyncer is one Fleet environment's relay on a machine whose skill
// folders it shares with every other environment.
func envSyncer(t *testing.T, home, fleetEnv string) (*Syncer, *fakeFleet) {
	t.Helper()
	fake := newFake()
	return &Syncer{
		Provider: fake, Store: NewStore(t.TempDir()), MachineID: "node-" + fleetEnv,
		Mat: &Materializer{Roots: DetectRoots(home), Env: fleetEnv, Now: func() time.Time { return time.Unix(1_800_000_000, 0) }},
	}, fake
}

// skillDirs lists the managed skill folders in ~/.claude/skills with the
// environment their marker names.
func skillDirs(t *testing.T, home string) []string {
	t.Helper()
	ents, _ := os.ReadDir(filepath.Join(home, ".claude", "skills"))
	var out []string
	for _, e := range ents {
		mk, err := ReadMarker(filepath.Join(home, ".claude", "skills", e.Name()))
		if err != nil {
			out = append(out, e.Name()+" (unmarked)")
			continue
		}
		out = append(out, e.Name()+" "+mk.FleetEnv()+" "+mk.CatalogID)
	}
	sort.Strings(out)
	return out
}

func mustSync(t *testing.T, s *Syncer) {
	t.Helper()
	if _, err := s.Sync(t.Context()); err != nil {
		t.Fatal(err)
	}
}

func equal(t *testing.T, what string, got []string, want ...string) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("%s:\n got %q\nwant %q", what, got, want)
	}
	for i := range got {
		if got[i] != want[i] {
			t.Fatalf("%s:\n got %q\nwant %q", what, got, want)
		}
	}
}

// The prod and dev relays (of one build or two) share ~/.claude/skills
// and ~/.agents/skills. Each installs into its own folder names and syncs,
// revokes, switches accounts and purges only its own environment's skills
// (issue #34 F1).
func TestSkillsOfTwoEnvsShareFolders(t *testing.T) {
	home := t.TempDir()
	for _, d := range []string{".claude", ".codex"} {
		os.MkdirAll(filepath.Join(home, d), 0o755)
	}
	prod, prodFleet := envSyncer(t, home, "prod")
	dev, devFleet := envSyncer(t, home, "dev")

	review := skillPayload(t, "review", "", nil)
	lint := skillPayload(t, "lint", "", nil)
	// Catalog ids come from different Fleets and may repeat.
	prodFleet.setBundle("bp", "p1",
		fleetapi.MandatedItem{CatalogID: "cat-1", Kind: fleetapi.CatalogKindAgentSkill, Version: "1", Payload: review},
		fleetapi.MandatedItem{CatalogID: "cat-2", Kind: fleetapi.CatalogKindAgentSkill, Version: "1", Payload: lint})
	devFleet.setBundle("bd", "d1",
		fleetapi.MandatedItem{CatalogID: "cat-1", Kind: fleetapi.CatalogKindAgentSkill, Version: "1", Payload: review})

	mustSync(t, prod)
	mustSync(t, dev)
	both := []string{"kameas-dev-review dev cat-1", "kameas-lint prod cat-2", "kameas-review prod cat-1"}
	equal(t, "both synced", skillDirs(t, home), both...)

	// Each sync's revocation sweep keeps the other environment's skills.
	for i := 0; i < 2; i++ {
		mustSync(t, prod)
		mustSync(t, dev)
	}
	equal(t, "repeated syncs", skillDirs(t, home), both...)
	if o, _ := dev.Mat.Owned(); len(o) != 2 { // one folder in each of the two roots
		t.Fatalf("dev owns %d folders", len(o))
	}

	// dev revokes its skill: only dev's folder goes.
	devFleet.setBundle("bd", "d2")
	mustSync(t, dev)
	equal(t, "dev revoked", skillDirs(t, home), "kameas-lint prod cat-2", "kameas-review prod cat-1")

	// Another account signs in to dev: dev's skills are dropped, prod's
	// stay.
	devFleet.setBundle("bd", "d3",
		fleetapi.MandatedItem{CatalogID: "cat-1", Kind: fleetapi.CatalogKindAgentSkill, Version: "1", Payload: review})
	mustSync(t, dev)
	devFleet.mu.Lock()
	devFleet.user = "u2"
	devFleet.mu.Unlock()
	devFleet.setBundle("bd", "d4")
	mustSync(t, dev)
	equal(t, "dev account switch", skillDirs(t, home), "kameas-lint prod cat-2", "kameas-review prod cat-1")

	// Logging out of dev purges dev's skills only; logging out of prod
	// leaves dev's.
	devFleet.setBundle("bd", "d5",
		fleetapi.MandatedItem{CatalogID: "cat-1", Kind: fleetapi.CatalogKindAgentSkill, Version: "1", Payload: review})
	mustSync(t, dev)
	if err := (&Lane{Syncer: dev}).Purge(); err != nil {
		t.Fatal(err)
	}
	equal(t, "dev logout", skillDirs(t, home), "kameas-lint prod cat-2", "kameas-review prod cat-1")
	mustSync(t, dev)
	if err := (&Lane{Syncer: prod}).Purge(); err != nil {
		t.Fatal(err)
	}
	equal(t, "prod logout", skillDirs(t, home), "kameas-dev-review dev cat-1")
	if _, err := os.Stat(filepath.Join(home, ".agents", "skills", "kameas-dev-review")); err != nil {
		t.Fatal("prod logout removed dev's Codex skill")
	}
}

// Markers written before they recorded the environment are prod's; a prod
// skill whose slug starts with "dev-" never takes over the dev folder of the
// same name; each environment's interrupted swaps are cleaned up only by
// that environment.
func TestSkillsEnvOwnershipEdges(t *testing.T) {
	home := t.TempDir()
	os.MkdirAll(filepath.Join(home, ".claude"), 0o755)
	root := filepath.Join(home, ".claude", "skills")
	prodMat := &Materializer{Roots: DetectRoots(home), Env: "prod"}
	devMat := &Materializer{Roots: DetectRoots(home), Env: "dev"}

	old := filepath.Join(root, "kameas-old")
	os.MkdirAll(old, 0o755)
	os.WriteFile(filepath.Join(old, MarkerName), []byte(`{"schema":1,"catalog_id":"cat-9","slug":"old","version":"1"}`), 0o644)
	if o, _ := prodMat.Owned(); len(o) != 1 || o[0].Marker.CatalogID != "cat-9" {
		t.Fatalf("prod does not own an env-less marker: %+v", o)
	}
	if o, _ := (&Materializer{Roots: DetectRoots(home)}).Owned(); len(o) != 1 {
		t.Fatal("an Env-less Materializer is not prod")
	}
	if o, _ := devMat.Owned(); len(o) != 0 {
		t.Fatalf("dev owns a prod marker: %+v", o)
	}
	if n, _ := devMat.Remove("cat-9"); n != 0 {
		t.Fatal("dev removed a prod skill")
	}

	if r := devMat.Install(Item{CatalogID: "cat-1", Kind: fleetapi.CatalogKindAgentSkill, Version: "1", Payload: skillPayload(t, "review", "", nil), Source: SourceMandated}); r.Status != fleetapi.AckApplied {
		t.Fatalf("dev install: %+v", r)
	}
	r := prodMat.Install(Item{CatalogID: "cat-1", Kind: fleetapi.CatalogKindAgentSkill, Version: "1", Payload: skillPayload(t, "dev-review", "", nil), Source: SourceMandated})
	if r.Status != fleetapi.AckApplied || len(r.Dirs) != 1 || filepath.Base(r.Dirs[0]) == "kameas-dev-review" {
		t.Fatalf("prod dev-review took dev's folder: %+v", r)
	}
	if mk, _ := ReadMarker(filepath.Join(root, "kameas-dev-review")); mk.FleetEnv() != "dev" {
		t.Fatalf("dev folder now %+v", mk)
	}

	for _, d := range []string{".kameas-tmp-1", ".kameas-old-1", ".kameas-dev-tmp-1", ".kameas-dev-old-1"} {
		os.MkdirAll(filepath.Join(root, d), 0o755)
	}
	prodMat.Owned()
	var left []string
	ents, _ := os.ReadDir(root)
	for _, e := range ents {
		if e.Name()[0] == '.' {
			left = append(left, e.Name())
		}
	}
	equal(t, "after prod cleanup", left, ".kameas-dev-old-1", ".kameas-dev-tmp-1")
	devMat.Owned()
	if _, err := os.Stat(filepath.Join(root, ".kameas-dev-tmp-1")); !os.IsNotExist(err) {
		t.Fatal("dev did not clean its own swap")
	}
}

// removeOwned and the swap re-check the marker's environment at the moment
// they act, so a folder another environment took over (or a list that went
// stale) is never removed or replaced.
func TestRemoveRechecksEnv(t *testing.T) {
	home := t.TempDir()
	os.MkdirAll(filepath.Join(home, ".claude"), 0o755)
	prodMat := &Materializer{Roots: DetectRoots(home), Env: "prod"}
	devMat := &Materializer{Roots: DetectRoots(home), Env: "dev"}
	r := prodMat.Install(Item{CatalogID: "cat-1", Kind: fleetapi.CatalogKindAgentSkill, Version: "1", Payload: skillPayload(t, "review", "", nil), Source: SourceMandated})
	if r.Status != fleetapi.AckApplied {
		t.Fatalf("%+v", r)
	}
	dir := r.Dirs[0]
	if err := devMat.removeOwned(dir); err == nil {
		t.Fatal("dev removed prod's skill")
	}
	if err := devMat.writeAtomic(filepath.Dir(dir), dir, map[string]string{ManifestName: "x"}, Marker{CatalogID: "cat-1", Env: "dev"}); err == nil {
		t.Fatal("dev replaced prod's skill")
	}
	if _, err := ReadMarker(dir); err != nil {
		t.Fatal("prod's skill is gone")
	}
	if err := prodMat.removeOwned(dir); err != nil {
		t.Fatal(err)
	}
}
