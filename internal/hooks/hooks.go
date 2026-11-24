// Package hooks reads the hook calls of AI coding agents and writes their
// answers. Claude Code, Codex CLI and Gemini CLI run a command before (and
// some after) each tool call and pass it the call as JSON on stdin. This
// package turns that JSON into an Event, and a verdict into the JSON the
// agent expects back.
package hooks

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/celaldrg/undoguard/internal/guard"
	"github.com/celaldrg/undoguard/internal/parser"
	"github.com/celaldrg/undoguard/internal/policy"
)

// The agents undoguard has hooks for.
const (
	Claude = "claude"
	Codex  = "codex"
	Gemini = "gemini"
)

// Kind is a hook as named on the undoguard command line.
type Kind struct {
	Agent string
	// Post is set for the hook that runs after the tool.
	Post bool
}

// Kinds lists the hooks that `undoguard hook` accepts.
var Kinds = map[string]Kind{
	"claude":      {Agent: Claude},
	"claude-post": {Agent: Claude, Post: true},
	"codex":       {Agent: Codex},
	"codex-post":  {Agent: Codex, Post: true},
	"gemini":      {Agent: Gemini},
	"gemini-post": {Agent: Gemini, Post: true},
}

// Event is one tool call that an agent is about to make or has made.
type Event struct {
	Agent   string
	Session string
	// Dir is the directory the command runs in, "" when unknown.
	Dir  string
	Tool string
	// Command is the shell command line, or "" when the tool does not
	// run one. For a file tool it describes the call instead.
	Command string
	// Edits lists the files a file tool is about to change. It is nil for
	// shell commands.
	Edits []guard.Edit
}

// shellTools are the tool names under which the agents run shell
// commands.
var shellTools = map[string]bool{
	"Bash":              true,
	"bash":              true,
	"shell":             true,
	"local_shell":       true,
	"exec_command":      true,
	"run_shell_command": true,
}

type input struct {
	SessionID string          `json:"session_id"`
	Cwd       string          `json:"cwd"`
	ToolName  string          `json:"tool_name"`
	ToolInput json.RawMessage `json:"tool_input"`
}

// fileTools are the tools that write files themselves, with the key of
// the file path in their input. apply_patch carries a patch instead.
var fileTools = map[string]string{
	"Write":        "file_path", // Claude Code
	"Edit":         "file_path",
	"MultiEdit":    "file_path",
	"NotebookEdit": "notebook_path",
	"write_file":   "file_path", // Gemini CLI
	"replace":      "file_path",
	"apply_patch":  "", // Codex CLI
}

type toolInput struct {
	Command   json.RawMessage `json:"command"`
	Cmd       json.RawMessage `json:"cmd"`
	Workdir   string          `json:"workdir"`
	Directory string          `json:"directory"`
}

// maxInput bounds the hook input that is read.
const maxInput = 16 << 20

// Parse reads one hook call of agent.
func Parse(agent string, r io.Reader) (*Event, error) {
	var in input
	if err := json.NewDecoder(io.LimitReader(r, maxInput)).Decode(&in); err != nil {
		return nil, fmt.Errorf("reading the %s hook input: %w", agent, err)
	}
	ev := &Event{Agent: agent, Session: in.SessionID, Dir: in.Cwd, Tool: in.ToolName}
	if len(in.ToolInput) == 0 {
		return ev, nil
	}
	if key, ok := fileTools[in.ToolName]; ok {
		return ev, parseEdits(ev, key, in.ToolInput)
	}
	if !shellTools[in.ToolName] {
		return ev, nil
	}
	var ti toolInput
	if err := json.Unmarshal(in.ToolInput, &ti); err != nil {
		return nil, fmt.Errorf("reading the input of the %s tool: %w", in.ToolName, err)
	}
	raw := ti.Command
	if len(raw) == 0 {
		raw = ti.Cmd
	}
	cmd, err := commandText(raw)
	if err != nil {
		return nil, fmt.Errorf("reading the input of the %s tool: %w", in.ToolName, err)
	}
	ev.Command = cmd
	for _, d := range []string{ti.Workdir, ti.Directory} {
		if d == "" {
			continue
		}
		if filepath.IsAbs(d) || ev.Dir == "" {
			ev.Dir = d
		} else {
			ev.Dir = filepath.Join(ev.Dir, d)
		}
		break
	}
	return ev, nil
}

// parseEdits fills in the files a file tool call changes, and a
// description of the call that is the same before and after the tool.
func parseEdits(ev *Event, key string, raw json.RawMessage) error {
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(raw, &fields); err != nil {
		return fmt.Errorf("reading the input of the %s tool: %w", ev.Tool, err)
	}
	if key == "" {
		var patch string
		if c := fields["command"]; len(c) > 0 {
			if s, err := commandText(c); err == nil {
				patch = s
			}
		}
		if patch == "" {
			if c := fields["input"]; len(c) > 0 {
				_ = json.Unmarshal(c, &patch)
			}
		}
		ev.Edits = PatchEdits(patch, ev.Dir)
	} else {
		var p string
		if err := json.Unmarshal(fields[key], &p); err == nil {
			if abs := absUnder(ev.Dir, p); abs != "" {
				ev.Edits = []guard.Edit{{Path: abs}}
			}
		}
	}
	if len(ev.Edits) == 0 {
		return nil
	}
	parts := []string{ev.Tool}
	for _, e := range ev.Edits {
		if e.Delete {
			parts = append(parts, "--delete")
		}
		parts = append(parts, e.Path)
	}
	ev.Command = parser.QuoteArgs(parts)
	return nil
}

// PatchEdits lists the files an apply_patch patch adds, updates, moves
// or deletes, resolved against dir.
func PatchEdits(patch, dir string) []guard.Edit {
	var out []guard.Edit
	seen := map[string]bool{}
	add := func(p string, del bool) {
		p = strings.TrimSpace(p)
		if p == "" {
			return
		}
		e := guard.Edit{Path: absUnder(dir, p), Delete: del}
		if e.Path == "" || seen[e.Path] {
			return
		}
		seen[e.Path] = true
		out = append(out, e)
	}
	for _, line := range strings.Split(patch, "\n") {
		line = strings.TrimRight(line, "\r")
		switch {
		case strings.HasPrefix(line, "*** Add File: "):
			add(strings.TrimPrefix(line, "*** Add File: "), false)
		case strings.HasPrefix(line, "*** Update File: "):
			add(strings.TrimPrefix(line, "*** Update File: "), false)
		case strings.HasPrefix(line, "*** Move to: "):
			add(strings.TrimPrefix(line, "*** Move to: "), false)
		case strings.HasPrefix(line, "*** Delete File: "):
			add(strings.TrimPrefix(line, "*** Delete File: "), true)
		}
	}
	return out
}

// absUnder makes p absolute against dir; it returns "" when that is not
// possible.
func absUnder(dir, p string) string {
	if p == "" {
		return ""
	}
	if strings.HasPrefix(p, "~/") {
		if home, err := os.UserHomeDir(); err == nil {
			p = filepath.Join(home, p[2:])
		}
	}
	if filepath.IsAbs(p) {
		return filepath.Clean(p)
	}
	if dir == "" || !filepath.IsAbs(dir) {
		return ""
	}
	return filepath.Join(dir, p)
}

// commandText turns a command given as a string or as an argument vector
// into one command line. A vector such as ["bash", "-lc", "rm -r x"]
// becomes a quoted line; the guard then reads the script inside it as it
// reads any `bash -c`.
func commandText(raw json.RawMessage) (string, error) {
	if len(raw) == 0 || string(raw) == "null" {
		return "", nil
	}
	var s string
	if err := json.Unmarshal(raw, &s); err == nil {
		return s, nil
	}
	var argv []string
	if err := json.Unmarshal(raw, &argv); err != nil {
		return "", errors.New("the command is neither a string nor a list of strings")
	}
	if len(argv) == 0 {
		return "", nil
	}
	return parser.QuoteArgs(argv), nil
}

// AskHint follows the reason when an agent cannot hand a question to its
// user from a hook, so that ask has to become deny.
const AskHint = ". This needs your confirmation, which this agent's hooks cannot ask for, " +
	"so it was blocked: run it yourself, or allow it in the undoguard policy."

// Write writes the answer of a hook that runs before a tool. An allow
// writes nothing, so that the agent's own permission rules still apply:
// undoguard never approves a command the agent would otherwise ask about.
func Write(w io.Writer, agent string, act policy.Action, reason string) error {
	if act == policy.Allow {
		return nil
	}
	reason = "undoguard: " + reason
	var v any
	switch agent {
	case Claude, Codex:
		decision := string(act)
		if agent == Codex && act == policy.Ask {
			decision, reason = string(policy.Deny), reason+AskHint
		}
		v = map[string]any{
			"hookSpecificOutput": map[string]string{
				"hookEventName":            "PreToolUse",
				"permissionDecision":       decision,
				"permissionDecisionReason": reason,
			},
		}
	case Gemini:
		if act == policy.Ask {
			reason += AskHint
		}
		v = map[string]string{"decision": string(policy.Deny), "reason": reason}
	default:
		return fmt.Errorf("unknown agent %q", agent)
	}
	b, err := json.Marshal(v)
	if err != nil {
		return err
	}
	_, err = w.Write(append(b, '\n'))
	return err
}
