// Package detect decides which commands are destructive and which paths
// or external resources they touch.
package detect

import (
	"path/filepath"
	"strings"

	"github.com/celaldrg/undoguard/internal/parser"
	"github.com/celaldrg/undoguard/internal/paths"
)

// Op is the kind of change a command makes.
type Op string

const (
	// OpDelete removes files or directories.
	OpDelete Op = "delete"
	// OpOverwrite replaces the content of a file.
	OpOverwrite Op = "overwrite"
	// OpTruncate cuts a file short.
	OpTruncate Op = "truncate"
	// OpModify edits a file in place.
	OpModify Op = "modify"
	// OpMetadata changes permissions or ownership.
	OpMetadata Op = "metadata"
	// OpAppend adds to the end of a file.
	OpAppend Op = "append"
	// OpExternal changes state outside the local files: containers,
	// clusters, services, databases or remote repositories.
	OpExternal Op = "external"
	// OpCritical destroys a disk or file system.
	OpCritical Op = "critical"
)

// GitScope narrows a target to the files a git command can change.
type GitScope string

const (
	// GitTracked is the tracked files with local changes.
	GitTracked GitScope = "tracked"
	// GitUntracked is the untracked files that are not ignored.
	GitUntracked GitScope = "untracked"
	// GitUntrackedIgnored is every untracked file, ignored or not.
	GitUntrackedIgnored GitScope = "untracked+ignored"
	// GitIgnored is the ignored files only.
	GitIgnored GitScope = "ignored"
)

// Target is one path a finding touches.
type Target struct {
	// Path is the absolute, lexically cleaned path. It is empty when the
	// target could not be resolved.
	Path string
	// Raw is the operand as the command received it.
	Raw string
	// Recursive is set when everything below Path is affected.
	Recursive bool
	// Follow is set when the operation acts on what a symlink at Path
	// points to, rather than on the link itself.
	Follow bool
	// Unresolved is set when the path could not be worked out.
	Unresolved bool
	// Reason explains an unresolved target.
	Reason string
	// Git, when set, narrows a recursive target to files in one git state.
	Git GitScope
	// Repo is the repository root for git targets.
	Repo string
}

// Finding is one destructive effect of one command.
type Finding struct {
	Op      Op
	Rule    string
	What    string
	Command *parser.Command
	Targets []Target
	// Resource names what an external finding acts on, such as
	// "docker volume" or "postgres database".
	Resource string
}

// Analyze returns the findings for every command in a parse result.
func Analyze(res *parser.Result) []Finding {
	var out []Finding
	for _, c := range res.Commands {
		out = append(out, Command(c)...)
	}
	return out
}

// Command returns the findings for one command, including its
// redirections.
func Command(c *parser.Command) []Finding {
	var out []Finding
	out = append(out, redirects(c)...)
	if c.NameDynamic || c.Name == ":" {
		return out
	}
	d, ok := detectors[c.Name]
	if !ok {
		for prefix, pd := range prefixDetectors {
			if strings.HasPrefix(c.Name, prefix) {
				d, ok = pd, true
				break
			}
		}
	}
	if !ok {
		return out
	}
	found := d(c)
	if w, opaque := opaqueArg(c); opaque && !anyUnresolved(found) {
		t := unresolved(w.Value, "an argument is only known at run time and may be an option that changes what this command does")
		if len(found) == 0 {
			found = []Finding{{Op: OpModify, Rule: c.Name + "-dynamic", What: "cannot be worked out before it runs", Command: c}}
		}
		found[0].Targets = append(found[0].Targets, t)
	}
	return append(out, found...)
}

// opaqueArg returns a run-time-only argument that sits where an option or
// subcommand could be: before the first plain operand, with no known text
// that rules out a leading dash.
func opaqueArg(c *parser.Command) (parser.Word, bool) {
	for _, a := range c.Args {
		if a.Dynamic {
			if a.Value == "" || strings.HasPrefix(a.Value, "-") {
				return a, true
			}
			continue
		}
		if a.Value == "--" || !strings.HasPrefix(a.Value, "-") {
			return parser.Word{}, false
		}
	}
	return parser.Word{}, false
}

func anyUnresolved(fs []Finding) bool {
	for _, f := range fs {
		for _, t := range f.Targets {
			if t.Unresolved {
				return true
			}
		}
	}
	return false
}

type detector func(c *parser.Command) []Finding

var detectors map[string]detector

var prefixDetectors = map[string]detector{
	"mkfs":   critical("mkfs", "creates a new file system, destroying what was there"),
	"python": inPlaceInterp,
	"perl":   inPlaceInterp,
	"ruby":   inPlaceInterp,
}

func init() {
	detectors = map[string]detector{}
	register(rm, "rm")
	register(rmdir, "rmdir")
	register(simple(OpDelete, "unlink", "deletes a file", false), "unlink")
	register(shred, "shred")
	register(truncate, "truncate")
	register(mv, "mv")
	register(cp, "cp")
	register(install, "install")
	register(ln, "ln")
	register(rsync, "rsync")
	register(tee, "tee")
	register(dd, "dd")
	register(sed, "sed", "gsed")
	register(chmodLike("chmod"), "chmod")
	register(chmodLike("chown"), "chown")
	register(chmodLike("chgrp"), "chgrp")
	register(find, "find")
	register(tar, "tar", "gtar", "bsdtar")
	register(unzip, "unzip")
	register(git, "git")
	register(container("docker"), "docker")
	register(container("podman"), "podman")
	register(container("nerdctl"), "nerdctl")
	register(compose, "docker-compose", "podman-compose")
	register(kubectl, "kubectl", "oc")
	register(helm, "helm")
	register(systemctl, "systemctl")
	register(service, "service")
	register(launchctl, "launchctl")
	register(crontab, "crontab")
	register(psql, "psql")
	register(mysql, "mysql", "mariadb")
	register(sqlite, "sqlite3")
	register(external("dropdb", "postgres database", "drops a database"), "dropdb")
	register(mysqladmin, "mysqladmin")
	register(redis, "redis-cli")
	register(critical("mke2fs", "creates a new file system, destroying what was there"), "mke2fs")
	register(critical("mkswap", "formats a device as swap"), "mkswap")
	register(critical("wipefs", "erases file system signatures"), "wipefs")
	register(critical("blkdiscard", "discards every block of a device"), "blkdiscard")
	register(critical("sgdisk", "rewrites a partition table"), "sgdisk")
	register(critical("sfdisk", "rewrites a partition table"), "sfdisk")
	register(critical("fdisk", "edits a partition table"), "fdisk")
	register(critical("gdisk", "edits a partition table"), "gdisk")
	register(critical("parted", "edits a partition table"), "parted")
	register(critical("newfs", "creates a new file system, destroying what was there"), "newfs")
	register(diskutil, "diskutil")
	register(external("shutdown", "host", "shuts the machine down"), "shutdown")
	register(external("reboot", "host", "restarts the machine"), "reboot")
	register(external("halt", "host", "halts the machine"), "halt")
	register(external("poweroff", "host", "powers the machine off"), "poweroff")
}

func register(d detector, names ...string) {
	for _, n := range names {
		detectors[n] = d
	}
}

// target turns an operand into a Target.
func target(c *parser.Command, w parser.Word) Target {
	t := Target{Raw: w.Value, Recursive: w.Subtree}
	switch {
	case w.Dynamic:
		t.Unresolved = true
		t.Reason = "depends on a value only known at run time"
	case w.Value == "":
		t.Unresolved = true
		t.Reason = "empty path"
	case !filepath.IsAbs(w.Value) && !c.DirKnown:
		t.Unresolved = true
		t.Reason = "relative to a directory that is not known"
	default:
		t.Path = paths.Abs(c.Dir, w.Value)
	}
	return t
}

// unresolved is a target that stands for paths that cannot be listed.
func unresolved(raw, reason string) Target {
	return Target{Raw: raw, Unresolved: true, Reason: reason}
}

var harmlessDevices = setOf(
	"/dev/null", "/dev/stdout", "/dev/stderr", "/dev/stdin", "/dev/tty", "/dev/zero",
	"/dev/random", "/dev/urandom", "/dev/full",
)

// isDevice reports whether p names a block or character device that holds
// data, such as a disk.
func isDevice(p string) bool {
	if !strings.HasPrefix(p, "/dev/") || harmlessDevices[p] || strings.HasPrefix(p, "/dev/fd/") ||
		strings.HasPrefix(p, "/dev/pts/") || strings.HasPrefix(p, "/dev/shm/") {
		return false
	}
	return true
}

func isHarmlessSink(p string) bool {
	return harmlessDevices[p] || strings.HasPrefix(p, "/dev/fd/") || strings.HasPrefix(p, "/dev/pts/")
}

func redirects(c *parser.Command) []Finding {
	var out []Finding
	for _, r := range c.Redirs {
		var op Op
		switch r.Op {
		case ">", ">|", "&>", ">&":
			op = OpOverwrite
		case ">>", "&>>":
			op = OpAppend
		case "<>":
			op = OpModify
		default:
			continue
		}
		t := target(c, r.Target)
		t.Follow = true
		if !t.Unresolved && isHarmlessSink(t.Path) {
			continue
		}
		if !t.Unresolved && isDevice(t.Path) {
			what := "writes straight to the device " + t.Path
			out = append(out, Finding{Op: OpCritical, Rule: "redirect-device", What: what, Command: c, Targets: []Target{t}})
			continue
		}
		what := "redirection " + r.Op + " replaces the file's content"
		switch op {
		case OpAppend:
			what = "redirection " + r.Op + " appends to the file"
		case OpModify:
			what = "redirection <> opens the file for writing"
		}
		out = append(out, Finding{Op: op, Rule: "redirect", What: what, Command: c, Targets: []Target{t}})
	}
	return out
}

func simple(op Op, rule, what string, recursive bool) detector {
	return func(c *parser.Command) []Finding {
		o := parseOpts(c.Args, "")
		if len(o.operands) == 0 {
			return nil
		}
		f := Finding{Op: op, Rule: rule, What: what, Command: c}
		for _, w := range o.operands {
			t := target(c, w)
			t.Recursive = t.Recursive || recursive
			f.Targets = append(f.Targets, t)
		}
		return []Finding{f}
	}
}

func critical(rule, what string) detector {
	return func(c *parser.Command) []Finding {
		o := parseOpts(c.Args, "")
		f := Finding{Op: OpCritical, Rule: rule, What: what, Command: c}
		for _, w := range o.operands {
			f.Targets = append(f.Targets, target(c, w))
		}
		if len(o.flags) == 1 && (o.has("help") || o.has("version")) {
			return nil
		}
		if rule == "fdisk" || rule == "gdisk" || rule == "parted" {
			if o.has("l") || o.has("list") {
				return nil
			}
		}
		return []Finding{f}
	}
}

func external(rule, resource, what string) detector {
	return func(c *parser.Command) []Finding {
		o := parseOpts(c.Args, "")
		if o.has("help") || o.has("version") {
			return nil
		}
		return []Finding{{Op: OpExternal, Rule: rule, What: what, Command: c, Resource: resource}}
	}
}

// opts is the result of parsing a command's options.
type opts struct {
	flags    map[string]string
	words    map[string]parser.Word
	multi    map[string][]parser.Word
	order    []string
	operands []parser.Word
}

func (o opts) has(names ...string) bool {
	for _, n := range names {
		if _, ok := o.flags[n]; ok {
			return true
		}
	}
	return false
}

func (o opts) value(names ...string) (string, bool) {
	for _, n := range names {
		if v, ok := o.flags[n]; ok {
			return v, true
		}
	}
	return "", false
}

// word is like value but keeps whether the value is only known at run
// time.
func (o opts) word(names ...string) (parser.Word, bool) {
	for _, n := range names {
		if w, ok := o.words[n]; ok {
			return w, true
		}
	}
	return parser.Word{}, false
}

// all returns every value given for the options, in order.
func (o opts) all(names ...string) []parser.Word {
	var out []parser.Word
	seen := map[string]int{}
	for _, k := range o.order {
		for _, n := range names {
			if k == n {
				out = append(out, o.multi[k][seen[k]])
				seen[k]++
			}
		}
	}
	return out
}

// hasPrefix reports whether any option name starts with prefix.
func (o opts) hasPrefix(prefix string) bool {
	for _, k := range o.order {
		if strings.HasPrefix(k, prefix) {
			return true
		}
	}
	return false
}

// parseOpts splits arguments into options and operands the way GNU
// getopt_long does, including options after operands. shortVal lists the
// short options that take a value and longVal the long options that take
// one. Flags are keyed by letter or long name, without dashes.
func parseOpts(args []parser.Word, shortVal string, longVal ...string) opts {
	o := opts{flags: map[string]string{}, words: map[string]parser.Word{}, multi: map[string][]parser.Word{}}
	set := func(k string, w parser.Word) {
		o.flags[k] = w.Value
		o.words[k] = w
		o.multi[k] = append(o.multi[k], w)
		o.order = append(o.order, k)
	}
	for i := 0; i < len(args); i++ {
		a := args[i].Value
		if args[i].Dynamic {
			o.operands = append(o.operands, args[i])
			continue
		}
		if a == "--" {
			o.operands = append(o.operands, args[i+1:]...)
			break
		}
		if len(a) < 2 || a[0] != '-' {
			o.operands = append(o.operands, args[i])
			continue
		}
		if strings.HasPrefix(a, "--") {
			k, v, has := strings.Cut(a[2:], "=")
			w := parser.Word{Value: v}
			if !has {
				for _, l := range longVal {
					if l == k && i+1 < len(args) {
						i++
						w = args[i]
						break
					}
				}
			}
			set(k, w)
			continue
		}
		for j := 1; j < len(a); j++ {
			ch := a[j : j+1]
			if strings.Contains(shortVal, ch) {
				w := parser.Word{Value: a[j+1:]}
				if w.Value == "" && i+1 < len(args) {
					i++
					w = args[i]
				}
				set(ch, w)
				break
			}
			set(ch, parser.Word{})
		}
	}
	return o
}

// setOf builds a lookup set from a list of names.
func setOf(items ...string) map[string]bool {
	m := make(map[string]bool, len(items))
	for _, it := range items {
		m[it] = true
	}
	return m
}
