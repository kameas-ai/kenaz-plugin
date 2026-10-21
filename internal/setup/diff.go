package setup

import (
	"fmt"
	"strings"
)

// unifiedDiff renders a unified diff (3 lines of context) of a → b.
func unifiedDiff(aName, bName string, a, b string) string {
	al, bl := diffLines(a), diffLines(b)
	ops := lcsOps(al, bl)
	if ops == nil {
		return ""
	}
	var out strings.Builder
	fmt.Fprintf(&out, "--- %s\n+++ %s\n", aName, bName)
	const ctx = 3
	for i := 0; i < len(ops); {
		if ops[i].kind == ' ' {
			i++
			continue
		}
		start := i
		for start > 0 && i-start < ctx && ops[start-1].kind == ' ' {
			start--
		}
		end := i
		for end < len(ops) {
			if ops[end].kind != ' ' {
				end++
				continue
			}
			run := end
			for run < len(ops) && ops[run].kind == ' ' {
				run++
			}
			if run == len(ops) || run-end > 2*ctx {
				end = min(end+ctx, len(ops))
				break
			}
			end = run
		}
		aStart, bStart, aN, bN := ops[start].a, ops[start].b, 0, 0
		for _, op := range ops[start:end] {
			if op.kind != '+' {
				aN++
			}
			if op.kind != '-' {
				bN++
			}
		}
		fmt.Fprintf(&out, "@@ -%s +%s @@\n", hunkRange(aStart, aN), hunkRange(bStart, bN))
		for _, op := range ops[start:end] {
			out.WriteByte(op.kind)
			out.WriteString(op.text)
			out.WriteByte('\n')
		}
		i = end
	}
	return out.String()
}

func hunkRange(start, n int) string {
	if n == 0 {
		return fmt.Sprintf("%d,0", start)
	}
	if n == 1 {
		return fmt.Sprintf("%d", start+1)
	}
	return fmt.Sprintf("%d,%d", start+1, n)
}

func diffLines(s string) []string {
	if s == "" {
		return nil
	}
	return strings.Split(strings.TrimSuffix(s, "\n"), "\n")
}

type diffOp struct {
	kind byte // ' ', '-', '+'
	text string
	a, b int // line index in a / b where this op sits
}

// lcsOps returns the edit script, or nil when a and b are equal.
func lcsOps(a, b []string) []diffOp {
	n, m := len(a), len(b)
	if n == m {
		eq := true
		for i := range a {
			if a[i] != b[i] {
				eq = false
				break
			}
		}
		if eq {
			return nil
		}
	}
	if n*m > 4_000_000 { // pathological: show a full replacement
		var ops []diffOp
		for i, l := range a {
			ops = append(ops, diffOp{'-', l, i, 0})
		}
		for j, l := range b {
			ops = append(ops, diffOp{'+', l, n, j})
		}
		return ops
	}
	dp := make([][]int, n+1)
	for i := range dp {
		dp[i] = make([]int, m+1)
	}
	for i := n - 1; i >= 0; i-- {
		for j := m - 1; j >= 0; j-- {
			if a[i] == b[j] {
				dp[i][j] = dp[i+1][j+1] + 1
			} else {
				dp[i][j] = max(dp[i+1][j], dp[i][j+1])
			}
		}
	}
	var ops []diffOp
	i, j := 0, 0
	for i < n || j < m {
		switch {
		case i < n && j < m && a[i] == b[j]:
			ops = append(ops, diffOp{' ', a[i], i, j})
			i++
			j++
		case i < n && (j == m || dp[i+1][j] >= dp[i][j+1]):
			ops = append(ops, diffOp{'-', a[i], i, j})
			i++
		default:
			ops = append(ops, diffOp{'+', b[j], i, j})
			j++
		}
	}
	return ops
}
