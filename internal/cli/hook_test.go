//go:build unix

package cli

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/celaldrg/undoguard/internal/hooks"
	"github.com/celaldrg/undoguard/internal/journal"
)

// claudeReply is the answer of a Claude Code or Codex hook that runs
// before a tool.
type claudeReply struct {
	HookSpecificOutput struct {
		HookEventName            string `json:"hookEventName"`
		PermissionDecision       string `json:"permissionDecision"`
		PermissionDecisionReason string `json:"permissionDecisionReason"`
	} `json:"hookSpecificOutput"`
}

// hookInput is what an agent writes to a hook for one tool call.
func hookInput(t *testing.T, session, dir, tool string, toolInput map[string]any) string {
	t.Helper()
	b, err := json.Marshal(map[string]any{
		"session_id":      session,
		"cwd":             dir,
		"hook_event_name": "PreToolUse",
		"tool_name":       tool,
		"tool_input":      toolInput,
	})
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// decodeReply decodes the answer of a hook that blocked or asked.
func decodeReply(t *testing.T, r cliResult) claudeReply {
	t.Helper()
	r.mustCode(t, exitOK)
	var out claudeReply
	if err := json.Unmarshal([]byte(r.stdout), &out); err != nil {
		t.Fatalf("hook reply: %v\n%s", err, r.stdout)
	}
	return out
}

// bashCall is a Claude Code Bash call in the project.
func (w *world) bashCall(t *testing.T, cmd string) string {
	t.Helper()
	return hookInput(t, "s1", w.proj, "Bash", map[string]any{"command": cmd})
}

// agentRuns plays Claude Code running cmd: the hook before the tool, the
// tool itself, and the hook after it.
func (w *world) agentRuns(t *testing.T, cmd string, run func()) {
	t.Helper()
	in := w.bashCall(t, cmd)
	r := runCLI(t, in, "hook", "claude")
	r.mustCode(t, exitOK)
	if r.stdout != "" {
		t.Fatalf("the hook did not allow %q:\n%s", cmd, r.stdout)
	}
	run()
	r = runCLI(t, in, "hook", "claude-post")
	r.mustCode(t, exitOK)
	if r.stdout != "" {
		t.Errorf("the hook after the tool printed %q", r.stdout)
	}
}

func TestHookClaudeDecisions(t *testing.T) {
	w := newWorld(t)
	for _, c := range []struct {
		cmd, decision, reason string
	}{
		{"rm -rf ~/important", "deny", "~/important"},
		{"git push --force origin main", "ask", "git push"},
	} {
		h := decodeReply(t, runCLI(t, w.bashCall(t, c.cmd), "hook", "claude")).HookSpecificOutput
		if h.HookEventName != "PreToolUse" || h.PermissionDecision != c.decision ||
			!strings.HasPrefix(h.PermissionDecisionReason, "undoguard: ") ||
			!strings.Contains(h.PermissionDecisionReason, c.reason) {
			t.Errorf("%s: got %+v", c.cmd, h)
		}
	}
	w.wantThesis(t)
	if list := snapshots(t); len(list) != 0 {
		t.Errorf("the hook took %d snapshots", len(list))
	}
	entries := journalEntries(t)
	if len(entries) != 2 {
		t.Fatalf("journal: %+v", entries)
	}
	if entries[0].Event != journal.EventAsk || entries[1].Event != journal.EventDeny {
		t.Errorf("journal events %q, %q", entries[0].Event, entries[1].Event)
	}
	for _, e := range entries {
		if e.Agent != hooks.Claude || e.Session != "s1" || e.Dir != w.proj {
			t.Errorf("journal entry %+v", e)
		}
	}
}

func TestHookAllowIsSilent(t *testing.T) {
	w := newWorld(t)
	for _, in := range []string{
		w.bashCall(t, "ls -la"),
		hookInput(t, "s1", w.proj, "Write", map[string]any{"file_path": filepath.Join(w.proj, "a"), "content": "x"}),
	} {
		r := runCLI(t, in, "hook", "claude")
		if r.wantCode(t, exitOK) && r.stdout != "" {
			t.Errorf("an allowed call printed %q", r.stdout)
		}
	}
	if entries := journalEntries(t); len(entries) != 0 {
		t.Errorf("journal: %+v", entries)
	}
}

func TestHookErrors(t *testing.T) {
	newWorld(t)
	for _, c := range []struct {
		args   []string
		stdin  string
		stderr string
	}{
		{[]string{"hook"}, "", "Usage: undoguard hook"},
		{[]string{"hook", "nope"}, "{}", `undoguard: unknown hook "nope"`},
		{[]string{"hook", "claude"}, "not json", "undoguard: reading the claude hook input"},
	} {
		r := runCLI(t, c.stdin, c.args...)
		r.wantCode(t, exitError)
		wantContains(t, strings.Join(c.args, " "), r.stderr, c.stderr)
	}
}

func TestHookCodexAndGemini(t *testing.T) {
	w := newWorld(t)
	// Codex cannot ask its user from a hook, so an ask turns into a deny.
	in := hookInput(t, "c1", w.proj, "shell", map[string]any{"command": []string{"git", "push", "--force", "origin", "main"}})
	h := decodeReply(t, runCLI(t, in, "hook", "codex")).HookSpecificOutput
	if h.PermissionDecision != "deny" || !strings.HasPrefix(h.PermissionDecisionReason, "undoguard: ") ||
		!strings.HasSuffix(h.PermissionDecisionReason, hooks.AskHint) {
		t.Errorf("codex: got %+v", h)
	}

	in = hookInput(t, "g1", w.proj, "run_shell_command", map[string]any{"command": "rm -rf ~/important"})
	r := runCLI(t, in, "hook", "gemini")
	r.mustCode(t, exitOK)
	var g map[string]string
	if err := json.Unmarshal([]byte(r.stdout), &g); err != nil {
		t.Fatalf("gemini reply: %v\n%s", err, r.stdout)
	}
	if g["decision"] != "deny" || !strings.HasPrefix(g["reason"], "undoguard: ") || strings.HasSuffix(g["reason"], hooks.AskHint) {
		t.Errorf("gemini: got %v", g)
	}
	w.wantThesis(t)
}

func TestHookSnapshotThenUndo(t *testing.T) {
	w := newWorld(t)
	notes := filepath.Join(w.proj, "notes.md")
	writeFile(t, notes, "first line\nsecond line\n")
	w.agentRuns(t, "rm notes.md", func() {
		if err := os.Remove(notes); err != nil {
			t.Fatal(err)
		}
	})

	list := snapshots(t)
	if len(list) != 1 {
		t.Fatalf("snapshots: %+v", list)
	}
	s := list[0]
	if s.Status != "ready" || s.Agent != hooks.Claude || s.Session != "s1" || s.Command != "rm notes.md" ||
		s.Dir != w.proj || s.Paths != 1 || s.Files != 1 {
		t.Errorf("snapshot %+v", s)
	}

	r := runCLI(t, "", "diff", s.ID)
	r.mustCode(t, exitOK)
	wantContains(t, "diff", r.stdout, "Snapshot "+s.ID, "1 path differ", "deleted", "~/project/notes.md",
		"-first line\n", "-second line\n")

	r = runCLI(t, "", "undo")
	r.mustCode(t, exitOK)
	wantContains(t, "undo", r.stdout, "Restored 1 path.")
	if got := readFile(t, notes); got != "first line\nsecond line\n" {
		t.Errorf("undo restored %q", got)
	}

	r = runCLI(t, "", "diff", s.ID)
	r.mustCode(t, exitOK)
	wantContains(t, "diff", r.stdout, "(undone)")

	r = runCLI(t, "", "undo")
	r.wantCode(t, exitError)
	wantContains(t, "stderr", r.stderr, "there is no snapshot left to undo")
}

func TestUndoRefusesLaterChanges(t *testing.T) {
	w := newWorld(t)
	notes := filepath.Join(w.proj, "notes.md")
	writeFile(t, notes, "first line\n")
	w.agentRuns(t, "rm notes.md", func() {
		if err := os.Remove(notes); err != nil {
			t.Fatal(err)
		}
	})
	writeFile(t, notes, "rewritten by hand\n")

	r := runCLI(t, "", "undo")
	r.wantCode(t, exitError)
	wantContains(t, "undo", r.stdout, "conflict: changed after the command ran")
	wantContains(t, "stderr", r.stderr, "--force overwrites them")
	if got := readFile(t, notes); got != "rewritten by hand\n" {
		t.Fatalf("undo without --force wrote %q", got)
	}

	runCLI(t, "", "undo", "--force").mustCode(t, exitOK)
	if got := readFile(t, notes); got != "first line\n" {
		t.Fatalf("undo --force left %q", got)
	}

	// The undo saved the state it replaced; undoing that brings the
	// later change back.
	var original, safety string
	list := snapshots(t)
	for _, s := range list {
		if s.Command == "rm notes.md" {
			original = s.ID
		}
	}
	for _, s := range list {
		if s.Status == "taken by undo" && s.Undoes == original {
			safety = s.ID
		}
	}
	if original == "" || safety == "" {
		t.Fatalf("no safety snapshot: %+v", list)
	}
	runCLI(t, "", "undo", safety).mustCode(t, exitOK)
	if got := readFile(t, notes); got != "rewritten by hand\n" {
		t.Errorf("undoing the undo left %q", got)
	}
}

// toolRuns plays an agent running one tool call: the hook before it, the
// tool, and the hook after it. It returns the answer of the first hook.
func toolRuns(t *testing.T, agent, in string, run func()) string {
	t.Helper()
	r := runCLI(t, in, "hook", agent)
	r.mustCode(t, exitOK)
	if r.stdout != "" {
		return r.stdout
	}
	run()
	runCLI(t, in, "hook", agent+"-post").mustCode(t, exitOK)
	return ""
}

func TestHookFileToolsAreUndoable(t *testing.T) {
	w := newWorld(t)
	readme := filepath.Join(w.proj, "README.md")
	thesis := filepath.Join(w.home, "important", "thesis.txt")
	before := readFile(t, readme)
	thesisBefore := readFile(t, thesis)

	// Claude Code's Write tool rewrites a project file.
	in := hookInput(t, "s1", w.proj, "Write", map[string]any{"file_path": readme, "content": "gone"})
	if out := toolRuns(t, "claude", in, func() { writeFile(t, readme, "gone") }); out != "" {
		t.Fatalf("Write in the project was not allowed: %s", out)
	}
	// Codex deletes a file outside the project with apply_patch. The
	// agent asked about that itself, so undoguard does not block it, but
	// it is snapshotted.
	patch := "*** Begin Patch\n*** Delete File: " + thesis + "\n*** End Patch\n"
	in = hookInput(t, "c1", w.proj, "apply_patch", map[string]any{"command": patch})
	if out := toolRuns(t, "codex", in, func() {
		if err := os.Remove(thesis); err != nil {
			t.Fatal(err)
		}
	}); out != "" {
		t.Fatalf("apply_patch was not allowed: %s", out)
	}

	list := snapshots(t)
	if len(list) != 2 {
		t.Fatalf("snapshots: %+v", list)
	}
	for _, s := range list {
		if s.Status != "ready" {
			t.Errorf("snapshot %s of %q is %q", s.ID, s.Command, s.Status)
		}
	}
	runCLI(t, "", "undo", "--session-id", "c1").mustCode(t, exitOK)
	runCLI(t, "", "undo", "--session-id", "s1").mustCode(t, exitOK)
	if got := readFile(t, readme); got != before {
		t.Errorf("README.md after undo: %q", got)
	}
	if got := readFile(t, thesis); got != thesisBefore {
		t.Errorf("thesis.txt after undo: %q", got)
	}
}

func TestHookFileToolsProtectedPaths(t *testing.T) {
	w := newWorld(t)
	key := filepath.Join(w.home, ".ssh", "authorized_keys")
	for _, c := range []struct{ agent, tool, field string }{
		{"claude", "Edit", "file_path"},
		{"gemini", "write_file", "file_path"},
	} {
		in := hookInput(t, "s1", w.proj, c.tool, map[string]any{c.field: key, "content": "x"})
		r := runCLI(t, in, "hook", c.agent)
		r.mustCode(t, exitOK)
		if !strings.Contains(r.stdout, `"deny"`) || !strings.Contains(r.stdout, "~/.ssh") {
			t.Errorf("%s %s on %s: %q", c.agent, c.tool, key, r.stdout)
		}
	}
	if list := snapshots(t); len(list) != 0 {
		t.Errorf("a blocked edit took snapshots: %+v", list)
	}
}

func TestHookCodexAskTakesNoSnapshot(t *testing.T) {
	w := newWorld(t)
	in := hookInput(t, "c1", w.proj, "Bash", map[string]any{"command": "find . -name '*.log' -delete"})
	out := decodeReply(t, runCLI(t, in, "hook", "codex")).HookSpecificOutput
	if out.PermissionDecision != "deny" {
		t.Fatalf("codex ask: %+v", out)
	}
	if list := snapshots(t); len(list) != 0 {
		t.Errorf("a command that Codex was told not to run was snapshotted: %+v", list)
	}
}

func TestUndoLastN(t *testing.T) {
	w := newWorld(t)
	var files []string
	for _, name := range []string{"a.txt", "b.txt", "c.txt"} {
		f := filepath.Join(w.proj, name)
		writeFile(t, f, name+"\n")
		files = append(files, f)
		w.agentRuns(t, "rm "+name, func() {
			if err := os.Remove(f); err != nil {
				t.Fatal(err)
			}
		})
	}
	// diff without an id shows what undo would restore: the newest one.
	r := runCLI(t, "", "diff", "--stat")
	r.mustCode(t, exitOK)
	wantContains(t, "diff", r.stdout, "Command:  rm c.txt")

	r = runCLI(t, "", "undo", "--last", "2")
	r.mustCode(t, exitOK)
	wantContains(t, "undo --last", r.stdout, "The last 2 snapshots: 2 snapshots to undo, newest first.")
	if _, err := os.Stat(files[0]); err == nil {
		t.Errorf("undo --last 2 also restored the oldest deletion")
	}
	for _, f := range files[1:] {
		if got := readFile(t, f); got != filepath.Base(f)+"\n" {
			t.Errorf("%s after undo: %q", f, got)
		}
	}
	for _, bad := range [][]string{{"undo", "--last", "2", "--session"}, {"undo", "--last", "-1"}, {"undo", "--last", "1", "some-id"}} {
		runCLI(t, "", bad...).wantCode(t, exitError)
	}
}
