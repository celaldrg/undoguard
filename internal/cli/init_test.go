//go:build unix

package cli

import (
	"encoding/json"
	"errors"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/celaldrg/undoguard/internal/hooks"
	"github.com/celaldrg/undoguard/internal/jsonedit"
)

// userSettings is a Claude Code settings file with keys of the user's own
// and a hook of another tool, laid out as jsonedit writes it.
const userSettings = `{
  "theme": "dark",
  "permissions": {
    "allow": [
      "Bash(ls:*)"
    ]
  },
  "hooks": {
    "PreToolUse": [
      {
        "matcher": "Write",
        "hooks": [
          {
            "type": "command",
            "command": "my-formatter"
          }
        ]
      }
    ]
  },
  "cleanupPeriodDays": 30
}
`

// oldInstall holds the hook of an earlier install at another path, in one
// group with the hook of another tool.
const oldInstall = `{
  "hooks": {
    "PreToolUse": [
      {
        "matcher": "Bash",
        "hooks": [
          {"type": "command", "command": "my-linter"},
          {"type": "command", "command": "/opt/undoguard/bin/undoguard hook claude", "timeout": 60}
        ]
      }
    ]
  }
}`

type settingsHook struct {
	Type    string `json:"type"`
	Command string `json:"command"`
	Timeout int    `json:"timeout"`
}

type settingsGroup struct {
	Matcher string         `json:"matcher"`
	Hooks   []settingsHook `json:"hooks"`
}

type settingsPermissions struct {
	Allow []string `json:"allow"`
}

// settingsFile is the part of an agent's settings file that the tests
// look at.
type settingsFile struct {
	Theme             string                     `json:"theme"`
	Permissions       settingsPermissions        `json:"permissions"`
	Hooks             map[string][]settingsGroup `json:"hooks"`
	CleanupPeriodDays int                        `json:"cleanupPeriodDays"`
}

func readSettings(t *testing.T, file string) settingsFile {
	t.Helper()
	var s settingsFile
	if err := json.Unmarshal([]byte(readFile(t, file)), &s); err != nil {
		t.Fatalf("%s: %v", file, err)
	}
	return s
}

func marshalSettings(t *testing.T, root *jsonedit.Object) settingsFile {
	t.Helper()
	b, err := jsonedit.Marshal(root, "  ")
	if err != nil {
		t.Fatal(err)
	}
	var s settingsFile
	if err := json.Unmarshal(b, &s); err != nil {
		t.Fatalf("%v\n%s", err, b)
	}
	return s
}

func parseObject(t *testing.T, src string) *jsonedit.Object {
	t.Helper()
	v, err := jsonedit.Parse([]byte(src))
	if err != nil {
		t.Fatal(err)
	}
	o, ok := v.(*jsonedit.Object)
	if !ok {
		t.Fatalf("%s does not hold an object", src)
	}
	return o
}

// backupsOf lists the backups init made of file.
func backupsOf(t *testing.T, file string) []string {
	t.Helper()
	list, err := filepath.Glob(file + ".undoguard-backup-*")
	if err != nil {
		t.Fatal(err)
	}
	return list
}

// eventGroups returns the n matcher groups of event.
func eventGroups(t *testing.T, s settingsFile, event string, n int) []settingsGroup {
	t.Helper()
	groups := s.Hooks[event]
	if len(groups) != n {
		t.Fatalf("%s holds %d groups, want %d: %+v", event, len(groups), n, groups)
	}
	return groups
}

// wantHook checks a matcher group that holds one hook.
func wantHook(t *testing.T, g settingsGroup, matcher, suffix string, timeout int) {
	t.Helper()
	if g.Matcher != matcher || len(g.Hooks) != 1 {
		t.Errorf("group %+v, want matcher %q and one hook", g, matcher)
		return
	}
	h := g.Hooks[0]
	if h.Type != "command" || !strings.HasSuffix(h.Command, suffix) || h.Timeout != timeout {
		t.Errorf("hook %+v, want a command ending in %q and timeout %d", h, suffix, timeout)
	}
}

// hookEntry is one entry of a matcher group.
func hookEntry(command string) *jsonedit.Object {
	h := jsonedit.NewObject()
	h.Set("type", "command")
	h.Set("command", command)
	return h
}

// wantNoDir reports when dir exists.
func wantNoDir(t *testing.T, dir string) {
	t.Helper()
	if _, err := os.Stat(dir); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("%s was made (%v)", dir, err)
	}
}

func wantMode(t *testing.T, file string, mode fs.FileMode) {
	t.Helper()
	info, err := os.Stat(file)
	if err != nil {
		t.Fatal(err)
	}
	if got := info.Mode().Perm(); got != mode {
		t.Errorf("%s has mode %v, want %v", file, got, mode)
	}
}

// TestInitFirstRun runs init once through Main. Later runs are tested on
// setUp: init takes the program path from os.Executable, which under go
// test is the test binary, and init only knows a hook as its own when that
// path holds "undoguard".
func TestInitFirstRun(t *testing.T) {
	w := newWorld(t)
	if err := os.MkdirAll(w.claude, 0o755); err != nil {
		t.Fatal(err)
	}
	file := filepath.Join(w.claude, "settings.json")
	writeFile(t, file, userSettings)
	if err := os.Chmod(file, 0o640); err != nil {
		t.Fatal(err)
	}

	r := runCLI(t, "", "init", "--dry-run")
	r.mustCode(t, exitOK)
	wantContains(t, "init --dry-run", r.stdout, "would change "+file, "hook claude-post")
	if text := readFile(t, file); text != userSettings {
		t.Errorf("a dry run changed the settings:\n%s", text)
	}
	if list := backupsOf(t, file); len(list) != 0 {
		t.Errorf("a dry run made the backups %q", list)
	}

	r = runCLI(t, "", "init")
	r.mustCode(t, exitOK)
	wantContains(t, "init", r.stdout, "added the hooks to "+file, "(backup: ")
	if n := strings.Count(r.stdout, "skipped: "); n != 2 {
		t.Errorf("%d agents skipped, want 2:\n%s", n, r.stdout)
	}
	wantNoDir(t, w.codex)
	wantNoDir(t, filepath.Join(w.home, ".gemini"))

	text := readFile(t, file)
	last := -1
	for _, key := range []string{`"theme"`, `"permissions"`, `"hooks"`, `"cleanupPeriodDays"`} {
		i := strings.Index(text, key)
		if i <= last {
			t.Errorf("the key %s is missing or out of order:\n%s", key, text)
		}
		last = i
	}
	s := readSettings(t, file)
	if s.Theme != "dark" || len(s.Permissions.Allow) != 1 || s.Permissions.Allow[0] != "Bash(ls:*)" ||
		s.CleanupPeriodDays != 30 {
		t.Errorf("the user's settings changed: %+v", s)
	}
	pre := eventGroups(t, s, "PreToolUse", 2)
	wantHook(t, pre[0], "Write", "my-formatter", 0)
	wantHook(t, pre[1], claudeTools, " hook claude", 300)
	wantHook(t, eventGroups(t, s, "PostToolUse", 1)[0], claudeTools, " hook claude-post", 120)

	backups := backupsOf(t, file)
	if len(backups) != 1 {
		t.Fatalf("backups %q, want one", backups)
	}
	if saved := readFile(t, backups[0]); saved != userSettings {
		t.Errorf("the backup holds:\n%s", saved)
	}
	wantMode(t, file, 0o640)
	wantMode(t, backups[0], 0o640)
}

func TestInitRemoveWithoutHooks(t *testing.T) {
	w := newWorld(t)
	if err := os.MkdirAll(w.claude, 0o755); err != nil {
		t.Fatal(err)
	}
	file := filepath.Join(w.claude, "settings.json")
	writeFile(t, file, userSettings)

	r := runCLI(t, "", "init", "--agent", "claude", "--remove")
	r.mustCode(t, exitOK)
	wantContains(t, "init --remove", r.stdout, "nothing to remove in "+file)
	if text := readFile(t, file); text != userSettings {
		t.Errorf("--remove changed the settings:\n%s", text)
	}
	if list := backupsOf(t, file); len(list) != 0 {
		t.Errorf("--remove made the backups %q", list)
	}

	r = runCLI(t, "", "init", "--agent", "codex", "--remove")
	r.mustCode(t, exitOK)
	wantContains(t, "init --remove", r.stdout, "nothing to remove: "+filepath.Join(w.codex, "hooks.json")+" does not exist")
	wantNoDir(t, w.codex)
}

func TestInitUnknownAgent(t *testing.T) {
	w := newWorld(t)
	r := runCLI(t, "", "init", "--agent", "claude,nope")
	r.wantCode(t, exitError)
	wantContains(t, "stderr", r.stderr, `unknown agent "nope"`)
	wantNoDir(t, w.claude)
}

func TestInitFreshFiles(t *testing.T) {
	w := newWorld(t)
	r := runCLI(t, "", "init", "--agent", "codex,gemini")
	r.mustCode(t, exitOK)
	codexFile := filepath.Join(w.codex, "hooks.json")
	geminiFile := filepath.Join(w.home, ".gemini", "settings.json")
	wantContains(t, "init", r.stdout, "added the hooks to "+codexFile+"\n", "added the hooks to ~/.gemini/settings.json\n")

	codexSettings := readSettings(t, codexFile)
	if len(codexSettings.Hooks) != 2 {
		t.Errorf("codex hooks %+v", codexSettings.Hooks)
	}
	wantHook(t, eventGroups(t, codexSettings, "PreToolUse", 1)[0], codexTools, " hook codex", 0)
	wantHook(t, eventGroups(t, codexSettings, "PostToolUse", 1)[0], codexTools, " hook codex-post", 0)

	geminiSettings := readSettings(t, geminiFile)
	if len(geminiSettings.Hooks) != 2 {
		t.Errorf("gemini hooks %+v", geminiSettings.Hooks)
	}
	wantHook(t, eventGroups(t, geminiSettings, "BeforeTool", 1)[0], geminiTools, " hook gemini", 300000)
	wantHook(t, eventGroups(t, geminiSettings, "AfterTool", 1)[0], geminiTools, " hook gemini-post", 120000)

	wantMode(t, codexFile, 0o600)
	wantMode(t, geminiFile, 0o600)
	wantNoDir(t, w.claude)
}

func TestSetUpIsIdempotentAndRemovable(t *testing.T) {
	w := newWorld(t)
	s := agentSetups()[0]
	file := filepath.Join(w.claude, "settings.json")
	if s.Agent != hooks.Claude || s.File != file {
		t.Fatalf("the first setup is %+v", s)
	}
	if err := os.MkdirAll(w.claude, 0o755); err != nil {
		t.Fatal(err)
	}
	writeFile(t, file, userSettings)

	// An installed program path, which has undoguard in it.
	self := filepath.Join(w.base, "bin", "undoguard")
	a := &app{stdin: strings.NewReader(""), stdout: io.Discard, stderr: io.Discard}
	step := func(remove bool, prefix string) string {
		t.Helper()
		msg, err := a.setUp(s, self, remove, false)
		if err != nil {
			t.Fatal(err)
		}
		if !strings.HasPrefix(msg, prefix) {
			t.Errorf("setUp said %q, want %q at the start", msg, prefix)
		}
		return readFile(t, file)
	}

	installed := step(false, "added the hooks to ")
	cmd := hookCommandPrefix(self)
	wantContains(t, "settings", installed, cmd+"claude\"", cmd+"claude-post\"")
	if again := step(false, "already set up in "); again != installed {
		t.Errorf("a second run changed the settings:\n%s", again)
	}
	if removed := step(true, "removed the hooks from "); removed != userSettings {
		t.Errorf("remove left:\n%s", removed)
	}
	if again := step(true, "nothing to remove in "); again != userSettings {
		t.Errorf("a second remove left:\n%s", again)
	}
}

func TestEditHooksReplacesOldInstall(t *testing.T) {
	specs := []hookSpec{
		{Event: "PreToolUse", Matcher: "Bash", Name: "claude", Timeout: 300},
		{Event: "PostToolUse", Matcher: "Bash", Name: "claude-post", Timeout: 120},
	}
	prefix := hookCommandPrefix("/usr/local/bin/undoguard")
	root := parseObject(t, oldInstall)
	if err := editHooks(root, specs, prefix, false); err != nil {
		t.Fatal(err)
	}
	s := marshalSettings(t, root)
	pre := eventGroups(t, s, "PreToolUse", 2)
	wantHook(t, pre[0], "Bash", "my-linter", 0)
	wantHook(t, pre[1], "Bash", prefix+"claude", 300)
	wantHook(t, eventGroups(t, s, "PostToolUse", 1)[0], "Bash", prefix+"claude-post", 120)

	if err := editHooks(root, specs, prefix, true); err != nil {
		t.Fatal(err)
	}
	s = marshalSettings(t, root)
	if len(s.Hooks) != 1 {
		t.Errorf("hooks after the removal: %+v", s.Hooks)
	}
	wantHook(t, eventGroups(t, s, "PreToolUse", 1)[0], "Bash", "my-linter", 0)
}

func TestEditHooksRejectsOddShapes(t *testing.T) {
	specs := []hookSpec{{Event: "PreToolUse", Matcher: "Bash", Name: "claude"}}
	prefix := hookCommandPrefix("/usr/local/bin/undoguard")
	for _, src := range []string{`{"hooks": []}`, `{"hooks": {"PreToolUse": {}}}`} {
		if err := editHooks(parseObject(t, src), specs, prefix, false); err == nil {
			t.Errorf("editHooks took %s", src)
		}
	}
	root := parseObject(t, `{"theme": "dark"}`)
	if err := editHooks(root, specs, prefix, true); err != nil {
		t.Fatal(err)
	}
	if _, ok := root.Get("hooks"); ok || root.Len() != 1 {
		t.Error("removing from settings without hooks changed them")
	}
}

func TestIsOurs(t *testing.T) {
	typeOnly := jsonedit.NewObject()
	typeOnly.Set("type", "command")
	for _, c := range []struct {
		h    any
		name string
		want bool
	}{
		{hookEntry("/usr/local/bin/undoguard hook claude"), "claude", true},
		{hookEntry("'/opt/my tools/undoguard' hook claude"), "claude", true},
		{hookEntry("/usr/local/bin/undoguard hook claude-post"), "claude", false},
		{hookEntry("/usr/local/bin/undoguard hook claude"), "claude-post", false},
		{hookEntry("my-formatter"), "claude", false},
		// A program path without undoguard in it, as under go test.
		{hookEntry("/tmp/go-build1/b001/cli.test hook claude"), "claude", false},
		{"/usr/local/bin/undoguard hook claude", "claude", false},
		{typeOnly, "claude", false},
	} {
		if got := isOurs(c.h, c.name); got != c.want {
			t.Errorf("isOurs(%v, %q) = %v, want %v", c.h, c.name, got, c.want)
		}
	}
}
