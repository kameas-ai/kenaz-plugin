package session

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/kameas-ai/kenaz-plugin/internal/canon"
)

type clock struct{ t time.Time }

func (c *clock) now() time.Time { return c.t }

var t0 = time.Date(2026, 10, 14, 9, 0, 0, 0, time.UTC)

func newAgg(t *testing.T, c *clock) *Aggregator {
	t.Helper()
	return New(Config{Key: []byte("k"), Now: c.now})
}

func count(outs []Out, k OutKind) int {
	n := 0
	for _, o := range outs {
		if o.Kind == k {
			n++
		}
	}
	return n
}

func ended(outs []Out) Out {
	for _, o := range outs {
		if o.Kind == OutEnded {
			return o
		}
	}
	return Out{}
}

func ev(kind canon.Kind, client canon.ClientKind, id string, at time.Duration) canon.Event {
	return canon.Event{Kind: kind, Client: client, SessionID: id, Time: t0.Add(at)}
}

func TestHashIsKeyedAndStable(t *testing.T) {
	a := New(Config{Key: []byte("k1")})
	b := New(Config{Key: []byte("k2")})
	h := a.Hash(canon.Codex, "abc")
	if len(h) != 16 || strings.Trim(h, "0123456789abcdef") != "" {
		t.Fatalf("bad hash %q", h)
	}
	if h != a.Hash(canon.Codex, "abc") || h == b.Hash(canon.Codex, "abc") || h == a.Hash(canon.ClaudeCode, "abc") {
		t.Fatal("hash must be stable per key and differ by key and client")
	}
}

func TestSessionEndHook(t *testing.T) {
	c := &clock{t0}
	a := newAgg(t, c)
	mc := ev(canon.KindModelCall, canon.ClaudeCode, "s1", 2*time.Second)
	mc.InputTokens, mc.OutputTokens, mc.CostUSD, mc.CostKnown = 10, 5, 0.5, true
	outs := a.Ingest([]canon.Event{ev(canon.KindSessionSeen, canon.ClaudeCode, "s1", 0), mc})
	if count(outs, OutStarted) != 1 || count(outs, OutModelCall) != 1 {
		t.Fatalf("outs = %+v", outs)
	}
	outs = a.End(canon.ClaudeCode, "s1")
	e := ended(outs)
	if e.Kind != OutEnded || e.Duration != 2*time.Second || e.TokenIn != 10 || e.CostUSD != 0.5 || !e.CostKnown {
		t.Fatalf("ended = %+v", e)
	}
	if again := a.End(canon.ClaudeCode, "s1"); len(again) != 0 {
		t.Fatal("ended twice")
	}
	// Late records after the end do not re-open the session.
	late := a.Ingest([]canon.Event{ev(canon.KindToolCall, canon.ClaudeCode, "s1", 3*time.Second)})
	if count(late, OutStarted) != 0 || count(late, OutToolCall) != 1 {
		t.Fatalf("late = %+v", late)
	}
}

func TestIdleClose(t *testing.T) {
	c := &clock{t0}
	a := newAgg(t, c)
	a.Ingest([]canon.Event{ev(canon.KindSessionSeen, canon.Codex, "s", 0)})
	c.t = t0.Add(29 * time.Minute)
	if outs := a.Tick(); len(outs) != 0 {
		t.Fatal("closed early")
	}
	c.t = t0.Add(30 * time.Minute)
	e := ended(a.Tick())
	if e.Kind != OutEnded || e.Duration != time.Millisecond {
		t.Fatalf("idle close = %+v (single-instant session must floor to 1ms)", e)
	}
	// After the tombstone expires a reused id is a new conversation.
	c.t = t0.Add(61 * time.Minute)
	a.Tick()
	if outs := a.Ingest([]canon.Event{ev(canon.KindSessionSeen, canon.Codex, "s", time.Hour)}); count(outs, OutStarted) != 1 {
		t.Fatal("tombstone did not expire")
	}
}

// The relay never estimates a cost: a call without a harness-reported cost
// is sent as cost unknown, whatever the model.
func TestUnreportedCostUnknown(t *testing.T) {
	a := newAgg(t, &clock{t0})
	mc := ev(canon.KindModelCall, canon.Codex, "s", time.Second)
	mc.Model, mc.InputTokens = "gpt-5", 100
	a.Ingest([]canon.Event{mc})
	e := ended(a.End(canon.Codex, "s"))
	if e.CostKnown || e.CostUSD != 0 {
		t.Fatalf("unreported cost priced: %+v", e)
	}
}

func TestRestartKeepsSessionWithoutSecondStart(t *testing.T) {
	c := &clock{t0}
	a := newAgg(t, c)
	mc := ev(canon.KindModelCall, canon.ClaudeCode, "native-secret-id", time.Second)
	mc.InputTokens, mc.CostUSD, mc.CostKnown = 7, 0.1, true
	a.Ingest([]canon.Event{mc})
	snap := a.Snapshot()
	raw, _ := json.Marshal(snap)
	if strings.Contains(string(raw), "native-secret-id") {
		t.Fatal("native id persisted")
	}
	var restored []State
	if err := json.Unmarshal(raw, &restored); err != nil {
		t.Fatal(err)
	}
	b := newAgg(t, c)
	b.Restore(restored)
	mc2 := ev(canon.KindModelCall, canon.ClaudeCode, "native-secret-id", 3*time.Second)
	mc2.InputTokens, mc2.CostUSD, mc2.CostKnown = 3, 0.2, true
	outs := b.Ingest([]canon.Event{mc2})
	if count(outs, OutStarted) != 0 {
		t.Fatal("restart emitted a second start")
	}
	e := ended(b.End(canon.ClaudeCode, "native-secret-id"))
	if e.TokenIn != 10 || e.Duration != 2*time.Second {
		t.Fatalf("restored totals = %+v", e)
	}
}

func TestIdleClamp(t *testing.T) {
	if New(Config{Idle: time.Second}).cfg.Idle != 5*time.Minute || New(Config{Idle: 24 * time.Hour}).cfg.Idle != 240*time.Minute {
		t.Fatal("idle not clamped to 5..240 min")
	}
}

func TestEndLocalClosesByLocalHashOnly(t *testing.T) {
	now := time.Date(2026, 10, 14, 12, 0, 0, 0, time.UTC)
	a := New(Config{Key: []byte("k"), Now: func() time.Time { return now }})
	a.Ingest([]canon.Event{{Client: canon.ClaudeCode, SessionID: "native-1", Time: now}})
	if out := a.EndLocal(canon.Codex, canon.LocalSessionHash("native-1")); out != nil {
		t.Fatal("client mismatch must not close the session")
	}
	if out := a.EndLocal(canon.ClaudeCode, canon.LocalSessionHash("nope")); out != nil {
		t.Fatal("unknown hash must be ignored")
	}
	if out := a.EndLocal(canon.ClaudeCode, canon.LocalSessionHash("native-1")); len(out) == 0 {
		t.Fatal("expected session close output")
	}
	if len(a.local) != 0 {
		t.Fatal("local index not cleaned on close")
	}
}
