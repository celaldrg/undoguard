package parser

import (
	"bytes"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"mvdan.cc/sh/v3/syntax"

	"github.com/celaldrg/undoguard/internal/paths"
)

var posixShells = setOf(
	"sh", "bash", "dash", "zsh", "ksh", "mksh", "ash", "yash", "posh", "rbash",
)

func isShell(name string) bool { return posixShells[name] }

func setPositionals(s *scope, ws []Word) {
	s.pos, s.posKnown = nil, true
	for _, a := range ws {
		if a.Dynamic {
			s.posKnown = false
		}
		s.pos = append(s.pos, a.Value)
	}
}

// shell follows the code a POSIX shell is asked to run: a -c string, a
// script file, or commands read from standard input.
func (w *walker) shell(name string, args []Word, sub *scope, stdin *Stdin) {
	ns := sub.child(sub.dir, sub.dirKnown)
	cFlag, sFlag := false, false
	i := 0
	for i < len(args) {
		a := args[i].Value
		if a == "--" || a == "-" {
			i++
			break
		}
		if len(a) < 2 || (a[0] != '-' && a[0] != '+') {
			break
		}
		if strings.HasPrefix(a, "--") {
			if a == "--rcfile" || a == "--init-file" {
				i++
			}
			i++
			continue
		}
		takesValue := false
		for j := 1; j < len(a); j++ {
			switch a[j] {
			case 'c':
				cFlag = true
			case 's':
				sFlag = true
			case 'o', 'O':
				takesValue = true
			}
		}
		i++
		if takesValue {
			i++
		}
	}
	var operands []Word
	if i < len(args) {
		operands = args[i:]
	}
	switch {
	case cFlag:
		if len(operands) == 0 {
			return
		}
		code := operands[0]
		if code.Dynamic {
			w.issue("dynamic-code", name+" -c runs code that is only known at run time")
		}
		if len(operands) > 1 {
			ns.arg0 = operands[1].Value
		}
		if len(operands) > 2 {
			setPositionals(ns, operands[2:])
		}
		w.script(code.Value, ns, name+" -c", stdin)
	case !sFlag && len(operands) > 0:
		file := operands[0]
		if file.Dynamic {
			w.issue("script", name+" runs a script whose name is only known at run time")
			return
		}
		if !filepath.IsAbs(file.Value) && !ns.dirKnown {
			w.issue("script", name+" runs "+file.Value+" from an unknown directory")
			return
		}
		data, ok := w.readScript(paths.Abs(ns.dir, file.Value), file.Value, name)
		if !ok {
			return
		}
		ns.arg0 = file.Value
		setPositionals(ns, operands[1:])
		w.script(string(data), ns, name+" "+filepath.Base(file.Value), stdin)
	default:
		if stdin == nil {
			return
		}
		setPositionals(ns, operands)
		text, ok := w.stdinText(stdin)
		if !ok {
			w.issue("pipe-to-shell", name+" runs commands from input that cannot be inspected")
			return
		}
		w.script(text, ns, name+" <stdin>", nil)
	}
}

// readScript reads a script that is about to run. It records an issue and
// returns false when the file cannot be read, or when an earlier command
// on the same line may have written it, because then what runs is not
// what is on disk now.
func (w *walker) readScript(p, shown, label string) ([]byte, bool) {
	what := strings.TrimSpace(label + " runs " + shown)
	if w.mentioned(p) {
		w.issue("script", what+", which an earlier command may change")
		return nil, false
	}
	data, ok := w.readSmall(p)
	if !ok {
		w.issue("script", what+", which could not be read")
		return nil, false
	}
	return data, true
}

// fileWriters are programs that commonly create or rewrite the files they
// name. Other programs only count when they redirect output to a file.
var fileWriters = setOf(
	"cp", "mv", "install", "ln", "rsync", "tar", "unzip", "git", "curl", "wget", "patch",
	"tee", "dd", "sed", "perl", "7z", "scp", "rclone",
)

// mentioned reports whether a command recorded before the current one
// redirects output to p, or is a file writer that names p or a directory
// containing p.
func (w *walker) mentioned(p string) bool {
	return w.writtenEarlier(p, 1)
}

// writtenEarlier reports whether a command recorded so far, leaving out
// the last skip ones, may have written p.
func (w *walker) writtenEarlier(p string, skip int) bool {
	cmds := w.res.Commands
	if n := len(cmds); n >= skip {
		cmds = cmds[:n-skip]
	}
	return mayWrite(cmds, p)
}

// WrittenBefore reports whether a command that runs before c on the same
// command line may have written p or a directory above it, for example by
// creating a symlink there. What the filesystem shows now then says little
// about what c will meet.
func (r *Result) WrittenBefore(c *Command, p string) bool {
	for i, x := range r.Commands {
		if x == c {
			return mayWrite(r.Commands[:i], p)
		}
	}
	return false
}

func mayWrite(cmds []*Command, p string) bool {
	p = filepath.Clean(p)
	base := filepath.Base(p)
	match := func(c *Command, v Word, ancestorOK bool) bool {
		if v.Value == "" {
			return v.Dynamic
		}
		if !filepath.IsAbs(v.Value) && !c.DirKnown {
			return filepath.Base(v.Value) == base
		}
		abs := paths.Abs(c.Dir, v.Value)
		if abs == p {
			return true
		}
		return ancestorOK && paths.Within(p, abs)
	}
	for _, c := range cmds {
		for _, r := range c.Redirs {
			if strings.HasPrefix(r.Op, ">") || strings.HasPrefix(r.Op, "&>") || r.Op == "<>" {
				if match(c, r.Target, false) {
					return true
				}
			}
		}
		if !fileWriters[c.Name] {
			continue
		}
		for _, a := range c.Args {
			if strings.HasPrefix(a.Value, "-") && !strings.Contains(a.Value, "/") {
				continue
			}
			if match(c, a, true) {
				return true
			}
		}
	}
	return false
}

// stdinText works out what a command reads on standard input.
func (w *walker) stdinText(in *Stdin) (string, bool) {
	if in == nil {
		return "", false
	}
	if in.Known {
		return in.Text, true
	}
	if in.From == nil {
		return "", false
	}
	return w.commandOutput(in.From)
}

// commandOutput predicts the output of the few commands that are used to
// feed code to a shell and whose output depends only on their arguments.
func (w *walker) commandOutput(c *Command) (string, bool) {
	for _, a := range c.Args {
		if a.Dynamic || a.Unmatched && c.Name == "cat" {
			return "", false
		}
	}
	args := c.ArgValues()
	switch c.Name {
	case "echo":
		newline, escapes := true, false
		for len(args) > 0 && len(args[0]) > 1 && args[0][0] == '-' && strings.Trim(args[0][1:], "neE") == "" {
			newline = newline && !strings.Contains(args[0], "n")
			escapes = escapes || strings.Contains(args[0], "e")
			args = args[1:]
		}
		out := strings.Join(args, " ")
		if escapes {
			out = ansiC(out)
		}
		if newline {
			out += "\n"
		}
		return out, true
	case "printf":
		if len(args) > 0 && args[0] == "--" {
			args = args[1:]
		}
		if len(args) == 0 {
			return "", false
		}
		return printfOutput(args[0], args[1:])
	case "cat":
		if len(args) == 0 {
			if c.Stdin != nil && c.Stdin.Known {
				return c.Stdin.Text, true
			}
			return "", false
		}
		var b strings.Builder
		for _, a := range args {
			if strings.HasPrefix(a, "-") {
				return "", false
			}
			p := a
			if !filepath.IsAbs(p) {
				if !c.DirKnown {
					return "", false
				}
				p = filepath.Join(c.Dir, p)
			}
			if w.mentioned(p) {
				return "", false
			}
			data, ok := w.readSmall(p)
			if !ok {
				return "", false
			}
			b.Write(data)
		}
		return b.String(), true
	}
	return "", false
}

// printfOutput formats the %s, %b and %% directives of printf and gives up
// on anything else.
func printfOutput(format string, args []string) (string, bool) {
	var b strings.Builder
	for round := 0; round < 64; round++ {
		used := 0
		for i := 0; i < len(format); i++ {
			c := format[i]
			if c == '\\' && i+1 < len(format) {
				j := i + 2
				if format[i+1] >= '0' && format[i+1] <= '7' {
					for j < len(format) && j < i+4 && format[j] >= '0' && format[j] <= '7' {
						j++
					}
				}
				b.WriteString(ansiC(format[i:j]))
				i = j - 1
				continue
			}
			if c != '%' {
				b.WriteByte(c)
				continue
			}
			if i+1 >= len(format) {
				return "", false
			}
			i++
			switch format[i] {
			case '%':
				b.WriteByte('%')
			case 's', 'b':
				v := ""
				if used < len(args) {
					v = args[used]
				}
				if format[i] == 'b' {
					v = ansiC(v)
				}
				b.WriteString(v)
				used++
			default:
				return "", false
			}
		}
		args = args[min(used, len(args)):]
		if used == 0 || len(args) == 0 {
			return b.String(), true
		}
	}
	return "", false
}

func isFindExpr(v string) bool {
	return len(v) > 1 && v[0] == '-' || v == "(" || v == "!" || v == ","
}

// procScope is the state of a program started directly, not by a shell:
// it cannot call shell functions.
func procScope(sub *scope) *scope {
	ex := sub.child(sub.dir, sub.dirKnown)
	ex.funcs = map[string]*syntax.Stmt{}
	return ex
}

// find follows the commands of -exec, -execdir, -ok and -okdir. Each {} is
// replaced by a start point marked as a subtree: find may hand any file
// below it to the command.
func (w *walker) find(args []Word, sub *scope) {
	i := 0
	for i < len(args) {
		a := args[i].Value
		switch {
		case a == "-H" || a == "-L" || a == "-P":
			i++
			continue
		case a == "-D":
			i += 2
			continue
		case strings.HasPrefix(a, "-O") || strings.HasPrefix(a, "-D"):
			i++
			continue
		}
		break
	}
	var roots []Word
	for i < len(args) && !isFindExpr(args[i].Value) {
		roots = append(roots, args[i])
		i++
	}
	if len(roots) == 0 {
		roots = []Word{{Value: "."}}
	}
	expr := args[i:]
	for k := 0; k < len(expr); k++ {
		op := expr[k].Value
		if op != "-exec" && op != "-execdir" && op != "-ok" && op != "-okdir" {
			continue
		}
		end := k + 1
		for end < len(expr) && expr[end].Value != ";" && (expr[end].Value != "+" || end <= k+1 || expr[end-1].Value != "{}") {
			end++
		}
		tmpl := expr[k+1 : end]
		k = end
		if len(tmpl) == 0 {
			continue
		}
		inDir := strings.HasSuffix(op, "dir")
		for _, root := range roots {
			ex := procScope(sub)
			rootWord := Word{Value: root.Value, Subtree: true, Dynamic: root.Dynamic}
			if inDir {
				if !filepath.IsAbs(root.Value) && !sub.dirKnown {
					rootWord.Dynamic = true
				} else {
					rootWord.Value = paths.Abs(sub.dir, root.Value)
				}
				ex.dirKnown = false
			}
			argv := make([]Word, 0, len(tmpl))
			for _, t := range tmpl {
				switch {
				case t.Value == "{}":
					argv = append(argv, rootWord)
				case strings.Contains(t.Value, "{}"):
					argv = append(argv, Word{Value: strings.ReplaceAll(t.Value, "{}", rootWord.Value), Dynamic: true})
				default:
					argv = append(argv, t)
				}
			}
			saved := w.via
			w.via = appendVia(w.via, "find "+op)
			w.run(argv, ex, ex, nil, nil, QuoteArgs(wordValues(argv)))
			w.via = saved
		}
	}
}

const maxXargsItems = 1024

// xargs follows the command xargs runs, with the items it would read.
func (w *walker) xargs(args []Word, sub *scope, stdin *Stdin) {
	repl, argFile := "", ""
	hasArgFile := false
	i := 0
	for i < len(args) {
		a := args[i].Value
		if a == "--" {
			i++
			break
		}
		if len(a) < 2 || a[0] != '-' {
			break
		}
		if strings.HasPrefix(a, "--") {
			k, v, has := strings.Cut(a, "=")
			switch k {
			case "--replace":
				repl = "{}"
				if has {
					repl = v
				}
			case "--arg-file":
				hasArgFile = true
				if has {
					argFile = v
				} else if i+1 < len(args) {
					i++
					argFile = args[i].Value
				}
			case "--max-args", "--max-lines", "--max-procs", "--max-chars", "--delimiter", "--eof", "--process-slot-var":
				if !has {
					i++
				}
			}
			i++
			continue
		}
		switch a[1] {
		case 'I':
			repl = a[2:]
			if repl == "" && i+1 < len(args) {
				i++
				repl = args[i].Value
			}
		case 'i':
			repl = a[2:]
			if repl == "" {
				repl = "{}"
			}
		case 'a':
			hasArgFile = true
			argFile = a[2:]
			if argFile == "" && i+1 < len(args) {
				i++
				argFile = args[i].Value
			}
		case 'e', 'l':
		default:
			for j := 1; j < len(a); j++ {
				if strings.IndexByte("nLPsdE", a[j]) >= 0 {
					if j == len(a)-1 {
						i++
					}
					break
				}
			}
		}
		i++
	}
	var cmd []Word
	if i < len(args) {
		cmd = args[i:]
	} else {
		cmd = []Word{{Value: "echo"}}
	}
	var items []Word
	if hasArgFile {
		items = w.itemsFromFile(argFile, sub)
	} else {
		items = w.xargsItems(stdin)
	}
	if len(items) > maxXargsItems {
		items = []Word{{Dynamic: true}}
	}
	ex := procScope(sub)
	saved := w.via
	w.via = appendVia(w.via, "xargs")
	defer func() { w.via = saved }()
	if repl == "" {
		argv := append(append([]Word(nil), cmd...), items...)
		w.run(argv, ex, ex, nil, nil, QuoteArgs(wordValues(argv)))
		return
	}
	for _, it := range items {
		argv := make([]Word, 0, len(cmd))
		for _, t := range cmd {
			switch {
			case t.Value == repl:
				argv = append(argv, it)
			case strings.Contains(t.Value, repl):
				argv = append(argv, Word{
					Value:   strings.ReplaceAll(t.Value, repl, it.Value),
					Dynamic: t.Dynamic || it.Dynamic || it.Subtree,
				})
			default:
				argv = append(argv, t)
			}
		}
		w.run(argv, procScope(sub), ex, nil, nil, QuoteArgs(wordValues(argv)))
	}
}

func (w *walker) itemsFromFile(name string, sub *scope) []Word {
	if name == "" || (!filepath.IsAbs(name) && !sub.dirKnown) {
		return []Word{{Dynamic: true}}
	}
	p := paths.Abs(sub.dir, name)
	if w.mentioned(p) {
		return []Word{{Dynamic: true}}
	}
	data, ok := w.readSmall(p)
	if !ok {
		return []Word{{Dynamic: true}}
	}
	return splitItems(string(data))
}

func splitItems(text string) []Word {
	var fields []string
	if strings.Contains(text, "\x00") {
		for _, f := range strings.Split(text, "\x00") {
			if f != "" {
				fields = append(fields, f)
			}
		}
	} else {
		fields = splitWords(text)
	}
	out := make([]Word, len(fields))
	for i, f := range fields {
		out[i] = Word{Value: f}
	}
	return out
}

// xargsItems works out the items xargs reads from its input. Input that
// cannot be predicted gives one dynamic item; the output of find gives its
// start points as subtrees.
func (w *walker) xargsItems(in *Stdin) []Word {
	unknown := []Word{{Dynamic: true}}
	if in == nil {
		return unknown
	}
	if in.Known {
		return splitItems(in.Text)
	}
	c := in.From
	if c == nil {
		return unknown
	}
	abs := func(v string) (Word, bool) {
		if filepath.IsAbs(v) {
			return Word{Value: filepath.Clean(v)}, true
		}
		if !c.DirKnown {
			return Word{}, false
		}
		return Word{Value: paths.Abs(c.Dir, v)}, true
	}
	switch c.Name {
	case "find", "fd", "fdfind":
		var out []Word
		for _, a := range c.Args {
			if a.Dynamic {
				return unknown
			}
			if c.Name == "find" && isFindExpr(a.Value) {
				break
			}
			if c.Name == "find" && (a.Value == "-H" || a.Value == "-L" || a.Value == "-P") {
				continue
			}
			if c.Name != "find" && strings.HasPrefix(a.Value, "-") {
				continue
			}
			if c.Name != "find" && !paths.IsDir(paths.Abs(c.Dir, a.Value)) {
				continue
			}
			wd, ok := abs(a.Value)
			if !ok {
				return unknown
			}
			wd.Subtree = true
			out = append(out, wd)
		}
		if len(out) == 0 {
			wd, ok := abs(".")
			if !ok {
				return unknown
			}
			wd.Subtree = true
			out = append(out, wd)
		}
		return out
	case "ls":
		var out []Word
		for _, a := range c.Args {
			if a.Dynamic {
				return unknown
			}
			if strings.HasPrefix(a.Value, "-") {
				continue
			}
			wd, ok := abs(a.Value)
			if !ok || paths.IsDir(wd.Value) {
				return unknown
			}
			out = append(out, Word{Value: a.Value})
		}
		if len(out) == 0 {
			wd, ok := abs(".")
			if !ok {
				return unknown
			}
			wd.Subtree = true
			return []Word{wd}
		}
		return out
	case "git":
		args := c.ArgValues()
		if len(args) > 0 && (args[0] == "ls-files" || args[0] == "diff" && contains(args, "--name-only")) && c.DirKnown {
			root := paths.GitRoot(c.Dir)
			if root == "" {
				root = c.Dir
			}
			return []Word{{Value: root, Subtree: true}}
		}
	}
	if text, ok := w.commandOutput(c); ok {
		return splitItems(text)
	}
	return unknown
}

func contains(list []string, v string) bool {
	for _, x := range list {
		if x == v {
			return true
		}
	}
	return false
}

// interpreterCode extracts inline code given to a language interpreter on
// its command line.
func interpreterCode(name string, args []Word) (Word, string, bool) {
	lang := ""
	var flags []string
	cluster := ""
	attached := false
	switch {
	case strings.HasPrefix(name, "python") || strings.HasPrefix(name, "pypy"):
		lang, flags, cluster = "python", []string{"-c"}, "c"
	case strings.HasPrefix(name, "perl"):
		lang, flags, cluster, attached = "perl", []string{"-e", "-E"}, "eE", true
	case strings.HasPrefix(name, "ruby") || name == "jruby":
		lang, flags, cluster, attached = "ruby", []string{"-e"}, "e", true
	case name == "node" || name == "nodejs" || name == "bun":
		lang, flags = "javascript", []string{"-e", "--eval", "-p", "--print"}
	case name == "deno":
		if len(args) > 1 && args[0].Value == "eval" {
			return args[len(args)-1], "javascript", true
		}
		return Word{}, "", false
	case strings.HasPrefix(name, "php"):
		lang, flags = "php", []string{"-r"}
	case name == "osascript":
		lang, flags = "applescript", []string{"-e"}
	case name == "lua" || name == "luajit":
		lang, flags = "lua", []string{"-e"}
	case name == "Rscript":
		lang, flags = "r", []string{"-e"}
	case name == "tclsh":
		lang = "tcl"
	case name == "fish" || name == "csh" || name == "tcsh" || name == "nu" || name == "xonsh" || name == "elvish":
		lang, flags, cluster = "shell:"+name, []string{"-c", "--command"}, "c"
	case name == "pwsh" || name == "powershell":
		lang, flags = "shell:"+name, []string{"-c", "-Command", "-command", "-EncodedCommand", "-e", "-ec"}
	case name == "awk" || name == "gawk" || name == "mawk" || name == "nawk" || name == "busybox-awk":
		return awkProgram(args)
	default:
		return Word{}, "", false
	}
	var parts []string
	dyn, found := false, false
	for i := 0; i < len(args); i++ {
		a := args[i].Value
		if a == "--" {
			break
		}
		take := false
		for _, f := range flags {
			if a == f {
				take = true
			}
		}
		if !take && cluster != "" && len(a) > 2 && a[0] == '-' && a[1] != '-' {
			if strings.IndexByte(cluster, a[len(a)-1]) >= 0 {
				take = true
			} else if attached && strings.IndexByte(cluster, a[1]) >= 0 {
				parts = append(parts, a[2:])
				dyn = dyn || args[i].Dynamic
				found = true
				continue
			}
		}
		if take {
			if i+1 < len(args) {
				parts = append(parts, args[i+1].Value)
				dyn = dyn || args[i+1].Dynamic
				found = true
				i++
			}
			continue
		}
		if !strings.HasPrefix(a, "-") && !found {
			// A script file operand: the code is in a file.
			break
		}
	}
	if !found {
		return Word{}, "", false
	}
	return Word{Value: strings.Join(parts, "\n"), Dynamic: dyn}, lang, true
}

func awkProgram(args []Word) (Word, string, bool) {
	for i := 0; i < len(args); i++ {
		a := args[i].Value
		switch {
		case a == "--":
			if i+1 < len(args) {
				return args[i+1], "awk", true
			}
			return Word{}, "", false
		case a == "-f" || strings.HasPrefix(a, "--file"):
			return Word{}, "", false
		case a == "-v" || a == "-F":
			i++
		case strings.HasPrefix(a, "-"):
		default:
			return args[i], "awk", true
		}
	}
	return Word{}, "", false
}

var (
	riskyCode = regexp.MustCompile(`(?i)rmtree|unlink|rmdir|remove|rename|truncate|\bsystem\b|popen|\bexec|spawn|shell_exec|passthru|proc_open|rmsync|\brm\(|\brm\s|writefile|appendfile|copyfile|child_process|fileutils|deno\.(run|command|writefile)|do shell script|os\.execute|io\.open|\bkill\b|chmod|chown|subprocess|shutil|pathlib|\bunlink|write_text|write_bytes|bun\.(write|spawn|\$)|qx|` + "`" + `|open\s*\(`)
	riskyAwk  = regexp.MustCompile(`system\s*\(|>\s*"|>\s*[A-Za-z_]|\|\s*"|\|\s*[A-Za-z_]|getline\s*<|close\s*\(`)
)

// inlineCode looks at code written in another language. It cannot be
// followed like shell code, so anything that might touch files or run
// commands raises an issue.
func (w *walker) inlineCode(lang string, code Word) {
	if code.Dynamic {
		w.issue("dynamic-code", lang+" code that is only known at run time")
		return
	}
	if strings.HasPrefix(lang, "shell:") {
		w.issue("dynamic-code", strings.TrimPrefix(lang, "shell:")+" code is not analysed: "+short(code.Value))
		return
	}
	re := riskyCode
	if lang == "awk" {
		re = riskyAwk
	}
	if re.MatchString(code.Value) {
		w.issue("dynamic-code", lang+" code may change files or run commands: "+short(code.Value))
	}
}

// directScript follows a program started by path. Shell scripts are
// walked; scripts for other interpreters are screened like inline code;
// compiled programs are not looked into.
func (w *walker) directScript(argv []Word, sub *scope, stdin *Stdin) {
	file := argv[0]
	if !filepath.IsAbs(file.Value) && !sub.dirKnown {
		w.issue("script", "runs "+file.Value+" from an unknown directory")
		return
	}
	p := paths.Abs(sub.dir, file.Value)
	fi, err := os.Stat(p)
	if err != nil {
		if w.mentioned(p) {
			w.issue("script", "runs "+file.Value+", which an earlier command may create")
		}
		return
	}
	if !fi.Mode().IsRegular() {
		return
	}
	if head := readHead(p, 512); isBinary(head) {
		return
	}
	data, ok := w.readScript(p, file.Value, "")
	if !ok {
		return
	}
	interp := shebang(data)
	ns := sub.child(sub.dir, sub.dirKnown)
	ns.arg0 = file.Value
	setPositionals(ns, argv[1:])
	label := filepath.Base(file.Value)
	if len(interp) == 0 || isShell(filepath.Base(interp[0])) {
		w.script(string(data), ns, label, stdin)
		return
	}
	prog := filepath.Base(interp[0])
	if prog == "env" {
		rest := interp[1:]
		for len(rest) > 0 && strings.HasPrefix(rest[0], "-") {
			rest = rest[1:]
		}
		if len(rest) == 0 {
			return
		}
		prog = filepath.Base(rest[0])
	}
	if isShell(prog) {
		w.script(string(data), ns, label, stdin)
		return
	}
	flagFor := map[string]string{"python": "-c", "perl": "-e", "ruby": "-e", "node": "-e", "php": "-r"}
	for prefix, flag := range flagFor {
		if strings.HasPrefix(prog, prefix) {
			if code, lang, ok := interpreterCode(prog, []Word{{Value: flag}, {Value: string(data)}}); ok {
				w.inlineCode(lang, code)
			}
			return
		}
	}
	if prog == "awk" || prog == "gawk" {
		w.inlineCode("awk", Word{Value: string(data)})
		return
	}
	w.issue("script", label+" runs with "+prog+", which is not analysed")
}

func shebang(data []byte) []string {
	if !bytes.HasPrefix(data, []byte("#!")) {
		return nil
	}
	line := data[2:]
	if i := bytes.IndexByte(line, '\n'); i >= 0 {
		line = line[:i]
	}
	return strings.Fields(string(line))
}

func readHead(p string, n int) []byte {
	f, err := os.Open(p)
	if err != nil {
		return nil
	}
	defer f.Close()
	buf := make([]byte, n)
	k, _ := f.Read(buf)
	return buf[:k]
}

func isBinary(head []byte) bool {
	if len(head) >= 4 {
		magic := [][]byte{
			{0x7f, 'E', 'L', 'F'},
			{0xfe, 0xed, 0xfa, 0xce}, {0xfe, 0xed, 0xfa, 0xcf},
			{0xce, 0xfa, 0xed, 0xfe}, {0xcf, 0xfa, 0xed, 0xfe},
			{0xca, 0xfe, 0xba, 0xbe},
		}
		for _, m := range magic {
			if bytes.HasPrefix(head, m) {
				return true
			}
		}
	}
	return bytes.IndexByte(head, 0) >= 0
}

// findScript locates a file named by source or a script operand. Names
// without a slash are looked up on PATH first when searchPath is set, then
// in dir.
func (w *walker) findScript(name, dir string, dirKnown bool, searchPath bool) (string, bool) {
	regular := func(p string) bool {
		fi, err := os.Stat(p)
		return err == nil && fi.Mode().IsRegular()
	}
	if strings.Contains(name, "/") {
		if !filepath.IsAbs(name) && !dirKnown {
			return "", false
		}
		return paths.Abs(dir, name), true
	}
	if searchPath {
		for _, kv := range w.opts.Env {
			if !strings.HasPrefix(kv, "PATH=") {
				continue
			}
			for _, d := range filepath.SplitList(kv[5:]) {
				if d == "" || !filepath.IsAbs(d) {
					continue
				}
				if p := filepath.Join(d, name); regular(p) {
					return p, true
				}
			}
		}
	}
	if !dirKnown {
		return "", false
	}
	return filepath.Join(dir, name), true
}
