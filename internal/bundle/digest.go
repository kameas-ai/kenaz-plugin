package bundle

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// TreeDigest is the canonical plugin tree digest the release pipeline
// records with each release: sha256 over the lines
// "<sha256(file)>  <path>\n" for every file (plugin/... and
// .claude-plugin/marketplace.json), sorted bytewise by path. In a checkout
// of a release tag it equals
//
//	find plugin .claude-plugin/marketplace.json -type f -print0 |
//	  LC_ALL=C sort -z | xargs -0 sha256sum | sha256sum
func TreeDigest(files map[string][]byte) string {
	names := make([]string, 0, len(files))
	for n := range files {
		names = append(names, n)
	}
	sort.Strings(names)
	h := sha256.New()
	for _, n := range names {
		sum := sha256.Sum256(files[n])
		fmt.Fprintf(h, "%s  %s\n", hex.EncodeToString(sum[:]), n)
	}
	return hex.EncodeToString(h.Sum(nil))
}

// PluginDigest is TreeDigest of the bundle's verified plugin/ and
// .claude-plugin/marketplace.json.
func (b *Bundle) PluginDigest() string { return TreeDigest(b.verified) }

// PluginFiles reads plugin/ and .claude-plugin/marketplace.json under root
// (a marketplace checkout), refusing symlinks and special files.
func PluginFiles(root string) (map[string][]byte, error) {
	files := map[string][]byte{}
	top := filepath.Join(root, "plugin")
	err := filepath.WalkDir(top, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() && d.Type()&os.ModeSymlink == 0 {
			return nil
		}
		rel, _ := filepath.Rel(root, p)
		rel = filepath.ToSlash(rel)
		data, err := readRegular(root, rel)
		if err != nil {
			return err
		}
		files[rel] = data
		return nil
	})
	if err != nil {
		return nil, err
	}
	const mk = ".claude-plugin/marketplace.json"
	data, err := readRegular(root, mk)
	if err != nil {
		return nil, err
	}
	files[mk] = data
	if len(files) == 1 {
		return nil, fmt.Errorf("%s has no plugin files", strings.TrimSuffix(top, "/"))
	}
	return files, nil
}
