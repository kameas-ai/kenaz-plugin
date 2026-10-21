package memory

import (
	"go/build"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

const module = "github.com/kameas-ai/kenaz-plugin"

// sendingPackages are the packages that talk to Kameas Fleet or queue data
// for it. Memory must never depend on them, directly or transitively.
var sendingPackages = []string{
	module + "/fleetapi",
	module + "/internal/forward",
	module + "/internal/spool",
}

func moduleRoot(t *testing.T) string {
	_, file, _, _ := runtime.Caller(0)
	return filepath.Join(filepath.Dir(file), "..", "..")
}

// moduleImports returns every package of this module that pkg imports,
// transitively (non-test files only).
func moduleImports(t *testing.T, root, pkg string) map[string]bool {
	t.Helper()
	seen := map[string]bool{}
	var walk func(string)
	walk = func(p string) {
		dir := filepath.Join(root, filepath.FromSlash(strings.TrimPrefix(strings.TrimPrefix(p, module), "/")))
		bp, err := build.ImportDir(dir, 0)
		if err != nil {
			t.Fatalf("%s: %v", p, err)
		}
		for _, imp := range bp.Imports {
			if (imp == module || strings.HasPrefix(imp, module+"/")) && !seen[imp] {
				seen[imp] = true
				walk(imp)
			}
		}
	}
	walk(pkg)
	return seen
}

// TestMemoryImportsNothingThatSends pins the privacy boundary: the memory
// store cannot reach the network code paths at all.
func TestMemoryImportsNothingThatSends(t *testing.T) {
	root := moduleRoot(t)
	deps := moduleImports(t, root, module+"/internal/memory")
	for dep := range deps {
		for _, bad := range sendingPackages {
			if dep == bad || strings.HasPrefix(dep, bad+"/") {
				t.Errorf("internal/memory imports %s", dep)
			}
		}
	}
	// The store package uses only the standard library and the SQLite
	// driver.
	bp, err := build.ImportDir(filepath.Join(root, "internal", "memory"), 0)
	if err != nil {
		t.Fatal(err)
	}
	for _, imp := range bp.Imports {
		if strings.Contains(imp, ".") && imp != "modernc.org/sqlite" {
			t.Errorf("internal/memory imports %s", imp)
		}
	}
}
