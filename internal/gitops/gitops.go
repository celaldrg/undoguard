// Package gitops lists the files a destructive git command can change, so
// that only those are snapshotted instead of the whole work tree.
package gitops

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/celaldrg/undoguard/internal/detect"
	"github.com/celaldrg/undoguard/internal/paths"
)

// Timeout bounds each git call.
var Timeout = 30 * time.Second

// Files returns the absolute paths below dir, inside the work tree at
// repo, that a command with the given scope can change. A path ending in
// a separator is a directory to take whole, such as a nested repository.
func Files(repo, dir string, scope detect.GitScope) ([]string, error) {
	// git diff prints paths relative to the top of the work tree whatever
	// the directory, so every query runs from there.
	top, err := run(repo, "rev-parse", "--show-toplevel")
	if err != nil {
		return nil, err
	}
	if len(top) != 1 || strings.TrimSpace(top[0]) == "" {
		return nil, fmt.Errorf("%s is not inside a git work tree", repo)
	}
	repo = strings.TrimSpace(top[0])
	// git prints the physical top directory, so the pathspec has to be
	// physical too, or git calls it outside the repository.
	dir = paths.Resolve(dir, true)
	var lists [][]string
	switch scope {
	case detect.GitTracked:
		// Staged and unstaged changes against HEAD, and unstaged changes
		// against the index for repositories without commits.
		withHead, err := run(repo, "diff", "-z", "--name-only", "--no-renames", "HEAD", "--", dir)
		if err != nil {
			withHead, err = run(repo, "ls-files", "-z", "--", dir)
			if err != nil {
				return nil, err
			}
		}
		index, err := run(repo, "diff", "-z", "--name-only", "--no-renames", "--", dir)
		if err != nil {
			return nil, err
		}
		lists = append(lists, withHead, index)
	case detect.GitUntracked:
		l, err := run(repo, "ls-files", "-z", "--others", "--exclude-standard", "--", dir)
		if err != nil {
			return nil, err
		}
		lists = append(lists, l)
	case detect.GitUntrackedIgnored:
		l, err := run(repo, "ls-files", "-z", "--others", "--", dir)
		if err != nil {
			return nil, err
		}
		lists = append(lists, l)
	case detect.GitIgnored:
		l, err := run(repo, "ls-files", "-z", "--others", "--ignored", "--exclude-standard", "--", dir)
		if err != nil {
			return nil, err
		}
		lists = append(lists, l)
	default:
		return nil, fmt.Errorf("unknown git scope %q", scope)
	}
	seen := map[string]bool{}
	var out []string
	for _, l := range lists {
		for _, rel := range l {
			abs := filepath.Join(repo, filepath.FromSlash(rel))
			if strings.HasSuffix(rel, "/") {
				abs += string(filepath.Separator)
			}
			if !seen[abs] {
				seen[abs] = true
				out = append(out, abs)
			}
		}
	}
	return out, nil
}

// run runs git in repo and splits its NUL-separated output. The caller's
// git environment is dropped, so that variables meant for the agent's
// command do not redirect these queries.
func run(repo string, args ...string) ([]string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), Timeout)
	defer cancel()
	full := append([]string{"--literal-pathspecs", "-c", "core.quotepath=off", "-C", repo}, args...)
	cmd := exec.CommandContext(ctx, "git", full...)
	cmd.Env = cleanEnv()
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	if err := cmd.Run(); err != nil {
		msg := strings.TrimSpace(stderr.String())
		if msg == "" {
			msg = err.Error()
		}
		return nil, fmt.Errorf("git %s: %s", args[0], msg)
	}
	var out []string
	for _, f := range bytes.Split(stdout.Bytes(), []byte{0}) {
		if len(f) > 0 {
			out = append(out, string(f))
		}
	}
	return out, nil
}

func cleanEnv() []string {
	var env []string
	for _, kv := range os.Environ() {
		if strings.HasPrefix(kv, "GIT_") {
			continue
		}
		env = append(env, kv)
	}
	return append(env, "GIT_OPTIONAL_LOCKS=0", "LC_ALL=C")
}
