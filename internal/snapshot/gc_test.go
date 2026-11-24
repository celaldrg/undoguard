//go:build unix

package snapshot

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/celaldrg/undoguard/internal/policy"
	"github.com/celaldrg/undoguard/internal/store"
)

// checkGC compares a clean-up with what it should have done. snaps are
// oldest first, and removed holds the indexes of those that should go.
func checkGC(t *testing.T, w *world, res *GCResult, snaps []*Manifest, removed []int, objects int, freed, storeSize int64, dryRun bool) {
	t.Helper()
	var want []string
	for _, i := range removed {
		want = append(want, snaps[i].ID)
	}
	slices.Sort(want)
	if !slices.Equal(res.Removed, want) {
		t.Errorf("removed %v, want %v", res.Removed, want)
	}
	kept := len(snaps) - len(removed)
	if res.Kept != kept || res.Objects != objects || res.Freed != freed || res.StoreSize != storeSize {
		t.Errorf("kept %d, objects %d, freed %d, store size %d; want %d, %d, %d, %d",
			res.Kept, res.Objects, res.Freed, res.StoreSize, kept, objects, freed, storeSize)
	}
	var left []string
	for i := len(snaps) - 1; i >= 0; i-- {
		if dryRun || !slices.Contains(removed, i) {
			left = append(left, snaps[i].ID)
		}
	}
	ids, err := IDs(w.data)
	must(t, err)
	if !slices.Equal(ids, left) {
		t.Errorf("snapshots left %v, want %v", ids, left)
	}
}

func TestGCByAge(t *testing.T) {
	base := time.Date(2020, 1, 1, 0, 0, 0, 0, time.UTC)
	day := 24 * time.Hour
	secondAt := base.Add(day)
	thirdAt := base.Add(10 * day)
	now := base.Add(11 * day)
	cases := []struct {
		name    string
		limits  policy.Limits
		dryRun  bool
		removed []int
		freed   int64
	}{
		{"past keep_days", policy.Limits{KeepDays: 5, KeepMin: 1}, false, []int{0, 1}, 4},
		{"keep_min holds one more", policy.Limits{KeepDays: 5, KeepMin: 2}, false, []int{0}, 4},
		{"keep_min holds all", policy.Limits{KeepDays: 5, KeepMin: 3}, false, nil, 0},
		{"no limits", policy.Limits{}, false, nil, 0},
		{"dry run", policy.Limits{KeepDays: 5, KeepMin: 1}, true, []int{0, 1}, 4},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			w := newWorld(t)
			// The two newer snapshots hold the same content, stored once.
			snaps := []*Manifest{
				w.snapFile(t, "old.txt", "old\n", Options{Command: "rm old.txt", Now: base}),
				w.snapFile(t, "shared1.txt", "shared\n", Options{Command: "rm shared1.txt", Now: secondAt}),
				w.snapFile(t, "shared2.txt", "shared\n", Options{Command: "rm shared2.txt", Now: thirdAt}),
			}
			res, err := GC(GCOptions{Root: w.data, Limits: tc.limits, DryRun: tc.dryRun, Now: now})
			must(t, err)
			objects := 0
			if tc.freed > 0 {
				objects = 1
			}
			storeSize := 11 - tc.freed
			checkGC(t, w, res, snaps, tc.removed, objects, tc.freed, storeSize, tc.dryRun)

			s, err := store.Open(w.data)
			must(t, err)
			oldKept := tc.dryRun || tc.freed == 0
			if got := s.Has(snaps[0].Entries[0].Before.Hash); got != oldKept {
				t.Errorf("object of old.txt present: %v, want %v", got, oldKept)
			}
			if !s.Has(snaps[2].Entries[0].Before.Hash) {
				t.Error("the shared object was removed")
			}
			_, err = os.Stat(stampPath(w.data))
			if stamped := err == nil; stamped == tc.dryRun {
				t.Errorf("gc stamp written: %v, dry run: %v", stamped, tc.dryRun)
			}
		})
	}
}

func TestGCByStoreLimit(t *testing.T) {
	base := time.Date(2020, 1, 1, 0, 0, 0, 0, time.UTC)
	day := 24 * time.Hour
	// Three snapshots of 100 bytes each. Two days in, the newer two are
	// 40 and 20 minutes old.
	times := []time.Time{base, base.Add(2*day - 40*time.Minute), base.Add(2*day - 20*time.Minute)}
	twoDaysIn := base.Add(2 * day)
	threeDaysIn := base.Add(3 * day)
	cases := []struct {
		name    string
		limits  policy.Limits
		now     time.Time
		removed []int
	}{
		{"oldest goes first", policy.Limits{StoreLimit: 250, KeepMin: 1}, threeDaysIn, []int{0}},
		{"keep_min stops it", policy.Limits{StoreLimit: 50, KeepMin: 2}, threeDaysIn, []int{0}},
		{"down to one", policy.Limits{StoreLimit: 1}, threeDaysIn, []int{0, 1}},
		{"young snapshots stay", policy.Limits{StoreLimit: 1}, twoDaysIn, []int{0}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			w := newWorld(t)
			var snaps []*Manifest
			for i, name := range []string{"a", "b", "c"} {
				file := name + ".txt"
				content := strings.Repeat(name, 99) + "\n"
				command := "rm " + file
				snaps = append(snaps, w.snapFile(t, file, content, Options{Command: command, Now: times[i]}))
			}
			res, err := GC(GCOptions{Root: w.data, Limits: tc.limits, Now: tc.now})
			must(t, err)
			n := int64(len(tc.removed))
			freed := 100 * n
			storeSize := 300 - freed
			checkGC(t, w, res, snaps, tc.removed, len(tc.removed), freed, storeSize, false)

			s, err := store.Open(w.data)
			must(t, err)
			for i, m := range snaps {
				want := !slices.Contains(tc.removed, i)
				if got := s.Has(m.Entries[0].Before.Hash); got != want {
					t.Errorf("object of %s present: %v, want %v", m.Command, got, want)
				}
			}
		})
	}
}

func TestGCUnreadable(t *testing.T) {
	w := newWorld(t)
	base := time.Date(2020, 2, 1, 0, 0, 0, 0, time.UTC)
	orphanAt := base.Add(time.Minute)
	now := base.Add(24 * time.Hour)
	kept := w.snapFile(t, "kept.txt", "kept\n", Options{Command: "rm kept.txt", Now: base})
	orphan := w.snapFile(t, "orphan.txt", "orphan\n", Options{Command: "rm orphan.txt", Now: orphanAt})
	// With its snapshot gone, nothing refers to the orphan's object.
	must(t, os.RemoveAll(filepath.Join(snapshotsDir(w.data), orphan.ID)))
	// A snapshot that cannot be read may still need it.
	broken := "20200101-000000-abcdef"
	must(t, os.MkdirAll(filepath.Join(snapshotsDir(w.data), broken), 0o700))
	must(t, os.WriteFile(manifestPath(w.data, broken), []byte("{"), 0o600))
	// A directory without a manifest is what an interrupted Take leaves.
	interrupted := filepath.Join(snapshotsDir(w.data), "20200101-000001-abcdef")
	must(t, os.Mkdir(interrupted, 0o700))

	res, err := GC(GCOptions{Root: w.data, Now: now})
	must(t, err)
	if !slices.Equal(res.Unreadable, []string{broken}) {
		t.Errorf("unreadable = %v, want %s", res.Unreadable, broken)
	}
	if len(res.Removed) != 0 || res.Kept != 1 || res.Objects != 0 || res.StoreSize != 12 {
		t.Errorf("result = %+v, want nothing removed and 12 bytes stored", res)
	}
	checkAbsent(t, interrupted)
	s, err := store.Open(w.data)
	must(t, err)
	keptHash := kept.Entries[0].Before.Hash
	orphanHash := orphan.Entries[0].Before.Hash
	if !s.Has(keptHash) || !s.Has(orphanHash) {
		t.Fatal("an object was removed while a snapshot could not be read")
	}

	must(t, os.RemoveAll(filepath.Join(snapshotsDir(w.data), broken)))
	res, err = GC(GCOptions{Root: w.data, Now: now})
	must(t, err)
	if len(res.Unreadable) != 0 || res.Objects != 1 || res.Freed != 7 || res.StoreSize != 5 {
		t.Errorf("result = %+v, want the orphan's 7 bytes freed", res)
	}
	if !s.Has(keptHash) || s.Has(orphanHash) {
		t.Errorf("kept object present: %v, orphan present: %v", s.Has(keptHash), s.Has(orphanHash))
	}
}

func TestMaybeGC(t *testing.T) {
	w := newWorld(t)
	w.snapFile(t, "a.txt", "a\n", Options{Command: "rm a.txt"})
	now := time.Now()
	soon := now.Add(time.Hour)
	later := now.Add(25 * time.Hour)

	res, err := MaybeGC(w.data, policy.Limits{}, now)
	if err != nil || res == nil || res.Kept != 1 {
		t.Fatalf("first run = %+v, %v, want a run that keeps the snapshot", res, err)
	}
	// The stamp holds off the next run for a day.
	if res, err := MaybeGC(w.data, policy.Limits{}, soon); err != nil || res != nil {
		t.Errorf("run an hour later = %+v, %v, want none", res, err)
	}
	if res, err := MaybeGC(w.data, policy.Limits{}, later); err != nil || res == nil {
		t.Errorf("run a day later = %+v, %v, want one", res, err)
	}
}

func TestNewIDSortsByTime(t *testing.T) {
	base := time.Date(2026, 11, 24, 15, 28, 6, 0, time.UTC)
	var ids []string
	for _, ns := range []int{0, 1_000_000, 250_000_000, 999_999_999} {
		id, err := newID(base.Add(time.Duration(ns)))
		if err != nil {
			t.Fatal(err)
		}
		if !idPattern.MatchString(id) {
			t.Fatalf("id %q does not match the id pattern", id)
		}
		ids = append(ids, id)
	}
	next, err := newID(base.Add(time.Second))
	if err != nil {
		t.Fatal(err)
	}
	ids = append(ids, next)
	for i := 1; i < len(ids); i++ {
		if ids[i-1] >= ids[i] {
			t.Errorf("id %q taken before %q sorts after it", ids[i-1], ids[i])
		}
	}
}
