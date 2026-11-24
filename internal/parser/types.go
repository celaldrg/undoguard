// Package parser turns a shell command line into the list of simple
// commands it would run. It parses with mvdan.cc/sh into a syntax tree,
// expands words as far as can be done without running anything, strips
// wrappers such as sudo and env, and follows nested code: sh -c, eval,
// pipes into a shell, heredocs, small script files, functions, find -exec
// and xargs.
package parser

import "strings"

// Word is one argument after expansion.
type Word struct {
	// Value is the expanded text. Expansions that cannot be known
	// contribute nothing to it.
	Value string
	// Dynamic is set when part of the word depends on something only known
	// at run time: command substitution, arithmetic, an unset variable or a
	// positional parameter that was not supplied.
	Dynamic bool
	// Globbed is set when Value is a filename produced by a glob match.
	Globbed bool
	// Unmatched is set when the word held an unquoted glob that matched
	// nothing. Value then holds the pattern text, as bash would pass it.
	Unmatched bool
	// Subtree is set when the word stands for an unknown set of paths
	// below Value, for example the files a find expression selects.
	Subtree bool
}

// Redirect is a file redirection attached to a command.
type Redirect struct {
	// Op is one of ">", ">|", ">>", "&>", "&>>", "<", "<>", "<<", "<<<"
	// or a descriptor duplication such as ">&".
	Op string
	// Fd is the explicit descriptor number, or "" when implicit.
	Fd string
	// Target is the file operand. It is empty for heredocs.
	Target Word
	// Body is the heredoc or here-string text.
	Body string
}

// Command is one simple command after wrapper stripping.
type Command struct {
	// Name is the program, reduced to its base name.
	Name string
	// Path is the program as written when it contained a slash.
	Path string
	// Args are the arguments after Name.
	Args []Word
	// Redirs are the redirections of the command and of the statements
	// that enclose it.
	Redirs []Redirect
	// Dir is the working directory the command runs in.
	Dir string
	// DirKnown is false when an earlier cd went somewhere unknown.
	DirKnown bool
	// Text is the source text of the command.
	Text string
	// Via lists the wrappers the command was found inside, outermost
	// first, for example "sudo" or "bash -c".
	Via []string
	// NameDynamic is set when the program name could not be resolved.
	NameDynamic bool
	// Stdin describes standard input when a pipe or heredoc feeds it.
	Stdin *Stdin
}

// Stdin describes what feeds a command's standard input.
type Stdin struct {
	// From is the command on the left of a pipe, when there is one.
	From *Command
	// Text is the input itself, when it could be worked out.
	Text string
	// Known is true when Text is exact.
	Known bool
}

// Issue is something the parser could not see through. The guard treats
// every issue as an unresolved target.
type Issue struct {
	// Kind is "parse", "dynamic-code", "dynamic-command", "pipe-to-shell",
	// "script" or "depth".
	Kind   string
	Detail string
}

// Result is everything Parse found.
type Result struct {
	Commands []*Command
	Issues   []Issue
}

// ArgValues returns the plain values of the arguments.
func (c *Command) ArgValues() []string {
	out := make([]string, len(c.Args))
	for i, a := range c.Args {
		out[i] = a.Value
	}
	return out
}

// Display renders the command for humans.
func (c *Command) Display() string {
	if c.Text != "" {
		return c.Text
	}
	parts := []string{c.Name}
	for _, a := range c.Args {
		parts = append(parts, Quote(a.Value))
	}
	return strings.Join(parts, " ")
}

// Quote returns s quoted for a POSIX shell when it needs quoting.
func Quote(s string) string {
	if s == "" {
		return "''"
	}
	safe := true
	for _, r := range s {
		if !isShellSafe(r) {
			safe = false
			break
		}
	}
	if safe {
		return s
	}
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

// QuoteArgs joins argv into a command line that a shell would split back
// into the same words.
func QuoteArgs(argv []string) string {
	q := make([]string, len(argv))
	for i, a := range argv {
		q[i] = Quote(a)
	}
	return strings.Join(q, " ")
}

// setOf builds a lookup set from a list of names.
func setOf(items ...string) map[string]bool {
	m := make(map[string]bool, len(items))
	for _, it := range items {
		m[it] = true
	}
	return m
}

// isShellSafe reports whether r can appear unquoted in a shell word.
func isShellSafe(r rune) bool {
	return r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' ||
		strings.ContainsRune("-_./=:,+@%^", r)
}
