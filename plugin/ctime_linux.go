package plugin

import (
	"io/fs"
	"syscall"
)

// changeTime returns fi's inode change time in nanoseconds. Unlike the
// modification time, it can't be set back.
func changeTime(fi fs.FileInfo) int64 {
	if st, ok := fi.Sys().(*syscall.Stat_t); ok {
		return st.Ctim.Nano()
	}
	return 0
}
