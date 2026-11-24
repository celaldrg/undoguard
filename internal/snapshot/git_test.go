//go:build unix

package snapshot

import (
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"testing"
	"time"

	"github.com/celaldrg/undoguard/internal/detect"
)

func entryPaths(m *Manifest) []string {
	var out []string
	for _, e := range m.Entries {
		out = append(out, e.Path)
	}
	return out
}

func TestTakeGit(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git is not installed")
	}
	w := newWorld(t)
	global := filepath.Join(w.base, "gitconfig")
	must(t, os.WriteFile(global, nil, 0o600))
	t.Setenv("GIT_CONFIG_NOSYSTEM", "1")
	t.Setenv("GIT_CONFIG_GLOBAL", global)
	repo := w.path("repo")
	must(t, os.MkdirAll(repo, 0o755))
	git := func(args ...string) {
		t.Helper()
		full := append([]string{"-c", "user.name=test", "-c", "user.email=test@example.invalid", "-c", "init.defaultBranch=main", "-C", repo}, args...)
		if out, err := exec.Command("git", full...).CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	git("init", "-q")
	w.writeFile(t, "repo/a.txt", "one\n", 0o644, time.Time{})
	w.writeFile(t, "repo/same.txt", "same\n", 0o644, time.Time{})
	git("add", "a.txt", "same.txt")
	git("commit", "-q", "-m", "first")
	// A local change to a tracked file, and a file git does not track.
	a := w.writeFile(t, "repo/a.txt", "two two\n", 0o644, time.Time{})
	u := w.writeFile(t, "repo/u.txt", "untracked\n", 0o644, time.Time{})

	// Each scope takes only its files, plus the directories above them.
	untracked := w.takeSnapshot(t, []Root{{Path: repo, Recursive: true, Git: string(detect.GitUntracked), Repo: repo}}, Options{Command: "git clean -f"})
	if got, want := entryPaths(untracked), []string{repo, u}; !slices.Equal(got, want) {
		t.Errorf("untracked snapshot holds %q, want %q", got, want)
	}
	m := w.takeSnapshot(t, []Root{{Path: repo, Recursive: true, Git: string(detect.GitTracked), Repo: repo}}, Options{Command: "git checkout ."})
	if got, want := entryPaths(m), []string{repo, a}; !slices.Equal(got, want) {
		t.Fatalf("tracked snapshot holds %q, want %q", got, want)
	}

	// The command puts the committed content back; undo restores the
	// local change and leaves the untracked file alone.
	w.writeFile(t, "repo/a.txt", "one\n", 0o644, time.Time{})
	m = w.finishSnapshot(t, m.ID)
	if len(m.Entries) != 2 {
		t.Errorf("finish added paths to a git snapshot: %q", entryPaths(m))
	}
	res, err := Restore(m, RestoreOptions{Root: w.data})
	must(t, err)
	if res.Done != 1 || len(res.Failed) != 0 {
		t.Errorf("done %d, failed %v", res.Done, res.Failed)
	}
	checkFile(t, a, "two two\n", 0o644, time.Time{})
	checkFile(t, u, "untracked\n", 0o644, time.Time{})
}
