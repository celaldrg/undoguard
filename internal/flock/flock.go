// Package flock holds advisory file locks between undoguard processes:
// several hooks may run at once, and gc must not delete stored files that
// a snapshot in progress is about to reference.
package flock

import (
	"os"
	"path/filepath"
)

// Lock is a held lock.
type Lock struct {
	f *os.File
}

// Shared takes a shared lock on path, creating the file if needed.
func Shared(path string) (*Lock, error) {
	return take(path, false)
}

// Exclusive takes an exclusive lock on path, creating the file if needed.
func Exclusive(path string) (*Lock, error) {
	return take(path, true)
}

func take(path string, exclusive bool) (*Lock, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, err
	}
	f, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE, 0o600)
	if err != nil {
		return nil, err
	}
	if err := lock(f, exclusive); err != nil {
		f.Close()
		return nil, err
	}
	return &Lock{f: f}, nil
}

// TryExclusive takes an exclusive lock on path without waiting. It
// returns a nil Lock and no error when another process holds the lock.
func TryExclusive(path string) (*Lock, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, err
	}
	f, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE, 0o600)
	if err != nil {
		return nil, err
	}
	ok, err := tryLock(f)
	if err != nil || !ok {
		f.Close()
		return nil, err
	}
	return &Lock{f: f}, nil
}

// Release drops the lock.
func (l *Lock) Release() {
	if l == nil || l.f == nil {
		return
	}
	unlock(l.f)
	l.f.Close()
	l.f = nil
}
