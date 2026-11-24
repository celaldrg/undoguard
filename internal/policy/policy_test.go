package policy

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestParseSize(t *testing.T) {
	good := map[string]int64{
		"":       0,
		"4096":   4096,
		"100k":   100 << 10,
		"512MiB": 512 << 20,
		"2GB":    2 << 30,
		"1t":     1 << 40,
		" 3 m ":  3 << 20,
	}
	for in, want := range good {
		got, err := ParseSize(in)
		if err != nil || got != want {
			t.Errorf("ParseSize(%q) = %d, %v; want %d", in, got, err, want)
		}
	}
	for _, in := range []string{"x", "5q", "-1", "1.5G", "99999999999t"} {
		if _, err := ParseSize(in); err == nil {
			t.Errorf("ParseSize(%q) succeeded", in)
		}
	}
}

func TestFormatSize(t *testing.T) {
	cases := map[int64]string{
		0:       "0 B",
		1023:    "1023 B",
		1024:    "1.0 KiB",
		1536:    "1.5 KiB",
		5 << 30: "5.0 GiB",
	}
	for in, want := range cases {
		if got := FormatSize(in); got != want {
			t.Errorf("FormatSize(%d) = %q, want %q", in, got, want)
		}
	}
}

func TestStricter(t *testing.T) {
	if Stricter(Allow, Ask) != Ask || Stricter(Deny, Ask) != Deny || Stricter(Allow, Allow) != Allow {
		t.Error("Stricter picked the wrong action")
	}
}

func TestGlobRegexp(t *testing.T) {
	cases := []struct {
		pat   string
		path  bool
		in    string
		match bool
	}{
		{"git push *", false, "git push --force origin main", true},
		{"git push *", false, "git pull", false},
		{"docker-*", false, "docker-volume-rm", true},
		{"/a/*", true, "/a/b", true},
		{"/a/*", true, "/a/b/c", false},
		{"/a/**", true, "/a", true},
		{"/a/**", true, "/a/b/c", true},
		{"/a/**", true, "/ab", false},
		{"/a/**/x", true, "/a/b/c/x", true},
		{"/a/b?", true, "/a/bc", true},
		{"/a/b?", true, "/a/b/", false},
		{"/a.b/c", true, "/aXb/c", false},
	}
	for _, c := range cases {
		re, err := globRegexp(c.pat, c.path)
		if err != nil {
			t.Fatal(err)
		}
		if got := re.MatchString(c.in); got != c.match {
			t.Errorf("%q (path %v) on %q = %v", c.pat, c.path, c.in, got)
		}
	}
}

// setHome points HOME and the undoguard directories into a temp dir.
func setHome(t *testing.T) (home, conf string) {
	t.Helper()
	base := t.TempDir()
	if r, err := filepath.EvalSymlinks(base); err == nil {
		base = r
	}
	home = filepath.Join(base, "home")
	conf = filepath.Join(base, "conf")
	for _, d := range []string{home, conf} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv("HOME", home)
	t.Setenv("UNDOGUARD_HOME", filepath.Join(base, "data"))
	t.Setenv("UNDOGUARD_CONFIG_DIR", conf)
	for _, k := range []string{"UNDOGUARD_PROJECT", "CLAUDE_PROJECT_DIR", "GEMINI_PROJECT_DIR"} {
		t.Setenv(k, "")
	}
	return home, conf
}

func load(t *testing.T, conf, content, cwd string) (*Policy, error) {
	t.Helper()
	file := filepath.Join(conf, "policy.toml")
	if err := os.WriteFile(file, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	return LoadFiles(file, cwd)
}

func TestLoadDefaults(t *testing.T) {
	home, conf := setHome(t)
	proj := filepath.Join(home, "p")
	if err := os.MkdirAll(proj, 0o755); err != nil {
		t.Fatal(err)
	}
	p, err := LoadFiles(filepath.Join(conf, "missing.toml"), proj)
	if err != nil {
		t.Fatal(err)
	}
	if p.Project != proj || p.Home != home {
		t.Errorf("project %q, home %q", p.Project, p.Home)
	}
	if p.Classes.Home != Deny || p.Classes.Project != Allow || p.Classes.ProjectRoot != Ask {
		t.Errorf("classes = %+v", p.Classes)
	}
	if p.Limits != DefaultLimits() {
		t.Errorf("limits = %+v", p.Limits)
	}
	for _, want := range []string{filepath.Join(home, ".ssh"), "/etc", conf} {
		found := false
		for _, pr := range p.Protected {
			found = found || pr == want
		}
		if !found {
			t.Errorf("%s is not protected: %v", want, p.Protected)
		}
	}
}

func TestProjectAtHomeIsIgnored(t *testing.T) {
	home, conf := setHome(t)
	p, err := LoadFiles(filepath.Join(conf, "missing.toml"), home)
	if err != nil {
		t.Fatal(err)
	}
	if p.Project != "" || len(p.Warnings) == 0 {
		t.Errorf("project %q, warnings %v", p.Project, p.Warnings)
	}
}

func TestLoadErrors(t *testing.T) {
	home, conf := setHome(t)
	for _, content := range []string{
		"this is not toml =",
		"[defaults]\nhome = \"maybe\"\n",
		"[[rule]]\naction = \"deny\"\n",
		"[[rule]]\ncommand = \"rm *\"\n",
		"[[rule]]\npath = \"relative/**\"\naction = \"deny\"\n",
		"[snapshot]\nmax_file_size = \"lots\"\n",
		"[snapshot]\nkeep_days = -1\n",
	} {
		if _, err := load(t, conf, content, home); err == nil {
			t.Errorf("loaded %q without error", content)
		}
	}
}

func TestUnknownKeysWarn(t *testing.T) {
	home, conf := setHome(t)
	p, err := load(t, conf, "colour = \"blue\"\n", home)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, w := range p.Warnings {
		found = found || strings.Contains(w, "colour")
	}
	if !found {
		t.Errorf("warnings %v do not mention the unknown key", p.Warnings)
	}
}

func TestUnprotected(t *testing.T) {
	home, conf := setHome(t)
	p, err := load(t, conf, "unprotected = [\"~/.config\"]\nprotected = [\"~/vault\"]\n", home)
	if err != nil {
		t.Fatal(err)
	}
	var hasConfig, hasVault bool
	for _, pr := range p.Protected {
		hasConfig = hasConfig || pr == filepath.Join(home, ".config")
		hasVault = hasVault || pr == filepath.Join(home, "vault")
	}
	if hasConfig || !hasVault {
		t.Errorf("protected = %v", p.Protected)
	}
}

func TestDecideRules(t *testing.T) {
	home, conf := setHome(t)
	p, err := load(t, conf, `
[[rule]]
name = "loose"
command = "rm *"
action = "allow"

[[rule]]
name = "strict"
command = "rm -rf *"
action = "ask"
`, home)
	if err != nil {
		t.Fatal(err)
	}
	act, by := p.Decide(Subject{Command: "rm -rf x"}, Deny, ClassHome)
	if act != Ask || by == nil || by.Name != "strict" {
		t.Errorf("got %s by %+v", act, by)
	}
	act, by = p.Decide(Subject{Command: "rm x"}, Deny, ClassHome)
	if act != Allow || by == nil || by.Name != "loose" {
		t.Errorf("got %s by %+v", act, by)
	}
	act, _ = p.Decide(Subject{Command: "rm x"}, Deny, ClassProtected)
	if act != Deny {
		t.Errorf("a rule loosened a protected path: %s", act)
	}
	act, by = p.Decide(Subject{Command: "ls"}, Allow, ClassProject)
	if act != Allow || by != nil {
		t.Errorf("no rule should match ls: %s by %+v", act, by)
	}
}
