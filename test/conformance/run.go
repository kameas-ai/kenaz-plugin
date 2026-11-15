package conformance

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"time"
)

// HostBinaries maps each host to the executable an installed journey
// drives.
var HostBinaries = map[string]string{
	"claude-code-cli": "claude",
	"codex-cli":       "codex",
	"opencode":        "opencode",
}

// DevReadyEnv is set by the operator once an official build is signed in
// to the development Fleet with an entitled test identity (E01). The
// runner never reads or records credentials themselves.
const DevReadyEnv = "KENAZ_CONFORMANCE_DEV"

// RunOptions configures Run. Funcs left nil use the real system.
type RunOptions struct {
	Root       string // module root: component journeys run here
	HarnessDir string // directory holding matrix.json and journeys/
	Matrix     Matrix
	Lock       Lock
	LockBytes  []byte
	Cells      []string // cell ids or prefixes; empty means all
	OutDir     string
	Revision   string
	Dirty      bool
	// Tier selects what Run executes: "" or "live" for the live tier,
	// "installed" for the credential-free installed tier. The two write
	// different evidence and never share an output directory by default.
	Tier string

	GOOS, GOARCH string
	OSRelease    string // /etc/os-release content on Linux
	// OS is the operating system the runner actually is. Nil: observed
	// from the system (sw_vers, the registry, OSRelease).
	OS       *OSInfo
	LookPath func(string) (string, error)
	Exec     func(ctx context.Context, dir string, env []string, name string, args ...string) ([]byte, int, error)
	Getenv   func(string) string
	Now      func() time.Time
}

func (o *RunOptions) defaults() {
	if o.GOOS == "" {
		o.GOOS, o.GOARCH = runtime.GOOS, runtime.GOARCH
	}
	if o.Getenv == nil {
		o.Getenv = os.Getenv
	}
	if o.Now == nil {
		o.Now = time.Now
	}
}

// CurrentOS maps the running platform to a matrix OS id, or "" when this
// machine is not a baseline target. Linux must be Ubuntu 24.04.
//
// It maps by platform only: any macOS on arm64 is "macos-14-arm64" and any
// Windows on amd64 is "windows-11-amd64", whatever the release. That is
// what decides which cells a runner may run, in both tiers, and what the
// live gate accepts; it is deliberately left as it is here. What the runner
// actually is goes into the evidence (OSInfo, os_observed), and the
// installed tier states a mismatch with the target (OSTargetMatch).
func CurrentOS(goos, goarch, osRelease string) string {
	switch {
	case goos == "darwin" && goarch == "arm64":
		return "macos-14-arm64"
	case goos == "windows" && goarch == "amd64":
		return "windows-11-amd64"
	case goos == "linux" && goarch == "amd64":
		kv := parseOSRelease(osRelease)
		if kv["ID"] == "ubuntu" && kv["VERSION_ID"] == "24.04" {
			return "ubuntu-24.04-amd64"
		}
	}
	return ""
}

func parseOSRelease(s string) map[string]string {
	out := map[string]string{}
	sc := bufio.NewScanner(strings.NewReader(s))
	for sc.Scan() {
		k, v, ok := strings.Cut(sc.Text(), "=")
		if ok {
			out[k] = strings.Trim(v, `"`)
		}
	}
	return out
}

// Run executes the selected cells and writes one evidence file per cell
// into OutDir. It returns the evidence in matrix order.
func Run(ctx context.Context, o RunOptions) ([]Evidence, error) {
	o.defaults()
	if err := os.MkdirAll(o.OutDir, 0o755); err != nil {
		return nil, err
	}
	switch o.Tier {
	case "", TierLive:
	case TierInstalled:
		return o.runInstalledTier(ctx)
	default:
		return nil, fmt.Errorf("tier %q, want live|installed", o.Tier)
	}
	here := CurrentOS(o.GOOS, o.GOARCH, o.OSRelease)
	osObserved := o.osInfo(ctx).describe(o.GOOS, o.GOARCH)
	lockSum := Digest(o.LockBytes)
	_, lockErr := o.Lock.Validate()
	component := map[string]*ComponentResult{}
	observed := map[string]string{}
	var out []Evidence
	for _, c := range o.Matrix.Cells {
		if !selected(c.ID, o.Cells) {
			continue
		}
		start := o.Now().UTC()
		e := Evidence{
			SchemaVersion: 1, Cell: c.ID, Revision: o.Revision, RevisionDirty: o.Dirty,
			LockSHA256: lockSum, CandidateSHA256: o.Lock.CandidateDigest(c.OS),
			HostVersion: o.Lock.HostVersion(c.Host), OS: c.OS, OSObserved: osObserved,
			Env: o.envRecord(), Expected: expected(o.Matrix, c, o.Lock),
		}
		block := func(gate, f string, a ...any) { e.Reasons = append(e.Reasons, Reason{gate, fmt.Sprintf(f, a...)}) }

		if c.OS != here {
			block(GateConformance, "runs only on a %s runner; this machine is %s", c.OS, osObserved)
		} else {
			// Supporting component journey (recorded fixtures, fake Fleet).
			if j := c.ComponentJourney; j != nil {
				// Every selection is part of the key: two journeys with the
				// same first selection and different `also` lists are
				// different runs.
				key := componentKey(j)
				if component[key] == nil {
					component[key] = o.runComponent(ctx, j)
				}
				e.Component = component[key]
			}
			if lockErr != nil {
				block(GateConformance, "compatibility lock is invalid: %v", lockErr)
			}
			if !o.Lock.HostLocked(c.Host, c.OS) {
				block(GateConformance, "compatibility lock has no verified %s release and artifact digest for %s", c.Host, c.OS)
			}
			if e.CandidateSHA256 == Unverified {
				block(GateConformance, "no qualified candidate artifact digest for %s in the lock", c.OS)
			}
			bin := HostBinaries[c.Host]
			if _, ok := observed[c.Host]; !ok {
				observed[c.Host] = o.observeHost(ctx, bin)
			}
			if v := observed[c.Host]; v == "" {
				block(GateConformance, "%s is not installed on this runner (%s not on PATH)", c.Host, bin)
			} else {
				e.HostVersionObserved = v
			}
			if o.Getenv(DevReadyEnv) == "" {
				block(GateIdentity, "no development Fleet identity prepared (%s unset): sign-in, consent and Fleet-side receipt cannot be observed", DevReadyEnv)
			}
			script := filepath.Join(o.HarnessDir, filepath.FromSlash(c.InstalledJourney))
			if _, err := os.Stat(script); err != nil {
				block(GateConformance, "no recorded installed journey (%s)", c.InstalledJourney)
			}
			if r := e.Component; r != nil && r.Result != Pass {
				// A component failure is a real failure whatever the gates.
				e.Reasons = append([]Reason{{GateComponent, "component journey " + r.Result + ": " + r.Command}}, e.Reasons...)
			}
			if len(e.Reasons) == 0 {
				e.Command = "sh " + c.InstalledJourney
				if _, code, err := o.exec(ctx, o.HarnessDir, []string{"KENAZ_CONFORMANCE_CELL=" + c.ID}, "sh", c.InstalledJourney); err != nil || code != 0 {
					block(GateConformance, "installed journey exited %d", code)
					e.Verdict = Fail
				} else {
					e.Verdict = Pass
				}
			}
		}
		if e.Command == "" {
			e.Command = "none (not run)"
			if e.Component != nil {
				e.Command = e.Component.Command
			}
		}
		if e.Verdict == "" {
			e.Verdict = Blocked
			if r := e.Component; r != nil && r.Result == Fail {
				e.Verdict = Fail
			}
		}
		e.Actual = actual(e)
		e.StartedAt, e.FinishedAt = start.Format(time.RFC3339), o.Now().UTC().Format(time.RFC3339)
		b, err := json.MarshalIndent(e, "", "  ")
		if err != nil {
			return out, err
		}
		if err := os.WriteFile(filepath.Join(o.OutDir, EvidenceFile(c.ID)), append(b, '\n'), 0o644); err != nil {
			return out, err
		}
		out = append(out, e)
	}
	return out, nil
}

// componentKey identifies a component journey's whole run: every `go test`
// selection it makes, in order.
func componentKey(j *ComponentJourney) string {
	var parts []string
	for _, sel := range j.Selections() {
		parts = append(parts, sel.Package+" "+sel.Run)
	}
	return strings.Join(parts, "\x00")
}

// osInfo is the runner's OS: the caller's, or observed once.
func (o *RunOptions) osInfo(ctx context.Context) OSInfo {
	if o.OS == nil {
		info := o.observeOS(ctx)
		o.OS = &info
	}
	return *o.OS
}

// EvidenceFile is the evidence file name for a cell id.
func EvidenceFile(cell string) string { return strings.ReplaceAll(cell, "/", "__") + ".json" }

func selected(id string, filter []string) bool {
	if len(filter) == 0 {
		return true
	}
	for _, f := range filter {
		if id == f || strings.HasPrefix(id, strings.TrimSuffix(f, "*")) && strings.HasSuffix(f, "*") {
			return true
		}
	}
	return false
}

func expected(m Matrix, c Cell, l Lock) string {
	desc := c.Feature
	for _, f := range m.Features {
		if f.ID == c.Feature && f.Description != "" {
			desc = f.Description
		}
	}
	return fmt.Sprintf("installed %s %s on %s, official candidate %s, development Fleet: %s; Fleet-side and local outcomes observed",
		c.Host, l.HostVersion(c.Host), c.OS, l.PluginCandidate.Version, desc)
}

func actual(e Evidence) string {
	var parts []string
	switch e.Verdict {
	case Pass:
		parts = append(parts, "installed journey passed")
	case Fail:
		parts = append(parts, "failed")
	default:
		parts = append(parts, fmt.Sprintf("not run: %d blocking condition(s)", len(e.Reasons)))
	}
	if r := e.Component; r != nil {
		parts = append(parts, fmt.Sprintf("component journey %s (%d passed, %d failed)", r.Result, r.Passed, r.Failed))
	} else {
		parts = append(parts, "no component journey in this checkout")
	}
	if e.HostVersionObserved != "" {
		parts = append(parts, "host observed: "+e.HostVersionObserved)
	}
	return strings.Join(parts, "; ")
}

// envRecord is the allowlisted environment: names and safe values only.
func (o *RunOptions) envRecord() map[string]string {
	set := func(k string) string {
		if o.Getenv(k) == "" {
			return "unset"
		}
		return "set"
	}
	env := map[string]string{
		"GOOS": o.GOOS, "GOARCH": o.GOARCH, "go": runtime.Version(),
		"fleet":     "none (component journeys use in-process fake Fleet providers)",
		DevReadyEnv: set(DevReadyEnv),
	}
	if v := o.Getenv("KENAZ_ENV"); v == "prod" || v == "dev" {
		env["KENAZ_ENV"] = v
	}
	if o.Getenv(DevReadyEnv) != "" {
		env["fleet"] = "development Fleet (" + DevReadyEnv + " set)"
	}
	return env
}

var versionLineRe = regexp.MustCompile(`[0-9]+\.[0-9]+(\.[0-9]+)?[0-9A-Za-z.+-]*`)

// observeHost returns the host's self-reported version, or "" when it is
// not installed. Only the version token is kept.
func (o *RunOptions) observeHost(ctx context.Context, bin string) string {
	look := o.LookPath
	if look == nil {
		look = execLookPath
	}
	if _, err := look(bin); err != nil {
		return ""
	}
	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	out, code, err := o.exec(ctx, "", nil, bin, "--version")
	if err != nil || code != 0 {
		return "installed (version not reported)"
	}
	if v := versionLineRe.FindString(string(out)); v != "" {
		return bin + " " + v
	}
	return "installed (version not reported)"
}

var testLineRe = regexp.MustCompile(`(?m)^\s*--- (PASS|FAIL): `)

func (o *RunOptions) runComponent(ctx context.Context, j *ComponentJourney) *ComponentResult {
	r := &ComponentResult{Result: Pass}
	var cmds []string
	var all []byte
	// The worst selection decides: a failure beats a selection that could
	// not run or matched nothing, which beats a pass.
	rank := map[string]int{Pass: 0, "missing": 1, "not_run": 2, Fail: 3}
	for _, sel := range j.Selections() {
		args := []string{"test", "-count=1", "-v", "-run", sel.Run, sel.Package}
		cmds = append(cmds, "go "+strings.Join(quoteArgs(args), " "))
		cctx, cancel := context.WithTimeout(ctx, 10*time.Minute)
		out, code, err := o.exec(cctx, o.Root, nil, "go", args...)
		cancel()
		all = append(all, out...)
		passed, failed := 0, 0
		for _, m := range testLineRe.FindAllSubmatch(out, -1) {
			if string(m[1]) == "PASS" {
				passed++
			} else {
				failed++
			}
		}
		r.Passed, r.Failed = r.Passed+passed, r.Failed+failed
		res := Pass
		switch {
		case err != nil:
			res = "not_run"
		case code != 0 || failed > 0:
			res = Fail
		case passed == 0 || bytes.Contains(out, []byte("no tests to run")):
			res = "missing"
		}
		if rank[res] > rank[r.Result] {
			r.Result = res
		}
	}
	r.Command = strings.Join(cmds, " && ")
	r.OutputSHA256 = Digest(all)
	return r
}

func quoteArgs(a []string) []string {
	out := make([]string, len(a))
	for i, s := range a {
		if strings.ContainsAny(s, " |()^$*") {
			s = "'" + s + "'"
		}
		out[i] = s
	}
	return out
}

func (o *RunOptions) exec(ctx context.Context, dir string, env []string, name string, args ...string) ([]byte, int, error) {
	if o.Exec != nil {
		return o.Exec(ctx, dir, env, name, args...)
	}
	return execCommand(ctx, dir, env, name, args...)
}

// TagsEnv names the build tags an installed-tier journey builds kenaz with
// ("" or unset: the default build). The journey records them.
const TagsEnv = "KENAZ_CONFORMANCE_TAGS"

// StepsEnv is the file an installed-tier journey writes its InstalledResult
// to; CellEnv is the cell it runs for.
const (
	StepsEnv = "KENAZ_CONFORMANCE_STEPS"
	CellEnv  = "KENAZ_CONFORMANCE_CELL"
)

// runInstalledTier runs the credential-free installed-tier journey of every
// selected cell for this machine's OS and writes one installed-tier
// evidence record per cell. It never consults the dev identity and never
// runs a live journey; nothing it writes can pass a live cell.
func (o *RunOptions) runInstalledTier(ctx context.Context) ([]Evidence, error) {
	here := CurrentOS(o.GOOS, o.GOARCH, o.OSRelease)
	info := o.osInfo(ctx)
	osObserved := info.describe(o.GOOS, o.GOARCH)
	lockSum := Digest(o.LockBytes)
	var out []Evidence
	for _, c := range o.Matrix.Cells {
		if !selected(c.ID, o.Cells) {
			continue
		}
		start := o.Now().UTC()
		env := o.envRecord()
		delete(env, DevReadyEnv)
		env["fleet"] = "none (unofficial build: it cannot connect to Kameas Fleet)"
		env["model_credentials"] = "none (the journey strips them and makes no model call)"
		env[TagsEnv] = o.Getenv(TagsEnv)
		e := Evidence{
			SchemaVersion: 1, Tier: TierInstalled, Cell: c.ID, Revision: o.Revision, RevisionDirty: o.Dirty,
			LockSHA256: lockSum, CandidateSHA256: Unverified, HostVersion: Unverified,
			OS: c.OS, OSObserved: osObserved, Env: env,
			Expected: "installed tier (credential-free): this checkout's unofficial kenaz, set up for " + c.Host + " in throwaway directories on " + c.OS + ", registers as `kenaz setup doctor` expects, serves the feature's MCP tools and behaves as documented with no sign-in. Not a live pass.",
		}
		block := func(f string, a ...any) {
			e.Reasons = append(e.Reasons, Reason{GateInstalled, fmt.Sprintf(f, a...)})
		}
		switch {
		case c.OS != here:
			block("runs only on a %s runner; this machine is %s", c.OS, osObserved)
			e.Verdict, e.Command = Blocked, "none (not run)"
		case c.InstalledTierJourney == "":
			block("the matrix names no installed-tier journey for this cell")
			e.Verdict, e.Command = Blocked, "none (not run)"
		default:
			// The cell runs here because the platform is the target's
			// (CurrentOS). Whether the runner is the target release is
			// recorded, and makes a pass a reduced one when it is not.
			e.OSMatch, e.OSNote = OSTargetMatch(c.OS, info)
			e.Command = "sh " + c.InstalledTierJourney
			steps, err := os.CreateTemp("", "kenaz-conformance-steps-*.json")
			if err != nil {
				return out, err
			}
			steps.Close()
			jctx, cancel := context.WithTimeout(ctx, 15*time.Minute)
			_, code, runErr := o.exec(jctx, o.HarnessDir, []string{CellEnv + "=" + c.ID, StepsEnv + "=" + steps.Name()}, "sh", c.InstalledTierJourney)
			cancel()
			raw, readErr := os.ReadFile(steps.Name())
			_ = os.Remove(steps.Name())
			var res InstalledResult
			switch {
			case runErr != nil:
				block("the journey could not start: %v", runErr)
			case readErr != nil || len(raw) == 0:
				block("the journey exited %d and wrote no result", code)
			case decodeStrict(raw, &res) != nil:
				block("the journey's result is not a valid installed-tier result")
			default:
				e.Installed = &res
				e.HostVersionObserved = res.HostCLI.Version
				if err := res.Validate(); err != nil {
					block("invalid journey result: %v", err)
				}
				for _, s := range res.Steps {
					if s.Status == StepFail {
						block("step %s failed: %s", s.ID, s.Detail)
					}
				}
				if p, _, _ := res.Count(); p == 0 {
					block("no step passed")
				}
				if code != 0 && len(e.Reasons) == 0 {
					block("the journey exited %d", code)
				}
			}
			e.Verdict = Pass
			if len(e.Reasons) > 0 {
				e.Verdict = Fail
			}
		}
		e.Actual = installedActual(e)
		e.StartedAt, e.FinishedAt = start.Format(time.RFC3339), o.Now().UTC().Format(time.RFC3339)
		b, err := json.MarshalIndent(e, "", "  ")
		if err != nil {
			return out, err
		}
		if err := os.WriteFile(filepath.Join(o.OutDir, EvidenceFile(c.ID)), append(b, '\n'), 0o644); err != nil {
			return out, err
		}
		out = append(out, e)
	}
	return out, nil
}

func installedActual(e Evidence) string {
	r := e.Installed
	if r == nil {
		return fmt.Sprintf("installed tier not run: %d reason(s)", len(e.Reasons))
	}
	p, f, n := r.Count()
	verdict := e.Verdict
	red := e.Reduction()
	if verdict == Pass && red.Reduced() {
		verdict = "pass (reduced)"
	}
	s := fmt.Sprintf("installed tier %s: %d step(s) passed, %d failed, %d not run; unofficial build (tags %q); host CLI ", verdict, p, f, n, r.Build.Tags)
	if r.HostCLI.Present {
		s += "present, visibility " + r.HostCLI.Visibility
	} else {
		s += "absent, host visibility not run"
	}
	s += "; ran on " + e.OSObserved
	if red.Reduced() {
		s += ". Reduced: " + red.String()
	}
	return s + ". Not a live pass."
}
