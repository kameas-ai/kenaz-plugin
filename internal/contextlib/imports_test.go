package contextlib

import (
	"go/build"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

const module = "github.com/kameas-ai/kenaz-plugin"

// sendingPackages talk to Kameas Fleet or queue data for it.
var sendingPackages = []string{module + "/fleetapi", module + "/internal/forward", module + "/internal/spool"}

// TestLibraryImportsNothingThatSends pins the privacy boundary: the library
// cannot reach the network code paths at all, and adds no outside dependency,
// so it stays thin.
func TestLibraryImportsNothingThatSends(t *testing.T) {
	_, file, _, _ := runtime.Caller(0)
	root := filepath.Join(filepath.Dir(file), "..", "..")
	seen := map[string]bool{}
	var walk func(string)
	walk = func(p string) {
		dir := filepath.Join(root, filepath.FromSlash(strings.TrimPrefix(strings.TrimPrefix(p, module), "/")))
		bp, err := build.ImportDir(dir, 0)
		if err != nil {
			t.Fatalf("%s: %v", p, err)
		}
		for _, imp := range bp.Imports {
			if strings.Contains(imp, ".") && !strings.HasPrefix(imp, module) {
				t.Errorf("%s imports %s; the library adds no outside dependency", p, imp)
			}
			if strings.HasPrefix(imp, module) && !seen[imp] {
				seen[imp] = true
				walk(imp)
			}
		}
	}
	walk(module + "/internal/contextlib")
	for dep := range seen {
		for _, bad := range sendingPackages {
			if dep == bad || strings.HasPrefix(dep, bad+"/") {
				t.Errorf("internal/contextlib imports %s", dep)
			}
		}
	}
}
