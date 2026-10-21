package memory

import (
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
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

// ProjectKey is the first 16 hex characters of the SHA-256 of root.
func ProjectKey(root string) string {
	sum := sha256.Sum256([]byte(root))
	return hex.EncodeToString(sum[:])[:16]
}

// Project is a resolved project: its key and its local root path.
type Project struct {
	Key  string
	Root string
}

// ProjectFor resolves dir to a Project.
func ProjectFor(dir string) (Project, error) {
	root, err := ProjectRoot(dir)
	if err != nil {
		return Project{}, err
	}
	return Project{Key: ProjectKey(root), Root: root}, nil
}
