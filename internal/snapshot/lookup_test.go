//go:build unix

package snapshot

import (
	"os"
	"slices"
	"testing"
	"time"
)

func idsOf(list []*Manifest) []string {
	var out []string
	for _, m := range list {
		out = append(out, m.ID)
	}
	return out
}

func TestLookupEmpty(t *testing.T) {
	w := newWorld(t)
	now := time.Now()
	if ids, err := IDs(w.data); err != nil || len(ids) != 0 {
		t.Errorf("IDs = %v, %v", ids, err)
	}
	if list, err := List(w.data); err != nil || len(list) != 0 {
		t.Errorf("List = %v, %v", list, err)
	}
	if m, err := Find(w.data, "2020"); err == nil {
		t.Errorf("Find = %v, want an error", m)
	}
	if m, err := Latest(w.data, ""); err == nil {
		t.Errorf("Latest = %v, want an error", m)
	}
	if m, err := Pending(w.data, "s", "rm x", time.Hour, now); err != nil || m != nil {
		t.Errorf("Pending = %v, %v", m, err)
	}
	if ids, err := Unfinished(w.data, "s", time.Hour, now); err != nil || len(ids) != 0 {
		t.Errorf("Unfinished = %v, %v", ids, err)
	}
	if list, err := SessionSnapshots(w.data, "s"); err != nil || len(list) != 0 {
		t.Errorf("SessionSnapshots = %v, %v", list, err)
	}
	if _, err := Load(w.data, "not-an-id"); err == nil {
		t.Error("Load accepted a malformed id")
	}
}

func TestLookup(t *testing.T) {
	w := newWorld(t)
	base := time.Date(2020, 3, 4, 10, 0, 0, 0, time.UTC)
	// snap snapshots a file for a session at a given time, then removes it.
	snap := func(name, session string, at time.Time) *Manifest {
		t.Helper()
		content := name + "\n"
		command := "rm " + name
		m := w.snapFile(t, name, content, Options{Session: session, Command: command, Now: at})
		must(t, os.Remove(w.path(name)))
		return m
	}
	bAt := base.Add(time.Minute)
	cAt := base.Add(2 * time.Minute)
	now := base.Add(3 * time.Minute)
	a := snap("a.txt", "s1", base)
	b := snap("b.txt", "s2", bAt)
	c := snap("c.txt", "s1", cAt)
	w.finishSnapshot(t, a.ID)

	ids, err := IDs(w.data)
	must(t, err)
	if want := []string{c.ID, b.ID, a.ID}; !slices.Equal(ids, want) {
		t.Fatalf("IDs = %v, want %v", ids, want)
	}
	list, err := List(w.data)
	must(t, err)
	if got := idsOf(list); !slices.Equal(got, ids) {
		t.Fatalf("List = %v, want %v", got, ids)
	}
	if oldest := list[len(list)-1]; !oldest.Created.Equal(base) || oldest.Command != "rm a.txt" || oldest.Finished == nil {
		t.Errorf("oldest snapshot = %+v", oldest)
	}

	suffix := c.ID[len(c.ID)-6:]
	finds := []struct {
		query string
		want  string
	}{
		{a.ID, a.ID},
		{"20200304-1001", b.ID},
		{suffix, c.ID},
		{"20200304", ""},
		{"19990101", ""},
		{"20200304-100000-zzzzzz", ""},
	}
	for _, f := range finds {
		m, err := Find(w.data, f.query)
		switch {
		case f.want == "" && err == nil:
			t.Errorf("Find(%q) = %s, want an error", f.query, m.ID)
		case f.want != "" && (err != nil || m.ID != f.want):
			t.Errorf("Find(%q) = %v, %v, want %s", f.query, m, err, f.want)
		}
	}

	for session, want := range map[string][]string{"s1": {c.ID}, "s2": {b.ID}} {
		got, err := Unfinished(w.data, session, time.Hour, now)
		if err != nil || !slices.Equal(got, want) {
			t.Errorf("Unfinished(%s) = %v, %v, want %v", session, got, err, want)
		}
	}

	pending := []struct {
		session string
		command string
		maxAge  time.Duration
		want    string
	}{
		{"s1", "rm c.txt", time.Hour, c.ID},
		{"s2", "rm b.txt", time.Hour, b.ID},
		// Finished snapshots are not pending.
		{"s1", "rm a.txt", time.Hour, ""},
		// The search stops at the first snapshot older than maxAge.
		{"s2", "rm b.txt", 90 * time.Second, ""},
		{"s1", "rm b.txt", time.Hour, ""},
	}
	for _, p := range pending {
		m, err := Pending(w.data, p.session, p.command, p.maxAge, now)
		got := ""
		if m != nil {
			got = m.ID
		}
		if err != nil || got != p.want {
			t.Errorf("Pending(%s, %q, %v) = %q, %v, want %q", p.session, p.command, p.maxAge, got, err, p.want)
		}
	}

	latest := []struct {
		session string
		want    string
	}{
		{"s1", c.ID},
		{"s2", b.ID},
		{"", c.ID},
		{"nobody", ""},
	}
	for _, l := range latest {
		m, err := Latest(w.data, l.session)
		got := ""
		if err == nil {
			got = m.ID
		}
		if got != l.want {
			t.Errorf("Latest(%q) = %q (%v), want %q", l.session, got, err, l.want)
		}
	}

	session, err := SessionSnapshots(w.data, "s1")
	must(t, err)
	if got, want := idsOf(session), []string{c.ID, a.ID}; !slices.Equal(got, want) {
		t.Errorf("SessionSnapshots = %v, want %v", got, want)
	}

	// Undoing c leaves a as the session's latest snapshot.
	res, err := Restore(c, RestoreOptions{Root: w.data, Session: "s1"})
	must(t, err)
	if res.Done != 1 {
		t.Errorf("done %d, failed %v", res.Done, res.Failed)
	}
	checkFile(t, w.path("c.txt"), "c.txt\n", 0o644, time.Time{})
	if m, err := Latest(w.data, "s1"); err != nil || m.ID != a.ID {
		t.Errorf("Latest after undo = %v (%v), want %s", m, err, a.ID)
	}
	session, err = SessionSnapshots(w.data, "s1")
	must(t, err)
	if got, want := idsOf(session), []string{a.ID}; !slices.Equal(got, want) {
		t.Errorf("SessionSnapshots after undo = %v, want %v", got, want)
	}
	safety, err := Load(w.data, res.Safety)
	must(t, err)
	if safety.Session != "s1" || safety.Undoes != c.ID || safety.Status() != "taken by undo" {
		t.Errorf("safety snapshot = %+v", safety)
	}
	undone, err := Load(w.data, c.ID)
	must(t, err)
	if undone.Status() != "undone" {
		t.Errorf("status of the undone snapshot = %q", undone.Status())
	}
}
