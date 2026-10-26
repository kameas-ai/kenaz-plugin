package app

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"github.com/kameas-ai/kenaz-plugin/fleetapi"
	"github.com/kameas-ai/kenaz-plugin/fleetapi/stub"
	"github.com/kameas-ai/kenaz-plugin/internal/daemon"
	"github.com/kameas-ai/kenaz-plugin/internal/logx"
	"github.com/kameas-ai/kenaz-plugin/internal/skills"
	"github.com/kameas-ai/kenaz-plugin/internal/spool"
	"github.com/kameas-ai/kenaz-plugin/internal/variant"
)

// twoEnvSkills gives this test its own skills home with one prod and one
// dev managed skill in Claude Code's folder.
func twoEnvSkills(t *testing.T) string {
	t.Helper()
	home := t.TempDir()
	orig := skillsHome
	skillsHome = func() (string, error) { return home, nil }
	t.Cleanup(func() { skillsHome = orig })
	for env, slug := range map[string]string{"prod": "prodskill", "dev": "devskill"} {
		d := filepath.Join(home, ".claude", "skills", skills.DirPrefixFor(env)+slug)
		os.MkdirAll(d, 0o755)
		mk := `{"schema":1,"catalog_id":"cat-` + slug + `","slug":"` + slug + `","version":"1","source":"mandated","env":"` + env + `"}`
		if err := os.WriteFile(filepath.Join(d, skills.MarkerName), []byte(mk), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return home
}

func skillFolders(home string) string {
	ents, _ := os.ReadDir(filepath.Join(home, ".claude", "skills"))
	var out []string
	for _, e := range ents {
		out = append(out, e.Name())
	}
	sort.Strings(out)
	return strings.Join(out, " ")
}

func devEnv(home string, p fleetapi.Provider, out, errb *bytes.Buffer) Env {
	return Env{Provider: p, Stdout: out, Stderr: errb, Ctx: context.Background(), KeyStore: &spool.Memory{},
		Getenv: func(k string) string {
			return map[string]string{variant.Shared.HomeVar: home, variant.Shared.EnvVar: "dev"}[k]
		},
		OpenBrowser: func(string) error { return nil }, Spawn: func(string, fleetapi.Env) error { return nil }}
}

// The dev relay's skills lane, `kenaz skills list` and `kenaz logout` for
// dev see and remove only dev's skills; prod's stay (issue #34 F1, wired
// through the app).
func TestSkillsWiringIsPerEnv(t *testing.T) {
	if !variant.Current.Allowed("dev") {
		t.Skip("dev not allowed")
	}
	home := twoEnvSkills(t)
	both := "kameas-dev-devskill kameas-prodskill"

	// The daemon lane.
	opt := &daemon.Options{Provider: stub.New(), Env: fleetapi.EnvDev, StateDir: t.TempDir(), Logger: logx.Discard()}
	if _, err := skillsDaemonHook(opt); err != nil {
		t.Fatal(err)
	}
	var lane *skills.Lane
	for _, l := range opt.Lanes {
		if sl, ok := l.(*skills.Lane); ok {
			lane = sl
		}
	}
	if lane == nil {
		t.Fatal("no skills lane")
	}
	if o, _ := lane.Syncer.Mat.Owned(); len(o) != 1 || o[0].Marker.Slug != "devskill" {
		t.Fatalf("dev lane owns %+v", o)
	}

	// The CLI.
	state := t.TempDir()
	var out, errb bytes.Buffer
	if code := RunEnv(devEnv(state, stub.New(), &out, &errb), []string{"skills", "list"}); code != 0 {
		t.Fatalf("skills list: %d %s", code, errb.String())
	}
	if !strings.Contains(out.String(), "devskill") || strings.Contains(out.String(), "prodskill") {
		t.Fatalf("skills list for dev:\n%s", out.String())
	}

	// The lane's purge (node removed) and logout's.
	if err := lane.Purge(); err != nil {
		t.Fatal(err)
	}
	if got := skillFolders(home); got != "kameas-prodskill" {
		t.Fatalf("after the dev lane's purge: %s", got)
	}
	home = twoEnvSkills(t)
	out.Reset()
	RunEnv(devEnv(state, stub.New(), &out, &errb), []string{"logout"})
	if got := skillFolders(home); got != "kameas-prodskill" {
		t.Fatalf("after logout from dev: %s (want prod's kept; before: %s)", got, both)
	}
}

type officialSession struct {
	sessionProvider
}

func (officialSession) Flavor() fleetapi.Flavor { return fleetapi.FlavorOfficial }

// RunEnv wires guardSessionEnv: with a stored session for prod, the staff
// dev build reports signed out, the public build signed in.
func TestRunEnvGuardsSessionEnv(t *testing.T) {
	p := officialSession{sessionProvider{stub.New(), fleetapi.EnvProd}}
	var out, errb bytes.Buffer
	env := devEnv(t.TempDir(), p, &out, &errb)
	if !variant.Current.Allowed("dev") {
		t.Skip("dev not allowed")
	}
	RunEnv(env, []string{"status", "--json"})
	var st daemon.Status
	if err := json.Unmarshal(out.Bytes(), &st); err != nil {
		t.Fatalf("%v: %s %s", err, out.String(), errb.String())
	}
	want := daemon.SessionSignedIn
	if !variant.Current.Allowed("prod") {
		want = daemon.SessionSignedOut
	}
	if st.Session != want {
		t.Fatalf("session %q, want %q", st.Session, want)
	}
}
