package capture

import (
	"fmt"
	"go/ast"
	"go/build"
	"go/importer"
	"go/parser"
	"go/token"
	"go/types"
	"io/fs"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"testing"
)

// The hard line: Kenaz never reads, stores, copies or forwards a vendor's credentials
// or session tokens. This guard resolves every reference in capture and in the module
// packages it depends on, with go/types, so aliases, function values, helpers and
// assembled names make no difference: anything that touches the file system, the
// environment or processes must be listed here, by function.
const guardModule = "github.com/kameas-ai/kenaz-plugin"

// guardedPkgs are the standard packages that can reach a login. Any reference to one
// needs an entry in guardAllow. "os/exec" is a whole-package allowance per function.
var guardedPkgs = map[string]bool{"os": true, "io/fs": true, "io/ioutil": true, "syscall": true, "os/exec": true, "plugin": true, "golang.org/x/sys/windows": true, "os/user": true}

// guardImports is what capture and its module dependencies may import. A new package
// is a decision: it can bring a way out that this guard does not see.
var guardImports = map[string]bool{
	"bytes": true, "context": true, "crypto/rand": true, "crypto/sha256": true, "database/sql": true, "encoding/hex": true, "encoding/json": true,
	"errors": true, "fmt": true, "io": true, "net/url": true, "os": true, "os/exec": true, "path/filepath": true, "regexp": true, "sort": true,
	"strconv": true, "strings": true, "time": true, "unicode": true, "unicode/utf8": true, "syscall": true, "math/rand/v2": true,
	"modernc.org/sqlite": true, "golang.org/x/sys/windows": true,
	guardModule + "/internal/usertext": true,
	guardModule + "/internal/memory":   true, guardModule + "/internal/memory/capture": true, guardModule + "/internal/safetext": true, guardModule + "/pkg/secretscan": true,
}

// guardImportFiles limits what only one file may import.
var guardImportFiles = map[string]map[string]bool{
	"syscall":                  {"lock_unix.go": true, "lock_windows.go": true},
	"golang.org/x/sys/windows": {"lock_windows.go": true},
	"os/exec":                  {"summarizer.go": true, "hook.go": true, "lock_unix.go": true, "lock_windows.go": true},
}

// guardAllow lists, per package and function, each guarded object it may use.
var guardAllow = map[string]map[string][]string{
	guardModule + "/internal/memory/capture": {
		"tryLock":          {"golang.org/x/sys/windows.Overlapped", "golang.org/x/sys/windows.LockFileEx", "golang.org/x/sys/windows.UnlockFileEx", "golang.org/x/sys/windows.Handle", "golang.org/x/sys/windows.LOCKFILE_EXCLUSIVE_LOCK", "golang.org/x/sys/windows.LOCKFILE_FAIL_IMMEDIATELY", "golang.org/x/sys/windows.ERROR_LOCK_VIOLATION", "os.OpenFile", "os.O_RDWR", "os.O_CREATE", "os.File.Close", "os.File.Fd", "syscall.Flock", "syscall.LOCK_EX", "syscall.LOCK_NB", "syscall.LOCK_UN", "syscall.EWOULDBLOCK"},
		"detach":           {"os/exec.*", "syscall.SysProcAttr", "syscall.Setsid", "syscall.CreationFlags"},
		"runProcess":       {"os/exec.*"},
		"StartDetached":    {"os/exec.*", "os.Process.Release"},
		"HostCLI.Complete": {"os/exec.*", "os.Environ"},
		"Command.Complete": {"os/exec.*", "os.Environ"},
		"HostCLI.env":      {"os.Environ"},
		"Paths.ensure":     {"os.MkdirAll"},
		"Enqueue":          {"os.ReadDir", "os.WriteFile", "os.Rename", "os.Remove"},
		"listQueue":        {"os.ReadDir", "os.ErrNotExist", "os.Remove", "os.ReadFile", "io/fs.DirEntry.Info", "io/fs.DirEntry.Name", "io/fs.FileInfo.ModTime"},
		"removeQueued":     {"os.Remove"},
		"QueueLen":         {"os.ReadDir", "io/fs.DirEntry.Name"},
		"PurgeQueue":       {"os.RemoveAll"},
		"noteDropped":      {"os.OpenFile", "os.O_WRONLY", "os.O_CREATE", "os.O_APPEND", "os.File.Close", "os.File.Write"},
		"droppedCount":     {"os.Stat", "io/fs.FileInfo.Size"},
		"fileExists":       {"os.Lstat"},
		"LoadState":        {"os.ReadFile"},
		"saveState":        {"os.WriteFile", "os.Rename"},
		"nextReminder":     {"os.MkdirAll", "os.ReadFile", "os.WriteFile"},
		"pruneReminders":   {"os.ReadDir", "os.Remove", "io/fs.DirEntry.Info", "io/fs.DirEntry.Name", "io/fs.FileInfo.ModTime"},
	},
	guardModule + "/internal/memory": {
		"OpenProfile":              {"os.MkdirAll", "os.Chmod"},
		"OpenProfileReadOnly":      {"os.Stat", "os.ErrNotExist"},
		"ClaimDisclosure":          {"os.OpenFile", "os.O_WRONLY", "os.O_CREATE", "os.O_EXCL", "os.File.Close", "os.File.WriteString"},
		"Purge":                    {"os.Remove", "os.ErrNotExist"},
		"PurgeAll":                 {"os.Remove"},
		"ProfileStores":            {"os.ReadDir", "os.Stat", "io/fs.DirEntry.IsDir", "io/fs.DirEntry.Name"},
		"ProjectRoot":              {"os.Lstat"},
		"gitCommonDir":             {"os.Lstat", "os.ReadFile", "io/fs.FileInfo.IsDir"},
		"gitRemote":                {"os.ReadFile"},
		"Store.migrateProjectKeys": {"os.Lstat"},
	},
}

// guardAppFiles are the app files that serve capture and memory. The app as a whole
// is too large to type-check here, so these get an import-aware syntax scan: any use
// of a guarded package needs an entry, and getenv takes no assembled name.
var guardAppFiles = map[string]map[string][]string{
	"memorycapture.go": {
		"runCaptureHook":    {"os.UserHomeDir", "os.Executable", "os.Environ"},
		"captureWorker":     {"os.UserHomeDir"},
		"captureExclude":    {"os.Getwd"},
		"capturePrintHooks": {"os.Executable"},
		"runCaptureIndex":   {"os.Getwd"},
	},
	"memory.go": {
		"memoryPaths.discloseOnce":     {"os.MkdirAll"},
		"memoryPaths.recallNoticeOnce": {"os.MkdirAll"},
		"memoryIndex.Index":            {"os.Getwd"},
		"memoryScope":                  {"os.Getwd"},
		"memoryPurge":                  {"os.Stat", "os.ErrNotExist"},
		"":                             {"os.Stdin", "os.ModeCharDevice"},
		"memoryStdinTerminal":          {"os.Stdin", "os.ModeCharDevice"},
	},
	"hook.go": {
		"runHook": {"os.Getenv", "os.Stdin"},
	},
}

// guardBannedWords may not appear in a string literal of any of these sources. The
// names that exist to keep these out are exempt.
var guardBannedWords = []string{".credentials", "credentials.json", "auth.json", "keychain", "find-generic-password", "oauth",
	"session token", "Claude Code-credentials", "AUTH_TOKEN", "OAUTH_TOKEN", "apiKeyHelper", ".claude/", ".codex/", ".config/anthropic"}

var guardWordExempt = map[string]bool{"ValidKeyEnv": true, "sensitivePath": true, "credentialCommand": true}

// guardRepoExempt is what the repo-wide scan leaves out: setup writes the host's own
// plugin and hook settings. Kenaz's own credential store is why "keychain" is scanned
// only in capture.
var guardRepoExempt = map[string]bool{"internal/setup": true}

// guardFuncKey is the allowlist key of a function: a method carries its receiver, so
// that the same name on another type gets nothing.
func guardFuncKey(d *ast.FuncDecl) string {
	if d.Recv == nil || len(d.Recv.List) == 0 {
		return d.Name.Name
	}
	t := d.Recv.List[0].Type
	if star, ok := t.(*ast.StarExpr); ok {
		t = star.X
	}
	if id, ok := t.(*ast.Ident); ok {
		return id.Name + "." + d.Name.Name
	}
	return d.Name.Name
}

func guardAllowed(allowed []string, obj string) bool {
	for _, a := range allowed {
		if a == obj || (strings.HasSuffix(a, ".*") && strings.HasPrefix(obj, strings.TrimSuffix(a, "*"))) {
			return true
		}
	}
	return false
}

func guardRoot(t *testing.T) string {
	t.Helper()
	_, file, _, _ := runtime.Caller(0)
	return filepath.Join(filepath.Dir(file), "..", "..", "..")
}

func guardBannedLiterals(t *testing.T, name string, f *ast.File, words []string) {
	t.Helper()
	check := func(n ast.Node) {
		ast.Inspect(n, func(n ast.Node) bool {
			lit, ok := n.(*ast.BasicLit)
			if !ok || lit.Kind != token.STRING {
				return true
			}
			for _, w := range words {
				if strings.Contains(strings.ToLower(lit.Value), strings.ToLower(w)) {
					t.Errorf("%s spells %q: %s", name, w, lit.Value)
				}
			}
			return true
		})
	}
	for _, decl := range f.Decls {
		switch d := decl.(type) {
		case *ast.FuncDecl:
			if !guardWordExempt[d.Name.Name] {
				check(d)
			}
		case *ast.GenDecl:
			for _, spec := range d.Specs {
				if v, ok := spec.(*ast.ValueSpec); !ok || !guardWordExempt[v.Names[0].Name] {
					check(spec)
				}
			}
		}
	}
}

// guardImporter checks the module's packages from source and the standard library
// through the source importer, which also loads a guarded package from outside the
// module. Any other package, which capture may only import for its side effects, is an
// empty stand-in.
type guardImporter struct {
	t    *testing.T
	root string
	fset *token.FileSet
	std  types.Importer
	seen map[string]*types.Package
	uses map[string][]guardFile
}

type guardFile struct {
	name string
	file *ast.File
	info *types.Info
}

func (g *guardImporter) Import(path string) (*types.Package, error) {
	if p, ok := g.seen[path]; ok {
		return p, nil
	}
	if !strings.HasPrefix(path, guardModule) {
		if strings.Contains(strings.Split(path, "/")[0], ".") && !guardedPkgs[path] {
			p := types.NewPackage(path, filepath.Base(path))
			p.MarkComplete()
			g.seen[path] = p
			return p, nil
		}
		return g.std.Import(path)
	}
	rel := strings.TrimPrefix(strings.TrimPrefix(path, guardModule), "/")
	dir := filepath.Join(g.root, filepath.FromSlash(rel))
	bp, err := build.ImportDir(dir, 0)
	if err != nil {
		return nil, err
	}
	var files []*ast.File
	names := map[*ast.File]string{}
	for _, name := range bp.GoFiles {
		f, err := parser.ParseFile(g.fset, filepath.Join(dir, name), nil, 0)
		if err != nil {
			return nil, err
		}
		files = append(files, f)
		names[f] = name
	}
	info := &types.Info{Uses: map[*ast.Ident]types.Object{}, Selections: map[*ast.SelectorExpr]*types.Selection{}}
	var errs []string
	conf := types.Config{Importer: g, Error: func(err error) { errs = append(errs, err.Error()) }}
	p, _ := conf.Check(path, g.fset, files, info)
	if len(errs) > 0 {
		g.t.Errorf("%s does not type-check: %s", path, strings.Join(errs, "; "))
	}
	g.seen[path] = p
	for _, f := range files {
		g.uses[path] = append(g.uses[path], guardFile{names[f], f, info})
	}
	return p, nil
}

// guardObject names a guarded object, with its receiver for a method or field.
func guardObject(obj types.Object) string {
	if obj == nil || obj.Pkg() == nil || !guardedPkgs[obj.Pkg().Path()] {
		return ""
	}
	path := obj.Pkg().Path()
	if fn, ok := obj.(*types.Func); ok {
		if recv := fn.Signature().Recv(); recv != nil {
			t := recv.Type()
			if ptr, ok := t.(*types.Pointer); ok {
				t = ptr.Elem()
			}
			if named, ok := t.(*types.Named); ok {
				return path + "." + named.Obj().Name() + "." + obj.Name()
			}
		}
	}
	return path + "." + obj.Name()
}

func TestCaptureNeverTouchesVendorLogins(t *testing.T) {
	root := guardRoot(t)
	fset := token.NewFileSet()
	g := &guardImporter{t: t, root: root, fset: fset, std: importer.ForCompiler(fset, "source", nil),
		seen: map[string]*types.Package{}, uses: map[string][]guardFile{}}
	if _, err := g.Import(guardModule + "/internal/memory/capture"); err != nil {
		t.Fatal(err)
	}
	if len(g.uses) < 3 {
		t.Fatalf("scanned only %d packages", len(g.uses))
	}
	var pkgs []string
	for pkg := range g.uses {
		pkgs = append(pkgs, pkg)
	}
	sort.Strings(pkgs)
	for _, pkg := range pkgs {
		// Imports, over every file of the package, for every platform.
		rel := strings.TrimPrefix(strings.TrimPrefix(pkg, guardModule), "/")
		dir := filepath.Join(root, filepath.FromSlash(rel))
		all, err := filepath.Glob(filepath.Join(dir, "*.go"))
		if err != nil {
			t.Fatal(err)
		}
		for _, path := range all {
			name := filepath.Base(path)
			if strings.HasSuffix(name, "_test.go") {
				continue
			}
			f, err := parser.ParseFile(token.NewFileSet(), path, nil, 0)
			if err != nil {
				t.Fatal(err)
			}
			for _, imp := range f.Imports {
				ip := strings.Trim(imp.Path.Value, `"`)
				if !guardImports[ip] {
					t.Errorf("%s/%s imports %s, which is not on the list", rel, name, ip)
				}
				if only, ok := guardImportFiles[ip]; ok && pkg == guardModule+"/internal/memory/capture" && !only[name] {
					t.Errorf("%s/%s imports %s, which only %v may", rel, name, ip, only)
				}
				if imp.Name != nil && imp.Name.Name == "." {
					t.Errorf("%s/%s dot-imports %s", rel, name, ip)
				}
			}
			if pkg == guardModule+"/internal/memory/capture" {
				guardBannedLiterals(t, name, f, guardBannedWords)
			}
			// A file this build leaves out, for its platform or a tag, is not type
			// checked: it gets the syntax scan, so that no build goes unread.
			checked := false
			for _, file := range g.uses[pkg] {
				checked = checked || file.name == name
			}
			if !checked {
				guardSyntaxScan(t, rel+"/"+name, f, guardAllow[pkg])
			}
		}
		// Every reference, resolved.
		for _, file := range g.uses[pkg] {
			scan := func(fn string, n ast.Node) {
				ast.Inspect(n, func(n ast.Node) bool {
					id, ok := n.(*ast.Ident)
					if !ok {
						return true
					}
					obj := guardObject(file.info.Uses[id])
					if obj == "" {
						return true
					}
					if guardAllowed(guardAllow[pkg][fn], obj) {
						return true
					}
					t.Errorf("%s/%s: %s uses %s, which is not on the list", rel, file.name, fn, obj)
					return true
				})
			}
			for _, decl := range file.file.Decls {
				switch d := decl.(type) {
				case *ast.FuncDecl:
					scan(guardFuncKey(d), d)
				case *ast.GenDecl:
					if d.Tok != token.IMPORT {
						scan("", d) // a package-level value: nothing is listed for these
					}
				}
			}
		}
	}
	guardAppScan(t, root)
	guardRepoScan(t, root)
}

// guardRepoScan looks for the banned words in every non-test Go file of the repository,
// whatever its package or build tags.
func guardRepoScan(t *testing.T, root string) {
	t.Helper()
	words := make([]string, 0, len(guardBannedWords))
	for _, w := range guardBannedWords {
		if w != "keychain" {
			words = append(words, w)
		}
	}
	scanned := 0
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		rel = filepath.ToSlash(rel)
		if d.IsDir() {
			if base := d.Name(); base == "testdata" || base == "vendor" || (base != "." && strings.HasPrefix(base, ".")) || guardRepoExempt[rel] {
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(rel, ".go") || strings.HasSuffix(rel, "_test.go") {
			return nil
		}
		f, err := parser.ParseFile(token.NewFileSet(), path, nil, 0)
		if err != nil {
			return err
		}
		scanned++
		guardBannedLiterals(t, rel, f, words)
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if scanned < 50 {
		t.Fatalf("the repo scan read only %d files", scanned)
	}
}

// guardAppScan is the syntax scan of the app files that serve capture and memory.
func guardAppScan(t *testing.T, root string) {
	t.Helper()
	for name, allow := range guardAppFiles {
		path := filepath.Join(root, "app", name)
		f, err := parser.ParseFile(token.NewFileSet(), path, nil, 0)
		if err != nil {
			t.Fatal(err)
		}
		if name == "memorycapture.go" {
			guardBannedLiterals(t, name, f, guardBannedWords)
		}
		guardSyntaxScan(t, name, f, allow)
	}
}

// guardSyntaxScan checks a file without type checking it. It sees a guarded package
// used by name, but not a method called on a value.
func guardSyntaxScan(t *testing.T, name string, f *ast.File, allow map[string][]string) {
	t.Helper()
	// What each import is called in this file.
	local := map[string]string{}
	for _, imp := range f.Imports {
		ip := strings.Trim(imp.Path.Value, `"`)
		if imp.Name != nil && imp.Name.Name == "." {
			t.Errorf("%s dot-imports %s", name, ip)
		}
		if guardedPkgs[ip] {
			as := filepath.Base(ip)
			if imp.Name != nil {
				as = imp.Name.Name
			}
			local[as] = ip
		}
	}
	scan := func(fn string, n ast.Node) {
		ast.Inspect(n, func(n ast.Node) bool {
			switch x := n.(type) {
			case *ast.SelectorExpr:
				if id, ok := x.X.(*ast.Ident); ok {
					if ip, ok := local[id.Name]; ok {
						obj := ip + "." + x.Sel.Name
						if !guardAllowed(allow[fn], obj) {
							t.Errorf("%s: %s uses %s, which is not on the list", name, fn, obj)
						}
					}
				}
			case *ast.CallExpr:
				if sel, ok := x.Fun.(*ast.SelectorExpr); ok && sel.Sel.Name == "getenv" && len(x.Args) == 1 {
					switch x.Args[0].(type) {
					case *ast.BasicLit, *ast.BinaryExpr:
						t.Errorf("%s: %s reads the environment by a spelled or assembled name: %s", name, fn, fmt.Sprint(x.Args[0]))
					}
				}
			}
			return true
		})
	}
	for _, decl := range f.Decls {
		switch d := decl.(type) {
		case *ast.FuncDecl:
			scan(guardFuncKey(d), d)
		case *ast.GenDecl:
			if d.Tok != token.IMPORT {
				scan("", d)
			}
		}
	}
}
