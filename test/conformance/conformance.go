// Package conformance holds the baseline conformance harness: the host
// compatibility lock, the feature × host × OS matrix, per-cell evidence
// records and the digest-bound release verdict that publication consumes.
//
// A cell passes only on an installed, locked host against a development
// Fleet with a recorded installed journey. Anything missing (an
// UNVERIFIED lock value, an absent host, absent credentials, no recorded
// journey, another OS) makes the cell blocked, never passed. Component
// journeys replayed against in-process fake Fleet providers are recorded
// as supporting evidence; a failing one fails the cell, a passing one
// does not pass it.
package conformance

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"regexp"
	"sort"
	"strings"
)

// Unverified marks a lock value nobody has observed yet.
const Unverified = "UNVERIFIED"

// Verdicts. Nothing else is valid.
const (
	Pass    = "pass"
	Fail    = "fail"
	Blocked = "blocked"
)

// Gates a blocked or failed cell cites.
const (
	GateIdentity    = "E01"       // dev identity, entitled test users, credentials
	GateConformance = "E06"       // installed hosts, lock, candidate digests, runners
	GateComponent   = "component" // this checkout's own component behavior
)

// Required baseline dimensions. The matrix must be exactly their product.
var (
	RequiredFeatures = []string{"otel-export", "context", "private-memory", "skills"}
	// RequiredHosts is the basic release: Claude Code and Codex. OpenCode
	// ships disabled and its cells wait in later/opencode.json.
	RequiredHosts = []string{"claude-code-cli", "codex-cli"}
	RequiredOS    = []string{"macos-14-arm64", "ubuntu-24.04-amd64", "windows-11-amd64"}
)

// Lock is compatibility.json: the exact host releases, OS builds and
// artifact digests a candidate is qualified against.
type Lock struct {
	SchemaVersion   int          `json:"schema_version"`
	Status          string       `json:"status"` // unverified | locked
	Note            string       `json:"note,omitempty"`
	RecordedAt      string       `json:"recorded_at"`
	PluginCandidate Candidate    `json:"plugin_candidate"`
	Hosts           []LockHost   `json:"hosts"`
	OSTargets       []LockTarget `json:"os_targets"`
}

// Candidate is the plugin release candidate under test.
type Candidate struct {
	Version      string     `json:"version"`
	SourceCommit string     `json:"source_commit"`
	Artifacts    []Artifact `json:"artifacts"`
}

// Artifact is one OS's final (signed) candidate artifact.
type Artifact struct {
	OS     string `json:"os"`
	Name   string `json:"name"`
	SHA256 string `json:"sha256"`
}

// LockHost is one host and its exact release per OS.
type LockHost struct {
	ID        string         `json:"id"`
	Name      string         `json:"name"`
	Channel   string         `json:"channel"`
	Version   string         `json:"version"`
	Artifacts []HostArtifact `json:"artifacts"`
}

// HostArtifact is the installed host artifact on one OS.
type HostArtifact struct {
	OS     string `json:"os"`
	Source string `json:"source"`
	SHA256 string `json:"sha256"`
}

// LockTarget is one OS target and its exact build.
type LockTarget struct {
	ID    string `json:"id"`
	Name  string `json:"name"`
	Build string `json:"build"`
}

var (
	sha256Re = regexp.MustCompile(`^[0-9a-f]{64}$`)
	commitRe = regexp.MustCompile(`^[0-9a-f]{40}$`)
	// A pinned version is exact: no channel names, ranges or wildcards.
	floatingVersionRe = regexp.MustCompile(`(?i)^(latest|stable|next|beta|canary|nightly|current)$|[\s*^~<>=|]|\.x\b`)
)

// LoadLock reads and decodes a lock file strictly.
func LoadLock(path string) (Lock, []byte, error) {
	var l Lock
	b, err := os.ReadFile(path)
	if err != nil {
		return l, nil, err
	}
	return l, b, decodeStrict(b, &l)
}

func decodeStrict(b []byte, v any) error {
	dec := json.NewDecoder(strings.NewReader(string(b)))
	dec.DisallowUnknownFields()
	return dec.Decode(v)
}

// Validate checks the lock's shape and returns the UNVERIFIED fields.
// An error means the lock is malformed or claims more than it holds (a
// "locked" status with UNVERIFIED values, a floating version, a digest
// that is not a SHA-256).
func (l Lock) Validate() (unverified []string, err error) {
	var errs []error
	bad := func(f string, a ...any) { errs = append(errs, fmt.Errorf(f, a...)) }
	check := func(field, v string, ok func(string) bool, want string) {
		switch {
		case v == Unverified:
			unverified = append(unverified, field)
		case v == "" || !ok(v):
			bad("%s: %q is not %s (or %s)", field, v, want, Unverified)
		}
	}
	isSHA := func(v string) bool { return sha256Re.MatchString(v) }
	isVersion := func(v string) bool { return !floatingVersionRe.MatchString(v) }
	nonEmpty := func(v string) bool { return strings.TrimSpace(v) != "" }

	if l.SchemaVersion != 1 {
		bad("schema_version %d, want 1", l.SchemaVersion)
	}
	check("recorded_at", l.RecordedAt, nonEmpty, "a timestamp")
	check("plugin_candidate.version", l.PluginCandidate.Version, isVersion, "an exact version")
	check("plugin_candidate.source_commit", l.PluginCandidate.SourceCommit, commitRe.MatchString, "a full commit id")
	seen := map[string]bool{}
	for _, a := range l.PluginCandidate.Artifacts {
		if !contains(RequiredOS, a.OS) || seen[a.OS] {
			bad("plugin_candidate.artifacts: unknown or duplicate os %q", a.OS)
		}
		seen[a.OS] = true
		check("plugin_candidate.artifacts["+a.OS+"].name", a.Name, nonEmpty, "a file name")
		check("plugin_candidate.artifacts["+a.OS+"].sha256", a.SHA256, isSHA, "a SHA-256")
	}
	if len(seen) != len(RequiredOS) {
		bad("plugin_candidate.artifacts: want one per OS %v", RequiredOS)
	}
	hosts := map[string]bool{}
	for _, h := range l.Hosts {
		if !contains(RequiredHosts, h.ID) || hosts[h.ID] {
			bad("hosts: unknown or duplicate host %q", h.ID)
		}
		hosts[h.ID] = true
		check("hosts["+h.ID+"].version", h.Version, isVersion, "an exact version")
		os := map[string]bool{}
		for _, a := range h.Artifacts {
			if !contains(RequiredOS, a.OS) || os[a.OS] {
				bad("hosts[%s].artifacts: unknown or duplicate os %q", h.ID, a.OS)
			}
			os[a.OS] = true
			check("hosts["+h.ID+"].artifacts["+a.OS+"].source", a.Source, nonEmpty, "a download source")
			check("hosts["+h.ID+"].artifacts["+a.OS+"].sha256", a.SHA256, isSHA, "a SHA-256")
		}
		if len(os) != len(RequiredOS) {
			bad("hosts[%s].artifacts: want one per OS %v", h.ID, RequiredOS)
		}
	}
	if len(hosts) != len(RequiredHosts) {
		bad("hosts: want exactly %v (the baseline is never reduced)", RequiredHosts)
	}
	targets := map[string]bool{}
	for _, o := range l.OSTargets {
		if !contains(RequiredOS, o.ID) || targets[o.ID] {
			bad("os_targets: unknown or duplicate %q", o.ID)
		}
		targets[o.ID] = true
		check("os_targets["+o.ID+"].build", o.Build, nonEmpty, "an exact OS build")
	}
	if len(targets) != len(RequiredOS) {
		bad("os_targets: want exactly %v", RequiredOS)
	}
	switch l.Status {
	case "unverified":
	case "locked":
		if len(unverified) > 0 {
			bad("status is locked but %d value(s) are %s", len(unverified), Unverified)
		}
	default:
		bad("status %q, want unverified|locked", l.Status)
	}
	return unverified, errors.Join(errs...)
}

// Locked reports whether the lock is valid with no UNVERIFIED value.
func (l Lock) Locked() bool {
	u, err := l.Validate()
	return err == nil && len(u) == 0 && l.Status == "locked"
}

// HostVersion returns the locked version of host (or UNVERIFIED).
func (l Lock) HostVersion(host string) string {
	for _, h := range l.Hosts {
		if h.ID == host {
			return h.Version
		}
	}
	return Unverified
}

// HostLocked reports whether host's version and its artifact on os are
// both recorded.
func (l Lock) HostLocked(host, os string) bool {
	for _, h := range l.Hosts {
		if h.ID != host || h.Version == Unverified {
			continue
		}
		for _, a := range h.Artifacts {
			if a.OS == os {
				return a.SHA256 != Unverified && a.Source != Unverified
			}
		}
	}
	return false
}

// CandidateDigest returns the candidate artifact digest for os.
func (l Lock) CandidateDigest(os string) string {
	for _, a := range l.PluginCandidate.Artifacts {
		if a.OS == os {
			return a.SHA256
		}
	}
	return Unverified
}

// Matrix is matrix.json.
type Matrix struct {
	SchemaVersion int        `json:"schema_version"`
	Features      []Named    `json:"features"`
	Hosts         []Named    `json:"hosts"`
	OSTargets     []OSTarget `json:"os_targets"`
	Cells         []Cell     `json:"cells"`
}

// Named is a dimension entry.
type Named struct {
	ID          string `json:"id"`
	Name        string `json:"name,omitempty"`
	Description string `json:"description,omitempty"`
}

// OSTarget maps a matrix OS to the Go platform a runner reports.
type OSTarget struct {
	ID     string `json:"id"`
	Name   string `json:"name"`
	GOOS   string `json:"goos"`
	GOARCH string `json:"goarch"`
}

// Cell is one required feature/host/OS combination.
type Cell struct {
	ID       string `json:"id"`
	Feature  string `json:"feature"`
	Host     string `json:"host"`
	OS       string `json:"os"`
	Required bool   `json:"required"`
	// InstalledJourney is the recorded installed-host journey script,
	// relative to the matrix directory. It may not exist yet.
	InstalledJourney string `json:"installed_journey"`
	// ComponentJourney replays recorded fixtures against fake Fleet
	// providers (supporting evidence only). Nil when none exists.
	ComponentJourney *ComponentJourney `json:"component_journey"`
}

// ComponentJourney is a `go test` selection.
type ComponentJourney struct {
	Package string `json:"package"`
	Run     string `json:"run"`
}

// LoadMatrix reads and decodes matrix.json strictly.
func LoadMatrix(path string) (Matrix, error) {
	var m Matrix
	b, err := os.ReadFile(path)
	if err != nil {
		return m, err
	}
	return m, decodeStrict(b, &m)
}

// Validate checks that the matrix is exactly the required product, every
// cell required, ids consistent and unique.
func (m Matrix) Validate() error {
	var errs []error
	bad := func(f string, a ...any) { errs = append(errs, fmt.Errorf(f, a...)) }
	if m.SchemaVersion != 1 {
		bad("schema_version %d, want 1", m.SchemaVersion)
	}
	if got := ids(m.Features); !equal(got, RequiredFeatures) {
		bad("features %v, want %v", got, RequiredFeatures)
	}
	if got := ids(m.Hosts); !equal(got, RequiredHosts) {
		bad("hosts %v, want %v", got, RequiredHosts)
	}
	var osIDs []string
	for _, o := range m.OSTargets {
		osIDs = append(osIDs, o.ID)
	}
	if !equal(osIDs, RequiredOS) {
		bad("os_targets %v, want %v", osIDs, RequiredOS)
	}
	want := len(RequiredFeatures) * len(RequiredHosts) * len(RequiredOS)
	if len(m.Cells) != want {
		bad("%d cells, want %d", len(m.Cells), want)
	}
	seen := map[string]bool{}
	for _, c := range m.Cells {
		if c.ID != c.Feature+"/"+c.Host+"/"+c.OS {
			bad("cell %q: id does not match feature/host/os", c.ID)
		}
		if !contains(RequiredFeatures, c.Feature) || !contains(RequiredHosts, c.Host) || !contains(RequiredOS, c.OS) {
			bad("cell %q: outside the baseline dimensions", c.ID)
		}
		if seen[c.ID] {
			bad("cell %q: duplicate", c.ID)
		}
		seen[c.ID] = true
		if !c.Required {
			bad("cell %q: every baseline cell is required", c.ID)
		}
		if c.InstalledJourney == "" || strings.Contains(c.InstalledJourney, "..") {
			bad("cell %q: installed_journey must be a path inside the harness", c.ID)
		}
		if j := c.ComponentJourney; j != nil && (!strings.HasPrefix(j.Package, "./") || j.Run == "") {
			bad("cell %q: component_journey needs a ./package and a -run pattern", c.ID)
		}
	}
	return errors.Join(errs...)
}

// Target returns the OS target for id.
func (m Matrix) Target(id string) (OSTarget, bool) {
	for _, o := range m.OSTargets {
		if o.ID == id {
			return o, true
		}
	}
	return OSTarget{}, false
}

// Reason is why a cell is blocked (or failed): the gate and a sentence.
type Reason struct {
	Gate   string `json:"gate"`
	Reason string `json:"reason"`
}

// ComponentResult is the outcome of a component journey.
type ComponentResult struct {
	Command      string `json:"command"`
	Result       string `json:"result"` // pass | fail | missing | not_run
	Passed       int    `json:"tests_passed"`
	Failed       int    `json:"tests_failed"`
	OutputSHA256 string `json:"output_sha256,omitempty"`
}

// Evidence is one cell's record. It never holds tokens, content or
// personal identifiers: environment values are allowlisted names.
type Evidence struct {
	SchemaVersion       int               `json:"schema_version"`
	Cell                string            `json:"cell"`
	Revision            string            `json:"revision"`
	RevisionDirty       bool              `json:"revision_dirty"`
	LockSHA256          string            `json:"lock_sha256"`
	CandidateSHA256     string            `json:"candidate_sha256"`
	HostVersion         string            `json:"host_version"`
	HostVersionObserved string            `json:"host_version_observed,omitempty"`
	OS                  string            `json:"os"`
	OSObserved          string            `json:"os_observed"`
	Env                 map[string]string `json:"env"`
	Command             string            `json:"command"`
	Expected            string            `json:"expected"`
	Actual              string            `json:"actual"`
	Component           *ComponentResult  `json:"component,omitempty"`
	Verdict             string            `json:"verdict"`
	Reasons             []Reason          `json:"reasons,omitempty"`
	StartedAt           string            `json:"started_at"`
	FinishedAt          string            `json:"finished_at"`
}

// Validate checks an evidence record. A pass must be backed by a locked
// host and candidate, a clean revision, a run installed journey and no
// blocking reason; a blocked or failed cell must say why.
func (e Evidence) Validate(m Matrix) error {
	var errs []error
	bad := func(f string, a ...any) { errs = append(errs, fmt.Errorf("%s: "+f, append([]any{e.Cell}, a...)...)) }
	if e.SchemaVersion != 1 {
		bad("schema_version %d", e.SchemaVersion)
	}
	found := false
	for _, c := range m.Cells {
		found = found || c.ID == e.Cell
	}
	if !found {
		bad("not a matrix cell")
	}
	if e.Revision == "" || e.LockSHA256 == "" || e.Command == "" || e.Expected == "" || e.Actual == "" {
		bad("revision, lock_sha256, command, expected and actual are required")
	}
	switch e.Verdict {
	case Pass:
		if len(e.Reasons) > 0 {
			bad("pass with blocking reasons")
		}
		if e.RevisionDirty {
			bad("pass from a modified working tree")
		}
		for name, v := range map[string]string{"host_version": e.HostVersion, "candidate_sha256": e.CandidateSHA256} {
			if v == "" || v == Unverified {
				bad("pass without a recorded %s", name)
			}
		}
		if e.Component != nil && e.Component.Result == Fail {
			bad("pass with a failing component journey")
		}
	case Fail, Blocked:
		if len(e.Reasons) == 0 {
			bad("%s without a reason", e.Verdict)
		}
	default:
		bad("verdict %q, want pass|fail|blocked", e.Verdict)
	}
	return errors.Join(errs...)
}

// Verdict is the digest-bound release verdict. Publication accepts a
// candidate only when CheckVerdict succeeds for its artifacts.
type Verdict struct {
	SchemaVersion  int           `json:"schema_version"`
	Revision       string        `json:"revision"`
	LockSHA256     string        `json:"lock_sha256"`
	LockStatus     string        `json:"lock_status"`
	Candidate      Candidate     `json:"candidate"`
	Cells          []VerdictCell `json:"cells"`
	Summary        Summary       `json:"summary"`
	ReleaseAllowed bool          `json:"release_allowed"`
	// VerdictSHA256 is SHA-256 over the canonical JSON of every other
	// field (with this one empty).
	VerdictSHA256 string `json:"verdict_sha256"`
}

// VerdictCell binds one cell's verdict to its evidence bytes.
type VerdictCell struct {
	Cell           string `json:"cell"`
	Verdict        string `json:"verdict"`
	EvidenceSHA256 string `json:"evidence_sha256"`
}

// Summary counts verdicts.
type Summary struct {
	Pass    int `json:"pass"`
	Fail    int `json:"fail"`
	Blocked int `json:"blocked"`
	Missing int `json:"missing"`
}

// BuildVerdict combines the lock and the evidence for every matrix cell.
// evidence maps cell id to the raw evidence file bytes. A missing or
// invalid record counts as missing; any non-pass blocks release.
func BuildVerdict(m Matrix, l Lock, lockBytes []byte, evidence map[string][]byte) (Verdict, error) {
	v := Verdict{SchemaVersion: 1, LockSHA256: Digest(lockBytes), LockStatus: l.Status, Candidate: l.PluginCandidate}
	var errs []error
	revs := map[string]bool{}
	for _, c := range m.Cells {
		raw, ok := evidence[c.ID]
		vc := VerdictCell{Cell: c.ID, Verdict: "missing"}
		if ok {
			var e Evidence
			if err := decodeStrict(raw, &e); err != nil {
				errs = append(errs, fmt.Errorf("%s: %w", c.ID, err))
			} else if err := e.Validate(m); err != nil {
				errs = append(errs, err)
			} else if e.LockSHA256 != v.LockSHA256 {
				errs = append(errs, fmt.Errorf("%s: evidence was recorded against another lock", c.ID))
			} else {
				vc.Verdict, vc.EvidenceSHA256 = e.Verdict, Digest(raw)
				revs[e.Revision] = true
			}
		}
		switch vc.Verdict {
		case Pass:
			v.Summary.Pass++
		case Fail:
			v.Summary.Fail++
		case Blocked:
			v.Summary.Blocked++
		default:
			v.Summary.Missing++
		}
		v.Cells = append(v.Cells, vc)
	}
	switch len(revs) {
	case 0:
		v.Revision = Unverified
	case 1:
		for r := range revs {
			v.Revision = r
		}
	default:
		errs = append(errs, errors.New("evidence spans more than one source revision"))
		v.Revision = "mixed"
	}
	v.ReleaseAllowed = len(errs) == 0 && l.Locked() && v.Summary.Pass == len(m.Cells) && len(m.Cells) > 0
	v.VerdictSHA256 = v.digest()
	return v, errors.Join(errs...)
}

func (v Verdict) digest() string {
	v.VerdictSHA256 = ""
	b, _ := json.Marshal(v)
	return Digest(b)
}

// CheckVerdict is the publication gate: the verdict is intact, allows
// release, covers every matrix cell with a pass, and binds exactly the
// candidate artifacts being published (os → SHA-256 of the final bytes).
func CheckVerdict(raw []byte, m Matrix, artifacts map[string]string) error {
	var v Verdict
	if err := decodeStrict(raw, &v); err != nil {
		return fmt.Errorf("verdict: %w", err)
	}
	var errs []error
	if v.VerdictSHA256 != v.digest() {
		errs = append(errs, errors.New("verdict digest does not match its content (modified)"))
	}
	if !v.ReleaseAllowed || v.LockStatus != "locked" {
		errs = append(errs, fmt.Errorf("release not allowed: %d pass, %d fail, %d blocked, %d missing; lock %s",
			v.Summary.Pass, v.Summary.Fail, v.Summary.Blocked, v.Summary.Missing, v.LockStatus))
	}
	got := map[string]string{}
	for _, c := range v.Cells {
		got[c.Cell] = c.Verdict
	}
	for _, c := range m.Cells {
		if got[c.ID] != Pass {
			errs = append(errs, fmt.Errorf("%s: %s", c.ID, orMissing(got[c.ID])))
		}
	}
	bound := map[string]string{}
	for _, a := range v.Candidate.Artifacts {
		bound[a.OS] = a.SHA256
	}
	if len(artifacts) == 0 {
		errs = append(errs, errors.New("no candidate artifacts given to check against the verdict"))
	}
	for os, sum := range artifacts {
		if bound[os] == "" || bound[os] == Unverified || bound[os] != sum {
			errs = append(errs, fmt.Errorf("artifact for %s (%s) is not the qualified candidate", os, short(sum)))
		}
	}
	return errors.Join(errs...)
}

// Digest is the lowercase hex SHA-256 of b.
func Digest(b []byte) string {
	h := sha256.Sum256(b)
	return hex.EncodeToString(h[:])
}

func orMissing(s string) string {
	if s == "" {
		return "missing"
	}
	return s
}

func short(s string) string {
	if len(s) > 12 {
		return s[:12]
	}
	return s
}

func ids(ns []Named) []string {
	out := make([]string, 0, len(ns))
	for _, n := range ns {
		out = append(out, n.ID)
	}
	return out
}

func equal(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	x, y := append([]string(nil), a...), append([]string(nil), b...)
	sort.Strings(x)
	sort.Strings(y)
	for i := range x {
		if x[i] != y[i] {
			return false
		}
	}
	return true
}

func contains(xs []string, x string) bool {
	for _, v := range xs {
		if v == x {
			return true
		}
	}
	return false
}
