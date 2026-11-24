//go:build !unix

package snapshot

import "io/fs"

func owner(fs.FileInfo) (int, int) {
	return -1, -1
}

func chown(string, State, State) error {
	return nil
}
