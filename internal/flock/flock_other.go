//go:build !unix

package flock

import "os"

// Locking is a no-op where flock is not available. undoguard supports
// Linux and macOS; this file only keeps the package building elsewhere.
func lock(*os.File, bool) error { return nil }

func unlock(*os.File) {}

func tryLock(*os.File) (bool, error) { return true, nil }
