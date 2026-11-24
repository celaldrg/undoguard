package detect

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/celaldrg/undoguard/internal/parser"
	"github.com/celaldrg/undoguard/internal/paths"
)

// gitExecConfig matches configuration keys whose value git runs as a
// program, or that pull in more configuration. Setting one with -c makes
// the call unpredictable.
var gitExecConfig = regexp.MustCompile(`(?i)^(alias\.|core\.(fsmonitor|hookspath|pager|sshcommand|editor|askpass|gitproxy)$|sequence\.editor$|diff\.|difftool\.|merge\.|mergetool\.|filter\.|credential\.|gpg\.|pager\.|include|uploadpack\.|ssh\.|protocol\.|remote\.|url\.)`)

// gitCall is a git command line split into global options, subcommand and
// subcommand arguments.
type gitCall struct {
	// cmd is a copy of the command whose Dir follows -C.
	cmd  *parser.Command
	sub  string
	args []parser.Word
	// risky explains why the global options make the call unpredictable.
	risky         string
	gitDirSet     bool
	workTreeSet   bool
	workTree      string
	workTreeKnown bool
}

func splitGit(c *parser.Command) gitCall {
	cc := *c
	g := gitCall{cmd: &cc}
	args := c.Args
	for i := 0; i < len(args); i++ {
		a := args[i]
		if a.Dynamic {
			g.risky = "a git option or subcommand is only known at run time"
			return g
		}
		if !strings.HasPrefix(a.Value, "-") {
			g.sub = a.Value
			g.args = args[i+1:]
			return g
		}
		name, val, hasVal := strings.Cut(a.Value, "=")
		var w parser.Word
		switch name {
		case "-C", "-c", "--git-dir", "--work-tree", "--namespace", "--config-env", "--super-prefix",
			"--attr-source", "--list-cmds":
			if hasVal {
				w = parser.Word{Value: val}
			} else {
				if i+1 >= len(args) {
					return g
				}
				i++
				w = args[i]
			}
		}
		switch name {
		case "-C":
			switch {
			case w.Dynamic:
				cc.DirKnown = false
			case filepath.IsAbs(w.Value):
				cc.Dir, cc.DirKnown = filepath.Clean(w.Value), true
			case cc.DirKnown:
				cc.Dir = paths.Abs(cc.Dir, w.Value)
			}
		case "-c", "--config-env":
			key, _, _ := strings.Cut(w.Value, "=")
			if w.Dynamic || gitExecConfig.MatchString(key) {
				g.risky = "git configuration set on the command line (" + key + ") can run other programs"
			}
		case "--exec-path":
			if hasVal {
				g.risky = "--exec-path makes git run its helpers from another directory"
			}
		case "--git-dir":
			g.gitDirSet = true
		case "--work-tree":
			g.workTreeSet = true
			switch {
			case w.Dynamic:
				g.workTreeKnown = false
			case filepath.IsAbs(w.Value):
				g.workTree, g.workTreeKnown = filepath.Clean(w.Value), true
			case cc.DirKnown:
				g.workTree, g.workTreeKnown = paths.Abs(cc.Dir, w.Value), true
			}
		}
	}
	return g
}

// root is the top of the work tree the call acts on.
func (g gitCall) root() (string, bool) {
	if g.workTreeSet {
		return g.workTree, g.workTreeKnown
	}
	if !g.cmd.DirKnown {
		return "", false
	}
	if g.gitDirSet {
		return g.cmd.Dir, true
	}
	if r := paths.GitRoot(g.cmd.Dir); r != "" {
		return r, true
	}
	return g.cmd.Dir, true
}

func (g gitCall) rootTarget(scope GitScope) Target {
	root, ok := g.root()
	if !ok {
		t := unresolved(".", "the repository location is not known")
		t.Recursive, t.Git = true, scope
		return t
	}
	return Target{Path: root, Raw: root, Recursive: true, Git: scope, Repo: root}
}

func (g gitCall) cwdTarget(scope GitScope) Target {
	if !g.cmd.DirKnown {
		t := unresolved(".", "relative to a directory that is not known")
		t.Recursive, t.Git = true, scope
		return t
	}
	repo, _ := g.root()
	return Target{Path: g.cmd.Dir, Raw: ".", Recursive: true, Git: scope, Repo: repo}
}

// pathspecBase cuts a pathspec back to the directory before its first
// wildcard, since git expands wildcards itself.
func pathspecBase(v string) string {
	i := strings.IndexAny(v, "*?[")
	if i < 0 {
		return v
	}
	v = v[:strings.LastIndex(v[:i], "/")+1]
	if v == "" {
		return "."
	}
	return v
}

func (g gitCall) pathspecs(ws []parser.Word, scope GitScope) []Target {
	repo, known := g.root()
	var ts []Target
	for _, w := range ws {
		if !w.Dynamic && strings.HasPrefix(w.Value, ":") {
			// Pathspec magic such as :/ can reach the whole repository.
			ts = append(ts, g.rootTarget(scope))
			continue
		}
		if !w.Dynamic {
			w.Value = pathspecBase(w.Value)
		}
		t := target(g.cmd, w)
		t.Recursive = true
		t.Git = scope
		if known {
			t.Repo = repo
		}
		ts = append(ts, t)
	}
	return ts
}

func (g gitCall) exists(v string) bool {
	if !g.cmd.DirKnown && !filepath.IsAbs(v) {
		return false
	}
	_, err := os.Lstat(paths.Abs(g.cmd.Dir, v))
	return err == nil
}

func splitDashDash(args []parser.Word) ([]parser.Word, []parser.Word, bool) {
	for i, a := range args {
		if !a.Dynamic && a.Value == "--" {
			return args[:i], args[i+1:], true
		}
	}
	return args, nil, false
}

type gitHandler func(g gitCall) []Finding

var gitSubs map[string]gitHandler

func init() {
	gitSubs = map[string]gitHandler{}
	gitSubs["reset"] = gitReset
	gitSubs["clean"] = gitClean
	gitSubs["checkout"] = gitCheckout
	gitSubs["restore"] = gitRestore
	gitSubs["switch"] = gitSwitch
	gitSubs["rm"] = gitRm
	gitSubs["mv"] = gitMv
	gitSubs["stash"] = gitStash
	gitSubs["branch"] = gitBranch
	gitSubs["push"] = gitPush
	gitSubs["reflog"] = gitReflog
	gitSubs["gc"] = gitGc
	gitSubs["prune"] = gitExternal("git-prune", "unreachable git objects", "deletes unreachable objects, which may hold lost work")
	gitSubs["filter-branch"] = gitExternal("git-filter-branch", "git history", "rewrites the repository history")
	gitSubs["filter-repo"] = gitExternal("git-filter-repo", "git history", "rewrites the repository history")
	gitSubs["update-ref"] = gitUpdateRef
	gitSubs["worktree"] = gitWorktree
	gitSubs["checkout-index"] = gitCheckoutIndex
	gitSubs["read-tree"] = gitReadTree
}

func git(c *parser.Command) []Finding {
	g := splitGit(c)
	var out []Finding
	if g.risky != "" {
		out = append(out, Finding{Op: OpModify, Rule: "git-config", What: g.risky, Targets: []Target{unresolved("", g.risky)}})
	}
	if h, ok := gitSubs[g.sub]; ok {
		out = append(out, h(g)...)
	}
	for i := range out {
		out[i].Command = c
	}
	return out
}

func gitExternal(rule, resource, what string) gitHandler {
	return func(g gitCall) []Finding {
		o := parseOpts(g.args, "")
		if o.has("n", "dry-run", "h", "help") {
			return nil
		}
		return []Finding{{Op: OpExternal, Rule: rule, What: what, Resource: resource}}
	}
}

func gitReset(g gitCall) []Finding {
	o := parseOpts(g.args, "", "pathspec-from-file")
	if !o.has("hard") {
		// Soft, mixed, keep and merge resets leave local changes alone.
		return nil
	}
	t := g.rootTarget(GitTracked)
	return []Finding{{Op: OpOverwrite, Rule: "git-reset-hard", What: "throws away uncommitted changes to tracked files", Targets: []Target{t}}}
}

func gitClean(g gitCall) []Finding {
	before, after, _ := splitDashDash(g.args)
	o := parseOpts(before, "e", "exclude")
	if o.has("n", "dry-run", "h", "help") {
		return nil
	}
	scope, what := GitUntracked, "deletes untracked files"
	if o.has("x") {
		scope, what = GitUntrackedIgnored, "deletes untracked files, ignored ones included"
	}
	if o.has("X") {
		scope, what = GitIgnored, "deletes ignored files"
	}
	specs := append(append([]parser.Word{}, o.operands...), after...)
	ts := []Target{g.cwdTarget(scope)}
	if len(specs) > 0 {
		ts = g.pathspecs(specs, scope)
	}
	return []Finding{{Op: OpDelete, Rule: "git-clean", What: what, Targets: ts}}
}

func gitCheckout(g gitCall) []Finding {
	before, after, dashed := splitDashDash(g.args)
	o := parseOpts(before, "bB", "orphan", "conflict", "pathspec-from-file")
	if o.has("h", "help") {
		return nil
	}
	var specs []parser.Word
	if dashed {
		specs = after
	} else if !o.has("b", "B", "orphan") {
		// Without --, an operand that names something on disk is a path,
		// not a branch.
		for _, w := range o.operands {
			if w.Dynamic || w.Value == "." || strings.ContainsAny(w.Value, "*?[") || g.exists(w.Value) {
				specs = append(specs, w)
			}
		}
	}
	var ts []Target
	if o.has("f", "force") {
		ts = append(ts, g.rootTarget(GitTracked))
	}
	ts = append(ts, g.pathspecs(specs, GitTracked)...)
	if v, ok := o.word("pathspec-from-file"); ok {
		ts = append(ts, unresolved(v.Value, "paths are read from a file"))
	}
	if len(ts) == 0 {
		return nil
	}
	return []Finding{{Op: OpOverwrite, Rule: "git-checkout", What: "replaces local changes with the version git has", Targets: ts}}
}

func gitRestore(g gitCall) []Finding {
	before, after, _ := splitDashDash(g.args)
	o := parseOpts(before, "s", "source", "conflict", "pathspec-from-file")
	if o.has("h", "help") || (o.has("S", "staged") && !o.has("W", "worktree")) {
		return nil
	}
	specs := append(append([]parser.Word{}, o.operands...), after...)
	ts := g.pathspecs(specs, GitTracked)
	if v, ok := o.word("pathspec-from-file"); ok {
		ts = append(ts, unresolved(v.Value, "paths are read from a file"))
	}
	if len(ts) == 0 {
		return nil
	}
	return []Finding{{Op: OpOverwrite, Rule: "git-restore", What: "replaces local changes with the version git has", Targets: ts}}
}

func gitSwitch(g gitCall) []Finding {
	o := parseOpts(g.args, "cC", "orphan", "conflict")
	if !o.has("f", "force", "discard-changes") {
		return nil
	}
	t := g.rootTarget(GitTracked)
	return []Finding{{Op: OpOverwrite, Rule: "git-switch-force", What: "throws away local changes while switching branch", Targets: []Target{t}}}
}

func gitRm(g gitCall) []Finding {
	before, after, _ := splitDashDash(g.args)
	o := parseOpts(before, "", "pathspec-from-file")
	if o.has("cached", "n", "dry-run", "h", "help") {
		return nil
	}
	specs := append(append([]parser.Word{}, o.operands...), after...)
	ts := g.pathspecs(specs, GitTracked)
	if v, ok := o.word("pathspec-from-file"); ok {
		ts = append(ts, unresolved(v.Value, "paths are read from a file"))
	}
	if len(ts) == 0 {
		return nil
	}
	return []Finding{{Op: OpDelete, Rule: "git-rm", What: "deletes tracked files from the work tree", Targets: ts}}
}

func gitMv(g gitCall) []Finding {
	o := parseOpts(g.args, "")
	if o.has("n", "dry-run", "h", "help") {
		return nil
	}
	srcs, dests := landing(g.cmd, o, true)
	if len(srcs) == 0 {
		return nil
	}
	out := []Finding{pathFinding(g.cmd, srcs, OpDelete, "git-mv", "moves away, so the old path stops existing", true)}
	if o.has("f", "force") {
		out = append(out, Finding{Op: OpOverwrite, Rule: "git-mv", What: "replaces what is at the destination", Targets: dests})
	}
	return out
}

func gitStash(g gitCall) []Finding {
	if len(g.args) == 0 {
		return nil
	}
	switch g.args[0].Value {
	case "drop", "clear":
		return []Finding{{Op: OpExternal, Rule: "git-stash-drop", What: "drops stashed changes", Resource: "git stash"}}
	}
	return nil
}

func gitBranch(g gitCall) []Finding {
	o := parseOpts(g.args, "", "set-upstream-to", "contains", "no-contains", "merged", "no-merged", "points-at", "sort", "format", "color")
	switch {
	case o.has("D") || (o.has("d", "delete") && o.has("f", "force")):
		return []Finding{{Op: OpExternal, Rule: "git-branch-delete", What: "deletes a branch even if it is not merged", Resource: "git branch"}}
	case o.has("M", "C"):
		return []Finding{{Op: OpExternal, Rule: "git-branch-force", What: "renames or copies a branch over an existing one", Resource: "git branch"}}
	}
	return nil
}

func gitPush(g gitCall) []Finding {
	o := parseOpts(g.args, "o", "push-option", "repo", "receive-pack", "exec", "recurse-submodules")
	if o.has("n", "dry-run", "h", "help") {
		return nil
	}
	forced := o.has("f", "force", "force-with-lease", "d", "delete", "mirror", "prune")
	for i, w := range o.operands {
		if i > 0 && (strings.HasPrefix(w.Value, "+") || strings.HasPrefix(w.Value, ":")) {
			forced = true
		}
	}
	if !forced {
		return nil
	}
	resource := "remote repository"
	if len(o.operands) > 0 {
		resource += " " + o.operands[0].Value
	}
	return []Finding{{Op: OpExternal, Rule: "git-push-force", What: "overwrites or deletes history on a remote", Resource: resource}}
}

func gitReflog(g gitCall) []Finding {
	if len(g.args) > 0 && (g.args[0].Value == "expire" || g.args[0].Value == "delete") {
		return []Finding{{Op: OpExternal, Rule: "git-reflog", What: "drops reflog entries, which are the way back to lost commits", Resource: "git reflog"}}
	}
	return nil
}

func gitGc(g gitCall) []Finding {
	o := parseOpts(g.args, "")
	if v, ok := o.value("prune"); ok && (v == "now" || v == "all") {
		return []Finding{{Op: OpExternal, Rule: "git-gc-prune", What: "deletes unreachable objects at once, which may hold lost work", Resource: "unreachable git objects"}}
	}
	return nil
}

func gitUpdateRef(g gitCall) []Finding {
	o := parseOpts(g.args, "m", "create-reflog")
	if o.has("d") {
		return []Finding{{Op: OpExternal, Rule: "git-update-ref", What: "deletes a ref", Resource: "git ref"}}
	}
	return nil
}

func gitWorktree(g gitCall) []Finding {
	if len(g.args) == 0 || g.args[0].Value != "remove" {
		return nil
	}
	o := parseOpts(g.args[1:], "")
	if len(o.operands) == 0 {
		return nil
	}
	f := pathFinding(g.cmd, o.operands, OpDelete, "git-worktree-remove", "deletes a linked work tree", true)
	return []Finding{f}
}

func gitCheckoutIndex(g gitCall) []Finding {
	o := parseOpts(g.args, "", "prefix", "stage")
	if !o.has("f", "force") {
		return nil
	}
	ts := g.pathspecs(o.operands, GitTracked)
	if len(ts) == 0 {
		ts = []Target{g.rootTarget(GitTracked)}
	}
	return []Finding{{Op: OpOverwrite, Rule: "git-checkout-index", What: "overwrites work tree files from the index", Targets: ts}}
}

func gitReadTree(g gitCall) []Finding {
	o := parseOpts(g.args, "", "prefix", "exclude-per-directory", "index-output")
	if !o.has("u") || !o.has("reset") {
		return nil
	}
	t := g.rootTarget(GitTracked)
	return []Finding{{Op: OpOverwrite, Rule: "git-read-tree", What: "overwrites work tree files from a tree", Targets: []Target{t}}}
}
