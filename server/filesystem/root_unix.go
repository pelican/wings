//go:build unix

package filesystem

import (
	"os"
	"syscall"
)

// isUnlinked reports whether the file described by st has been deleted, which is
// the case for a directory that is still held open after it was removed. The
// inode number of a deleted directory can be handed straight to a new one, so
// comparing the two with os.SameFile is not enough on its own.
func isUnlinked(st os.FileInfo) bool {
	if s, ok := st.Sys().(*syscall.Stat_t); ok {
		return s.Nlink == 0
	}
	return false
}
