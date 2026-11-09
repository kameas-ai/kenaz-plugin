package memory

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// ProjectRoot returns the canonical absolute project root for dir: the
// enclosing git working tree's top level if there is one (found by
// walking up to a .git directory or file, without running git), else dir
// itself. Symlinks are resolved so one checkout always has one key.
func ProjectRoot(dir string) (string, error) {
	abs, err := filepath.Abs(dir)
	if err != nil {
		return "", err
	}
	if real, err := filepath.EvalSymlinks(abs); err == nil {
		abs = real
	}
	for d := abs; ; {
		if _, err := os.Lstat(filepath.Join(d, ".git")); err == nil {
			return d, nil
		}
		parent := filepath.Dir(d)
		if parent == d {
			return abs, nil
		}
		d = parent
	}
}

// ProjectKey is the first 16 hex characters of the SHA-256 of root: the key of
// a project with no git remote. It stays on one device.
func ProjectKey(root string) string {
	sum := sha256.Sum256([]byte(root))
	return hex.EncodeToString(sum[:])[:16]
}

// RemoteKeyPrefix marks a key derived from a git remote, the same on every
// device and in every worktree and clone.
const RemoteKeyPrefix = "rk1-"

// RemoteKey is the portable key of a normalised remote. Only the hash leaves
// this function, so the key names no host or repository.
func RemoteKey(normalised string) string {
	sum := sha256.Sum256([]byte(normalised))
	return RemoteKeyPrefix + hex.EncodeToString(sum[:])[:32]
}

// IsPortableKey reports whether a project key is the same across devices.
func IsPortableKey(key string) bool { return strings.HasPrefix(key, RemoteKeyPrefix) }

// NormalizeRemote reduces a git remote URL to host/path in lower case, so that
// the ssh and https forms of one repository agree, with no scheme, credentials,
// port or .git suffix. A remote on the local filesystem has no such form and
// returns "".
func NormalizeRemote(remote string) string {
	remote = strings.TrimSpace(remote)
	if remote == "" {
		return ""
	}
	var host, path string
	if i := strings.Index(remote, "://"); i >= 0 {
		u, err := url.Parse(remote)
		if err != nil || u.Scheme == "file" || u.Hostname() == "" {
			return ""
		}
		host, path = u.Hostname(), u.Path
	} else {
		// scp form: [user@]host:path. A colon after a slash is a local path.
		// Whatever comes before the @ is a user, perhaps with a password.
		// An @ after that, as in org/repo@v2.git, belongs to the path.
		for rest := remote; ; {
			at := strings.LastIndexByte(rest, '@')
			if at < 0 {
				break
			}
			slash := strings.IndexByte(remote, '/')
			if (slash < 0 || at < slash) && strings.Contains(rest[at+1:], ":") {
				remote = remote[at+1:]
				break
			}
			rest = rest[:at]
		}
		slash := strings.IndexByte(remote, '/')
		colon := strings.IndexByte(remote, ':')
		if colon <= 0 || (slash >= 0 && slash < colon) || len(remote) >= 2 && remote[1] == ':' {
			return ""
		}
		host, path = remote[:colon], remote[colon+1:]
	}
	path = strings.TrimSuffix(strings.Trim(path, "/"), ".git")
	path = strings.Trim(path, "/")
	if host == "" || path == "" {
		return ""
	}
	return strings.ToLower(host + "/" + path)
}

// gitCommonDir finds the directory that holds a checkout's shared git state: its
// .git directory, or for a linked worktree the main repository's. linked says
// which. A submodule's .git file names a git directory of its own, with no shared
// state, so a submodule is its own checkout. It reads files only, and returns ""
// when root is not a git checkout.
func gitCommonDir(root string) (common string, linked bool) {
	dotGit := filepath.Join(root, ".git")
	fi, err := os.Lstat(dotGit)
	if err != nil {
		return "", false
	}
	if fi.IsDir() {
		return dotGit, false
	}
	b, err := os.ReadFile(dotGit)
	if err != nil {
		return "", false
	}
	gitdir, ok := strings.CutPrefix(strings.TrimSpace(string(b)), "gitdir:")
	if !ok {
		return "", false
	}
	gitdir = strings.TrimSpace(gitdir)
	if !filepath.IsAbs(gitdir) {
		gitdir = filepath.Join(root, gitdir)
	}
	if c, err := os.ReadFile(filepath.Join(gitdir, "commondir")); err == nil {
		shared := strings.TrimSpace(string(c))
		if !filepath.IsAbs(shared) {
			shared = filepath.Join(gitdir, shared)
		}
		return filepath.Clean(shared), true
	}
	return filepath.Clean(gitdir), false
}

// gitRemote reads the remote's URL from the shared git config: origin, or the
// first remote by name when there is no origin.
func gitRemote(commonDir string) string {
	b, err := os.ReadFile(filepath.Join(commonDir, "config"))
	if err != nil {
		return ""
	}
	urls := map[string]string{}
	section := ""
	for _, line := range strings.Split(string(b), "\n") {
		line = strings.TrimSpace(line)
		switch {
		case strings.HasPrefix(line, "["):
			section = ""
			if rest, ok := strings.CutPrefix(line, `[remote "`); ok {
				section, _, _ = strings.Cut(rest, `"`)
			}
		case section != "":
			k, v, ok := strings.Cut(line, "=")
			if ok && strings.EqualFold(strings.TrimSpace(k), "url") {
				if _, seen := urls[section]; !seen {
					urls[section] = strings.TrimSpace(v)
				}
			}
		}
	}
	if u, ok := urls["origin"]; ok {
		return u
	}
	names := make([]string, 0, len(urls))
	for n := range urls {
		names = append(names, n)
	}
	sort.Strings(names)
	if len(names) > 0 {
		return urls[names[0]]
	}
	return ""
}

// Project is a resolved project: its key and its local root path. Root is the
// main checkout, also when dir is in a linked worktree, and the worktree itself
// when its repository is bare and so has no main checkout. Checkout is the top
// level of the working tree dir is in.
type Project struct {
	Key      string
	Root     string
	Checkout string
}

// ProjectFor resolves dir to a Project. Every worktree and clone of a repository
// with a network remote has the same key on every device. Anything else keeps a
// key from its local path, and a linked worktree shares the main checkout's.
func ProjectFor(dir string) (Project, error) {
	root, err := ProjectRoot(dir)
	if err != nil {
		return Project{}, err
	}
	common, linked := gitCommonDir(root)
	if common == "" {
		return Project{Key: ProjectKey(root), Root: root, Checkout: root}, nil
	}
	main := root
	if linked && filepath.Base(common) == ".git" {
		main = filepath.Dir(common)
		if real, err := filepath.EvalSymlinks(main); err == nil {
			main = real
		}
	}
	if n := NormalizeRemote(gitRemote(common)); n != "" {
		return Project{Key: RemoteKey(n), Root: main, Checkout: root}, nil
	}
	return Project{Key: ProjectKey(main), Root: main, Checkout: root}, nil
}

// metaProjectKeys records that this store's notes were moved to portable keys.
const metaProjectKeys = "project_keys_v2"

// migrateProjectKeys moves notes saved under an earlier path-based key to the
// project's key now, so that a project's notes survive the change and its
// worktrees' notes come together. It runs once per store. A note whose checkout
// is no longer on this device keeps its old key: nothing can say what it was.
func (s *Store) migrateProjectKeys(ctx context.Context) error {
	done, err := readMeta(ctx, s.db, metaProjectKeys)
	if err != nil || done != "" {
		return err
	}
	return s.tx(ctx, func(tx *sql.Tx) error {
		rows, err := tx.QueryContext(ctx, `SELECT DISTINCT project_key, project_root FROM records WHERE project_key != ? AND project_root != ''`, GlobalProject)
		if err != nil {
			return err
		}
		type pair struct{ key, root string }
		var pairs []pair
		for rows.Next() {
			var p pair
			if err := rows.Scan(&p.key, &p.root); err != nil {
				rows.Close()
				return err
			}
			pairs = append(pairs, p)
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return err
		}
		for _, p := range pairs {
			if IsPortableKey(p.key) || p.key != ProjectKey(p.root) {
				continue
			}
			if _, err := os.Lstat(p.root); err != nil {
				continue
			}
			now, err := ProjectFor(p.root)
			if err != nil || (now.Key == p.key && now.Root == p.root) {
				continue
			}
			if _, err := tx.ExecContext(ctx, `UPDATE records SET project_key = ?, project_root = ? WHERE project_key = ? AND project_root = ?`, now.Key, now.Root, p.key, p.root); err != nil {
				return err
			}
		}
		return setMeta(ctx, tx, metaProjectKeys, "1")
	})
}
