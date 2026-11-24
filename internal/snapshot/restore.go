package snapshot

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/celaldrg/undoguard/internal/flock"
	"github.com/celaldrg/undoguard/internal/paths"
	"github.com/celaldrg/undoguard/internal/policy"
	"github.com/celaldrg/undoguard/internal/store"
)

// ChangeKind is what undo does to one path.
type ChangeKind string

// The kinds of change. Skip marks a path that differs but cannot be
// restored.
const (
	ChangeRestore  ChangeKind = "restore"
	ChangeRecreate ChangeKind = "recreate"
	ChangeRemove   ChangeKind = "remove"
	ChangeMetadata ChangeKind = "metadata"
	ChangeSkip     ChangeKind = "skip"
)

// Change is one step of an undo.
type Change struct {
	Kind ChangeKind
	Path string
	// Now is the current state and Want the state to go back to.
	Now  State
	Want State
	// Conflict says why the path is not as the command left it. Undo
	// overwrites such paths only when forced.
	Conflict string
	// Reason says why a skipped path cannot be restored.
	Reason string
}

// Plan is what undoing a snapshot would do.
type Plan struct {
	Manifest  *Manifest
	Changes   []Change
	Unchanged int
}

// Empty reports whether undo has nothing to do.
func (p *Plan) Empty() bool {
	for _, c := range p.Changes {
		if c.Kind != ChangeSkip {
			return false
		}
	}
	return true
}

// Conflicts returns the changes that need force.
func (p *Plan) Conflicts() []Change {
	var out []Change
	for _, c := range p.Changes {
		if c.Conflict != "" {
			out = append(out, c)
		}
	}
	return out
}

// PlanRestore compares each path of the snapshot with the disk.
func PlanRestore(m *Manifest) *Plan {
	p := &Plan{Manifest: m}
	for i := range m.Entries {
		e := &m.Entries[i]
		now, err := stateOf(e.Path)
		if err != nil {
			p.Changes = append(p.Changes, Change{Kind: ChangeSkip, Path: e.Path, Want: e.Before, Reason: err.Error()})
			continue
		}
		if same(now, e.Before) {
			p.Unchanged++
			continue
		}
		c := Change{Path: e.Path, Now: now, Want: e.Before}
		switch b := e.Before; {
		case b.Kind == KindAbsent:
			c.Kind = ChangeRemove
		case b.Kind == KindOther:
			c.Kind, c.Reason = ChangeSkip, "sockets, devices and named pipes are not restored"
		case b.Kind == KindDir && now.Kind == KindDir, metadataOnly(b, now):
			c.Kind = ChangeMetadata
		case b.Kind == KindDir:
			c.Kind = ChangeRecreate
		case b.Kind == KindFile && b.Hash == "":
			c.Kind, c.Reason = ChangeSkip, "its content was not stored"
			if b.Skipped != "" {
				c.Reason += ": " + b.Skipped
			}
		default:
			c.Kind = ChangeRestore
		}
		if c.Kind != ChangeSkip {
			c.Conflict = conflict(e, now)
		}
		p.Changes = append(p.Changes, c)
	}
	return p
}

// metadataOnly reports whether only the mode or owner of a file or
// symlink differ.
func metadataOnly(a, b State) bool {
	if a.Kind != b.Kind {
		return false
	}
	switch a.Kind {
	case KindFile:
		return a.Size == b.Size && a.MTime == b.MTime
	case KindSymlink:
		return a.Link == b.Link
	}
	return false
}

// conflict says why restoring e would overwrite something the command
// did not leave behind.
func conflict(e *Entry, now State) string {
	switch {
	case now.Kind == KindAbsent:
		return ""
	case e.After == nil:
		return "the command's result was not recorded, so a later change cannot be ruled out"
	case same(now, *e.After):
		return ""
	}
	return "changed after the command ran"
}

// RestoreOptions control an undo.
type RestoreOptions struct {
	// Root is the data directory.
	Root   string
	Limits policy.Limits
	// Force overwrites paths that changed after the command ran, and
	// goes on when the current state cannot be saved first.
	Force bool
	// DryRun only plans.
	DryRun bool
	// Session is recorded on the safety snapshot.
	Session string
}

// Result is what an undo did.
type Result struct {
	Plan *Plan
	// Safety is the snapshot of the state the undo replaced.
	Safety string
	// Done counts the changes applied and Failed lists "path: reason".
	Done   int
	Failed []string
	// Warnings lists paths that were restored without their owner.
	Warnings []string
}

// ConflictError lists the paths that changed after the command ran.
type ConflictError struct {
	Changes []Change
}

func (e *ConflictError) Error() string {
	n := len(e.Changes)
	what := "paths have"
	if n == 1 {
		what = "path has"
	}
	return fmt.Sprintf("%d %s changed since the command ran; nothing was restored (--force overwrites them)", n, what)
}

// Restore puts the paths of m back to their state before the command.
//
// It refuses when a path changed after the command ran, unless forced.
// Before it writes anything, it snapshots the paths it is about to
// replace, so that the undo itself can be undone.
func Restore(m *Manifest, o RestoreOptions) (*Result, error) {
	lock, err := flock.Shared(LockPath(o.Root))
	if err != nil {
		return nil, err
	}
	defer lock.Release()

	plan := PlanRestore(m)
	res := &Result{Plan: plan}
	if c := plan.Conflicts(); len(c) > 0 && !o.Force {
		return res, &ConflictError{Changes: c}
	}
	if o.DryRun || plan.Empty() {
		return res, nil
	}

	var roots []Root
	for _, c := range plan.Changes {
		if c.Kind != ChangeSkip {
			roots = append(roots, Root{Path: c.Path})
		}
	}
	safety, err := take(roots, Options{
		Root:    o.Root,
		Limits:  o.Limits,
		Session: o.Session,
		Command: "undoguard undo " + m.ID,
		Undoes:  m.ID,
	})
	if err != nil && !o.Force {
		return res, fmt.Errorf("could not save the current state first, so nothing was restored (--force restores anyway): %w", err)
	}
	if safety != nil {
		res.Safety = safety.ID
	}

	s, err := store.Open(o.Root)
	if err != nil {
		return res, err
	}
	a := &applier{store: s, res: res, failed: map[string]bool{}}
	a.apply(m, plan)

	now := time.Now()
	if res.Done > 0 {
		m.Restored = &now
		m.RestoredBy = res.Safety
		if err := m.save(o.Root); err != nil {
			return res, err
		}
		if m.Undoes != "" {
			// Undoing an undo makes the original snapshot current again.
			if orig, err := Load(o.Root, m.Undoes); err == nil && orig.Restored != nil {
				orig.Restored, orig.RestoredBy = nil, ""
				_ = orig.save(o.Root)
			}
		}
	}
	if safety != nil {
		if _, err := finish(o.Root, safety.ID, now); err != nil {
			res.Failed = append(res.Failed, "recording the undo: "+err.Error())
		}
	}
	return res, nil
}

// applier writes a plan to disk.
type applier struct {
	store  *store.Store
	res    *Result
	failed map[string]bool
}

func (a *applier) fail(p string, err error) {
	a.failed[p] = true
	a.res.Failed = append(a.res.Failed, paths.Display(p)+": "+err.Error())
}

func (a *applier) owner(p string, err error) {
	if err != nil {
		a.res.Warnings = append(a.res.Warnings, paths.Display(p)+": owner not restored: "+err.Error())
	}
}

func depth(p string) int {
	return strings.Count(p, string(filepath.Separator))
}

// apply runs the plan in an order that works for nested paths: make
// directories writable, remove what the command created, recreate
// directories, put files and symlinks back, then fix modes, owners and
// times, directories last and deepest first.
func (a *applier) apply(m *Manifest, p *Plan) {
	changes := make([]*Change, 0, len(p.Changes))
	for i := range p.Changes {
		if p.Changes[i].Kind != ChangeSkip {
			changes = append(changes, &p.Changes[i])
		}
	}
	shallowFirst := func(list []*Change) {
		sort.SliceStable(list, func(i, j int) bool { return depth(list[i].Path) < depth(list[j].Path) })
	}
	var dirs []string
	for _, e := range m.Entries {
		if e.Before.Kind == KindDir {
			dirs = append(dirs, e.Path)
		}
	}
	sort.SliceStable(dirs, func(i, j int) bool { return depth(dirs[i]) < depth(dirs[j]) })

	// A command such as chmod -R 000 can leave directories that cannot be
	// entered. Their final modes are set at the end.
	for _, d := range dirs {
		if info, err := os.Lstat(d); err == nil && info.IsDir() && info.Mode().Perm()&0o700 != 0o700 {
			_ = os.Chmod(d, info.Mode()&modeBits|0o700)
		}
	}

	shallowFirst(changes)
	for i := len(changes) - 1; i >= 0; i-- {
		c := changes[i]
		if c.Now.Kind == KindAbsent {
			continue
		}
		inTheWay := c.Kind == ChangeRemove || c.Kind == ChangeRecreate ||
			(c.Kind == ChangeRestore && c.Now.Kind == KindDir)
		if !inTheWay {
			continue
		}
		if err := os.Remove(c.Path); err != nil {
			a.fail(c.Path, err)
			continue
		}
		if c.Kind == ChangeRemove {
			a.res.Done++
		}
	}

	for _, c := range changes {
		if c.Kind != ChangeRecreate || a.failed[c.Path] {
			continue
		}
		if err := os.Mkdir(c.Path, 0o700); err != nil && !errors.Is(err, fs.ErrExist) {
			a.fail(c.Path, err)
			continue
		}
		a.res.Done++
	}

	for _, c := range changes {
		if c.Kind != ChangeRestore || a.failed[c.Path] {
			continue
		}
		ownerErr, err := a.restore(c.Path, c.Want)
		if err != nil {
			a.fail(c.Path, err)
			continue
		}
		a.owner(c.Path, ownerErr)
		a.res.Done++
	}

	for _, c := range changes {
		if c.Kind != ChangeMetadata || c.Want.Kind == KindDir || a.failed[c.Path] {
			continue
		}
		ownerErr, err := setMeta(c.Path, c.Want)
		if err != nil {
			a.fail(c.Path, err)
			continue
		}
		a.owner(c.Path, ownerErr)
		a.res.Done++
	}

	before := make(map[string]State, len(dirs))
	for _, e := range m.Entries {
		if e.Before.Kind == KindDir {
			before[e.Path] = e.Before
		}
	}
	isMetaChange := map[string]bool{}
	for _, c := range changes {
		if c.Kind == ChangeMetadata && c.Want.Kind == KindDir {
			isMetaChange[c.Path] = true
		}
	}
	for i := len(dirs) - 1; i >= 0; i-- {
		d := dirs[i]
		if a.failed[d] {
			continue
		}
		if info, err := os.Lstat(d); err != nil || !info.IsDir() {
			continue
		}
		ownerErr, err := setMeta(d, before[d])
		if err != nil {
			a.fail(d, err)
			continue
		}
		a.owner(d, ownerErr)
		if isMetaChange[d] {
			a.res.Done++
		}
	}
}

// restore writes a file or symlink next to p and renames it into place,
// so that p never holds half a file. A failure to set the owner is
// returned apart, since the content is back all the same.
func (a *applier) restore(p string, want State) (ownerErr, err error) {
	dir := filepath.Dir(p)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, err
	}
	var b [6]byte
	if _, err := rand.Read(b[:]); err != nil {
		return nil, err
	}
	tmp := filepath.Join(dir, ".undoguard-"+hex.EncodeToString(b[:])+".tmp")
	if want.Kind == KindSymlink {
		err = os.Symlink(want.Link, tmp)
	} else {
		err = a.store.WriteTo(want.Hash, tmp)
	}
	if err != nil {
		return nil, err
	}
	ownerErr, err = setMeta(tmp, want)
	if err == nil {
		err = os.Rename(tmp, p)
	}
	if err != nil {
		_ = os.Remove(tmp)
		return nil, err
	}
	return ownerErr, nil
}

// setMeta gives p the owner, mode and modification time in want. The
// owner goes first, because changing it clears the setuid and setgid
// bits. Only root may give a file away, so a failure to set the owner is
// returned apart and the rest is still done. Symlinks keep their mode
// and times, which Go cannot set without following them.
func setMeta(p string, want State) (ownerErr, err error) {
	now, err := stateOf(p)
	if err != nil {
		return nil, err
	}
	ownerErr = chown(p, want, now)
	if want.Kind == KindSymlink {
		return ownerErr, nil
	}
	if err := os.Chmod(p, fs.FileMode(want.Mode)); err != nil {
		return ownerErr, err
	}
	mt := time.Unix(0, want.MTime)
	return ownerErr, os.Chtimes(p, mt, mt)
}

// Latest returns the newest snapshot that undo would change something
// for: not undone, not taken by an undo, not matching the disk already,
// and from session when one is given.
func Latest(root, session string) (*Manifest, error) {
	ids, err := IDs(root)
	if err != nil {
		return nil, err
	}
	for _, id := range ids {
		m, err := Load(root, id)
		if err != nil || m.Restored != nil || m.Undoes != "" {
			continue
		}
		if session != "" && m.Session != session {
			continue
		}
		if PlanRestore(m).Empty() {
			continue
		}
		return m, nil
	}
	return nil, errors.New("there is no snapshot left to undo")
}

// LatestN returns the newest n snapshots that Latest would pick one after
// the other: not undone, not taken by an undo, and with something left
// to restore. They come newest first.
func LatestN(root string, n int) ([]*Manifest, error) {
	ids, err := IDs(root)
	if err != nil {
		return nil, err
	}
	var out []*Manifest
	for _, id := range ids {
		if len(out) == n {
			break
		}
		m, err := Load(root, id)
		if err != nil || m.Restored != nil || m.Undoes != "" || PlanRestore(m).Empty() {
			continue
		}
		out = append(out, m)
	}
	return out, nil
}

// SessionSnapshots returns the snapshots of a session that are not undone
// and were not taken by an undo, newest first.
func SessionSnapshots(root, session string) ([]*Manifest, error) {
	list, err := List(root)
	if err != nil {
		return nil, err
	}
	var out []*Manifest
	for _, m := range list {
		if m.Session == session && m.Restored == nil && m.Undoes == "" {
			out = append(out, m)
		}
	}
	return out, nil
}
