// Package guard decides whether a command line may run: it parses it,
// finds the destructive commands in it, places every target with the
// policy and works out what has to be snapshotted first.
package guard

import (
	"strings"

	"github.com/celaldrg/undoguard/internal/detect"
	"github.com/celaldrg/undoguard/internal/parser"
	"github.com/celaldrg/undoguard/internal/paths"
	"github.com/celaldrg/undoguard/internal/policy"
)

// Options configures Check.
type Options struct {
	// Dir is the directory the command line runs in.
	Dir string
	// Env is the environment it runs with. Nil means os.Environ().
	Env []string
	// Policy is the loaded policy.
	Policy *policy.Policy
}

// Assessment is the verdict on one target, resource or unknown part of a
// command line.
type Assessment struct {
	// Command is the simple command, as written, or "" for a parse issue.
	Command string
	// Op is the kind of change.
	Op detect.Op
	// Rule is the detector rule, such as "rm" or "git-clean".
	Rule string
	// What says what the command does.
	What string
	// Target is the path, when there is one.
	Target detect.Target
	// Resource names an external resource, such as "docker volume".
	Resource string
	// Class is where the target is.
	Class policy.Class
	// Why explains the class.
	Why string
	// Action is the verdict.
	Action policy.Action
	// By is the policy rule that set the action, or nil when the class
	// default did.
	By *policy.LoadedRule
}

// Decider names what set the action, for output.
func (a *Assessment) Decider() string {
	if a.By != nil {
		s := "rule " + a.By.Label()
		if a.By.Reason != "" {
			s += ": " + a.By.Reason
		}
		return s
	}
	if a.Class == "" {
		return "no destructive effect"
	}
	return "default for " + string(a.Class) + " paths"
}

// SnapshotItem is one path to copy before the command runs.
type SnapshotItem struct {
	// Path is the absolute path. It may not exist yet, in which case
	// undo removes what the command creates there.
	Path string
	// Recursive is set when everything below Path is affected.
	Recursive bool
	// Follow is set when the command acts on a symlink's target.
	Follow bool
	// Git narrows a recursive item to the files in one git state.
	Git detect.GitScope
	// Repo is the repository root for git items.
	Repo string
}

// Decision is the verdict on a whole command line.
type Decision struct {
	// Command is the command line.
	Command string
	// Dir is the directory it was checked for.
	Dir string
	// Action is the strictest verdict of all assessments, or allow when
	// nothing is destructive.
	Action policy.Action
	// Deciding is the first assessment with the strictest action, or nil
	// when there is none.
	Deciding *Assessment
	// Assessments lists every verdict, in command order.
	Assessments []Assessment
	// Snapshot lists what to copy before the command runs, unless the
	// decision is deny.
	Snapshot []SnapshotItem
	// Warnings are policy load warnings.
	Warnings []string
}

// Destructive reports whether anything in the command line changes or
// removes data.
func (d *Decision) Destructive() bool {
	for _, a := range d.Assessments {
		if a.Op != "" {
			return true
		}
	}
	return false
}

// Check decides on a command line.
func Check(cmd string, o Options) *Decision {
	res := parser.Parse(cmd, parser.Options{Dir: o.Dir, Env: o.Env})
	return decide(cmd, o, res)
}

// CheckArgv decides on one command given as an argument vector.
func CheckArgv(argv []string, o Options) *Decision {
	res := parser.ParseArgv(argv, parser.Options{Dir: o.Dir, Env: o.Env})
	return decide(parser.QuoteArgs(argv), o, res)
}

type checker struct {
	p    *policy.Policy
	res  *parser.Result
	d    *Decision
	seen map[string]bool
	snap map[string]bool
}

func decide(cmd string, o Options, res *parser.Result) *Decision {
	d := &Decision{Command: cmd, Dir: o.Dir, Action: policy.Allow, Warnings: o.Policy.Warnings}
	ck := &checker{p: o.Policy, res: res, d: d, seen: map[string]bool{}, snap: map[string]bool{}}
	for _, is := range res.Issues {
		ck.issue(is)
	}
	for _, c := range res.Commands {
		ck.command(c)
	}
	d.settle()
	return d
}

// settle sets the action of the whole decision from its assessments.
func (d *Decision) settle() {
	for i := range d.Assessments {
		a := &d.Assessments[i]
		if d.Deciding == nil || a.Action.Rank() > d.Action.Rank() {
			d.Action, d.Deciding = a.Action, a
		}
	}
	if d.Deciding != nil && d.Deciding.Action == policy.Allow && !d.Destructive() {
		d.Deciding = nil
	}
	if d.Action == policy.Deny {
		d.Snapshot = nil
	}
}

// Edit is one file that an agent's own file tool (Write, Edit,
// apply_patch, write_file, ...) is about to change.
type Edit struct {
	// Path is the absolute path of the file.
	Path string
	// Delete is set when the tool removes the file rather than writing it.
	Delete bool
}

// CheckEdits decides on the files an agent's file tool is about to
// change. cmd describes the call; it is what the snapshot is recorded
// under.
//
// The agents already ask before their file tools write outside the
// project, so the class defaults do not block an edit here: an edit is
// snapshotted wherever it lands, and only protected paths, the undoguard
// policy files and the user's own path rules can stop it.
func CheckEdits(cmd string, edits []Edit, o Options) *Decision {
	d := &Decision{Command: cmd, Dir: o.Dir, Action: policy.Allow, Warnings: o.Policy.Warnings}
	ck := &checker{p: o.Policy, res: &parser.Result{}, d: d, seen: map[string]bool{}, snap: map[string]bool{}}
	for _, e := range edits {
		ck.edit(cmd, e)
	}
	d.settle()
	return d
}

func (ck *checker) edit(cmd string, e Edit) {
	if e.Path == "" || !strings.HasPrefix(e.Path, "/") {
		return
	}
	// Writing through a symlink writes what it points to; deleting
	// removes the link itself.
	t := detect.Target{Path: e.Path, Raw: e.Path, Follow: !e.Delete}
	a := Assessment{Command: cmd, Op: detect.OpOverwrite, Rule: "edit-tool", What: "the agent's file tool rewrites the file", Target: t}
	if e.Delete {
		a.Op, a.What = detect.OpDelete, "the agent's file tool deletes the file"
	}
	pl := ck.p.Place(t.Path, t.Follow, false, false)
	if !paths.Exists(t.Path) && pl.Class != policy.ClassProtected {
		ck.snapshot(t, pl.Class)
		return
	}
	base := policy.Allow
	if pl.Class == policy.ClassProtected || pl.Class == policy.ClassPolicyFile {
		base = pl.Action
	}
	a.Class, a.Why = pl.Class, pl.Why
	a.Action, a.By = ck.p.Decide(policy.Subject{Command: cmd, Finding: a.Rule, Path: t.Path}, base, pl.Class)
	if pl.Path != t.Path {
		act, by := ck.p.Decide(policy.Subject{Command: cmd, Finding: a.Rule, Path: pl.Path}, base, pl.Class)
		if act.Rank() > a.Action.Rank() {
			a.Action, a.By = act, by
		}
	}
	ck.add(a)
	if a.Action != policy.Deny {
		ck.snapshot(t, pl.Class)
	}
}

func (ck *checker) add(a Assessment) {
	key := strings.Join([]string{a.Command, a.Rule, a.Target.Path, a.Target.Raw, a.Resource, string(a.Class), string(a.Action)}, "\x00")
	if ck.seen[key] {
		return
	}
	ck.seen[key] = true
	ck.d.Assessments = append(ck.d.Assessments, a)
}

func (ck *checker) issue(is parser.Issue) {
	class := policy.ClassUnresolved
	act, by := ck.p.Decide(policy.Subject{Finding: "issue-" + is.Kind}, ck.p.Action(class), class)
	ck.add(Assessment{
		Op:     detect.OpModify,
		Rule:   "issue-" + is.Kind,
		What:   is.Detail,
		Class:  class,
		Why:    "this part of the command line cannot be seen through before it runs",
		Action: act,
		By:     by,
	})
}

// commandLine renders a command for rule matching: the program name and
// the plain argument values, separated by spaces.
func commandLine(c *parser.Command) string {
	parts := append([]string{c.Name}, c.ArgValues()...)
	return strings.Join(parts, " ")
}

func (ck *checker) command(c *parser.Command) {
	line := commandLine(c)
	findings := detect.Command(c)
	if len(findings) == 0 {
		act, by := ck.p.Decide(policy.Subject{Command: line}, policy.Allow, "")
		if by != nil && act != policy.Allow {
			ck.add(Assessment{Command: c.Display(), Rule: "command", What: "matches a policy rule", Action: act, By: by})
		}
		return
	}
	for i := range findings {
		ck.finding(c, line, &findings[i])
	}
}

func (ck *checker) finding(c *parser.Command, line string, f *detect.Finding) {
	base := Assessment{Command: c.Display(), Op: f.Op, Rule: f.Rule, What: f.What, Resource: f.Resource}
	switch {
	case f.Op == detect.OpExternal:
		ck.place(base, line, policy.ClassExternal, "changes "+resourceName(f)+", which cannot be snapshotted", "")
	case f.Op == detect.OpCritical && len(f.Targets) == 0:
		ck.place(base, line, policy.ClassCritical, "can destroy a whole disk or file system", "")
	case len(f.Targets) == 0:
		ck.place(base, line, policy.ClassUnresolved, "what it changes cannot be worked out", "")
	}
	for _, t := range f.Targets {
		a := base
		a.Target = t
		switch {
		case t.Unresolved:
			ck.place(a, line, policy.ClassUnresolved, t.Reason, "")
		case f.Op == detect.OpCritical:
			ck.place(a, line, policy.ClassCritical, "can destroy a whole disk or file system", t.Path)
		case ck.res.WrittenBefore(c, t.Path):
			ck.place(a, line, policy.ClassUnresolved, "an earlier command on the same line may create or replace it, so it cannot be checked now", t.Path)
		default:
			ck.path(a, line, t)
		}
	}
}

func resourceName(f *detect.Finding) string {
	if f.Resource != "" {
		return f.Resource
	}
	return "state outside the local files"
}

// place records an assessment for a fixed class.
func (ck *checker) place(a Assessment, line string, class policy.Class, why, path string) {
	a.Class, a.Why = class, why
	a.Action, a.By = ck.p.Decide(policy.Subject{Command: line, Finding: a.Rule, Path: path}, ck.p.Action(class), class)
	ck.add(a)
}

// path places a resolved local target.
func (ck *checker) path(a Assessment, line string, t detect.Target) {
	pl := ck.p.Place(t.Path, t.Follow, t.Recursive, t.Recursive && t.Git == "")
	exists := paths.Exists(t.Path)
	if !exists && pl.Class != policy.ClassProtected {
		// Nothing is lost by writing a path that does not exist yet. It
		// is still snapshotted as absent, so undo can remove it again.
		ck.snapshot(t, pl.Class)
		return
	}
	a.Class, a.Why = pl.Class, pl.Why
	a.Action, a.By = ck.p.Decide(policy.Subject{Command: line, Finding: a.Rule, Path: t.Path}, pl.Action, pl.Class)
	if pl.Path != t.Path {
		// Rules may name either form of the path.
		act, by := ck.p.Decide(policy.Subject{Command: line, Finding: a.Rule, Path: pl.Path}, pl.Action, pl.Class)
		if act.Rank() > a.Action.Rank() {
			a.Action, a.By = act, by
		}
	}
	ck.add(a)
	if a.Action != policy.Deny {
		ck.snapshot(t, pl.Class)
	}
}

func (ck *checker) snapshot(t detect.Target, class policy.Class) {
	if class == policy.ClassScratch || class == policy.ClassProtected {
		return
	}
	key := t.Path + "\x00" + string(t.Git)
	if ck.snap[key] {
		return
	}
	ck.snap[key] = true
	ck.d.Snapshot = append(ck.d.Snapshot, SnapshotItem{Path: t.Path, Recursive: t.Recursive, Follow: t.Follow, Git: t.Git, Repo: t.Repo})
}

// Subject names what an assessment is about: a path, a resource or an
// operand that could not be resolved.
func (a *Assessment) Subject() string {
	switch {
	case a.Target.Path != "":
		return paths.Display(a.Target.Path)
	case a.Resource != "":
		return a.Resource
	case a.Target.Raw != "":
		return a.Target.Raw
	}
	return ""
}

// Reason explains the decision in one line.
func (d *Decision) Reason() string {
	a := d.Deciding
	if a == nil {
		return "nothing in this command line deletes or overwrites data"
	}
	head, subject := a.Command, a.Subject()
	if head == "" {
		head = a.What
	} else if subject == "" {
		subject = a.What
	}
	s := head
	if subject != "" {
		s += ": " + subject
	}
	if a.Why != "" {
		s += ", " + a.Why
	}
	return s + " [" + a.Decider() + "]"
}
