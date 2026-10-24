package app

import (
	"encoding/binary"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/kameas-ai/kenaz-plugin/fleetapi"
	"github.com/kameas-ai/kenaz-plugin/fleetapi/stub"
	"github.com/kameas-ai/kenaz-plugin/internal/consent"
	"github.com/kameas-ai/kenaz-plugin/internal/enroll"
	"github.com/kameas-ai/kenaz-plugin/internal/fixtures"
	"github.com/kameas-ai/kenaz-plugin/internal/project"
	"github.com/kameas-ai/kenaz-plugin/internal/spool"
)

// previousNodeID is the node id file of an installation enrolled by the
// previous release.
const previousNodeID = "01K7ZQ5V3W4X5Y6Z7A8B9C0D1E"

// writePreviousReleaseState lays out a state directory exactly as release
// 0.3.1 writes it: node_id, consent.json without queue_owner, and a spool
// holding one batch record (tag 'b') sealed under the env's spool key.
func writePreviousReleaseState(t *testing.T, dir string, ks *spool.Memory, admittedAt time.Time) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, "node_id"), []byte(previousNodeID+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	prev := map[string]any{
		"accepted_notice_version": map[string]string{"sub-1": consent.NoticeVersion("", project.TableVersion)},
		"admitted_at":             map[string]time.Time{"org-1": admittedAt},
	}
	b, _ := json.MarshalIndent(prev, "", "  ")
	if err := os.WriteFile(filepath.Join(dir, "consent.json"), b, 0o600); err != nil {
		t.Fatal(err)
	}
	key, err := spool.GetOrCreateKey(ks, "dev")
	if err != nil {
		t.Fatal(err)
	}
	sp, err := spool.Open(filepath.Join(dir, "spool"), key, spool.Options{})
	if err != nil {
		t.Fatal(err)
	}
	batch, _ := json.Marshal(fleetapi.ProjectedBatch{Signal: fleetapi.SignalLogs, ContentType: fleetapi.ContentTypeProtobuf, Body: []byte("queued-before-upgrade"), Records: 1})
	rec := make([]byte, 9, 9+len(batch))
	rec[0] = 'b'
	binary.BigEndian.PutUint64(rec[1:], uint64(fixtures.Base.UnixNano()))
	if err := sp.Append(telemetryLaneName, append(rec, batch...)); err != nil {
		t.Fatal(err)
	}
	sp.Close()
}

// Upgrading over the previous release's state keeps enrollment (node id)
// and consent (accepted notice, admission moment). Its queued batch carries
// no owner and may belong to another account (that release recorded no
// queue owner, and a login over an existing session did not purge the
// spool), so the ownerless queue is purged when it is first claimed,
// never sent: a one-time loss of unsent telemetry.
func TestUpgradeFromPreviousReleaseState(t *testing.T) {
	dir, ks := t.TempDir(), &spool.Memory{}
	admittedAt := fixtures.Base.Add(-2 * time.Hour).UTC()
	writePreviousReleaseState(t, dir, ks, admittedAt)

	f := &laneFleet{Provider: stub.New(), optedIn: true}
	l, _, _ := newTestLaneIn(t, f, dir, ks, nil)
	if !waitFor(t, func() bool { return l.dec.SignedIn() && l.sp.Empty() }) {
		t.Fatalf("queued batch not dropped after upgrade; status %+v", l.Status())
	}
	time.Sleep(100 * time.Millisecond)
	if n := f.attemptCount(); n != 0 {
		t.Fatalf("%d uploads of the previous release's ownerless batch", n)
	}
	if id, err := enroll.LoadOrCreateNodeID(dir, time.Now()); err != nil || id != previousNodeID {
		t.Fatalf("node id %q (%v): enrollment identity changed", id, err)
	}
	st := l.cm.State()
	if !st.AnyOn() || st.NoticeReconfirm || !st.AdmittedAt.Equal(admittedAt) {
		t.Fatalf("consent not preserved: on=%v reconfirm=%v admitted=%v", st.AnyOn(), st.NoticeReconfirm, st.AdmittedAt)
	}
	var after struct {
		Accepted   map[string]string    `json:"accepted_notice_version"`
		Admitted   map[string]time.Time `json:"admitted_at"`
		QueueOwner string               `json:"queue_owner"`
	}
	b, _ := os.ReadFile(filepath.Join(dir, "consent.json"))
	if err := json.Unmarshal(b, &after); err != nil {
		t.Fatal(err)
	}
	if after.Accepted["sub-1"] == "" || !after.Admitted["org-1"].Equal(admittedAt) || after.QueueOwner == "" {
		t.Fatalf("consent.json after upgrade: %s", b)
	}
}

// Held records ('r') queued by a build that recorded no queue owner may
// belong to another account. After an upgrade with someone else signed in,
// they are purged when the queue is first claimed, never re-gated and sent
// under the new account.
func TestUpgradeOwnerlessHeldRecordsPurged(t *testing.T) {
	dir, ks := t.TempDir(), &spool.Memory{}
	f := &laneFleet{Provider: stub.New(), optedIn: true}
	l1, rx, stop := newTestLaneIn(t, f, dir, ks, nil)
	if !waitFor(t, func() bool { return l1.dec.SignedIn() && admitted(l1) }) {
		t.Fatal("lane never admitted")
	}
	l1.cm.SetLockdown(fleetapi.LockdownState{Locked: true, Reason: "incident"})
	l1.dec.invalidate()
	postClaudeSession(t, l1, rx)
	if !waitFor(t, func() bool { return l1.held.Load() > 0 }) {
		t.Fatal("nothing was held")
	}
	stop()
	l1.sp.Close()

	// What an earlier build leaves: no queue owner. Another account (also
	// past the notice) signs in before the upgraded relay starts.
	p := filepath.Join(dir, "consent.json")
	var c map[string]any
	b, _ := os.ReadFile(p)
	if err := json.Unmarshal(b, &c); err != nil {
		t.Fatal(err)
	}
	delete(c, "queue_owner")
	if acc, ok := c["accepted_notice_version"].(map[string]any); ok {
		acc["sub-2"] = acc["sub-1"]
	}
	b, _ = json.Marshal(c)
	os.WriteFile(p, b, 0o600)
	f.set(func(f *laneFleet) { f.user = "sub-2" })

	l2, _, _ := newTestLaneIn(t, f, dir, ks, nil)
	if !waitFor(t, func() bool { return l2.dec.SignedIn() && l2.sp.Empty() }) {
		t.Fatalf("ownerless held records not purged; status %+v", l2.Status())
	}
	time.Sleep(300 * time.Millisecond)
	if n := f.postedCount(); n != 0 {
		t.Fatalf("posted %d batches of the previous account's held records under sub-2", n)
	}
}
