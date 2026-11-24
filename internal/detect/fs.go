package detect

import (
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"

	"github.com/celaldrg/undoguard/internal/parser"
)

// pathFinding builds one finding whose targets are the given operands.
func pathFinding(c *parser.Command, ws []parser.Word, op Op, rule, what string, recursive bool) Finding {
	f := Finding{Op: op, Rule: rule, What: what, Command: c}
	for _, w := range ws {
		t := target(c, w)
		t.Recursive = t.Recursive || recursive
		f.Targets = append(f.Targets, t)
	}
	return f
}

// fileWrites builds findings for programs that open their operands for
// writing. Devices are treated the way redirections treat them, and "-"
// stands for standard output.
func fileWrites(c *parser.Command, ws []parser.Word, op Op, rule, what string) []Finding {
	f := Finding{Op: op, Rule: rule, What: what, Command: c}
	var out []Finding
	for _, w := range ws {
		if w.Value == "-" && !w.Dynamic {
			continue
		}
		t := target(c, w)
		t.Follow = true
		if !t.Unresolved && isHarmlessSink(t.Path) {
			continue
		}
		if !t.Unresolved && isDevice(t.Path) {
			dev := Finding{Op: OpCritical, Rule: rule + "-device", What: "writes straight to the device " + t.Path, Command: c}
			dev.Targets = []Target{t}
			out = append(out, dev)
			continue
		}
		f.Targets = append(f.Targets, t)
	}
	if len(f.Targets) > 0 {
		out = append(out, f)
	}
	return out
}

func rm(c *parser.Command) []Finding {
	o := parseOpts(c.Args, "")
	if o.has("help", "version") || len(o.operands) == 0 {
		return nil
	}
	rec := o.has("r", "R", "recursive")
	what := "deletes files"
	if rec {
		what = "deletes files and directories with everything in them"
	}
	f := pathFinding(c, o.operands, OpDelete, "rm", what, rec)
	for i, w := range o.operands {
		// rm -r link/ goes through the link and empties the directory it
		// points to.
		if rec && strings.HasSuffix(w.Value, "/") {
			f.Targets[i].Follow = true
		}
	}
	return []Finding{f}
}

func rmdir(c *parser.Command) []Finding {
	o := parseOpts(c.Args, "")
	if o.has("help", "version") || len(o.operands) == 0 {
		return nil
	}
	f := Finding{Op: OpDelete, Rule: "rmdir", What: "removes empty directories", Command: c}
	for _, w := range o.operands {
		t := target(c, w)
		f.Targets = append(f.Targets, t)
		if t.Unresolved || !o.has("p", "parents") {
			continue
		}
		// rmdir -p a/b/c also removes a/b and a.
		cur := strings.TrimRight(w.Value, "/")
		for {
			parent := filepath.Dir(cur)
			if parent == "." || parent == "/" || parent == cur {
				break
			}
			f.Targets = append(f.Targets, target(c, parser.Word{Value: parent}))
			cur = parent
		}
	}
	return []Finding{f}
}

func shred(c *parser.Command) []Finding {
	o := parseOpts(c.Args, "ns", "iterations", "size", "random-source")
	if o.has("help", "version") || len(o.operands) == 0 {
		return nil
	}
	out := fileWrites(c, o.operands, OpOverwrite, "shred", "overwrites the file's content with noise")
	if o.has("u", "remove") {
		out = append(out, pathFinding(c, o.operands, OpDelete, "shred", "deletes the file after overwriting it", false))
	}
	return out
}

func truncate(c *parser.Command) []Finding {
	o := parseOpts(c.Args, "rs", "reference", "size")
	if o.has("help", "version") {
		return nil
	}
	op, what := OpTruncate, "cuts the file to a new size"
	if v, ok := o.value("s", "size"); ok && (strings.HasPrefix(v, "+") || strings.HasPrefix(v, ">")) {
		op, what = OpModify, "grows the file"
	}
	return fileWrites(c, o.operands, op, "truncate", what)
}

// isDirAt reports whether p is a directory. With follow, a symlink to a
// directory counts as one.
func isDirAt(p string, follow bool) bool {
	fi, err := os.Lstat(p)
	if err != nil {
		return false
	}
	if fi.IsDir() {
		return true
	}
	if follow && fi.Mode()&os.ModeSymlink != 0 {
		st, err := os.Stat(p)
		return err == nil && st.IsDir()
	}
	return false
}

// below is where a source lands inside a destination directory.
func below(dir Target, src parser.Word) Target {
	if dir.Unresolved {
		return dir
	}
	base := filepath.Base(strings.TrimRight(src.Value, "/"))
	if src.Dynamic || base == "" || base == "." || base == "/" || base == ".." {
		// The name is unknown, so anything in the directory may be hit.
		return Target{Path: dir.Path, Raw: dir.Raw, Recursive: true}
	}
	return Target{Path: filepath.Join(dir.Path, base), Raw: filepath.Join(dir.Raw, base)}
}

// cwdTarget is the working directory as a target.
func cwdTarget(c *parser.Command) Target {
	if !c.DirKnown {
		return unresolved(".", "relative to a directory that is not known")
	}
	return Target{Path: c.Dir, Raw: "."}
}

// landing works out where copies, moves and links are written: the
// sources and one destination for each. It understands -t DIR, -T and a
// last operand that is an existing directory.
func landing(c *parser.Command, o opts, followDir bool) ([]parser.Word, []Target) {
	if w, ok := o.word("t", "target-directory"); ok {
		dir := target(c, w)
		var dests []Target
		for _, s := range o.operands {
			dests = append(dests, below(dir, s))
		}
		return o.operands, dests
	}
	n := len(o.operands)
	if n < 2 {
		return nil, nil
	}
	srcs := o.operands[:n-1]
	lastWord := o.operands[n-1]
	last := target(c, lastWord)
	if o.has("T", "no-target-directory") {
		return srcs, []Target{last}
	}
	intoDir := n > 2 || strings.HasSuffix(lastWord.Value, "/")
	if !last.Unresolved && isDirAt(last.Path, followDir) {
		intoDir = true
	}
	if !intoDir {
		return srcs, []Target{last}
	}
	var dests []Target
	for _, s := range srcs {
		dests = append(dests, below(last, s))
	}
	return srcs, dests
}

// noClobber reports whether existing destinations are left alone.
func noClobber(o opts) bool {
	if o.has("n", "no-clobber") {
		return true
	}
	v, ok := o.value("update")
	return ok && v == "none"
}

func mv(c *parser.Command) []Finding {
	o := parseOpts(c.Args, "tS", "target-directory", "suffix")
	if o.has("help", "version") {
		return nil
	}
	srcs, dests := landing(c, o, true)
	if len(srcs) == 0 {
		return nil
	}
	out := []Finding{pathFinding(c, srcs, OpDelete, "mv", "moves away, so the old path stops existing", true)}
	if !noClobber(o) {
		f := Finding{Op: OpOverwrite, Rule: "mv", What: "replaces what is at the destination", Command: c, Targets: dests}
		for i := range f.Targets {
			f.Targets[i].Recursive = true
		}
		out = append(out, f)
	}
	return out
}

func cp(c *parser.Command) []Finding {
	o := parseOpts(c.Args, "tS", "target-directory", "suffix", "no-preserve", "sparse")
	if o.has("help", "version") || noClobber(o) {
		return nil
	}
	_, dests := landing(c, o, true)
	if len(dests) == 0 {
		return nil
	}
	rec := o.has("r", "R", "a", "recursive", "archive")
	f := Finding{Op: OpOverwrite, Rule: "cp", What: "copies over what is at the destination", Command: c, Targets: dests}
	for i := range f.Targets {
		f.Targets[i].Recursive = f.Targets[i].Recursive || rec
		// cp writes through a symlink at the destination.
		f.Targets[i].Follow = !o.has("remove-destination")
	}
	return []Finding{f}
}

func install(c *parser.Command) []Finding {
	o := parseOpts(c.Args, "gmoSt", "group", "mode", "owner", "suffix", "target-directory", "strip-program")
	if o.has("help", "version", "d", "directory") {
		return nil
	}
	_, dests := landing(c, o, true)
	if len(dests) == 0 {
		return nil
	}
	return []Finding{{Op: OpOverwrite, Rule: "install", What: "replaces the destination file", Command: c, Targets: dests}}
}

func ln(c *parser.Command) []Finding {
	o := parseOpts(c.Args, "St", "suffix", "target-directory")
	if o.has("help", "version") || !o.has("f", "force") {
		// Without -f, ln refuses to replace anything.
		return nil
	}
	var dests []Target
	if _, ok := o.word("t", "target-directory"); !ok && len(o.operands) == 1 {
		dests = []Target{below(cwdTarget(c), o.operands[0])}
	} else {
		_, dests = landing(c, o, !o.has("n", "no-dereference"))
	}
	if len(dests) == 0 {
		return nil
	}
	return []Finding{{Op: OpOverwrite, Rule: "ln", What: "replaces the destination with a link", Command: c, Targets: dests}}
}

var rsyncLongValues = []string{
	"rsh", "rsync-path", "exclude", "include", "filter", "files-from", "exclude-from", "include-from",
	"backup-dir", "suffix", "temp-dir", "compare-dest", "copy-dest", "link-dest", "partial-dir",
	"log-file", "log-file-format", "password-file", "port", "chmod", "chown", "usermap", "groupmap",
	"timeout", "contimeout", "bwlimit", "max-size", "min-size", "max-delete", "max-alloc", "out-format",
	"block-size", "modify-window", "info", "debug", "sockopts", "address", "protocol", "iconv",
	"checksum-choice", "cc", "compress-choice", "zc", "compress-level", "zl", "skip-compress",
	"remote-option", "stop-after", "stop-at", "outbuf", "write-batch", "only-write-batch", "read-batch",
	"early-input", "checksum-seed",
}

// isRemoteSpec reports whether an rsync or scp operand names another host.
func isRemoteSpec(v string) bool {
	if strings.HasPrefix(v, "rsync://") || strings.Contains(v, "::") {
		return true
	}
	colon := strings.IndexByte(v, ':')
	slash := strings.IndexByte(v, '/')
	return colon > 0 && (slash < 0 || colon < slash)
}

func rsync(c *parser.Command) []Finding {
	o := parseOpts(c.Args, "eBfTM@", rsyncLongValues...)
	if o.has("help", "version", "list-only", "n", "dry-run") || len(o.operands) < 2 {
		return nil
	}
	n := len(o.operands)
	destWord := o.operands[n-1]
	srcs := o.operands[:n-1]
	if !destWord.Dynamic && isRemoteSpec(destWord.Value) {
		host, _, _ := strings.Cut(strings.TrimPrefix(destWord.Value, "rsync://"), ":")
		return []Finding{{Op: OpExternal, Rule: "rsync", What: "writes files on another host", Command: c, Resource: "remote host " + host}}
	}
	dest := target(c, destWord)
	dest.Recursive = true
	var out []Finding
	if o.hasPrefix("delete") || o.has("del") {
		out = append(out, Finding{Op: OpDelete, Rule: "rsync-delete", What: "deletes files in the destination that the source lacks", Command: c, Targets: []Target{dest}})
	}
	out = append(out, Finding{Op: OpOverwrite, Rule: "rsync", What: "copies into the destination, replacing what is there", Command: c, Targets: []Target{dest}})
	if o.has("remove-source-files") {
		var local []parser.Word
		for _, s := range srcs {
			if s.Dynamic || !isRemoteSpec(s.Value) {
				local = append(local, s)
			}
		}
		if len(local) > 0 {
			out = append(out, pathFinding(c, local, OpDelete, "rsync", "deletes the source files once they are sent", true))
		}
	}
	return out
}

func tee(c *parser.Command) []Finding {
	o := parseOpts(c.Args, "")
	if o.has("help", "version") {
		return nil
	}
	op, what := OpOverwrite, "writes its input over the file"
	if o.has("a", "append") {
		op, what = OpAppend, "appends its input to the file"
	}
	return fileWrites(c, o.operands, op, "tee", what)
}

func dd(c *parser.Command) []Finding {
	var of parser.Word
	have := false
	conv, oflag := "", ""
	for _, a := range c.Args {
		k, v, ok := strings.Cut(a.Value, "=")
		if !ok {
			if a.Dynamic {
				t := unresolved(a.Value, "a dd operand is only known at run time")
				return []Finding{{Op: OpOverwrite, Rule: "dd", What: "may write to a file or device", Command: c, Targets: []Target{t}}}
			}
			continue
		}
		switch k {
		case "of":
			of = parser.Word{Value: v, Dynamic: a.Dynamic}
			have = true
		case "conv":
			conv = v
		case "oflag":
			oflag = v
		}
	}
	if !have {
		return nil
	}
	op, what := OpOverwrite, "writes over the output file"
	if strings.Contains(conv, "notrunc") {
		op, what = OpModify, "writes into the output file in place"
		if strings.Contains(oflag, "append") {
			op, what = OpAppend, "appends to the output file"
		}
	}
	return fileWrites(c, []parser.Word{of}, op, "dd", what)
}

// isSedSuffix reports whether w reads as a BSD sed -i backup suffix.
func isSedSuffix(w parser.Word) bool {
	return !w.Dynamic && (w.Value == "" || strings.HasPrefix(w.Value, ".") && !strings.Contains(w.Value, "/"))
}

func sed(c *parser.Command) []Finding {
	// BSD sed on macOS takes the backup suffix of -i as its own argument.
	// GNU sed is often first on PATH there too, so only a word that reads as
	// a suffix ('' or .bak) is taken as one; anything else is read the GNU way,
	// which never misses a file the BSD reading would edit.
	bsd := runtime.GOOS == "darwin" && c.Name == "sed"
	inPlace, haveScript, follow := false, false, false
	var operands []parser.Word
	args := c.Args
	for i := 0; i < len(args); i++ {
		a := args[i]
		v := a.Value
		if a.Dynamic || len(v) < 2 || v[0] != '-' {
			operands = append(operands, a)
			continue
		}
		if v == "--" {
			operands = append(operands, args[i+1:]...)
			break
		}
		if strings.HasPrefix(v, "--") {
			name, _, hasVal := strings.Cut(v[2:], "=")
			switch name {
			case "in-place":
				inPlace = true
			case "expression", "file":
				haveScript = true
				if !hasVal {
					i++
				}
			case "line-length":
				if !hasVal {
					i++
				}
			case "follow-symlinks":
				follow = true
			}
			continue
		}
		for j := 1; j < len(v); j++ {
			switch v[j] {
			case 'i', 'I':
				inPlace = true
				if bsd && j+1 == len(v) && i+1 < len(args) && isSedSuffix(args[i+1]) {
					i++
				}
				j = len(v)
			case 'e', 'f':
				haveScript = true
				if j+1 == len(v) {
					i++
				}
				j = len(v)
			case 'l':
				if j+1 == len(v) {
					i++
				}
				j = len(v)
			}
		}
	}
	if !inPlace {
		return nil
	}
	if !haveScript && len(operands) > 0 {
		operands = operands[1:]
	}
	if len(operands) == 0 {
		return nil
	}
	f := pathFinding(c, operands, OpModify, "sed-in-place", "edits the file in place", false)
	for i := range f.Targets {
		f.Targets[i].Follow = follow
	}
	return []Finding{f}
}

var interpName = regexp.MustCompile(`^(perl|ruby)[0-9.]*$`)

// inPlaceInterp finds perl -i and ruby -i, which edit their file operands.
func inPlaceInterp(c *parser.Command) []Finding {
	if !interpName.MatchString(c.Name) {
		return nil
	}
	perl := strings.HasPrefix(c.Name, "perl")
	separate := "ICr"
	if perl {
		separate = "I"
	}
	inPlace, haveCode := false, false
	var operands []parser.Word
	args := c.Args
	for i := 0; i < len(args); i++ {
		v := args[i].Value
		if args[i].Dynamic || len(v) < 2 || v[0] != '-' {
			operands = append(operands, args[i:]...)
			break
		}
		if v == "--" {
			operands = append(operands, args[i+1:]...)
			break
		}
		if strings.HasPrefix(v, "--") {
			continue
		}
		for j := 1; j < len(v); j++ {
			ch := v[j]
			switch {
			case ch == 'i':
				inPlace = true
				j = len(v)
			case ch == 'e' || (ch == 'E' && perl):
				haveCode = true
				if j+1 == len(v) {
					i++
				}
				j = len(v)
			case strings.IndexByte(separate, ch) >= 0:
				if j+1 == len(v) {
					i++
				}
				j = len(v)
			case strings.IndexByte("0lxMmFCDdEKTW", ch) >= 0:
				j = len(v)
			}
		}
	}
	if !inPlace {
		return nil
	}
	if !haveCode && len(operands) > 0 {
		operands = operands[1:]
	}
	if len(operands) == 0 {
		return nil
	}
	return []Finding{pathFinding(c, operands, OpModify, c.Name+"-in-place", "edits the file in place", false)}
}

// chmodMode matches symbolic modes such as -w or -x,u+r, which chmod
// accepts even though they look like options.
var chmodMode = regexp.MustCompile(`^-[rwxXst][rwxXstugoa,+=0-7-]*$`)

func chmodLike(name string) detector {
	return func(c *parser.Command) []Finding {
		args := c.Args
		if name == "chmod" {
			args = make([]parser.Word, len(c.Args))
			copy(args, c.Args)
			for i, a := range args {
				if !a.Dynamic && chmodMode.MatchString(a.Value) {
					args[i].Value = "mode" + a.Value
				}
			}
		}
		o := parseOpts(args, "", "reference", "from")
		if o.has("help", "version") {
			return nil
		}
		ops := o.operands
		if !o.has("reference") {
			if len(ops) == 0 {
				return nil
			}
			ops = ops[1:]
		}
		if len(ops) == 0 {
			return nil
		}
		rec := o.has("R", "recursive")
		what := "changes permissions"
		if name != "chmod" {
			what = "changes ownership"
		}
		if rec {
			what += " of everything below"
		}
		f := pathFinding(c, ops, OpMetadata, name, what, rec)
		follow := name == "chmod" || !o.has("h", "no-dereference")
		for i := range f.Targets {
			f.Targets[i].Follow = follow
		}
		return []Finding{f}
	}
}

// findValuePrimaries are find tests and actions that take one argument.
var findValuePrimaries = setOf(
	"-name", "-iname", "-path", "-ipath", "-wholename", "-iwholename", "-regex", "-iregex",
	"-lname", "-ilname", "-newer", "-anewer", "-cnewer", "-type", "-xtype", "-user", "-group",
	"-perm", "-size", "-mtime", "-mmin", "-atime", "-amin", "-ctime", "-cmin", "-maxdepth",
	"-mindepth", "-samefile", "-inum", "-links", "-uid", "-gid", "-used", "-fstype", "-context",
	"-printf", "-regextype", "-files0-from", "-newermt", "-newerct", "-neweraa",
)

func isFindExprStart(w parser.Word) bool {
	if w.Dynamic {
		return false
	}
	v := w.Value
	return (len(v) > 1 && v[0] == '-') || v == "(" || v == "!" || v == ","
}

func find(c *parser.Command) []Finding {
	args := c.Args
	i := 0
	for i < len(args) && !args[i].Dynamic {
		v := args[i].Value
		if v == "-H" || v == "-L" || v == "-P" || strings.HasPrefix(v, "-O") {
			i++
			continue
		}
		if v == "-D" {
			i += 2
			continue
		}
		break
	}
	var roots []parser.Word
	for i < len(args) && !isFindExprStart(args[i]) {
		roots = append(roots, args[i])
		i++
	}
	if len(roots) == 0 {
		roots = []parser.Word{{Value: "."}}
	}
	var expr []parser.Word
	if i < len(args) {
		expr = args[i:]
	}
	del, unsure := false, false
	var outs []parser.Word
	for j := 0; j < len(expr); j++ {
		w := expr[j]
		if w.Dynamic {
			unsure = true
			continue
		}
		switch v := w.Value; {
		case v == "-delete":
			del = true
		case v == "-fprint" || v == "-fprint0" || v == "-fls":
			if j+1 < len(expr) {
				outs = append(outs, expr[j+1])
				j++
			}
		case v == "-fprintf":
			if j+1 < len(expr) {
				outs = append(outs, expr[j+1])
				j += 2
			}
		case v == "-exec" || v == "-execdir" || v == "-ok" || v == "-okdir":
			// The command itself is analysed on its own; skip to its end.
			for j++; j < len(expr); j++ {
				if expr[j].Value == ";" || (expr[j].Value == "+" && expr[j-1].Value == "{}") {
					break
				}
			}
		case findValuePrimaries[v]:
			j++
		}
	}
	var out []Finding
	if del || unsure {
		f := pathFinding(c, roots, OpDelete, "find-delete", "deletes every file the expression matches", true)
		if !del {
			f.What = "may delete files: part of the expression is only known at run time"
			f.Targets = append(f.Targets, unresolved("", "part of the find expression is only known at run time"))
		}
		out = append(out, f)
	}
	if len(outs) > 0 {
		out = append(out, fileWrites(c, outs, OpOverwrite, "find-fprint", "writes the list of matches over the file")...)
	}
	return out
}

var diskutilErase = setOf(
	"erasedisk", "erasevolume", "reformat", "partitiondisk", "zerodisk", "randomdisk",
	"secureerase", "splitpartition", "mergepartitions", "eraseoptical",
)

func diskutil(c *parser.Command) []Finding {
	o := parseOpts(c.Args, "")
	if len(o.operands) == 0 {
		return nil
	}
	sub := strings.ToLower(o.operands[0].Value)
	destructive := diskutilErase[sub] || o.operands[0].Dynamic
	if (sub == "apfs" || sub == "cs" || sub == "appleraid" || sub == "ar") && len(o.operands) > 1 {
		sub2 := strings.ToLower(o.operands[1].Value)
		destructive = strings.HasPrefix(sub2, "delete") || strings.HasPrefix(sub2, "erase") || o.operands[1].Dynamic
	}
	if !destructive {
		return nil
	}
	return []Finding{{Op: OpCritical, Rule: "diskutil", What: "erases or repartitions a disk", Command: c, Resource: "disk"}}
}
