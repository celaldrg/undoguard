//go:build unix

package store

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func newStore(t *testing.T) *Store {
	t.Helper()
	s, err := Open(filepath.Join(t.TempDir(), "store"))
	if err != nil {
		t.Fatal(err)
	}
	return s
}

// writeFile creates name with content in a fresh directory.
func writeFile(t *testing.T, name, content string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), name)
	if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	return p
}

func hashOf(content string) string {
	sum := sha256.Sum256([]byte(content))
	return hex.EncodeToString(sum[:])
}

func listObjects(t *testing.T, s *Store) map[string]int64 {
	t.Helper()
	got := map[string]int64{}
	err := s.Walk(func(hash string, size int64) error {
		got[hash] = size
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return got
}

func checkTempEmpty(t *testing.T, s *Store) {
	t.Helper()
	left, err := os.ReadDir(s.tmp)
	if err != nil || len(left) != 0 {
		t.Errorf("tmp holds %v (%v)", left, err)
	}
}

func TestPutDeduplicates(t *testing.T) {
	s := newStore(t)
	const same, other = "same content\n", "other content\n"

	first, size, _, err := s.Put(writeFile(t, "a", same))
	if err != nil {
		t.Fatal(err)
	}
	if first != hashOf(same) || size != int64(len(same)) {
		t.Errorf("Put = %s, %d, want %s, %d", first, size, hashOf(same), len(same))
	}
	second, size, _, err := s.Put(writeFile(t, "b", same))
	if err != nil {
		t.Fatal(err)
	}
	if second != first || size != int64(len(same)) {
		t.Errorf("second Put = %s, %d, want %s, %d", second, size, first, len(same))
	}
	if got := listObjects(t, s); len(got) != 1 || got[first] != int64(len(same)) {
		t.Errorf("objects after two identical files = %v, want one", got)
	}

	third, _, _, err := s.Put(writeFile(t, "c", other))
	if err != nil {
		t.Fatal(err)
	}
	if third != hashOf(other) {
		t.Errorf("hash of other content = %s, want %s", third, hashOf(other))
	}
	if got := listObjects(t, s); len(got) != 2 || got[third] != int64(len(other)) {
		t.Errorf("objects = %v, want two", got)
	}

	// Objects are read-only, and nothing is left behind in tmp.
	p, err := s.path(first)
	if err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(p)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o400 {
		t.Errorf("object mode = %v, want 0400", info.Mode().Perm())
	}
	checkTempEmpty(t, s)
}

func TestPutMissingSource(t *testing.T) {
	s := newStore(t)
	if _, _, _, err := s.Put(filepath.Join(t.TempDir(), "missing")); err == nil {
		t.Error("Put of a missing file succeeded")
	}
	if got := listObjects(t, s); len(got) != 0 {
		t.Errorf("objects = %v, want none", got)
	}
	checkTempEmpty(t, s)
}

func TestOpenAndWriteTo(t *testing.T) {
	s := newStore(t)
	const content = "line one\nline two\n"
	src := writeFile(t, "src", content)
	if err := os.Chmod(src, 0o755); err != nil {
		t.Fatal(err)
	}
	hash, _, _, err := s.Put(src)
	if err != nil {
		t.Fatal(err)
	}

	f, err := s.Open(hash)
	if err != nil {
		t.Fatal(err)
	}
	data, err := io.ReadAll(f)
	f.Close()
	if err != nil || string(data) != content {
		t.Errorf("Open read %q (%v), want %q", data, err, content)
	}

	dir := t.TempDir()
	dst := filepath.Join(dir, "dst")
	if err := s.WriteTo(hash, dst); err != nil {
		t.Fatal(err)
	}
	data, err = os.ReadFile(dst)
	if err != nil || string(data) != content {
		t.Errorf("WriteTo wrote %q (%v), want %q", data, err, content)
	}
	info, err := os.Stat(dst)
	if err != nil {
		t.Fatal(err)
	}
	// The caller sets the final mode; until then only the owner has access.
	if info.Mode().Perm() != 0o600 {
		t.Errorf("mode of the written file = %v, want 0600", info.Mode().Perm())
	}
	if err := os.WriteFile(dst, []byte("someone else's file\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := s.WriteTo(hash, dst); err == nil {
		t.Error("WriteTo over an existing file succeeded")
	}
	// A failed WriteTo must leave a file that was already there alone.
	if data, err := os.ReadFile(dst); err != nil || string(data) != "someone else's file\n" {
		t.Errorf("WriteTo over an existing file left %q (%v)", data, err)
	}

	missing := hashOf("never stored\n")
	absent := filepath.Join(dir, "absent")
	if err := s.WriteTo(missing, absent); err == nil {
		t.Error("WriteTo of a missing object succeeded")
	}
	if _, err := os.Lstat(absent); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("WriteTo of a missing object created %s (%v)", absent, err)
	}
	if _, err := s.Open(missing); err == nil {
		t.Error("Open of a missing object succeeded")
	}
	if err := s.WriteTo("not-a-hash", absent); err == nil {
		t.Error("WriteTo accepted a bad object name")
	}
	if _, err := s.Open("not-a-hash"); err == nil {
		t.Error("Open accepted a bad object name")
	}
}

func TestHasAndRemove(t *testing.T) {
	s := newStore(t)
	const content = "to be removed\n"
	hash := hashOf(content)
	if s.Has(hash) {
		t.Fatal("Has reports an object that was never stored")
	}
	if _, _, _, err := s.Put(writeFile(t, "a", content)); err != nil {
		t.Fatal(err)
	}
	if !s.Has(hash) {
		t.Fatal("Has misses a stored object")
	}
	if err := s.Remove(hash); err != nil {
		t.Fatal(err)
	}
	if s.Has(hash) {
		t.Error("Has reports a removed object")
	}
	if err := s.Remove(hash); err != nil {
		t.Errorf("removing a missing object: %v", err)
	}
	if err := s.Remove("not-a-hash"); err == nil {
		t.Error("Remove accepted a bad object name")
	}
	if s.Has("not-a-hash") {
		t.Error("Has accepted a bad object name")
	}
	if got := listObjects(t, s); len(got) != 0 {
		t.Errorf("objects = %v, want none", got)
	}
}

func TestValidHash(t *testing.T) {
	cases := []struct {
		in   string
		want bool
	}{
		{strings.Repeat("ab", 32), true},
		{strings.Repeat("0", 64), true},
		{hashOf("x"), true},
		{strings.Repeat("AB", 32), false},
		{strings.Repeat("ab", 32)[:63], false},
		{strings.Repeat("ab", 32) + "0", false},
		{strings.Repeat("g", 64), false},
		{"../" + strings.Repeat("a", 61), false},
		{"", false},
	}
	for _, c := range cases {
		if got := ValidHash(c.in); got != c.want {
			t.Errorf("ValidHash(%q) = %v, want %v", c.in, got, c.want)
		}
	}
}

func TestWalk(t *testing.T) {
	s := newStore(t)
	const content = "content\n"
	hash, _, _, err := s.Put(writeFile(t, "a", content))
	if err != nil {
		t.Fatal(err)
	}
	// A file whose path does not spell an object name is not an object.
	stray := filepath.Join(s.objects, "zz")
	if err := os.MkdirAll(stray, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(stray, "notahash"), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	if got := listObjects(t, s); len(got) != 1 || got[hash] != int64(len(content)) {
		t.Errorf("objects = %v, want only %s", got, hash)
	}

	// A store without an object directory has no objects.
	if err := os.RemoveAll(s.objects); err != nil {
		t.Fatal(err)
	}
	if got := listObjects(t, s); len(got) != 0 {
		t.Errorf("objects = %v, want none", got)
	}
}

func TestCleanTemp(t *testing.T) {
	s := newStore(t)
	hash, _, _, err := s.Put(writeFile(t, "a", "kept\n"))
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"obj-000000000000000000000000", "obj-ffffffffffffffffffffffff"} {
		if err := os.WriteFile(filepath.Join(s.tmp, name), []byte("partial"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	s.CleanTemp()
	checkTempEmpty(t, s)
	if !s.Has(hash) {
		t.Error("CleanTemp removed an object")
	}
}
