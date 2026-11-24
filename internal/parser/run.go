package parser

import (
	"path/filepath"
	"strings"

	"mvdan.cc/sh/v3/syntax"

	"github.com/celaldrg/undoguard/internal/paths"
)

// runState is what wrappers can change for the command they run.
type runState struct {
	env      *scope
	cloned   bool
	dir      string
	dirKnown bool
	via      []string
}

func (r *runState) ownEnv() *scope {
	if !r.cloned {
		r.env = r.env.clone()
		r.cloned = true
	}
	return r.env
}

func (r *runState) chdir(target Word) {
	tmp := &scope{dir: r.dir, dirKnown: r.dirKnown}
	tmp.chdir(target)
	r.dir, r.dirKnown = tmp.dir, tmp.dirKnown
}

// run handles one expanded command: wrappers are stripped, builtins that
// change shell state are applied, nested code is followed and the command
// itself is recorded.
func (w *walker) run(argv []Word, env, s *scope, redirs []Redirect, stdin *Stdin, text string) {
	rs := &runState{env: env, dir: s.dir, dirKnown: s.dirKnown, via: w.via}
	record := func(argv []Word) *Command {
		c := &Command{Redirs: redirs, Dir: rs.dir, DirKnown: rs.dirKnown, Text: text, Via: rs.via, Stdin: stdin}
		if len(argv) == 0 {
			c.Name = ":"
		} else {
			c.Name = filepath.Base(argv[0].Value)
			if strings.Contains(argv[0].Value, "/") {
				c.Path = argv[0].Value
			}
			c.Args = argv[1:]
		}
		w.res.Commands = append(w.res.Commands, c)
		return c
	}
	// external is set once a wrapper that is a separate program (sudo,
	// env, nice...) has been stripped: the command then never reaches a
	// shell function, and builtins it names cannot change this shell.
	external := false
	for guard := 0; guard < 32; guard++ {
		if len(argv) == 0 {
			if len(redirs) > 0 {
				record(nil)
			}
			return
		}
		if argv[0].Dynamic {
			w.issue("dynamic-command", "the program name is only known at run time: "+short(text))
			record(argv).NameDynamic = true
			return
		}
		next, label, wrapped := w.unwrap(argv, rs, stdin)
		if !wrapped {
			break
		}
		if next == nil {
			if len(redirs) > 0 {
				record(nil)
			}
			return
		}
		rs.via = appendVia(rs.via, label)
		if label != "command" && label != "builtin" && label != "time" {
			external = true
		}
		argv = next
	}

	name := filepath.Base(argv[0].Value)
	args := argv[1:]
	hasSlash := strings.Contains(argv[0].Value, "/")

	if body, ok := rs.env.funcs[name]; ok && !hasSlash && !external {
		if len(redirs) > 0 {
			record(nil)
		}
		w.callFunc(name, body, args, rs, s, stdin)
		return
	}

	record(argv)
	sub := rs.env.clone()
	sub.dir, sub.dirKnown = rs.dir, rs.dirKnown
	saved := w.via
	w.via = rs.via
	defer func() { w.via = saved }()

	switch {
	case (external || hasSlash) && stateBuiltins[name]:
		// A separate program of that name cannot touch this shell.
	case name == "cd":
		w.builtinCd(args, s)
	case name == "pushd":
		if len(args) > 0 && !strings.HasPrefix(args[0].Value, "-") && !strings.HasPrefix(args[0].Value, "+") {
			s.dirStack = append(s.dirStack, s.dir)
			s.chdir(args[0])
		} else {
			s.dirKnown = false
		}
	case name == "popd":
		if n := len(s.dirStack); n > 0 && len(args) == 0 {
			s.dir, s.dirKnown = s.dirStack[n-1], true
			s.dirStack = s.dirStack[:n-1]
		} else {
			s.dirKnown = false
		}
	case name == "set":
		builtinSet(args, s)
	case name == "shift":
		if s.posKnown {
			n := 1
			if len(args) > 0 {
				n = atoiDefault(args[0].Value, 1)
			}
			if n <= len(s.pos) {
				s.pos = s.pos[n:]
			} else {
				s.posKnown = false
			}
		}
	case name == "unset":
		funcs := false
		for _, a := range args {
			switch a.Value {
			case "-f":
				funcs = true
			case "-v", "-n":
			default:
				if funcs {
					delete(s.funcs, a.Value)
				} else {
					s.unset(a.Value)
				}
			}
		}
	case name == "read" || name == "mapfile" || name == "readarray" || name == "getopts":
		builtinRead(name, args, s)
	case name == "export" || name == "declare" || name == "typeset" || name == "local" || name == "readonly":
		builtinDeclare(name, args, s)
	case name == "source" || name == ".":
		w.source(args, s)
	case name == "eval":
		code, dyn := joinWords(args)
		if dyn {
			w.issue("dynamic-code", "eval of code that is only known at run time: "+short(text))
		}
		w.script(code, s, "eval", stdin)
	case name == "trap":
		if len(args) >= 2 {
			code := args[0]
			if code.Value != "-" && code.Value != "" && !strings.HasPrefix(code.Value, "-") {
				if code.Dynamic {
					w.issue("dynamic-code", "trap with code that is only known at run time")
				}
				w.script(code.Value, s.clone(), "trap", nil)
			}
		}
	case isShell(name):
		w.shell(name, args, sub, stdin)
	case name == "find":
		w.find(args, sub)
	case name == "xargs":
		w.xargs(args, sub, stdin)
	default:
		if code, lang, ok := interpreterCode(name, args); ok {
			w.inlineCode(lang, code)
		} else if hasSlash {
			w.directScript(argv, sub, stdin)
		}
	}
}

func (w *walker) builtinCd(args []Word, s *scope) {
	for len(args) > 0 && strings.HasPrefix(args[0].Value, "-") && args[0].Value != "-" {
		if args[0].Value == "--" {
			args = args[1:]
			break
		}
		args = args[1:]
	}
	switch {
	case len(args) == 0:
		s.chdir(Word{Value: paths.Home()})
	case args[0].Value == "-":
		s.dirKnown = false
	default:
		s.chdir(args[0])
	}
}

// stateBuiltins are the builtins whose only effect is on the shell's own
// state, which the walker tracks.
var stateBuiltins = setOf(
	"cd", "pushd", "popd", "set", "shift", "unset", "read", "mapfile", "readarray",
	"getopts", "export", "declare", "typeset", "local", "readonly",
)

func builtinDeclare(name string, args []Word, s *scope) {
	export := name == "export"
	funcs := false
	for _, a := range args {
		v := a.Value
		if strings.HasPrefix(v, "-") && !strings.Contains(v, "=") {
			if strings.Contains(v, "x") {
				export = true
			}
			if strings.Contains(v, "f") {
				funcs = true
			}
			continue
		}
		k, val, hasVal := strings.Cut(v, "=")
		if !isAssignment(k + "=") {
			continue
		}
		if funcs {
			if export {
				s.xfuncs[k] = true
			}
			continue
		}
		if hasVal {
			if a.Dynamic {
				s.setDynamic(k)
			} else {
				s.set(k, val)
			}
		}
		if export {
			s.exported[k] = true
		}
	}
}

func builtinSet(args []Word, s *scope) {
	start := -1
	for i, a := range args {
		if a.Value == "--" {
			start = i + 1
			break
		}
		if i == 0 && !strings.HasPrefix(a.Value, "-") && !strings.HasPrefix(a.Value, "+") {
			start = 0
			break
		}
		if a.Value == "-o" || a.Value == "+o" {
			if i+1 < len(args) && args[i+1].Value == "allexport" {
				s.allExport = a.Value == "-o"
			}
			return
		}
		if strings.HasPrefix(a.Value, "-") && strings.Contains(a.Value, "a") {
			s.allExport = true
		}
	}
	if start < 0 {
		return
	}
	s.pos = nil
	s.posKnown = true
	for _, a := range args[start:] {
		if a.Dynamic {
			s.posKnown = false
		}
		s.pos = append(s.pos, a.Value)
	}
}

func builtinRead(name string, args []Word, s *scope) {
	valueOpts := map[string]bool{
		"-a": true, "-d": true, "-i": true, "-n": true, "-N": true, "-p": true, "-t": true, "-u": true,
		"-C": true, "-c": true, "-O": true, "-s": true,
	}
	if name == "read" {
		valueOpts["-s"] = false
	}
	var names []string
	for i := 0; i < len(args); i++ {
		v := args[i].Value
		if strings.HasPrefix(v, "-") && len(v) > 1 {
			if valueOpts[v] {
				if v == "-a" && name == "read" && i+1 < len(args) {
					names = append(names, args[i+1].Value)
				}
				i++
			}
			continue
		}
		names = append(names, v)
	}
	switch name {
	case "getopts":
		s.setDynamic("OPTARG")
		s.setDynamic("OPTIND")
		if len(names) > 1 {
			names = names[1:2]
		}
	case "mapfile", "readarray":
		if len(names) == 0 {
			names = []string{"MAPFILE"}
		}
	default:
		if len(names) == 0 {
			names = []string{"REPLY"}
		}
	}
	for _, n := range names {
		s.setDynamic(n)
	}
}

// callFunc runs a shell function body with the call's arguments as its
// positional parameters, in the caller's shell.
func (w *walker) callFunc(name string, body *syntax.Stmt, args []Word, rs *runState, s *scope, stdin *Stdin) {
	if w.calls[name] >= 2 || w.depth >= w.opts.MaxDepth {
		return
	}
	w.calls[name]++
	defer func() { w.calls[name]-- }()
	savedPos, savedKnown := s.pos, s.posKnown
	s.pos, s.posKnown = nil, true
	for _, a := range args {
		if a.Dynamic {
			s.posKnown = false
		}
		s.pos = append(s.pos, a.Value)
	}
	savedVia := w.via
	w.via = appendVia(rs.via, name+"()")
	w.pending = stdin
	w.stmt(body, s)
	w.via = savedVia
	s.pos, s.posKnown = savedPos, savedKnown
}

func (w *walker) source(args []Word, s *scope) {
	if len(args) == 0 {
		return
	}
	file := args[0]
	if file.Dynamic {
		w.issue("script", "sources a file whose name is only known at run time")
		return
	}
	p, ok := w.findScript(file.Value, s.dir, s.dirKnown, true)
	if !ok {
		w.issue("script", "sources "+file.Value+", which could not be read")
		return
	}
	data, ok := w.readScript(p, file.Value, "source")
	if !ok {
		return
	}
	if len(args) > 1 {
		savedPos, savedKnown := s.pos, s.posKnown
		s.pos, s.posKnown = nil, true
		for _, a := range args[1:] {
			if a.Dynamic {
				s.posKnown = false
			}
			s.pos = append(s.pos, a.Value)
		}
		defer func() { s.pos, s.posKnown = savedPos, savedKnown }()
	}
	w.script(string(data), s, "source "+filepath.Base(p), nil)
}

func joinWords(ws []Word) (string, bool) {
	parts := make([]string, len(ws))
	dyn := false
	for i, a := range ws {
		parts[i] = a.Value
		dyn = dyn || a.Dynamic
	}
	return strings.Join(parts, " "), dyn
}

func atoiDefault(s string, def int) int {
	n := 0
	if s == "" {
		return def
	}
	for i := 0; i < len(s); i++ {
		if s[i] < '0' || s[i] > '9' {
			return def
		}
		n = n*10 + int(s[i]-'0')
		if n > 1<<20 {
			return def
		}
	}
	return n
}

func short(s string) string {
	s = strings.Join(strings.Fields(s), " ")
	if len(s) > 120 {
		return s[:117] + "..."
	}
	return s
}
