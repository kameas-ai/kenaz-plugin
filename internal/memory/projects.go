package memory

import (
	"context"
	"database/sql"
	"encoding/json"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"
)

// A project note leaves this device under one Fleet-owned id per project key. The key
// stays here. This file keeps the mapping, in the store's meta table, so that an older
// program can still open the store.
const (
	projectMetaPrefix = "memproj:"
	// projectRequestPrefix makes minting an id idempotent, on every device: the key is
	// the same on all of them, so they all get the same project.
	projectRequestPrefix = "memproj:"
	// fleetProjectPrefix is a local stand-in key for a project this device has only
	// heard of through a pulled note. It is replaced once the real key maps to that id.
	fleetProjectPrefix = "fp1-"
	// maxProjectName is Fleet's limit on a project's name.
	maxProjectName = 120
)

// Local states of a mapping. Fleet's "active", "merged" and "deleted", and "gone" when
// it says the project isn't there or this member can't see it.
const (
	projectActive  = "active"
	projectMerged  = "merged"
	projectDeleted = "deleted"
	projectGone    = "gone"
)

// ProjectMapping is what this device knows about one project key.
type ProjectMapping struct {
	ID    string `json:"id"`
	State string `json:"state"`
	Into  string `json:"into,omitempty"`
}

// final reports a key that never syncs again. A deleted key is never asked for again:
// Fleet would keep answering with the same tombstone.
func (m ProjectMapping) final() bool { return m.State == projectDeleted || m.State == projectGone }

// target is the id to send.
func (m ProjectMapping) target() string {
	if m.State == projectMerged && m.Into != "" {
		return m.Into
	}
	return m.ID
}

// ProjectName is the name Fleet stores for a project: its folder's name, never a path
// or a remote. One that looks like it holds a credential isn't sent, and nor is a home
// directory's, which is a username. This is the one place to change.
func ProjectName(key, root string, homes ...string) string {
	r := strings.TrimRight(strings.ReplaceAll(root, "\\", "/"), "/")
	// By hand: the path may be another platform's, and filepath treats C: its own way.
	dir, name := "", r
	if i := strings.LastIndex(r, "/"); i >= 0 {
		dir, name = r[:i], r[i+1:]
	}
	name = strings.TrimSpace(name)
	parent := strings.ToLower(dir[strings.LastIndex(dir, "/")+1:])
	unknown := name == "" || name == "." || name == ".." || strings.HasSuffix(name, ":") ||
		parent == "users" || parent == "home" || utf8.RuneCountInString(name) > maxProjectName ||
		strings.IndexFunc(name, unicode.IsControl) >= 0 || LooksLikeCredentialForSync(name)
	// A network share's server and its top folder can be a machine or a person.
	if strings.HasPrefix(r, "//") && strings.Count(strings.TrimPrefix(r, "//"), "/") < 2 {
		unknown = true
	}
	for _, home := range homes {
		if h := strings.TrimRight(strings.ReplaceAll(home, "\\", "/"), "/"); h != "" && strings.EqualFold(r, h) {
			unknown = true
		}
	}
	if unknown {
		return ProjectFallbackName(key)
	}
	return name
}

// ProjectFallbackName says nothing about the project.
func ProjectFallbackName(key string) string {
	k := strings.TrimPrefix(key, RemoteKeyPrefix)
	if len(k) > 8 {
		k = k[:8]
	}
	return "Project " + k
}

// ProjectRequestID is the idempotency key for minting key's id.
func ProjectRequestID(key string) string { return projectRequestPrefix + key }

// syncableProjectKey reports a key that can leave the device.
func syncableProjectKey(key string) bool {
	return IsPortableKey(key) || strings.HasPrefix(key, fleetProjectPrefix)
}

func loadProject(ctx context.Context, q rowQuerier, key string) (ProjectMapping, bool) {
	v, err := readMeta(ctx, q, projectMetaPrefix+key)
	if err != nil || v == "" {
		return ProjectMapping{}, false
	}
	var m ProjectMapping
	return m, json.Unmarshal([]byte(v), &m) == nil
}

func loadProjects(ctx context.Context, q interface {
	QueryContext(context.Context, string, ...any) (*sql.Rows, error)
}) (map[string]ProjectMapping, error) {
	rows, err := q.QueryContext(ctx, `SELECT key, value FROM meta WHERE key LIKE 'memproj:%'`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]ProjectMapping{}
	for rows.Next() {
		var k, v string
		if err := rows.Scan(&k, &v); err != nil {
			return nil, err
		}
		var m ProjectMapping
		if json.Unmarshal([]byte(v), &m) == nil {
			out[strings.TrimPrefix(k, projectMetaPrefix)] = m
		}
	}
	return out, rows.Err()
}

func saveProject(ctx context.Context, tx *sql.Tx, key string, m ProjectMapping) error {
	b, _ := json.Marshal(m)
	if err := setMeta(ctx, tx, projectMetaPrefix+key, string(b)); err != nil {
		return err
	}
	// Notes pulled under a stand-in for this id now have their real project.
	if IsPortableKey(key) {
		if id := m.target(); id != "" {
			stand := fleetProjectPrefix + id
			if _, err := tx.ExecContext(ctx, `UPDATE records SET project_key = ? WHERE project_key = ?`, key, stand); err != nil {
				return err
			}
			if _, err := tx.ExecContext(ctx, `DELETE FROM meta WHERE key = ?`, projectMetaPrefix+stand); err != nil {
				return err
			}
		}
	}
	return nil
}

// ProjectToMap is a project key with notes queued and no id yet, and the checkout its
// notes came from.
type ProjectToMap struct{ Key, Root string }

// ProjectsToMap lists them.
func (s *Store) ProjectsToMap(ctx context.Context, limit int) ([]ProjectToMap, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT r.project_key, max(r.project_root) FROM outbox o JOIN records r ON r.id = o.record_id
WHERE o.op = 'upsert' AND o.state = 'ready' AND r.scope = 'project' AND r.project_key LIKE 'rk1-%'
AND NOT EXISTS (SELECT 1 FROM meta m WHERE m.key = 'memproj:' || r.project_key) GROUP BY r.project_key LIMIT ?`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []ProjectToMap
	for rows.Next() {
		var p ProjectToMap
		if err := rows.Scan(&p.Key, &p.Root); err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

// metaProjectScopeSeen records that this member's settings on Fleet have included project
// notes since they last acknowledged the notice here. If project then goes missing,
// another client turned sync back on, and project sync needs a fresh acknowledgement.
const metaProjectScopeSeen = "project_scope_seen"

// ProjectScopeSeen reports it.
func (s *Store) ProjectScopeSeen(ctx context.Context) (bool, error) {
	v, err := readMeta(ctx, s.db, metaProjectScopeSeen)
	return v != "", err
}

// MarkProjectScopeSeen records it.
func (s *Store) MarkProjectScopeSeen(ctx context.Context) error {
	return s.tx(ctx, func(tx *sql.Tx) error { return setMeta(ctx, tx, metaProjectScopeSeen, "1") })
}

// ReleaseStrandedProjectNotes puts back project notes an older relay refused for good: it
// sent them without their project's id, and Fleet's answer was about that. Called when
// Fleet's settings have just been confirmed to take project notes.
func (s *Store) ReleaseStrandedProjectNotes(ctx context.Context) error {
	return s.tx(ctx, func(tx *sql.Tx) error {
		set, err := loadSettings(ctx, tx)
		if err != nil {
			return err
		}
		_, _, granted, err := s.grantRoom(ctx, tx, set)
		if err != nil {
			return err
		}
		rows, err := tx.QueryContext(ctx, `SELECT `+prefixedRecordCols+`, o.op_id FROM outbox o JOIN records r ON r.id = o.record_id
WHERE o.state = 'blocked' AND o.op = 'upsert' AND o.depends_on_op = 0 AND o.last_code IN ('invalid_field','scope_not_supported_yet')
AND r.scope = 'project' AND r.sync_state = 'rejected'`)
		if err != nil {
			return err
		}
		type stranded struct {
			r  record
			op int64
		}
		var found []stranded
		for rows.Next() {
			var st stranded
			dest := append(recordDest(&st.r), &st.op)
			if err := rows.Scan(dest...); err != nil {
				rows.Close()
				return err
			}
			found = append(found, st)
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return err
		}
		for _, st := range found {
			// An automatic note goes only under the grant, and a note goes only if it
			// still passes the scan.
			if (IsAuto(st.r.Kind) && !granted) || LooksLikeCredentialForSync(st.r.Title, st.r.Content, st.r.Source) {
				continue
			}
			for _, q := range []struct {
				sql  string
				args []any
			}{
				// Attempt stays above 0 if it was: it may have reached Fleet before it was refused.
				{`UPDATE outbox SET state = 'ready', attempt = min(attempt, 1), next_attempt_at = 0, last_code = '' WHERE op_id = ?`, []any{st.op}},
				// Swept up with it: they may never have been approved, so they ask.
				{`UPDATE outbox SET state = 'unapproved', last_code = '' WHERE depends_on_op = ? AND state = 'blocked'`, []any{st.op}},
				{`UPDATE records SET sync_state = ?, sync_code = '' WHERE id = ?`, []any{StatePending, st.r.ID}},
				{`UPDATE replacements SET phase = 'prepared', code = '' WHERE phase = 'blocked' AND new_upsert_op = ?`, []any{st.op}},
			} {
				if _, err := tx.ExecContext(ctx, q.sql, q.args...); err != nil {
					return err
				}
			}
		}
		return nil
	})
}

// HasProjectWork reports queued project notes, and ones an older relay left refused.
func (s *Store) HasProjectWork(ctx context.Context) (bool, error) {
	var n int
	err := s.db.QueryRowContext(ctx, `SELECT count(*) FROM outbox o JOIN records r ON r.id = o.record_id
WHERE o.op = 'upsert' AND r.scope = 'project' AND (o.state = 'ready' OR (o.state = 'blocked' AND o.last_code IN ('invalid_field','scope_not_supported_yet')))`).Scan(&n)
	return n > 0, err
}

// SetProject records what Fleet answered for key. id is the project's id, state its
// state, and into where a merged project went.
func (s *Store) SetProject(ctx context.Context, key, id, state, into string) error {
	return s.tx(ctx, func(tx *sql.Tx) error {
		return saveProject(ctx, tx, key, ProjectMapping{ID: id, State: state, Into: into})
	})
}

// ProjectScopeHold reports a refusal of project notes being waited out, and whether the
// wait is still running.
func (s *Store) ProjectScopeHold(ctx context.Context) (code string, holding bool) {
	st := loadClass(ctx, s.db, syncClassProject)
	return st.Code, st.holding(s.now().UnixMilli())
}

// HoldProjectScope records that Fleet doesn't take project notes for this member yet.
func (s *Store) HoldProjectScope(ctx context.Context, code string) error {
	return s.tx(ctx, func(tx *sql.Tx) error { return holdClass(ctx, tx, syncClassProject, code, s.now()) })
}

// CodeProjectScopeNotOffered is the project hold while Kameas Fleet doesn't say, in
// memory_project_scope, that it syncs project notes for the organization.
const CodeProjectScopeNotOffered = "project_scope_not_offered"

// projectOfferHold is how long one not-offered answer holds project notes. Each sync
// cycle renews it while the answer stands, so it can't lapse between two cycles.
const projectOfferHold = 24 * time.Hour

// HoldProjectScopeNotOffered keeps project notes, and edits of them, on this device:
// nothing in the project scope is sent while it holds. Deletions still go.
func (s *Store) HoldProjectScopeNotOffered(ctx context.Context) error {
	return s.tx(ctx, func(tx *sql.Tx) error {
		st := loadClass(ctx, tx, syncClassProject)
		st.Code, st.Until, st.Confirmed = CodeProjectScopeNotOffered, s.now().Add(projectOfferHold).UnixMilli(), false
		return saveClass(ctx, tx, syncClassProject, st)
	})
}

// ClearProjectScopeNotOffered lifts that hold, and only that one, once Fleet says it
// syncs project notes. A refusal Fleet gave stays to be waited out.
func (s *Store) ClearProjectScopeNotOffered(ctx context.Context) (bool, error) {
	cleared := false
	err := s.tx(ctx, func(tx *sql.Tx) error {
		if loadClass(ctx, tx, syncClassProject).Code != CodeProjectScopeNotOffered {
			return nil
		}
		cleared = true
		return saveClass(ctx, tx, syncClassProject, classState{})
	})
	return cleared, err
}

// ClearProjectScopeHold records that Fleet's settings now take project notes.
func (s *Store) ClearProjectScopeHold(ctx context.Context) error {
	return s.tx(ctx, func(tx *sql.Tx) error { return saveClass(ctx, tx, syncClassProject, classState{}) })
}

// projectKeyFor finds the local key for a pulled note's project id. An id this device
// hasn't mapped yet gets a stand-in, which saveProject replaces later.
func projectKeyFor(ctx context.Context, tx *sql.Tx, id string) (string, error) {
	maps, err := loadProjects(ctx, tx)
	if err != nil {
		return "", err
	}
	for k, m := range maps {
		if m.target() == id {
			return k, nil
		}
	}
	stand := fleetProjectPrefix + id
	return stand, saveProject(ctx, tx, stand, ProjectMapping{ID: id, State: projectActive})
}

// followPulledProject handles a known note arriving under another project id: a merge
// moves notes without changing their scope clock. The key stays, and now maps to the
// new id.
func (s *Store) followPulledProject(ctx context.Context, tx *sql.Tx, key, id string) error {
	m, ok := loadProject(ctx, tx, key)
	if !ok || m.target() == id || !IsPortableKey(key) {
		return nil
	}
	return saveProject(ctx, tx, key, ProjectMapping{ID: m.ID, State: projectMerged, Into: id})
}
