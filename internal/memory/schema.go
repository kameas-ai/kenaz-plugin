package memory

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
)

// SchemaVersion is the current store schema (PRAGMA user_version).
//
//	1  the on-device notebook shipped first: one memories table + FTS5.
//	2  records/aliases/tombstones/outbox/sync_state/replacements for
//	   opt-in Fleet sync (018 storage-sync contract); memories rows are
//	   copied into records losslessly and the v1 table is dropped.
//
// A store is v2 when its meta and records tables exist, whatever
// user_version says: an older binary that opens it recreates an empty
// memories table and sets user_version to 1 (see repairDowngraded).
const SchemaVersion = 2

// ErrNewerSchema means the store was written by a newer Kenaz. It is left
// untouched.
var ErrNewerSchema = errors.New("this memory store was written by a newer version of Kenaz; update Kenaz to use it")

// ErrCorruptStore means the file is not a usable memory store.
var ErrCorruptStore = errors.New("the memory store is damaged or not a Kenaz memory store")

// schemaV1 is the shipped v1 schema, verbatim. It is created for a brand
// new file and then migrated like any v1 store, so new and upgraded stores
// take the same path.
const schemaV1 = `
CREATE TABLE IF NOT EXISTS memories (
  id           TEXT PRIMARY KEY,
  created_at   INTEGER NOT NULL,
  updated_at   INTEGER NOT NULL,
  project_key  TEXT NOT NULL,
  project_root TEXT NOT NULL DEFAULT '',
  title        TEXT NOT NULL,
  body         TEXT NOT NULL,
  tags         TEXT NOT NULL DEFAULT '[]',
  source       TEXT NOT NULL
);
CREATE INDEX IF NOT EXISTS memories_project ON memories(project_key, created_at);
CREATE VIRTUAL TABLE IF NOT EXISTS memories_fts USING fts5(
  title, body, tags, content='memories', content_rowid='rowid'
);
CREATE TRIGGER IF NOT EXISTS memories_ai AFTER INSERT ON memories BEGIN
  INSERT INTO memories_fts(rowid, title, body, tags) VALUES (new.rowid, new.title, new.body, new.tags);
END;
CREATE TRIGGER IF NOT EXISTS memories_ad AFTER DELETE ON memories BEGIN
  INSERT INTO memories_fts(memories_fts, rowid, title, body, tags) VALUES ('delete', old.rowid, old.title, old.body, old.tags);
END;
`

// migrateV2 turns a v1 store into v2. It runs inside one transaction with
// the user_version bump, so a crash leaves either v1 or v2, never a mix.
//
// records is the contract's record table; the v1 columns map 1:1 (body →
// content). Notes saved under project "global" get scope global, the rest
// scope project (which Fleet does not sync yet). Upgraded notes are
// sync_state 'local': nothing saved before this version is ever uploaded
// without a new, explicit user action.
//
// The search index covers only visible records: the triggers index a row
// while visibility='visible' and unindex it when it is hidden or deleted.
const migrateV2 = `
CREATE TABLE meta (
  key   TEXT PRIMARY KEY,
  value TEXT NOT NULL
);
CREATE TABLE records (
  id            TEXT NOT NULL UNIQUE,
  request_key   TEXT UNIQUE,
  content       TEXT NOT NULL,
  content_hash  TEXT NOT NULL,
  title         TEXT NOT NULL DEFAULT '',
  kind          TEXT NOT NULL DEFAULT 'raw',
  scope         TEXT NOT NULL,
  project_key   TEXT NOT NULL DEFAULT '',
  project_root  TEXT NOT NULL DEFAULT '',
  tags          TEXT NOT NULL DEFAULT '[]',
  source        TEXT NOT NULL DEFAULT '',
  provenance    TEXT NOT NULL DEFAULT 'device',
  created_at    INTEGER NOT NULL,
  updated_at    INTEGER NOT NULL,
  created_hlc   TEXT NOT NULL DEFAULT '',
  title_hlc     TEXT NOT NULL DEFAULT '',
  scope_hlc     TEXT NOT NULL DEFAULT '',
  pinned        INTEGER NOT NULL DEFAULT 0,
  pinned_hlc    TEXT NOT NULL DEFAULT '',
  visibility    TEXT NOT NULL DEFAULT 'visible',
  sync_state    TEXT NOT NULL DEFAULT 'local',
  sync_code     TEXT NOT NULL DEFAULT '',
  server_seq    TEXT NOT NULL DEFAULT '',
  version       INTEGER NOT NULL DEFAULT 1
);
CREATE INDEX records_project ON records(project_key, created_at);
CREATE INDEX records_sync ON records(sync_state);
CREATE TABLE aliases (
  alias_id     TEXT PRIMARY KEY,
  canonical_id TEXT NOT NULL
);
CREATE TABLE tombstones (
  id         TEXT PRIMARY KEY,
  reason     TEXT NOT NULL,
  hlc        TEXT NOT NULL DEFAULT '',
  server_seq TEXT NOT NULL DEFAULT '',
  acked      INTEGER NOT NULL DEFAULT 0,
  created_at INTEGER NOT NULL
);
CREATE TABLE outbox (
  op_id           INTEGER PRIMARY KEY AUTOINCREMENT,
  request_key     TEXT NOT NULL UNIQUE,
  record_id       TEXT NOT NULL,
  op              TEXT NOT NULL,
  payload         BLOB NOT NULL,
  depends_on_op   INTEGER NOT NULL DEFAULT 0,
  state           TEXT NOT NULL,
  attempt         INTEGER NOT NULL DEFAULT 0,
  ever_sent       INTEGER NOT NULL DEFAULT 0,
  next_attempt_at INTEGER NOT NULL DEFAULT 0,
  last_code       TEXT NOT NULL DEFAULT '',
  created_at      INTEGER NOT NULL
);
CREATE INDEX outbox_ready ON outbox(state, next_attempt_at);
CREATE INDEX outbox_record ON outbox(record_id);
CREATE TABLE sync_state (
  singleton           INTEGER PRIMARY KEY CHECK (singleton = 1),
  enabled             INTEGER NOT NULL DEFAULT 0,
  ever_enabled        INTEGER NOT NULL DEFAULT 0,
  consent_version     TEXT NOT NULL DEFAULT '',
  enabled_scopes      TEXT NOT NULL DEFAULT '["global","long_term"]',
  cursor              TEXT NOT NULL DEFAULT '',
  last_hlc            TEXT NOT NULL DEFAULT '',
  reset_phase         TEXT NOT NULL DEFAULT '',
  reset_reason        TEXT NOT NULL DEFAULT '',
  reset_erased_before TEXT NOT NULL DEFAULT '',
  snapshot_generation INTEGER NOT NULL DEFAULT 0,
  erased_before       TEXT NOT NULL DEFAULT '',
  erase_pending       INTEGER NOT NULL DEFAULT 0,
  erase_barrier_hlc   TEXT NOT NULL DEFAULT '',
  own_erase_before    TEXT NOT NULL DEFAULT '',
  profile_generation  INTEGER NOT NULL DEFAULT 0,
  lease_holder        TEXT NOT NULL DEFAULT '',
  lease_until         INTEGER NOT NULL DEFAULT 0,
  backoff_until       INTEGER NOT NULL DEFAULT 0,
  backoff_step        INTEGER NOT NULL DEFAULT 0,
  status_code         TEXT NOT NULL DEFAULT '',
  last_pull_at        INTEGER NOT NULL DEFAULT 0,
  last_push_at        INTEGER NOT NULL DEFAULT 0
);
INSERT INTO sync_state(singleton) VALUES (1);
CREATE TABLE replacements (
  job_id         INTEGER PRIMARY KEY AUTOINCREMENT,
  old_id         TEXT NOT NULL,
  new_id         TEXT NOT NULL,
  new_upsert_op  INTEGER NOT NULL DEFAULT 0,
  old_forget_op  INTEGER NOT NULL DEFAULT 0,
  phase          TEXT NOT NULL,
  code           TEXT NOT NULL DEFAULT ''
);
CREATE TABLE applied_requests (
  request_key TEXT PRIMARY KEY,
  operation   TEXT NOT NULL,
  result      BLOB NOT NULL,
  created_at  INTEGER NOT NULL
);
CREATE TABLE snapshot_stage (
  generation INTEGER NOT NULL,
  id         TEXT NOT NULL,
  record     BLOB NOT NULL,
  PRIMARY KEY (generation, id)
);

INSERT INTO records(rowid, id, content, content_hash, title, kind, scope, project_key, project_root, tags, source,
                    provenance, created_at, updated_at, visibility, sync_state)
SELECT rowid, id, body, '', title, 'raw', CASE WHEN project_key = 'global' THEN 'global' ELSE 'project' END,
       project_key, project_root, tags, source, 'device', created_at, updated_at, 'visible', 'local'
FROM memories;

DROP TRIGGER memories_ai;
DROP TRIGGER memories_ad;
DROP TABLE memories_fts;
DROP TABLE memories;

CREATE VIRTUAL TABLE records_fts USING fts5(
  title, content, tags, content='records', content_rowid='rowid'
);
INSERT INTO records_fts(rowid, title, content, tags)
  SELECT rowid, title, content, tags FROM records WHERE visibility = 'visible';
CREATE TRIGGER records_ai AFTER INSERT ON records WHEN new.visibility = 'visible' BEGIN
  INSERT INTO records_fts(rowid, title, content, tags) VALUES (new.rowid, new.title, new.content, new.tags);
END;
CREATE TRIGGER records_ad AFTER DELETE ON records WHEN old.visibility = 'visible' BEGIN
  INSERT INTO records_fts(records_fts, rowid, title, content, tags) VALUES ('delete', old.rowid, old.title, old.content, old.tags);
END;
CREATE TRIGGER records_au_del AFTER UPDATE OF title, content, tags, visibility ON records WHEN old.visibility = 'visible' BEGIN
  INSERT INTO records_fts(records_fts, rowid, title, content, tags) VALUES ('delete', old.rowid, old.title, old.content, old.tags);
END;
CREATE TRIGGER records_au_ins AFTER UPDATE OF title, content, tags, visibility ON records WHEN new.visibility = 'visible' BEGIN
  INSERT INTO records_fts(rowid, title, content, tags) VALUES (new.rowid, new.title, new.content, new.tags);
END;
`

// migrate brings the open database to SchemaVersion. Each step runs in a
// transaction together with its user_version bump. hashFill computes the
// content hashes migrated rows still lack (SQLite has no sha256).
func migrate(ctx context.Context, db *sql.DB, p Profile, newDevice func() string) error {
	// Cheap check without the write lock: an up-to-date store needs no
	// lock at all.
	var v int
	if err := db.QueryRowContext(ctx, `PRAGMA user_version`).Scan(&v); err != nil {
		return fmt.Errorf("%w: %v", ErrCorruptStore, err)
	}
	if v == SchemaVersion {
		stray, err := hasTable(ctx, db, "memories")
		if err != nil {
			return fmt.Errorf("%w: %v", ErrCorruptStore, err)
		}
		if !stray {
			return nil
		}
	}
	// Every step re-reads the version inside one BEGIN IMMEDIATE
	// transaction (the writer DSN's _txlock), so concurrent openers
	// (an MCP server, the CLI, the relay) serialize and each applies only
	// the steps still missing. A brand-new file goes from 0 to 2 in a
	// single transaction, so no other process ever sees a half-made store.
	err := inTx(ctx, db, func(tx *sql.Tx) error {
		if err := tx.QueryRowContext(ctx, `PRAGMA user_version`).Scan(&v); err != nil {
			return fmt.Errorf("%w: %v", ErrCorruptStore, err)
		}
		if v <= SchemaVersion {
			// The v2 tables decide, not user_version: a shipped v1 binary
			// opening a v2 store runs its CREATE IF NOT EXISTS DDL (an empty
			// memories table) and sets user_version back to 1.
			isV2, err := hasV2Tables(ctx, tx)
			if err != nil {
				return fmt.Errorf("%w: %v", ErrCorruptStore, err)
			}
			if isV2 {
				return repairDowngraded(ctx, tx)
			}
		}
		switch {
		case v > SchemaVersion:
			return ErrNewerSchema
		case v == SchemaVersion:
			return nil
		case v == 1 && !p.IsLocal():
			// Only the device notebook ever had a v1 store: an account
			// profile never adopts one (its notes were promised to stay
			// on the device).
			return ErrProfileMismatch
		}
		if v == 0 {
			// A brand-new file, or a v1 store from before user_version was
			// set: v1's DDL is idempotent (IF NOT EXISTS).
			if _, err := tx.ExecContext(ctx, schemaV1); err != nil {
				return fmt.Errorf("%w: %v", ErrCorruptStore, err)
			}
		}
		if _, err := tx.ExecContext(ctx, migrateV2); err != nil {
			return fmt.Errorf("memory store upgrade: %w", err)
		}
		if err := fillHashes(ctx, tx); err != nil {
			return err
		}
		if err := bindProfile(ctx, tx, p, newDevice()); err != nil {
			return err
		}
		_, err := tx.ExecContext(ctx, `PRAGMA user_version = 2`)
		return err
	})
	if err != nil && !errors.Is(err, ErrNewerSchema) && !errors.Is(err, ErrProfileMismatch) && !errors.Is(err, ErrCorruptStore) &&
		!strings.HasPrefix(err.Error(), "memory store upgrade") {
		return fmt.Errorf("%w: %v", ErrCorruptStore, err)
	}
	return err
}

// hasTable reports whether the database has a table named name.
func hasTable(ctx context.Context, q interface {
	QueryRowContext(context.Context, string, ...any) *sql.Row
}, name string) (bool, error) {
	var n int
	err := q.QueryRowContext(ctx, `SELECT count(*) FROM sqlite_master WHERE type = 'table' AND name = ?`, name).Scan(&n)
	return n > 0, err
}

// hasV2Tables reports whether the v2 schema's own tables exist.
func hasV2Tables(ctx context.Context, tx *sql.Tx) (bool, error) {
	var n int
	err := tx.QueryRowContext(ctx, `SELECT count(*) FROM sqlite_master WHERE type = 'table' AND name IN ('meta','records')`).Scan(&n)
	return n == 2, err
}

// repairDowngraded restores a v2 store that an older (v1) Kenaz opened:
// notes that binary saved into its stray memories table are merged into
// records (an id already there is kept as is, so a repeat repair never
// duplicates), exactly as the v1→v2 upgrade copies them (sync_state
// 'local': never uploaded without a new user action); then the v1 tables
// are dropped and user_version is set back to 2. It runs inside the
// caller's transaction.
func repairDowngraded(ctx context.Context, tx *sql.Tx) error {
	stray, err := hasTable(ctx, tx, "memories")
	if err != nil {
		return err
	}
	if stray {
		if _, err := tx.ExecContext(ctx, `
INSERT INTO records(id, content, content_hash, title, kind, scope, project_key, project_root, tags, source,
                    provenance, created_at, updated_at, visibility, sync_state)
SELECT m.id, m.body, '', m.title, 'raw', CASE WHEN m.project_key = 'global' THEN 'global' ELSE 'project' END,
       m.project_key, m.project_root, m.tags, m.source, 'device', m.created_at, m.updated_at, 'visible', 'local'
FROM memories m
WHERE NOT EXISTS (SELECT 1 FROM records r WHERE r.id = m.id)
ORDER BY m.rowid`); err != nil {
			return fmt.Errorf("memory store repair: %w", err)
		}
		if err := fillHashes(ctx, tx); err != nil {
			return err
		}
	}
	for _, q := range []string{
		`DROP TRIGGER IF EXISTS memories_ai`,
		`DROP TRIGGER IF EXISTS memories_ad`,
		`DROP TABLE IF EXISTS memories_fts`,
		`DROP INDEX IF EXISTS memories_project`,
		`DROP TABLE IF EXISTS memories`,
		`PRAGMA user_version = 2`,
	} {
		if _, err := tx.ExecContext(ctx, q); err != nil {
			return fmt.Errorf("memory store repair: %w", err)
		}
	}
	return nil
}

func fillHashes(ctx context.Context, tx *sql.Tx) error {
	rows, err := tx.QueryContext(ctx, `SELECT id, content FROM records WHERE content_hash = ''`)
	if err != nil {
		return err
	}
	type pair struct{ id, hash string }
	var todo []pair
	for rows.Next() {
		var id, c string
		if err := rows.Scan(&id, &c); err != nil {
			rows.Close()
			return err
		}
		todo = append(todo, pair{id, ContentHash(c)})
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return err
	}
	for _, p := range todo {
		if _, err := tx.ExecContext(ctx, `UPDATE records SET content_hash = ? WHERE id = ?`, p.hash, p.id); err != nil {
			return err
		}
	}
	return nil
}

func inTx(ctx context.Context, db *sql.DB, fn func(*sql.Tx) error) error {
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	if err := fn(tx); err != nil {
		tx.Rollback()
		return err
	}
	return tx.Commit()
}
