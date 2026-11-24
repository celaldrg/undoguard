// Package snapshot records what a command is about to change, restores it
// on request, and trims old snapshots.
//
// A snapshot is a manifest of entries, one per path, with the state before
// the command ran and, once the command has finished, the state after it.
// File contents live in the content-addressed store. Undo compares each
// path with both states: it restores paths that still look the way the
// command left them, and refuses to touch paths that changed since,
// unless forced.
package snapshot

import (
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"syscall"
	"time"
)

// FormatVersion is the version of the manifest format.
const FormatVersion = 1

// Kind is the type of a path.
type Kind string

// The kinds of path.
const (
	KindAbsent  Kind = "absent"
	KindFile    Kind = "file"
	KindDir     Kind = "dir"
	KindSymlink Kind = "symlink"
	// KindOther is a socket, device or named pipe. They are recorded but
	// not restored.
	KindOther Kind = "other"
)

// State is what was at a path at one moment.
type State struct {
	Kind Kind `json:"kind"`
	// Mode holds the permission bits plus setuid, setgid and sticky.
	Mode uint32 `json:"mode,omitempty"`
	Size int64  `json:"size,omitempty"`
	// MTime is the modification time in nanoseconds since the epoch.
	MTime int64 `json:"mtime,omitempty"`
	// UID and GID are -1 where ownership is not known.
	UID int `json:"uid"`
	GID int `json:"gid"`
	// Hash names the stored content of a file.
	Hash string `json:"hash,omitempty"`
	// Link is the target of a symlink.
	Link string `json:"link,omitempty"`
	// Skipped says why a file's content was not stored.
	Skipped string `json:"skipped,omitempty"`
}

// Entry is one path in a snapshot.
type Entry struct {
	Path   string `json:"path"`
	Before State  `json:"before"`
	// After is the state once the command finished. It is nil until the
	// command is known to have finished.
	After *State `json:"after,omitempty"`
}

// Root is one path the guard asked to snapshot.
type Root struct {
	Path      string `json:"path"`
	Recursive bool   `json:"recursive,omitempty"`
	Follow    bool   `json:"follow,omitempty"`
	// Git narrows a recursive root to the files in one git state.
	Git  string `json:"git,omitempty"`
	Repo string `json:"repo,omitempty"`
}

// Manifest is one snapshot.
type Manifest struct {
	Version int       `json:"version"`
	ID      string    `json:"id"`
	Created time.Time `json:"created"`
	Session string    `json:"session,omitempty"`
	Agent   string    `json:"agent,omitempty"`
	Command string    `json:"command"`
	Dir     string    `json:"dir,omitempty"`
	Roots   []Root    `json:"roots"`
	Entries []Entry   `json:"entries"`
	// Bytes is the size of the stored contents.
	Bytes int64 `json:"bytes"`
	// Cloned counts files stored as reflinks.
	Cloned int `json:"cloned,omitempty"`
	// Finished is when the state after the command was recorded.
	Finished *time.Time `json:"finished,omitempty"`
	// Restored is when the snapshot was undone.
	Restored *time.Time `json:"restored,omitempty"`
	// RestoredBy is the snapshot of the state the undo replaced.
	RestoredBy string `json:"restored_by,omitempty"`
	// Undoes is set on snapshots taken by an undo, to the snapshot
	// being undone.
	Undoes string `json:"undoes,omitempty"`
}

// Files counts the entries with stored content.
func (m *Manifest) Files() int {
	n := 0
	for _, e := range m.Entries {
		if e.Before.Hash != "" {
			n++
		}
	}
	return n
}

// Status describes where the snapshot stands.
func (m *Manifest) Status() string {
	switch {
	case m.Restored != nil:
		return "undone"
	case m.Undoes != "":
		return "taken by undo"
	case m.Finished == nil:
		return "not confirmed"
	}
	return "ready"
}

var idPattern = regexp.MustCompile(`^[0-9]{8}-[0-9]{6}-[0-9a-f]{6}$`)

func newID(now time.Time) (string, error) {
	// The last part starts with the fraction of the second in 1/65536ths
	// and ends with a random byte, so that ids sort in the order they
	// were taken even within one second: undo picks the newest by id.
	var b [1]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	now = now.UTC()
	frac := uint64(now.Nanosecond()) * 65536 / 1e9
	return fmt.Sprintf("%s-%04x%02x", now.Format("20060102-150405"), frac, b[0]), nil
}

// LockPath is the lock that snapshots share and gc takes exclusively.
func LockPath(root string) string {
	return filepath.Join(root, "lock")
}

func snapshotsDir(root string) string {
	return filepath.Join(root, "snapshots")
}

func manifestPath(root, id string) string {
	return filepath.Join(snapshotsDir(root), id, "manifest.json")
}

func (m *Manifest) save(root string) error {
	dir := filepath.Join(snapshotsDir(root), m.ID)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	data, err := json.MarshalIndent(m, "", " ")
	if err != nil {
		return err
	}
	tmp := manifestPath(root, m.ID) + ".tmp"
	if err := os.WriteFile(tmp, append(data, '\n'), 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, manifestPath(root, m.ID))
}

// Load reads one snapshot by its full id.
func Load(root, id string) (*Manifest, error) {
	if !idPattern.MatchString(id) {
		return nil, fmt.Errorf("%q is not a snapshot id", id)
	}
	data, err := os.ReadFile(manifestPath(root, id))
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil, fmt.Errorf("no snapshot %s", id)
		}
		return nil, err
	}
	var m Manifest
	if err := json.Unmarshal(data, &m); err != nil {
		return nil, fmt.Errorf("snapshot %s: %w", id, err)
	}
	if m.Version > FormatVersion {
		return nil, fmt.Errorf("snapshot %s was written by a newer undoguard", id)
	}
	return &m, nil
}

// IDs lists the snapshot ids, newest first. An id starts with the UTC
// time it was taken, so the order of the names is the order in time.
func IDs(root string) ([]string, error) {
	des, err := os.ReadDir(snapshotsDir(root))
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var ids []string
	for _, de := range des {
		if de.IsDir() && idPattern.MatchString(de.Name()) {
			ids = append(ids, de.Name())
		}
	}
	sort.Sort(sort.Reverse(sort.StringSlice(ids)))
	return ids, nil
}

// idTime returns the time encoded in an id.
func idTime(id string) time.Time {
	t, err := time.Parse("20060102-150405", id[:15])
	if err != nil {
		return time.Time{}
	}
	return t
}

// List returns every snapshot, newest first. Unreadable ones are skipped.
func List(root string) ([]*Manifest, error) {
	ids, err := IDs(root)
	if err != nil {
		return nil, err
	}
	var out []*Manifest
	for _, id := range ids {
		m, err := Load(root, id)
		if err != nil {
			continue
		}
		out = append(out, m)
	}
	return out, nil
}

// Find returns the snapshot whose id is prefix, starts with it, or ends
// with it after the last dash.
func Find(root, prefix string) (*Manifest, error) {
	if idPattern.MatchString(prefix) {
		return Load(root, prefix)
	}
	list, err := List(root)
	if err != nil {
		return nil, err
	}
	var found []*Manifest
	for _, m := range list {
		if strings.HasPrefix(m.ID, prefix) || strings.HasSuffix(m.ID, "-"+prefix) {
			found = append(found, m)
		}
	}
	switch len(found) {
	case 0:
		return nil, fmt.Errorf("no snapshot matches %q", prefix)
	case 1:
		return found[0], nil
	}
	return nil, fmt.Errorf("%q matches %d snapshots; give more of the id", prefix, len(found))
}

const modeBits = fs.ModePerm | fs.ModeSetuid | fs.ModeSetgid | fs.ModeSticky

// stateOf reads the state of a path without following a final symlink.
func stateOf(p string) (State, error) {
	info, err := os.Lstat(p)
	if errors.Is(err, fs.ErrNotExist) || errors.Is(err, syscall.ENOTDIR) {
		return State{Kind: KindAbsent, UID: -1, GID: -1}, nil
	}
	if err != nil {
		return State{}, err
	}
	return stateFromInfo(p, info), nil
}

func stateFromInfo(p string, info fs.FileInfo) State {
	s := State{Mode: uint32(info.Mode() & modeBits), MTime: info.ModTime().UnixNano()}
	s.UID, s.GID = owner(info)
	t := info.Mode().Type()
	switch {
	case t == 0:
		s.Kind, s.Size = KindFile, info.Size()
	case t&fs.ModeDir != 0:
		s.Kind = KindDir
	case t&fs.ModeSymlink != 0:
		s.Kind = KindSymlink
		s.Link, _ = os.Readlink(p)
	default:
		s.Kind = KindOther
	}
	return s
}

// same reports whether two states look alike. Files compare by size,
// modification time and mode, as rsync and git do, and directories by
// mode only, since their times change whenever their content does.
func same(a, b State) bool {
	if a.Kind != b.Kind {
		return false
	}
	if a.Kind == KindAbsent {
		return true
	}
	if a.Kind != KindSymlink && a.Mode != b.Mode {
		return false
	}
	if a.UID >= 0 && b.UID >= 0 && (a.UID != b.UID || a.GID != b.GID) {
		return false
	}
	switch a.Kind {
	case KindFile:
		return a.Size == b.Size && a.MTime == b.MTime
	case KindSymlink:
		return a.Link == b.Link
	}
	return true
}
