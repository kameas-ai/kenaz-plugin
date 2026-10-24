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
)

type multi []string

func (m *multi) String() string     { return strings.Join(*m, ",") }
func (m *multi) Set(v string) error { *m = append(*m, v); return nil }

func main() { os.Exit(run(os.Args[1:])) }

func run(args []string) int {
	if len(args) == 0 {
		fmt.Fprintln(os.Stderr, "usage: conformance validate|run|verdict|check-verdict [flags]")
		return 2
	}
	fs := flag.NewFlagSet(args[0], flag.ContinueOnError)
	root := fs.String("root", ".", "module root")
	harness := fs.String("harness", "", "harness directory (default ROOT/test/conformance)")
	out := fs.String("out", "", "output directory or file")
	evidence := fs.String("evidence", "", "evidence directory (verdict)")
	verdict := fs.String("verdict", "", "verdict file (check-verdict)")
	var cells, artifacts multi
	fs.Var(&cells, "cell", "cell id, or prefix ending in * (repeatable)")
	fs.Var(&artifacts, "artifact", "OS=PATH of a final candidate artifact (repeatable)")
	if err := fs.Parse(args[1:]); err != nil {
		return 2
	}
	if *harness == "" {
		*harness = filepath.Join(*root, "test", "conformance")
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
		}
		rev, dirty := revision(*root)
		osRelease, _ := os.ReadFile("/etc/os-release")
		ev, err := conformance.Run(context.Background(), conformance.RunOptions{
			Root: *root, HarnessDir: *harness, Matrix: m, Lock: lock, LockBytes: lockBytes,
			Cells: cells, OutDir: *out, Revision: rev, Dirty: dirty, OSRelease: string(osRelease),
		})
		if err != nil {
			fmt.Fprintln(os.Stderr, err.Error())
			return 1
		}
		n := map[string]int{}
		for _, e := range ev {
			n[e.Verdict]++
			fmt.Printf("%-8s %s\n", e.Verdict, e.Cell)
			for _, r := range e.Reasons {
				fmt.Printf("         [%s] %s\n", r.Gate, r.Reason)
			}
		}
		fmt.Printf("%d pass, %d fail, %d blocked; evidence in %s\n", n[conformance.Pass], n[conformance.Fail], n[conformance.Blocked], *out)
		if n[conformance.Fail] > 0 {
			return 1
		}
		return 0
	case "verdict":
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
		if err := conformance.CheckVerdict(raw, m, sums); err != nil {
			lines := strings.Split(err.Error(), "\n")
			sort.Strings(lines)
			fmt.Fprintln(os.Stderr, "publication refused:\n  "+strings.Join(lines, "\n  "))
			return 1
		}
		fmt.Println("verdict ok: every cell passed and the artifacts are the qualified candidate")
		return 0
	}
	fmt.Fprintln(os.Stderr, "unknown command "+args[0])
	return 2
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
