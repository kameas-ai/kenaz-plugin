package session

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/kameas-ai/kenaz-plugin/internal/canon"
)

func TestSessionEndEventClosesAndNeverOpens(t *testing.T) {
	c := &clock{t0}
	a := newAgg(t, c)
	end := ev(canon.KindSessionEnd, canon.OpenCode, "s1", time.Minute)
	if out := a.Ingest([]canon.Event{end}); len(out) != 0 {
		t.Fatalf("end for an unknown session must emit nothing, got %v", out)
	}
	out := a.Ingest([]canon.Event{ev(canon.KindSessionSeen, canon.OpenCode, "s1", 0)})
	if count(out, OutStarted) != 1 {
		t.Fatalf("want started, got %v", out)
	}
	out = a.Ingest([]canon.Event{ev(canon.KindSessionEnd, canon.OpenCode, "s1", 2*time.Minute)})
	if count(out, OutEnded) != 1 || ended(out).Duration != 2*time.Minute {
		t.Fatalf("want one ended with 2m duration, got %+v", out)
	}
	// Late records after the end do not reopen the conversation.
	out = a.Ingest([]canon.Event{ev(canon.KindSessionSeen, canon.OpenCode, "s1", 3*time.Minute)})
	if count(out, OutStarted) != 0 {
		t.Fatalf("tombstoned session reopened: %v", out)
	}
}

func TestDedupKeySurvivesRetryAndRestart(t *testing.T) {
	c := &clock{t0}
	a := newAgg(t, c)
	mc := ev(canon.KindModelCall, canon.OpenCode, "s1", time.Second)
	mc.InputTokens, mc.OutputTokens, mc.DedupKey = 10, 5, "m:msg_1"
	tool := ev(canon.KindToolCall, canon.OpenCode, "s1", time.Second)
	tool.ToolName, tool.Success, tool.DedupKey = "read", true, "t:call_1"

	out := a.Ingest([]canon.Event{mc, tool})
	if count(out, OutModelCall) != 1 || count(out, OutToolCall) != 1 {
		t.Fatalf("first delivery: %v", out)
	}
	out = a.Ingest([]canon.Event{mc, tool})
	if count(out, OutModelCall) != 0 || count(out, OutToolCall) != 0 {
		t.Fatalf("retried delivery counted again: %v", out)
	}

	// The dedup set is persisted (hashed) across a relay restart.
	snap, _ := json.Marshal(a.Snapshot())
	if string(snap) == "" || strings.Contains(string(snap), "msg_1") || strings.Contains(string(snap), "call_1") {
		t.Fatalf("snapshot leaks native ids: %s", snap)
	}
	var st []State
	json.Unmarshal(snap, &st)
	b := newAgg(t, c)
	b.Restore(st)
	out = b.Ingest([]canon.Event{mc})
	if count(out, OutModelCall) != 0 {
		t.Fatalf("restored aggregator counted a duplicate: %v", out)
	}
	e := ended(b.CloseAll())
	if e.TokenIn != 10 || e.TokenOut != 5 {
		t.Fatalf("session totals double counted: %+v", e)
	}
	// OpenCode reported no cost for this call: the total stays unknown.
	if e.CostKnown {
		t.Fatalf("cost must stay unknown: %+v", e)
	}
}

func TestDedupSetIsBounded(t *testing.T) {
	st := &State{}
	for i := 0; i < maxSeen+10; i++ {
		st.markSeen(string(rune('a'+i%26)) + time.Duration(i).String())
	}
	if len(st.Seen) != maxSeen {
		t.Fatalf("seen set not bounded: %d", len(st.Seen))
	}
}
