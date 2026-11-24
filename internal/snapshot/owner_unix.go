//go:build unix

package snapshot

import (
	"io/fs"
	"os"
	"syscall"
)

func owner(info fs.FileInfo) (int, int) {
	if st, ok := info.Sys().(*syscall.Stat_t); ok {
		return int(st.Uid), int(st.Gid)
	}
	return -1, -1
}

// chown gives p the owner in s, without following a symlink, when it
// differs from now. Only root may give files away, so for other users a
// failure is expected and only reported.
func chown(p string, s, now State) error {
	if s.UID < 0 || (s.UID == now.UID && s.GID == now.GID) {
		return nil
	}
	return os.Lchown(p, s.UID, s.GID)
}
