//go:build unix

package cli

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"testing"
)

func TestExecExitCodes(t *testing.T) {
	newWorld(t)
	runCLI(t, "", "exec", "--", "true").wantCode(t, exitOK)
	runCLI(t, "", "exec", "--", "sh", "-c", "exit 7").wantCode(t, 7)
}

func TestExecBlocks(t *testing.T) {
	w := newWorld(t)
	for _, c := range []struct {
		args   []string
		stderr string
	}{
		{[]string{"--", "rm", "-rf", filepath.Join(w.home, "important")}, "undoguard: blocked: "},
		{[]string{"--", "git", "push", "--force", "origin", "main"}, "undoguard: blocked, needs confirmation: "},
		{[]string{"--shell", "/bin/sh", "--", "-c", "rm -rf ~/important"}, "undoguard: blocked: "},
		// One argument that is a whole command line is run with sh -c.
		{[]string{"--", "rm -rf ~/important"}, "undoguard: blocked: "},
		{[]string{"cd .. && rm -rf important"}, "undoguard: blocked: "},
	} {
		r := runCLI(t, "", append([]string{"exec"}, c.args...)...)
		r.wantCode(t, exitBlocked)
		wantContains(t, "stderr", r.stderr, c.stderr)
	}
	w.wantThesis(t)
	if list := snapshots(t); len(list) != 0 {
		t.Errorf("exec took %d snapshots for blocked commands", len(list))
	}
}

func TestExecUsage(t *testing.T) {
	newWorld(t)
	r := runCLI(t, "", "exec")
	r.wantCode(t, exitError)
	wantContains(t, "stderr", r.stderr, "Usage: undoguard exec")
}

func TestExecSnapshotsAndUndo(t *testing.T) {
	w := newWorld(t)
	notes := filepath.Join(w.proj, "notes.md")
	writeFile(t, notes, "keep me\n")
	runCLI(t, "", "exec", "--shell", "/bin/sh", "--", "-c", "rm notes.md").mustCode(t, exitOK)
	if _, err := os.Stat(notes); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("the command did not run: %v", err)
	}
	list := snapshots(t)
	if len(list) != 1 || list[0].Status != "ready" || list[0].Command != "rm notes.md" {
		t.Fatalf("snapshots: %+v", list)
	}
	runCLI(t, "", "undo").mustCode(t, exitOK)
	if got := readFile(t, notes); got != "keep me\n" {
		t.Errorf("undo restored %q", got)
	}
}

func TestRun(t *testing.T) {
	newWorld(t)
	r := runCLI(t, "", "run", "--", "true")
	r.wantCode(t, exitOK)
	wantContains(t, "stderr", r.stderr, "undoguard: guarding the shell commands of true, session run-")
	r = runCLI(t, "", "run")
	r.wantCode(t, exitError)
	wantContains(t, "stderr", r.stderr, "Usage: undoguard run")
}
