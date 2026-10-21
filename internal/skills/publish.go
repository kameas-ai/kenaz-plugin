package skills

import (
	"encoding/json"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
)

// Package reads a local Agent Skills directory into an agent_skill/v1
// payload for publishing (skills_publish). Only regular files are
// read; symlinks and special files are refused. Dot-files and
// dot-directories (.git, .DS_Store, a .kameas-managed marker) are skipped.
// The result passes the same rules a relay applies before writing.
func Package(dir string) (*AgentSkill, []byte, error) {
	fi, err := os.Lstat(dir)
	if err != nil {
		return nil, nil, invalid("path", "cannot read the skill directory")
	}
	if !fi.IsDir() {
		return nil, nil, invalid("path", "must be a directory containing SKILL.md")
	}
	files := map[string]string{}
	total := 0
	err = filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if p == dir {
			return nil
		}
		name := d.Name()
		if strings.HasPrefix(name, ".") {
			if d.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		if d.Type()&fs.ModeSymlink != 0 {
			return invalid("files", "%q is a symlink; only regular files can be published", name)
		}
		if d.IsDir() {
			return nil
		}
		if !d.Type().IsRegular() {
			return invalid("files", "%q is not a regular file", name)
		}
		rel, err := filepath.Rel(dir, p)
		if err != nil {
			return err
		}
		rel = filepath.ToSlash(rel)
		if err := CheckPath(rel); err != nil {
			return err
		}
		if len(files) >= MaxFiles {
			return invalid("files", "at most %d files", MaxFiles)
		}
		b, err := os.ReadFile(p)
		if err != nil {
			return err
		}
		total += len(b)
		if total > MaxBytes {
			return invalid("payload", "skill exceeds %d bytes", MaxBytes)
		}
		files[rel] = string(b)
		return nil
	})
	if err != nil {
		return nil, nil, err
	}
	manifest, ok := files[ManifestName]
	if !ok {
		return nil, nil, invalid("files", "%s is required", ManifestName)
	}
	fm, err := ParseFrontmatter(manifest)
	if err != nil {
		return nil, nil, err
	}
	s := &AgentSkill{Schema: SchemaV1, Name: fm.Get("name"), Description: strings.TrimSpace(fm.Get("description")), Files: files}
	if err := s.Validate(); err != nil {
		return nil, nil, err
	}
	payload, err := json.Marshal(s)
	if err != nil {
		return nil, nil, err
	}
	if len(payload) > MaxBytes {
		return nil, nil, invalid("payload", "exceeds %d bytes", MaxBytes)
	}
	return s, payload, nil
}
