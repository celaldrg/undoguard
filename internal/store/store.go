// Package store keeps file contents in a content-addressed directory. Each
// object is named by the SHA-256 of its bytes, so content that is
// snapshotted many times is stored once. Where the file system supports
// it, an object is a reflink of the original (FICLONE on Linux, clonefile
// on macOS), which shares the original's blocks until one side changes.
package store

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
)

// Store is a content-addressed object directory.
type Store struct {
	objects string
	tmp     string
}

// Open opens or creates the store below root.
func Open(root string) (*Store, error) {
	s := &Store{objects: filepath.Join(root, "objects"), tmp: filepath.Join(root, "tmp")}
	for _, d := range []string{s.objects, s.tmp} {
		if err := os.MkdirAll(d, 0o700); err != nil {
			return nil, err
		}
	}
	return s, nil
}

// ValidHash reports whether h looks like an object name.
func ValidHash(h string) bool {
	if len(h) != 64 {
		return false
	}
	_, err := hex.DecodeString(h)
	return err == nil && strings.ToLower(h) == h
}

func (s *Store) path(hash string) (string, error) {
	if !ValidHash(hash) {
		return "", fmt.Errorf("bad object name %q", hash)
	}
	return filepath.Join(s.objects, hash[:2], hash[2:]), nil
}

func (s *Store) tempName() (string, error) {
	var b [12]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	return filepath.Join(s.tmp, "obj-"+hex.EncodeToString(b[:])), nil
}

// Put stores the content of the regular file src and returns its hash and
// size. cloned reports whether the object shares blocks with src.
func (s *Store) Put(src string) (hash string, size int64, cloned bool, err error) {
	tmp, err := s.tempName()
	if err != nil {
		return "", 0, false, err
	}
	defer os.Remove(tmp)
	if cloneFile(src, tmp) == nil {
		cloned = true
		hash, size, err = hashFile(tmp)
	} else {
		hash, size, err = copyHashed(src, tmp)
	}
	if err != nil {
		return "", 0, false, err
	}
	obj, err := s.path(hash)
	if err != nil {
		return "", 0, false, err
	}
	if _, err := os.Lstat(obj); err == nil {
		return hash, size, cloned, nil
	}
	if err := os.MkdirAll(filepath.Dir(obj), 0o700); err != nil {
		return "", 0, false, err
	}
	if err := os.Chmod(tmp, 0o400); err != nil {
		return "", 0, false, err
	}
	if err := os.Rename(tmp, obj); err != nil {
		return "", 0, false, err
	}
	return hash, size, cloned, nil
}

// Has reports whether the object exists.
func (s *Store) Has(hash string) bool {
	p, err := s.path(hash)
	if err != nil {
		return false
	}
	_, err = os.Lstat(p)
	return err == nil
}

// Open opens an object for reading.
func (s *Store) Open(hash string) (*os.File, error) {
	p, err := s.path(hash)
	if err != nil {
		return nil, err
	}
	return os.Open(p)
}

// WriteTo creates dst, which must not exist, with the object's content.
// The new file is only readable by the owner until the caller sets its
// mode.
func (s *Store) WriteTo(hash, dst string) error {
	p, err := s.path(hash)
	if err != nil {
		return err
	}
	if _, err := os.Lstat(p); err != nil {
		return fmt.Errorf("stored copy is missing: %w", err)
	}
	if cloneFile(p, dst) == nil {
		return os.Chmod(dst, 0o600)
	}
	// copyHashed creates dst exclusively and removes it again if the copy
	// fails, so an error here never touches a file that was already there.
	got, _, err := copyHashed(p, dst)
	if err != nil {
		return err
	}
	if got != hash {
		os.Remove(dst)
		return fmt.Errorf("stored copy %s is corrupt", hash[:12])
	}
	return nil
}

// Remove deletes an object.
func (s *Store) Remove(hash string) error {
	p, err := s.path(hash)
	if err != nil {
		return err
	}
	err = os.Remove(p)
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	return err
}

// Walk calls fn for every object.
func (s *Store) Walk(fn func(hash string, size int64) error) error {
	return filepath.WalkDir(s.objects, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			if errors.Is(err, fs.ErrNotExist) {
				return nil
			}
			return err
		}
		if d.IsDir() {
			return nil
		}
		rel, err := filepath.Rel(s.objects, p)
		if err != nil {
			return err
		}
		h := strings.ReplaceAll(filepath.ToSlash(rel), "/", "")
		if !ValidHash(h) {
			return nil
		}
		info, err := d.Info()
		if err != nil {
			return nil //nolint:nilerr // removed by a concurrent gc: skip it
		}
		return fn(h, info.Size())
	})
}

// CleanTemp removes leftovers of interrupted writes.
func (s *Store) CleanTemp() {
	entries, _ := os.ReadDir(s.tmp)
	for _, e := range entries {
		os.Remove(filepath.Join(s.tmp, e.Name()))
	}
}

func hashFile(p string) (string, int64, error) {
	f, err := os.Open(p)
	if err != nil {
		return "", 0, err
	}
	defer f.Close()
	h := sha256.New()
	n, err := io.Copy(h, f)
	if err != nil {
		return "", 0, err
	}
	return hex.EncodeToString(h.Sum(nil)), n, nil
}

// copyHashed copies src to the new file dst and hashes what it wrote. dst
// must not exist; if the copy fails, the file it created is removed.
func copyHashed(src, dst string) (string, int64, error) {
	in, err := os.Open(src)
	if err != nil {
		return "", 0, err
	}
	defer in.Close()
	out, err := os.OpenFile(dst, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return "", 0, err
	}
	h := sha256.New()
	n, err := io.Copy(io.MultiWriter(out, h), in)
	if err != nil {
		out.Close()
		os.Remove(dst)
		return "", 0, err
	}
	if err := out.Close(); err != nil {
		os.Remove(dst)
		return "", 0, err
	}
	return hex.EncodeToString(h.Sum(nil)), n, nil
}
