package parser

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"mvdan.cc/sh/v3/syntax"

	"github.com/celaldrg/undoguard/internal/paths"
)

// Options configure Parse.
type Options struct {
	// Dir is the working directory the command line starts in.
	Dir string
	// Env is the environment, as from os.Environ. Nil means os.Environ().
	Env []string
	// MaxDepth limits how deep nested code is followed. Zero means 8.
	MaxDepth int
	// MaxScriptBytes caps script files that are read and followed. Zero
	// means 256 KiB.
	MaxScriptBytes int64
}

func (o Options) withDefaults() Options {
	if o.Dir == "" {
		if wd, err := os.Getwd(); err == nil {
			o.Dir = wd
		} else {
			o.Dir = "/"
		}
	}
	if o.Env == nil {
		o.Env = os.Environ()
	}
	if o.MaxDepth <= 0 {
		o.MaxDepth = 8
	}
	if o.MaxScriptBytes <= 0 {
		o.MaxScriptBytes = 256 << 10
	}
	return o
}

type walker struct {
	opts    Options
	res     *Result
	src     string
	via     []string
	depth   int
	pending *Stdin
	calls   map[string]int
}

// Parse analyses a command line and returns every simple command it would
// run, in order.
func Parse(src string, opts Options) *Result {
	opts = opts.withDefaults()
	w := &walker{opts: opts, res: &Result{}, calls: map[string]int{}}
	s := newScope(opts.Dir, opts.Env)
	w.script(src, s, "", nil)
	return w.res
}

// ParseArgv analyses one command given as an argument vector, as a shell
// wrapper receives it.
func ParseArgv(argv []string, opts Options) *Result {
	return Parse(QuoteArgs(argv), opts)
}

func (w *walker) issue(kind, detail string) {
	for _, is := range w.res.Issues {
		if is.Kind == kind && is.Detail == detail {
			return
		}
	}
	w.res.Issues = append(w.res.Issues, Issue{Kind: kind, Detail: detail})
}

func appendVia(via []string, label string) []string {
	out := make([]string, 0, len(via)+1)
	out = append(out, via...)
	return append(out, label)
}

// script parses src and walks it in scope s. label names the wrapper the
// code was found in; stdin, when set, feeds the first command.
func (w *walker) script(src string, s *scope, label string, stdin *Stdin) {
	if w.depth >= w.opts.MaxDepth {
		w.issue("depth", fmt.Sprintf("code nested more than %d levels deep", w.opts.MaxDepth))
		return
	}
	c := &walker{opts: w.opts, res: w.res, src: src, via: w.via, depth: w.depth + 1, calls: w.calls}
	if label != "" {
		c.via = appendVia(w.via, label)
	}
	p := syntax.NewParser(syntax.KeepComments(false), syntax.Variant(syntax.LangBash), syntax.RecoverErrors(3))
	f, err := p.Parse(strings.NewReader(src), "")
	if err != nil {
		w.issue("parse", err.Error())
	}
	if f == nil {
		return
	}
	c.pending = stdin
	c.stmts(f.Stmts, s)
}

func (w *walker) stmts(list []*syntax.Stmt, s *scope) {
	for _, st := range list {
		w.stmt(st, s)
	}
}

func (w *walker) stmt(st *syntax.Stmt, s *scope) {
	if st == nil {
		return
	}
	redirs, in := w.redirects(st.Redirs, s)
	if in != nil {
		w.pending = in
	}
	if call, ok := st.Cmd.(*syntax.CallExpr); ok {
		w.call(call, s, redirs)
		w.pending = nil
		return
	}
	if len(redirs) > 0 {
		w.res.Commands = append(w.res.Commands, &Command{
			Name: ":", Redirs: redirs, Dir: s.dir, DirKnown: s.dirKnown,
			Text: w.text(st), Via: w.via,
		})
	}
	w.command(st.Cmd, s)
	w.pending = nil
}

func (w *walker) command(cmd syntax.Command, s *scope) {
	switch c := cmd.(type) {
	case nil:
	case *syntax.BinaryCmd:
		switch c.Op {
		case syntax.Pipe, syntax.PipeAll:
			before := len(w.res.Commands)
			w.stmt(c.X, s.clone())
			var from *Command
			if n := len(w.res.Commands); n > before {
				from = w.res.Commands[n-1]
			}
			in := &Stdin{From: from}
			if from != nil {
				// Work the text out now, so that consumers outside
				// the parser see it too.
				if text, ok := w.commandOutput(from); ok {
					in.Text, in.Known = text, true
				}
			}
			w.pending = in
			w.stmt(c.Y, s.clone())
		default:
			w.stmt(c.X, s)
			w.stmt(c.Y, s)
		}
	case *syntax.Subshell:
		w.stmts(c.Stmts, s.clone())
	case *syntax.Block:
		w.stmts(c.Stmts, s)
	case *syntax.IfClause:
		w.ifClause(c, s)
	case *syntax.WhileClause:
		// The body is walked twice so that a later pass sees what an
		// earlier pass may have written, as a second iteration would.
		for range 2 {
			w.stmts(c.Cond, s)
			w.stmts(c.Do, s)
		}
	case *syntax.ForClause:
		w.forClause(c, s)
	case *syntax.CaseClause:
		w.expandJoined(c.Word, s)
		var branches []*scope
		for _, item := range c.Items {
			b := s.clone()
			w.stmts(item.Stmts, b)
			branches = append(branches, b)
		}
		branches = append(branches, s.clone())
		s.mergeFrom(branches)
	case *syntax.FuncDecl:
		if c.Name != nil && c.Body != nil {
			s.funcs[c.Name.Value] = c.Body
		}
	case *syntax.DeclClause:
		for _, a := range c.Args {
			w.assign(a, s, s)
		}
	case *syntax.TimeClause:
		w.stmt(c.Stmt, s)
	case *syntax.CoprocClause:
		w.stmt(c.Stmt, s.clone())
	default:
		w.walkEmbedded(cmd, s)
	}
}

func (w *walker) ifClause(c *syntax.IfClause, s *scope) {
	var branches []*scope
	hasElse := false
	for cl := c; cl != nil; cl = cl.Else {
		if cl != c && len(cl.Cond) == 0 {
			hasElse = true
		}
		w.stmts(cl.Cond, s)
		b := s.clone()
		w.stmts(cl.Then, b)
		branches = append(branches, b)
	}
	if !hasElse {
		branches = append(branches, s.clone())
	}
	s.mergeFrom(branches)
}

const maxLoopItems = 256

func (w *walker) forClause(c *syntax.ForClause, s *scope) {
	iter, ok := c.Loop.(*syntax.WordIter)
	if !ok || iter.Name == nil {
		if c.Loop != nil {
			w.walkEmbedded(c.Loop, s)
		}
		// Walked twice for the same reason as a while loop.
		w.stmts(c.Do, s)
		w.stmts(c.Do, s)
		return
	}
	var items []Word
	if len(iter.Items) == 0 {
		if s.posKnown {
			for _, p := range s.pos {
				items = append(items, Word{Value: p})
			}
		} else {
			items = []Word{{Dynamic: true}}
		}
	} else {
		for _, it := range iter.Items {
			items = append(items, w.expandWord(it, s)...)
		}
	}
	if len(items) > maxLoopItems {
		items = []Word{{Dynamic: true}}
	}
	name := iter.Name.Value
	for _, it := range items {
		if it.Dynamic {
			s.setDynamic(name)
		} else {
			s.set(name, it.Value)
		}
		w.stmts(c.Do, s)
	}
}

// walkEmbedded finds command and process substitutions inside nodes that
// are otherwise not commands, such as arithmetic and [[ ]] tests.
func (w *walker) walkEmbedded(node syntax.Node, s *scope) {
	if node == nil {
		return
	}
	syntax.Walk(node, func(n syntax.Node) bool {
		switch x := n.(type) {
		case *syntax.CmdSubst:
			w.subshell(x.Stmts, s, "$(...)")
			return false
		case *syntax.ProcSubst:
			w.subshell(x.Stmts, s, "process substitution")
			return false
		}
		return true
	})
}

func (w *walker) subshell(list []*syntax.Stmt, s *scope, label string) {
	savedVia, savedPending := w.via, w.pending
	if label != "" {
		w.via = appendVia(savedVia, label)
	}
	w.pending = nil
	w.stmts(list, s.clone())
	w.via, w.pending = savedVia, savedPending
}

func (w *walker) cmdSubst(cs *syntax.CmdSubst, s *scope) (string, bool) {
	before := len(w.res.Commands)
	w.subshell(cs.Stmts, s, "$(...)")
	added := w.res.Commands[before:]
	if len(cs.Stmts) == 1 && len(added) == 1 {
		if v, ok := staticOutput(added[0]); ok {
			return v, false
		}
	}
	return "", true
}

// staticOutput predicts the output of a few commands that are common in
// substitutions and depend only on their arguments.
func staticOutput(c *Command) (string, bool) {
	for _, a := range c.Args {
		if a.Dynamic {
			return "", false
		}
	}
	args := c.ArgValues()
	switch c.Name {
	case "pwd":
		if c.DirKnown {
			if len(args) == 1 && args[0] == "-P" {
				return paths.Resolve(c.Dir, true), true
			}
			return c.Dir, true
		}
	case "git":
		if len(args) == 2 && args[0] == "rev-parse" && args[1] == "--show-toplevel" && c.DirKnown {
			if r := paths.GitRoot(c.Dir); r != "" {
				return r, true
			}
		}
	case "echo":
		for len(args) > 0 && (args[0] == "-n" || args[0] == "-e" || args[0] == "-E") {
			args = args[1:]
		}
		return strings.Join(args, " "), true
	case "basename":
		if len(args) == 1 {
			return filepath.Base(args[0]), true
		}
	case "dirname":
		if len(args) == 1 {
			return filepath.Dir(args[0]), true
		}
	case "realpath", "readlink":
		if c.Name == "readlink" {
			if len(args) != 2 || (args[0] != "-f" && args[0] != "-e" && args[0] != "-m") {
				return "", false
			}
			args = args[1:]
		}
		if len(args) == 1 && c.DirKnown {
			return paths.Resolve(paths.Abs(c.Dir, args[0]), true), true
		}
	}
	return "", false
}

func (w *walker) call(c *syntax.CallExpr, s *scope, redirs []Redirect) {
	text := w.text(c)
	if len(c.Args) == 0 {
		for _, a := range c.Assigns {
			w.assign(a, s, s)
		}
		if len(redirs) > 0 {
			w.res.Commands = append(w.res.Commands, &Command{
				Name: ":", Redirs: redirs, Dir: s.dir, DirKnown: s.dirKnown, Text: text, Via: w.via,
			})
		}
		return
	}
	env := s
	if len(c.Assigns) > 0 {
		env = s.clone()
		for _, a := range c.Assigns {
			w.assign(a, env, s)
			if a.Name != nil {
				env.exported[a.Name.Value] = true
			}
		}
	}
	var argv []Word
	for _, a := range c.Args {
		argv = append(argv, w.expandWord(a, s)...)
	}
	stdin := w.pending
	w.pending = nil
	w.run(argv, env, s, redirs, stdin, text)
}

// assign applies one assignment, evaluating the value in eval and storing
// it in target.
func (w *walker) assign(a *syntax.Assign, target, eval *scope) {
	if a == nil || a.Name == nil {
		return
	}
	name := a.Name.Value
	if a.Array != nil || a.Index != nil {
		w.walkEmbedded(a, eval)
		target.setDynamic(name)
		return
	}
	if a.Naked {
		return
	}
	v := w.expandJoined(a.Value, eval)
	if a.Append {
		cur, _, known := eval.lookup(name)
		if !known {
			v.Dynamic = true
		}
		v.Value = cur + v.Value
	}
	if v.Dynamic {
		target.setDynamic(name)
	} else {
		target.set(name, v.Value)
	}
}

func (w *walker) redirects(rs []*syntax.Redirect, s *scope) ([]Redirect, *Stdin) {
	var out []Redirect
	var in *Stdin
	for _, r := range rs {
		rd := Redirect{Op: redirOp(r.Op)}
		if r.N != nil {
			rd.Fd = r.N.Value
		}
		// Only input on descriptor 0 is standard input.
		feedsStdin := rd.Fd == "" || rd.Fd == "0"
		switch r.Op {
		case syntax.Hdoc, syntax.DashHdoc:
			if r.Hdoc != nil {
				rd.Body = w.text(r.Hdoc)
				w.walkEmbedded(r.Hdoc, s)
			}
			if feedsStdin {
				in = &Stdin{Text: rd.Body, Known: true}
			}
		case syntax.WordHdoc:
			wv := w.expandJoined(r.Word, s)
			rd.Body = wv.Value + "\n"
			if feedsStdin {
				in = &Stdin{Text: rd.Body, Known: !wv.Dynamic}
			}
		case syntax.DplIn, syntax.DplOut:
			wv := w.expandJoined(r.Word, s)
			rd.Target = wv
			if r.Op == syntax.DplOut && !isDigits(wv.Value) && wv.Value != "-" && !wv.Dynamic {
				rd.Op = "&>"
			}
		default:
			ws := w.expandWord(r.Word, s)
			switch len(ws) {
			case 0:
				rd.Target = Word{Dynamic: true}
			case 1:
				rd.Target = ws[0]
			default:
				rd.Target = Word{Value: ws[0].Value, Dynamic: true}
			}
			if r.Op == syntax.RdrIn && feedsStdin {
				in = &Stdin{}
				p := rd.Target.Value
				if !filepath.IsAbs(p) {
					p = filepath.Join(s.dir, p)
				}
				// The command itself is not recorded yet, so every recorded
				// command came before it.
				if rd.Target.Dynamic || (!filepath.IsAbs(rd.Target.Value) && !s.dirKnown) || w.writtenEarlier(p, 0) {
					break
				}
				if data, ok := w.readSmall(p); ok {
					in = &Stdin{Text: string(data), Known: true}
				}
			}
		}
		out = append(out, rd)
	}
	return out, in
}

func redirOp(op syntax.RedirOperator) string {
	switch op {
	case syntax.RdrOut:
		return ">"
	case syntax.AppOut:
		return ">>"
	case syntax.RdrIn:
		return "<"
	case syntax.RdrInOut:
		return "<>"
	case syntax.DplIn:
		return "<&"
	case syntax.DplOut:
		return ">&"
	case syntax.RdrClob:
		return ">|"
	case syntax.AppClob:
		return ">>"
	case syntax.Hdoc:
		return "<<"
	case syntax.DashHdoc:
		return "<<-"
	case syntax.WordHdoc:
		return "<<<"
	case syntax.RdrAll:
		return "&>"
	case syntax.RdrAllClob:
		return "&>"
	case syntax.AppAll:
		return "&>>"
	}
	return "?"
}

// text returns the source text of a node, or "" when its offsets do not
// fall inside the current source.
func (w *walker) text(n syntax.Node) string {
	if n == nil {
		return ""
	}
	start, end := int(n.Pos().Offset()), int(n.End().Offset())
	if start < 0 || end > len(w.src) || start >= end {
		return ""
	}
	return w.src[start:end]
}

// readSmall reads a file if it is a regular file no larger than the
// script size limit.
func (w *walker) readSmall(p string) ([]byte, bool) {
	fi, err := os.Stat(p)
	if err != nil || !fi.Mode().IsRegular() || fi.Size() > w.opts.MaxScriptBytes {
		return nil, false
	}
	data, err := os.ReadFile(p)
	if err != nil {
		return nil, false
	}
	return data, true
}
