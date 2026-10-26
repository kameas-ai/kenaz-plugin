package variant_test

import (
	"bytes"
	"encoding/json"
	"flag"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"testing"

	"github.com/kameas-ai/kenaz-plugin/internal/bundle"
	"github.com/kameas-ai/kenaz-plugin/internal/credstore"
	"github.com/kameas-ai/kenaz-plugin/internal/daemonstate"
	"github.com/kameas-ai/kenaz-plugin/internal/variant"
	"github.com/kameas-ai/kenaz-plugin/internal/variant/varianttest"
)

var update = flag.Bool("update", false, "rewrite golden files")

// repoRoot is the module root, two levels up from this package.
var repoRoot, _ = filepath.Abs(filepath.Join("..", ".."))

// built holds the binaries TestMain builds once: every build variant
// (kenaz, and kenaz-dev with -tags kenaz_dev), the one under test (this
// test binary's variant) and the fake host CLIs.
var built struct {
	dir   string
	bins  map[string]string // by variant name
	kenaz string            // variant.Current's binary
	hosts string
	err   error
}

func exe(name string) string {
	if runtime.GOOS == "windows" {
		return name + ".exe"
	}
	return name
}

func goBuild(out, pkg string, tags ...string) error {
	args := []string{"build", "-o", out}
	if len(tags) > 0 {
		args = append(args, "-tags", strings.Join(tags, ","))
	}
	args = append(args, pkg)
	cmd := exec.Command("go", args...)
	cmd.Dir = repoRoot
	cmd.Env = append(os.Environ(), "CGO_ENABLED=0")
	if b, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("go %s: %v\n%s", strings.Join(args, " "), err, b)
	}
	return nil
}

func TestMain(m *testing.M) {
	flag.Parse()
	dir, err := os.MkdirTemp("", "kenaz-variant-")
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	built.dir = dir
	built.bins = map[string]string{}
	built.hosts = filepath.Join(dir, "hosts")
	for _, v := range variant.Variants {
		var tags []string
		if v.Name == variant.KenazDev.Name {
			tags = []string{"kenaz_dev"}
		}
		bin := filepath.Join(dir, v.Name, exe(v.Binary))
		if built.err == nil {
			built.err = goBuild(bin, "./cmd/kenaz", tags...)
		}
		built.bins[v.Name] = bin
	}
	built.kenaz = built.bins[variant.Current.Name]
	if built.err == nil {
		built.err = goBuild(filepath.Join(built.hosts, exe("claude")), "./test/variant/fakehost")
	}
	if built.err == nil {
		built.err = goBuild(filepath.Join(built.hosts, exe("codex")), "./test/variant/fakehost")
	}
	code := m.Run()
	os.RemoveAll(dir)
	os.Exit(code)
}

// machine is one user's machine: a temp HOME with fake Claude Code and
// Codex CLIs on PATH and no access to the real home, keychain or hosts.
type machine struct {
	t    *testing.T
	root string
	home string
	log  string
}

func newMachine(t *testing.T) *machine {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("the variant tests run on macOS and Linux")
	}
	if built.err != nil {
		t.Fatal(built.err)
	}
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	m := &machine{t: t, root: root, home: filepath.Join(root, "home"), log: filepath.Join(root, "hosts.log")}
	if err := os.MkdirAll(m.home, 0o700); err != nil {
		t.Fatal(err)
	}
	return m
}

func (m *machine) environ() []string {
	return []string{
		"HOME=" + m.home,
		"PATH=" + built.hosts + string(os.PathListSeparator) + "/usr/bin" + string(os.PathListSeparator) + "/bin",
		"TMPDIR=" + filepath.Join(m.root, "tmp"),
		"FAKEHOST_LOG=" + m.log,
		"LANG=C",
	}
}

// layout is where the builds keep their files on this machine: one root,
// one state directory per Fleet environment, shared by every build.
type layout struct {
	ConfigDir string
	StateRoot string
}

func (l layout) state(env string) string { return filepath.Join(l.StateRoot, env) }

// layout is ~/.kenaz/plugin on every platform (as the harness uses
// ~/.kenaz/harness).
func (m *machine) layout() layout {
	root := filepath.Join(m.home, ".kenaz", "plugin")
	return layout{ConfigDir: root, StateRoot: root}
}

// prepare seeds what a signed-in machine has before setup: the file
// credential store (so nothing touches an OS keychain) and a relay's
// daemon.json on port for env.
func (m *machine) prepare(l layout, env string, port int) {
	m.t.Helper()
	if err := credstore.WriteMarker(filepath.Join(l.StateRoot, "credentials"), credstore.Files); err != nil {
		m.t.Fatal(err)
	}
	st := l.state(env)
	if err := os.MkdirAll(st, 0o700); err != nil {
		m.t.Fatal(err)
	}
	b, _ := json.Marshal(daemonstate.State{PID: 1, Port: port, Version: "test", Nonce: "n"})
	if err := os.WriteFile(filepath.Join(st, daemonstate.FileName), b, 0o600); err != nil {
		m.t.Fatal(err)
	}
}

func copyTree(t *testing.T, src, dst string) {
	t.Helper()
	err := filepath.WalkDir(src, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(src, p)
		if d.IsDir() {
			return os.MkdirAll(filepath.Join(dst, rel), 0o755)
		}
		b, err := os.ReadFile(p)
		if err != nil {
			return err
		}
		return os.WriteFile(filepath.Join(dst, rel), b, 0o644)
	})
	if err != nil {
		t.Fatal(err)
	}
}

// pluginVersion is the version the repository's plugin manifests carry.
func pluginVersion(t *testing.T) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(repoRoot, "plugin", ".claude-plugin", "plugin.json"))
	if err != nil {
		t.Fatal(err)
	}
	var pj struct{ Version string }
	if err := json.Unmarshal(b, &pj); err != nil {
		t.Fatal(err)
	}
	return pj.Version
}

// installBundle lays out a release archive the way release packaging
// does, around the built binary, and returns the path of its executable.
func (m *machine) installBundle(name, bin string, tree func(dst string)) string {
	m.t.Helper()
	v := pluginVersion(m.t)
	root := filepath.Join(m.root, "Kenaz Bundles", name+"_"+v+"_"+runtime.GOOS+"_"+runtime.GOARCH)
	x := filepath.Join(root, "bin", filepath.Base(bin))
	if err := os.MkdirAll(filepath.Dir(x), 0o755); err != nil {
		m.t.Fatal(err)
	}
	b, err := os.ReadFile(bin)
	if err != nil {
		m.t.Fatal(err)
	}
	if err := os.WriteFile(x, b, 0o755); err != nil {
		m.t.Fatal(err)
	}
	tree(root)
	files := map[string]string{}
	filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		rel, _ := filepath.Rel(root, p)
		files[filepath.ToSlash(rel)], _ = bundle.FileSHA(p)
		return nil
	})
	rel := "bin/" + filepath.Base(bin)
	man := map[string]any{
		"schema": 1, "name": name, "version": v, "os": runtime.GOOS, "arch": runtime.GOARCH,
		"executable": rel, "executable_sha256": files[rel], "opencode": false, "files": files,
	}
	mb, _ := json.MarshalIndent(man, "", "  ")
	if err := os.WriteFile(filepath.Join(root, bundle.BundleFile), mb, 0o644); err != nil {
		m.t.Fatal(err)
	}
	return x
}

// pluginTree writes the repository's plugin sources, as variant v ships
// them, into a bundle root.
func pluginTree(t *testing.T, v variant.Identity) func(string) {
	return func(dst string) {
		if err := varianttest.WritePluginTree(repoRoot, dst, v); err != nil {
			t.Fatal(err)
		}
	}
}

type result struct {
	Code           int
	Stdout, Stderr string
}

func (m *machine) run(bin string, extraEnv []string, args ...string) result {
	m.t.Helper()
	os.MkdirAll(filepath.Join(m.root, "tmp"), 0o700)
	cmd := exec.Command(bin, args...)
	cmd.Env = append(m.environ(), extraEnv...)
	cmd.Dir = m.home
	cmd.Stdin = strings.NewReader("")
	var out, errb bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &errb
	err := cmd.Run()
	code := 0
	if ee, ok := err.(*exec.ExitError); ok {
		code = ee.ExitCode()
	} else if err != nil {
		m.t.Fatal(err)
	}
	return result{code, out.String(), errb.String()}
}

// norm replaces this machine's temp paths with stable tokens.
func (m *machine) norm(s string) string {
	s = strings.ReplaceAll(s, built.dir, "$BUILD")
	s = strings.ReplaceAll(s, m.home, "$HOME")
	return strings.ReplaceAll(s, m.root, "$ROOT")
}

func (r result) render(m *machine, args ...string) string {
	return fmt.Sprintf("$ %s\nexit %d\n--- stdout\n%s--- stderr\n%s", strings.Join(args, " "), r.Code, m.norm(r.Stdout), m.norm(r.Stderr))
}

// tree lists every file under dir, sorted, with the contents of those
// contents selects.
func (m *machine) tree(dir string, contents func(rel string) bool) string {
	m.t.Helper()
	var rels []string
	filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		rel, _ := filepath.Rel(dir, p)
		rels = append(rels, filepath.ToSlash(rel))
		return nil
	})
	sort.Strings(rels)
	var b strings.Builder
	for _, rel := range rels {
		fmt.Fprintf(&b, "== %s\n", m.norm(rel))
		if contents(rel) {
			c, _ := os.ReadFile(filepath.Join(dir, filepath.FromSlash(rel)))
			b.WriteString(m.norm(string(c)))
			if len(c) > 0 && c[len(c)-1] != '\n' {
				b.WriteString("\n")
			}
		}
	}
	return b.String()
}

func golden(t *testing.T, name, got string) {
	t.Helper()
	p := filepath.Join("testdata", goldenDir, name+".golden")
	if *update {
		os.MkdirAll(filepath.Dir(p), 0o755)
		os.WriteFile(p, []byte(got), 0o644)
	}
	want, err := os.ReadFile(p)
	if err != nil {
		t.Fatalf("%v (run with -update)", err)
	}
	if string(want) != got {
		t.Errorf("%s differs from %s:\n--- want\n%s\n--- got\n%s", name, p, want, got)
	}
}
