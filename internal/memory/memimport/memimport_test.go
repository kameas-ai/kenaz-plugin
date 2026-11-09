package memimport

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/kameas-ai/kenaz-plugin/internal/memory"
)

const (
	promptCanary  = "PROMPT-CANARY-do-not-import"
	queueCanary   = "QUEUE-CANARY-do-not-import"
	requestCanary = "REQUEST-CANARY-do-not-import"
	rawCanary     = "RAW-TEXT-CANARY-do-not-import"
	firstCanary   = "FIRST-PROMPT-CANARY-do-not-import"
)

var now = time.Date(2026, 11, 4, 12, 0, 0, 0, time.UTC)

// fixture builds a database with the layout's tables and columns, all invented data.
func fixture(t *testing.T) (*sql.DB, string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "other.db")
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	for _, q := range []string{
		`CREATE TABLE sdk_sessions (id INTEGER PRIMARY KEY, content_session_id TEXT, memory_session_id TEXT, project TEXT, user_prompt TEXT)`,
		`CREATE TABLE user_prompts (id INTEGER PRIMARY KEY, content_session_id TEXT, prompt_number INTEGER, prompt_text TEXT, created_at TEXT, created_at_epoch INTEGER)`,
		`CREATE TABLE pending_messages (id INTEGER PRIMARY KEY, session_db_id INTEGER, content_session_id TEXT, message_type TEXT, tool_name TEXT, tool_input TEXT, tool_response TEXT, cwd TEXT, last_user_message TEXT, last_assistant_message TEXT, created_at_epoch INTEGER)`,
		`CREATE TABLE observations (id INTEGER PRIMARY KEY, memory_session_id TEXT, project TEXT, text TEXT, type TEXT, title TEXT, subtitle TEXT, facts TEXT, narrative TEXT, concepts TEXT, files_read TEXT, files_modified TEXT, prompt_number INTEGER, discovery_tokens INTEGER, created_at TEXT, created_at_epoch INTEGER)`,
		`CREATE TABLE session_summaries (id INTEGER PRIMARY KEY, memory_session_id TEXT, project TEXT, request TEXT, investigated TEXT, learned TEXT, completed TEXT, next_steps TEXT, files_read TEXT, files_edited TEXT, notes TEXT, prompt_number INTEGER, discovery_tokens INTEGER, created_at TEXT, created_at_epoch INTEGER)`,
		`INSERT INTO sdk_sessions(content_session_id, memory_session_id, project, user_prompt) VALUES ('c1','m1','app','` + firstCanary + `')`,
		`INSERT INTO user_prompts(content_session_id, prompt_number, prompt_text, created_at, created_at_epoch) VALUES ('c1',1,'` + promptCanary + `','x',1)`,
		`INSERT INTO pending_messages(session_db_id, content_session_id, message_type, tool_input, tool_response, cwd, created_at_epoch) VALUES (1,'c1','observation','` + queueCanary + `','` + queueCanary + `','/x',1)`,
	} {
		if _, err := db.Exec(q); err != nil {
			t.Fatal(err)
		}
	}
	return db, path
}

func addObs(t *testing.T, db *sql.DB, typ, title, narrative string, age time.Duration) {
	t.Helper()
	_, err := db.Exec(`INSERT INTO observations(memory_session_id, project, text, type, title, subtitle, facts, narrative, concepts, files_read, files_modified, discovery_tokens, created_at, created_at_epoch)
VALUES ('m1','app',?,?,?,'a subtitle','["fact one","fact two"]',?,'["how-it-works"]','["/home/someone/app/main.go"]','[]',42,'x',?)`, rawCanary, typ, title, narrative, now.Add(-age).UnixMilli())
	if err != nil {
		t.Fatal(err)
	}
}

func addSummary(t *testing.T, db *sql.DB, learned string, age time.Duration) {
	t.Helper()
	_, err := db.Exec(`INSERT INTO session_summaries(memory_session_id, project, request, investigated, learned, completed, next_steps, notes, discovery_tokens, created_at, created_at_epoch)
VALUES ('m1','app',?,'looked at it',?,'done','nothing','','7','x',?)`, requestCanary, learned, now.Add(-age).UnixMilli())
	if err != nil {
		t.Fatal(err)
	}
}

func opts(path string) Options { return Options{DB: path, Home: "/home/someone", Now: now} }

func fileHash(t *testing.T, path string) [32]byte {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return sha256.Sum256(b)
}

const day = 24 * time.Hour

// It takes decisions of any age, recent fixes and summaries, and nothing else.
func TestSelection(t *testing.T) {
	db, path := fixture(t)
	addObs(t, db, "decision", "an old decision", "why", 400*day)
	addObs(t, db, "decision", "a new decision", "why", day)
	addObs(t, db, "bugfix", "a recent fix", "fixed", 3*day)
	addObs(t, db, "bugfix", "an old fix", "fixed", 90*day)
	addObs(t, db, "discovery", "a discovery", "found", day)
	addObs(t, db, "change", "a change", "changed", day)
	addSummary(t, db, "a recent lesson", 2*day)
	addSummary(t, db, "an old lesson", 90*day)
	plan, err := Prepare(context.Background(), opts(path))
	if err != nil {
		t.Fatal(err)
	}
	if plan.Decisions != 2 || plan.Bugfixes != 1 || plan.Summaries != 1 {
		t.Errorf("%+v", plan)
	}
}

// What was never meant to come over doesn't, by any route.
func TestNeverPromptsOrTheQueue(t *testing.T) {
	db, path := fixture(t)
	addObs(t, db, "decision", "a decision", "why", day)
	addSummary(t, db, "a lesson", day)
	plan, err := Prepare(context.Background(), opts(path))
	if err != nil {
		t.Fatal(err)
	}
	if plan.Total() != 2 {
		t.Fatalf("%+v", plan)
	}
	for _, n := range plan.notes {
		text := n.Auto.Title + n.Auto.Body + strings.Join(n.Auto.Tags, " ")
		for _, c := range []string{promptCanary, queueCanary, requestCanary, rawCanary, firstCanary} {
			if strings.Contains(text, c) {
				t.Errorf("%s came over: %s", c, text)
			}
		}
	}
	for _, q := range []string{observationColumns, summaryColumns} {
		for _, col := range []string{"prompt_text", "tool_input", "tool_response", "user_prompt", "request", "text,", "concepts"} {
			if strings.Contains(q, col) {
				t.Errorf("reads %s", col)
			}
		}
	}
}

// Credentials and email addresses leave the note behind, and the rest is scrubbed.
func TestExclusionAndScrub(t *testing.T) {
	db, path := fixture(t)
	addObs(t, db, "decision", "with a key", "the key is AKIAABCDEFGHIJKLMNOP here", day)
	addObs(t, db, "decision", "with a password", "password = hunter2hunter2", day)
	addObs(t, db, "decision", "from someone", "ask jane.doe@example.com about it", day)
	addObs(t, db, "decision", "with a private part", "keep this <private>not this</private> and /home/someone/x", day)
	plan, err := Prepare(context.Background(), opts(path))
	if err != nil {
		t.Fatal(err)
	}
	if plan.Total() != 1 || plan.ExcludedCredential != 2 || plan.ExcludedEmail != 1 {
		t.Fatalf("%+v", plan)
	}
	body := plan.notes[0].Auto.Body
	if strings.Contains(body, "not this") || strings.Contains(body, "/home/someone") {
		t.Errorf("not scrubbed: %s", body)
	}
	if !strings.Contains(body, "~/x") || !strings.Contains(body, "~/app/main.go") {
		t.Errorf("scrubbed too much: %s", body)
	}
}

// The limits leave room in Fleet's quota: decisions first, then the newest.
func TestBudget(t *testing.T) {
	db, path := fixture(t)
	for i := 0; i < 5; i++ {
		addObs(t, db, "bugfix", fmt.Sprint("fix ", i), "fixed", time.Duration(i+1)*time.Hour)
	}
	addObs(t, db, "decision", "an old decision", "why", 400*day)
	o := opts(path)
	o.MaxNotes = 3
	plan, err := Prepare(context.Background(), o)
	if err != nil {
		t.Fatal(err)
	}
	if plan.Decisions != 1 || plan.Bugfixes != 2 || plan.OverBudget != 3 {
		t.Fatalf("%+v", plan)
	}
	if plan.notes[1].Auto.Title != "fix 0" {
		t.Errorf("not the newest first: %+v", plan.notes[1].Auto.Title)
	}
	o.MaxNotes, o.MaxBytes = 100, 1
	if plan, err = Prepare(context.Background(), o); err != nil || plan.Total() != 0 {
		t.Errorf("byte limit: %+v %v", plan, err)
	}
	if n, b := RoomFor(5000, 20000, 10<<20, 50<<20); n != 7500 || b != 20<<20 {
		t.Errorf("room %d %d", n, b)
	}
	if n, b := RoomFor(0, 0, 0, 0); n != 0 || b != 0 {
		t.Errorf("unreported limits %d %d", n, b)
	}
	if n, b := RoomFor(20000, 20000, 50<<20, 50<<20); n != 1 || b != 1 {
		t.Errorf("no room %d %d", n, b)
	}
}

// The database is opened so that it can't change, and it doesn't.
func TestReadOnly(t *testing.T) {
	db, path := fixture(t)
	addObs(t, db, "decision", "a decision", "why", day)
	db.Close()
	before := fileHash(t, path)
	if _, err := Prepare(context.Background(), opts(path)); err != nil {
		t.Fatal(err)
	}
	ro, err := open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer ro.Close()
	if _, err := ro.Exec(`DELETE FROM observations`); err == nil {
		t.Error("wrote through the read-only handle")
	}
	if fileHash(t, path) != before {
		t.Error("the database changed")
	}
}

// Forget-all empties the notes but not the record of what was imported, so importing
// again brings over only what never came over.
func TestForgetAllDoesNotReopenTheImport(t *testing.T) {
	ctx := context.Background()
	db, path := fixture(t)
	addObs(t, db, "decision", "a decision", "why", 400*day)
	plan, err := Prepare(ctx, opts(path))
	if err != nil {
		t.Fatal(err)
	}
	st, err := memory.Open(filepath.Join(t.TempDir(), memory.FileName))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	if res, err := Apply(ctx, st, plan); err != nil || res.Saved != 1 {
		t.Fatalf("%+v %v", res, err)
	}
	if _, err := st.ForgetAll(ctx, memory.MutateOptions{}); err != nil {
		t.Fatal(err)
	}
	addObs(t, db, "decision", "a later decision", "why", day)
	again, err := Prepare(ctx, opts(path))
	if err != nil {
		t.Fatal(err)
	}
	if err := again.LeaveOutImported(ctx, st); err != nil {
		t.Fatal(err)
	}
	if again.Before != 1 || again.Total() != 1 {
		t.Errorf("planned %d, before %d; want 1 new and 1 before", again.Total(), again.Before)
	}
	res, err := Apply(ctx, st, again)
	if err != nil || res.Saved != 1 {
		t.Fatalf("%+v %v", res, err)
	}
	entries, err := st.Recent(ctx, memory.AllProjects, 10)
	if err != nil || len(entries) != 1 {
		t.Fatalf("%+v %v", entries, err)
	}
}

// Imported notes are automatic and labelled, keep when they were written, and don't
// come over twice.
func TestApply(t *testing.T) {
	ctx := context.Background()
	db, path := fixture(t)
	addObs(t, db, "decision", "a decision", "why", 400*day)
	addSummary(t, db, "a lesson", day)
	plan, err := Prepare(ctx, opts(path))
	if err != nil {
		t.Fatal(err)
	}
	st, err := memory.Open(filepath.Join(t.TempDir(), memory.FileName))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	res, err := Apply(ctx, st, plan)
	if err != nil || res.Saved != 2 {
		t.Fatalf("%+v %v", res, err)
	}
	entries, err := st.Recent(ctx, memory.AllProjects, 10)
	if err != nil || len(entries) != 2 {
		t.Fatalf("%+v %v", entries, err)
	}
	for _, e := range entries {
		if !memory.IsAuto(e.Kind) {
			t.Errorf("%+v", e)
		}
	}
	if e := entries[1]; !e.CreatedAt.Before(now.Add(-300 * day)) {
		t.Errorf("the date was lost: %v", e.CreatedAt)
	}
	notes, _, err := st.Get(ctx, []string{entries[0].ID})
	if err != nil || len(notes) != 1 || !strings.Contains(strings.Join(notes[0].Tags, " "), ImportedTag) || !strings.Contains(strings.Join(notes[0].Tags, " "), memory.AutoTag) {
		t.Errorf("%+v %v", notes, err)
	}
	if res, err = Apply(ctx, st, plan); err != nil || res.Saved != 0 || res.Before != 2 {
		t.Errorf("second run: %+v %v", res, err)
	}
}

// A project found on disk gets its real key; one not found stays on this device.
func TestProjects(t *testing.T) {
	db, path := fixture(t)
	addObs(t, db, "decision", "in a project", "why", day)
	root := t.TempDir()
	repo := filepath.Join(root, "app")
	if err := os.MkdirAll(filepath.Join(repo, ".git"), 0o755); err != nil {
		t.Fatal(err)
	}
	cfg := "[remote \"origin\"]\n\turl = git@github.example:org/app.git\n"
	if err := os.WriteFile(filepath.Join(repo, ".git", "config"), []byte(cfg), 0o644); err != nil {
		t.Fatal(err)
	}
	o := opts(path)
	plan, err := Prepare(context.Background(), o)
	if err != nil {
		t.Fatal(err)
	}
	if pp := plan.Projects["app"]; pp.Notes != 1 || pp.Portable || memory.IsPortableKey(plan.notes[0].Auto.ProjectKey) {
		t.Errorf("no checkout: %+v %q", plan.Projects, plan.notes[0].Auto.ProjectKey)
	}
	o.Roots = []string{root}
	if plan, err = Prepare(context.Background(), o); err != nil {
		t.Fatal(err)
	}
	if pp := plan.Projects["app"]; !pp.Portable || !memory.IsPortableKey(plan.notes[0].Auto.ProjectKey) {
		t.Errorf("with a checkout: %+v", plan.Projects)
	}
}

// Counting reads counts.
func TestCountSince(t *testing.T) {
	db, path := fixture(t)
	addObs(t, db, "decision", "new", "why", day)
	addObs(t, db, "decision", "old", "why", 30*day)
	addSummary(t, db, "new", day)
	c, err := CountSince(context.Background(), path, now.AddDate(0, 0, -7))
	if err != nil || c.Observations != 1 || c.Summaries != 1 || c.WorkTokens != 49 {
		t.Errorf("%+v %v", c, err)
	}
}

// A database with another layout is refused, with nothing written.
func TestUnexpectedLayout(t *testing.T) {
	path := filepath.Join(t.TempDir(), "other.db")
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err := db.Exec(`CREATE TABLE observations (id INTEGER)`); err != nil {
		t.Fatal(err)
	}
	if _, err := Prepare(context.Background(), opts(path)); err == nil || !strings.Contains(err.Error(), "unexpected layout") {
		t.Errorf("%v", err)
	}
}

// One bad row costs that row: the dry run says so, and the rest come over.
func TestBadRowsDontStopAnImport(t *testing.T) {
	ctx := context.Background()
	for name, val := range map[string]any{"too long": strings.Repeat("x", 9<<10), "nul": "abc\x00def", "not text": []byte{'a', 0xff, 0xfe, 'b'}} {
		db, path := fixture(t)
		addObs(t, db, "decision", "newer", "fine", day)
		if _, err := db.Exec(`INSERT INTO observations(memory_session_id, project, text, type, title, subtitle, facts, narrative, concepts, files_read, files_modified, discovery_tokens, created_at, created_at_epoch)
VALUES ('m1','app','x','decision',?,'s','[]',?,'[]','[]','[]',1,'x',?)`, val, val, now.Add(-2*day).UnixMilli()); err != nil {
			t.Fatal(err)
		}
		addObs(t, db, "decision", "older", "fine", 3*day)
		plan, err := Prepare(ctx, opts(path))
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		st, err := memory.Open(filepath.Join(t.TempDir(), memory.FileName))
		if err != nil {
			t.Fatal(err)
		}
		res, err := Apply(ctx, st, plan)
		st.Close()
		if err != nil || res.Saved+res.Skipped != plan.Total() || res.Saved < 2 {
			t.Errorf("%s: plan %+v, result %+v, %v", name, plan, res, err)
		}
	}
	// A session with no id is fine.
	db, path := fixture(t)
	addObs(t, db, "decision", "ok", "fine", day)
	if _, err := db.Exec(`UPDATE observations SET memory_session_id = NULL`); err != nil {
		t.Fatal(err)
	}
	if plan, err := Prepare(ctx, opts(path)); err != nil || plan.Total() != 1 {
		t.Errorf("no session id: %+v %v", plan, err)
	}
}

// Every field goes through the scrub.
func TestEveryFieldIsScrubbed(t *testing.T) {
	db, path := fixture(t)
	if _, err := db.Exec(`INSERT INTO observations(memory_session_id, project, text, type, title, subtitle, facts, narrative, concepts, files_read, files_modified, discovery_tokens, created_at, created_at_epoch)
VALUES ('m1','app','x','decision','title /home/someone/a','sub /home/someone/b','["fact /home/someone/c"]','narr /home/someone/d <private>hide-me</private>','[]','["/home/someone/e"]','["/home/someone/f"]',1,'x',?)`, now.UnixMilli()); err != nil {
		t.Fatal(err)
	}
	plan, err := Prepare(context.Background(), opts(path))
	if err != nil || plan.Total() != 1 {
		t.Fatalf("%+v %v", plan, err)
	}
	all := plan.notes[0].Auto.Title + "\n" + plan.notes[0].Auto.Body
	if strings.Contains(all, "/home/someone") || strings.Contains(all, "hide-me") {
		t.Errorf("leak: %q", all)
	}
}

// Two repositories in folders of the same name: neither gets the notes.
func TestSameNamedFoldersAreNotGuessedBetween(t *testing.T) {
	db, path := fixture(t)
	addObs(t, db, "decision", "in a project", "why", day)
	mk := func(remote string) string {
		root := t.TempDir()
		repo := filepath.Join(root, "app")
		if err := os.MkdirAll(filepath.Join(repo, ".git"), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(repo, ".git", "config"), []byte("[remote \"origin\"]\n\turl = "+remote+"\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		return root
	}
	o := opts(path)
	o.Roots = []string{mk("git@github.example:one/app.git"), mk("git@github.example:two/app.git")}
	plan, err := Prepare(context.Background(), o)
	if err != nil {
		t.Fatal(err)
	}
	if pp := plan.Projects["app"]; !pp.Ambiguous || pp.Portable || memory.IsPortableKey(plan.notes[0].Auto.ProjectKey) {
		t.Errorf("%+v", plan.Projects)
	}
	o.Roots = o.Roots[:1]
	if plan, err = Prepare(context.Background(), o); err != nil {
		t.Fatal(err)
	}
	if pp := plan.Projects["app"]; pp.Ambiguous || !pp.Portable || pp.Checkout == "" {
		t.Errorf("one match: %+v", plan.Projects)
	}
}

// A dry run after an import says nothing comes over, even after a forget-all.
func TestPlanLeavesOutWhatWasImported(t *testing.T) {
	ctx := context.Background()
	db, path := fixture(t)
	addObs(t, db, "decision", "a decision", "why", day)
	addSummary(t, db, "a lesson", day)
	plan, err := Prepare(ctx, opts(path))
	if err != nil {
		t.Fatal(err)
	}
	st, err := memory.Open(filepath.Join(t.TempDir(), memory.FileName))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	if res, err := Apply(ctx, st, plan); err != nil || res.Saved != 2 {
		t.Fatalf("%+v %v", res, err)
	}
	if _, err := st.ForgetAll(ctx, memory.MutateOptions{SyncApproved: true}); err != nil {
		t.Fatal(err)
	}
	addObs(t, db, "decision", "a new decision", "why", day/2)
	again, err := Prepare(ctx, opts(path))
	if err != nil {
		t.Fatal(err)
	}
	if err := again.LeaveOutImported(ctx, st); err != nil {
		t.Fatal(err)
	}
	if again.Total() != 1 || again.Before != 2 || again.Decisions != 1 || again.Summaries != 0 || again.Projects["app"].Notes != 1 {
		t.Errorf("%+v", again)
	}
}

// ApplyEach reports each note, so a long import can show that it's moving.
func TestApplyEachReportsProgress(t *testing.T) {
	db, path := fixture(t)
	addObs(t, db, "decision", "a decision", "why", day)
	addObs(t, db, "bugfix", "a fix", "fixed", day)
	plan, err := Prepare(context.Background(), opts(path))
	if err != nil || plan.Total() != 2 {
		t.Fatalf("%+v %v", plan, err)
	}
	st, err := memory.Open(filepath.Join(t.TempDir(), memory.FileName))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	var last, total int
	if _, err := ApplyEach(context.Background(), st, plan, func(d, n int) { last, total = d, n }); err != nil {
		t.Fatal(err)
	}
	if total != plan.Total() || last != total {
		t.Fatalf("%d of %d, plan %d", last, total, plan.Total())
	}
}
