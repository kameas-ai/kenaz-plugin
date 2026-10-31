package setup

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/kameas-ai/kenaz-plugin/internal/bundle"
	"github.com/kameas-ai/kenaz-plugin/internal/variant"
)

// Release pinning: the plugin is pinned to a signed release, and no
// supported install path tracks a moving branch. A GitHub marketplace counts
// as this release only when it is pinned to the full commit the release tag
// points at; anything else (no ref, a branch, a tag, another commit) is
// treated as moving.
//
//   - Claude Code: only an inline "settings" catalog whose kenaz entry sets
//     sha. A github/git marketplace source's ref is a branch or tag name,
//     never a commit, however it looks.
//   - Codex: ref = "<commit>" in config.toml is only a claim. Codex runs
//     `git checkout <ref>`, which prefers a branch named like the commit, so
//     whoever controls the repository can serve other content under that
//     name. Setup therefore reads the HEAD of Codex's marketplace clone and,
//     when a verified bundle is at hand, compares the clone's plugin tree
//     with the bundle's.

// Release identifies the signed release a kenaz was built from.
type Release struct {
	// Version is the release version without the leading "v" (app.Version).
	Version string
	// Commit is the full public commit the release tag points at
	// (app.BuildCommit, set by the official release pipeline).
	Commit string
}

var (
	fullCommit = regexp.MustCompile(`^[0-9a-f]{40}$`)
	releaseTag = regexp.MustCompile(`^v[0-9]+\.[0-9]+\.[0-9]+(-[0-9A-Za-z.-]+)?$`)
)

// Tag is the public repository's release tag, "v<Version>".
func (r Release) Tag() string { return "v" + strings.TrimPrefix(r.Version, "v") }

// Pinnable reports whether r names a release tag and its full commit.
func (r Release) Pinnable() bool {
	return releaseTag.MatchString(r.Tag()) && fullCommit.MatchString(r.Commit)
}

// isCommit reports whether s is a full 40-hex commit (any case).
func isCommit(s string) bool { return fullCommit.MatchString(strings.ToLower(s)) }

// pinnedTo reports whether m is pinned to commit, the full commit of this
// build's release. An unofficial build (no commit) never matches.
func (m marketplace) pinnedTo(commit string) bool {
	return m.Pin != "" && isCommit(commit) && strings.EqualFold(m.Pin, commit)
}

// describeRef says what an unpinned official marketplace follows.
func (m marketplace) describeRef() string {
	if m.Ref == "" {
		return "the repository's default branch"
	}
	if m.Pin != "" {
		return "commit " + m.Ref
	}
	return "ref " + m.Ref
}

// pinnedAddHint is the command that registers the GitHub marketplace pinned
// to r, or "" when the host cannot pin a marketplace to a commit from its
// CLI (Claude Code: a github/git marketplace source takes a branch or tag,
// not a commit; only the managed snippet's inline catalog pins a commit).
func pinnedAddHint(h harness, r Release) string {
	if h == harnessCodex && r.Pinnable() {
		return fmt.Sprintf("codex plugin marketplace add %s --ref %s", OfficialRepo, r.Commit)
	}
	return ""
}

// pinHint is a sentence naming the pinned registration command, if any.
func pinHint(h harness, r Release) string {
	if c := pinnedAddHint(h, r); c != "" {
		return fmt.Sprintf(" To keep using GitHub, register it pinned to this release: `%s plugin marketplace remove %s`, then `%s`.", hostCLI(h), bundleMarketplace, c)
	}
	return ""
}

// readdHint is the command that adds the GitHub marketplace back after
// --undo, in the most pinned form the host's CLI accepts: a commit for
// Codex, the release tag for Claude Code (whose marketplace sources take
// no commit), the bare repository for a build with no release.
func readdHint(h harness, r Release) string {
	if c := pinnedAddHint(h, r); c != "" {
		return c
	}
	if r.Pinnable() {
		return fmt.Sprintf("%s plugin marketplace add %s#%s", hostCLI(h), OfficialRepo, r.Tag())
	}
	return fmt.Sprintf("%s plugin marketplace add %s", hostCLI(h), OfficialRepo)
}

// codexCloneDir is where Codex materializes a git marketplace
// (codex-rs core-plugins installed_marketplaces.rs: $CODEX_HOME/.tmp/marketplaces/<name>).
func (o Options) codexCloneDir() string {
	return filepath.Join(o.CodexDir, ".tmp", "marketplaces", bundleMarketplace)
}

// gitHead resolves the commit checked out in the git work tree dir, read
// from its files (no git process): a detached HEAD, or the branch HEAD
// names, loose or packed.
func gitHead(dir string) (string, error) {
	gd := filepath.Join(dir, ".git")
	b, err := os.ReadFile(filepath.Join(gd, "HEAD"))
	if err != nil {
		return "", err
	}
	head := strings.TrimSpace(string(b))
	ref, sym := strings.CutPrefix(head, "ref: ")
	if !sym {
		if !isCommit(head) {
			return "", fmt.Errorf("unreadable HEAD %q", head)
		}
		return strings.ToLower(head), nil
	}
	if !strings.HasPrefix(ref, "refs/") || strings.Contains(ref, "..") {
		return "", fmt.Errorf("unexpected HEAD ref %q", ref)
	}
	if b, err := os.ReadFile(filepath.Join(gd, filepath.FromSlash(ref))); err == nil {
		if c := strings.TrimSpace(string(b)); isCommit(c) {
			return strings.ToLower(c), nil
		}
	}
	if b, err := os.ReadFile(filepath.Join(gd, "packed-refs")); err == nil {
		for _, l := range strings.Split(string(b), "\n") {
			if f := strings.Fields(l); len(f) == 2 && f[1] == ref && isCommit(f[0]) {
				return strings.ToLower(f[0]), nil
			}
		}
	}
	return "", fmt.Errorf("HEAD names %s, which does not resolve", ref)
}

// releasePin reports whether the official marketplace mk in h is pinned to
// this build's release, and if not, why. For Codex it also checks what
// Codex actually checked out: the clone's HEAD must be the release commit
// and, with a verified bundle (b non-nil), its plugin tree must equal the
// bundle's.
func (o Options) releasePin(h harness, mk marketplace, b *Bundle) (bool, string) {
	if !mk.official() || !mk.pinnedTo(o.Release.Commit) {
		return false, "follows " + mk.describeRef() + ", not this release's signed commit"
	}
	if h != harnessCodex {
		return true, ""
	}
	dir := o.codexCloneDir()
	head, err := gitHead(dir)
	switch {
	case err != nil:
		return false, fmt.Sprintf("is configured with ref %s, but its checkout (%s) cannot be verified: %v", mk.Ref, dir, err)
	case head != strings.ToLower(o.Release.Commit):
		return false, fmt.Sprintf("is configured with ref %s, but Codex checked out %s (a branch named like the commit?)", mk.Ref, head)
	}
	if b != nil {
		files, err := bundle.PluginFiles(dir)
		if err != nil {
			return false, fmt.Sprintf("is pinned, but its plugin files cannot be verified: %v", err)
		}
		if bundle.TreeDigest(files) != b.PluginDigest() {
			return false, "is pinned, but its plugin files differ from this release's verified copy"
		}
	}
	return true, ""
}

// noBundleHint says how to pin a GitHub marketplace when setup has no
// verified copy to register instead (Homebrew, npm, --no-bundle).
func noBundleHint(h harness, r Release) string {
	bin := variant.Current.Binary
	if h == harnessCodex {
		if c := pinnedAddHint(h, r); c != "" {
			return fmt.Sprintf("To pin it to this release: `codex plugin marketplace remove %s`, then `%s` (Codex does not verify what that ref checks out; `%s setup doctor` does), or install Kenaz from a release archive (install.sh), whose verified copy setup registers.", bundleMarketplace, c, bin)
		}
		return "To pin it, install Kenaz from a release archive (install.sh), whose verified copy setup registers."
	}
	return fmt.Sprintf("To pin it to this release, replace its entry in ~/.claude/settings.json with the extraKnownMarketplaces entry from `%s setup print-managed claude --raw` (an inline catalog with the release tag and commit, auto-update off), or install Kenaz from a release archive (install.sh), whose verified copy setup registers.", bin)
}
