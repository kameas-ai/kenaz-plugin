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
//
// There are two evidence tiers, recorded separately (see README.md):
//
//   - live: the pass described above. Only live evidence feeds the release
//     verdict and the publication gate.
//   - installed: the credential-free tier. It builds this checkout's
//     unofficial kenaz, sets it up in throwaway directories and exercises
//     what works with no model credentials and no Fleet sign-in. An
//     installed-tier pass is never a live pass: BuildVerdict rejects it and
//     CheckVerdict refuses an installed-tier summary by name.
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
	GateInstalled   = "installed" // the credential-free installed-tier journey
)

// Evidence tiers. An evidence record with no tier is live (records written
// before the installed tier existed).
const (
	TierLive      = "live"
	TierInstalled = "installed"
)

// Step results of an installed-tier journey. NotRun is a step the journey
// could not honestly perform on this machine (for example the host CLI is
// absent); it is recorded, never counted as a pass.
const (
	StepPass   = "pass"
	StepFail   = "fail"
	StepNotRun = "not_run"
)

// Host visibility: whether the real host CLI was asked, without a model
// call or a sign-in, and listed the plugin.
const (
	VisibilityVerified    = "verified"
	VisibilityNotRun      = "not_run"       // host CLI absent, or its listing command is unavailable
	VisibilityNeedsSignIn = "needs_sign_in" // host listing needs sign-in
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
	// InstalledTierJourney is the credential-free installed-tier journey
	// script, relative to the matrix directory. It is a different script
	// from InstalledJourney on purpose: writing it must never unblock the
	// live tier.
	InstalledTierJourney string `json:"installed_tier_journey,omitempty"`
	// ComponentJourney replays recorded fixtures against fake Fleet
	// providers (supporting evidence only). Nil when none exists.
	ComponentJourney *ComponentJourney `json:"component_journey"`
}

// ComponentJourney is a `go test` selection, plus further selections in
// other packages when the feature's tests span more than one.
type ComponentJourney struct {
	Package string          `json:"package"`
	Run     string          `json:"run"`
	Also    []TestSelection `json:"also,omitempty"`
}

// TestSelection is one `go test -run RUN PACKAGE`.
type TestSelection struct {
	Package string `json:"package"`
	Run     string `json:"run"`
}

// Selections returns every `go test` selection of the journey, in order.
func (j *ComponentJourney) Selections() []TestSelection {
	if j == nil {
		return nil
	}
	return append([]TestSelection{{j.Package, j.Run}}, j.Also...)
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
		for _, sel := range c.ComponentJourney.Selections() {
			if !strings.HasPrefix(sel.Package, "./") || sel.Run == "" {
				bad("cell %q: component_journey needs a ./package and a -run pattern", c.ID)
			}
		}
		if j := c.InstalledTierJourney; j != "" && (strings.Contains(j, "..") || j == c.InstalledJourney || strings.HasPrefix(j, "journeys/")) {
			bad("cell %q: installed_tier_journey must be a path inside the harness, outside journeys/ and different from installed_journey", c.ID)
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
	// Tier is "live" or "installed"; empty means live. Installed holds the
	// installed-tier journey's result and is set exactly when Tier is
	// "installed" and the journey ran.
	Tier      string           `json:"tier,omitempty"`
	Installed *InstalledResult `json:"installed,omitempty"`
	// OSMatch says how the runner compares with the cell's OS target, for an
	// installed-tier cell that ran: exact (the runner is the target OS
	// release), family (the same OS family and architecture, another
	// release or edition; OSNote names it) or unknown (the release could
	// not be observed). OSObserved holds what the runner is. The live gate
	// does not read these.
	OSMatch string `json:"os_match,omitempty"`
	OSNote  string `json:"os_note,omitempty"`
}

// How a runner compares with a cell's OS target (Evidence.OSMatch).
const (
	OSMatchExact   = "exact"
	OSMatchFamily  = "family"
	OSMatchUnknown = "unknown"
)

// Reduction says why an installed-tier pass is a reduced one: steps the
// journey recorded as not run, and a runner that is not the cell's target
// OS. A pass with neither is a full installed-tier pass.
type Reduction struct {
	NotRunSteps []string // step ids, in journey order
	OSNote      string   // "" when the runner is the target OS
}

// Reduced reports whether there is anything to say.
func (r Reduction) Reduced() bool { return len(r.NotRunSteps) > 0 || r.OSNote != "" }

// String is the one-line form used in reports.
func (r Reduction) String() string {
	var parts []string
	if n := len(r.NotRunSteps); n > 0 {
		parts = append(parts, fmt.Sprintf("%d step(s) not run: %s", n, strings.Join(r.NotRunSteps, ", ")))
	}
	if r.OSNote != "" {
		parts = append(parts, r.OSNote)
	}
	return strings.Join(parts, "; ")
}

// Reduction returns what an installed-tier record leaves out of a full
// pass. It is derived from the record, so evidence written before the OS
// fields existed still reports its steps not run.
func (e Evidence) Reduction() Reduction {
	var r Reduction
	if e.Tier != TierInstalled || e.Installed == nil {
		return r
	}
	for _, s := range e.Installed.Steps {
		if s.Status == StepNotRun {
			r.NotRunSteps = append(r.NotRunSteps, s.ID)
		}
	}
	switch e.OSMatch {
	case "", OSMatchExact:
	default:
		r.OSNote = e.OSNote
		if r.OSNote == "" {
			r.OSNote = "the runner (" + e.OSObserved + ") is not known to be the target " + e.OS
		}
	}
	return r
}

// EvidenceTier is the record's tier: "installed", or "live" for anything
// else a valid record can hold.
func (e Evidence) EvidenceTier() string {
	if e.Tier == TierInstalled {
		return TierInstalled
	}
	return TierLive
}

// InstalledResult is what an installed-tier journey observed. It holds no
// tokens, no file or tool content and no user paths: details are short,
// fixed sentences and counts.
type InstalledResult struct {
	Build   InstalledBuild  `json:"build"`
	HostCLI InstalledHost   `json:"host_cli"`
	Steps   []InstalledStep `json:"steps"`
	// Unproven lists what this tier leaves unproven for the cell.
	Unproven []string `json:"unproven"`
}

// InstalledBuild is the kenaz the journey built and ran.
type InstalledBuild struct {
	Flavor  string `json:"flavor"` // always "unofficial": the journey refuses anything else
	Version string `json:"version"`
	Tags    string `json:"tags"` // build tags, "" for the default build
	// OfficialDifferences names the checks that would differ for an
	// official build.
	OfficialDifferences []string `json:"official_differences"`
}

// InstalledHost is the real host CLI on the runner, if any.
type InstalledHost struct {
	Present bool   `json:"present"`
	Version string `json:"version,omitempty"`
	// Visibility is verified, not_run or needs_sign_in (see the constants).
	Visibility string `json:"visibility"`
	Note       string `json:"note,omitempty"`
}

// InstalledStep is one step of an installed-tier journey.
type InstalledStep struct {
	ID      string `json:"id"`
	Status  string `json:"status"` // pass | fail | not_run
	Command string `json:"command,omitempty"`
	Detail  string `json:"detail,omitempty"`
}

// Validate checks a journey result's shape.
func (r InstalledResult) Validate() error {
	var errs []error
	if r.Build.Flavor != "unofficial" {
		errs = append(errs, fmt.Errorf("installed tier ran a %q build; it runs the unofficial build only", r.Build.Flavor))
	}
	switch r.HostCLI.Visibility {
	case VisibilityVerified:
		if !r.HostCLI.Present {
			errs = append(errs, errors.New("host visibility verified without a host CLI"))
		}
	case VisibilityNotRun, VisibilityNeedsSignIn:
	default:
		errs = append(errs, fmt.Errorf("host visibility %q", r.HostCLI.Visibility))
	}
	if len(r.Steps) == 0 {
		errs = append(errs, errors.New("no steps"))
	}
	seen := map[string]bool{}
	for _, s := range r.Steps {
		if s.ID == "" || seen[s.ID] {
			errs = append(errs, fmt.Errorf("step id %q empty or repeated", s.ID))
		}
		seen[s.ID] = true
		switch s.Status {
		case StepPass:
		case StepFail, StepNotRun:
			if s.Detail == "" {
				errs = append(errs, fmt.Errorf("step %s: %s without a detail", s.ID, s.Status))
			}
		default:
			errs = append(errs, fmt.Errorf("step %s: status %q", s.ID, s.Status))
		}
	}
	if len(r.Unproven) == 0 {
		errs = append(errs, errors.New("an installed-tier result must say what it leaves unproven"))
	}
	return errors.Join(errs...)
}

// Count returns the number of steps with each status.
func (r InstalledResult) Count() (pass, fail, notRun int) {
	for _, s := range r.Steps {
		switch s.Status {
		case StepPass:
			pass++
		case StepFail:
			fail++
		default:
			notRun++
		}
	}
	return
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
	switch e.OSMatch {
	case "", OSMatchExact:
	case OSMatchFamily, OSMatchUnknown:
		if e.OSNote == "" {
			bad("os_match %s without an os_note", e.OSMatch)
		}
	default:
		bad("os_match %q, want exact|family|unknown", e.OSMatch)
	}
	switch e.Tier {
	case "", TierLive:
		if e.Installed != nil {
			bad("live evidence with an installed-tier result")
		}
		if e.OSMatch != "" {
			bad("live evidence with an installed-tier os_match")
		}
	case TierInstalled:
		if e.Component != nil {
			bad("installed-tier evidence with a component journey")
		}
		if e.Installed != nil {
			if err := e.Installed.Validate(); err != nil {
				bad("%v", err)
			}
		}
	default:
		bad("tier %q, want live|installed", e.Tier)
	}
	switch {
	case e.Verdict == Pass && e.Tier == TierInstalled:
		// An installed-tier pass: every step the journey ran passed. It
		// needs no lock and no candidate digest, which is exactly why it
		// can never stand in for a live pass.
		if len(e.Reasons) > 0 {
			bad("pass with blocking reasons")
		}
		if e.Installed == nil {
			bad("installed-tier pass without a journey result")
		} else if p, f, _ := e.Installed.Count(); f > 0 || p == 0 {
			bad("installed-tier pass with %d failed and %d passed step(s)", f, p)
		}
	case e.Verdict == Pass:
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
	case e.Verdict == Fail, e.Verdict == Blocked:
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
// invalid record counts as missing; any non-pass blocks release. Only
// live-tier evidence counts: an installed-tier record is an error and its
// cell is missing, whatever its verdict.
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
			} else if e.EvidenceTier() != TierLive {
				errs = append(errs, fmt.Errorf("%s: %s-tier evidence cannot count toward a release verdict; a release needs a live pass", c.ID, e.EvidenceTier()))
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
//
// It checks the tier first and names the one it found: only a live verdict
// can pass. An installed-tier summary (or anything else that says it is not
// live) is refused before anything else is read from it.
func CheckVerdict(raw []byte, m Matrix, artifacts map[string]string) error {
	tier := VerdictTier(raw)
	if tier != TierLive {
		return fmt.Errorf("found tier %q, not a live release verdict: the publication gate needs a live pass of every cell (an installed-tier pass proves setup and local behavior of an unofficial build only; a release verdict is written by `conformance verdict` and declares no tier)", tier)
	}
	var v Verdict
	if err := decodeStrict(raw, &v); err != nil {
		return fmt.Errorf("verdict (tier live): %w", err)
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

// VerdictTier reads the tier of a verdict-like document without trusting
// anything else in it. A release verdict is live by construction (it is
// built from live evidence only) and carries no tier field, so that its
// bytes and digest are what every released check-verdict already reads; a
// document is taken for one when it declares no tier and has the digest
// field only a verdict has. Anything that declares a tier is that tier as
// written ("installed" for the installed-tier summary). Everything else is
// "unknown".
func VerdictTier(raw []byte) string {
	var t map[string]json.RawMessage
	if err := json.Unmarshal(raw, &t); err != nil {
		return "unknown"
	}
	if declared, ok := t["tier"]; ok {
		var tier string
		if json.Unmarshal(declared, &tier) != nil || tier == "" || tier == TierLive {
			// A release verdict never declares a tier, so one that says
			// "live" (or nothing readable) is not what BuildVerdict wrote.
			return "unknown"
		}
		return tier
	}
	if _, ok := t["verdict_sha256"]; ok {
		return TierLive
	}
	return "unknown"
}

// InstalledSummary is the installed tier's own summary. It is not a release
// verdict: it has no release_allowed that can be true, no candidate and no
// lock, and CheckVerdict refuses it by its tier.
type InstalledSummary struct {
	SchemaVersion int    `json:"schema_version"`
	Kind          string `json:"kind"` // "installed-tier-summary"
	Tier          string `json:"tier"` // "installed"
	Revision      string `json:"revision"`
	// ReleaseAllowed is always false. The field exists so that a reader
	// looking for it finds the answer.
	ReleaseAllowed bool            `json:"release_allowed"`
	Note           string          `json:"note"`
	Cells          []InstalledCell `json:"cells"`
	Summary        InstalledCounts `json:"summary"`
}

// InstalledCell is one cell's installed-tier outcome.
type InstalledCell struct {
	Cell           string `json:"cell"`
	Result         string `json:"result"` // pass | fail | blocked | not_run
	HostVisibility string `json:"host_visibility,omitempty"`
	StepsPassed    int    `json:"steps_passed"`
	StepsFailed    int    `json:"steps_failed"`
	StepsNotRun    int    `json:"steps_not_run"`
	Tags           string `json:"tags,omitempty"`
	EvidenceSHA256 string `json:"evidence_sha256,omitempty"`
	// Reduced marks a pass that is not a full one: the journey recorded
	// steps as not run (NotRunSteps), or the runner was not the cell's
	// target OS (OSNote). A reduced pass is still result "pass"; a reader
	// must not take it for a full installed-tier pass.
	Reduced     bool     `json:"reduced,omitempty"`
	NotRunSteps []string `json:"not_run_steps,omitempty"`
	OSObserved  string   `json:"os_observed,omitempty"`
	OSMatch     string   `json:"os_match,omitempty"`
	OSNote      string   `json:"os_note,omitempty"`
}

// InstalledCounts counts installed-tier outcomes. Pass counts every pass;
// PassReduced is how many of them are reduced (InstalledCell.Reduced), so
// the full passes are Pass - PassReduced.
type InstalledCounts struct {
	Pass        int `json:"pass"`
	PassReduced int `json:"pass_reduced"`
	Fail        int `json:"fail"`
	Blocked     int `json:"blocked"`
	NotRun      int `json:"not_run"`
}

// InstalledSummaryNote is the fixed sentence every installed-tier summary
// carries.
const InstalledSummaryNote = "Installed tier only: an unofficial build, no model credentials, no Fleet sign-in. This is not a release verdict and cannot satisfy the publication gate, which needs a live pass of every cell."

// BuildInstalledSummary combines installed-tier evidence for every matrix
// cell. A cell with no record, or with a record that is invalid or not
// installed-tier, is not_run.
func BuildInstalledSummary(m Matrix, evidence map[string][]byte) (InstalledSummary, error) {
	s := InstalledSummary{SchemaVersion: 1, Kind: "installed-tier-summary", Tier: TierInstalled, Note: InstalledSummaryNote}
	var errs []error
	revs := map[string]bool{}
	for _, c := range m.Cells {
		ic := InstalledCell{Cell: c.ID, Result: "not_run"}
		if raw, ok := evidence[c.ID]; ok {
			var e Evidence
			switch err := decodeStrict(raw, &e); {
			case err != nil:
				errs = append(errs, fmt.Errorf("%s: %w", c.ID, err))
			case e.Validate(m) != nil:
				errs = append(errs, e.Validate(m))
			case e.Tier != TierInstalled:
				errs = append(errs, fmt.Errorf("%s: not installed-tier evidence", c.ID))
			default:
				ic.Result, ic.EvidenceSHA256 = e.Verdict, Digest(raw)
				if r := e.Installed; r != nil {
					ic.HostVisibility, ic.Tags = r.HostCLI.Visibility, r.Build.Tags
					ic.StepsPassed, ic.StepsFailed, ic.StepsNotRun = r.Count()
					ic.OSObserved, ic.OSMatch = e.OSObserved, e.OSMatch
					red := e.Reduction()
					ic.NotRunSteps, ic.OSNote = red.NotRunSteps, red.OSNote
					ic.Reduced = e.Verdict == Pass && red.Reduced()
				}
				revs[e.Revision] = true
			}
		}
		switch ic.Result {
		case Pass:
			s.Summary.Pass++
			if ic.Reduced {
				s.Summary.PassReduced++
			}
		case Fail:
			s.Summary.Fail++
		case Blocked:
			s.Summary.Blocked++
		default:
			s.Summary.NotRun++
		}
		s.Cells = append(s.Cells, ic)
	}
	switch len(revs) {
	case 0:
		s.Revision = Unverified
	case 1:
		for r := range revs {
			s.Revision = r
		}
	default:
		s.Revision = "mixed"
	}
	return s, errors.Join(errs...)
}

// CellStates. A cell is reported in exactly one of them per tier pair.
const (
	StateLivePass      = "live pass"
	StateInstalledPass = "installed-tier pass"
	// StateInstalledReduced is an installed-tier pass that is not a full
	// one: steps were not run, or the runner was not the target OS.
	StateInstalledReduced = "installed-tier pass (reduced)"
	StateNotRun           = "not run"
)

// CellReport is one cell's standing across both tiers.
type CellReport struct {
	Cell           string `json:"cell"`
	State          string `json:"state"`     // live pass | installed-tier pass | installed-tier pass (reduced) | not run
	Live           string `json:"live"`      // pass | fail | blocked | not_run
	Installed      string `json:"installed"` // pass | fail | blocked | not_run
	HostVisibility string `json:"host_visibility,omitempty"`
	// What an installed-tier pass leaves out (StateInstalledReduced).
	NotRunSteps []string `json:"not_run_steps,omitempty"`
	OSObserved  string   `json:"os_observed,omitempty"`
	OSNote      string   `json:"os_note,omitempty"`
}

// Reduction is the report row's reduction, for printing.
func (r CellReport) Reduction() Reduction { return Reduction{r.NotRunSteps, r.OSNote} }

// Report states, for every matrix cell, which tier (if any) it has passed:
// a live pass, else an installed-tier pass (a full one, or a reduced one
// when the journey left steps not run or the runner was not the target
// OS), else not run. A failed or blocked record in either tier is shown in
// that tier's column and never raises the state.
func Report(m Matrix, live, installed map[string][]byte) []CellReport {
	read := func(raw []byte, tier string) (e Evidence, verdict string) {
		if raw == nil || decodeStrict(raw, &e) != nil || e.Validate(m) != nil || e.EvidenceTier() != tier {
			return Evidence{}, "not_run"
		}
		return e, e.Verdict
	}
	var out []CellReport
	for _, c := range m.Cells {
		r := CellReport{Cell: c.ID, State: StateNotRun}
		_, r.Live = read(live[c.ID], TierLive)
		var e Evidence
		e, r.Installed = read(installed[c.ID], TierInstalled)
		if e.Installed != nil {
			r.HostVisibility = e.Installed.HostCLI.Visibility
		}
		switch {
		case r.Live == Pass:
			r.State = StateLivePass
		case r.Installed == Pass:
			r.State = StateInstalledPass
			if red := e.Reduction(); red.Reduced() {
				r.State = StateInstalledReduced
				r.NotRunSteps, r.OSNote, r.OSObserved = red.NotRunSteps, red.OSNote, e.OSObserved
			}
		}
		out = append(out, r)
	}
	return out
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
