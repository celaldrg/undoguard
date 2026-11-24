//go:build darwin

package store

import (
	"fmt"
	"os"

	"golang.org/x/sys/unix"
)

// cloneFile creates dst as a copy-on-write clone of src. It works on APFS
// and fails elsewhere.
func cloneFile(src, dst string) error {
	if err := unix.Clonefile(src, dst, unix.CLONE_NOFOLLOW); err != nil {
		return err
	}
	// With CLONE_NOFOLLOW a symlink is cloned as a symlink. src should be
	// a regular file, so anything else means it was replaced while it was
	// read; a cloned link must not reach a later chmod, which would follow
	// it to some other file.
	info, err := os.Lstat(dst)
	if err == nil && info.Mode().IsRegular() {
		return nil
	}
	os.Remove(dst)
	if err == nil {
		err = fmt.Errorf("%s is not a regular file", src)
	}
	return err
}
