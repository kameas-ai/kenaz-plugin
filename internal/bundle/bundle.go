// Package bundle verifies a self-contained Kenaz release archive and
// renders the per-user copy of its plugin with absolute launch commands.
// It never modifies the archive and never touches host configuration;
// registering the rendered plugin with a host is internal/setup's job.
//
// The only edits Render makes are: the build's MCP server command ("kenaz"
// in the public build) in .mcp.json and .codex-mcp.json, and the leading
// executable name of each existing hook command, both replaced by the
// archive's absolute executable. It adds
// no server, hook, event or setting (test-enforced).
package bundle

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"runtime"
	"sort"
	"strings"

	"github.com/kameas-ai/kenaz-plugin/internal/variant"
)

// A release archive holds
//
//	bin/kenaz[.exe]   plugin/   .claude-plugin/marketplace.json
//	opencode/ (optional)   bundle.json   SHA256SUMS   licenses/ ...
//
// and bundle.json lists every file's SHA-256. Setup in bundle mode verifies
// the bundle, never edits it, and registers it with the hosts so that every
// launch uses the bundle's executable by absolute path, with no PATH, npm or
// network dependency:
//

// BundleFile is the bundle manifest's file name.
const BundleFile = "bundle.json"

// Bundle is a verified release archive directory.
type Bundle struct {
	Root     string // absolute
	Version  string
	OS, Arch string
	Exe      string // absolute path of the bundle's executable
	// Launch is the path hosts launch: Exe, or a version-independent path
	// to the same executable (StablePath) when the bundle sits in a
	// versioned directory an upgrade removes. Render writes Launch.
	Launch   string
	OpenCode bool // the bundle carries opencode/

	// verified holds the bytes LoadBundle hashed for every file Render
	// copies (plugin/ and .claude-plugin/marketplace.json), so Render
	// writes exactly what was verified and never re-reads a path.
	verified map[string][]byte
}

// walkedDirs are the bundle directories in which every file must be
// listed in bundle.json: nothing unlisted (an extra skill, command, hook
// script or adapter module) can ride along in a verified bundle. Files at
// the bundle root (bundle.json, SHA256SUMS, licenses) are not walked;
// inside these directories only a file named exactly .DS_Store is
// ignored.
var walkedDirs = []string{"bin", "plugin", "opencode"}

// rendered reports whether rel is a file Render copies.
func rendered(rel string) bool {
	return strings.HasPrefix(rel, "plugin/") || rel == ".claude-plugin/marketplace.json"
}

// readRegular reads abs/rel, refusing a symlink or other non-regular file
// at rel or at any directory on the way to it.
func readRegular(abs, rel string) ([]byte, error) {
	cur := abs
	parts := strings.Split(rel, "/")
	for i, part := range parts {
		cur = filepath.Join(cur, part)
		fi, err := os.Lstat(cur)
		if err != nil {
			return nil, err
		}
		if fi.Mode()&os.ModeSymlink != 0 {
			return nil, fmt.Errorf("%s is a symlink", strings.Join(parts[:i+1], "/"))
		}
		if i < len(parts)-1 && !fi.IsDir() {
			return nil, fmt.Errorf("%s is not a directory", strings.Join(parts[:i+1], "/"))
		}
		if i == len(parts)-1 && !fi.Mode().IsRegular() {
			return nil, fmt.Errorf("%s is not a regular file", rel)
		}
	}
	return os.ReadFile(cur)
}

// ID names the bundle's rendered directory.
func (b *Bundle) ID() string { return b.Version + "_" + b.OS + "_" + b.Arch }

// AdapterDir is the bundle's OpenCode adapter directory.
func (b *Bundle) AdapterDir() string { return filepath.Join(b.Root, "opencode") }

type bundleManifest struct {
	Schema           int               `json:"schema"`
	Name             string            `json:"name"`
	Version          string            `json:"version"`
	OS               string            `json:"os"`
	Arch             string            `json:"arch"`
	Executable       string            `json:"executable"`
	ExecutableSHA256 string            `json:"executable_sha256"`
	OpenCode         bool              `json:"opencode"`
	Files            map[string]string `json:"files"`
}

var hexDigest = regexp.MustCompile(`^[0-9a-f]{64}$`)

func FileSHA(p string) (string, error) {
	f, err := os.Open(p)
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// safeRel reports whether rel is a clean relative slash path inside the
// bundle.
func safeRel(rel string) bool {
	if rel == "" || strings.Contains(rel, "\\") || path.IsAbs(rel) || path.Clean(rel) != rel || rel == ".." || strings.HasPrefix(rel, "../") {
		return false
	}
	return !filepath.IsAbs(rel) && !regexp.MustCompile(`^[A-Za-z]:`).MatchString(rel)
}

// LoadBundle verifies the bundle at root: manifest, platform, every listed
// file's digest (read once; no symlinks anywhere on its path), that bin/,
// plugin/ and opencode/ hold no file bundle.json does not list and no
// symlink, and that the plugin, marketplace and adapter carry the bundle's
// version. Nothing is written. The bytes of the files Render copies are
// kept from this single verified read.
func LoadBundle(root string) (*Bundle, error) {
	abs, err := filepath.Abs(root)
	if err != nil {
		return nil, err
	}
	raw, err := os.ReadFile(filepath.Join(abs, BundleFile))
	if err != nil {
		return nil, fmt.Errorf("%s is not a Kenaz bundle: %v", abs, err)
	}
	var m bundleManifest
	if err := json.Unmarshal(raw, &m); err != nil {
		return nil, fmt.Errorf("%s: %v", BundleFile, err)
	}
	switch {
	case m.Schema != 1 || m.Name != variant.Current.BundleName:
		return nil, fmt.Errorf("%s: unsupported bundle (schema %d, name %q)", BundleFile, m.Schema, m.Name)
	case m.Version == "":
		return nil, fmt.Errorf("%s: no version", BundleFile)
	case m.OS != runtime.GOOS || m.Arch != runtime.GOARCH:
		return nil, fmt.Errorf("bundle is for %s/%s, this machine is %s/%s", m.OS, m.Arch, runtime.GOOS, runtime.GOARCH)
	case !safeRel(m.Executable) || !hexDigest.MatchString(m.ExecutableSHA256) || m.Files[m.Executable] != m.ExecutableSHA256:
		return nil, fmt.Errorf("%s: invalid executable entry", BundleFile)
	case len(m.Files) == 0:
		return nil, fmt.Errorf("%s: no file list", BundleFile)
	}
	names := make([]string, 0, len(m.Files))
	for rel := range m.Files {
		names = append(names, rel)
	}
	sort.Strings(names)
	verified := map[string][]byte{}
	for _, rel := range names {
		if !safeRel(rel) || !hexDigest.MatchString(m.Files[rel]) {
			return nil, fmt.Errorf("%s: invalid file entry %q", BundleFile, rel)
		}
		data, err := readRegular(abs, rel)
		if err != nil {
			return nil, fmt.Errorf("bundle file %s: %v", rel, err)
		}
		sum := sha256.Sum256(data)
		if hex.EncodeToString(sum[:]) != m.Files[rel] {
			return nil, fmt.Errorf("bundle file %s does not match bundle.json; the bundle was modified or is incomplete", rel)
		}
		if rendered(rel) {
			verified[rel] = data
		}
	}
	// Nothing unlisted, and no symlink, in the directories hosts load
	// from.
	for _, dir := range walkedDirs {
		top := filepath.Join(abs, dir)
		fi, err := os.Lstat(top)
		if errors.Is(err, os.ErrNotExist) && dir == "opencode" {
			continue
		}
		if err != nil {
			return nil, fmt.Errorf("bundle %s/: %v", dir, err)
		}
		if !fi.IsDir() || fi.Mode()&os.ModeSymlink != 0 {
			return nil, fmt.Errorf("bundle %s is not a directory", dir)
		}
		err = filepath.WalkDir(top, func(p string, d os.DirEntry, err error) error {
			if err != nil {
				return err
			}
			rel, _ := filepath.Rel(abs, p)
			rel = filepath.ToSlash(rel)
			switch {
			case d.Type()&os.ModeSymlink != 0:
				return fmt.Errorf("bundle contains a symlink: %s", rel)
			case d.IsDir():
				return nil
			case !d.Type().IsRegular():
				return fmt.Errorf("bundle contains a special file: %s", rel)
			}
			if d.Name() == ".DS_Store" {
				// macOS Finder metadata, created by browsing an installed
				// version. Ignored by name only: it is never rendered or
				// registered (Render copies only verified listed files).
				return nil
			}
			if _, ok := m.Files[rel]; !ok {
				return fmt.Errorf("bundle file %s is not listed in bundle.json; the bundle was modified", rel)
			}
			return nil
		})
		if err != nil {
			return nil, err
		}
	}
	for _, rel := range []string{"plugin/.claude-plugin/plugin.json", "plugin/.codex-plugin/plugin.json", "plugin/.mcp.json", "plugin/.codex-mcp.json", "plugin/hooks/hooks.json", ".claude-plugin/marketplace.json"} {
		if _, ok := m.Files[rel]; !ok {
			return nil, fmt.Errorf("bundle lacks %s", rel)
		}
	}
	b := &Bundle{Root: abs, Version: m.Version, OS: m.OS, Arch: m.Arch, Exe: filepath.Join(abs, filepath.FromSlash(m.Executable)), OpenCode: m.OpenCode,
		verified: verified}
	b.Launch = StablePath(b.Exe)
	versions := map[string]string{}
	for _, rel := range []string{"plugin/.claude-plugin/plugin.json", "plugin/.codex-plugin/plugin.json"} {
		var pj struct{ Version string }
		_ = json.Unmarshal(verified[rel], &pj)
		versions[rel] = pj.Version
	}
	var mk struct {
		Plugins []struct{ Name, Version, Source string }
	}
	_ = json.Unmarshal(verified[".claude-plugin/marketplace.json"], &mk)
	for _, p := range mk.Plugins {
		if p.Name == variant.Current.Plugin {
			versions["marketplace.json"] = p.Version
		}
	}
	if b.OpenCode {
		if _, ok := m.Files["opencode/package.json"]; !ok {
			return nil, errors.New("bundle.json says opencode but opencode/package.json is not listed")
		}
		var pj struct{ Version string }
		pb, err := readRegular(abs, "opencode/package.json")
		if err != nil {
			return nil, err
		}
		_ = json.Unmarshal(pb, &pj)
		versions["opencode/package.json"] = pj.Version
	}
	for k, v := range versions {
		if v != b.Version {
			return nil, fmt.Errorf("bundle version mismatch: %s is %q, bundle is %q", k, v, b.Version)
		}
	}
	return b, nil
}

// InstallStateFile marks a release installer's install root: it holds
// install-state.json, versions/<id>/ (one unpacked bundle per version) and
// current, a link to the active version. The installer upgrades and rolls
// back by switching current, and prunes old versions.
const InstallStateFile = "install-state.json"

// StablePath returns the path hosts should launch for the resolved
// executable exe, so that an upgrade which removes exe's versioned
// directory breaks no host registration:
//
//   - Archive installer: when exe is <root>/versions/<id>/bin/<name> under
//     an install root (install-state.json present) whose current link
//     resolves to <root>/versions/<id>, it returns
//     <root>/current/bin/<name>, so an upgrade or rollback (switching
//     current) reaches every host and pruning the old version breaks none.
//   - Homebrew cask (macOS): when exe is
//     <prefix>/Caskroom/kenaz/<version>/<dir>/bin/<name> and
//     <prefix>/bin/<name> is a symlink that resolves to exe, it returns
//     <prefix>/bin/<name>. `brew upgrade` deletes <version>/ and relinks
//     <prefix>/bin/<name> to the new version (see HomebrewCaskPrefix).
//
// Any other executable is returned unchanged.
func StablePath(exe string) string {
	if exe == "" {
		return exe
	}
	if p, ok := homebrewLink(exe); ok {
		return p
	}
	binDir := filepath.Dir(exe)
	verDir := filepath.Dir(binDir)
	versions := filepath.Dir(verDir)
	root := filepath.Dir(versions)
	if filepath.Base(binDir) != "bin" || filepath.Base(versions) != "versions" {
		return exe
	}
	if fi, err := os.Stat(filepath.Join(root, InstallStateFile)); err != nil || !fi.Mode().IsRegular() {
		return exe
	}
	cur := filepath.Join(root, "current")
	target, err := filepath.EvalSymlinks(cur)
	if err != nil {
		return exe
	}
	want, err := filepath.EvalSymlinks(verDir)
	if err != nil || target != want {
		return exe
	}
	return filepath.Join(cur, "bin", filepath.Base(exe))
}

// HomebrewCasks reports whether this OS has Homebrew casks (macOS only:
// the kenaz cask is macOS-only, and on Linux and Windows StablePath never
// maps a Caskroom path). Tests may set it.
var HomebrewCasks = runtime.GOOS == "darwin"

// HomebrewCaskPrefix reports whether exe sits in a Homebrew cask's
// versioned directory, <prefix>/Caskroom/kenaz/<version>/<dir>/bin/<name>,
// and returns <prefix> (for example /opt/homebrew or /usr/local). It looks
// at the path only.
func HomebrewCaskPrefix(exe string) (string, bool) {
	if !HomebrewCasks || exe == "" || !filepath.IsAbs(exe) {
		return "", false
	}
	binDir := filepath.Dir(exe)
	verDir := filepath.Dir(filepath.Dir(binDir))
	token := filepath.Dir(verDir)
	caskroom := filepath.Dir(token)
	v := filepath.Base(verDir)
	if filepath.Base(binDir) != "bin" || filepath.Base(token) != variant.Current.CaskToken || filepath.Base(caskroom) != "Caskroom" ||
		v == "" || strings.HasPrefix(v, ".") {
		return "", false
	}
	return filepath.Dir(caskroom), true
}

// homebrewLink returns <prefix>/bin/<name> for a cask executable when that
// link is a symlink resolving to exe itself. A <prefix>/bin/<name> that is
// a plain file, is missing, or resolves to another kenaz (a non-cask
// install in the same prefix) is not used.
func homebrewLink(exe string) (string, bool) {
	prefix, ok := HomebrewCaskPrefix(exe)
	if !ok {
		return "", false
	}
	link := filepath.Join(prefix, "bin", filepath.Base(exe))
	fi, err := os.Lstat(link)
	if err != nil || fi.Mode()&os.ModeSymlink == 0 {
		return "", false
	}
	got, err := filepath.EvalSymlinks(link)
	if err != nil {
		return "", false
	}
	want, err := filepath.EvalSymlinks(exe)
	if err != nil || got != want {
		return "", false
	}
	return link, true
}

// StableRoot is StablePath for a bundle root: <root>/versions/<id> under
// an install root becomes <root>/current when current points at it.
func StableRoot(root string) string {
	abs, err := filepath.Abs(root)
	if err != nil {
		return root
	}
	if r, err := filepath.EvalSymlinks(abs); err == nil {
		abs = r
	}
	exe := StablePath(filepath.Join(abs, "bin", variant.Current.Binary))
	if filepath.Base(filepath.Dir(filepath.Dir(exe))) == "current" {
		return filepath.Dir(filepath.Dir(exe))
	}
	return root
}

// DetectBundle returns the bundle root for an executable at
// <root>/bin/kenaz when <root>/bundle.json exists, else "". When exe is a
// link outside any bundle (Homebrew's <prefix>/bin/kenaz), the bundle its
// target sits in is returned.
func DetectBundle(exe string) string {
	if exe == "" {
		return ""
	}
	root := filepath.Dir(filepath.Dir(exe))
	if _, err := os.Stat(filepath.Join(root, BundleFile)); err == nil {
		return root
	}
	r, err := filepath.EvalSymlinks(exe)
	if err != nil || r == exe {
		return ""
	}
	root = filepath.Dir(filepath.Dir(r))
	if _, err := os.Stat(filepath.Join(root, BundleFile)); err != nil {
		return ""
	}
	return root
}

// hookQuote quotes an absolute executable path for a hook command line.
// Double quotes work in POSIX shells and cmd.exe; characters that either
// would still expand are refused rather than escaped.
func hookQuote(p string) (string, error) {
	if strings.ContainsAny(p, "\"$`%!\n\r") {
		return "", fmt.Errorf("the bundle path %q contains a character that cannot be quoted safely in a hook command; move the bundle", p)
	}
	return `"` + p + `"`, nil
}

// launch is the executable path Render writes into launch commands.
func (b *Bundle) launch() string {
	if b.Launch != "" {
		return b.Launch
	}
	return b.Exe
}

// Render writes the per-user copy of the bundle's plugin and local
// marketplace into dir, with absolute launch commands (b.Launch). It
// returns a digest of everything rendered.
func Render(b *Bundle, dir string, write bool) (string, error) {
	quoted, err := hookQuote(b.launch())
	if err != nil {
		return "", err
	}
	// Only the bytes LoadBundle verified: nothing is read again by path.
	if b.verified == nil {
		return "", errors.New("bundle was not verified (use LoadBundle)")
	}
	files := map[string][]byte{}
	for rel, data := range b.verified {
		files[rel] = append([]byte(nil), data...)
	}
	for _, rel := range []string{"plugin/.mcp.json", "plugin/.codex-mcp.json"} {
		out, err := rewriteMCP(files[rel], b.launch())
		if err != nil {
			return "", fmt.Errorf("%s: %v", rel, err)
		}
		files[rel] = out
	}
	hooks, err := rewriteHooks(files["plugin/hooks/hooks.json"], quoted)
	if err != nil {
		return "", fmt.Errorf("plugin/hooks/hooks.json: %v", err)
	}
	files["plugin/hooks/hooks.json"] = hooks

	names := make([]string, 0, len(files))
	for n := range files {
		names = append(names, n)
	}
	sort.Strings(names)
	h := sha256.New()
	for _, n := range names {
		fmt.Fprintf(h, "%s\x00%d\x00", n, len(files[n]))
		h.Write(files[n])
	}
	digest := hex.EncodeToString(h.Sum(nil))
	if !write {
		return digest, nil
	}

	if old, err := os.ReadFile(filepath.Join(dir, ".kenaz-rendered")); err == nil && strings.TrimSpace(string(old)) == digest {
		return digest, nil
	}
	tmp := dir + ".tmp"
	_ = os.RemoveAll(tmp)
	for _, n := range names {
		p := filepath.Join(tmp, filepath.FromSlash(n))
		if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
			return "", err
		}
		if err := os.WriteFile(p, files[n], 0o600); err != nil {
			return "", err
		}
	}
	if err := os.WriteFile(filepath.Join(tmp, ".kenaz-rendered"), []byte(digest+"\n"), 0o600); err != nil {
		return "", err
	}
	_ = os.RemoveAll(dir)
	if err := os.MkdirAll(filepath.Dir(dir), 0o700); err != nil {
		return "", err
	}
	return digest, os.Rename(tmp, dir)
}

func rewriteMCP(raw []byte, exe string) ([]byte, error) {
	var doc map[string]any
	if err := json.Unmarshal(raw, &doc); err != nil {
		return nil, err
	}
	servers, _ := doc["mcpServers"].(map[string]any)
	srv, _ := servers[variant.Current.MCPServer].(map[string]any)
	if srv == nil || srv["command"] != variant.Current.Binary {
		return nil, fmt.Errorf("no %q server launching %s", variant.Current.MCPServer, variant.Current.Binary)
	}
	srv["command"] = exe
	out, err := json.MarshalIndent(doc, "", "  ")
	return append(out, '\n'), err
}

func rewriteHooks(raw []byte, quotedExe string) ([]byte, error) {
	var doc map[string]any
	if err := json.Unmarshal(raw, &doc); err != nil {
		return nil, err
	}
	n := 0
	events, _ := doc["hooks"].(map[string]any)
	for _, groups := range events {
		gs, _ := groups.([]any)
		for _, g := range gs {
			gm, _ := g.(map[string]any)
			hs, _ := gm["hooks"].([]any)
			for _, h := range hs {
				hm, _ := h.(map[string]any)
				cmd, _ := hm["command"].(string)
				if !strings.HasPrefix(cmd, variant.Current.Binary+" ") {
					return nil, fmt.Errorf("unexpected hook command %q", cmd)
				}
				hm["command"] = quotedExe + cmd[len(variant.Current.Binary):]
				n++
			}
		}
	}
	if n == 0 {
		return nil, errors.New("no hook commands")
	}
	out, err := json.MarshalIndent(doc, "", "  ")
	return append(out, '\n'), err
}
