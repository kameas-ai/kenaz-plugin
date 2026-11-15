package conformance

import (
	"context"
	"encoding/json"
	"go/ast"
	"go/build"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// installedResult is a journey result with the given step statuses.
func installedResult(statuses ...string) InstalledResult {
	r := InstalledResult{
		Build:    InstalledBuild{Flavor: "unofficial", Version: "0.0.0-dev", OfficialDifferences: []string{"x"}},
		HostCLI:  InstalledHost{Visibility: VisibilityNotRun, Note: "host CLI absent"},
		Unproven: []string{"everything that needs a model or Fleet"},
	}
	for i, s := range statuses {
		st := InstalledStep{ID: "step-" + string(rune('a'+i)), Status: s}
		if s != StepPass {
			st.Detail = "why"
		}
		r.Steps = append(r.Steps, st)
	}
	return r
}

// installedSys runs the installed tier with a journey that writes result
// and exits code.
func installedSys(t *testing.T, m Matrix, l Lock, lb []byte, result *InstalledResult, code int) (RunOptions, *[]string) {
	t.Helper()
	sys := &fakeSys{}
	o := sys.opts(t, m, l, lb, nil)
	o.Tier = TierInstalled
	var ran []string
	o.Exec = func(_ context.Context, _ string, env []string, name string, args ...string) ([]byte, int, error) {
		ran = append(ran, name+" "+strings.Join(args, " "))
		for _, kv := range env {
			if p, ok := strings.CutPrefix(kv, StepsEnv+"="); ok && result != nil {
				b, _ := json.Marshal(result)
				if err := os.WriteFile(p, b, 0o600); err != nil {
					t.Fatal(err)
				}
			}
		}
		return nil, code, nil
	}
	return o, &ran
}

// The installed tier runs the cell's installed-tier journey (never the
// live one), needs no lock, host or dev identity, and records what the
// journey observed. A failed step fails the cell; cells of another OS are
// blocked.
func TestInstalledTierRun(t *testing.T) {
	m, l, lb := shipped(t)
	res := installedResult(StepPass, StepNotRun, StepPass)
	o, ran := installedSys(t, m, l, lb, &res, 0)
	ev, err := Run(context.Background(), o)
	if err != nil || len(ev) != 24 {
		t.Fatalf("%d records, %v", len(ev), err)
	}
	pass := 0
	for _, e := range ev {
		if err := e.Validate(m); err != nil {
			t.Fatal(err)
		}
		if e.Tier != TierInstalled || e.Component != nil {
			t.Fatalf("%s: tier %q, component %v", e.Cell, e.Tier, e.Component)
		}
		switch {
		case strings.HasSuffix(e.Cell, "ubuntu-24.04-amd64"):
			if e.Verdict != Pass || e.Installed == nil || !strings.Contains(e.Actual, "Not a live pass") {
				t.Fatalf("%s: %s %v %q", e.Cell, e.Verdict, e.Reasons, e.Actual)
			}
			pass++
		case e.Verdict != Blocked || !hasGate(e, GateInstalled):
			t.Fatalf("%s on another OS: %s %v", e.Cell, e.Verdict, e.Reasons)
		}
		if e.HostVersion != Unverified || e.CandidateSHA256 != Unverified {
			t.Fatalf("%s: installed-tier evidence claims a locked host or candidate", e.Cell)
		}
	}
	if pass != 8 || len(*ran) != 8 {
		t.Fatalf("%d passed, ran %v", pass, *ran)
	}
	for _, r := range *ran {
		if !strings.HasPrefix(r, "sh installed/") || strings.Contains(r, "journeys/") {
			t.Fatalf("the installed tier ran %q", r)
		}
	}

	// A failed step fails the cell, whatever the exit code.
	res = installedResult(StepPass, StepFail)
	o, _ = installedSys(t, m, l, lb, &res, 0)
	o.Cells = []string{"skills/codex-cli/ubuntu-24.04-amd64"}
	ev, _ = Run(context.Background(), o)
	if e := ev[0]; e.Verdict != Fail || !hasGate(e, GateInstalled) || e.Validate(m) != nil {
		t.Fatalf("%+v %v", e, e.Validate(m))
	}
	// No result, a non-zero exit, no passed step or an official build: fail.
	for name, tc := range map[string]struct {
		res  *InstalledResult
		code int
	}{
		"no result":      {nil, 1},
		"non-zero exit":  {ptr(installedResult(StepPass)), 3},
		"nothing passed": {ptr(installedResult(StepNotRun)), 0},
		"official build": {func() *InstalledResult { r := installedResult(StepPass); r.Build.Flavor = "official"; return &r }(), 0},
	} {
		o, _ = installedSys(t, m, l, lb, tc.res, tc.code)
		o.Cells = []string{"skills/codex-cli/ubuntu-24.04-amd64"}
		ev, _ = Run(context.Background(), o)
		if ev[0].Verdict != Fail {
			t.Errorf("%s: %s", name, ev[0].Verdict)
		}
	}
}

func ptr[T any](v T) *T { return &v }

// installedPassEvidence is a valid installed-tier pass for every cell,
// recorded against lock bytes lb.
func installedPassEvidence(t *testing.T, m Matrix, lb []byte) map[string][]byte {
	t.Helper()
	out := map[string][]byte{}
	for _, c := range m.Cells {
		res := installedResult(StepPass)
		e := Evidence{SchemaVersion: 1, Tier: TierInstalled, Cell: c.ID, Revision: strings.Repeat("d", 40), LockSHA256: Digest(lb),
			CandidateSHA256: Unverified, HostVersion: Unverified, OS: c.OS, OSObserved: "test",
			Env: map[string]string{}, Command: "sh j", Expected: "x", Actual: "y", Verdict: Pass, Installed: &res}
		if err := e.Validate(m); err != nil {
			t.Fatal(err)
		}
		b, _ := json.Marshal(e)
		out[c.ID] = b
	}
	return out
}

// Installed-tier results never satisfy the live gate: not as evidence for a
// release verdict (even on a fully locked lock, even dressed up with a
// locked host and candidate), and not as a verdict handed to the
// publication check, which names the tier it found.
func TestInstalledTierNeverSatisfiesLiveGate(t *testing.T) {
	m, _, _ := shipped(t)
	l, lb := lockedFixture(t)
	arts := map[string]string{"ubuntu-24.04-amd64": strings.Repeat("ab", 32)}

	ev := installedPassEvidence(t, m, lb)
	v, err := BuildVerdict(m, l, lb, ev)
	if err == nil || !strings.Contains(err.Error(), "installed-tier evidence cannot count toward a release verdict") {
		t.Fatalf("BuildVerdict accepted installed-tier evidence: %v", err)
	}
	if v.ReleaseAllowed || v.Summary.Pass != 0 || v.Summary.Missing != 24 {
		t.Fatalf("%+v allowed=%v", v.Summary, v.ReleaseAllowed)
	}
	raw, _ := json.Marshal(v)
	if err := CheckVerdict(raw, m, arts); err == nil {
		t.Fatal("the gate passed a verdict built from installed-tier evidence")
	}

	// Installed-tier evidence that also claims a locked host and candidate
	// is still installed-tier.
	for id, b := range ev {
		var e Evidence
		json.Unmarshal(b, &e)
		e.HostVersion, e.CandidateSHA256 = "9.9.9-test", strings.Repeat("ab", 32)
		ev[id], _ = json.Marshal(e)
	}
	if v, err := BuildVerdict(m, l, lb, ev); err == nil || v.ReleaseAllowed {
		t.Fatal("dressed-up installed-tier evidence allowed a release")
	}
	// One installed-tier record among live passes blocks the release.
	live := passEvidence(t, m, lb)
	live[m.Cells[3].ID] = installedPassEvidence(t, m, lb)[m.Cells[3].ID]
	if v, err := BuildVerdict(m, l, lb, live); err == nil || v.ReleaseAllowed || v.Summary.Pass != 23 {
		t.Fatalf("one installed-tier record: %+v %v", v.Summary, err)
	}

	// The installed tier's own summary is refused by name.
	sum, err := BuildInstalledSummary(m, installedPassEvidence(t, m, lb))
	if err != nil || sum.Summary.Pass != 24 || sum.ReleaseAllowed || sum.Tier != TierInstalled {
		t.Fatalf("%+v %v", sum, err)
	}
	sraw, _ := json.Marshal(sum)
	if err := CheckVerdict(sraw, m, arts); err == nil || !strings.Contains(err.Error(), `found tier "installed"`) {
		t.Fatalf("installed summary at the gate: %v", err)
	}
	// So is a summary edited to claim release_allowed, and one relabelled
	// live (it is not a verdict: no digest, no candidate).
	edited := strings.Replace(string(sraw), `"release_allowed":false`, `"release_allowed":true`, 1)
	if err := CheckVerdict([]byte(edited), m, arts); err == nil {
		t.Fatal("an edited installed summary passed the gate")
	}
	for _, repl := range []string{`"tier":"live"`, `"tier":""`, `"tier":null`} {
		relabelled := strings.Replace(edited, `"tier":"installed"`, repl, 1)
		if err := CheckVerdict([]byte(relabelled), m, arts); err == nil {
			t.Fatalf("an installed summary relabelled %s passed the gate", repl)
		}
	}
	// With its tier removed it is still not a verdict: it has no digest.
	var noTier map[string]json.RawMessage
	json.Unmarshal([]byte(edited), &noTier)
	delete(noTier, "tier")
	nraw, _ := json.Marshal(noTier)
	if err := CheckVerdict(nraw, m, arts); err == nil || !strings.Contains(err.Error(), `found tier "unknown"`) {
		t.Fatalf("an installed summary with no tier: %v", err)
	}

	// A real live verdict passes, is found live, and is byte-for-byte what
	// the gate has always read: it declares no tier.
	lv, err := BuildVerdict(m, l, lb, passEvidence(t, m, lb))
	if err != nil || !lv.ReleaseAllowed {
		t.Fatal(err)
	}
	lraw, _ := json.Marshal(lv)
	if VerdictTier(lraw) != TierLive || CheckVerdict(lraw, m, arts) != nil || strings.Contains(string(lraw), `"tier"`) {
		t.Fatalf("live verdict: tier %s, %v", VerdictTier(lraw), CheckVerdict(lraw, m, arts))
	}
	// A verdict that declares a tier is not one BuildVerdict wrote.
	for _, add := range []string{`"tier":"installed",`, `"tier":"live",`, `"tier":"",`} {
		bad := strings.Replace(string(lraw), `{`, `{`+add, 1)
		if err := CheckVerdict([]byte(bad), m, arts); err == nil || !strings.Contains(err.Error(), "found tier") {
			t.Fatalf("%s: %v", add, err)
		}
	}
	if VerdictTier([]byte(`[]`)) != "unknown" || VerdictTier([]byte(`{"release_allowed":true}`)) != "unknown" {
		t.Fatal("a document that is neither a verdict nor a declared tier must be unknown")
	}
}

// An installed-tier record cannot claim a pass without a clean journey
// result, and a live record cannot carry one.
func TestInstalledEvidenceRejectsOverclaims(t *testing.T) {
	m, _, _ := shipped(t)
	good := installedResult(StepPass, StepNotRun)
	base := Evidence{SchemaVersion: 1, Tier: TierInstalled, Cell: m.Cells[0].ID, Revision: "r", LockSHA256: "x", CandidateSHA256: Unverified,
		HostVersion: Unverified, Command: "c", Expected: "e", Actual: "a", Verdict: Pass, Installed: &good}
	if err := base.Validate(m); err != nil {
		t.Fatal(err)
	}
	for name, mutate := range map[string]func(e *Evidence){
		"no result":        func(e *Evidence) { e.Installed = nil },
		"failed step":      func(e *Evidence) { e.Installed = ptr(installedResult(StepPass, StepFail)) },
		"nothing passed":   func(e *Evidence) { e.Installed = ptr(installedResult(StepNotRun)) },
		"official build":   func(e *Evidence) { r := installedResult(StepPass); r.Build.Flavor = "official"; e.Installed = &r },
		"no unproven list": func(e *Evidence) { r := installedResult(StepPass); r.Unproven = nil; e.Installed = &r },
		"visibility lie": func(e *Evidence) {
			r := installedResult(StepPass)
			r.HostCLI.Visibility = VisibilityVerified
			e.Installed = &r
		},
		"reason on a pass":  func(e *Evidence) { e.Reasons = []Reason{{GateInstalled, "x"}} },
		"unknown tier":      func(e *Evidence) { e.Tier = "smoke" },
		"live with result":  func(e *Evidence) { e.Tier = TierLive },
		"component journey": func(e *Evidence) { e.Component = &ComponentResult{Result: Pass} },
	} {
		e := base
		mutate(&e)
		if err := e.Validate(m); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}

// Report puts each cell in exactly one state: a live pass, else an
// installed-tier pass, else not run. Evidence filed under the wrong tier
// counts for nothing.
func TestReportStates(t *testing.T) {
	m, _, _ := shipped(t)
	_, lb := lockedFixture(t)
	live, inst := passEvidence(t, m, lb), installedPassEvidence(t, m, lb)
	a, b, c, d := m.Cells[0].ID, m.Cells[1].ID, m.Cells[2].ID, m.Cells[3].ID
	rep := Report(m,
		map[string][]byte{a: live[a], d: inst[d]},             // d: installed evidence in the live directory
		map[string][]byte{a: inst[a], b: inst[b], c: live[c]}) // c: live evidence in the installed directory
	got := map[string]CellReport{}
	for _, r := range rep {
		got[r.Cell] = r
	}
	if len(rep) != 24 || got[a].State != StateLivePass || got[b].State != StateInstalledPass || got[c].State != StateNotRun || got[d].State != StateNotRun {
		t.Fatalf("%+v %+v %+v %+v", got[a], got[b], got[c], got[d])
	}
	if got[m.Cells[9].ID].State != StateNotRun || got[b].Live != "not_run" {
		t.Fatalf("%+v", got[b])
	}
}

// Every cell names an installed-tier journey that ships, outside journeys/
// (where a live journey would unblock the live tier), and no live journey
// has been written by accident.
func TestInstalledTierJourneysShipped(t *testing.T) {
	m, _, _ := shipped(t)
	for _, c := range m.Cells {
		want := "installed/" + c.Feature + "." + c.Host + ".sh"
		if c.InstalledTierJourney != want {
			t.Errorf("%s: installed_tier_journey %q, want %q", c.ID, c.InstalledTierJourney, want)
		}
		b, err := os.ReadFile(filepath.FromSlash(c.InstalledTierJourney))
		if err != nil {
			t.Errorf("%s: %v", c.ID, err)
			continue
		}
		if !strings.Contains(string(b), "journey.sh\" "+c.Feature+" "+c.Host) {
			t.Errorf("%s: %s does not run the %s/%s journey", c.ID, c.InstalledTierJourney, c.Feature, c.Host)
		}
		if _, err := os.Stat(filepath.FromSlash(c.InstalledJourney)); err == nil {
			t.Errorf("%s: a live journey %s exists; it must be a recorded live journey, reviewed as such", c.ID, c.InstalledJourney)
		}
	}
	bad := m
	bad.Cells = append([]Cell(nil), m.Cells...)
	bad.Cells[0].InstalledTierJourney = bad.Cells[0].InstalledJourney
	if bad.Validate() == nil {
		t.Fatal("an installed-tier journey that is the live journey was accepted")
	}
}

// The schema describes what the Go types write for both tiers.
func TestSchemaKnowsTiers(t *testing.T) {
	b, err := os.ReadFile("evidence.schema.json")
	if err != nil {
		t.Fatal(err)
	}
	var s struct {
		Defs map[string]struct {
			Properties map[string]json.RawMessage `json:"properties"`
		} `json:"$defs"`
	}
	if err := json.Unmarshal(b, &s); err != nil {
		t.Fatal(err)
	}
	for def, typ := range map[string]any{"evidence": Evidence{}, "verdict": Verdict{}, "installedSummary": InstalledSummary{}, "installedResult": InstalledResult{}} {
		raw, _ := json.Marshal(typ)
		var fields map[string]json.RawMessage
		json.Unmarshal(raw, &fields)
		for f := range fields {
			if _, ok := s.Defs[def].Properties[f]; !ok {
				t.Errorf("evidence.schema.json $defs.%s has no property %q", def, f)
			}
		}
	}
	e := Evidence{Tier: TierInstalled, Installed: &InstalledResult{}}
	raw, _ := json.Marshal(e)
	var fields map[string]json.RawMessage
	json.Unmarshal(raw, &fields)
	for _, f := range []string{"tier", "installed"} {
		if _, ok := fields[f]; !ok {
			t.Fatalf("evidence does not marshal %q", f)
		}
		if _, ok := s.Defs["evidence"].Properties[f]; !ok {
			t.Errorf("evidence.schema.json $defs.evidence has no property %q", f)
		}
	}
}

// testFuncs lists the Test functions of the package in dir that the
// default build compiles.
func testFuncs(t *testing.T, dir string) []string {
	t.Helper()
	pkg, err := build.ImportDir(dir, 0)
	if err != nil {
		t.Fatalf("%s: %v", dir, err)
	}
	var names []string
	fset := token.NewFileSet()
	for _, f := range append(append([]string(nil), pkg.TestGoFiles...), pkg.XTestGoFiles...) {
		file, err := parser.ParseFile(fset, filepath.Join(dir, f), nil, parser.SkipObjectResolution)
		if err != nil {
			t.Fatal(err)
		}
		for _, d := range file.Decls {
			if fn, ok := d.(*ast.FuncDecl); ok && fn.Recv == nil && strings.HasPrefix(fn.Name.Name, "Test") && fn.Name.Name != "TestMain" {
				names = append(names, fn.Name.Name)
			}
		}
	}
	return names
}

// alternatives splits the top-level test selector of a -run pattern
// ("^(TestA|TestB.*)$", or "^TestA$/^sub$") into its alternatives.
func alternatives(run string) []string {
	top, _, _ := strings.Cut(run, "/")
	top = strings.TrimSuffix(strings.TrimPrefix(top, "^"), "$")
	if strings.HasPrefix(top, "(") && strings.HasSuffix(top, ")") {
		top = top[1 : len(top)-1]
	}
	var out []string
	depth, start := 0, 0
	for i, r := range top {
		switch r {
		case '(':
			depth++
		case ')':
			depth--
		case '|':
			if depth == 0 {
				out = append(out, top[start:i])
				start = i + 1
			}
		}
	}
	return append(out, top[start:])
}

// Every Test name a component journey lists (in matrix.json and in the
// parked OpenCode cells) exists in its package, so the lists cannot rot
// silently: a renamed or deleted test fails here instead of quietly
// shrinking what the component journey covers.
func TestComponentJourneyTestsExist(t *testing.T) {
	m, _, _ := shipped(t)
	cells := m.Cells
	if later, err := LoadMatrix(filepath.Join("later", "opencode.json")); err == nil {
		cells = append(append([]Cell(nil), cells...), later.Cells...)
	}
	funcs := map[string][]string{}
	checked := map[string]bool{}
	n := 0
	for _, c := range cells {
		for _, sel := range c.ComponentJourney.Selections() {
			key := sel.Package + " " + sel.Run
			if checked[key] {
				continue
			}
			checked[key] = true
			if _, ok := funcs[sel.Package]; !ok {
				funcs[sel.Package] = testFuncs(t, filepath.Join("..", "..", filepath.FromSlash(sel.Package)))
			}
			if _, err := regexp.Compile(sel.Run); err != nil {
				t.Errorf("%s: -run %q: %v", c.ID, sel.Run, err)
				continue
			}
			for _, alt := range alternatives(sel.Run) {
				re, err := regexp.Compile("^(?:" + alt + ")$")
				if err != nil || !strings.HasPrefix(alt, "Test") {
					t.Errorf("%s: %q in the -run pattern of %s is not a Test name or prefix pattern", c.ID, alt, sel.Package)
					continue
				}
				found := false
				for _, f := range funcs[sel.Package] {
					found = found || re.MatchString(f)
				}
				if !found {
					t.Errorf("%s: component journey lists %s in %s, but no such test exists there (renamed or removed?)", c.ID, alt, sel.Package)
				}
				n++
			}
		}
	}
	if n < 20 {
		t.Fatalf("checked only %d listed tests", n)
	}
}

// The check above catches a name that does not exist.
func TestAlternatives(t *testing.T) {
	for run, want := range map[string]string{
		"^(TestA|TestB.*|TestC)$":                     "TestA,TestB.*,TestC",
		"^TestConformanceOTelJourney$/^codex$":        "TestConformanceOTelJourney",
		"^TestSaveSearchGetForget$":                   "TestSaveSearchGetForget",
		"^(Test(List|Read).*|TestWrite.*)$":           "Test(List|Read).*,TestWrite.*",
		"^(TestGone|TestComponentJourneyTestsExist)$": "TestGone,TestComponentJourneyTestsExist",
	} {
		if got := strings.Join(alternatives(run), ","); got != want {
			t.Errorf("%s: %s, want %s", run, got, want)
		}
	}
	names := testFuncs(t, ".")
	has := func(n string) bool {
		for _, f := range names {
			if f == n {
				return true
			}
		}
		return false
	}
	if !has("TestComponentJourneyTestsExist") || has("TestGone") {
		t.Fatalf("testFuncs: %v", names)
	}
}

// The context cells' component journey covers what ships now: the context
// file tools, sharing a file, team pull, sync and the share grant.
func TestContextComponentJourneyCoversWhatShips(t *testing.T) {
	m, _, _ := shipped(t)
	for _, c := range m.Cells {
		if c.Feature != "context" {
			continue
		}
		var all string
		for _, sel := range c.ComponentJourney.Selections() {
			all += sel.Package + " " + sel.Run + "\n"
		}
		for _, want := range []string{
			"./internal/mcp/tools/context", "TestShareFile", "TestPullAll", "TestShareGrant", "TestFileToolsNeedContextFiles",
			"./internal/mcp/tools/localcontext", "TestWrite", "TestRead", "TestListEmptyIsNotNil",
			"./app", "TestContextSync", "TestContextGrant", "TestRelayContextGrantWiring",
		} {
			if !strings.Contains(all, want) {
				t.Errorf("%s: component journey does not cover %s", c.ID, want)
			}
		}
	}
}

// A component journey with several selections runs each and the worst
// result decides.
func TestRunComponentSelections(t *testing.T) {
	m, l, lb := shipped(t)
	var runs []string
	sys := &fakeSys{goTest: func(args []string) ([]byte, int) {
		runs = append(runs, args[len(args)-1])
		if args[len(args)-1] == "./app" {
			return []byte("--- PASS: TestA (0.00s)\n--- FAIL: TestB (0.00s)\nFAIL\n"), 1
		}
		return []byte("--- PASS: TestX (0.00s)\nok\n"), 0
	}}
	o := sys.opts(t, m, l, lb, nil)
	o.Cells = []string{"context/claude-code-cli/ubuntu-24.04-amd64"}
	ev, _ := Run(context.Background(), o)
	c := ev[0].Component
	if len(runs) != 3 || c == nil || c.Result != Fail || c.Passed != 3 || c.Failed != 1 || ev[0].Verdict != Fail || strings.Count(c.Command, " && ") != 2 {
		t.Fatalf("runs %v, component %+v, verdict %s", runs, c, ev[0].Verdict)
	}
}

// Two component journeys that share their first selection and differ in
// `also` are different runs: the second must not be handed the first's
// cached result.
func TestComponentCacheKeyIncludesAlso(t *testing.T) {
	m, l, lb := shipped(t)
	a, b := "context/claude-code-cli/ubuntu-24.04-amd64", "context/codex-cli/ubuntu-24.04-amd64"
	for i := range m.Cells {
		switch m.Cells[i].ID {
		case a:
			m.Cells[i].ComponentJourney = &ComponentJourney{Package: "./x", Run: "TestShared"}
		case b:
			m.Cells[i].ComponentJourney = &ComponentJourney{Package: "./x", Run: "TestShared", Also: []TestSelection{{Package: "./y", Run: "TestOnlyHere"}}}
		}
	}
	var runs []string
	sys := &fakeSys{goTest: func(args []string) ([]byte, int) {
		runs = append(runs, args[len(args)-1])
		if args[len(args)-1] == "./y" {
			return []byte("--- FAIL: TestOnlyHere (0.00s)\nFAIL\n"), 1
		}
		return []byte("--- PASS: TestShared (0.00s)\nok\n"), 0
	}}
	o := sys.opts(t, m, l, lb, nil)
	o.Cells = []string{a, b}
	ev := byCell(mustRun(t, o))
	if strings.Join(runs, " ") != "./x ./x ./y" {
		t.Fatalf("go test ran for %v, want ./x, then ./x and ./y", runs)
	}
	ca, cb := ev[a].Component, ev[b].Component
	if ca == nil || cb == nil || ca.Result != Pass || cb.Result != Fail || strings.Contains(ca.Command, "./y") || !strings.Contains(cb.Command, "./y") {
		t.Fatalf("components: %+v %+v", ca, cb)
	}
	if ev[b].Verdict != Fail {
		t.Fatalf("the cell whose `also` selection failed is %s", ev[b].Verdict)
	}
	// The same journey twice is still run once.
	if componentKey(m.Cells[0].ComponentJourney) != componentKey(m.Cells[0].ComponentJourney) {
		t.Fatal("the key is not stable")
	}
}

func mustRun(t *testing.T, o RunOptions) []Evidence {
	t.Helper()
	ev, err := Run(context.Background(), o)
	if err != nil {
		t.Fatal(err)
	}
	return ev
}

// A pass whose journey left steps not run, or that ran on a runner that is
// not the target OS, is reported as a reduced pass everywhere: the report,
// the summary and the record's own `actual`. Only a pass with every step
// run on the target OS is a plain installed-tier pass.
func TestReducedPassIsNotAPlainPass(t *testing.T) {
	m, _, lb := shipped(t)
	full, steps, osOnly, both := m.Cells[0].ID, m.Cells[1].ID, m.Cells[2].ID, m.Cells[3].ID
	record := func(cell string, res InstalledResult, match, note string) []byte {
		var osID string
		for _, c := range m.Cells {
			if c.ID == cell {
				osID = c.OS
			}
		}
		e := Evidence{SchemaVersion: 1, Tier: TierInstalled, Cell: cell, Revision: strings.Repeat("d", 40), LockSHA256: Digest(lb),
			CandidateSHA256: Unverified, HostVersion: Unverified, OS: osID, OSObserved: "macOS 26.2 (build 25C56) darwin/arm64",
			Env: map[string]string{}, Command: "sh j", Expected: "x", Verdict: Pass, Installed: &res, OSMatch: match, OSNote: note}
		e.Actual = installedActual(e)
		if err := e.Validate(m); err != nil {
			t.Fatal(err)
		}
		b, _ := json.Marshal(e)
		return b
	}
	const note = "this runner is macOS 26.2 (build 25C56), not the target macOS 14 (macos-14-arm64): the cell ran on the same OS family and architecture only"
	inst := map[string][]byte{
		full:   record(full, installedResult(StepPass, StepPass), OSMatchExact, ""),
		steps:  record(steps, installedResult(StepPass, StepNotRun, StepPass, StepNotRun), OSMatchExact, ""),
		osOnly: record(osOnly, installedResult(StepPass), OSMatchFamily, note),
		both:   record(both, installedResult(StepPass, StepNotRun), OSMatchFamily, note),
	}

	got := map[string]CellReport{}
	for _, r := range Report(m, nil, inst) {
		got[r.Cell] = r
	}
	if r := got[full]; r.State != StateInstalledPass || r.Reduction().Reduced() {
		t.Errorf("full pass: %+v", r)
	}
	if r := got[steps]; r.State != StateInstalledReduced || strings.Join(r.NotRunSteps, ",") != "step-b,step-d" || r.OSNote != "" {
		t.Errorf("steps not run: %+v", r)
	}
	if r := got[osOnly]; r.State != StateInstalledReduced || len(r.NotRunSteps) != 0 || r.OSNote != note || !strings.Contains(r.OSObserved, "macOS 26.2") {
		t.Errorf("another OS release: %+v", r)
	}
	if r := got[both]; r.State != StateInstalledReduced || len(r.NotRunSteps) != 1 || r.OSNote == "" || !strings.Contains(r.Reduction().String(), "1 step(s) not run: step-b") {
		t.Errorf("both: %+v", r)
	}
	for _, r := range got {
		if r.Installed == Pass && r.State != StateInstalledPass && r.State != StateInstalledReduced {
			t.Errorf("%s: an installed-tier pass shown as %q", r.Cell, r.State)
		}
	}

	sum, err := BuildInstalledSummary(m, inst)
	if err != nil || sum.Summary.Pass != 4 || sum.Summary.PassReduced != 3 || sum.Summary.NotRun != 20 {
		t.Fatalf("%+v %v", sum.Summary, err)
	}
	cells := map[string]InstalledCell{}
	for _, c := range sum.Cells {
		cells[c.Cell] = c
	}
	if c := cells[full]; c.Reduced || len(c.NotRunSteps) != 0 || c.OSNote != "" || c.OSMatch != OSMatchExact {
		t.Errorf("full pass in the summary: %+v", c)
	}
	if c := cells[steps]; !c.Reduced || c.StepsNotRun != 2 || strings.Join(c.NotRunSteps, ",") != "step-b,step-d" {
		t.Errorf("steps not run in the summary: %+v", c)
	}
	if c := cells[osOnly]; !c.Reduced || c.OSMatch != OSMatchFamily || c.OSNote != note || c.OSObserved == "" {
		t.Errorf("another OS release in the summary: %+v", c)
	}
	// The summary still validates against the schema's cell shape, and the
	// new fields are additive: a reader of the old fields sees what it did.
	raw, _ := json.Marshal(sum)
	for _, want := range []string{`"pass":4`, `"pass_reduced":3`, `"reduced":true`, `"not_run_steps":["step-b","step-d"]`} {
		if !strings.Contains(string(raw), want) {
			t.Errorf("summary JSON lacks %s", want)
		}
	}

	// The record says so itself.
	var e Evidence
	_ = json.Unmarshal(inst[both], &e)
	if !strings.Contains(e.Actual, "installed tier pass (reduced)") || !strings.Contains(e.Actual, "step-b") || !strings.Contains(e.Actual, "not the target macOS 14") {
		t.Errorf("actual: %q", e.Actual)
	}
	_ = json.Unmarshal(inst[full], &e)
	if strings.Contains(e.Actual, "reduced") || !strings.Contains(e.Actual, "installed tier pass:") {
		t.Errorf("actual of a full pass: %q", e.Actual)
	}

	// Evidence written before the OS fields existed (no os_match) still
	// reports its steps not run.
	old := Evidence{Tier: TierInstalled, Installed: ptr(installedResult(StepPass, StepNotRun))}
	if r := old.Reduction(); !r.Reduced() || len(r.NotRunSteps) != 1 || r.OSNote != "" {
		t.Errorf("older evidence: %+v", r)
	}
	// A mismatch needs its note, and live evidence carries no os_match.
	for name, mutate := range map[string]func(e *Evidence){
		"family without a note": func(e *Evidence) { e.OSMatch, e.OSNote = OSMatchFamily, "" },
		"unknown value":         func(e *Evidence) { e.OSMatch = "close enough" },
		"live with os_match":    func(e *Evidence) { e.Tier, e.Installed, e.OSMatch = "", nil, OSMatchExact },
	} {
		var e Evidence
		_ = json.Unmarshal(inst[full], &e)
		mutate(&e)
		if e.Validate(m) == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}

// The runner's actual OS is compared with the cell's target. Only the
// target release is exact; another release or edition of the same family
// is named as such.
func TestOSTargetMatch(t *testing.T) {
	for _, c := range []struct {
		target string
		info   OSInfo
		match  string
		note   string
	}{
		{"macos-14-arm64", OSInfo{Name: "macOS", Version: "14.7.1", Build: "23H222"}, OSMatchExact, ""},
		{"macos-14-arm64", OSInfo{Name: "macOS", Version: "26.2", Build: "25C56"}, OSMatchFamily, "this runner is macOS 26.2 (build 25C56), not the target macOS 14"},
		{"macos-14-arm64", OSInfo{Name: "macOS", Version: "15.6"}, OSMatchFamily, "not the target macOS 14"},
		{"macos-14-arm64", OSInfo{}, OSMatchUnknown, "could not be observed"},
		{"windows-11-amd64", OSInfo{Name: "Windows 11 Professional", Version: "24H2", Build: "26100.4349"}, OSMatchExact, ""},
		{"windows-11-amd64", OSInfo{Name: "Windows Server 2025 Datacenter", Version: "24H2", Build: "26100.4349", Server: true}, OSMatchFamily, "this runner is Windows Server 2025 Datacenter 24H2 (build 26100.4349), not the target Windows 11"},
		{"windows-11-amd64", OSInfo{Name: "Windows 10 Professional", Version: "22H2", Build: "19045.5011"}, OSMatchFamily, "not the target Windows 11"},
		{"windows-11-amd64", OSInfo{Name: "Windows 11 Professional"}, OSMatchUnknown, "build could not be observed"},
		{"ubuntu-24.04-amd64", OSInfo{Name: "Ubuntu 24.04.3 LTS", Version: "24.04"}, OSMatchExact, ""},
		{"ubuntu-24.04-amd64", OSInfo{Name: "Ubuntu 26.04 LTS", Version: "26.04"}, OSMatchFamily, "not the target Ubuntu 24.04"},
		{"solaris-11-sparc", OSInfo{Name: "Solaris"}, OSMatchUnknown, "no rule"},
	} {
		match, note := OSTargetMatch(c.target, c.info)
		if match != c.match || !strings.Contains(note, c.note) || (match == OSMatchExact) != (note == "") {
			t.Errorf("%s on %+v: %s %q", c.target, c.info, match, note)
		}
	}
	if got := (OSInfo{Name: "Ubuntu 24.04.3 LTS", Version: "24.04"}).describe("linux", "amd64"); got != "Ubuntu 24.04.3 LTS linux/amd64" {
		t.Errorf("describe: %q", got)
	}
	if got := (OSInfo{}).describe("darwin", "arm64"); got != "darwin/arm64" {
		t.Errorf("describe of an unobserved OS: %q", got)
	}
}

// An installed-tier cell records what the runner actually is and, when
// that is not the target release, says so and is a reduced pass. Which
// cells run is unchanged: the platform decides (CurrentOS), as before.
func TestInstalledTierRecordsTheActualOS(t *testing.T) {
	m, l, lb := shipped(t)
	for name, c := range map[string]struct {
		goos, goarch string
		info         OSInfo
		suffix       string
		match        string
		observed     string
	}{
		"macOS 26 runner":         {"darwin", "arm64", OSInfo{Name: "macOS", Version: "26.2", Build: "25C56"}, "macos-14-arm64", OSMatchFamily, "macOS 26.2 (build 25C56) darwin/arm64"},
		"macOS 14 runner":         {"darwin", "arm64", OSInfo{Name: "macOS", Version: "14.7.1", Build: "23H222"}, "macos-14-arm64", OSMatchExact, "macOS 14.7.1 (build 23H222) darwin/arm64"},
		"Windows Server runner":   {"windows", "amd64", OSInfo{Name: "Windows Server 2025 Datacenter", Version: "24H2", Build: "26100.4349", Server: true}, "windows-11-amd64", OSMatchFamily, "Windows Server 2025 Datacenter 24H2 (build 26100.4349) windows/amd64"},
		"Windows 11 runner":       {"windows", "amd64", OSInfo{Name: "Windows 11 Enterprise", Version: "24H2", Build: "26100.4349"}, "windows-11-amd64", OSMatchExact, "Windows 11 Enterprise 24H2 (build 26100.4349) windows/amd64"},
		"OS release not observed": {"darwin", "arm64", OSInfo{}, "macos-14-arm64", OSMatchUnknown, "darwin/arm64"},
	} {
		res := installedResult(StepPass, StepPass)
		o, _ := installedSys(t, m, l, lb, &res, 0)
		o.GOOS, o.GOARCH, o.OSRelease, o.OS = c.goos, c.goarch, "", &c.info
		ran := 0
		for _, e := range mustRun(t, o) {
			if err := e.Validate(m); err != nil {
				t.Fatalf("%s: %v", name, err)
			}
			if !strings.HasSuffix(e.Cell, c.suffix) {
				if e.Verdict != Blocked || e.OSMatch != "" {
					t.Errorf("%s: %s on another OS: %s, os_match %q", name, e.Cell, e.Verdict, e.OSMatch)
				}
				continue
			}
			ran++
			if e.Verdict != Pass || e.OSObserved != c.observed || e.OSMatch != c.match || (c.match == OSMatchExact) != (e.OSNote == "") {
				t.Errorf("%s: %s: %s, observed %q, match %q, note %q", name, e.Cell, e.Verdict, e.OSObserved, e.OSMatch, e.OSNote)
			}
			if red := e.Reduction(); red.Reduced() != (c.match != OSMatchExact) || strings.Contains(e.Actual, "reduced") != red.Reduced() {
				t.Errorf("%s: %s: reduction %+v, actual %q", name, e.Cell, red, e.Actual)
			}
			if !strings.Contains(e.Actual, "ran on "+c.observed) {
				t.Errorf("%s: actual does not name the runner's OS: %q", name, e.Actual)
			}
		}
		if ran != 8 {
			t.Errorf("%s: %d cells ran, want the 8 of %s", name, ran, c.suffix)
		}
	}

	// The live tier records the same observation and nothing else: no
	// os_match, and the same cells blocked for the same reasons as before.
	sys := &fakeSys{}
	o := sys.opts(t, m, l, lb, nil)
	o.GOOS, o.GOARCH, o.OSRelease = "darwin", "arm64", ""
	o.OS = &OSInfo{Name: "macOS", Version: "26.2", Build: "25C56"}
	for _, e := range mustRun(t, o) {
		if e.OSMatch != "" || e.OSNote != "" || e.Verdict == Pass || !strings.HasPrefix(e.OSObserved, "macOS 26.2") {
			t.Fatalf("live %s: %s, os_match %q, observed %q", e.Cell, e.Verdict, e.OSMatch, e.OSObserved)
		}
	}
}
