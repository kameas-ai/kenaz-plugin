// Package pipeline wires telemetry end to end for the relay daemon:
// receiver → decode → session aggregation → project → gate → encode. It
// implements otlprx.Sink. Queueing, spooling and forwarding are the
// daemon's and arrive as callbacks, so this package does no I/O.
package pipeline

import (
	"sync"
	"time"

	logsv1 "go.opentelemetry.io/proto/otlp/logs/v1"
	metricsv1 "go.opentelemetry.io/proto/otlp/metrics/v1"
	tracev1 "go.opentelemetry.io/proto/otlp/trace/v1"

	"github.com/kameas-ai/kenaz-plugin/fleetapi"
	"github.com/kameas-ai/kenaz-plugin/internal/canon"
	"github.com/kameas-ai/kenaz-plugin/internal/decode"
	"github.com/kameas-ai/kenaz-plugin/internal/gate"
	"github.com/kameas-ai/kenaz-plugin/internal/project"
	"github.com/kameas-ai/kenaz-plugin/internal/session"
)

// Config wires the pipeline. Every func must be cheap and non-blocking.
type Config struct {
	Decoder    decode.Decoder
	Aggregator *session.Aggregator
	// Decider answers consent questions.
	Decider gate.Decider
	// Options returns the current schema revision (from the capabilities
	// poller, fleetapi.Capabilities.TelemetrySchema).
	Options func() project.Options
	// Identity returns the stamped identity; ok=false when not signed in.
	Identity func() (project.Identity, bool)
	// Emit receives final batches for the forwarder queue, with the
	// earliest event time among the records they carry.
	Emit func(batches []fleetapi.ProjectedBatch, earliest time.Time)
	// Hold receives records to spool until the hold reason clears; the
	// daemon re-submits them through Regate.
	Hold func(recs []project.Record, reason string)
}

// Pipeline implements otlprx.Sink.
type Pipeline struct {
	cfg   Config
	gates gate.Counters

	mu       sync.Mutex
	counters map[string]int
	versions map[canon.ClientKind]string
}

// New returns a Pipeline.
func New(cfg Config) *Pipeline {
	return &Pipeline{cfg: cfg, counters: map[string]int{}, versions: map[canon.ClientKind]string{}}
}

func (p *Pipeline) note(r decode.Result) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.counters["dropped_unknown_source"] += r.UnknownSource
	p.counters["content_seen"] += r.ContentSeen
	for k, v := range r.ClientVersions {
		p.versions[k] = v
	}
}

// Logs implements otlprx.Sink.
func (p *Pipeline) Logs(m *logsv1.LogsData) { p.ingest(p.cfg.Decoder.Logs(m)) }

// Traces implements otlprx.Sink.
func (p *Pipeline) Traces(m *tracev1.TracesData) { p.ingest(p.cfg.Decoder.Traces(m)) }

// Metrics implements otlprx.Sink.
func (p *Pipeline) Metrics(m *metricsv1.MetricsData) { p.ingest(p.cfg.Decoder.Metrics(m)) }

// SessionEnd implements otlprx.Sink.
func (p *Pipeline) SessionEnd(c canon.ClientKind, id string) {
	p.process(p.cfg.Aggregator.End(c, id))
}

// SessionEndLocal closes a session by its on-device local hash (the
// SessionEnd hook path).
func (p *Pipeline) SessionEndLocal(c canon.ClientKind, localHash string) {
	p.process(p.cfg.Aggregator.EndLocal(c, localHash))
}

// Tick closes idle sessions; the daemon calls it about once a minute.
func (p *Pipeline) Tick() { p.process(p.cfg.Aggregator.Tick()) }

func (p *Pipeline) ingest(r decode.Result) {
	p.note(r)
	// While the organization is not admitted to early access nothing is
	// collected: events are dropped before session aggregation, so no
	// pre-admission usage can reach a session summary sent after admission.
	if p.cfg.Decider != nil && !gate.Admitted(p.cfg.Decider) {
		p.gates.AddNotAdmitted(len(r.Events))
		return
	}
	p.process(p.cfg.Aggregator.Ingest(r.Events))
}

// Reset discards open sessions without emitting them (see
// session.Aggregator.Reset).
func (p *Pipeline) Reset() { p.cfg.Aggregator.Reset() }

func (p *Pipeline) opts() project.Options {
	if p.cfg.Options == nil {
		return project.Options{}
	}
	return p.cfg.Options()
}

func (p *Pipeline) process(outs []session.Out) {
	if len(outs) == 0 {
		return
	}
	p.Regate(project.Project(outs, p.opts()))
}

// Regate runs records through the gates and emits what may be sent. The
// daemon calls it again for spooled records once a hold clears.
func (p *Pipeline) Regate(recs []project.Record) {
	o := gate.Apply(p.cfg.Decider, recs, &p.gates)
	if len(o.Hold) > 0 && p.cfg.Hold != nil {
		p.cfg.Hold(o.Hold, o.HoldReason)
	}
	if len(o.Send) == 0 {
		return
	}
	id, ok := project.Identity{}, false
	if p.cfg.Identity != nil {
		id, ok = p.cfg.Identity()
	}
	if !ok {
		p.add("dropped_no_identity", len(o.Send))
		return
	}
	p.mu.Lock()
	id.ClientVersions = make(map[canon.ClientKind]string, len(p.versions))
	for k, v := range p.versions {
		id.ClientVersions[k] = v
	}
	p.mu.Unlock()
	batches, err := project.Encode(o.Send, id, p.opts())
	if err != nil {
		p.add("dropped_encode_error", len(o.Send))
		return
	}
	if p.cfg.Emit != nil {
		p.cfg.Emit(batches, gate.Earliest(o.Send))
	}
}

func (p *Pipeline) add(k string, n int) {
	p.mu.Lock()
	p.counters[k] += n
	p.mu.Unlock()
}

// Counters returns pipeline and gate counters for `kenaz status`.
// ContentSeen > 0 drives the doctor advisory.
func (p *Pipeline) Counters() map[string]int {
	out := p.gates.Snapshot()
	p.mu.Lock()
	defer p.mu.Unlock()
	for k, v := range p.counters {
		out[k] = v
	}
	return out
}
