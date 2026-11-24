//go:build unix

package snapshot

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/celaldrg/undoguard/internal/policy"
)

// world is a throwaway work tree with a data directory beside it.
type world struct {
	base, work, data string
}

func newWorld(t *testing.T) *world {
	t.Helper()
	base := t.TempDir()
	if r, err := filepath.EvalSymlinks(base); err == nil {
		base = r
	}
	w := &world{
		base: base,
		work: filepath.Join(base, "work"),
		data: filepath.Join(base, "data"),
	}
	t.Setenv("HOME", filepath.Join(base, "home"))
	t.Setenv("UNDOGUARD_HOME", w.data)
	t.Setenv("UNDOGUARD_CONFIG_DIR", filepath.Join(base, "conf"))
	for _, k := range []string{"UNDOGUARD_PROJECT", "CLAUDE_PROJECT_DIR", "GEMINI_PROJECT_DIR", "XDG_DATA_HOME", "XDG_CONFIG_HOME"} {
		t.Setenv(k, "")
	}
	must(t, os.MkdirAll(w.work, 0o755))
	return w
}

func must(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}

// path returns name, written with slashes, below the work tree.
func (w *world) path(name string) string {
	return filepath.Join(w.work, filepath.FromSlash(name))
}

// writeFile creates or replaces name with content and mode, and sets its
// modification time unless mtime is zero.
func (w *world) writeFile(t *testing.T, name, content string, mode os.FileMode, mtime time.Time) string {
	t.Helper()
	p := w.path(name)
	must(t, os.MkdirAll(filepath.Dir(p), 0o755))
	must(t, os.WriteFile(p, []byte(content), 0o600))
	must(t, os.Chmod(p, mode))
	if !mtime.IsZero() {
		must(t, os.Chtimes(p, mtime, mtime))
	}
	return p
}

func (w *world) takeSnapshot(t *testing.T, roots []Root, o Options) *Manifest {
	t.Helper()
	o.Root = w.data
	m, err := Take(roots, o)
	must(t, err)
	return m
}

func (w *world) finishSnapshot(t *testing.T, id string) *Manifest {
	t.Helper()
	m, err := Finish(w.data, id)
	must(t, err)
	return m
}

// snapFile writes name and takes a snapshot of that file alone.
func (w *world) snapFile(t *testing.T, name, content string, o Options) *Manifest {
	t.Helper()
	p := w.writeFile(t, name, content, 0o644, time.Time{})
	return w.takeSnapshot(t, []Root{{Path: p}}, o)
}

func readFile(t *testing.T, p string) string {
	t.Helper()
	data, err := os.ReadFile(p)
	must(t, err)
	return string(data)
}

// checkFile fails the test unless p is a regular file with content and
// mode and, when mtime is not zero, that modification time.
func checkFile(t *testing.T, p, content string, mode os.FileMode, mtime time.Time) {
	t.Helper()
	info, err := os.Lstat(p)
	if err != nil {
		t.Error(err)
		return
	}
	if !info.Mode().IsRegular() {
		t.Errorf("%s is %v, want a regular file", p, info.Mode())
		return
	}
	if got := readFile(t, p); got != content {
		t.Errorf("%s holds %q, want %q", p, got, content)
	}
	if got := info.Mode().Perm(); got != mode {
		t.Errorf("%s has mode %v, want %v", p, got, mode)
	}
	if !mtime.IsZero() && !info.ModTime().Equal(mtime) {
		t.Errorf("%s was modified at %v, want %v", p, info.ModTime(), mtime)
	}
}

func checkAbsent(t *testing.T, p string) {
	t.Helper()
	if _, err := os.Lstat(p); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("%s: got %v, want it gone", p, err)
	}
}

// rewrite replaces the content of p with a shorter text.
func rewrite(p string) error {
	return os.WriteFile(p, []byte("rewritten\n"), 0o600)
}

// makePrivate takes the access of group and others to p away.
func makePrivate(p string) error {
	return os.Chmod(p, 0o600)
}

// replaceWithDir puts an empty directory where the file p was.
func replaceWithDir(p string) error {
	if err := os.Remove(p); err != nil {
		return err
	}
	return os.Mkdir(p, 0o755)
}

func TestRestoreFile(t *testing.T) {
	mtime := time.Date(2020, 1, 2, 3, 4, 5, 0, time.UTC)
	const content = "original content\n"
	cases := []struct {
		name    string
		command func(string) error
		kind    ChangeKind
	}{
		{"removed", os.Remove, ChangeRestore},
		{"rewritten", rewrite, ChangeRestore},
		{"mode changed", makePrivate, ChangeMetadata},
		{"replaced by a directory", replaceWithDir, ChangeRestore},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			w := newWorld(t)
			p := w.writeFile(t, "a.txt", content, 0o640, mtime)
			m := w.takeSnapshot(t, []Root{{Path: p}}, Options{Limits: policy.DefaultLimits(), Session: "s", Command: "edit a.txt"})
			if len(m.Entries) != 1 || m.Entries[0].Path != p || m.Entries[0].Before.Hash == "" {
				t.Fatalf("entries = %+v", m.Entries)
			}
			if m.Status() != "not confirmed" {
				t.Errorf("status before finish = %q", m.Status())
			}

			must(t, tc.command(p))
			m = w.finishSnapshot(t, m.ID)
			if m.Status() != "ready" || m.Entries[0].After == nil {
				t.Errorf("status after finish = %q, after = %+v", m.Status(), m.Entries[0].After)
			}

			res, err := Restore(m, RestoreOptions{Root: w.data, Limits: policy.DefaultLimits(), Session: "s"})
			must(t, err)
			if len(res.Plan.Changes) != 1 || res.Plan.Changes[0].Kind != tc.kind {
				t.Errorf("plan = %+v, want one %s", res.Plan.Changes, tc.kind)
			}
			if res.Done != 1 || len(res.Failed) != 0 {
				t.Errorf("done %d, failed %v", res.Done, res.Failed)
			}
			checkFile(t, p, content, 0o640, mtime)

			undone, err := Load(w.data, m.ID)
			must(t, err)
			if undone.Status() != "undone" || res.Safety == "" || undone.RestoredBy != res.Safety {
				t.Errorf("status %q, restored by %q, safety %q", undone.Status(), undone.RestoredBy, res.Safety)
			}
			safety, err := Load(w.data, res.Safety)
			must(t, err)
			if safety.Undoes != m.ID || safety.Status() != "taken by undo" || safety.Finished == nil {
				t.Errorf("safety snapshot = %+v", safety)
			}
			if _, err := Latest(w.data, ""); err == nil {
				t.Error("Latest found something to undo after the only snapshot was undone")
			}
		})
	}
}

func TestRestoreTree(t *testing.T) {
	w := newWorld(t)
	old := time.Date(2021, 6, 7, 8, 9, 10, 0, time.UTC)
	files := []struct {
		name    string
		content string
		mode    os.FileMode
		mtime   time.Time
	}{
		{"tree/top.txt", "top\n", 0o640, old},
		{"tree/empty.txt", "", 0o600, old.Add(time.Hour)},
		{"tree/bin/run.sh", "#!/bin/sh\necho hi\n", 0o755, old.Add(2 * time.Hour)},
		{"tree/sub/deep/nested.txt", "nested\n", 0o644, old.Add(3 * time.Hour)},
	}
	var total int64
	for _, f := range files {
		w.writeFile(t, f.name, f.content, f.mode, f.mtime)
		total += int64(len(f.content))
	}
	link := w.path("tree/link")
	must(t, os.Symlink("top.txt", link))
	// Directory times are set last, since adding entries changes them.
	dirs := []struct {
		name  string
		mode  os.FileMode
		mtime time.Time
	}{
		{"tree/sub/deep", 0o700, old.Add(10 * time.Hour)},
		{"tree/sub", 0o755, old.Add(11 * time.Hour)},
		{"tree/bin", 0o750, old.Add(12 * time.Hour)},
		{"tree", 0o755, old.Add(13 * time.Hour)},
	}
	for _, d := range dirs {
		p := w.path(d.name)
		must(t, os.Chmod(p, d.mode))
		must(t, os.Chtimes(p, d.mtime, d.mtime))
	}

	root := w.path("tree")
	m := w.takeSnapshot(t, []Root{{Path: root, Recursive: true}}, Options{Limits: policy.DefaultLimits(), Command: "rm -rf tree"})
	if len(m.Entries) != 9 || m.Files() != 4 || m.Bytes != total {
		t.Fatalf("snapshot has %d entries, %d files and %d bytes, want 9, 4 and %d", len(m.Entries), m.Files(), m.Bytes, total)
	}
	for _, e := range m.Entries {
		if e.Path == link && (e.Before.Kind != KindSymlink || e.Before.Link != "top.txt") {
			t.Errorf("link recorded as %+v", e.Before)
		}
	}

	must(t, os.RemoveAll(root))
	m = w.finishSnapshot(t, m.ID)
	res, err := Restore(m, RestoreOptions{Root: w.data, Limits: policy.DefaultLimits()})
	must(t, err)
	if res.Done != 9 || len(res.Failed) != 0 {
		t.Errorf("done %d, failed %v", res.Done, res.Failed)
	}

	for _, f := range files {
		checkFile(t, w.path(f.name), f.content, f.mode, f.mtime)
	}
	// Symlinks keep the mode and times they get on creation.
	if target, err := os.Readlink(link); err != nil || target != "top.txt" {
		t.Errorf("link points to %q (%v), want top.txt", target, err)
	}
	for _, d := range dirs {
		info, err := os.Lstat(w.path(d.name))
		if err != nil {
			t.Error(err)
			continue
		}
		if !info.IsDir() || info.Mode().Perm() != d.mode || !info.ModTime().Equal(d.mtime) {
			t.Errorf("%s: mode %v, modified %v, want %v and %v", d.name, info.Mode(), info.ModTime(), d.mode, d.mtime)
		}
	}
	count := 0
	err = filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		count++
		if strings.HasPrefix(d.Name(), ".undoguard-") {
			t.Errorf("temporary file left behind: %s", p)
		}
		return nil
	})
	if err != nil || count != 9 {
		t.Errorf("tree holds %d paths (%v), want 9", count, err)
	}
}

func TestRestoreRemovesWhatTheCommandCreated(t *testing.T) {
	w := newWorld(t)
	keep := w.writeFile(t, "tree/keep.txt", "keep\n", 0o644, time.Time{})
	tree := w.path("tree")
	newFile := w.path("new.txt")
	out := w.path("out")
	roots := []Root{{Path: newFile}, {Path: tree, Recursive: true}, {Path: out, Recursive: true}}
	m := w.takeSnapshot(t, roots, Options{Command: "build"})
	if len(m.Entries) != 4 {
		t.Fatalf("entries = %+v, want new.txt, out, tree and tree/keep.txt", m.Entries)
	}
	for _, e := range m.Entries {
		if (e.Path == newFile || e.Path == out) && e.Before.Kind != KindAbsent {
			t.Errorf("%s recorded as %s, want absent", e.Path, e.Before.Kind)
		}
	}

	for _, name := range []string{"new.txt", "tree/made.txt", "tree/madedir/inner.txt", "out/x.txt"} {
		w.writeFile(t, name, "made\n", 0o644, time.Time{})
	}
	m = w.finishSnapshot(t, m.ID)
	// Finish adds what appeared below the recursive roots.
	if len(m.Entries) != 8 {
		t.Fatalf("entries after finish = %+v, want 8", m.Entries)
	}

	plan := PlanRestore(m)
	if len(plan.Changes) != 6 || plan.Unchanged != 2 || len(plan.Conflicts()) != 0 {
		t.Fatalf("plan = %+v", plan)
	}
	for _, c := range plan.Changes {
		if c.Kind != ChangeRemove {
			t.Errorf("%s: %s, want %s", c.Path, c.Kind, ChangeRemove)
		}
	}

	res, err := Restore(m, RestoreOptions{Root: w.data})
	must(t, err)
	if res.Done != 6 || len(res.Failed) != 0 {
		t.Errorf("done %d, failed %v", res.Done, res.Failed)
	}
	for _, p := range []string{newFile, out, w.path("tree/made.txt"), w.path("tree/madedir")} {
		checkAbsent(t, p)
	}
	checkFile(t, keep, "keep\n", 0o644, time.Time{})
}

func TestRestoreConflict(t *testing.T) {
	w := newWorld(t)
	p := w.writeFile(t, "a.txt", "v1\n", 0o644, time.Time{})
	m := w.takeSnapshot(t, []Root{{Path: p}}, Options{Command: "edit a.txt"})
	must(t, os.WriteFile(p, []byte("v2 by the command\n"), 0o644))
	m = w.finishSnapshot(t, m.ID)
	// A change made after the command finished, of another size than the
	// state Finish recorded.
	const later = "v3, written later on\n"
	must(t, os.WriteFile(p, []byte(later), 0o644))

	res, err := Restore(m, RestoreOptions{Root: w.data})
	var ce *ConflictError
	if !errors.As(err, &ce) {
		t.Fatalf("err = %v, want a ConflictError", err)
	}
	if len(ce.Changes) != 1 || ce.Changes[0].Path != p || ce.Changes[0].Conflict == "" {
		t.Errorf("conflicts = %+v", ce.Changes)
	}
	if res == nil || res.Done != 0 || res.Safety != "" {
		t.Errorf("result = %+v, want nothing done", res)
	}
	if got := readFile(t, p); got != later {
		t.Errorf("a refused undo changed the file to %q", got)
	}
	if ids, err := IDs(w.data); err != nil || len(ids) != 1 {
		t.Errorf("snapshots = %v (%v), want only the original", ids, err)
	}

	res, err = Restore(m, RestoreOptions{Root: w.data, Force: true})
	must(t, err)
	if res.Done != 1 || res.Safety == "" {
		t.Errorf("forced undo: done %d, safety %q", res.Done, res.Safety)
	}
	if got := readFile(t, p); got != "v1\n" {
		t.Errorf("after a forced undo the file holds %q, want v1", got)
	}

	// The undo saved what it replaced, so it can be undone in turn.
	safety, err := Load(w.data, res.Safety)
	must(t, err)
	if safety.Undoes != m.ID || safety.Finished == nil {
		t.Fatalf("safety snapshot = %+v", safety)
	}
	if _, err := Restore(safety, RestoreOptions{Root: w.data}); err != nil {
		t.Fatal(err)
	}
	if got := readFile(t, p); got != later {
		t.Errorf("undoing the undo left %q, want %q", got, later)
	}
	orig, err := Load(w.data, m.ID)
	must(t, err)
	if orig.Restored != nil || orig.RestoredBy != "" {
		t.Errorf("original still marked undone: %v by %q", orig.Restored, orig.RestoredBy)
	}
	if got, err := Latest(w.data, ""); err != nil || got.ID != m.ID {
		t.Errorf("Latest = %v (%v), want %s", got, err, m.ID)
	}
}

func TestRestoreUnfinished(t *testing.T) {
	const content = "before the command\n"
	cases := []struct {
		name     string
		command  func(string) error
		conflict bool
	}{
		// A path that is gone cannot hold a later change.
		{"removed", os.Remove, false},
		// Without the state after the command, any change may be a later one.
		{"rewritten", rewrite, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			w := newWorld(t)
			p := w.writeFile(t, "a.txt", content, 0o644, time.Time{})
			m := w.takeSnapshot(t, []Root{{Path: p}}, Options{Command: "edit a.txt"})
			must(t, tc.command(p))

			_, err := Restore(m, RestoreOptions{Root: w.data})
			var ce *ConflictError
			if got := errors.As(err, &ce); got != tc.conflict {
				t.Fatalf("err = %v, want a conflict: %v", err, tc.conflict)
			}
			if tc.conflict {
				if got := readFile(t, p); got != "rewritten\n" {
					t.Errorf("a refused undo changed the file to %q", got)
				}
				return
			}
			must(t, err)
			checkFile(t, p, content, 0o644, time.Time{})
		})
	}
}

func TestRestoreDryRun(t *testing.T) {
	w := newWorld(t)
	p := w.writeFile(t, "a.txt", "keep me\n", 0o644, time.Time{})
	m := w.takeSnapshot(t, []Root{{Path: p}}, Options{Command: "rm a.txt"})
	must(t, os.Remove(p))
	m = w.finishSnapshot(t, m.ID)

	res, err := Restore(m, RestoreOptions{Root: w.data, DryRun: true})
	must(t, err)
	if len(res.Plan.Changes) != 1 || res.Plan.Changes[0].Kind != ChangeRestore {
		t.Errorf("plan = %+v, want one restore", res.Plan.Changes)
	}
	if res.Done != 0 || res.Safety != "" {
		t.Errorf("dry run: done %d, safety %q", res.Done, res.Safety)
	}
	checkAbsent(t, p)
	if ids, err := IDs(w.data); err != nil || len(ids) != 1 {
		t.Errorf("snapshots = %v (%v), want only the original", ids, err)
	}
}

func TestTakeLimits(t *testing.T) {
	cases := []struct {
		name   string
		limits policy.Limits
		what   string
		size   int64
		limit  int64
	}{
		{"file over", policy.Limits{MaxFileSize: 5}, "file", 6, 5},
		{"file at", policy.Limits{MaxFileSize: 6}, "", 0, 0},
		{"total over", policy.Limits{MaxTotalSize: 11}, "total", 12, 11},
		{"total at", policy.Limits{MaxTotalSize: 12}, "", 0, 0},
		{"paths over", policy.Limits{MaxFiles: 2}, "paths", 0, 2},
		{"paths at", policy.Limits{MaxFiles: 3}, "", 0, 0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			w := newWorld(t)
			// Three paths: the directory and two files of 6 bytes.
			first := w.writeFile(t, "d/a.txt", "aaaaa\n", 0o644, time.Time{})
			w.writeFile(t, "d/b.txt", "bbbbb\n", 0o644, time.Time{})
			roots := []Root{{Path: w.path("d"), Recursive: true}}
			m, err := Take(roots, Options{Root: w.data, Limits: tc.limits, Command: "rm -r d"})
			if tc.what == "" {
				must(t, err)
				if len(m.Entries) != 3 {
					t.Errorf("entries = %+v, want 3", m.Entries)
				}
				return
			}
			var tl *TooLargeError
			if !errors.As(err, &tl) {
				t.Fatalf("err = %v, want a TooLargeError", err)
			}
			if m != nil {
				t.Errorf("got a snapshot %s along with the error", m.ID)
			}
			if tl.What != tc.what || tl.Size != tc.size || tl.Limit != tc.limit {
				t.Errorf("error = %+v, want %s, size %d, limit %d", tl, tc.what, tc.size, tc.limit)
			}
			if tc.what == "file" && tl.Path != first {
				t.Errorf("error names %s, want %s", tl.Path, first)
			}
			if tl.Error() == "" {
				t.Error("empty error message")
			}
			if ids, err := IDs(w.data); err != nil || len(ids) != 0 {
				t.Errorf("snapshots = %v (%v), want none", ids, err)
			}
		})
	}
}

func TestTakeNothing(t *testing.T) {
	w := newWorld(t)
	if m, err := Take(nil, Options{Root: w.data}); err == nil {
		t.Errorf("Take without roots = %+v, want an error", m)
	}
}

func TestTakeIncomplete(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root can read any directory")
	}
	w := newWorld(t)
	w.writeFile(t, "d/ok.txt", "ok\n", 0o644, time.Time{})
	w.writeFile(t, "d/locked/inner.txt", "inner\n", 0o644, time.Time{})
	locked := w.path("d/locked")
	must(t, os.Chmod(locked, 0o000))
	t.Cleanup(func() { _ = os.Chmod(locked, 0o755) })

	m, err := Take([]Root{{Path: w.path("d"), Recursive: true}}, Options{Root: w.data, Command: "rm -r d"})
	var ie *IncompleteError
	if !errors.As(err, &ie) {
		t.Fatalf("err = %v, want an IncompleteError", err)
	}
	if m == nil || ie.ID != m.ID {
		t.Fatalf("snapshot %+v, error names %s", m, ie.ID)
	}
	if len(ie.Skipped) != 1 || !strings.Contains(ie.Skipped[0], locked) {
		t.Errorf("skipped = %q, want only %s", ie.Skipped, locked)
	}
	// The unreadable directory is recorded, its content is not.
	if len(m.Entries) != 3 {
		t.Errorf("entries = %+v, want d, d/locked and d/ok.txt", m.Entries)
	}
	if _, err := Load(w.data, m.ID); err != nil {
		t.Errorf("the snapshot was not saved: %v", err)
	}
}

func TestSame(t *testing.T) {
	file := State{Kind: KindFile, Mode: 0o644, Size: 3, MTime: 100, UID: 1, GID: 1}
	with := func(change func(*State)) State {
		s := file
		change(&s)
		return s
	}
	dir := State{Kind: KindDir, Mode: 0o755, MTime: 1, UID: -1, GID: -1}
	link := State{Kind: KindSymlink, Mode: 0o777, Link: "a", UID: -1, GID: -1}
	cases := []struct {
		name string
		a, b State
		want bool
	}{
		{"identical files", file, file, true},
		{"newer file", file, with(func(s *State) { s.MTime++ }), false},
		{"resized file", file, with(func(s *State) { s.Size = 4 }), false},
		{"other mode", file, with(func(s *State) { s.Mode = 0o600 }), false},
		{"other owner", file, with(func(s *State) { s.UID = 2 }), false},
		{"unknown owner", file, with(func(s *State) { s.UID, s.GID = -1, -1 }), true},
		{"file and directory", file, dir, false},
		{"directory times", dir, State{Kind: KindDir, Mode: 0o755, MTime: 2, UID: -1, GID: -1}, true},
		{"directory mode", dir, State{Kind: KindDir, Mode: 0o700, MTime: 1, UID: -1, GID: -1}, false},
		{"symlink mode", link, State{Kind: KindSymlink, Mode: 0o755, Link: "a", UID: -1, GID: -1}, true},
		{"symlink target", link, State{Kind: KindSymlink, Mode: 0o777, Link: "b", UID: -1, GID: -1}, false},
		{"absent", State{Kind: KindAbsent, UID: -1, GID: -1}, State{Kind: KindAbsent}, true},
	}
	for _, c := range cases {
		if got := same(c.a, c.b); got != c.want {
			t.Errorf("%s: same(a, b) = %v, want %v", c.name, got, c.want)
		}
		if got := same(c.b, c.a); got != c.want {
			t.Errorf("%s: same(b, a) = %v, want %v", c.name, got, c.want)
		}
	}
}
