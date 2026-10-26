package bundle

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/kameas-ai/kenaz-plugin/internal/variant"
)

// withHomebrewCasks sets HomebrewCasks for one test.
func withHomebrewCasks(t *testing.T, on bool) {
	t.Helper()
	prev := HomebrewCasks
	HomebrewCasks = on
	t.Cleanup(func() { HomebrewCasks = prev })
}

// fakeCask lays out what `brew install --cask kenaz` leaves on disk:
// <prefix>/Caskroom/kenaz/<version>/kenaz_<version>_<os>_<arch>/bin/kenaz
// with bundle.json beside bin/, and <prefix>/bin/kenaz linking to it. It
// returns the resolved executable.
func fakeCask(t *testing.T, prefix, version string) string {
	t.Helper()
	root := filepath.Join(prefix, "Caskroom", variant.Current.CaskToken, version, variant.Current.BundleName+"_"+version+"_darwin_arm64")
	if err := os.MkdirAll(filepath.Join(root, "bin"), 0o755); err != nil {
		t.Fatal(err)
	}
	exe := filepath.Join(root, "bin", variant.Current.Binary)
	os.WriteFile(exe, []byte("kenaz "+version), 0o755)
	os.WriteFile(filepath.Join(root, BundleFile), []byte("{}"), 0o644)
	return exe
}

// brewLink points <prefix>/bin/kenaz at exe, as the cask's binary stanza
// does on install and on every upgrade.
func brewLink(t *testing.T, prefix, exe string) string {
	t.Helper()
	link := filepath.Join(prefix, "bin", variant.Current.Binary)
	os.MkdirAll(filepath.Dir(link), 0o755)
	os.Remove(link)
	if err := os.Symlink(exe, link); err != nil {
		t.Skip("symlinks unavailable:", err)
	}
	return link
}

func resolvedTemp(t *testing.T) string {
	t.Helper()
	d, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	return d
}

func TestStablePathHomebrewCask(t *testing.T) {
	withHomebrewCasks(t, true)
	prefix := filepath.Join(resolvedTemp(t), "homebrew")
	exe := fakeCask(t, prefix, "0.5.0")
	link := brewLink(t, prefix, exe)

	if got := StablePath(exe); got != link {
		t.Fatalf("StablePath = %s, want %s", got, link)
	}
	if p, ok := HomebrewCaskPrefix(exe); !ok || p != prefix {
		t.Fatalf("HomebrewCaskPrefix = %q %v", p, ok)
	}
	// The link itself is already stable, and DetectBundle follows it to the
	// versioned bundle that setup verifies and renders.
	if got := StablePath(link); got != link {
		t.Fatalf("StablePath(link) = %s", got)
	}
	if got := DetectBundle(link); got != filepath.Dir(filepath.Dir(exe)) {
		t.Fatalf("DetectBundle(link) = %q", got)
	}

	// brew upgrade 0.5.0 -> 0.5.1: the new version is unpacked, the link
	// is switched, the old version directory is deleted. The stable path
	// still resolves, now to 0.5.1.
	exe2 := fakeCask(t, prefix, "0.5.1")
	brewLink(t, prefix, exe2)
	os.RemoveAll(filepath.Join(prefix, "Caskroom", "kenaz", "0.5.0"))
	if r, err := filepath.EvalSymlinks(link); err != nil || r != exe2 {
		t.Fatalf("after upgrade %s -> %q (%v)", link, r, err)
	}
	if got := StablePath(exe2); got != link {
		t.Fatalf("StablePath after upgrade = %s", got)
	}
}

// Homebrew present, but <prefix>/bin/kenaz is not this cask's link: the
// versioned path is kept (no other kenaz is ever registered in its place).
func TestStablePathHomebrewWithoutCaskLink(t *testing.T) {
	withHomebrewCasks(t, true)
	for _, c := range []struct {
		name string
		lay  func(t *testing.T, prefix, exe string)
	}{
		{"no link", func(t *testing.T, prefix, exe string) {}},
		{"plain file", func(t *testing.T, prefix, exe string) {
			os.MkdirAll(filepath.Join(prefix, "bin"), 0o755)
			os.WriteFile(filepath.Join(prefix, "bin", "kenaz"), []byte("copied"), 0o755)
		}},
		{"other kenaz", func(t *testing.T, prefix, exe string) {
			other := filepath.Join(prefix, "Cellar", "kenaz", "9.9.9", "bin", "kenaz")
			os.MkdirAll(filepath.Dir(other), 0o755)
			os.WriteFile(other, []byte("formula"), 0o755)
			brewLink(t, prefix, other)
		}},
		{"dangling", func(t *testing.T, prefix, exe string) {
			brewLink(t, prefix, filepath.Join(prefix, "gone", "kenaz"))
		}},
	} {
		t.Run(c.name, func(t *testing.T) {
			prefix := filepath.Join(resolvedTemp(t), "usr", "local")
			exe := fakeCask(t, prefix, "0.5.0")
			c.lay(t, prefix, exe)
			if got := StablePath(exe); got != exe {
				t.Fatalf("StablePath = %s, want the versioned path", got)
			}
		})
	}

	// A non-cask kenaz inside a Homebrew prefix (a formula, a copied
	// binary) is not a cask path at all.
	prefix := filepath.Join(resolvedTemp(t), "homebrew")
	formula := filepath.Join(prefix, "Cellar", "kenaz", "0.5.0", "bin", "kenaz")
	os.MkdirAll(filepath.Dir(formula), 0o755)
	os.WriteFile(formula, []byte("x"), 0o755)
	brewLink(t, prefix, formula)
	if _, ok := HomebrewCaskPrefix(formula); ok {
		t.Fatal("formula path taken for a cask")
	}
	if got := StablePath(formula); got != formula {
		t.Fatalf("formula StablePath = %s", got)
	}
	// Caskroom metadata is not a version directory.
	meta := filepath.Join(prefix, "Caskroom", "kenaz", ".metadata", "x", "bin", "kenaz")
	if _, ok := HomebrewCaskPrefix(meta); ok {
		t.Fatal(".metadata taken for a version")
	}
}

// Linux and Windows have no kenaz cask: a Caskroom-shaped path is left as
// it is.
func TestStablePathHomebrewNoOpOffMacOS(t *testing.T) {
	withHomebrewCasks(t, false)
	prefix := filepath.Join(resolvedTemp(t), "linuxbrew")
	exe := fakeCask(t, prefix, "0.5.0")
	brewLink(t, prefix, exe)
	if got := StablePath(exe); got != exe {
		t.Fatalf("StablePath = %s", got)
	}
	if _, ok := HomebrewCaskPrefix(exe); ok {
		t.Fatal("cask detected off macOS")
	}
}
