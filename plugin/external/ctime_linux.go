package external

import (
	"io/fs"
	"syscall"
)

// changeTime returns fi's inode change time in nanoseconds. Unlike the
// modification time, a process can't set it back.
func changeTime(fi fs.FileInfo) int64 {
	if st, ok := fi.Sys().(*syscall.Stat_t); ok {
		return st.Ctim.Nano()
	}
	return 0
}
