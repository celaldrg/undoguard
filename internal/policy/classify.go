package policy

import (
	"path/filepath"
	"regexp"
	"strings"

	"github.com/celaldrg/undoguard/internal/paths"
)

// Class is the kind of place a target is in.
type Class string

// The classes a target can fall in.
const (
	ClassProtected   Class = "protected"
	ClassPolicyFile  Class = "policy file"
	ClassProjectRoot Class = "project root"
	ClassProject     Class = "project"
	ClassScratch     Class = "scratch"
	ClassHome        Class = "home"
	ClassElsewhere   Class = "elsewhere"
	ClassUnresolved  Class = "unresolved"
	ClassExternal    Class = "external"
	ClassCritical    Class = "critical"
)

// Action returns the configured action for a class.
func (p *Policy) Action(c Class) Action {
	switch c {
	case ClassProtected:
		return Deny
	case ClassPolicyFile:
		return Ask
	case ClassProjectRoot:
		return p.Classes.ProjectRoot
	case ClassProject:
		return p.Classes.Project
	case ClassScratch:
		return p.Classes.Scratch
	case ClassHome:
		return p.Classes.Home
	case ClassElsewhere:
		return p.Classes.Elsewhere
	case ClassUnresolved:
		return p.Classes.Unresolved
	case ClassExternal:
		return p.Classes.External
	default:
		return p.Classes.Critical
	}
}

// Placement is where a target was found to be.
type Placement struct {
	Class  Class
	Action Action
	// Why explains the class, such as "inside protected path ~/.ssh".
	Why string
	// Path is the form of the path that decided: as written, or after
	// resolving symlinks.
	Path string
}

// Place classifies a path. Every form of it is checked, the path as
// written, with its directories resolved, and fully resolved when the
// command follows a final symlink, and the strictest result wins.
// recursive marks operations on a whole tree; wholeRoot is false when a
// recursive operation on the project root only touches files in one git
// state, which is ordinary work inside the project.
func (p *Policy) Place(path string, follow, recursive, wholeRoot bool) Placement {
	forms := []string{filepath.Clean(path), paths.Resolve(path, false)}
	if follow {
		forms = append(forms, paths.Resolve(path, true))
	}
	var best Placement
	for i, f := range forms {
		if i > 0 && f == forms[0] {
			continue
		}
		pl := p.placeOne(f, recursive, wholeRoot)
		if i > 0 && pl.Class != ClassProtected && pl.Why != "" {
			pl.Why += " (after resolving symlinks)"
		}
		if i == 0 || pl.Action.Rank() > best.Action.Rank() {
			best = pl
		}
	}
	return best
}

func (p *Policy) placeOne(f string, recursive, wholeRoot bool) Placement {
	pl := func(c Class, why string) Placement {
		return Placement{Class: c, Action: p.Action(c), Why: why, Path: f}
	}
	for _, pr := range p.Protected {
		if paths.Within(f, pr) {
			return pl(ClassProtected, "inside protected path "+paths.Display(pr))
		}
		if recursive && paths.Below(pr, f) {
			return pl(ClassProtected, "contains protected path "+paths.Display(pr))
		}
	}
	homes := p.forms(p.Home)
	if recursive {
		for _, h := range homes {
			if paths.Below(h, f) {
				return pl(ClassHome, "contains the home directory")
			}
		}
	}
	for _, root := range p.forms(p.Project) {
		if f == filepath.Join(root, ProjectFileName) {
			return pl(ClassPolicyFile, "the project's undoguard policy")
		}
		if f == root && wholeRoot {
			return pl(ClassProjectRoot, "the project directory itself")
		}
		if paths.Within(f, root) {
			return pl(ClassProject, "inside the project "+paths.Display(root))
		}
	}
	for _, s := range p.Scratch {
		if paths.Below(f, s) {
			return pl(ClassScratch, "inside the temporary directory "+s)
		}
	}
	for _, h := range homes {
		if paths.Within(f, h) {
			return pl(ClassHome, "in the home directory, outside the project")
		}
	}
	return pl(ClassElsewhere, "outside the project and the home directory")
}

// forms returns a directory as configured and with symlinks resolved.
func (p *Policy) forms(dir string) []string {
	if dir == "" {
		return nil
	}
	if r := paths.Resolve(dir, true); r != dir {
		return []string{dir, r}
	}
	return []string{dir}
}

// Subject is what rules are matched against: a simple command, and
// optionally one of its findings and one of its target paths.
type Subject struct {
	// Command is the program name followed by its arguments.
	Command string
	// Finding is the detector rule, or "" for the command alone.
	Finding string
	// Path is the target path, or "" when there is none.
	Path string
}

// Decide applies the rules that match s to base, the action of the class
// the subject is in. The strictest matching user rule replaces base, but
// cannot loosen a protected path. The strictest matching project rule can
// only tighten. The returned rule is the one that set the action, or nil
// when the class decided.
func (p *Policy) Decide(s Subject, base Action, class Class) (Action, *LoadedRule) {
	var user, proj *LoadedRule
	for i := range p.Rules {
		r := &p.Rules[i]
		if !r.matches(s) {
			continue
		}
		best := &user
		if r.Project {
			best = &proj
		}
		if *best == nil || r.Act.Rank() > (*best).Act.Rank() {
			*best = r
		}
	}
	act := base
	var by *LoadedRule
	if user != nil && (class != ClassProtected || user.Act.Rank() > act.Rank()) {
		act, by = user.Act, user
	}
	if proj != nil && proj.Act.Rank() > act.Rank() {
		act, by = proj.Act, proj
	}
	return act, by
}

func (r *LoadedRule) matches(s Subject) bool {
	if r.command != nil && !r.command.MatchString(s.Command) {
		return false
	}
	if r.finding != nil && (s.Finding == "" || !r.finding.MatchString(s.Finding)) {
		return false
	}
	if r.path != nil && (s.Path == "" || !r.path.MatchString(s.Path)) {
		return false
	}
	return true
}

// compile prepares the rule's patterns.
func (r *LoadedRule) compile() error {
	var err error
	if r.Command != "" {
		if r.command, err = globRegexp(r.Command, false); err != nil {
			return err
		}
	}
	if r.Finding != "" {
		if r.finding, err = globRegexp(r.Finding, false); err != nil {
			return err
		}
	}
	if r.Path != "" {
		if r.path, err = globRegexp(expand(r.Path), true); err != nil {
			return err
		}
	}
	return nil
}

// globRegexp turns a glob into an anchored regular expression. In path
// patterns "*" and "?" stay within one path element, "**" crosses them,
// and a trailing "/**" also matches the directory itself. Elsewhere "*"
// matches anything.
func globRegexp(pat string, path bool) (*regexp.Regexp, error) {
	var b strings.Builder
	b.WriteString(`(?s)^`)
	tail := "$"
	if path && strings.HasSuffix(pat, "/**") {
		pat = strings.TrimSuffix(pat, "/**")
		tail = "(/.*)?$"
	}
	rs := []rune(pat)
	for i := 0; i < len(rs); i++ {
		switch r := rs[i]; {
		case r == '*' && i+1 < len(rs) && rs[i+1] == '*':
			b.WriteString(".*")
			i++
		case r == '*' && path:
			b.WriteString("[^/]*")
		case r == '*':
			b.WriteString(".*")
		case r == '?' && path:
			b.WriteString("[^/]")
		case r == '?':
			b.WriteString(".")
		default:
			b.WriteString(regexp.QuoteMeta(string(r)))
		}
	}
	b.WriteString(tail)
	return regexp.Compile(b.String())
}
