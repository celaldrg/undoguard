//go:build unix

package cli

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/celaldrg/undoguard/internal/journal"
)

// world is a throwaway home directory with a git project in it, and the
// undoguard store, the configuration and the agent directories next to it.
// The agent directories are not created.
type world struct {
	base, home, proj, data, conf, claude, codex string
}

func newWorld(t *testing.T) *world {
	t.Helper()
	base := t.TempDir()
	if r, err := filepath.EvalSymlinks(base); err == nil {
		base = r
	}
	w := &world{
		base:   base,
		home:   filepath.Join(base, "home"),
		data:   filepath.Join(base, "data"),
		conf:   filepath.Join(base, "conf"),
		claude: filepath.Join(base, "claude"),
		codex:  filepath.Join(base, "codex"),
	}
	w.proj = filepath.Join(w.home, "project")
	t.Setenv("HOME", w.home)
	t.Setenv("UNDOGUARD_HOME", w.data)
	t.Setenv("UNDOGUARD_CONFIG_DIR", w.conf)
	t.Setenv("CLAUDE_CONFIG_DIR", w.claude)
	t.Setenv("CODEX_HOME", w.codex)
	for _, k := range []string{
		"UNDOGUARD_PROJECT", "CLAUDE_PROJECT_DIR", "GEMINI_PROJECT_DIR", "XDG_DATA_HOME", "XDG_CONFIG_HOME",
		envSession, envAgent, envActive,
	} {
		t.Setenv(k, "")
	}
	for _, d := range []string{
		w.conf,
		filepath.Join(w.proj, ".git"),
		filepath.Join(w.proj, "build", "out"),
		filepath.Join(w.proj, "src"),
		filepath.Join(w.home, "important"),
	} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	for _, f := range []string{
		filepath.Join(w.proj, "README.md"),
		filepath.Join(w.proj, "src", "main.go"),
		filepath.Join(w.proj, "build", "out", "app"),
		filepath.Join(w.home, "important", "thesis.txt"),
		filepath.Join(base, "elsewhere.txt"),
	} {
		writeFile(t, f, "data\n")
	}
	t.Chdir(w.proj)
	return w
}

// wantThesis reports when the file that the denied commands aim at is gone.
func (w *world) wantThesis(t *testing.T) {
	t.Helper()
	if _, err := os.Stat(filepath.Join(w.home, "important", "thesis.txt")); err != nil {
		t.Errorf("the file in the home directory is gone: %v", err)
	}
}

// cliResult is what one run of the command line gave.
type cliResult struct {
	code   int
	stdout string
	stderr string
}

// runCLI runs the command line with args and the given standard input.
func runCLI(t *testing.T, stdin string, args ...string) cliResult {
	t.Helper()
	var stdout, stderr bytes.Buffer
	code := Main(append([]string{"undoguard"}, args...), strings.NewReader(stdin), &stdout, &stderr)
	return cliResult{code: code, stdout: stdout.String(), stderr: stderr.String()}
}

// wantCode reports a wrong exit code, and whether the code was right.
func (r cliResult) wantCode(t *testing.T, code int) bool {
	t.Helper()
	if r.code != code {
		t.Errorf("exit code %d, want %d\nstdout:\n%s\nstderr:\n%s", r.code, code, r.stdout, r.stderr)
		return false
	}
	return true
}

// mustCode stops the test on a wrong exit code.
func (r cliResult) mustCode(t *testing.T, code int) {
	t.Helper()
	if r.code != code {
		t.Fatalf("exit code %d, want %d\nstdout:\n%s\nstderr:\n%s", r.code, code, r.stdout, r.stderr)
	}
}

// wantContains reports each of subs that s lacks.
func wantContains(t *testing.T, what, s string, subs ...string) {
	t.Helper()
	for _, sub := range subs {
		if !strings.Contains(s, sub) {
			t.Errorf("%s lacks %q:\n%s", what, sub, s)
		}
	}
}

func writeFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func readFile(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// hasRow reports whether a line of out holds exactly fields.
func hasRow(out string, fields ...string) bool {
	for _, line := range strings.Split(out, "\n") {
		if slices.Equal(strings.Fields(line), fields) {
			return true
		}
	}
	return false
}

// snapshots lists every snapshot, newest first.
func snapshots(t *testing.T) []logJSON {
	t.Helper()
	r := runCLI(t, "", "log", "--json", "-n", "0")
	r.mustCode(t, exitOK)
	var list []logJSON
	if err := json.Unmarshal([]byte(r.stdout), &list); err != nil {
		t.Fatalf("log --json: %v\n%s", err, r.stdout)
	}
	return list
}

// journalEntries lists the guard decisions and undos, newest first.
func journalEntries(t *testing.T) []journal.Entry {
	t.Helper()
	r := runCLI(t, "", "log", "--all", "--json", "-n", "0")
	r.mustCode(t, exitOK)
	var list []journal.Entry
	if err := json.Unmarshal([]byte(r.stdout), &list); err != nil {
		t.Fatalf("log --all --json: %v\n%s", err, r.stdout)
	}
	return list
}

// decodeCheck decodes the output of check --json.
func decodeCheck(t *testing.T, r cliResult) jsonDecision {
	t.Helper()
	var d jsonDecision
	if err := json.Unmarshal([]byte(r.stdout), &d); err != nil {
		t.Fatalf("check --json: %v\n%s", err, r.stdout)
	}
	return d
}

func TestCheckDecisions(t *testing.T) {
	w := newWorld(t)
	important := filepath.Join(w.home, "important")
	cases := []struct {
		args    []string
		code    int
		want    []string
		notWant []string
	}{
		{
			args: []string{"rm -rf build"},
			code: exitOK,
			want: []string{"Decision: allow\n", "\nSnapshot before running:\n  ~/project/build (recursive)\n"},
		},
		{
			args: []string{"--dir", filepath.Join(w.proj, "build"), "rm -rf out"},
			code: exitOK,
			want: []string{"Decision: allow\n", "\nSnapshot before running:\n  ~/project/build/out (recursive)\n"},
		},
		{
			args: []string{"rm " + shQuote(filepath.Join(w.base, "elsewhere.txt"))},
			code: exitAsk,
			want: []string{"Decision: ask\n"},
		},
		{
			args:    []string{"git push --force origin main"},
			code:    exitAsk,
			want:    []string{"Decision: ask\n", "\nNothing to snapshot.\n"},
			notWant: []string{"Snapshot before running:"},
		},
		{
			args:    []string{"rm -rf ~/important"},
			code:    exitDeny,
			want:    []string{"Decision: deny\n", "~/important"},
			notWant: []string{"Snapshot before running:", "Nothing to snapshot."},
		},
		{
			args: []string{"--", "rm", "-rf", important},
			code: exitDeny,
			want: []string{"Decision: deny\n"},
		},
		{
			args:    []string{"ls -la"},
			code:    exitOK,
			want:    []string{"Decision: allow\nReason:   nothing in this command line deletes or overwrites data\n"},
			notWant: []string{"Snapshot"},
		},
	}
	for _, c := range cases {
		r := runCLI(t, "", append([]string{"check"}, c.args...)...)
		if !r.wantCode(t, c.code) {
			continue
		}
		wantContains(t, strings.Join(c.args, " "), r.stdout, c.want...)
		for _, s := range c.notWant {
			if strings.Contains(r.stdout, s) {
				t.Errorf("%q: the output holds %q:\n%s", c.args, s, r.stdout)
			}
		}
	}
	w.wantThesis(t)
	if list := snapshots(t); len(list) != 0 {
		t.Errorf("check took %d snapshots", len(list))
	}
}

func TestCheckJSON(t *testing.T) {
	w := newWorld(t)
	r := runCLI(t, "", "check", "--json", "rm -rf ~/important")
	r.mustCode(t, exitDeny)
	d := decodeCheck(t, r)
	if d.Decision != "deny" || d.Command != "rm -rf ~/important" || d.Dir != w.proj {
		t.Errorf("got %+v", d)
	}
	if !strings.Contains(d.Reason, "~/important") {
		t.Errorf("the reason %q does not name the target", d.Reason)
	}
	if d.Snapshot == nil || len(d.Snapshot) > 0 {
		t.Errorf("a denied command lists the snapshot %#v", d.Snapshot)
	}
	found := false
	for _, f := range d.Findings {
		if f.Action == "deny" && f.Path == filepath.Join(w.home, "important") && f.Class == "home" {
			found = true
		}
	}
	if !found {
		t.Errorf("no deny finding for the home directory: %+v", d.Findings)
	}

	r = runCLI(t, "", "check", "--json", "rm -rf build")
	r.mustCode(t, exitOK)
	d = decodeCheck(t, r)
	if d.Decision != "allow" || !slices.Contains(d.Snapshot, filepath.Join(w.proj, "build")) {
		t.Errorf("got %+v", d)
	}
	w.wantThesis(t)
}

func TestCheckUsage(t *testing.T) {
	newWorld(t)
	r := runCLI(t, "", "check")
	r.wantCode(t, exitError)
	wantContains(t, "stderr", r.stderr, "Usage: undoguard check")
	r = runCLI(t, "", "check", "-h")
	r.wantCode(t, exitOK)
	wantContains(t, "stderr", r.stderr, "Usage: undoguard check")
}

func TestPolicyDefaults(t *testing.T) {
	w := newWorld(t)
	r := runCLI(t, "", "policy")
	r.mustCode(t, exitOK)
	wantContains(t, "policy", r.stdout,
		"User policy:    "+filepath.Join(w.conf, "policy.toml")+"\n",
		"Files read:     none, built-in defaults only\n",
		"Project:        ~/project\n",
		"Snapshot store: "+w.data+"\n",
		"Defaults:\n",
		"\nSnapshot limits:\n",
	)
	for _, row := range [][]string{
		{"project", "allow"},
		{"project", "root", "ask"},
		{"scratch", "allow"},
		{"home", "deny"},
		{"elsewhere", "ask"},
	} {
		if !hasRow(r.stdout, row...) {
			t.Errorf("no row %q in:\n%s", row, r.stdout)
		}
	}
	if strings.Contains(r.stdout, "\nRules:") {
		t.Errorf("rules listed without a policy file:\n%s", r.stdout)
	}
}

func TestPolicyUserRule(t *testing.T) {
	w := newWorld(t)
	file := filepath.Join(w.conf, "policy.toml")
	writeFile(t, file, "[[rule]]\nname = \"no force push\"\ncommand = \"git push *\"\naction = \"deny\"\n")
	r := runCLI(t, "", "policy")
	r.mustCode(t, exitOK)
	wantContains(t, "policy", r.stdout, "Files read:     "+file+"\n", "\nRules:\n", "no force push")

	r = runCLI(t, "", "check", "git push --force origin main")
	r.wantCode(t, exitDeny)
	wantContains(t, "check", r.stdout, "Decision: deny\n", "rule no force push")
}

func TestBrokenPolicy(t *testing.T) {
	w := newWorld(t)
	writeFile(t, filepath.Join(w.conf, "policy.toml"), "[defaults]\nhome = \"maybe\"\n")
	r := runCLI(t, "", "policy")
	r.wantCode(t, exitError)
	wantContains(t, "stderr", r.stderr, "unknown action")
	runCLI(t, "", "check", "ls").wantCode(t, exitError)

	// The hook does not fail open on a broken policy: it asks.
	h := decodeReply(t, runCLI(t, w.bashCall(t, "ls"), "hook", "claude")).HookSpecificOutput
	if h.PermissionDecision != "ask" || !strings.Contains(h.PermissionDecisionReason, "could not be loaded") {
		t.Errorf("got %+v", h)
	}
}

func TestVersionHelpAndUnknown(t *testing.T) {
	newWorld(t)
	for _, arg := range []string{"version", "--version", "-v"} {
		r := runCLI(t, "", arg)
		if !r.wantCode(t, exitOK) {
			continue
		}
		if !strings.HasPrefix(r.stdout, "undoguard ") || strings.Count(r.stdout, "\n") != 1 {
			t.Errorf("%s printed %q", arg, r.stdout)
		}
	}
	for _, arg := range []string{"help", "-h", "--help"} {
		r := runCLI(t, "", arg)
		r.wantCode(t, exitOK)
		wantContains(t, arg, r.stdout, "Usage: undoguard <command> [options]")
	}
	r := runCLI(t, "")
	r.wantCode(t, exitError)
	wantContains(t, "stderr", r.stderr, "Usage: undoguard <command> [options]")
	r = runCLI(t, "", "frobnicate")
	r.wantCode(t, exitError)
	wantContains(t, "stderr", r.stderr, `undoguard: unknown command "frobnicate"`)
}
