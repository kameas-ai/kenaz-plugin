package memory

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"testing"
	"time"
)

func autoStore(t *testing.T) *Store {
	t.Helper()
	s, err := Open(filepath.Join(t.TempDir(), "memory.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

func autoNote(i int) AutoNote {
	return AutoNote{ProjectKey: "p1", Title: fmt.Sprintf("auto %d", i), Body: "body", Tags: []string{AutoTag, "session:abc"}, Source: SourceClaudeCode, Kind: KindObservation}
}

func TestSaveAutoIsIdempotentAndChecked(t *testing.T) {
	s := autoStore(t)
	ctx := context.Background()
	n := autoNote(1)
	n.RequestKey = "k"
	id, dup, err := s.SaveAuto(ctx, n)
	if err != nil || dup {
		t.Fatal(err, dup)
	}
	id2, dup, err := s.SaveAuto(ctx, n)
	if err != nil || !dup || id2 != id {
		t.Errorf("second save: %s %v %v", id2, dup, err)
	}
	if c, _ := s.Count(ctx); c != 1 {
		t.Errorf("count %d", c)
	}
	bad := autoNote(2)
	bad.Kind = "raw"
	if _, _, err := s.SaveAuto(ctx, bad); !errors.Is(err, ErrInvalidInput) {
		t.Errorf("unknown kind: %v", err)
	}
	secret := autoNote(3)
	secret.Body = "AKIA" + "ABCDEFGHIJKLMNOP"
	if _, _, err := s.SaveAuto(ctx, secret); !errors.Is(err, ErrLooksSecret) {
		t.Errorf("secret: %v", err)
	}
}

// Capture can never make a deliberate save fail, and deliberate notes are
// never pruned.
func TestAutoNotesHaveTheirOwnLimits(t *testing.T) {
	s := autoStore(t)
	ctx := context.Background()
	if _, err := s.Save(ctx, NewNote{ProjectKey: "p1", Title: "keep me", Body: "deliberate", Source: SourceCLI}); err != nil {
		t.Fatal(err)
	}
	old := MaxAutoNotes
	MaxAutoNotes = 2500
	defer func() { MaxAutoNotes = old }()
	now := time.Now().UnixMilli()
	if _, err := s.db.ExecContext(ctx, `WITH RECURSIVE n(i) AS (SELECT 1 UNION ALL SELECT i+1 FROM n WHERE i < ?)
INSERT INTO records(id, content, content_hash, title, kind, scope, project_key, source, created_at, updated_at)
SELECT 'auto-'||i, 'b', 'h', 't', 'observation', 'project', 'p1', 'codex', ?+i, ?+i FROM n`, MaxAutoNotes, now, now); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Save(ctx, NewNote{ProjectKey: "p1", Title: "still works", Body: "deliberate", Source: SourceCLI}); err != nil {
		t.Fatalf("a deliberate save failed: %v", err)
	}
	if _, _, err := s.SaveAuto(ctx, autoNote(1)); err != nil {
		t.Fatal(err)
	}
	var auto, deliberate, oldest int
	s.db.QueryRowContext(ctx, `SELECT count(*) FROM records WHERE kind = 'observation'`).Scan(&auto)
	s.db.QueryRowContext(ctx, `SELECT count(*) FROM records WHERE `+notAuto).Scan(&deliberate)
	s.db.QueryRowContext(ctx, `SELECT count(*) FROM records WHERE id = 'auto-1'`).Scan(&oldest)
	if int64(auto) != MaxAutoNotes || deliberate != 2 || oldest != 0 {
		t.Errorf("auto %d, deliberate %d, oldest still there %d", auto, deliberate, oldest)
	}
}

// Automatic notes stay on the device for now, are portable, and go with
// forget-all.
func TestAutoNotesLifecycle(t *testing.T) {
	ctx := context.Background()
	p := Profile{Env: "dev", OrgID: "o", UserID: "u"}
	s, err := OpenProfile(filepath.Join(t.TempDir(), "memory.db"), p)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if _, _, err := s.SaveAuto(ctx, autoNote(1)); err != nil {
		t.Fatal(err)
	}
	if _, err := s.EnableSync(ctx, "memory-sync/v3", MutateOptions{}); err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.SaveAuto(ctx, autoNote(2)); err != nil {
		t.Fatal(err)
	}
	cand, err := s.LocalSyncCandidates(ctx)
	if err != nil || len(cand) != 0 {
		t.Errorf("sync candidates: %d %v", len(cand), err)
	}
	var queued int
	s.db.QueryRowContext(ctx, `SELECT count(*) FROM outbox`).Scan(&queued)
	if queued != 0 {
		t.Errorf("%d notes queued for Fleet", queued)
	}
	var buf bytes.Buffer
	if n, err := s.Export(ctx, &buf, "account"); err != nil || n != 2 {
		t.Errorf("export: %d %v", n, err)
	}
	var rec ExportRecord
	if err := json.Unmarshal(bytes.SplitN(buf.Bytes(), []byte("\n"), 2)[0], &rec); err != nil || rec.Kind != KindObservation {
		t.Errorf("export record: %+v %v", rec, err)
	}
	notes, err := s.SessionObservations(ctx, "session:abc", 10)
	if err != nil || len(notes) != 2 {
		t.Errorf("session observations: %d %v", len(notes), err)
	}
	res, err := s.ForgetAll(ctx, MutateOptions{})
	if err != nil || res.Deleted != 2 {
		t.Errorf("forget-all: %+v %v", res, err)
	}
}

// A turn's observations are those since the session's last summary, so a
// summary never repeats an earlier turn.
func TestTurnObservations(t *testing.T) {
	s := autoStore(t)
	ctx := context.Background()
	save := func(n AutoNote) {
		t.Helper()
		if _, _, err := s.SaveAuto(ctx, n); err != nil {
			t.Fatal(err)
		}
	}
	save(autoNote(1))
	sum := autoNote(2)
	sum.Kind = KindSessionSummary
	save(sum)
	save(autoNote(3))
	save(autoNote(4))
	other := autoNote(5)
	other.Tags = []string{AutoTag, "session:other"}
	save(other)
	notes, err := s.TurnObservations(ctx, "session:abc", 10)
	if err != nil || len(notes) != 2 || notes[0].Title != "auto 3" {
		t.Fatalf("%d notes, %v: %+v", len(notes), err, notes)
	}
}

// A refusal that waits on someone else never turns into a rejection, however long it
// lasts. Any other retry still runs out.
func TestWaitingRefusalsDoNotRunOut(t *testing.T) {
	ctx := context.Background()
	st, err := OpenProfile(filepath.Join(t.TempDir(), "m.db"), Profile{Env: "dev", OrgID: "o", UserID: "u"})
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	if _, err := st.EnableSync(ctx, "memory-sync/v3", MutateOptions{SyncApproved: true}); err != nil {
		t.Fatal(err)
	}
	save := func(title string) string {
		r, err := st.SaveNote(ctx, NewNote{ProjectKey: GlobalProject, Title: title, Body: title, Source: SourceCLI}, SaveOptions{SyncApproved: true})
		if err != nil {
			t.Fatal(err)
		}
		return r.ID
	}
	yes, no := true, false
	check := func(code string, retry *bool, wantState string) {
		t.Helper()
		id := save(code)
		l, err := st.AcquireLease(ctx, "test", time.Minute)
		if err != nil {
			t.Fatal(err)
		}
		defer st.ReleaseLease(ctx, l)
		batch, err := st.LeasePushBatch(ctx, l)
		if err != nil {
			t.Fatal(err)
		}
		if len(batch.Ops) != 1 || batch.Ops[0].RecordID != id {
			t.Fatalf("%s: batch %+v", code, batch.Ops)
		}
		batch.Ops[0].Attempt = MaxOpAttempts + 50
		results := []PushResult{{Status: "rejected", Code: code, Retry: retry}}
		if _, err := st.ApplyPushResults(ctx, l, batch.Ops, results); err != nil {
			t.Fatal(err)
		}
		var state string
		if err := st.db.QueryRowContext(ctx, `SELECT sync_state FROM records WHERE id = ?`, id).Scan(&state); err != nil {
			t.Fatal(err)
		}
		if state != wantState {
			t.Errorf("%s: note is %s, want %s", code, state, wantState)
		}
	}
	check("capture_off", &yes, StatePending)
	check("capture_off", &no, StatePending) // an older server
	check("capture_off", nil, StatePending)
	check("consent_upgrade_required", &yes, StatePending)
	check("internal_error", &yes, StateRejected)
}

// A note written from what happened before a forget-all isn't saved, whenever the model
// finishes. One from after it is.
func TestAutomaticNoteFromBeforeAForgetAll(t *testing.T) {
	ctx := context.Background()
	st, err := OpenProfile(filepath.Join(t.TempDir(), FileName), acct)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	before := time.Now().Add(-time.Minute)
	if _, err := st.ForgetAll(ctx, MutateOptions{SyncApproved: true}); err != nil {
		t.Fatal(err)
	}
	n := AutoNote{ProjectKey: GlobalProject, Title: "t", Body: "b", Source: SourceCLI, Kind: KindObservation, EventsFrom: before}
	if _, _, err := st.SaveAuto(ctx, n); !errors.Is(err, ErrErasedSince) {
		t.Errorf("%v", err)
	}
	if c, _ := st.Count(ctx); c != 0 {
		t.Errorf("%d notes", c)
	}
	n.EventsFrom = time.Now().Add(time.Second)
	if _, _, err := st.SaveAuto(ctx, n); err != nil {
		t.Error(err)
	}
}
