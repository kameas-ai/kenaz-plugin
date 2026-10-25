package variant_test

import (
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"testing"
)

// These goldens pin what the public build does with its identity: the CLI
// surface, where setup keeps its state, what it registers with each host
// and the plugin tree it renders. They are compared byte for byte, so a
// change to any identity value shows up here.

var (
	timestamp = regexp.MustCompile(`\d{4}-\d{2}-\d{2}T\d{2}:\d{2}:\d{2}(\.\d+)?(Z|[+-]\d{2}:\d{2})|\d{8}T\d{6}(\.\d+)?Z`)
	// Digests cover rendered files that name this run's temp paths.
	digest = regexp.MustCompile(`\b[0-9a-f]{64}\b`)
)

// stable replaces what differs between runs: times, digests and the
// platform suffix of bundle directories.
func stable(s string) string {
	s = timestamp.ReplaceAllString(s, "$$TIME")
	s = digest.ReplaceAllString(s, "$$SHA256")
	return strings.ReplaceAll(s, "_"+runtime.GOOS+"_"+runtime.GOARCH, "_$PLATFORM")
}

// stableVersion also replaces the plugin manifests' version, which every
// release bumps.
func stableVersion(t *testing.T, s string) string {
	v := pluginVersion(t)
	s = strings.ReplaceAll(s, `"version": "`+v+`"`, `"version": "$VERSION"`)
	s = strings.ReplaceAll(s, "_"+v+"_", "_$VERSION_")
	return strings.ReplaceAll(s, "/"+v+"_", "/$VERSION_")
}

func TestGoldenCLI(t *testing.T) {
	m := newMachine(t)
	var b strings.Builder
	for _, args := range [][]string{
		{"--help"},
		{"--version"},
		{"setup", "print-managed", "claude"},
		{"setup", "print-managed", "codex"},
		{"setup", "--help"},
	} {
		b.WriteString(m.run(built.kenaz, nil, args...).render(m, args...))
	}
	golden(t, "cli", b.String())
}

func TestGoldenSetup(t *testing.T) {
	m := newMachine(t)
	l := m.layoutFor(publicConfigDir)
	m.prepare(l, "prod", 4318)
	bin := m.installBundle("kenaz", built.kenaz, publicTree(t))
	all := func(string) bool { return true }

	var b strings.Builder
	step := func(args ...string) {
		b.WriteString(stableVersion(t, stable(m.run(bin, nil, args...).render(m, args...))))
	}
	step("setup", "claude", "--dry-run")
	step("setup", "codex", "--dry-run")
	golden(t, "setup-dry-run", b.String())

	b.Reset()
	step("setup", "claude", "--yes")
	step("setup", "codex", "--yes")
	b.WriteString("--- files\n" + stableVersion(t, stable(m.tree(m.home, all))))
	golden(t, "setup-apply", b.String())

	b.Reset()
	step("setup", "--undo", "claude", "--yes")
	step("setup", "--undo", "codex", "--yes")
	b.WriteString("--- files\n" + stableVersion(t, stable(m.tree(m.home, all))))
	b.WriteString("--- host commands\n" + stableVersion(t, stable(m.norm(readFile(t, m.log)))))
	golden(t, "setup-undo", b.String())
}

// goldenDir holds one set of goldens per platform.
var goldenDir = filepath.Join("kenaz", runtime.GOOS)
