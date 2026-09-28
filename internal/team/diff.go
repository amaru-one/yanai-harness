package team

import (
	"fmt"
	"strings"
)

// lineDiff renders a unified-style diff of two small texts (configuration
// files, the project state document). Unchanged runs longer than the context
// window are elided. It is a review aid, not a patch format.
func lineDiff(before, after, fromName, toName string) string {
	a := strings.Split(strings.TrimRight(before, "\n"), "\n")
	b := strings.Split(strings.TrimRight(after, "\n"), "\n")
	// Longest common subsequence table; these files are a few hundred lines.
	lcs := make([][]int, len(a)+1)
	for i := range lcs {
		lcs[i] = make([]int, len(b)+1)
	}
	for i := len(a) - 1; i >= 0; i-- {
		for j := len(b) - 1; j >= 0; j-- {
			if a[i] == b[j] {
				lcs[i][j] = lcs[i+1][j+1] + 1
			} else {
				lcs[i][j] = max(lcs[i+1][j], lcs[i][j+1])
			}
		}
	}
	type line struct {
		op   byte
		text string
	}
	var lines []line
	i, j := 0, 0
	for i < len(a) && j < len(b) {
		switch {
		case a[i] == b[j]:
			lines = append(lines, line{' ', a[i]})
			i++
			j++
		case lcs[i+1][j] >= lcs[i][j+1]:
			lines = append(lines, line{'-', a[i]})
			i++
		default:
			lines = append(lines, line{'+', b[j]})
			j++
		}
	}
	for ; i < len(a); i++ {
		lines = append(lines, line{'-', a[i]})
	}
	for ; j < len(b); j++ {
		lines = append(lines, line{'+', b[j]})
	}
	changed := false
	for _, l := range lines {
		if l.op != ' ' {
			changed = true
		}
	}
	if !changed {
		return "(no changes)\n"
	}
	const context = 3
	keep := make([]bool, len(lines))
	for k, l := range lines {
		if l.op == ' ' {
			continue
		}
		for d := -context; d <= context; d++ {
			if k+d >= 0 && k+d < len(lines) {
				keep[k+d] = true
			}
		}
	}
	var out strings.Builder
	fmt.Fprintf(&out, "--- %s\n+++ %s\n", fromName, toName)
	skipped := false
	for k, l := range lines {
		if !keep[k] {
			skipped = true
			continue
		}
		if skipped {
			out.WriteString("@@ …\n")
			skipped = false
		}
		out.WriteByte(l.op)
		out.WriteString(l.text)
		out.WriteByte('\n')
	}
	return out.String()
}
