package textdiff

import (
	"fmt"
	"strings"
	"testing"
)

// apply applies a unified diff made by Unified to a, so that tests can
// check that the diff turns a into b.
func apply(t *testing.T, a []byte, diff string) string {
	t.Helper()
	old := splitLines(a)
	var out []string
	pos := 0
	var prev byte
	lines := strings.SplitAfter(diff, "\n")
	if len(lines) < 2 || !strings.HasPrefix(lines[0], "--- ") || !strings.HasPrefix(lines[1], "+++ ") {
		t.Fatalf("diff has no header:\n%s", diff)
	}
	for _, l := range lines[2:] {
		if l == "" {
			continue
		}
		switch l[0] {
		case '@':
			var as, ac, bs, bc int
			if _, err := fmt.Sscanf(l, "@@ -%d,%d +%d,%d @@", &as, &ac, &bs, &bc); err != nil {
				t.Fatalf("bad hunk header %q: %v", l, err)
			}
			start := as - 1
			if ac == 0 {
				start = as
			}
			for pos < start {
				out = append(out, old[pos])
				pos++
			}
		case ' ':
			if strings.TrimSuffix(old[pos], "\n") != strings.TrimSuffix(l[1:], "\n") {
				t.Fatalf("context %q does not match %q", l, old[pos])
			}
			out = append(out, old[pos])
			pos++
		case '-':
			if strings.TrimSuffix(old[pos], "\n") != strings.TrimSuffix(l[1:], "\n") {
				t.Fatalf("removed line %q does not match %q", l, old[pos])
			}
			pos++
		case '+':
			out = append(out, l[1:])
		case '\\':
			if prev == '+' {
				out[len(out)-1] = strings.TrimSuffix(out[len(out)-1], "\n")
			}
		default:
			t.Fatalf("unexpected diff line %q", l)
		}
		prev = l[0]
	}
	out = append(out, old[pos:]...)
	return strings.Join(out, "")
}

func TestUnifiedApplies(t *testing.T) {
	cases := []struct{ a, b string }{
		{"", "x\n"},
		{"x\n", ""},
		{"a\nb\nc\n", "a\nB\nc\n"},
		{"x", "y"},
		{"x", "x\n"},
		{"x\n", "x"},
		{"a\nb\nc\nd\ne\nf\ng\nh\ni\nj\nk\nl\n", "A\nb\nc\nd\ne\nf\ng\nh\ni\nj\nk\nL\n"},
		{"1\n2\n3\n", "0\n1\n2\n3\n4\n"},
		{"same\nsame\nsame\n", "same\nother\nsame\nsame\n"},
		{"a\nb\n", "c\nd\n"},
	}
	for _, c := range cases {
		for _, ctx := range []int{0, 1, 3} {
			d := Unified("a", "b", []byte(c.a), []byte(c.b), ctx)
			if got := apply(t, []byte(c.a), d); got != c.b {
				t.Errorf("context %d: diff of %q -> %q applied gives %q:\n%s", ctx, c.a, c.b, got, d)
			}
		}
	}
}

func TestUnifiedEqual(t *testing.T) {
	if d := Unified("a", "b", []byte("x\n"), []byte("x\n"), 3); d != "" {
		t.Errorf("equal texts gave %q", d)
	}
}

func TestUnifiedAddition(t *testing.T) {
	want := "--- old\n+++ new\n@@ -0,0 +1,1 @@\n+x\n"
	if d := Unified("old", "new", nil, []byte("x\n"), 3); d != want {
		t.Errorf("got %q, want %q", d, want)
	}
}

func TestUnifiedSeparateHunks(t *testing.T) {
	var a, b strings.Builder
	for i := range 40 {
		fmt.Fprintf(&a, "line %d\n", i)
		switch i {
		case 2, 30:
			fmt.Fprintf(&b, "changed %d\n", i)
		default:
			fmt.Fprintf(&b, "line %d\n", i)
		}
	}
	d := Unified("a", "b", []byte(a.String()), []byte(b.String()), 3)
	if n := strings.Count(d, "@@ -"); n != 2 {
		t.Errorf("want 2 hunks, got %d:\n%s", n, d)
	}
	if got := apply(t, []byte(a.String()), d); got != b.String() {
		t.Errorf("diff does not apply:\n%s", d)
	}
}

func TestBinary(t *testing.T) {
	if !Binary([]byte{'a', 0, 'b'}) || Binary([]byte("text")) {
		t.Error("Binary misjudged its input")
	}
	d := Unified("a", "b", []byte{0, 1}, []byte{0, 2}, 3)
	if d != "Binary files a and b differ\n" {
		t.Errorf("got %q", d)
	}
}
