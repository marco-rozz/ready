package tools

import (
	"fmt"
	"strings"
)

// unifiedDiff renders a redacted unified diff between the staged base and
// the new staged state (both already redacted — no classified value can
// reach it). It is a minimal LCS line diff with @@ hunk headers; hunk
// context of 2 lines keeps the output compact.
func unifiedDiff(base, next []byte) string {
	baseLines := splitLines(base)
	nextLines := splitLines(next)
	ops := lcsOps(baseLines, nextLines)

	const context = 2
	var b strings.Builder
	b.WriteString("--- config_manager/staged_base\n")
	b.WriteString("+++ config_manager/staged\n")

	// Group ops into hunks with leading/trailing context.
	i := 0
	for i < len(ops) {
		// Find the next changed op.
		if ops[i].kind == opEqual {
			i++

			continue
		}
		// Hunk start: include up to `context` equal lines before.
		start := i
		keptBefore := 0
		for start > 0 && ops[start-1].kind == opEqual && keptBefore < context {
			start--
			keptBefore++
		}
		// Extend through trailing changes and merges.
		// Merge change islands separated by ≤ 2×context equal lines.
		end := i + 1
		lastChange := i
		for end < len(ops) {
			if ops[end].kind != opEqual {
				lastChange = end
				end++

				continue
			}
			runEnd := end
			for runEnd < len(ops) && ops[runEnd].kind == opEqual {
				runEnd++
			}
			if runEnd == len(ops) || runEnd-end > context*2 {
				break
			}
			lastChange = runEnd - 1
			end = runEnd
		}
		// Keep at most `context` trailing equal lines after the last change.
		trimmed := lastChange + 1 + context
		if trimmed > end {
			trimmed = end
		}
		if trimmed <= i {
			trimmed = i + 1
		}
		end = trimmed

		baseFrom := hunkBaseStart(ops, start)
		nextFrom := hunkNextStart(ops, start)
		baseCount, nextCount := 0, 0
		var body []string
		for k := start; k < end; k++ {
			switch ops[k].kind {
			case opEqual:
				body = append(body, " "+
					strings.TrimSuffix(ops[k].text, "\n"))
				baseCount++
				nextCount++
			case opDelete:
				body = append(body, "-"+
					strings.TrimSuffix(ops[k].text, "\n"))
				baseCount++
			case opInsert:
				body = append(body, "+"+
					strings.TrimSuffix(ops[k].text, "\n"))
				nextCount++
			}
		}
		fmt.Fprintf(&b, "@@ -%d,%d +%d,%d @@\n", baseFrom, baseCount, nextFrom, nextCount)
		for _, line := range body {
			b.WriteString(line)
			b.WriteByte('\n')
		}
		i = end
	}
	if noDiffBody(ops) {
		return ""
	}

	return b.String()
}

func noDiffBody(ops []diffOp) bool {
	for _, op := range ops {
		if op.kind != opEqual {
			return false
		}
	}

	return true
}

func hunkBaseStart(ops []diffOp, start int) int {
	from := 1
	for k := 0; k < start; k++ {
		if ops[k].kind != opInsert {
			from++
		}
	}

	return from
}

func hunkNextStart(ops []diffOp, start int) int {
	from := 1
	for k := 0; k < start; k++ {
		if ops[k].kind != opDelete {
			from++
		}
	}

	return from
}

func splitLines(data []byte) []string {
	s := string(data)
	if s == "" {
		return nil
	}
	lines := strings.Split(s, "\n")
	if lines[len(lines)-1] == "" {
		lines = lines[:len(lines)-1]
	}
	// Re-attach the newline so ops carry exact line text.
	for i := range lines {
		lines[i] += "\n"
	}

	return lines
}

type diffOpKind int

const (
	opEqual diffOpKind = iota
	opDelete
	opInsert
)

type diffOp struct {
	kind diffOpKind
	text string
}

// lcsOps computes the line-level diff via LCS dynamic programming. Config
// files are small; the O(n·m) table is acceptable.
func lcsOps(a, b []string) []diffOp {
	type cell struct{ len, dir int } // dir: 0 diag, 1 up, 2 left
	n, m := len(a), len(b)
	table := make([][]cell, n+1)
	for i := range table {
		table[i] = make([]cell, m+1)
	}
	for i := 1; i <= n; i++ {
		for j := 1; j <= m; j++ {
			switch {
			case a[i-1] == b[j-1]:
				table[i][j] = cell{table[i-1][j-1].len + 1, 0}
			case table[i-1][j].len >= table[i][j-1].len:
				table[i][j] = cell{table[i-1][j].len, 1}
			default:
				table[i][j] = cell{table[i][j-1].len, 2}
			}
		}
	}

	// Walk back from (n, m) collecting ops.
	ops := make([]diffOp, 0, n+m)
	i, j := n, m
	for i > 0 || j > 0 {
		switch {
		case i > 0 && j > 0 && table[i][j].dir == 0:
			ops = append(ops, diffOp{opEqual, a[i-1]})
			i--
			j--
		case i > 0 && (j == 0 || table[i][j].dir == 1):
			ops = append(ops, diffOp{opDelete, a[i-1]})
			i--
		default:
			ops = append(ops, diffOp{opInsert, b[j-1]})
			j--
		}
	}
	// Reverse in place.
	for l, r := 0, len(ops)-1; l < r; l, r = l+1, r-1 {
		ops[l], ops[r] = ops[r], ops[l]
	}

	return ops
}
