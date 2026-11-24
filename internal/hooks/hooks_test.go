package hooks

import (
	"bytes"
	"encoding/json"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/celaldrg/undoguard/internal/guard"
	"github.com/celaldrg/undoguard/internal/policy"
)

func TestParseClaude(t *testing.T) {
	in := `{"session_id":"s1","cwd":"/work","hook_event_name":"PreToolUse","tool_name":"Bash","tool_input":{"command":"rm -rf build","description":"clean"}}`
	ev, err := Parse(Claude, strings.NewReader(in))
	if err != nil {
		t.Fatal(err)
	}
	if ev.Command != "rm -rf build" || ev.Session != "s1" || ev.Dir != "/work" || ev.Tool != "Bash" || ev.Agent != Claude {
		t.Errorf("got %+v", ev)
	}
}

func TestParseOtherTool(t *testing.T) {
	in := `{"session_id":"s1","cwd":"/work","tool_name":"Read","tool_input":{"file_path":"/work/a"}}`
	ev, err := Parse(Claude, strings.NewReader(in))
	if err != nil {
		t.Fatal(err)
	}
	if ev.Command != "" || ev.Edits != nil {
		t.Errorf("a tool that changes nothing gave %q, %+v", ev.Command, ev.Edits)
	}
}

func TestParseFileTools(t *testing.T) {
	for _, c := range []struct {
		agent, in, cmd string
		edits          []guard.Edit
	}{
		{Claude, `{"cwd":"/work","tool_name":"Write","tool_input":{"file_path":"/work/a","content":"x"}}`,
			"Write /work/a", []guard.Edit{{Path: "/work/a"}}},
		{Claude, `{"cwd":"/work","tool_name":"Edit","tool_input":{"file_path":"src/b.go","old_string":"x","new_string":"y"}}`,
			"Edit /work/src/b.go", []guard.Edit{{Path: "/work/src/b.go"}}},
		{Claude, `{"cwd":"/work","tool_name":"NotebookEdit","tool_input":{"notebook_path":"/work/n.ipynb","new_source":""}}`,
			"NotebookEdit /work/n.ipynb", []guard.Edit{{Path: "/work/n.ipynb"}}},
		{Gemini, `{"cwd":"/work","tool_name":"replace","tool_input":{"file_path":"/work/c.txt","old_string":"a","new_string":"b"}}`,
			"replace /work/c.txt", []guard.Edit{{Path: "/work/c.txt"}}},
		{Codex, `{"cwd":"/work","tool_name":"apply_patch","tool_input":{"command":"*** Begin Patch\n*** Add File: hello.txt\n+hi\n*** Update File: src/app.py\n*** Move to: src/main.py\n@@\n-a\n+b\n*** Delete File: /work/old.txt\n*** End Patch\n"}}`,
			"apply_patch /work/hello.txt /work/src/app.py /work/src/main.py --delete /work/old.txt",
			[]guard.Edit{{Path: "/work/hello.txt"}, {Path: "/work/src/app.py"}, {Path: "/work/src/main.py"}, {Path: "/work/old.txt", Delete: true}}},
		// A relative path with no directory to resolve it against is
		// dropped rather than guessed.
		{Claude, `{"tool_name":"Write","tool_input":{"file_path":"a"}}`, "", nil},
	} {
		ev, err := Parse(c.agent, strings.NewReader(c.in))
		if err != nil {
			t.Fatalf("%s: %v", c.in, err)
		}
		if ev.Command != c.cmd || !reflect.DeepEqual(ev.Edits, c.edits) {
			t.Errorf("%s:\n got %q %+v\nwant %q %+v", c.in, ev.Command, ev.Edits, c.cmd, c.edits)
		}
	}
}

func TestParseCodexArgv(t *testing.T) {
	in := `{"session_id":"c1","cwd":"/work","tool_name":"shell","tool_input":{"command":["bash","-lc","rm -rf build"],"workdir":"sub"}}`
	ev, err := Parse(Codex, strings.NewReader(in))
	if err != nil {
		t.Fatal(err)
	}
	if ev.Command != "bash -lc 'rm -rf build'" {
		t.Errorf("command = %q", ev.Command)
	}
	if want := filepath.Join("/work", "sub"); ev.Dir != want {
		t.Errorf("dir = %q, want %q", ev.Dir, want)
	}
}

func TestParseGemini(t *testing.T) {
	in := `{"session_id":"g1","cwd":"/work","tool_name":"run_shell_command","tool_input":{"command":"ls","directory":"/abs"}}`
	ev, err := Parse(Gemini, strings.NewReader(in))
	if err != nil {
		t.Fatal(err)
	}
	if ev.Command != "ls" || ev.Dir != "/abs" {
		t.Errorf("got %+v", ev)
	}
}

func TestParseErrors(t *testing.T) {
	for _, in := range []string{
		`not json`,
		`{"tool_name":"Bash","tool_input":{"command":5}}`,
		`{"tool_name":"Bash","tool_input":"rm"}`,
	} {
		if _, err := Parse(Claude, strings.NewReader(in)); err == nil {
			t.Errorf("Parse(%q) succeeded", in)
		}
	}
}

func TestWriteAllowIsSilent(t *testing.T) {
	for _, agent := range []string{Claude, Codex, Gemini} {
		var buf bytes.Buffer
		if err := Write(&buf, agent, policy.Allow, "fine"); err != nil {
			t.Fatal(err)
		}
		if buf.Len() != 0 {
			t.Errorf("%s: allow wrote %q", agent, buf.String())
		}
	}
}

type preToolUse struct {
	HookSpecificOutput struct {
		HookEventName            string `json:"hookEventName"`
		PermissionDecision       string `json:"permissionDecision"`
		PermissionDecisionReason string `json:"permissionDecisionReason"`
	} `json:"hookSpecificOutput"`
}

func TestWriteClaude(t *testing.T) {
	for _, act := range []policy.Action{policy.Ask, policy.Deny} {
		var buf bytes.Buffer
		if err := Write(&buf, Claude, act, "rm: ~/x"); err != nil {
			t.Fatal(err)
		}
		var out preToolUse
		if err := json.Unmarshal(buf.Bytes(), &out); err != nil {
			t.Fatalf("%s: %v in %q", act, err, buf.String())
		}
		h := out.HookSpecificOutput
		if h.HookEventName != "PreToolUse" || h.PermissionDecision != string(act) || h.PermissionDecisionReason != "undoguard: rm: ~/x" {
			t.Errorf("%s: got %+v", act, h)
		}
	}
}

func TestWriteCodexAskBecomesDeny(t *testing.T) {
	var buf bytes.Buffer
	if err := Write(&buf, Codex, policy.Ask, "why"); err != nil {
		t.Fatal(err)
	}
	var out preToolUse
	if err := json.Unmarshal(buf.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	h := out.HookSpecificOutput
	if h.PermissionDecision != "deny" || h.PermissionDecisionReason != "undoguard: why"+AskHint {
		t.Errorf("got %+v", h)
	}
}

func TestWriteGemini(t *testing.T) {
	cases := map[policy.Action]string{
		policy.Deny: "undoguard: why",
		policy.Ask:  "undoguard: why" + AskHint,
	}
	for act, reason := range cases {
		var buf bytes.Buffer
		if err := Write(&buf, Gemini, act, "why"); err != nil {
			t.Fatal(err)
		}
		var out map[string]string
		if err := json.Unmarshal(buf.Bytes(), &out); err != nil {
			t.Fatal(err)
		}
		if out["decision"] != "deny" || out["reason"] != reason {
			t.Errorf("%s: got %v", act, out)
		}
	}
}

func TestWriteUnknownAgent(t *testing.T) {
	if err := Write(&bytes.Buffer{}, "other", policy.Deny, "x"); err == nil {
		t.Error("unknown agent accepted")
	}
}

func TestKinds(t *testing.T) {
	for name, k := range Kinds {
		if strings.HasSuffix(name, "-post") != k.Post || !strings.HasPrefix(name, k.Agent) {
			t.Errorf("%s: %+v", name, k)
		}
	}
}
