package okf

import (
	"fmt"
	"strings"
)

// maxDiffCells bounds the LCS table; larger bodies get no line diff.
const maxDiffCells = 4_000_000

// MaxDiffBytes bounds the body diff of a write receipt.
const MaxDiffBytes = 4 << 10

type diffOp struct {
	kind byte // ' ', '-', '+'
	text string
}

// unifiedDiff renders a unified diff (2 lines of context) of two texts,
// bounded to limit bytes with a truncation note. ok is false when the inputs
// are too large to diff.
func unifiedDiff(oldText, newText string, limit int) (string, bool) {
	a, b := splitDiffLines(oldText), splitDiffLines(newText)
	if len(a)*len(b) > maxDiffCells {
		return "", false
	}
	ops := diffOps(a, b)

	const ctx = 2
	var sb strings.Builder
	oldLine, newLine := 1, 1
	idx := 0
	for idx < len(ops) {
		if ops[idx].kind == ' ' {
			oldLine++
			newLine++
			idx++

			continue
		}
		// hunk start with leading context
		start := idx
		for k := 0; k < ctx && start > 0 && ops[start-1].kind == ' '; k++ {
			start--
		}
		end := idx
		for end < len(ops) {
			if ops[end].kind != ' ' {
				end++

				continue
			}
			run := 0
			for run < len(ops)-end && ops[end+run].kind == ' ' {
				run++
			}
			if end+run >= len(ops) || run > 2*ctx {
				end += min(run, ctx)

				break
			}
			end += run
		}
		oldStart, newStart := oldLine-(idx-start), newLine-(idx-start)
		oc, nc := 0, 0
		for _, op := range ops[start:end] {
			if op.kind != '+' {
				oc++
			}
			if op.kind != '-' {
				nc++
			}
		}
		fmt.Fprintf(&sb, "@@ -%d,%d +%d,%d @@\n", oldStart, oc, newStart, nc)
		for _, op := range ops[start:end] {
			sb.WriteByte(op.kind)
			sb.WriteString(op.text)
			sb.WriteByte('\n')
		}
		for _, op := range ops[idx:end] {
			if op.kind != '+' {
				oldLine++
			}
			if op.kind != '-' {
				newLine++
			}
		}
		idx = end
	}
	out := sb.String()
	if len(out) > limit {
		cut := out[:limit]
		if nl := strings.LastIndexByte(cut, '\n'); nl > 0 {
			cut = cut[:nl+1]
		}
		out = cut + fmt.Sprintf("(diff truncated at %d bytes)\n", limit)
	}

	return out, true
}

func splitDiffLines(s string) []string {
	s = strings.ReplaceAll(s, "\r\n", "\n")
	s = strings.TrimRight(s, "\n")
	if s == "" {
		return nil
	}

	return strings.Split(s, "\n")
}

// diffOps computes the line edit script of a and b through an LCS table.
func diffOps(a, b []string) []diffOp {
	n, m := len(a), len(b)
	lcs := make([][]int, n+1)
	for i := range lcs {
		lcs[i] = make([]int, m+1)
	}
	for i := n - 1; i >= 0; i-- {
		for j := m - 1; j >= 0; j-- {
			if a[i] == b[j] {
				lcs[i][j] = lcs[i+1][j+1] + 1
			} else {
				lcs[i][j] = max(lcs[i+1][j], lcs[i][j+1])
			}
		}
	}
	var ops []diffOp
	i, j := 0, 0
	for i < n && j < m {
		switch {
		case a[i] == b[j]:
			ops = append(ops, diffOp{' ', a[i]})
			i++
			j++
		case lcs[i+1][j] >= lcs[i][j+1]:
			ops = append(ops, diffOp{'-', a[i]})
			i++
		default:
			ops = append(ops, diffOp{'+', b[j]})
			j++
		}
	}
	for ; i < n; i++ {
		ops = append(ops, diffOp{'-', a[i]})
	}
	for ; j < m; j++ {
		ops = append(ops, diffOp{'+', b[j]})
	}

	return ops
}
