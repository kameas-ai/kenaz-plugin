// Command conformance runs the baseline conformance harness.
//
//	conformance validate                      check compatibility.json and matrix.json
//	conformance run [-out DIR] [-cell ID]...  run cells on this machine, write evidence
//	conformance verdict [-evidence DIR] [-out FILE]
//	                                          build the digest-bound release verdict
//	conformance check-verdict -verdict FILE -artifact OS=PATH...
//	                                          publication gate: verdict passes and binds
//	                                          exactly these candidate artifacts
//
// The credential-free installed tier (never a release verdict; see README.md):
//
//	conformance run -tier installed [-out DIR] [-cell ID]... [-min-pass N]
//	                                          run the installed-tier journeys for this OS
//	conformance verdict -tier installed [-evidence DIR] [-out FILE]
//	                                          summarize installed-tier evidence
//	conformance report [-evidence DIR] [-installed-evidence DIR]
//	                                          per cell: not run / installed-tier pass /
//	                                          installed-tier pass (reduced) / live pass
//	conformance journey -feature F -host H [-out FILE]
//	                                          one installed-tier journey (what the scripts
//	                                          in installed/ call)
//
// Paths default to the harness directory (test/conformance) of the
// module in the current directory.
package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"

	"github.com/kameas-ai/kenaz-plugin/test/conformance"
	"github.com/kameas-ai/kenaz-plugin/test/conformance/installed"
)

type multi []string

func (m *multi) String() string     { return strings.Join(*m, ",") }
func (m *multi) Set(v string) error { *m = append(*m, v); return nil }

func main() { os.Exit(run(os.Args[1:])) }

func run(args []string) int {
	if len(args) == 0 {
		fmt.Fprintln(os.Stderr, "usage: conformance validate|run|verdict|check-verdict|report|journey [flags]")
		return 2
	}
	fs := flag.NewFlagSet(args[0], flag.ContinueOnError)
	root := fs.String("root", ".", "module root")
	harness := fs.String("harness", "", "harness directory (default ROOT/test/conformance)")
	out := fs.String("out", "", "output directory or file")
	evidence := fs.String("evidence", "", "evidence directory (verdict)")
	verdict := fs.String("verdict", "", "verdict file (check-verdict)")
	tier := fs.String("tier", conformance.TierLive, "evidence tier: live or installed (run, verdict)")
	installedEvidence := fs.String("installed-evidence", "", "installed-tier evidence directory (report)")
	minPass := fs.Int("min-pass", 0, "run -tier installed: fail unless at least this many cells pass (8 on a baseline OS; guards against a runner on which every cell is blocked)")
	feature := fs.String("feature", "", "matrix feature id (journey)")
	host := fs.String("host", "", "matrix host id (journey)")
	var cells, artifacts multi
	fs.Var(&cells, "cell", "cell id, or prefix ending in * (repeatable)")
	fs.Var(&artifacts, "artifact", "OS=PATH of a final candidate artifact (repeatable)")
	if err := fs.Parse(args[1:]); err != nil {
		return 2
	}
	if *harness == "" {
		*harness = filepath.Join(*root, "test", "conformance")
	}
	if *tier != conformance.TierLive && *tier != conformance.TierInstalled {
		fmt.Fprintln(os.Stderr, "-tier wants live or installed")
		return 2
	}
	// The installed tier keeps its evidence apart from live evidence.
	installedDir := filepath.Join(*harness, "evidence", "installed")
	if args[0] == "journey" {
		return journey(*root, *feature, *host, *out)
	}
	m, err := conformance.LoadMatrix(filepath.Join(*harness, "matrix.json"))
	if err == nil {
		err = m.Validate()
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "matrix.json: "+err.Error())
		return 1
	}
	lock, lockBytes, err := conformance.LoadLock(filepath.Join(*harness, "compatibility.json"))
	if err != nil {
		fmt.Fprintln(os.Stderr, "compatibility.json: "+err.Error())
		return 1
	}
	unverified, lockErr := lock.Validate()

	switch args[0] {
	case "validate":
		if lockErr != nil {
			fmt.Fprintln(os.Stderr, "compatibility.json: "+lockErr.Error())
			return 1
		}
		fmt.Printf("matrix.json: ok (%d cells)\n", len(m.Cells))
		fmt.Printf("compatibility.json: ok, status %s, %d %s value(s)\n", lock.Status, len(unverified), conformance.Unverified)
		return 0
	case "run":
		if *out == "" {
			*out = filepath.Join(*harness, "evidence")
			if *tier == conformance.TierInstalled {
				*out = installedDir
			}
		}
		rev, dirty := revision(*root)
		osRelease, _ := os.ReadFile("/etc/os-release")
		ev, err := conformance.Run(context.Background(), conformance.RunOptions{
			Root: *root, HarnessDir: *harness, Matrix: m, Lock: lock, LockBytes: lockBytes,
			Cells: cells, OutDir: *out, Revision: rev, Dirty: dirty, OSRelease: string(osRelease), Tier: *tier,
		})
		if err != nil {
			fmt.Fprintln(os.Stderr, err.Error())
			return 1
		}
		n := map[string]int{}
		reduced := 0
		for _, e := range ev {
			n[e.Verdict]++
			red := e.Reduction()
			if e.Verdict == conformance.Pass && red.Reduced() {
				reduced++
				fmt.Printf("%-8s %s\n", "pass*", e.Cell)
				fmt.Printf("         [reduced] %s\n", red)
			} else {
				fmt.Printf("%-8s %s\n", e.Verdict, e.Cell)
			}
			for _, r := range e.Reasons {
				fmt.Printf("         [%s] %s\n", r.Gate, r.Reason)
			}
		}
		if *tier == conformance.TierInstalled {
			fmt.Printf("installed tier: %d pass (%d full, %d reduced*), %d fail, %d blocked; evidence in %s\n", n[conformance.Pass], n[conformance.Pass]-reduced, reduced, n[conformance.Fail], n[conformance.Blocked], *out)
			if reduced > 0 {
				fmt.Println("* A reduced pass left steps not run or ran on a runner that is not the cell's target OS: every step that ran passed, but it is not a full installed-tier pass.")
			}
			fmt.Println("An installed-tier pass is not a live pass and cannot satisfy the release gate.")
			if n[conformance.Pass] < *minPass {
				fmt.Fprintf(os.Stderr, "installed tier: %d cell(s) passed, want at least %d. A cell runs only on its own OS target (macOS arm64, Ubuntu 24.04 amd64, Windows amd64); on any other machine every cell is blocked.\n", n[conformance.Pass], *minPass)
				return 1
			}
		} else {
			fmt.Printf("%d pass, %d fail, %d blocked; evidence in %s\n", n[conformance.Pass], n[conformance.Fail], n[conformance.Blocked], *out)
		}
		if n[conformance.Fail] > 0 {
			return 1
		}
		return 0
	case "verdict":
		if *tier == conformance.TierInstalled {
			if *evidence == "" {
				*evidence = installedDir
			}
			if *out == "" {
				*out = filepath.Join(*evidence, "installed-summary.json")
			}
			sum, err := conformance.BuildInstalledSummary(m, readEvidence(m, *evidence))
			if err != nil {
				fmt.Fprintln(os.Stderr, "evidence: "+err.Error())
			}
			b, _ := jsonIndent(sum)
			if werr := os.WriteFile(*out, b, 0o644); werr != nil {
				fmt.Fprintln(os.Stderr, werr.Error())
				return 1
			}
			fmt.Printf("installed-tier summary %s: %d pass (%d full, %d reduced), %d fail, %d blocked, %d not run. Tier installed: not a release verdict, release_allowed=false.\n",
				*out, sum.Summary.Pass, sum.Summary.Pass-sum.Summary.PassReduced, sum.Summary.PassReduced, sum.Summary.Fail, sum.Summary.Blocked, sum.Summary.NotRun)
			for _, c := range sum.Cells {
				if c.Reduced {
					fmt.Printf("  reduced: %s: %s\n", c.Cell, conformance.Reduction{NotRunSteps: c.NotRunSteps, OSNote: c.OSNote})
				}
			}
			if err != nil || sum.Summary.Fail > 0 {
				return 1
			}
			return 0
		}
		if *evidence == "" {
			*evidence = filepath.Join(*harness, "evidence")
		}
		if *out == "" {
			*out = filepath.Join(*evidence, "verdict.json")
		}
		raw := map[string][]byte{}
		for _, c := range m.Cells {
			if b, err := os.ReadFile(filepath.Join(*evidence, conformance.EvidenceFile(c.ID))); err == nil {
				raw[c.ID] = b
			}
		}
		v, err := conformance.BuildVerdict(m, lock, lockBytes, raw)
		if err != nil {
			fmt.Fprintln(os.Stderr, "evidence: "+err.Error())
		}
		b, _ := jsonIndent(v)
		if werr := os.WriteFile(*out, b, 0o644); werr != nil {
			fmt.Fprintln(os.Stderr, werr.Error())
			return 1
		}
		fmt.Printf("verdict %s: release_allowed=%v (%d pass, %d fail, %d blocked, %d missing) sha256 %s\n",
			*out, v.ReleaseAllowed, v.Summary.Pass, v.Summary.Fail, v.Summary.Blocked, v.Summary.Missing, v.VerdictSHA256)
		if err != nil || !v.ReleaseAllowed {
			return 1
		}
		return 0
	case "check-verdict":
		raw, err := os.ReadFile(*verdict)
		if err != nil {
			fmt.Fprintln(os.Stderr, err.Error())
			return 1
		}
		sums := map[string]string{}
		for _, a := range artifacts {
			osID, path, ok := strings.Cut(a, "=")
			if !ok {
				fmt.Fprintln(os.Stderr, "-artifact wants OS=PATH")
				return 2
			}
			b, err := os.ReadFile(path)
			if err != nil {
				fmt.Fprintln(os.Stderr, err.Error())
				return 1
			}
			sums[osID] = conformance.Digest(b)
		}
		found := conformance.VerdictTier(raw)
		if err := conformance.CheckVerdict(raw, m, sums); err != nil {
			lines := strings.Split(err.Error(), "\n")
			sort.Strings(lines)
			fmt.Fprintln(os.Stderr, "publication refused (tier found: "+found+"):\n  "+strings.Join(lines, "\n  "))
			return 1
		}
		fmt.Println("verdict ok (tier found: " + found + "): every cell passed live and the artifacts are the qualified candidate")
		return 0
	case "report":
		if *evidence == "" {
			*evidence = filepath.Join(*harness, "evidence")
		}
		if *installedEvidence == "" {
			*installedEvidence = installedDir
		}
		n := map[string]int{}
		fmt.Printf("%-52s %-30s %-8s %-10s %s\n", "CELL", "STATE", "LIVE", "INSTALLED", "HOST VISIBILITY")
		for _, r := range conformance.Report(m, readEvidence(m, *evidence), readEvidence(m, *installedEvidence)) {
			n[r.State]++
			fmt.Printf("%-52s %-30s %-8s %-10s %s\n", r.Cell, r.State, r.Live, r.Installed, r.HostVisibility)
			if r.State == conformance.StateInstalledReduced {
				fmt.Printf("%-52s   reduced: %s\n", "", r.Reduction())
			}
		}
		fmt.Printf("%d live pass, %d installed-tier pass, %d installed-tier pass (reduced), %d not run. A reduced pass left steps not run or ran on a runner that is not the cell's target OS. Only a live pass of every cell allows a release.\n",
			n[conformance.StateLivePass], n[conformance.StateInstalledPass], n[conformance.StateInstalledReduced], n[conformance.StateNotRun])
		return 0
	}
	fmt.Fprintln(os.Stderr, "unknown command "+args[0])
	return 2
}

// readEvidence reads the evidence file of every matrix cell found in dir.
func readEvidence(m conformance.Matrix, dir string) map[string][]byte {
	raw := map[string][]byte{}
	for _, c := range m.Cells {
		if b, err := os.ReadFile(filepath.Join(dir, conformance.EvidenceFile(c.ID))); err == nil {
			raw[c.ID] = b
		}
	}
	return raw
}

// journey runs one installed-tier journey and writes its result to out
// (default: the file named by KENAZ_CONFORMANCE_STEPS, else stdout). It
// exits 1 when a step failed.
func journey(root, feature, host, out string) int {
	if feature == "" || host == "" {
		if f, h, ok := strings.Cut(strings.TrimSuffix(os.Getenv(conformance.CellEnv), "/"), "/"); ok && feature == "" && host == "" {
			feature = f
			host, _, _ = strings.Cut(h, "/")
		}
	}
	absRoot, err := filepath.Abs(root)
	if err != nil {
		fmt.Fprintln(os.Stderr, err.Error())
		return 1
	}
	res, err := installed.Run(context.Background(), installed.Options{
		Root: absRoot, Feature: feature, Host: host, Tags: os.Getenv(conformance.TagsEnv), Log: os.Stderr,
	})
	if err != nil {
		fmt.Fprintln(os.Stderr, "journey: "+err.Error())
		if len(res.Steps) == 0 {
			return 1
		}
	}
	b, _ := jsonIndent(res)
	if out == "" {
		out = os.Getenv(conformance.StepsEnv)
	}
	if out == "" {
		os.Stdout.Write(b)
	} else if werr := os.WriteFile(out, b, 0o644); werr != nil {
		fmt.Fprintln(os.Stderr, werr.Error())
		return 1
	}
	if _, failed, _ := res.Count(); failed > 0 || err != nil {
		return 1
	}
	return 0
}

// revision is the module's commit and whether the tree has changes.
func revision(root string) (string, bool) {
	out, err := exec.Command("git", "-C", root, "rev-parse", "HEAD").Output()
	if err != nil {
		return conformance.Unverified, true
	}
	st, err := exec.Command("git", "-C", root, "status", "--porcelain").Output()
	return strings.TrimSpace(string(out)), err != nil || len(strings.TrimSpace(string(st))) > 0
}
