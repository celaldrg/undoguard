package cli

import (
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/celaldrg/undoguard/internal/hooks"
	"github.com/celaldrg/undoguard/internal/jsonedit"
	"github.com/celaldrg/undoguard/internal/paths"
	"github.com/celaldrg/undoguard/internal/textdiff"
)

// hookSpec is one hook that init installs.
type hookSpec struct {
	// Event is the agent's name for the moment the hook runs.
	Event string
	// Matcher selects the tools, "" for all of them.
	Matcher string
	// Name is the undoguard hook name, such as "claude-post".
	Name string
	// Timeout is in the agent's unit, 0 for its default.
	Timeout int
}

// agentSetup is where and how one agent takes its hooks.
type agentSetup struct {
	Agent string
	// Dir is the agent's configuration directory and File the file that
	// holds the hooks.
	Dir   string
	File  string
	Hooks []hookSpec
}

// The tool matchers of the hooks: the shell tool and the tools that
// write files, of each agent.
const (
	claudeTools = "Bash|Write|Edit|MultiEdit|NotebookEdit"
	codexTools  = "Bash|apply_patch"
	geminiTools = "run_shell_command|write_file|replace"
)

func agentSetups() []agentSetup {
	home := paths.Home()
	claude := os.Getenv("CLAUDE_CONFIG_DIR")
	if claude == "" {
		claude = filepath.Join(home, ".claude")
	}
	codex := os.Getenv("CODEX_HOME")
	if codex == "" {
		codex = filepath.Join(home, ".codex")
	}
	gemini := filepath.Join(home, ".gemini")
	return []agentSetup{
		{
			Agent: hooks.Claude,
			Dir:   claude,
			File:  filepath.Join(claude, "settings.json"),
			Hooks: []hookSpec{
				{Event: "PreToolUse", Matcher: claudeTools, Name: "claude", Timeout: 300},
				{Event: "PostToolUse", Matcher: claudeTools, Name: "claude-post", Timeout: 120},
			},
		},
		{
			// Shell commands match as Bash in Codex, file edits as
			// apply_patch. Codex asks to trust new hooks in /hooks.
			Agent: hooks.Codex,
			Dir:   codex,
			File:  filepath.Join(codex, "hooks.json"),
			Hooks: []hookSpec{
				{Event: "PreToolUse", Matcher: codexTools, Name: "codex"},
				{Event: "PostToolUse", Matcher: codexTools, Name: "codex-post"},
			},
		},
		{
			Agent: hooks.Gemini,
			Dir:   gemini,
			File:  filepath.Join(gemini, "settings.json"),
			Hooks: []hookSpec{
				{Event: "BeforeTool", Matcher: geminiTools, Name: "gemini", Timeout: 300000},
				{Event: "AfterTool", Matcher: geminiTools, Name: "gemini-post", Timeout: 120000},
			},
		},
	}
}

func cmdInit(a *app, args []string) int {
	fs := a.flags("init", "[--agent claude,codex,gemini] [--dry-run] [--remove]")
	only := fs.String("agent", "", "comma-separated `agents` to set up (default: those whose configuration directory exists)")
	dryRun := fs.Bool("dry-run", false, "show the changes without writing them")
	remove := fs.Bool("remove", false, "remove the undoguard hooks instead of adding them")
	if code, ok := parse(fs, args); !ok {
		return code
	}
	if fs.NArg() > 0 {
		fs.Usage()
		return exitError
	}
	self, err := os.Executable()
	if err != nil {
		return a.errorf("finding the undoguard program: %v", err)
	}
	if resolved, err := filepath.EvalSymlinks(self); err == nil {
		self = resolved
	}

	setups := agentSetups()
	chosen := map[string]bool{}
	for _, name := range strings.Split(*only, ",") {
		if name = strings.TrimSpace(name); name == "" {
			continue
		}
		known := false
		for _, s := range setups {
			known = known || s.Agent == name
		}
		if !known {
			return a.errorf("unknown agent %q; choose from claude, codex and gemini", name)
		}
		chosen[name] = true
	}

	failed, done := false, 0
	for _, s := range setups {
		if len(chosen) > 0 && !chosen[s.Agent] {
			continue
		}
		if len(chosen) == 0 && !paths.IsDir(s.Dir) {
			fmt.Fprintf(a.stdout, "%-7s skipped: %s does not exist (--agent %s sets it up anyway)\n", s.Agent,
				paths.Display(s.Dir), s.Agent)
			continue
		}
		msg, err := a.setUp(s, self, *remove, *dryRun)
		if err != nil {
			fmt.Fprintf(a.stderr, "undoguard: %s: %v\n", s.Agent, err)
			failed = true
			continue
		}
		fmt.Fprintf(a.stdout, "%-7s %s\n", s.Agent, msg)
		done++
	}
	if failed {
		return exitError
	}
	if done == 0 {
		fmt.Fprintln(a.stdout, "\nNo agent was set up. Name one with --agent.")
		return exitOK
	}
	if !*dryRun && !*remove {
		fmt.Fprintln(a.stdout, "\nRestart the agents for the hooks to take effect.")
		fmt.Fprintln(a.stdout, "Try: undoguard check 'rm -rf ~/'")
	}
	return exitOK
}

// setUp adds or removes the hooks of one agent and says what it did.
func (a *app) setUp(s agentSetup, self string, remove, dryRun bool) (string, error) {
	file := s.File
	if resolved, err := filepath.EvalSymlinks(file); err == nil {
		// Write through a symlink, as a dotfile manager may have made.
		file = resolved
	}
	old, err := os.ReadFile(file)
	exists := err == nil
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		return "", err
	}
	if !exists && remove {
		return "nothing to remove: " + paths.Display(s.File) + " does not exist", nil
	}

	var doc any = jsonedit.NewObject()
	indent := "  "
	if exists && len(strings.TrimSpace(string(old))) > 0 {
		if doc, err = jsonedit.Parse(old); err != nil {
			return "", fmt.Errorf("%s is not valid JSON (%v); add the hooks by hand, see the README", paths.Display(s.File), err)
		}
		indent = jsonedit.Indent(old)
	}
	root, ok := doc.(*jsonedit.Object)
	if !ok {
		return "", fmt.Errorf("%s does not hold a JSON object", paths.Display(s.File))
	}
	before, err := jsonedit.Marshal(root, indent)
	if err != nil {
		return "", err
	}
	if err := editHooks(root, s.Hooks, hookCommandPrefix(self), remove); err != nil {
		return "", fmt.Errorf("%s: %v", paths.Display(s.File), err)
	}
	after, err := jsonedit.Marshal(root, indent)
	if err != nil {
		return "", err
	}
	if exists && string(before) == string(after) {
		if remove {
			return "nothing to remove in " + paths.Display(s.File), nil
		}
		return "already set up in " + paths.Display(s.File), nil
	}
	if dryRun {
		label := paths.Display(s.File)
		io.WriteString(a.stdout, textdiff.Unified(label, label+" (new)", old, after, 3))
		return "would change " + label, nil
	}

	mode := fs.FileMode(0o600)
	backup := ""
	if exists {
		if info, err := os.Stat(file); err == nil {
			mode = info.Mode().Perm()
		}
		b, err := writeBackup(file, old, mode)
		if err != nil {
			return "", fmt.Errorf("writing the backup: %w", err)
		}
		backup = b
	} else if err := os.MkdirAll(filepath.Dir(file), 0o700); err != nil {
		return "", err
	}
	if err := writeAtomic(file, after, mode); err != nil {
		return "", err
	}
	verb := "added the hooks to "
	if remove {
		verb = "removed the hooks from "
	}
	msg := verb + paths.Display(s.File)
	if backup != "" {
		msg += " (backup: " + paths.Display(backup) + ")"
	}
	return msg, nil
}

// hookCommandPrefix is the start of every hook command: the undoguard
// program, quoted for the shell that runs hooks.
func hookCommandPrefix(self string) string {
	return shQuote(self) + " hook "
}

// editHooks removes the undoguard entries of the given hooks from the
// "hooks" object of a settings document and, unless remove is set, adds
// them again at the end of their event. Entries of other tools are kept
// as they are.
func editHooks(root *jsonedit.Object, specs []hookSpec, prefix string, remove bool) error {
	var hooksObj *jsonedit.Object
	if v, ok := root.Get("hooks"); ok {
		if hooksObj, ok = v.(*jsonedit.Object); !ok {
			return errors.New(`"hooks" is not an object`)
		}
	} else if remove {
		return nil
	} else {
		hooksObj = jsonedit.NewObject()
		root.Set("hooks", hooksObj)
	}
	for _, spec := range specs {
		var groups []any
		if v, ok := hooksObj.Get(spec.Event); ok {
			if groups, ok = v.([]any); !ok {
				return fmt.Errorf(`"hooks.%s" is not a list`, spec.Event)
			}
		}
		groups = dropOurs(groups, spec.Name)
		if !remove {
			groups = append(groups, newGroup(spec, prefix+spec.Name))
		}
		if len(groups) == 0 {
			hooksObj.Delete(spec.Event)
		} else {
			hooksObj.Set(spec.Event, groups)
		}
	}
	if remove && hooksObj.Len() == 0 {
		root.Delete("hooks")
	}
	return nil
}

// dropOurs removes the undoguard hook name from a list of matcher groups,
// and the groups that held nothing else.
func dropOurs(groups []any, name string) []any {
	var out []any
	for _, g := range groups {
		obj, ok := g.(*jsonedit.Object)
		if !ok {
			out = append(out, g)
			continue
		}
		v, _ := obj.Get("hooks")
		list, ok := v.([]any)
		if !ok {
			out = append(out, g)
			continue
		}
		var kept []any
		for _, h := range list {
			if isOurs(h, name) {
				continue
			}
			kept = append(kept, h)
		}
		if len(kept) == len(list) {
			out = append(out, g)
			continue
		}
		if len(kept) == 0 {
			continue
		}
		obj.Set("hooks", kept)
		out = append(out, g)
	}
	return out
}

// isOurs reports whether a hook entry runs `undoguard hook <name>`,
// wherever undoguard was installed.
func isOurs(h any, name string) bool {
	obj, ok := h.(*jsonedit.Object)
	if !ok {
		return false
	}
	v, _ := obj.Get("command")
	cmd, ok := v.(string)
	if !ok {
		return false
	}
	f := strings.Fields(cmd)
	n := len(f)
	return n >= 3 && f[n-2] == "hook" && f[n-1] == name && strings.Contains(strings.Join(f[:n-2], " "), "undoguard")
}

// writeBackup saves data next to file under a name no earlier backup has,
// so a second run in the same second never replaces the first backup,
// which may be the only copy of the settings from before undoguard.
func writeBackup(file string, data []byte, mode fs.FileMode) (string, error) {
	base := file + ".undoguard-backup-" + time.Now().UTC().Format("20060102-150405")
	for i := 0; i < 100; i++ {
		name := base
		if i > 0 {
			name = fmt.Sprintf("%s-%d", base, i)
		}
		f, err := os.OpenFile(name, os.O_WRONLY|os.O_CREATE|os.O_EXCL, mode)
		if errors.Is(err, fs.ErrExist) {
			continue
		}
		if err != nil {
			return "", err
		}
		_, err = f.Write(data)
		if cerr := f.Close(); err == nil {
			err = cerr
		}
		if err != nil {
			os.Remove(name)
			return "", err
		}
		return name, nil
	}
	return "", fmt.Errorf("too many backups of %s in one second", file)
}

// newGroup builds one matcher group with the hook command.
func newGroup(spec hookSpec, command string) *jsonedit.Object {
	h := jsonedit.NewObject()
	h.Set("type", "command")
	h.Set("command", command)
	if spec.Timeout > 0 {
		h.Set("timeout", spec.Timeout)
	}
	g := jsonedit.NewObject()
	if spec.Matcher != "" {
		g.Set("matcher", spec.Matcher)
	}
	g.Set("hooks", []any{h})
	return g
}

// writeAtomic replaces file with data through a temporary file in the
// same directory.
func writeAtomic(file string, data []byte, mode fs.FileMode) error {
	tmp, err := os.CreateTemp(filepath.Dir(file), "."+filepath.Base(file)+".undoguard-*")
	if err != nil {
		return err
	}
	name := tmp.Name()
	_, werr := tmp.Write(data)
	cerr := tmp.Close()
	if err := errors.Join(werr, cerr); err != nil {
		os.Remove(name)
		return err
	}
	if err := os.Chmod(name, mode); err != nil {
		os.Remove(name)
		return err
	}
	if err := os.Rename(name, file); err != nil {
		os.Remove(name)
		return err
	}
	return nil
}
