// Package diff renders line-based unified diffs for the preview.
package diff

import (
	"fmt"
	"strings"
)

const context = 3

// maxCells bounds the LCS table; larger inputs get a summary instead.
const maxCells = 4_000_000

// Unified returns a unified diff from a to b, or "" when they are equal.
func Unified(a, b, nameA, nameB string) string {
	if a == b {
		return ""
	}
	x, y := splitLines(a), splitLines(b)
	if len(x)*len(y) > maxCells {
		return fmt.Sprintf("--- %s\n+++ %s\n(%d → %d lines; too large to diff here)\n", nameA, nameB, len(x), len(y))
	}
	ops := script(x, y)

	var out strings.Builder
	fmt.Fprintf(&out, "--- %s\n+++ %s\n", nameA, nameB)
	for i := 0; i < len(ops); {
		if ops[i].kind == ' ' {
			i++
			continue
		}
		start := max(i-context, 0)
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
			if run == len(ops) || run-end > 2*context {
				end = min(end+context, len(ops))
				break
			}
			end = run
		}
		ax, ay := ops[start].ai, ops[start].bi
		var ca, cb int
		for _, o := range ops[start:end] {
			if o.kind != '+' {
				ca++
			}
			if o.kind != '-' {
				cb++
			}
		}
		fmt.Fprintf(&out, "@@ -%d,%d +%d,%d @@\n", ax+1, ca, ay+1, cb)
		for _, o := range ops[start:end] {
			out.WriteByte(o.kind)
			out.WriteString(o.text)
			out.WriteByte('\n')
		}
		i = end
	}
	return out.String()
}

type op struct {
	kind   byte // ' ', '-', '+'
	text   string
	ai, bi int // line index in a and b at this op
}

func script(x, y []string) []op {
	n, m := len(x), len(y)
	lcs := make([][]int32, n+1)
	for i := range lcs {
		lcs[i] = make([]int32, m+1)
	}
	for i := n - 1; i >= 0; i-- {
		for j := m - 1; j >= 0; j-- {
			if x[i] == y[j] {
				lcs[i][j] = lcs[i+1][j+1] + 1
			} else {
				lcs[i][j] = max(lcs[i+1][j], lcs[i][j+1])
			}
		}
	}
	var ops []op
	i, j := 0, 0
	for i < n || j < m {
		switch {
		case i < n && j < m && x[i] == y[j]:
			ops = append(ops, op{' ', x[i], i, j})
			i++
			j++
		case i < n && (j == m || lcs[i+1][j] >= lcs[i][j+1]):
			ops = append(ops, op{'-', x[i], i, j})
			i++
		default:
			ops = append(ops, op{'+', y[j], i, j})
			j++
		}
	}
	return ops
}

func splitLines(s string) []string {
	if s == "" {
		return nil
	}
	return strings.Split(strings.TrimSuffix(s, "\n"), "\n")
}
