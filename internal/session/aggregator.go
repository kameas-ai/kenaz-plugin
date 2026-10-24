// Package session aggregates canonical events into conversations: it emits conversation_started once on first sight, passes
// per-call events through, and emits conversation_ended exactly once on a
// SessionEnd signal, an idle timeout or an explicit close.
//
// Native session ids never reach state: every event is keyed by
// HMAC(k_install, client || ":" || native id) from first touch.
package session

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"sort"
	"sync"
	"time"

	"github.com/kameas-ai/kenaz-plugin/internal/canon"
)

// OutKind discriminates Out.
type OutKind int

const (
	OutStarted OutKind = iota + 1
	OutEnded
	OutModelCall
	OutToolCall
	OutError
)

// Out is one aggregator output, ready for projection. It carries the
// session hash, never the native id.
type Out struct {
	Kind     OutKind
	Client   canon.ClientKind
	Hash     string
	Time     time.Time
	Provider string

	// OutEnded
	Duration  time.Duration
	TokenIn   int64 // input + cache read + cache creation (harness parity)
	TokenOut  int64
	CostUSD   float64
	CostKnown bool

	// OutModelCall
	Model                                               string
	InputTokens, OutputTokens, CacheRead, CacheCreation int64
	Reasoning                                           int64
	CallCostUSD                                         float64
	CallCostKnown                                       bool
	CallDuration, TTFT                                  time.Duration
	HasTTFT                                             bool

	// OutToolCall
	ToolName  string
	ToolIsMCP bool
	Success   bool
	Latency   time.Duration

	// OutError
	StatusCode int
	Timeout    bool
}

// Config configures an Aggregator.
type Config struct {
	// Key is the per-install HMAC key (32 bytes, keychain-held, rotated on
	// logout).
	Key []byte
	// Idle closes a session after this long without records (default 30m,
	// clamped to 5m..240m).
	Idle time.Duration
	// Tombstone keeps ended sessions from re-opening on late records.
	Tombstone time.Duration
	Now       func() time.Time
}

// DefaultIdle is the default idle timeout.
const DefaultIdle = 30 * time.Minute

// Default providers per client when the harness named none.
var defaultProvider = map[canon.ClientKind]string{
	canon.ClaudeCode: "anthropic",
	canon.Codex:      "openai",
	// OpenCode is multi-provider: the provider comes from each model call.
	canon.OpenCode: "",
}

// maxSeen bounds the per-session dedup set (OpenCode call ids).
const maxSeen = 1024

// State is the persisted per-session state (JSON-safe; no native ids).
type State struct {
	Client      canon.ClientKind `json:"client"`
	Hash        string           `json:"hash"`
	Provider    string           `json:"provider"`
	First       time.Time        `json:"first"`
	Last        time.Time        `json:"last"`
	TokenIn     int64            `json:"token_in"`
	TokenOut    int64            `json:"token_out"`
	Cost        float64          `json:"cost"`
	CostMissing bool             `json:"cost_missing"`
	TurnCost    float64          `json:"turn_cost"`
	HasTurnCost bool             `json:"has_turn_cost"`
	SawSSEUsage bool             `json:"saw_sse_usage"`
	// Fallback holds Codex turn-span usage until close; it is used only if
	// no sse_event usage arrived for the session.
	Fallback []Out `json:"fallback,omitempty"`
	// Seen holds keyed hashes of the call ids already counted for this
	// session (canon.Event.DedupKey), oldest first, at most maxSeen.
	Seen []string `json:"seen,omitempty"`
}

// Aggregator is safe for concurrent use.
type Aggregator struct {
	mu    sync.Mutex
	cfg   Config
	live  map[string]*State
	ended map[string]time.Time
	// local maps canon.LocalSessionHash(native id) → keyed hash for live
	// sessions, so a SessionEnd hook can close a session without sending
	// the native id to the daemon. In memory only.
	local map[string]string
}

// New returns an Aggregator.
func New(cfg Config) *Aggregator {
	if cfg.Idle == 0 {
		cfg.Idle = DefaultIdle
	}
	if cfg.Idle < 5*time.Minute {
		cfg.Idle = 5 * time.Minute
	}
	if cfg.Idle > 240*time.Minute {
		cfg.Idle = 240 * time.Minute
	}
	if cfg.Tombstone == 0 {
		cfg.Tombstone = 30 * time.Minute
	}
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	return &Aggregator{cfg: cfg, live: map[string]*State{}, ended: map[string]time.Time{}, local: map[string]string{}}
}

// Hash returns the 16-hex conversation id for a native session id.
func (a *Aggregator) Hash(client canon.ClientKind, nativeID string) string {
	m := hmac.New(sha256.New, a.cfg.Key)
	m.Write([]byte(string(client) + ":" + nativeID))
	return hex.EncodeToString(m.Sum(nil))[:16]
}

// Ingest folds events in and returns outputs in event order.
func (a *Aggregator) Ingest(events []canon.Event) []Out {
	a.mu.Lock()
	defer a.mu.Unlock()
	var out []Out
	for _, e := range events {
		if !e.Client.Valid() || e.SessionID == "" {
			continue
		}
		h := a.Hash(e.Client, e.SessionID)
		if e.Kind == canon.KindSessionEnd {
			// An end signal closes a live session and never opens one.
			if st := a.live[h]; st != nil && e.Time.After(st.Last) {
				st.Last = e.Time
			}
			out = append(out, a.close(h)...)
			continue
		}
		if _, ok := a.live[h]; !ok {
			if _, tomb := a.ended[h]; !tomb {
				a.local[canon.LocalSessionHash(e.SessionID)] = h
			}
		}
		out = append(out, a.ingestOne(h, e)...)
	}
	return out
}

func (a *Aggregator) ingestOne(h string, e canon.Event) []Out {
	var out []Out
	st := a.live[h]
	_, tomb := a.ended[h]
	if st == nil && !tomb {
		st = &State{Client: e.Client, Hash: h, Provider: defaultProvider[e.Client], First: e.Time, Last: e.Time}
		if e.Provider != "" {
			st.Provider = e.Provider
		}
		a.live[h] = st
		out = append(out, Out{Kind: OutStarted, Client: e.Client, Hash: h, Time: e.Time, Provider: st.Provider})
	}
	provider := defaultProvider[e.Client]
	if st != nil {
		if e.Time.After(st.Last) {
			st.Last = e.Time
		}
		if e.Time.Before(st.First) {
			st.First = e.Time
		}
		if e.Kind == canon.KindSessionSeen && e.Provider != "" {
			st.Provider = e.Provider
		}
		provider = st.Provider
	}

	if e.DedupKey != "" && st != nil && (e.Kind == canon.KindModelCall || e.Kind == canon.KindToolCall) {
		if !st.markSeen(a.dedupHash(h, e.DedupKey)) {
			return out // a retried export of a call already counted
		}
	}

	switch e.Kind {
	case canon.KindModelCall:
		mc := a.modelCall(h, provider, e)
		if e.FromTurnSpan {
			if st != nil && !st.SawSSEUsage {
				st.Fallback = append(st.Fallback, mc)
			}
			return out
		}
		if st != nil {
			if !st.SawSSEUsage && len(st.Fallback) > 0 {
				st.Fallback = nil // real per-call usage supersedes the fallback
			}
			st.SawSSEUsage = st.SawSSEUsage || e.Client == canon.Codex
			st.add(mc)
		}
		out = append(out, mc)
	case canon.KindCostDelta:
		if st != nil {
			st.TurnCost += e.CostUSD
			st.HasTurnCost = true
		}
	case canon.KindToolCall:
		out = append(out, Out{Kind: OutToolCall, Client: e.Client, Hash: h, Time: e.Time,
			ToolName: e.ToolName, ToolIsMCP: e.ToolIsMCP, Success: e.Success, Latency: e.Duration})
	case canon.KindAPIError:
		out = append(out, Out{Kind: OutError, Client: e.Client, Hash: h, Time: e.Time,
			StatusCode: e.StatusCode, Timeout: e.Timeout})
	}
	return out
}

// dedupHash keys a producer call id to the session, so the persisted
// dedup set holds no native ids.
func (a *Aggregator) dedupHash(h, key string) string {
	m := hmac.New(sha256.New, a.cfg.Key)
	m.Write([]byte("dedup:" + h + ":" + key))
	return hex.EncodeToString(m.Sum(nil))[:16]
}

// markSeen records k and reports whether it was new.
func (st *State) markSeen(k string) bool {
	for _, s := range st.Seen {
		if s == k {
			return false
		}
	}
	st.Seen = append(st.Seen, k)
	if len(st.Seen) > maxSeen {
		st.Seen = append([]string(nil), st.Seen[len(st.Seen)-maxSeen:]...)
	}
	return true
}

func (a *Aggregator) modelCall(h, provider string, e canon.Event) Out {
	o := Out{Kind: OutModelCall, Client: e.Client, Hash: h, Time: e.Time, Provider: provider,
		Model: e.Model, InputTokens: e.InputTokens, OutputTokens: e.OutputTokens,
		CacheRead: e.CacheRead, CacheCreation: e.CacheCreation, Reasoning: e.Reasoning,
		CallDuration: e.Duration, TTFT: e.TTFT, HasTTFT: e.HasTTFT}
	// Only a cost the harness itself reported is passed through; the
	// relay never estimates one. Anything else is sent as cost unknown.
	if e.CostKnown {
		o.CallCostUSD, o.CallCostKnown = e.CostUSD, true
	}
	return o
}

func (st *State) add(mc Out) {
	st.TokenIn += mc.InputTokens + mc.CacheRead + mc.CacheCreation
	st.TokenOut += mc.OutputTokens
	if mc.CallCostKnown {
		st.Cost += mc.CallCostUSD
	} else {
		st.CostMissing = true
	}
}

// End closes the session for a native id (the SessionEnd hook signal).
func (a *Aggregator) End(client canon.ClientKind, nativeID string) []Out {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.close(a.Hash(client, nativeID))
}

// EndLocal closes a session identified by canon.LocalSessionHash, as sent
// by the SessionEnd hook. Unknown hashes are ignored (the idle timeout
// still closes the session).
func (a *Aggregator) EndLocal(client canon.ClientKind, localHash string) []Out {
	a.mu.Lock()
	defer a.mu.Unlock()
	h, ok := a.local[localHash]
	if !ok || a.live[h] == nil || a.live[h].Client != client {
		return nil
	}
	return a.close(h)
}

// Tick closes idle sessions and expires tombstones.
func (a *Aggregator) Tick() []Out {
	a.mu.Lock()
	defer a.mu.Unlock()
	now := a.cfg.Now()
	var out []Out
	for _, h := range a.sortedLive() {
		if now.Sub(a.live[h].Last) >= a.cfg.Idle {
			out = append(out, a.close(h)...)
		}
	}
	for h, t := range a.ended {
		if now.Sub(t) >= a.cfg.Tombstone {
			delete(a.ended, h)
		}
	}
	return out
}

// Reset discards every open session without emitting anything. The daemon
// calls it when queued data is purged (withdrawal, an early-access refusal)
// so nothing aggregated before that moment survives in memory.
func (a *Aggregator) Reset() {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.live = map[string]*State{}
	a.local = map[string]string{}
}

// CloseAll closes every open session (explicit shutdown flush).
func (a *Aggregator) CloseAll() []Out {
	a.mu.Lock()
	defer a.mu.Unlock()
	var out []Out
	for _, h := range a.sortedLive() {
		out = append(out, a.close(h)...)
	}
	return out
}

func (a *Aggregator) sortedLive() []string {
	hs := make([]string, 0, len(a.live))
	for h := range a.live {
		hs = append(hs, h)
	}
	sort.Strings(hs)
	return hs
}

func (a *Aggregator) close(h string) []Out {
	st := a.live[h]
	if st == nil {
		return nil
	}
	delete(a.live, h)
	for k, v := range a.local {
		if v == h {
			delete(a.local, k)
		}
	}
	a.ended[h] = a.cfg.Now()
	var out []Out
	if !st.SawSSEUsage {
		for _, mc := range st.Fallback {
			st.add(mc)
			out = append(out, mc)
		}
	}
	cost, known := st.Cost, !st.CostMissing
	if st.HasTurnCost {
		cost, known = st.TurnCost, true
	}
	d := st.Last.Sub(st.First)
	if d < time.Millisecond {
		d = time.Millisecond // Fleet requires duration_ms > 0
	}
	return append(out, Out{Kind: OutEnded, Client: st.Client, Hash: h, Time: st.Last,
		Provider: st.Provider, Duration: d, TokenIn: st.TokenIn, TokenOut: st.TokenOut,
		CostUSD: cost, CostKnown: known})
}

// Snapshot returns open-session state for persistence in the spool.
func (a *Aggregator) Snapshot() []State {
	a.mu.Lock()
	defer a.mu.Unlock()
	out := make([]State, 0, len(a.live))
	for _, h := range a.sortedLive() {
		out = append(out, *a.live[h])
	}
	return out
}

// Restore reloads persisted open sessions after a restart. Restored
// sessions do not emit conversation_started again.
func (a *Aggregator) Restore(states []State) {
	a.mu.Lock()
	defer a.mu.Unlock()
	for i := range states {
		s := states[i]
		a.live[s.Hash] = &s
	}
}
