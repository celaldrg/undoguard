// Package textdiff renders the difference between two texts as a unified
// diff. It finds a longest common subsequence of lines after trimming the
// common head and tail. When the remaining middle is too large for that,
// it reports the middle as replaced as a whole: still a correct diff,
// only not a minimal one.
package textdiff

import (
	"bytes"
	"fmt"
	"strings"
)

// maxCells bounds the table of the line matching, in cells.
const maxCells = 4_000_000

// Binary reports whether b looks like binary data.
func Binary(b []byte) bool {
	if len(b) > 8000 {
		b = b[:8000]
	}
	return bytes.IndexByte(b, 0) >= 0
}

// op is one line of the diff; kind is ' ', '-' or '+'.
type op struct {
	kind byte
	line string
}

// Unified returns the unified diff of a and b with context lines around
// each change, or "" when they are equal.
func Unified(aName, bName string, a, b []byte, context int) string {
	if bytes.Equal(a, b) {
		return ""
	}
	if Binary(a) || Binary(b) {
		return fmt.Sprintf("Binary files %s and %s differ\n", aName, bName)
	}
	ops := diffLines(splitLines(a), splitLines(b))
	var out strings.Builder
	fmt.Fprintf(&out, "--- %s\n+++ %s\n", aName, bName)
	for _, h := range hunks(ops, context) {
		writeHunk(&out, ops, h)
	}
	return out.String()
}

// splitLines splits after each newline. A last line without one keeps
// that difference, so that "x" and "x\n" compare unequal.
func splitLines(b []byte) []string {
	if len(b) == 0 {
		return nil
	}
	s := string(b)
	var out []string
	for len(s) > 0 {
		i := strings.IndexByte(s, '\n')
		if i < 0 {
			out = append(out, s)
			break
		}
		out = append(out, s[:i+1])
		s = s[i+1:]
	}
	return out
}

func diffLines(a, b []string) []op {
	head := 0
	for head < len(a) && head < len(b) && a[head] == b[head] {
		head++
	}
	tail := 0
	for tail < len(a)-head && tail < len(b)-head && a[len(a)-1-tail] == b[len(b)-1-tail] {
		tail++
	}
	var ops []op
	for _, l := range a[:head] {
		ops = append(ops, op{' ', l})
	}
	ops = append(ops, middle(a[head:len(a)-tail], b[head:len(b)-tail])...)
	for _, l := range a[len(a)-tail:] {
		ops = append(ops, op{' ', l})
	}
	return ops
}

// middle matches the lines of a and b by a longest common subsequence.
func middle(a, b []string) []op {
	n, m := len(a), len(b)
	var ops []op
	if n == 0 || m == 0 || (n+1)*(m+1) > maxCells {
		for _, l := range a {
			ops = append(ops, op{'-', l})
		}
		for _, l := range b {
			ops = append(ops, op{'+', l})
		}
		return ops
	}
	// lcs[i][j] is the length of the longest common subsequence of a[i:]
	// and b[j:], stored row by row.
	w := m + 1
	lcs := make([]int32, (n+1)*w)
	for i := n - 1; i >= 0; i-- {
		for j := m - 1; j >= 0; j-- {
			switch {
			case a[i] == b[j]:
				lcs[i*w+j] = lcs[(i+1)*w+j+1] + 1
			case lcs[(i+1)*w+j] >= lcs[i*w+j+1]:
				lcs[i*w+j] = lcs[(i+1)*w+j]
			default:
				lcs[i*w+j] = lcs[i*w+j+1]
			}
		}
	}
	i, j := 0, 0
	for i < n && j < m {
		switch {
		case a[i] == b[j]:
			ops = append(ops, op{' ', a[i]})
			i++
			j++
		case lcs[(i+1)*w+j] >= lcs[i*w+j+1]:
			ops = append(ops, op{'-', a[i]})
			i++
		default:
			ops = append(ops, op{'+', b[j]})
			j++
		}
	}
	for ; i < n; i++ {
		ops = append(ops, op{'-', a[i]})
	}
	for ; j < m; j++ {
		ops = append(ops, op{'+', b[j]})
	}
	return ops
}

// hunk is a range of ops to print.
type hunk struct{ from, to int }

// hunks groups the changes with their context; changes closer than twice
// the context share a hunk.
func hunks(ops []op, context int) []hunk {
	var out []hunk
	for i := 0; i < len(ops); {
		if ops[i].kind == ' ' {
			i++
			continue
		}
		from := max(i-context, 0)
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
				break
			}
			end = run
		}
		to := min(end+context, len(ops))
		if n := len(out); n > 0 && from <= out[n-1].to {
			out[n-1].to = to
		} else {
			out = append(out, hunk{from, to})
		}
		i = end
	}
	return out
}

func writeHunk(w *strings.Builder, ops []op, h hunk) {
	// Line numbers of the hunk's first line in a and in b.
	aLine, bLine := 1, 1
	for _, o := range ops[:h.from] {
		if o.kind != '+' {
			aLine++
		}
		if o.kind != '-' {
			bLine++
		}
	}
	aCount, bCount := 0, 0
	for _, o := range ops[h.from:h.to] {
		if o.kind != '+' {
			aCount++
		}
		if o.kind != '-' {
			bCount++
		}
	}
	// An empty side is numbered by the line before it.
	if aCount == 0 {
		aLine--
	}
	if bCount == 0 {
		bLine--
	}
	fmt.Fprintf(w, "@@ -%d,%d +%d,%d @@\n", aLine, aCount, bLine, bCount)
	for _, o := range ops[h.from:h.to] {
		w.WriteByte(o.kind)
		w.WriteString(o.line)
		if !strings.HasSuffix(o.line, "\n") {
			w.WriteString("\n\\ No newline at end of file\n")
		}
	}
}
