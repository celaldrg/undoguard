package snapshot

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"time"

	"github.com/celaldrg/undoguard/internal/flock"
	"github.com/celaldrg/undoguard/internal/policy"
	"github.com/celaldrg/undoguard/internal/store"
)

// minAge protects snapshots that a running command may still finish.
const minAge = time.Hour

// gcInterval is how often the hooks clean up on their own.
const gcInterval = 24 * time.Hour

// GCOptions control a clean-up.
type GCOptions struct {
	Root   string
	Limits policy.Limits
	// DryRun reports what would go without deleting anything.
	DryRun bool
	// Now is the current time; zero means time.Now.
	Now time.Time
}

// GCResult is what a clean-up removed.
type GCResult struct {
	// Removed lists the dropped snapshots, oldest first.
	Removed []string
	Kept    int
	// Objects and Freed count the stored files deleted and their size.
	Objects int
	Freed   int64
	// StoreSize is the size of the stored files afterwards.
	StoreSize int64
	// Unreadable lists snapshots that could not be read. While there are
	// any, no stored file is deleted, since one of them may need it.
	Unreadable []string
}

// GC drops the snapshots that are past their retention and the stored
// files that no snapshot needs.
//
// The newest keep_min snapshots always stay. Older ones go after
// keep_days, and then, oldest first, until the store fits store_limit.
// Snapshots younger than an hour always stay, since their command may
// still be running.
func GC(o GCOptions) (*GCResult, error) {
	lock, err := flock.Exclusive(LockPath(o.Root))
	if err != nil {
		return nil, err
	}
	defer lock.Release()
	return gc(o)
}

// MaybeGC runs GC when the last run is more than a day old and no other
// undoguard process is using the store. It never waits, and returns nil
// when it did not run.
func MaybeGC(root string, limits policy.Limits, now time.Time) (*GCResult, error) {
	if info, err := os.Stat(stampPath(root)); err == nil && now.Sub(info.ModTime()) < gcInterval {
		return nil, nil
	}
	lock, err := flock.TryExclusive(LockPath(root))
	if err != nil || lock == nil {
		return nil, err
	}
	defer lock.Release()
	return gc(GCOptions{Root: root, Limits: limits, Now: now})
}

func stampPath(root string) string {
	return filepath.Join(root, "gc-stamp")
}

func gc(o GCOptions) (*GCResult, error) {
	if o.Now.IsZero() {
		o.Now = time.Now()
	}
	res := &GCResult{}
	ids, err := IDs(o.Root)
	if err != nil {
		return nil, err
	}
	var all []*Manifest
	for _, id := range ids {
		m, err := Load(o.Root, id)
		if err == nil {
			all = append(all, m)
			continue
		}
		dir := filepath.Join(snapshotsDir(o.Root), id)
		_, statErr := os.Stat(manifestPath(o.Root, id))
		if errors.Is(statErr, fs.ErrNotExist) && o.Now.Sub(idTime(id)) >= minAge {
			// Left behind by a snapshot that was interrupted before it
			// was saved.
			if !o.DryRun {
				_ = os.RemoveAll(dir)
			}
			continue
		}
		res.Unreadable = append(res.Unreadable, id)
	}

	keepMin := o.Limits.KeepMin
	droppable := func(i int, m *Manifest) bool {
		return i >= keepMin && o.Now.Sub(m.Created) >= minAge
	}
	var keep, drop []*Manifest
	cutoff := o.Now.Add(-time.Duration(o.Limits.KeepDays) * 24 * time.Hour)
	for i, m := range all {
		if o.Limits.KeepDays > 0 && droppable(i, m) && m.Created.Before(cutoff) {
			drop = append(drop, m)
		} else {
			keep = append(keep, m)
		}
	}

	s, err := store.Open(o.Root)
	if err != nil {
		return nil, err
	}
	sizes := map[string]int64{}
	if err := s.Walk(func(hash string, size int64) error {
		sizes[hash] = size
		return nil
	}); err != nil {
		return nil, err
	}
	refs := map[string]int{}
	var total int64
	for _, m := range keep {
		for _, h := range hashes(m) {
			if refs[h] == 0 {
				total += sizes[h]
			}
			refs[h]++
		}
	}
	for o.Limits.StoreLimit > 0 && total > o.Limits.StoreLimit && len(keep) > 1 {
		i := len(keep) - 1
		last := keep[i]
		if !droppable(i, last) {
			break
		}
		keep = keep[:i]
		drop = append(drop, last)
		for _, h := range hashes(last) {
			refs[h]--
			if refs[h] == 0 {
				total -= sizes[h]
			}
		}
	}

	for _, m := range drop {
		res.Removed = append(res.Removed, m.ID)
		if !o.DryRun {
			if err := os.RemoveAll(filepath.Join(snapshotsDir(o.Root), m.ID)); err != nil {
				return res, err
			}
		}
	}
	sort.Strings(res.Removed)
	res.Kept = len(keep)

	res.StoreSize = total
	for h, size := range sizes {
		if refs[h] > 0 {
			continue
		}
		if len(res.Unreadable) > 0 {
			res.StoreSize += size
			continue
		}
		res.Objects++
		res.Freed += size
		if !o.DryRun {
			if err := s.Remove(h); err != nil {
				return res, err
			}
		}
	}
	if !o.DryRun {
		s.CleanTemp()
		_ = os.WriteFile(stampPath(o.Root), []byte(o.Now.UTC().Format(time.RFC3339)+"\n"), 0o600)
	}
	return res, nil
}

// hashes returns the stored files a snapshot refers to, once each.
func hashes(m *Manifest) []string {
	seen := map[string]bool{}
	var out []string
	for _, e := range m.Entries {
		if h := e.Before.Hash; h != "" && !seen[h] {
			seen[h] = true
			out = append(out, h)
		}
	}
	return out
}
