package parser

import (
	"path/filepath"
	"strconv"
	"strings"

	"mvdan.cc/sh/v3/syntax"
)

// scope is the shell state the walker tracks while it reads a script:
// variables, the working directory, positional parameters and functions.
// Subshells work on a clone.
type scope struct {
	vars     map[string]string
	dynamic  map[string]bool
	dir      string
	dirKnown bool
	arg0     string
	pos      []string
	posKnown bool
	funcs    map[string]*syntax.Stmt
	dirStack []string
	// exported names the variables a child process sees; xfuncs names
	// functions exported with export -f; allExport is set -a.
	exported  map[string]bool
	xfuncs    map[string]bool
	allExport bool
}

func newScope(dir string, env []string) *scope {
	s := &scope{
		vars:     map[string]string{},
		dynamic:  map[string]bool{},
		dir:      filepath.Clean(dir),
		dirKnown: true,
		funcs:    map[string]*syntax.Stmt{},
		exported: map[string]bool{},
		xfuncs:   map[string]bool{},
	}
	for _, kv := range env {
		if i := strings.IndexByte(kv, '='); i > 0 {
			s.vars[kv[:i]] = kv[i+1:]
			s.exported[kv[:i]] = true
		}
	}
	return s
}

func copyBools(m map[string]bool) map[string]bool {
	out := make(map[string]bool, len(m))
	for k, v := range m {
		out[k] = v
	}
	return out
}

func (s *scope) clone() *scope {
	c := *s
	c.vars = make(map[string]string, len(s.vars))
	for k, v := range s.vars {
		c.vars[k] = v
	}
	c.dynamic = copyBools(s.dynamic)
	c.exported = copyBools(s.exported)
	c.xfuncs = copyBools(s.xfuncs)
	c.funcs = make(map[string]*syntax.Stmt, len(s.funcs))
	for k, v := range s.funcs {
		c.funcs[k] = v
	}
	c.pos = append([]string(nil), s.pos...)
	c.dirStack = append([]string(nil), s.dirStack...)
	return &c
}

// setEnv sets a variable that is placed in a command's environment.
func (s *scope) setEnv(name, val string, dynamic bool) {
	if dynamic {
		s.setDynamic(name)
	} else {
		s.set(name, val)
	}
	s.exported[name] = true
}

// child returns the state a new shell process starts with: exported
// variables keep their values, other variables become unknown (they are
// unset in the child, but export tracking is not perfect), and functions
// are gone unless they were exported.
func (s *scope) child(dir string, dirKnown bool) *scope {
	c := s.clone()
	if !s.allExport {
		for k := range s.vars {
			if !s.exported[k] {
				c.setDynamic(k)
			}
		}
	}
	c.funcs = map[string]*syntax.Stmt{}
	for k := range s.xfuncs {
		if f, ok := s.funcs[k]; ok {
			c.funcs[k] = f
		}
	}
	c.pos, c.posKnown, c.arg0 = nil, true, ""
	c.dir, c.dirKnown = dir, dirKnown
	c.dirStack = nil
	return c
}

func (s *scope) set(name, val string) {
	s.vars[name] = val
	delete(s.dynamic, name)
}

func (s *scope) setDynamic(name string) {
	delete(s.vars, name)
	s.dynamic[name] = true
}

func (s *scope) unset(name string) {
	delete(s.vars, name)
	delete(s.dynamic, name)
}

// chdir moves the working directory. A target that does not exist leaves
// the directory where it was, which is what the shell does when cd fails.
func (s *scope) chdir(target Word) {
	if target.Dynamic {
		s.dirKnown = false
		return
	}
	if !s.dirKnown && !filepath.IsAbs(target.Value) {
		return
	}
	next := target.Value
	if !filepath.IsAbs(next) {
		next = filepath.Join(s.dir, next)
	}
	next = filepath.Clean(next)
	if !dirExists(next) {
		return
	}
	s.dir = next
	s.dirKnown = true
}

// mergeFrom folds the state of alternative branches back into s: anything
// the branches disagree on becomes unknown.
func (s *scope) mergeFrom(branches []*scope) {
	if len(branches) == 0 {
		return
	}
	first := branches[0]
	same := true
	for _, b := range branches[1:] {
		if b.dir != first.dir || b.dirKnown != first.dirKnown {
			same = false
		}
	}
	if same {
		s.dir, s.dirKnown = first.dir, first.dirKnown
	} else {
		s.dirKnown = false
	}
	names := map[string]bool{}
	for _, b := range branches {
		for k := range b.vars {
			names[k] = true
		}
		for k := range b.dynamic {
			names[k] = true
		}
		for k, f := range b.funcs {
			s.funcs[k] = f
		}
	}
	for k := range names {
		v0, ok0 := first.vars[k]
		agree := !first.dynamic[k]
		for _, b := range branches[1:] {
			v, ok := b.vars[k]
			if ok != ok0 || v != v0 || b.dynamic[k] {
				agree = false
			}
		}
		switch {
		case agree && ok0:
			s.set(k, v0)
		case agree:
			s.unset(k)
		default:
			s.setDynamic(k)
		}
	}
}

var runtimeVars = setOf(
	"?", "$", "!", "-", "_", "RANDOM", "SRANDOM", "SECONDS", "LINENO", "BASHPID",
	"EPOCHSECONDS", "EPOCHREALTIME", "PPID", "OLDPWD", "REPLY",
)

// lookup returns a parameter's value, whether it is set, and whether the
// answer is certain. Ordinary variables missing from the environment are
// reported as certainly unset.
func (s *scope) lookup(name string) (val string, set bool, known bool) {
	switch {
	case name == "PWD":
		return s.dir, true, s.dirKnown
	case name == "#":
		if s.posKnown {
			return strconv.Itoa(len(s.pos)), true, true
		}
		return "", false, false
	case name == "@" || name == "*":
		if s.posKnown {
			return strings.Join(s.pos, " "), len(s.pos) > 0, true
		}
		return "", false, false
	case name == "0":
		if s.arg0 != "" {
			return s.arg0, true, true
		}
		return "", false, false
	case isDigits(name):
		if !s.posKnown {
			return "", false, false
		}
		n, err := strconv.Atoi(name)
		if err == nil && n >= 1 && n <= len(s.pos) {
			return s.pos[n-1], true, true
		}
		return "", false, true
	case runtimeVars[name]:
		return "", false, false
	case s.dynamic[name]:
		return "", true, false
	}
	if v, ok := s.vars[name]; ok {
		return v, true, true
	}
	return "", false, true
}

func isDigits(s string) bool {
	if s == "" {
		return false
	}
	for i := 0; i < len(s); i++ {
		if s[i] < '0' || s[i] > '9' {
			return false
		}
	}
	return true
}
