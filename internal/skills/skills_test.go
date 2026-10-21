package skills

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/kameas-ai/kenaz-plugin/fleetapi"
)

type env struct {
	home  string
	fake  *fakeFleet
	sync  *Syncer
	store Store
}

func newEnv(t *testing.T) *env {
	t.Helper()
	home := t.TempDir()
	for _, d := range []string{".claude", ".codex"} {
		if err := os.MkdirAll(filepath.Join(home, d), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	fake := newFake()
	store := NewStore(t.TempDir())
	return &env{home: home, fake: fake, store: store, sync: &Syncer{
		Provider: fake, Store: store, MachineID: "node-1",
		Mat: &Materializer{Roots: DetectRoots(home), Now: func() time.Time { return time.Unix(1_800_000_000, 0) }},
	}}
}

func (e *env) claudeDir(slug string) string {
	return filepath.Join(e.home, ".claude", "skills", DirPrefix+slug)
}
func (e *env) codexDir(slug string) string {
	return filepath.Join(e.home, ".agents", "skills", DirPrefix+slug)
}

func (e *env) run(t *testing.T) Report {
	t.Helper()
	rep, err := e.sync.Sync(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	return rep
}

func readFile(t *testing.T, p string) string {
	t.Helper()
	b, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// A mandated agent_skill lands in both roots with valid markers and
// is acked applied.
func TestMandatedMaterialize(t *testing.T) {
	e := newEnv(t)
	p := skillPayload(t, "pr-review", "", map[string]string{"references/style.md": "be kind\n", "scripts/lint.sh": "echo hi\n"})
	e.fake.setBundle("b1", "c1", fleetapi.MandatedItem{CatalogID: "cat-1", Kind: fleetapi.CatalogKindAgentSkill, Version: "1.0.0", Payload: p, ReviewedSHA256: PayloadSHA256(p)})
	rep := e.run(t)
	if rep.Applied != 1 || rep.Failed != 0 {
		t.Fatalf("report = %+v", rep)
	}
	for _, d := range []string{e.claudeDir("pr-review"), e.codexDir("pr-review")} {
		mustContain(t, readFile(t, filepath.Join(d, "SKILL.md")), "name: pr-review")
		if readFile(t, filepath.Join(d, "references", "style.md")) != "be kind\n" {
			t.Fatal("resource not written")
		}
		fi, err := os.Stat(filepath.Join(d, "scripts", "lint.sh"))
		if err != nil || fi.Mode().Perm()&0o111 != 0 {
			t.Fatalf("script must exist and not be executable: %v %v", fi.Mode(), err)
		}
		mk, err := ReadMarker(d)
		if err != nil || mk.CatalogID != "cat-1" || mk.Source != SourceMandated || mk.Version != "1.0.0" || mk.SHA256 == "" {
			t.Fatalf("marker = %+v err=%v", mk, err)
		}
	}
	acks := e.fake.takeAcks()
	if len(acks) != 1 || acks[0].Status != fleetapi.AckApplied || acks[0].Detail != "" {
		t.Fatalf("acks = %+v", acks)
	}
	if e.store.Generation() != 1 {
		t.Fatalf("generation = %d", e.store.Generation())
	}

	// Idempotent: an unchanged second pass writes nothing.
	e.run(t)
	if e.store.Generation() != 1 {
		t.Fatalf("unchanged pass bumped generation to %d", e.store.Generation())
	}

	// A user deleting a mandated skill gets it back on the next poll (304
	// path triggers a full re-pull), without anything being locked.
	if err := os.RemoveAll(e.claudeDir("pr-review")); err != nil {
		t.Fatal(err)
	}
	e.run(t)
	if _, err := ReadMarker(e.claudeDir("pr-review")); err != nil {
		t.Fatal("mandated skill not re-materialized after user deletion")
	}
}

// An unmarked directory at the target name is never touched.
func TestNeverTouchUnmarked(t *testing.T) {
	e := newEnv(t)
	d := e.claudeDir("foo")
	if err := os.MkdirAll(d, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(d, "SKILL.md"), []byte("mine\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	p := skillPayload(t, "foo", "", nil)
	e.fake.setBundle("b1", "c1", fleetapi.MandatedItem{CatalogID: "cat-foo", Kind: fleetapi.CatalogKindAgentSkill, Version: "1", Payload: p})
	e.run(t)
	if readFile(t, filepath.Join(d, "SKILL.md")) != "mine\n" {
		t.Fatal("unmarked directory modified")
	}
	if _, err := os.Stat(filepath.Join(d, MarkerName)); !os.IsNotExist(err) {
		t.Fatal("marker written into unmarked directory")
	}
	if _, err := os.Stat(e.codexDir("foo")); !os.IsNotExist(err) {
		t.Fatal("other root written despite the conflict")
	}
	acks := e.fake.takeAcks()
	if len(acks) != 1 || acks[0].Status != fleetapi.AckFailed || acks[0].Detail != DetailNameConflict {
		t.Fatalf("acks = %+v", acks)
	}
	// The sweep must not delete it either.
	e.fake.setBundle("b2", "c2")
	e.run(t)
	if readFile(t, filepath.Join(d, "SKILL.md")) != "mine\n" {
		t.Fatal("sweep touched an unmarked directory")
	}
}

// Two different catalog items with the same slug get distinct directories.
func TestSlugCollisionSuffix(t *testing.T) {
	e := newEnv(t)
	a := skillPayload(t, "dup", "", nil)
	e.fake.setBundle("b1", "c1",
		fleetapi.MandatedItem{CatalogID: "cat-a", Kind: fleetapi.CatalogKindAgentSkill, Version: "1", Payload: a},
		fleetapi.MandatedItem{CatalogID: "cat-b", Kind: fleetapi.CatalogKindAgentSkill, Version: "1", Payload: a})
	rep := e.run(t)
	if rep.Applied != 2 {
		t.Fatalf("report = %+v", rep)
	}
	owned, _ := e.sync.Mat.Owned()
	if len(owned) != 4 {
		t.Fatalf("owned = %d, want 4", len(owned))
	}
}

// Invalid payloads are rejected with nothing written.
func TestPayloadValidation(t *testing.T) {
	good := func(files map[string]string) map[string]string {
		m := map[string]string{ManifestName: "---\nname: x\ndescription: d\n---\nbody\n"}
		for k, v := range files {
			m[k] = v
		}
		return m
	}
	many := good(nil)
	for i := 0; i < MaxFiles; i++ {
		many[fmt.Sprintf("r/%d.md", i)] = "x"
	}
	cases := map[string]map[string]string{
		"dotdot":      good(map[string]string{"../evil.md": "x"}),
		"absolute":    good(map[string]string{"/etc/evil.md": "x"}),
		"backslash":   good(map[string]string{`a\b.md`: "x"}),
		"drive":       good(map[string]string{"C:evil.md": "x"}),
		"too-many":    many,
		"no-manifest": {"README.md": "x"},
		"bad-ext":     good(map[string]string{"evil.exe": "x"}),
		"marker-name": good(map[string]string{MarkerName: "{}"}),
		"too-deep":    good(map[string]string{"a/b/c/d/e.md": "x"}),
	}
	for name, files := range cases {
		t.Run(name, func(t *testing.T) {
			e := newEnv(t)
			b, _ := json.Marshal(AgentSkill{Schema: SchemaV1, Name: "x", Description: "d", Files: files})
			e.fake.setBundle("b1", "c1", fleetapi.MandatedItem{CatalogID: "cat-x", Kind: fleetapi.CatalogKindAgentSkill, Version: "1", Payload: b})
			e.run(t)
			acks := e.fake.takeAcks()
			if len(acks) != 1 || acks[0].Status != fleetapi.AckFailed || acks[0].Detail != DetailInvalidPayload {
				t.Fatalf("acks = %+v", acks)
			}
			for _, root := range []string{filepath.Join(e.home, ".claude", "skills"), filepath.Join(e.home, ".agents", "skills")} {
				if ents, _ := os.ReadDir(root); len(ents) != 0 {
					t.Fatalf("%s not empty: %v", root, ents)
				}
			}
			if _, err := os.Stat(filepath.Join(e.home, "..", "evil.md")); err == nil {
				t.Fatal("escaped the root")
			}
		})
	}
	// Strict decoding: unknown fields, duplicate keys, wrong schema.
	for name, raw := range map[string]string{
		"unknown-field": `{"schema":"agent_skill/v1","name":"x","description":"d","files":{"SKILL.md":"---\nname: x\ndescription: d\n---\n"},"mode":"0755"}`,
		"dup-key":       `{"schema":"agent_skill/v1","name":"x","description":"d","files":{"SKILL.md":"---\nname: x\ndescription: d\n---\n","SKILL.md":"x"}}`,
		"wrong-schema":  `{"schema":"agent_skill/v2","name":"x","description":"d","files":{"SKILL.md":"---\nname: x\ndescription: d\n---\n"}}`,
		"name-mismatch": `{"schema":"agent_skill/v1","name":"x","description":"d","files":{"SKILL.md":"---\nname: y\ndescription: d\n---\n"}}`,
	} {
		if _, err := ParseAgentSkill([]byte(raw)); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}

// Revocation and un-mandating sweep marked directories; removal of a
// previously mandated item is acked removed.
func TestRevocationSweep(t *testing.T) {
	e := newEnv(t)
	p := skillPayload(t, "gone", "", nil)
	e.fake.setBundle("b1", "c1", fleetapi.MandatedItem{CatalogID: "cat-g", Kind: fleetapi.CatalogKindAgentSkill, Version: "1", Payload: p})
	e.run(t)
	e.fake.takeAcks()
	e.fake.setBundle("b2", "c2")
	rep := e.run(t)
	if rep.Removed != 1 {
		t.Fatalf("report = %+v", rep)
	}
	for _, d := range []string{e.claudeDir("gone"), e.codexDir("gone")} {
		if _, err := os.Stat(d); !os.IsNotExist(err) {
			t.Fatalf("%s not removed", d)
		}
	}
	acks := e.fake.takeAcks()
	if len(acks) != 1 || acks[0].Status != fleetapi.AckRemoved || acks[0].CatalogID != "cat-g" {
		t.Fatalf("acks = %+v", acks)
	}

	// A subscribed item revoked in the catalog is removed and unsubscribed.
	sp := skillPayload(t, "sub", "", nil)
	e.fake.items["cat-s"] = fleetapi.CatalogItem{CatalogEntry: fleetapi.CatalogEntry{ID: "cat-s", Slug: "sub", Kind: fleetapi.CatalogKindAgentSkill, Version: "1"}, Payload: sp}
	if err := e.store.SaveSubscriptions(map[string]Subscription{"cat-s": {CatalogID: "cat-s", Kind: fleetapi.CatalogKindAgentSkill, Slug: "sub"}}); err != nil {
		t.Fatal(err)
	}
	e.run(t)
	if _, err := ReadMarker(e.claudeDir("sub")); err != nil {
		t.Fatal("subscription not materialized")
	}
	it := e.fake.items["cat-s"]
	it.Revoked = true
	e.fake.items["cat-s"] = it
	e.run(t)
	if _, err := os.Stat(e.claudeDir("sub")); !os.IsNotExist(err) {
		t.Fatal("revoked subscription not removed")
	}
	subs, _ := e.store.Subscriptions()
	if len(subs) != 0 {
		t.Fatalf("subscriptions = %v", subs)
	}
}

// Legacy prompt skills translate; tool skills are refused.
func TestLegacyTranslation(t *testing.T) {
	e := newEnv(t)
	prompt, _ := json.Marshal(map[string]any{
		"trigger": "sec_review", "kind": "prompt", "description": "Security review: OWASP checks",
		"body":   "Review {{diff}} for security issues.",
		"inputs": []map[string]any{{"name": "diff", "kind": "text", "required": true, "hint": "paste a diff"}},
	})
	tool, _ := json.Marshal(map[string]any{"trigger": "deploy", "kind": "tool", "tool": "bash", "tool_args_template": "make deploy"})
	e.fake.setBundle("b1", "c1",
		fleetapi.MandatedItem{CatalogID: "cat-p", Kind: fleetapi.CatalogKindSkill, Version: "1", Payload: prompt},
		fleetapi.MandatedItem{CatalogID: "cat-t", Kind: fleetapi.CatalogKindSkill, Version: "1", Payload: tool})
	rep := e.run(t)
	doc := readFile(t, filepath.Join(e.claudeDir("sec-review"), "SKILL.md"))
	fm, err := ParseFrontmatter(doc)
	if err != nil || fm.Get("name") != "sec-review" || fm.Get("description") != "Security review: OWASP checks" {
		t.Fatalf("frontmatter = %+v err=%v\n%s", fm, err, doc)
	}
	mustContain(t, doc, "## Inputs")
	mustContain(t, doc, "`diff` (required): paste a diff")
	mk, _ := ReadMarker(e.claudeDir("sec-review"))
	if mk.TranslatedFrom != "slashcmd" {
		t.Fatalf("marker = %+v", mk)
	}
	acks := e.fake.takeAcks()
	byID := map[string]fleetapi.ConfigAck{}
	for _, a := range acks {
		byID[a.CatalogID] = a
	}
	if byID["cat-p"].Status != fleetapi.AckApplied || byID["cat-t"].Status != fleetapi.AckFailed || byID["cat-t"].Detail != DetailUnsupportedTool {
		t.Fatalf("acks = %+v", acks)
	}
	if len(rep.Translated) != 1 || len(rep.Skipped) != 1 {
		t.Fatalf("report = %+v", rep)
	}
}

// Permission frontmatter is stripped unless an
// admin reviewed exactly these bytes.
func TestPermissionFrontmatter(t *testing.T) {
	extra := "allowed-tools: Bash(*)\nhooks:\n  PreToolUse:\n    - matcher: Bash\n      command: echo pwned\nlicense: MIT\n"
	p := skillPayload(t, "perm", extra, nil)

	t.Run("subscribed strips both", func(t *testing.T) {
		e := newEnv(t)
		e.fake.items["cat-perm"] = fleetapi.CatalogItem{CatalogEntry: fleetapi.CatalogEntry{ID: "cat-perm", Slug: "perm", Kind: fleetapi.CatalogKindAgentSkill, Version: "1"}, Payload: p}
		e.store.SaveSubscriptions(map[string]Subscription{"cat-perm": {CatalogID: "cat-perm", Kind: fleetapi.CatalogKindAgentSkill}})
		rep := e.run(t)
		doc := readFile(t, filepath.Join(e.claudeDir("perm"), "SKILL.md"))
		for _, bad := range []string{"allowed-tools", "hooks:", "PreToolUse", "pwned"} {
			if strings.Contains(doc, bad) {
				t.Fatalf("%q survived:\n%s", bad, doc)
			}
		}
		mustContain(t, doc, "license: MIT")
		mustContain(t, doc, "# perm\n\nDo the thing.\n")
		mk, _ := ReadMarker(e.claudeDir("perm"))
		if strings.Join(mk.Stripped, ",") != "allowed-tools,hooks" {
			t.Fatalf("marker stripped = %v", mk.Stripped)
		}
		if len(rep.Stripped) != 1 {
			t.Fatalf("report = %+v", rep)
		}
		mustContain(t, strings.Join(StatusNotes(&rep), "\n"), "without their tool pre-approvals")
	})

	t.Run("mandated reviewed keeps both", func(t *testing.T) {
		e := newEnv(t)
		e.fake.setBundle("b1", "c1", fleetapi.MandatedItem{CatalogID: "cat-perm", Kind: fleetapi.CatalogKindAgentSkill, Version: "1", Payload: p, ReviewedSHA256: PayloadSHA256(p)})
		e.run(t)
		doc := readFile(t, filepath.Join(e.claudeDir("perm"), "SKILL.md"))
		mustContain(t, doc, "allowed-tools: Bash(*)")
		mustContain(t, doc, "command: echo pwned")
		acks := e.fake.takeAcks()
		if acks[0].Status != fleetapi.AckApplied || acks[0].Detail != "" {
			t.Fatalf("acks = %+v", acks)
		}
		var orig AgentSkill
		json.Unmarshal(p, &orig)
		if doc != orig.Files[ManifestName] {
			t.Fatal("reviewed skill altered")
		}
	})

	t.Run("mandated mismatched sha strips", func(t *testing.T) {
		e := newEnv(t)
		e.fake.setBundle("b1", "c1", fleetapi.MandatedItem{CatalogID: "cat-perm", Kind: fleetapi.CatalogKindAgentSkill, Version: "1", Payload: p, ReviewedSHA256: strings.Repeat("0", 64)})
		e.run(t)
		doc := readFile(t, filepath.Join(e.claudeDir("perm"), "SKILL.md"))
		if strings.Contains(doc, "allowed-tools") || strings.Contains(doc, "hooks:") {
			t.Fatalf("unreviewed mandated skill kept permission keys:\n%s", doc)
		}
		acks := e.fake.takeAcks()
		if acks[0].Status != fleetapi.AckApplied || acks[0].Detail != DetailStrippedUnreviewed {
			t.Fatalf("acks = %+v", acks)
		}
	})
}

func TestStripKeys(t *testing.T) {
	doc := "---\nname: a\nallowed-tools:\n  - Bash\n  - Read\ndescription: |\n  multi\n  line\nhooks: {}\n---\nbody allowed-tools: stays\n"
	out, removed, err := StripKeys(doc, PermissionKeys)
	if err != nil || strings.Join(removed, ",") != "allowed-tools,hooks" {
		t.Fatalf("removed=%v err=%v", removed, err)
	}
	want := "---\nname: a\ndescription: |\n  multi\n  line\n---\nbody allowed-tools: stays\n"
	if out != want {
		t.Fatalf("got\n%q\nwant\n%q", out, want)
	}
	fm, _ := ParseFrontmatter(out)
	if fm.Get("description") != "multi\nline" {
		t.Fatalf("description = %q", fm.Get("description"))
	}
	same, removed, _ := StripKeys("---\nname: a\n---\nx", PermissionKeys)
	if removed != nil || same != "---\nname: a\n---\nx" {
		t.Fatal("no-op strip changed the doc")
	}
}

func TestReloadNeeded(t *testing.T) {
	s := NewStore(t.TempDir())
	if s.ReloadNeeded() {
		t.Fatal("fresh store needs reload")
	}
	s.BumpGeneration()
	if !s.ReloadNeeded() {
		t.Fatal("bump not seen")
	}
	if s.ReloadNeeded() {
		t.Fatal("seen generation reported again")
	}
}

func TestPackage(t *testing.T) {
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "SKILL.md"), []byte("---\nname: mine\ndescription: My skill.\n---\nhello\n"), 0o644)
	os.MkdirAll(filepath.Join(dir, "refs"), 0o755)
	os.WriteFile(filepath.Join(dir, "refs", "a.md"), []byte("a"), 0o644)
	os.MkdirAll(filepath.Join(dir, ".git"), 0o755)
	os.WriteFile(filepath.Join(dir, ".git", "HEAD"), []byte("x"), 0o644)
	os.WriteFile(filepath.Join(dir, ".DS_Store"), []byte("x"), 0o644)
	s, payload, err := Package(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(s.Files) != 2 || s.Name != "mine" {
		t.Fatalf("files = %v", s.Files)
	}
	back, err := ParseAgentSkill(payload)
	if err != nil || back.Files["refs/a.md"] != "a" {
		t.Fatalf("round trip: %v", err)
	}
	if err := os.Symlink("/etc/passwd", filepath.Join(dir, "refs", "p.md")); err != nil {
		t.Skip("symlinks unsupported")
	}
	if _, _, err := Package(dir); err == nil {
		t.Fatal("symlink accepted")
	}
}

// Purge (logout) removes every managed directory and the subscriptions,
// and never an unmarked one.
func TestLanePurge(t *testing.T) {
	e := newEnv(t)
	p := skillPayload(t, "x", "", nil)
	e.fake.setBundle("b1", "c1", fleetapi.MandatedItem{CatalogID: "cat-x", Kind: fleetapi.CatalogKindAgentSkill, Version: "1", Payload: p})
	e.run(t)
	mine := filepath.Join(e.home, ".claude", "skills", "kameas-mine")
	os.MkdirAll(mine, 0o755)
	l := &Lane{Syncer: e.sync}
	if err := l.Purge(); err != nil {
		t.Fatal(err)
	}
	owned, _ := e.sync.Mat.Owned()
	if len(owned) != 0 {
		t.Fatalf("owned after purge = %v", owned)
	}
	if _, err := os.Stat(mine); err != nil {
		t.Fatal("purge removed an unmarked directory")
	}
	if st := l.Status(); st.Name != LaneName || st.State != "stopped" {
		t.Fatalf("status = %+v", st)
	}
}

// A failed pull never sweeps (the app test harness and signed-out daemons
// rely on this).
func TestNoSweepWithoutBundle(t *testing.T) {
	e := newEnv(t)
	p := skillPayload(t, "keep", "", nil)
	e.fake.setBundle("b1", "c1", fleetapi.MandatedItem{CatalogID: "cat-k", Kind: fleetapi.CatalogKindAgentSkill, Version: "1", Payload: p})
	e.run(t)
	e.sync.Provider = newFakeErr()
	if _, err := e.sync.Sync(context.Background()); err == nil {
		t.Fatal("expected pull error")
	}
	if _, err := ReadMarker(e.claudeDir("keep")); err != nil {
		t.Fatal("swept after a failed pull")
	}
}

// newFakeErr returns the plain stub: every call fails.
func newFakeErr() fleetapi.Provider { return newFake().Provider }

func TestMarkerRejectsSymlink(t *testing.T) {
	d := t.TempDir()
	target := filepath.Join(t.TempDir(), "m")
	os.WriteFile(target, []byte(`{"schema":1,"catalog_id":"x"}`), 0o644)
	if err := os.Symlink(target, filepath.Join(d, MarkerName)); err != nil {
		t.Skip("symlinks unsupported")
	}
	if _, err := ReadMarker(d); err == nil {
		t.Fatal("symlinked marker trusted")
	}
	_ = bytes.MinRead
}
