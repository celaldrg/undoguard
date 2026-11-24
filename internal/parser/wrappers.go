package parser

import (
	"path/filepath"
	"strings"
)

// unwrap strips one wrapper program, such as sudo or timeout, from the
// front of argv. It returns the wrapped command, a label for the wrapper
// and true; or nil and true when the wrapper runs nothing that needs a
// further look (command -v, sudo -l) or ran its code through the walker
// already (su -c, watch); or false when argv[0] is not a wrapper.
func (w *walker) unwrap(argv []Word, rs *runState, stdin *Stdin) ([]Word, string, bool) {
	name := filepath.Base(argv[0].Value)
	args := argv[1:]
	switch name {
	case "sudo":
		rest, ok := sudoArgs(args, rs)
		if !ok {
			return nil, name, true
		}
		return rest, name, true
	case "doas":
		return skipOptions(args, "uC"), name, true
	case "env":
		return envArgs(args, rs), name, true
	case "nice":
		return skipOptions(args, "n", "--adjustment"), name, true
	case "ionice":
		for _, a := range args {
			if a.Value == "-p" || a.Value == "-P" || a.Value == "-u" || strings.HasPrefix(a.Value, "--pid") {
				return nil, name, true
			}
		}
		return skipOptions(args, "cn", "--class", "--classdata"), name, true
	case "nohup", "builtin", "unbuffer", "busybox":
		return skipOptions(args, ""), name, true
	case "exec":
		return skipOptions(args, "a"), name, true
	case "time":
		return skipOptions(args, "fo", "--format", "--output"), name, true
	case "timeout":
		rest := skipOptions(args, "sk", "--signal", "--kill-after")
		if len(rest) == 0 {
			return nil, name, true
		}
		return rest[1:], name, true
	case "command":
		i := 0
		for i < len(args) && strings.HasPrefix(args[i].Value, "-") && len(args[i].Value) > 1 {
			a := args[i].Value
			if a == "--" {
				i++
				break
			}
			if strings.ContainsAny(a[1:], "vV") {
				return nil, name, true
			}
			i++
		}
		return args[i:], name, true
	case "stdbuf":
		return skipOptions(args, "ioe", "--input", "--output", "--error"), name, true
	case "setsid":
		return skipOptions(args, ""), name, true
	case "chrt":
		for _, a := range args {
			if a.Value == "-p" || a.Value == "--pid" {
				return nil, name, true
			}
		}
		rest := skipOptions(args, "TPD", "--sched-runtime", "--sched-period", "--sched-deadline")
		if len(rest) == 0 {
			return nil, name, true
		}
		return rest[1:], name, true
	case "taskset":
		for _, a := range args {
			if a.Value == "-p" || a.Value == "--pid" || (strings.HasPrefix(a.Value, "-") && !strings.HasPrefix(a.Value, "--") && strings.Contains(a.Value, "p")) {
				return nil, name, true
			}
		}
		rest := skipOptions(args, "")
		if len(rest) == 0 {
			return nil, name, true
		}
		return rest[1:], name, true
	case "caffeinate":
		return skipOptions(args, "tw"), name, true
	case "flock":
		return w.flockArgs(args, rs, stdin), name, true
	case "watch":
		w.watchArgs(args, rs)
		return nil, name, true
	case "su", "runuser":
		for i := 0; i < len(args); i++ {
			a := args[i].Value
			switch {
			case a == "-c" || a == "--command" || a == "--session-command":
				if i+1 < len(args) {
					w.scriptWord(args[i+1], rs, name+" -c", stdin)
				}
				return nil, name, true
			case strings.HasPrefix(a, "--command="):
				w.scriptWord(Word{Value: strings.TrimPrefix(a, "--command="), Dynamic: args[i].Dynamic}, rs, name+" -c", stdin)
				return nil, name, true
			case a == "--" && name == "runuser":
				return args[i+1:], name, true
			}
		}
		return nil, name, true
	}
	return nil, "", false
}

// skipOptions drops leading options. shortValue lists the short options
// that take a value; longValue lists the long ones that take a separate
// value when written without "=".
func skipOptions(args []Word, shortValue string, longValue ...string) []Word {
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
			takesNext := false
			if !strings.Contains(a, "=") {
				for _, l := range longValue {
					if a == l {
						takesNext = true
					}
				}
			}
			i++
			if takesNext {
				i++
			}
			continue
		}
		takesNext := false
		for j := 1; j < len(a); j++ {
			if strings.IndexByte(shortValue, a[j]) >= 0 {
				takesNext = j == len(a)-1
				break
			}
		}
		i++
		if takesNext {
			i++
		}
	}
	if i >= len(args) {
		return nil
	}
	return args[i:]
}

func sudoArgs(args []Word, rs *runState) ([]Word, bool) {
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
			key, val, hasVal := strings.Cut(a, "=")
			switch key {
			case "--list", "--validate", "--version", "--help", "--remove-timestamp", "--edit":
				return nil, false
			case "--user", "--group", "--host", "--prompt", "--close-from", "--chdir",
				"--role", "--type", "--command-timeout", "--other-user":
				if !hasVal && i+1 < len(args) {
					i++
					val = args[i].Value
				}
				if key == "--chdir" {
					rs.chdir(Word{Value: val})
				}
			}
			i++
			continue
		}
	cluster:
		for j := 1; j < len(a); j++ {
			c := a[j]
			switch {
			case strings.IndexByte("lvVKe", c) >= 0:
				return nil, false
			case strings.IndexByte("ughpCDrtTU", c) >= 0:
				val := a[j+1:]
				if val == "" && i+1 < len(args) {
					i++
					val = args[i].Value
				}
				if c == 'D' {
					rs.chdir(Word{Value: val})
				}
				break cluster
			}
		}
		i++
	}
	if i > len(args) {
		return nil, true
	}
	rest := args[i:]
	for len(rest) > 0 && isAssignment(rest[0].Value) {
		k, v, _ := strings.Cut(rest[0].Value, "=")
		rs.ownEnv().setEnv(k, v, rest[0].Dynamic)
		rest = rest[1:]
	}
	return rest, true
}

func envArgs(args []Word, rs *runState) []Word {
	i := 0
loop:
	for i < len(args) {
		a := args[i].Value
		switch {
		case a == "--":
			i++
			break loop
		case a == "-" || a == "-i" || a == "--ignore-environment":
			e := rs.ownEnv()
			e.vars = map[string]string{}
			e.dynamic = map[string]bool{}
			e.exported = map[string]bool{}
			i++
		case a == "-u" || a == "--unset":
			if i+1 < len(args) {
				rs.ownEnv().unset(args[i+1].Value)
			}
			i += 2
		case strings.HasPrefix(a, "--unset="):
			rs.ownEnv().unset(strings.TrimPrefix(a, "--unset="))
			i++
		case strings.HasPrefix(a, "-u") && !strings.HasPrefix(a, "--"):
			rs.ownEnv().unset(a[2:])
			i++
		case a == "-C" || a == "--chdir":
			if i+1 < len(args) {
				rs.chdir(args[i+1])
			}
			i += 2
		case strings.HasPrefix(a, "--chdir="):
			rs.chdir(Word{Value: strings.TrimPrefix(a, "--chdir="), Dynamic: args[i].Dynamic})
			i++
		case strings.HasPrefix(a, "-C") && !strings.HasPrefix(a, "--"):
			rs.chdir(Word{Value: a[2:], Dynamic: args[i].Dynamic})
			i++
		case a == "-S" || a == "--split-string" || strings.HasPrefix(a, "--split-string=") ||
			(strings.HasPrefix(a, "-S") && !strings.HasPrefix(a, "--")):
			var str Word
			switch {
			case a == "-S" || a == "--split-string":
				if i+1 >= len(args) {
					return nil
				}
				str = args[i+1]
				i += 2
			case strings.HasPrefix(a, "--split-string="):
				str = Word{Value: strings.TrimPrefix(a, "--split-string="), Dynamic: args[i].Dynamic}
				i++
			default:
				str = Word{Value: a[2:], Dynamic: args[i].Dynamic}
				i++
			}
			out := []Word{{Value: "env"}}
			for _, part := range splitWords(str.Value) {
				out = append(out, Word{Value: part, Dynamic: str.Dynamic})
			}
			return append(out, args[i:]...)
		case isAssignment(a):
			k, v, _ := strings.Cut(a, "=")
			rs.ownEnv().setEnv(k, v, args[i].Dynamic)
			i++
		case strings.HasPrefix(a, "-") && len(a) > 1:
			i++
		default:
			break loop
		}
	}
	if i >= len(args) {
		return nil
	}
	return args[i:]
}

func (w *walker) flockArgs(args []Word, rs *runState, stdin *Stdin) []Word {
	rest := skipOptions(args, "wE", "--timeout", "--conflict-exit-code")
	if len(rest) == 0 || (len(rest) == 1 && isDigits(rest[0].Value)) {
		return nil
	}
	rest = rest[1:]
	for len(rest) > 0 && strings.HasPrefix(rest[0].Value, "-") && len(rest[0].Value) > 1 {
		a := rest[0].Value
		if a == "-c" || a == "--command" {
			if len(rest) > 1 {
				w.scriptWord(rest[1], rs, "flock -c", stdin)
			}
			return nil
		}
		if a == "--" {
			rest = rest[1:]
			break
		}
		if a == "-w" || a == "-E" || a == "--timeout" || a == "--conflict-exit-code" {
			rest = rest[1:]
		}
		if len(rest) > 0 {
			rest = rest[1:]
		}
	}
	return rest
}

func (w *walker) watchArgs(args []Word, rs *runState) {
	execMode := false
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
		if a == "-x" || a == "--exec" {
			execMode = true
		}
		if a == "-n" || a == "--interval" || a == "-q" || a == "--equexit" {
			i++
		}
		i++
	}
	if i >= len(args) {
		return
	}
	rest := args[i:]
	if execMode {
		w.run(rest, rs.env, rs.env.child(rs.dir, rs.dirKnown), nil, nil, QuoteArgs(wordValues(rest)))
		return
	}
	code, dyn := joinWords(rest)
	w.scriptWord(Word{Value: code, Dynamic: dyn}, rs, "watch", nil)
}

func (w *walker) scriptWord(code Word, rs *runState, label string, stdin *Stdin) {
	if code.Dynamic {
		w.issue("dynamic-code", label+" runs code that is only known at run time")
	}
	saved := w.via
	w.via = rs.via
	w.script(code.Value, rs.env.child(rs.dir, rs.dirKnown), label, stdin)
	w.via = saved
}

func isAssignment(s string) bool {
	eq := strings.IndexByte(s, '=')
	if eq <= 0 {
		return false
	}
	for i := 0; i < eq; i++ {
		c := s[i]
		if c != '_' && (c < 'a' || c > 'z') && (c < 'A' || c > 'Z') && (i == 0 || c < '0' || c > '9') {
			return false
		}
	}
	return true
}

// splitWords splits a string the way env -S does: on blanks, honouring
// single quotes, double quotes and backslashes.
func splitWords(s string) []string {
	var out []string
	var cur strings.Builder
	inWord := false
	var quote byte
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case quote == '\'':
			if c == '\'' {
				quote = 0
			} else {
				cur.WriteByte(c)
			}
		case quote == '"':
			switch {
			case c == '"':
				quote = 0
			case c == '\\' && i+1 < len(s):
				i++
				cur.WriteByte(s[i])
			default:
				cur.WriteByte(c)
			}
		case c == '\'' || c == '"':
			quote = c
			inWord = true
		case c == '\\' && i+1 < len(s):
			i++
			cur.WriteByte(s[i])
			inWord = true
		case c == ' ' || c == '\t' || c == '\n':
			if inWord {
				out = append(out, cur.String())
				cur.Reset()
				inWord = false
			}
		default:
			cur.WriteByte(c)
			inWord = true
		}
	}
	if inWord {
		out = append(out, cur.String())
	}
	return out
}

func wordValues(ws []Word) []string {
	out := make([]string, len(ws))
	for i, w := range ws {
		out[i] = w.Value
	}
	return out
}
