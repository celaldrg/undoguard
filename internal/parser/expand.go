package parser

import (
	"os/user"
	"path/filepath"
	"strconv"
	"strings"
	"unicode/utf8"

	"mvdan.cc/sh/v3/syntax"

	"github.com/celaldrg/undoguard/internal/paths"
)

// field is one word under construction.
type field struct {
	pat     string // pattern text, see pattern.go
	glob    bool   // pat holds a live glob metacharacter
	dynamic bool
	keep    bool // the field survives even when empty: it had quotes
}

type builder struct {
	w      *walker
	s      *scope
	split  bool
	fields []*field
}

func (b *builder) cur() *field { return b.fields[len(b.fields)-1] }

func (b *builder) addQuoted(text string) {
	f := b.cur()
	f.pat += escAll(text)
	f.keep = true
}

// addBare appends unquoted literal source text: backslash escapes are
// resolved and metacharacters stay live.
func (b *builder) addBare(text string) {
	f := b.cur()
	var sb strings.Builder
	for i := 0; i < len(text); i++ {
		c := text[i]
		if c == '\\' && i+1 < len(text) {
			i++
			if text[i] == '\n' {
				continue
			}
			sb.WriteString(escAll(text[i : i+1]))
			f.keep = true
			continue
		}
		if c == '*' || c == '?' || c == '[' {
			f.glob = true
		}
		sb.WriteByte(c)
	}
	f.pat += sb.String()
}

// addExpansion appends the result of a parameter expansion or command
// substitution. Unquoted results are split into fields and globbed.
func (b *builder) addExpansion(val string, quoted, dynamic bool) {
	if dynamic {
		b.cur().dynamic = true
	}
	if quoted {
		b.addQuoted(val)
		return
	}
	if !b.split {
		f := b.cur()
		f.pat += escNoGlob(val)
		if strings.ContainsAny(val, "*?[") {
			f.glob = true
		}
		return
	}
	isIFS := func(r rune) bool { return r == ' ' || r == '\t' || r == '\n' }
	parts := strings.FieldsFunc(val, isIFS)
	startsSep := val != "" && isIFS(rune(val[0]))
	endsSep := val != "" && isIFS(rune(val[len(val)-1]))
	for i, p := range parts {
		if i > 0 || startsSep {
			b.breakField()
		}
		f := b.cur()
		f.pat += escNoGlob(p)
		if strings.ContainsAny(p, "*?[") {
			f.glob = true
		}
	}
	if endsSep || (val != "" && len(parts) == 0) {
		b.breakField()
	}
}

func (b *builder) breakField() {
	f := b.cur()
	if f.pat != "" || f.keep || f.dynamic {
		b.fields = append(b.fields, &field{})
	}
}

func (b *builder) parts(parts []syntax.WordPart, tilde bool) {
	for i, part := range parts {
		switch p := part.(type) {
		case *syntax.Lit:
			v := p.Value
			if i == 0 && tilde && strings.HasPrefix(v, "~") {
				if home, rest, ok := tildePrefix(v, len(parts) == 1); ok {
					b.cur().pat += escAll(home)
					b.cur().keep = true
					v = rest
				}
			}
			b.addBare(v)
		case *syntax.SglQuoted:
			v := p.Value
			if p.Dollar {
				v = ansiC(v)
			}
			b.addQuoted(v)
		case *syntax.DblQuoted:
			b.cur().keep = true
			b.dblParts(p.Parts)
		case *syntax.ParamExp:
			val, dyn := b.w.paramValue(p, b.s)
			b.addExpansion(val, false, dyn)
		case *syntax.CmdSubst:
			val, dyn := b.w.cmdSubst(p, b.s)
			b.addExpansion(val, false, dyn)
		case *syntax.ArithmExp:
			b.w.walkEmbedded(p, b.s)
			b.addExpansion("", false, true)
		case *syntax.ProcSubst:
			b.w.subshell(p.Stmts, b.s, "process substitution")
			b.addQuoted("/dev/fd/63")
		default:
			b.cur().dynamic = true
		}
	}
}

func (b *builder) dblParts(parts []syntax.WordPart) {
	for _, part := range parts {
		switch p := part.(type) {
		case *syntax.Lit:
			b.addQuoted(dqUnescape(p.Value))
		case *syntax.ParamExp:
			if p.Param != nil && p.Param.Value == "@" && p.Exp == nil && !p.Length && b.s.posKnown {
				for j, a := range b.s.pos {
					if j > 0 {
						b.fields = append(b.fields, &field{keep: true})
					}
					b.addQuoted(a)
				}
				continue
			}
			val, dyn := b.w.paramValue(p, b.s)
			b.addExpansion(val, true, dyn)
		case *syntax.CmdSubst:
			val, dyn := b.w.cmdSubst(p, b.s)
			b.addExpansion(val, true, dyn)
		case *syntax.ArithmExp:
			b.w.walkEmbedded(p, b.s)
			b.addExpansion("", true, true)
		default:
			b.cur().dynamic = true
		}
	}
}

// tildePrefix expands ~ and ~user at the start of an unquoted word.
func tildePrefix(v string, whole bool) (home, rest string, ok bool) {
	name := v[1:]
	rest = ""
	if i := strings.IndexByte(v, '/'); i >= 0 {
		name, rest = v[1:i], v[i:]
	} else if !whole {
		return "", "", false
	}
	if name == "" {
		h := paths.Home()
		return h, rest, h != ""
	}
	u, err := user.Lookup(name)
	if err != nil || u.HomeDir == "" {
		return "", "", false
	}
	return u.HomeDir, rest, true
}

// expandWord performs the full set of word expansions: tilde, parameters,
// command substitution, field splitting, braces and filename globbing.
func (w *walker) expandWord(word *syntax.Word, s *scope) []Word {
	if word == nil {
		return nil
	}
	b := &builder{w: w, s: s, split: true, fields: []*field{{}}}
	b.parts(word.Parts, true)
	var out []Word
	for _, f := range b.fields {
		if f.pat == "" && !f.keep && !f.dynamic {
			continue
		}
		out = append(out, w.finishField(f, s)...)
	}
	return out
}

// expandJoined expands a word without splitting, braces or globbing, the
// way assignment values and heredoc delimiters are treated.
func (w *walker) expandJoined(word *syntax.Word, s *scope) Word {
	if word == nil {
		return Word{}
	}
	b := &builder{w: w, s: s, split: false, fields: []*field{{}}}
	b.parts(word.Parts, true)
	f := b.fields[0]
	return Word{Value: unescape(f.pat), Dynamic: f.dynamic}
}

// expandPattern expands a word used as a pattern and returns it in the
// escaped pattern form.
func (w *walker) expandPattern(word *syntax.Word, s *scope) (string, bool) {
	if word == nil {
		return "", false
	}
	b := &builder{w: w, s: s, split: false, fields: []*field{{}}}
	b.parts(word.Parts, false)
	return b.fields[0].pat, b.fields[0].dynamic
}

func (w *walker) finishField(f *field, s *scope) []Word {
	pats, ok := braceExpand(f.pat)
	if !ok {
		return []Word{{Value: unescape(f.pat), Dynamic: true}}
	}
	var out []Word
	for _, p := range pats {
		if f.glob && hasMeta(p) {
			out = append(out, globWord(p, s, f.dynamic)...)
			continue
		}
		out = append(out, Word{Value: unescape(p), Dynamic: f.dynamic})
	}
	return out
}

func globWord(p string, s *scope, dynamic bool) []Word {
	literal := []Word{{Value: unescape(p), Unmatched: true, Dynamic: dynamic}}
	if dynamic {
		return literal
	}
	full := p
	relative := !strings.HasPrefix(p, "/")
	if relative {
		if !s.dirKnown {
			literal[0].Dynamic = true
			return literal
		}
		if s.dir == "/" {
			full = "/" + p
		} else {
			full = escAll(s.dir) + "/" + p
		}
	}
	matches, err := filepath.Glob(full)
	if err != nil || len(matches) == 0 {
		return literal
	}
	matches = dropHidden(full, matches)
	if len(matches) == 0 {
		return literal
	}
	out := make([]Word, 0, len(matches))
	for _, m := range matches {
		v := m
		if relative {
			prefix := s.dir + "/"
			if s.dir == "/" {
				prefix = "/"
			}
			v = strings.TrimPrefix(m, prefix)
		}
		out = append(out, Word{Value: v, Globbed: true})
	}
	return out
}

// paramValue evaluates a parameter expansion. Forms that cannot be worked
// out statically come back dynamic.
func (w *walker) paramValue(pe *syntax.ParamExp, s *scope) (string, bool) {
	if pe.Param == nil || pe.Excl || pe.Index != nil || pe.Slice != nil || pe.Repl != nil ||
		pe.Names != 0 || pe.Width || pe.IsSet || pe.Flags != nil || pe.NestedParam != nil ||
		len(pe.Modifiers) > 0 {
		return "", true
	}
	name := pe.Param.Value
	val, set, known := s.lookup(name)
	if pe.Length {
		if !known {
			return "", true
		}
		return strconv.Itoa(utf8.RuneCountInString(val)), false
	}
	if pe.Exp == nil {
		return val, !known || !set
	}
	op := pe.Exp.Op
	arg := func() (string, bool) {
		wv := w.expandJoined(pe.Exp.Word, s)
		return wv.Value, wv.Dynamic
	}
	switch op {
	case syntax.DefaultUnset, syntax.DefaultUnsetOrNull, syntax.AssignUnset, syntax.AssignUnsetOrNull:
		if !known {
			return "", true
		}
		orNull := op == syntax.DefaultUnsetOrNull || op == syntax.AssignUnsetOrNull
		if set && (!orNull || val != "") {
			return val, false
		}
		d, dyn := arg()
		if op == syntax.AssignUnset || op == syntax.AssignUnsetOrNull {
			if dyn {
				s.setDynamic(name)
			} else {
				s.set(name, d)
			}
		}
		return d, dyn
	case syntax.AlternateUnset, syntax.AlternateUnsetOrNull:
		if !known {
			return "", true
		}
		if !set || (op == syntax.AlternateUnsetOrNull && val == "") {
			return "", false
		}
		return arg()
	case syntax.ErrorUnset, syntax.ErrorUnsetOrNull:
		return val, !known || !set
	case syntax.RemSmallPrefix, syntax.RemLargePrefix, syntax.RemSmallSuffix, syntax.RemLargeSuffix:
		if !known || !set {
			return "", true
		}
		pat, dyn := w.expandPattern(pe.Exp.Word, s)
		if dyn {
			return val, true
		}
		return removeAffix(op, val, pat), false
	}
	return "", true
}
