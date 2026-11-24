//go:build unix

package parser_test

import (
	"os"
	"path/filepath"
	"slices"
	"testing"

	"github.com/celaldrg/undoguard/internal/parser"
)

// tempDir is a fresh directory, with symlinks resolved.
func tempDir(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	if r, err := filepath.EvalSymlinks(dir); err == nil {
		dir = r
	}
	return dir
}

// parseIn parses src as run in dir, with HOME set to /h.
func parseIn(t *testing.T, src, dir string) *parser.Result {
	t.Helper()
	res := parser.Parse(src, parser.Options{Dir: dir, Env: []string{"HOME=/h"}})
	if res == nil {
		t.Fatalf("Parse(%q) returned nil", src)
	}
	return res
}

func names(res *parser.Result) []string {
	var out []string
	for _, c := range res.Commands {
		out = append(out, c.Name)
	}
	return out
}

func TestSimpleCommand(t *testing.T) {
	dir := tempDir(t)
	res := parseIn(t, "rm -rf build", dir)
	if len(res.Issues) != 0 || len(res.Commands) != 1 {
		t.Fatalf("commands %q, issues %+v", names(res), res.Issues)
	}
	c := res.Commands[0]
	if c.Name != "rm" || !slices.Equal(c.ArgValues(), []string{"-rf", "build"}) {
		t.Errorf("got %s %q", c.Name, c.ArgValues())
	}
	if c.Dir != dir || !c.DirKnown || len(c.Via) != 0 {
		t.Errorf("runs in %q (known %v), via %q", c.Dir, c.DirKnown, c.Via)
	}

	res = parseIn(t, "rm $HOME/x", dir)
	if len(res.Commands) != 1 || !slices.Equal(res.Commands[0].ArgValues(), []string{"/h/x"}) {
		t.Errorf("$HOME/x: commands %q", names(res))
	} else if res.Commands[0].Args[0].Dynamic {
		t.Error("an expansion of a known variable is marked dynamic")
	}
}

func TestPipelineAndList(t *testing.T) {
	dir := tempDir(t)
	res := parseIn(t, "cat a | grep b && echo done", dir)
	if got := names(res); !slices.Equal(got, []string{"cat", "grep", "echo"}) {
		t.Fatalf("commands %q", got)
	}
	if in := res.Commands[1].Stdin; in == nil || in.From != res.Commands[0] {
		t.Errorf("grep does not read from cat: %+v", in)
	}

	if err := os.Mkdir(filepath.Join(dir, "sub"), 0o755); err != nil {
		t.Fatal(err)
	}
	res = parseIn(t, "cd sub && rm x", dir)
	if got := names(res); !slices.Equal(got, []string{"cd", "rm"}) {
		t.Fatalf("commands %q", got)
	}
	if rm := res.Commands[1]; rm.Dir != filepath.Join(dir, "sub") || !rm.DirKnown {
		t.Errorf("rm runs in %q (known %v)", rm.Dir, rm.DirKnown)
	}
}

func TestNestedShellAndSudo(t *testing.T) {
	dir := tempDir(t)
	res := parseIn(t, "bash -c 'rm -rf build'", dir)
	if got := names(res); !slices.Equal(got, []string{"bash", "rm"}) {
		t.Fatalf("commands %q", got)
	}
	if via := res.Commands[0].Via; len(via) != 0 {
		t.Errorf("bash found inside %q", via)
	}
	rm := res.Commands[1]
	if !slices.Equal(rm.Via, []string{"bash -c"}) || !slices.Equal(rm.ArgValues(), []string{"-rf", "build"}) {
		t.Errorf("rm: via %q, args %q", rm.Via, rm.ArgValues())
	}

	res = parseIn(t, "sudo -E rm -rf x", dir)
	if got := names(res); !slices.Equal(got, []string{"rm"}) {
		t.Fatalf("commands %q", got)
	}
	rm = res.Commands[0]
	if !slices.Equal(rm.Via, []string{"sudo"}) || !slices.Equal(rm.ArgValues(), []string{"-rf", "x"}) {
		t.Errorf("rm: via %q, args %q", rm.Via, rm.ArgValues())
	}
}

func TestQuote(t *testing.T) {
	for in, want := range map[string]string{
		"":      "''",
		"plain": "plain",
		"a b":   "'a b'",
		"it's":  `'it'\''s'`,
		"$HOME": "'$HOME'",
	} {
		if got := parser.Quote(in); got != want {
			t.Errorf("Quote(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestQuoteArgsRoundTrip(t *testing.T) {
	argv := []string{"printf", "a b", "it's", "$HOME", "*", "x;y"}
	line := parser.QuoteArgs(argv)
	res := parser.ParseArgv(argv, parser.Options{Dir: tempDir(t), Env: []string{"HOME=/h"}})
	if len(res.Issues) != 0 || len(res.Commands) != 1 {
		t.Fatalf("%s: commands %q, issues %+v", line, names(res), res.Issues)
	}
	c := res.Commands[0]
	if c.Name != "printf" || !slices.Equal(c.ArgValues(), argv[1:]) {
		t.Errorf("%s came back as %s %q", line, c.Name, c.ArgValues())
	}
}

func TestParseError(t *testing.T) {
	res := parseIn(t, "&& ls", tempDir(t))
	for _, is := range res.Issues {
		if is.Kind == "parse" && is.Detail != "" {
			return
		}
	}
	t.Errorf("no parse issue: %+v", res.Issues)
}
