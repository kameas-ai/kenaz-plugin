package conformance

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func shipped(t *testing.T) (Matrix, Lock, []byte) {
	t.Helper()
	m, err := LoadMatrix("matrix.json")
	if err != nil {
		t.Fatal(err)
	}
	l, b, err := LoadLock("compatibility.json")
	if err != nil {
		t.Fatal(err)
	}
	return m, l, b
}

// The shipped lock and matrix are valid; the lock is honestly unverified
// (no invented versions or digests).
func TestShippedFiles(t *testing.T) {
	m, l, _ := shipped(t)
	if err := m.Validate(); err != nil {
		t.Fatal(err)
	}
	if len(m.Cells) != 24 {
		t.Fatalf("%d cells", len(m.Cells))
	}
	u, err := l.Validate()
	if err != nil {
		t.Fatal(err)
	}
	if l.Status != "unverified" || l.Locked() || len(u) == 0 {
		t.Fatalf("shipped lock must stay unverified until observed: status %s, %d unverified", l.Status, len(u))
	}
	for _, h := range l.Hosts {
		if h.Version != Unverified {
			t.Fatalf("host %s carries version %q without recorded evidence", h.ID, h.Version)
		}
	}
}

// lockedFixture is a synthetic, fully filled lock for tests only.
func lockedFixture(t *testing.T) (Lock, []byte) {
	t.Helper()
	_, l, _ := shipped(t)
	sum := strings.Repeat("ab", 32)
	l.Status, l.RecordedAt = "locked", "2026-01-01T00:00:00Z"
	l.PluginCandidate.Version, l.PluginCandidate.SourceCommit = "9.9.9-test", strings.Repeat("c", 40)
	for i := range l.PluginCandidate.Artifacts {
		l.PluginCandidate.Artifacts[i].Name, l.PluginCandidate.Artifacts[i].SHA256 = "kenaz-test.tar.gz", sum
	}
	for i := range l.Hosts {
		l.Hosts[i].Version = "9.9.9-test"
		for j := range l.Hosts[i].Artifacts {
			l.Hosts[i].Artifacts[j].Source, l.Hosts[i].Artifacts[j].SHA256 = "test://fixture", sum
		}
	}
	for i := range l.OSTargets {
		l.OSTargets[i].Build = "test-build"
	}
	b, _ := json.Marshal(l)
	return l, b
}

func TestLockRejectsOverclaims(t *testing.T) {
	good, _ := lockedFixture(t)
	if !good.Locked() {
		_, err := good.Validate()
		t.Fatalf("fixture not locked: %v", err)
	}
	for name, mutate := range map[string]func(l *Lock){
		"floating version":       func(l *Lock) { l.Hosts[0].Version = "latest" },
		"version range":          func(l *Lock) { l.Hosts[1].Version = ">=0.147" },
		"wildcard version":       func(l *Lock) { l.Hosts[1].Version = "1.x" },
		"bad digest":             func(l *Lock) { l.Hosts[0].Artifacts[0].SHA256 = "abc" },
		"locked with unverified": func(l *Lock) { l.Hosts[0].Artifacts[1].SHA256 = Unverified },
		"host dropped":           func(l *Lock) { l.Hosts = l.Hosts[:1] },
		"os dropped":             func(l *Lock) { l.OSTargets = l.OSTargets[1:] },
		"short commit":           func(l *Lock) { l.PluginCandidate.SourceCommit = "abc123" },
	} {
		l, _ := lockedFixture(t)
		mutate(&l)
		if _, err := l.Validate(); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}

func TestMatrixRejectsReduction(t *testing.T) {
	for name, mutate := range map[string]func(m *Matrix){
		"cell dropped":   func(m *Matrix) { m.Cells = m.Cells[1:] },
		"duplicate cell": func(m *Matrix) { m.Cells[1] = m.Cells[0] },
		"optional cell":  func(m *Matrix) { m.Cells[3].Required = false },
		"host dropped":   func(m *Matrix) { m.Hosts = m.Hosts[:1] },
		"id mismatch":    func(m *Matrix) { m.Cells[0].ID = "skills/codex-cli/macos-14-arm64" },
	} {
		m, _, _ := shipped(t)
		mutate(&m)
		if m.Validate() == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}

const ubuntu = "ID=ubuntu\nVERSION_ID=\"24.04\"\nPRETTY_NAME=\"Ubuntu 24.04 LTS\"\n"

type fakeSys struct {
	installed map[string]bool
	goTest    func(args []string) ([]byte, int)
	journey   int // exit code of installed journeys
	ran       []string
}

func (f *fakeSys) opts(t *testing.T, m Matrix, l Lock, lb []byte, env map[string]string) RunOptions {
	return RunOptions{
		Root: ".", HarnessDir: t.TempDir(), Matrix: m, Lock: l, LockBytes: lb, OutDir: t.TempDir(),
		Revision: strings.Repeat("d", 40), GOOS: "linux", GOARCH: "amd64", OSRelease: ubuntu,
		LookPath: func(b string) (string, error) {
			if f.installed[b] {
				return "/usr/bin/" + b, nil
			}
			return "", os.ErrNotExist
		},
		Exec: func(_ context.Context, _ string, _ []string, name string, args ...string) ([]byte, int, error) {
			f.ran = append(f.ran, name+" "+strings.Join(args, " "))
			switch name {
			case "go":
				if f.goTest != nil {
					out, code := f.goTest(args)
					return out, code, nil
				}
				return []byte("--- PASS: TestX (0.00s)\nok\n"), 0, nil
			case "sh":
				return nil, f.journey, nil
			}
			return []byte(name + " 9.9.9-test\n"), 0, nil
		},
		Getenv: func(k string) string { return env[k] },
		Now:    func() time.Time { return time.Unix(1_900_000_000, 0) },
	}
}

func byCell(ev []Evidence) map[string]Evidence {
	out := map[string]Evidence{}
	for _, e := range ev {
		out[e.Cell] = e
	}
	return out
}

// Without lock values, hosts and credentials every cell is blocked with
// reasons and gates, never passed; other OSes are blocked as such.
func TestRunBlocksWithoutPrerequisites(t *testing.T) {
	m, l, lb := shipped(t)
	sys := &fakeSys{installed: map[string]bool{"claude": true}}
	ev, err := Run(context.Background(), sys.opts(t, m, l, lb, nil))
	if err != nil {
		t.Fatal(err)
	}
	if len(ev) != 24 {
		t.Fatalf("%d records", len(ev))
	}
	for _, e := range ev {
		if e.Verdict != Blocked || len(e.Reasons) == 0 {
			t.Fatalf("%s: %s %v", e.Cell, e.Verdict, e.Reasons)
		}
		if err := e.Validate(m); err != nil {
			t.Fatal(err)
		}
	}
	got := byCell(ev)
	c := got["otel-export/claude-code-cli/ubuntu-24.04-amd64"]
	if c.Component == nil || c.Component.Result != Pass || c.HostVersionObserved != "claude 9.9.9-test" || !hasGate(c, GateIdentity) || !hasGate(c, GateConformance) {
		t.Fatalf("claude ubuntu cell: %+v", c)
	}
	if _, ok := got["skills/opencode/ubuntu-24.04-amd64"]; ok {
		t.Fatal("the basic release matrix has an OpenCode cell")
	}
	if w := got["context/codex-cli/windows-11-amd64"]; w.Component != nil || !strings.Contains(w.Reasons[0].Reason, "windows-11-amd64 runner") {
		t.Fatalf("windows cell on linux: %+v", w)
	}
}

// Missing credentials keep a cell blocked even when everything else is
// in place.
func TestRunMissingCredentialsNeverPass(t *testing.T) {
	m, _, _ := shipped(t)
	l, lb := lockedFixture(t)
	sys := &fakeSys{installed: map[string]bool{"claude": true, "codex": true, "opencode": true}}
	o := sys.opts(t, m, l, lb, nil)
	writeJourneys(t, o.HarnessDir, m)
	ev, _ := Run(context.Background(), o)
	for _, e := range ev {
		if e.Verdict == Pass {
			t.Fatalf("%s passed without a development Fleet identity", e.Cell)
		}
	}
}

// A failing component journey fails the cell whatever the gates.
func TestRunComponentFailureFails(t *testing.T) {
	m, l, lb := shipped(t)
	sys := &fakeSys{goTest: func([]string) ([]byte, int) { return []byte("--- FAIL: TestX (0.00s)\nFAIL\n"), 1 }}
	ev, _ := Run(context.Background(), sys.opts(t, m, l, lb, nil))
	got := byCell(ev)
	if e := got["otel-export/codex-cli/ubuntu-24.04-amd64"]; e.Verdict != Fail || e.Reasons[0].Gate != GateComponent {
		t.Fatalf("%+v", e)
	}
	// A selection that matches no test is missing, not passed.
	sys = &fakeSys{goTest: func([]string) ([]byte, int) { return []byte("testing: warning: no tests to run\nok\n"), 0 }}
	ev, _ = Run(context.Background(), sys.opts(t, m, l, lb, nil))
	if e := byCell(ev)["skills/codex-cli/ubuntu-24.04-amd64"]; e.Verdict != Blocked || e.Component.Result != "missing" {
		t.Fatalf("%+v", e)
	}
}

func writeJourneys(t *testing.T, dir string, m Matrix) {
	t.Helper()
	for _, c := range m.Cells {
		p := filepath.Join(dir, filepath.FromSlash(c.InstalledJourney))
		os.MkdirAll(filepath.Dir(p), 0o755)
		os.WriteFile(p, []byte("exit 0\n"), 0o755)
	}
}

// With every prerequisite present, a recorded installed journey decides
// the cell: exit 0 passes, anything else fails.
func TestRunInstalledJourneyDecides(t *testing.T) {
	m, _, _ := shipped(t)
	l, lb := lockedFixture(t)
	env := map[string]string{DevReadyEnv: "1"}
	sys := &fakeSys{installed: map[string]bool{"claude": true, "codex": true, "opencode": true}}
	o := sys.opts(t, m, l, lb, env)
	writeJourneys(t, o.HarnessDir, m)
	o.Cells = []string{"otel-export/claude-code-cli/*"}
	ev, _ := Run(context.Background(), o)
	if e := byCell(ev)["otel-export/claude-code-cli/ubuntu-24.04-amd64"]; e.Verdict != Pass || e.Validate(m) != nil {
		t.Fatalf("%+v %v", e, e.Validate(m))
	}
	sys.journey = 3
	ev, _ = Run(context.Background(), o)
	if e := byCell(ev)["otel-export/claude-code-cli/ubuntu-24.04-amd64"]; e.Verdict != Fail {
		t.Fatalf("%+v", e)
	}
}

func hasGate(e Evidence, g string) bool {
	for _, r := range e.Reasons {
		if r.Gate == g {
			return true
		}
	}
	return false
}

// passEvidence builds valid pass records for every cell.
func passEvidence(t *testing.T, m Matrix, lb []byte) map[string][]byte {
	t.Helper()
	out := map[string][]byte{}
	for _, c := range m.Cells {
		e := Evidence{SchemaVersion: 1, Cell: c.ID, Revision: strings.Repeat("d", 40), LockSHA256: Digest(lb),
			CandidateSHA256: strings.Repeat("ab", 32), HostVersion: "9.9.9-test", OS: c.OS, OSObserved: "test",
			Env: map[string]string{}, Command: "sh j", Expected: "x", Actual: "y", Verdict: Pass}
		b, _ := json.Marshal(e)
		out[c.ID] = b
	}
	return out
}

// The verdict binds the lock, every cell's evidence bytes and the
// candidate digests; publication accepts only an intact, all-pass verdict
// for exactly the qualified artifacts.
func TestVerdictDigestBound(t *testing.T) {
	m, _, _ := shipped(t)
	l, lb := lockedFixture(t)
	ev := passEvidence(t, m, lb)
	v, err := BuildVerdict(m, l, lb, ev)
	if err != nil || !v.ReleaseAllowed || v.Summary.Pass != 24 {
		t.Fatalf("%+v %v", v.Summary, err)
	}
	raw, _ := json.Marshal(v)
	arts := map[string]string{"ubuntu-24.04-amd64": strings.Repeat("ab", 32)}
	if err := CheckVerdict(raw, m, arts); err != nil {
		t.Fatal(err)
	}
	// Other bytes than the qualified candidate.
	if err := CheckVerdict(raw, m, map[string]string{"ubuntu-24.04-amd64": strings.Repeat("ef", 32)}); err == nil {
		t.Fatal("accepted an artifact that was not qualified")
	}
	if err := CheckVerdict(raw, m, nil); err == nil {
		t.Fatal("accepted a check without artifacts")
	}
	// A modified verdict.
	var tampered Verdict
	json.Unmarshal(raw, &tampered)
	tampered.Cells[0].EvidenceSHA256 = strings.Repeat("0", 64)
	tb, _ := json.Marshal(tampered)
	if err := CheckVerdict(tb, m, arts); err == nil || !strings.Contains(err.Error(), "modified") {
		t.Fatalf("tampered verdict: %v", err)
	}

	// One blocked cell blocks release.
	var e Evidence
	json.Unmarshal(ev[m.Cells[5].ID], &e)
	e.Verdict, e.Reasons = Blocked, []Reason{{GateIdentity, "no dev identity"}}
	ev[m.Cells[5].ID], _ = json.Marshal(e)
	v, _ = BuildVerdict(m, l, lb, ev)
	raw, _ = json.Marshal(v)
	if v.ReleaseAllowed || CheckVerdict(raw, m, arts) == nil {
		t.Fatal("a blocked cell allowed release")
	}
	// A missing cell blocks release.
	delete(ev, m.Cells[7].ID)
	if v, _ := BuildVerdict(m, l, lb, ev); v.ReleaseAllowed || v.Summary.Missing != 1 {
		t.Fatalf("%+v", v.Summary)
	}
	// An unverified lock blocks release even with all cells passing.
	_, ul, ulb := shipped(t)
	if v, _ := BuildVerdict(m, ul, ulb, passEvidence(t, m, ulb)); v.ReleaseAllowed {
		t.Fatal("release allowed on an unverified lock")
	}
}

// A pass record that overclaims is rejected.
func TestEvidenceRejectsOverclaimedPass(t *testing.T) {
	m, _, _ := shipped(t)
	base := Evidence{SchemaVersion: 1, Cell: m.Cells[0].ID, Revision: "r", LockSHA256: "x", CandidateSHA256: strings.Repeat("ab", 32),
		HostVersion: "1", Command: "c", Expected: "e", Actual: "a", Verdict: Pass}
	for name, mutate := range map[string]func(e *Evidence){
		"unverified host":    func(e *Evidence) { e.HostVersion = Unverified },
		"unverified digest":  func(e *Evidence) { e.CandidateSHA256 = Unverified },
		"blocking reason":    func(e *Evidence) { e.Reasons = []Reason{{GateIdentity, "x"}} },
		"dirty tree":         func(e *Evidence) { e.RevisionDirty = true },
		"failing component":  func(e *Evidence) { e.Component = &ComponentResult{Result: Fail} },
		"unknown verdict":    func(e *Evidence) { e.Verdict = "skipped" },
		"blocked w/o reason": func(e *Evidence) { e.Verdict = Blocked },
	} {
		e := base
		mutate(&e)
		if e.Validate(m) == nil {
			t.Errorf("%s: accepted", name)
		}
	}
	if err := base.Validate(m); err != nil {
		t.Fatal(err)
	}
}

func TestCurrentOS(t *testing.T) {
	for _, c := range []struct{ goos, arch, rel, want string }{
		{"linux", "amd64", ubuntu, "ubuntu-24.04-amd64"},
		{"linux", "amd64", "ID=ubuntu\nVERSION_ID=\"22.04\"\n", ""},
		{"linux", "arm64", ubuntu, ""},
		{"darwin", "arm64", "", "macos-14-arm64"},
		{"windows", "amd64", "", "windows-11-amd64"},
	} {
		if got := CurrentOS(c.goos, c.arch, c.rel); got != c.want {
			t.Errorf("%v: %q", c, got)
		}
	}
}

// The OpenCode cells kept for a later release still form a complete,
// valid matrix when merged back with OpenCode required again.
func TestLaterOpenCodeCellsStayValid(t *testing.T) {
	m, l, _ := shipped(t)
	raw, err := os.ReadFile(filepath.Join("later", "opencode.json"))
	if err != nil {
		t.Fatal(err)
	}
	var later struct {
		MatrixHost []Named    `json:"matrix_host"`
		LockHost   []LockHost `json:"lock_host"`
		Cells      []Cell     `json:"cells"`
	}
	if err := json.Unmarshal(raw, &later); err != nil {
		t.Fatal(err)
	}
	if len(later.Cells) != len(RequiredFeatures)*len(RequiredOS) {
		t.Fatalf("%d later OpenCode cells", len(later.Cells))
	}
	old := RequiredHosts
	RequiredHosts = append(append([]string(nil), old...), "opencode")
	defer func() { RequiredHosts = old }()
	m.Hosts = append(m.Hosts, later.MatrixHost...)
	m.Cells = append(m.Cells, later.Cells...)
	if err := m.Validate(); err != nil {
		t.Fatalf("merged matrix: %v", err)
	}
	l.Hosts = append(l.Hosts, later.LockHost...)
	if _, err := l.Validate(); err != nil {
		t.Fatalf("merged lock: %v", err)
	}
}
