package memory

import (
	"context"
	"path/filepath"
	"strings"
	"testing"
)

func outboxRows(t *testing.T, s *Store) (ready, unapproved []string) {
	t.Helper()
	rows, err := s.db.Query(`SELECT state, CAST(payload AS TEXT) FROM outbox`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	for rows.Next() {
		var st, p string
		rows.Scan(&st, &p)
		switch st {
		case opReady, opInflight:
			ready = append(ready, p)
		case opUnapproved:
			unapproved = append(unapproved, p)
		}
	}
	return
}

// Security review regression (the reviewer's PoC): an edit made without
// approval must not inherit the approval of the save it replaces, for the
// body or the title, before or after the first upload.
func TestUnapprovedEditNeverInheritsApproval(t *testing.T) {
	ctx := context.Background()
	s, err := OpenProfile(filepath.Join(t.TempDir(), DirName, "profiles", "x", FileName), acct)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if _, err := s.EnableSync(ctx, "memory-sync/v1", MutateOptions{SyncApproved: true}); err != nil {
		t.Fatal(err)
	}
	r, err := s.SaveNote(ctx, NewNote{ProjectKey: GlobalProject, Title: "approved", Body: "approved body", Source: SourceCLI}, SaveOptions{RequestKey: "k1", SyncApproved: true})
	if err != nil || r.State != StatePending {
		t.Fatalf("save = %+v %v", r, err)
	}
	nb, nt := "AGENT TEXT NEVER APPROVED", "AGENT TITLE NEVER APPROVED"
	u, err := s.UpdateNote(ctx, r.ID, Update{Body: &nb, Title: &nt}, MutateOptions{SyncApproved: false})
	if err != nil || u.State != StateUnapproved {
		t.Fatalf("update = %+v %v", u, err)
	}
	ready, held := outboxRows(t, s)
	if len(ready) != 0 || len(held) != 1 || !strings.Contains(held[0], "AGENT TEXT") {
		t.Fatalf("ready=%v held=%v", ready, held)
	}
	// After the note is on Fleet: an unapproved body edit is a held
	// replacement, an unapproved title edit a held register write.
	r2, _ := s.SaveNote(ctx, NewNote{ProjectKey: GlobalProject, Title: "t2", Body: "b2", Source: SourceCLI}, SaveOptions{SyncApproved: true})
	s.db.Exec(`DELETE FROM outbox WHERE record_id = ?`, r2.ID)
	s.db.Exec(`UPDATE records SET sync_state = 'synced', server_seq = '7' WHERE id = ?`, r2.ID)
	nb2, nt2 := "AGENT BODY 2", "AGENT TITLE 2"
	if _, err := s.UpdateNote(ctx, r2.ID, Update{Title: &nt2}, MutateOptions{}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.UpdateNote(ctx, r2.ID, Update{Body: &nb2}, MutateOptions{}); err != nil {
		t.Fatal(err)
	}
	ready, _ = outboxRows(t, s)
	for _, p := range ready {
		if strings.Contains(p, "AGENT") {
			t.Fatalf("unapproved content is ready to send: %s", p)
		}
	}
}

// Turning sync on uploads exactly the notes, with exactly the content,
// that the approval listed.
func TestEnableQueuesOnlyListedContent(t *testing.T) {
	ctx := context.Background()
	s, err := OpenProfile(filepath.Join(t.TempDir(), DirName, "profiles", "x", FileName), acct)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	a, _ := s.Save(ctx, NewNote{ProjectKey: GlobalProject, Title: "a", Body: "listed", Source: SourceCLI})
	b, _ := s.Save(ctx, NewNote{ProjectKey: GlobalProject, Title: "b", Body: "changed after listing", Source: SourceCLI})
	listed, _ := s.LocalSyncCandidates(ctx)
	nb := "EDITED AFTER THE APPROVAL PAGE"
	s.UpdateNote(ctx, b, Update{Body: &nb}, MutateOptions{})
	c, _ := s.Save(ctx, NewNote{ProjectKey: GlobalProject, Title: "c", Body: "saved after listing", Source: SourceCLI})
	res, err := s.EnableSync(ctx, "memory-sync/v1", MutateOptions{SyncApproved: true, Records: listed})
	if err != nil || res.Queued != 1 {
		t.Fatalf("enable = %+v %v", res, err)
	}
	for id, want := range map[string]string{a: StatePending, b: StateLocal, c: StateLocal} {
		n, _, _ := s.Get(ctx, []string{id})
		if n[0].SyncState != want {
			t.Fatalf("%s = %s, want %s", n[0].Title, n[0].SyncState, want)
		}
	}
}

// Without approval, an account deletion only hides the note; revert
// shows it again and an approved sync_approve deletes it.
func TestOfflineForgetOnlyHides(t *testing.T) {
	ctx := context.Background()
	e := newSvc(t, true)
	e.syncOn(t)
	e.relay.avail = false
	out, _ := e.svc.Save(ctx, note(GlobalProject, "t", "keep me"))
	f, err := e.svc.Forget(ctx, out.ID)
	if err != nil || !f.Hidden || f.Remote != "unapproved" || f.Status != OutcomeDone {
		t.Fatalf("forget = %+v %v", f, err)
	}
	if n, _ := e.svc.Count(ctx); n != 0 {
		t.Fatal("hidden note still retrievable")
	}
	st := e.store(t, acct)
	if err := st.Revert(ctx, out.ID); err != nil {
		t.Fatal(err)
	}
	if n, _ := e.svc.Count(ctx); n != 1 {
		t.Fatal("revert did not restore the note")
	}
	e.svc.Forget(ctx, out.ID)
	e.relay.avail = true
	pend, err := e.svc.ApproveHeld(ctx)
	if err != nil || pend.Status != OutcomeApprovalPending {
		t.Fatalf("approve = %+v %v", pend, err)
	}
	if q := e.relay.reqs[pend.RequestID]; !strings.Contains(string(q.Payload), OpDeleteNote) {
		t.Fatalf("payload = %s", q.Payload)
	}
	if _, err := e.relay.approve(t, ctx, pend.RequestID); err != nil {
		t.Fatal(err)
	}
	var n int
	st.db.QueryRow(`SELECT count(*) FROM records WHERE id = ?`, out.ID).Scan(&n)
	if n != 0 {
		t.Fatal("approved deletion left the note")
	}
}

// Adopt moves only notes whose content still matches the approval.
func TestAdoptBoundToContent(t *testing.T) {
	ctx := context.Background()
	e := newSvc(t, true)
	e.active = Local
	d, _ := e.svc.Save(ctx, note(GlobalProject, "d", "original"))
	e.active = acct
	pend, err := e.svc.Adopt(ctx, []string{d.ID})
	if err != nil {
		t.Fatal(err)
	}
	e.active = Local
	nb := "changed after the approval page"
	e.svc.Update(ctx, d.ID, Update{Body: &nb})
	e.active = acct
	b, err := e.relay.approve(t, ctx, pend.RequestID)
	if err != nil || !strings.Contains(string(b), "Moved 0 notes") {
		t.Fatalf("adopt = %s %v", b, err)
	}
	if n, _ := e.svc.Count(ctx); n != 1 {
		t.Fatalf("count = %d (the edited note must stay in the notebook only)", n)
	}
}
