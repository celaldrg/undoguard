package snapshot

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/celaldrg/undoguard/internal/detect"
	"github.com/celaldrg/undoguard/internal/flock"
	"github.com/celaldrg/undoguard/internal/gitops"
	"github.com/celaldrg/undoguard/internal/paths"
	"github.com/celaldrg/undoguard/internal/policy"
	"github.com/celaldrg/undoguard/internal/store"
)

// Options describe the command a snapshot is taken for.
type Options struct {
	// Root is the data directory that holds the store and the snapshots.
	Root   string
	Limits policy.Limits
	// Session, Agent and Command describe who ran what, and where.
	Session string
	Agent   string
	Command string
	Dir     string
	// Undoes is set when undo takes the snapshot, to the snapshot it
	// restores.
	Undoes string
	// Now is the time to record; zero means the current time.
	Now time.Time
}

// TooLargeError reports that the paths exceed a snapshot limit. Nothing
// was stored.
type TooLargeError struct {
	// What is "file", "total" or "paths".
	What  string
	Path  string
	Size  int64
	Limit int64
}

func (e *TooLargeError) Error() string {
	switch e.What {
	case "file":
		return fmt.Sprintf("%s is %s, over the %s limit for one file", paths.Display(e.Path),
			policy.FormatSize(e.Size), policy.FormatSize(e.Limit))
	case "paths":
		return fmt.Sprintf("the command touches more than %d paths", e.Limit)
	}
	return fmt.Sprintf("the files add up to more than the %s limit per command", policy.FormatSize(e.Limit))
}

// IncompleteError reports a snapshot that was saved without the content
// of some paths.
type IncompleteError struct {
	ID string
	// Skipped holds "path: reason" lines.
	Skipped []string
}

func (e *IncompleteError) Error() string {
	shown := e.Skipped
	if len(shown) > 3 {
		shown = shown[:3]
	}
	more := ""
	if len(e.Skipped) > len(shown) {
		more = fmt.Sprintf(" and %d more", len(e.Skipped)-len(shown))
	}
	return fmt.Sprintf("snapshot %s could not store everything: %s%s", e.ID, strings.Join(shown, "; "), more)
}

// Take snapshots every path below the roots before a command changes
// them.
//
// On TooLargeError nothing is saved. On IncompleteError the snapshot is
// saved and returned with the error, and the paths it lacks are listed.
func Take(roots []Root, o Options) (*Manifest, error) {
	lock, err := flock.Shared(LockPath(o.Root))
	if err != nil {
		return nil, err
	}
	defer lock.Release()
	return take(roots, o)
}

// take does the work of Take for a caller that holds the lock.
func take(roots []Root, o Options) (*Manifest, error) {
	if len(roots) == 0 {
		return nil, errors.New("nothing to snapshot")
	}
	now := o.Now
	if now.IsZero() {
		now = time.Now()
	}
	c := &collector{max: o.Limits.MaxFiles, seen: map[string]bool{}}
	for _, r := range roots {
		if err := c.root(r); err != nil {
			if errors.Is(err, errTooMany) {
				return nil, &TooLargeError{What: "paths", Limit: int64(o.Limits.MaxFiles)}
			}
			return nil, err
		}
	}
	sort.Strings(c.list)

	m := &Manifest{
		Version: FormatVersion,
		Created: now,
		Session: o.Session,
		Agent:   o.Agent,
		Command: o.Command,
		Dir:     o.Dir,
		Roots:   roots,
		Undoes:  o.Undoes,
	}
	skipped := c.notes
	var total int64
	for _, p := range c.list {
		st, err := stateOf(p)
		if err != nil {
			st = State{Kind: KindOther, UID: -1, GID: -1, Skipped: err.Error()}
			skipped = append(skipped, paths.Display(p)+": "+err.Error())
		}
		if st.Kind == KindFile {
			if l := o.Limits.MaxFileSize; l > 0 && st.Size > l {
				return nil, &TooLargeError{What: "file", Path: p, Size: st.Size, Limit: l}
			}
			total += st.Size
			if l := o.Limits.MaxTotalSize; l > 0 && total > l {
				return nil, &TooLargeError{What: "total", Size: total, Limit: l}
			}
		}
		m.Entries = append(m.Entries, Entry{Path: p, Before: st})
	}

	s, err := store.Open(o.Root)
	if err != nil {
		return nil, err
	}
	for i := range m.Entries {
		e := &m.Entries[i]
		if e.Before.Kind != KindFile {
			continue
		}
		size, cloned, err := storeFile(s, e, o.Limits.MaxFileSize)
		if err != nil {
			var tooLarge *TooLargeError
			if errors.As(err, &tooLarge) {
				return nil, err
			}
			e.Before.Skipped = err.Error()
			skipped = append(skipped, paths.Display(e.Path)+": "+err.Error())
			continue
		}
		m.Bytes += size
		// A file can grow between the size check above and the copy.
		if l := o.Limits.MaxTotalSize; l > 0 && m.Bytes > l {
			return nil, &TooLargeError{What: "total", Size: m.Bytes, Limit: l}
		}
		if cloned {
			m.Cloned++
		}
	}

	if m.ID, err = claimID(o.Root, now); err != nil {
		return nil, err
	}
	if err := m.save(o.Root); err != nil {
		return nil, err
	}
	if len(skipped) > 0 {
		return m, &IncompleteError{ID: m.ID, Skipped: skipped}
	}
	return m, nil
}

// claimID creates the directory of a new snapshot and returns its id.
func claimID(root string, now time.Time) (string, error) {
	if err := os.MkdirAll(snapshotsDir(root), 0o700); err != nil {
		return "", err
	}
	for range 5 {
		id, err := newID(now)
		if err != nil {
			return "", err
		}
		err = os.Mkdir(filepath.Join(snapshotsDir(root), id), 0o700)
		if err == nil {
			return id, nil
		}
		if !errors.Is(err, fs.ErrExist) {
			return "", err
		}
	}
	return "", errors.New("could not pick a free snapshot id")
}

// storeFile copies a file into the store. A file that changes while it is
// copied is tried again, so that the recorded state matches the stored
// content. A file that grows past maxSize (if not zero) meanwhile gives a
// TooLargeError.
func storeFile(s *store.Store, e *Entry, maxSize int64) (int64, bool, error) {
	for range 3 {
		hash, size, cloned, err := s.Put(e.Path)
		if err != nil {
			return 0, false, err
		}
		now, err := stateOf(e.Path)
		if err != nil {
			return 0, false, err
		}
		if same(now, e.Before) && size == e.Before.Size {
			e.Before.Hash = hash
			return size, cloned, nil
		}
		if now.Kind != KindFile {
			return 0, false, fmt.Errorf("it turned into a %s while it was copied", now.Kind)
		}
		if maxSize > 0 && now.Size > maxSize {
			return 0, false, &TooLargeError{What: "file", Path: e.Path, Size: now.Size, Limit: maxSize}
		}
		e.Before = now
	}
	return 0, false, errors.New("it kept changing while it was copied")
}

var errTooMany = errors.New("too many paths")

// collector gathers the paths of a snapshot once each.
type collector struct {
	max  int
	seen map[string]bool
	list []string
	// notes holds "path: reason" for content that could not be listed.
	notes []string
}

func (c *collector) add(p string) error {
	p = filepath.Clean(p)
	if c.seen[p] {
		return nil
	}
	if c.max > 0 && len(c.list) >= c.max {
		return errTooMany
	}
	c.seen[p] = true
	c.list = append(c.list, p)
	return nil
}

// walk adds dir and everything below it. Like rm -r, it does not follow
// symlinks below dir, and a symlink at dir is taken as the link itself.
func (c *collector) walk(dir string) error {
	return filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
		if aerr := c.add(p); aerr != nil {
			return aerr
		}
		if err != nil && (p != dir || !errors.Is(err, fs.ErrNotExist)) {
			// The directory is recorded, its content is not.
			c.notes = append(c.notes, paths.Display(p)+": "+err.Error())
		}
		return nil
	})
}

// addParents adds the directories from p's parent up to top, so that
// undo can recreate them with their modes.
func (c *collector) addParents(p, top string) error {
	for d := filepath.Dir(p); paths.Within(d, top); d = filepath.Dir(d) {
		if err := c.add(d); err != nil {
			return err
		}
		if d == top {
			break
		}
	}
	return nil
}

func (c *collector) root(r Root) error {
	p := filepath.Clean(r.Path)
	switch {
	case r.Git != "":
		files, err := gitops.Files(r.Repo, p, detect.GitScope(r.Git))
		if err != nil {
			// Without git's list, take the whole tree.
			return c.walk(p)
		}
		// git prints physical paths.
		top := paths.Resolve(p, true)
		for _, f := range files {
			if strings.HasSuffix(f, string(filepath.Separator)) {
				err = c.walk(filepath.Clean(f))
			} else {
				err = c.add(f)
			}
			if err == nil {
				err = c.addParents(filepath.Clean(f), top)
			}
			if err != nil {
				return err
			}
		}
		return nil
	case r.Recursive:
		if err := c.walk(p); err != nil {
			return err
		}
		if target, ok := linkTarget(p); ok && r.Follow {
			return c.walk(target)
		}
		return nil
	default:
		if err := c.add(p); err != nil {
			return err
		}
		if target, ok := linkTarget(p); ok && r.Follow {
			return c.add(target)
		}
		return nil
	}
}

// linkTarget returns where p points when p itself is a symlink.
func linkTarget(p string) (string, bool) {
	info, err := os.Lstat(p)
	if err != nil || info.Mode()&fs.ModeSymlink == 0 {
		return "", false
	}
	target, err := filepath.EvalSymlinks(p)
	if err != nil {
		return "", false
	}
	return target, true
}

// maxNewPaths bounds how many created paths Finish records.
const maxNewPaths = 100000

// Finish records the state of every path after the command ran, and the
// paths it created below the directories it worked on recursively, so
// that undo can tell the command's changes from later ones.
func Finish(root, id string) (*Manifest, error) {
	lock, err := flock.Shared(LockPath(root))
	if err != nil {
		return nil, err
	}
	defer lock.Release()
	return finish(root, id, time.Now())
}

func finish(root, id string, now time.Time) (*Manifest, error) {
	m, err := Load(root, id)
	if err != nil {
		return nil, err
	}
	if m.Finished != nil {
		return m, nil
	}
	known := make(map[string]bool, len(m.Entries))
	for i := range m.Entries {
		e := &m.Entries[i]
		known[e.Path] = true
		st, err := stateOf(e.Path)
		if err != nil {
			// Left unknown: undo then treats the path as changed later.
			continue
		}
		e.After = &st
	}
	added := 0
	record := func(p string, d fs.DirEntry, err error) error {
		if err != nil || known[p] {
			return nil //nolint:nilerr // unreadable entries are skipped, not fatal
		}
		if added >= maxNewPaths {
			return filepath.SkipAll
		}
		info, err := d.Info()
		if err != nil {
			return nil //nolint:nilerr // vanished while walking: nothing to record
		}
		st := stateFromInfo(p, info)
		known[p] = true
		m.Entries = append(m.Entries, Entry{Path: p, Before: State{Kind: KindAbsent, UID: -1, GID: -1}, After: &st})
		added++
		return nil
	}
	for _, r := range m.Roots {
		if !r.Recursive || r.Git != "" {
			continue
		}
		_ = filepath.WalkDir(filepath.Clean(r.Path), record)
		if target, ok := linkTarget(r.Path); ok && r.Follow {
			_ = filepath.WalkDir(target, record)
		}
	}
	sort.Slice(m.Entries, func(i, j int) bool { return m.Entries[i].Path < m.Entries[j].Path })
	m.Finished = &now
	if err := m.save(root); err != nil {
		return nil, err
	}
	return m, nil
}

// Pending returns the newest unfinished snapshot that session took for
// command within maxAge, or nil when there is none.
func Pending(root, session, command string, maxAge time.Duration, now time.Time) (*Manifest, error) {
	ids, err := IDs(root)
	if err != nil {
		return nil, err
	}
	for _, id := range ids {
		if now.Sub(idTime(id)) > maxAge {
			break
		}
		m, err := Load(root, id)
		if err != nil {
			continue
		}
		if m.Finished == nil && m.Undoes == "" && m.Session == session && m.Command == command {
			return m, nil
		}
	}
	return nil, nil
}

// Unfinished returns the ids of the snapshots that session took within
// maxAge and that no one finished, newest first. An agent without a hook
// after each command runs its commands one at a time, so when its next
// command arrives these have ended.
func Unfinished(root, session string, maxAge time.Duration, now time.Time) ([]string, error) {
	ids, err := IDs(root)
	if err != nil {
		return nil, err
	}
	var out []string
	for _, id := range ids {
		if now.Sub(idTime(id)) > maxAge {
			break
		}
		m, err := Load(root, id)
		if err != nil {
			continue
		}
		if m.Finished == nil && m.Undoes == "" && m.Session == session {
			out = append(out, id)
		}
	}
	return out, nil
}
