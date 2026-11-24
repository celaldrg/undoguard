package cli

import (
	"errors"
	"fmt"
	"io"

	"github.com/celaldrg/undoguard/internal/journal"
	"github.com/celaldrg/undoguard/internal/paths"
	"github.com/celaldrg/undoguard/internal/policy"
	"github.com/celaldrg/undoguard/internal/snapshot"
)

// limits returns the snapshot limits of the policy. Undo and gc must work
// while a policy file is broken, so they fall back to the built-in limits.
func (a *app) limits() policy.Limits {
	dir, err := workDir("")
	if err == nil {
		var p *policy.Policy
		if p, err = a.loadPolicy(dir, true); err == nil {
			return p.Limits
		}
	}
	fmt.Fprintf(a.stderr, "undoguard: warning: %v; using the built-in snapshot limits\n", err)
	return policy.DefaultLimits()
}

func cmdUndo(a *app, args []string) int {
	fs := a.flags("undo", "[--force] [--dry-run] [--last N | --session | --session-id ID | <id>]")
	force := fs.Bool("force", false, "overwrite paths that changed after the command ran")
	dryRun := fs.Bool("dry-run", false, "show what would be restored without changing anything")
	session := fs.Bool("session", false, "undo every snapshot of the session of the last snapshot, newest first")
	sessionID := fs.String("session-id", "", "undo every snapshot of the session with this `ID`, newest first")
	last := fs.Int("last", 0, "undo the newest `N` snapshots, newest first")
	if code, ok := parse(fs, args); !ok {
		return code
	}
	bySession := *session || *sessionID != ""
	modes := 0
	for _, set := range []bool{*session, *sessionID != "", *last != 0, fs.NArg() > 0} {
		if set {
			modes++
		}
	}
	if fs.NArg() > 1 || modes > 1 || *last < 0 {
		fs.Usage()
		return exitError
	}
	root := paths.DataDir()
	opts := snapshot.RestoreOptions{Root: root, Limits: a.limits(), Force: *force, DryRun: *dryRun}

	if *last > 0 {
		list, err := snapshot.LatestN(root, *last)
		if err != nil {
			return a.errorf("%v", err)
		}
		if len(list) == 0 {
			return a.errorf("there is no snapshot left to undo")
		}
		label := fmt.Sprintf("The last %d %s", len(list), plural(len(list), "snapshot", "snapshots"))
		if len(list) < *last {
			label += fmt.Sprintf(" (only %d left of the %d asked for)", len(list), *last)
		}
		return a.undoList(label, list, opts)
	}

	if !bySession {
		var m *snapshot.Manifest
		var err error
		if fs.NArg() == 1 {
			m, err = snapshot.Find(root, fs.Arg(0))
		} else {
			m, err = snapshot.Latest(root, "")
		}
		if err != nil {
			return a.errorf("%v", err)
		}
		if !a.undoOne(m, opts) {
			return exitError
		}
		return exitOK
	}

	id := *sessionID
	if *session {
		last, err := snapshot.Latest(root, "")
		if err != nil {
			return a.errorf("%v", err)
		}
		if last.Session == "" {
			return a.errorf("the last snapshot (%s) belongs to no session; undo it by id", last.ID)
		}
		id = last.Session
	}
	list, err := snapshot.SessionSnapshots(root, id)
	if err != nil {
		return a.errorf("%v", err)
	}
	if len(list) == 0 {
		return a.errorf("session %q has no snapshot left to undo", id)
	}
	return a.undoList("Session "+id, list, opts)
}

// undoList undoes snapshots newest first and stops at the first one that
// fails, since the older ones may depend on it.
func (a *app) undoList(label string, list []*snapshot.Manifest, opts snapshot.RestoreOptions) int {
	fmt.Fprintf(a.stdout, "%s: %d %s to undo, newest first.\n", label, len(list),
		plural(len(list), "snapshot", "snapshots"))
	if opts.DryRun && len(list) > 1 {
		fmt.Fprintln(a.stdout, "A dry run compares each snapshot with the disk as it is now, so the plans of older")
		fmt.Fprintln(a.stdout, "snapshots do not yet reflect the newer ones being undone.")
	}
	for i, m := range list {
		fmt.Fprintln(a.stdout)
		if !a.undoOne(m, opts) {
			if left := len(list) - i - 1; left > 0 {
				fmt.Fprintf(a.stderr, "undoguard: stopped; %d older %s were not undone\n", left,
					plural(left, "snapshot", "snapshots"))
			}
			return exitError
		}
	}
	return exitOK
}

// undoOne restores one snapshot and reports what happened. It returns
// false when the undo was refused or a path failed.
func (a *app) undoOne(m *snapshot.Manifest, o snapshot.RestoreOptions) bool {
	w := a.stdout
	fmt.Fprintf(w, "Snapshot %s, taken %s\n", m.ID, m.Created.Local().Format(timeLayout))
	fmt.Fprintf(w, "Command:  %s\n", journal.Redact(m.Command))
	if m.Restored != nil {
		fmt.Fprintf(w, "Note:     already undone on %s", m.Restored.Local().Format(timeLayout))
		if m.RestoredBy != "" {
			fmt.Fprintf(w, "; 'undoguard undo %s' reverts that undo", m.RestoredBy)
		}
		fmt.Fprintln(w)
	}
	if o.Session == "" {
		o.Session = m.Session
	}
	res, err := snapshot.Restore(m, o)
	if res != nil {
		printPlan(w, res.Plan, o.DryRun)
	}
	var ce *snapshot.ConflictError
	switch {
	case errors.As(err, &ce):
		fmt.Fprintln(a.stderr)
		for _, c := range ce.Changes {
			fmt.Fprintf(a.stderr, "  %s: %s\n", paths.Display(c.Path), c.Conflict)
		}
		fmt.Fprintf(a.stderr, "undoguard: %v\n", err)
		fmt.Fprintf(a.stderr, "Check them with 'undoguard diff %s' first.\n", m.ID)
		return false
	case err != nil:
		fmt.Fprintf(a.stderr, "undoguard: %v\n", err)
		return false
	}
	if res.Plan.Empty() {
		if len(res.Plan.Changes) > 0 {
			fmt.Fprintln(w, "Nothing can be undone: none of the changed paths can be restored.")
			return false
		}
		fmt.Fprintln(w, "Nothing to undo: the disk already matches the snapshot.")
		return true
	}
	if o.DryRun {
		return true
	}
	for _, x := range res.Warnings {
		fmt.Fprintf(a.stderr, "undoguard: warning: %s\n", x)
	}
	for _, x := range res.Failed {
		fmt.Fprintf(a.stderr, "undoguard: failed: %s\n", x)
	}
	fmt.Fprintf(w, "\nRestored %d %s", res.Done, plural(res.Done, "path", "paths"))
	if len(res.Failed) > 0 {
		fmt.Fprintf(w, ", %d failed", len(res.Failed))
	}
	fmt.Fprintln(w, ".")
	if res.Safety != "" {
		fmt.Fprintf(w, "The state it replaced is snapshot %s; 'undoguard undo %s' brings it back.\n", res.Safety, res.Safety)
	} else {
		fmt.Fprintln(w, "The state it replaced could not be saved, so this undo cannot be undone.")
	}

	reason := fmt.Sprintf("restored %d of %d paths", res.Done, len(res.Plan.Changes))
	if len(res.Failed) > 0 {
		reason += fmt.Sprintf(", %d failed", len(res.Failed))
	}
	if res.Safety != "" {
		reason += "; replaced state saved as " + res.Safety
	}
	if err := journal.Append(o.Root, journal.Entry{
		Event:    journal.EventUndo,
		Agent:    m.Agent,
		Session:  m.Session,
		Command:  m.Command,
		Dir:      m.Dir,
		Reason:   reason,
		Snapshot: m.ID,
	}); err != nil {
		fmt.Fprintf(a.stderr, "undoguard: warning: writing the journal: %v\n", err)
	}
	return len(res.Failed) == 0
}

// printPlan lists the changes of an undo.
func printPlan(w io.Writer, p *snapshot.Plan, dryRun bool) {
	if p == nil || len(p.Changes) == 0 {
		return
	}
	if dryRun {
		fmt.Fprintln(w, "\nWould change:")
	} else {
		fmt.Fprintln(w, "\nChanges:")
	}
	for _, c := range p.Changes {
		fmt.Fprintf(w, "  %-9s %s\n", verb(c), paths.Display(c.Path))
		switch {
		case c.Reason != "":
			fmt.Fprintf(w, "            not restored: %s\n", c.Reason)
		case c.Conflict != "":
			fmt.Fprintf(w, "            conflict: %s\n", c.Conflict)
		}
	}
	if p.Unchanged > 0 {
		fmt.Fprintf(w, "  (%d unchanged %s)\n", p.Unchanged, plural(p.Unchanged, "path", "paths"))
	}
}

// verb says what undo does to one path.
func verb(c snapshot.Change) string {
	switch c.Kind {
	case snapshot.ChangeRestore:
		if c.Now.Kind == snapshot.KindAbsent {
			return "restore"
		}
		return "revert"
	case snapshot.ChangeRecreate:
		return "recreate"
	case snapshot.ChangeRemove:
		return "remove"
	case snapshot.ChangeMetadata:
		return "mode"
	}
	return "skip"
}

func cmdGC(a *app, args []string) int {
	fs := a.flags("gc", "[--dry-run]")
	dryRun := fs.Bool("dry-run", false, "show what would be deleted without deleting it")
	if code, ok := parse(fs, args); !ok {
		return code
	}
	if fs.NArg() > 0 {
		fs.Usage()
		return exitError
	}
	res, err := snapshot.GC(snapshot.GCOptions{Root: paths.DataDir(), Limits: a.limits(), DryRun: *dryRun})
	if res != nil {
		w := a.stdout
		what := "Removed"
		if *dryRun {
			what = "Would remove"
		}
		fmt.Fprintf(w, "%s %d %s and %d stored %s, %s.\n", what, len(res.Removed),
			plural(len(res.Removed), "snapshot", "snapshots"), res.Objects, plural(res.Objects, "file", "files"),
			policy.FormatSize(res.Freed))
		for _, id := range res.Removed {
			fmt.Fprintf(w, "  %s\n", id)
		}
		fmt.Fprintf(w, "Kept %d %s; the store holds %s.\n", res.Kept, plural(res.Kept, "snapshot", "snapshots"),
			policy.FormatSize(res.StoreSize))
		if len(res.Unreadable) > 0 {
			fmt.Fprintf(w, "%d %s could not be read, so no stored file was deleted:\n", len(res.Unreadable),
				plural(len(res.Unreadable), "snapshot", "snapshots"))
			for _, id := range res.Unreadable {
				fmt.Fprintf(w, "  %s\n", id)
			}
		}
	}
	if err != nil {
		return a.errorf("%v", err)
	}
	return exitOK
}
