//go:build !linux

package external

import "io/fs"

// changeTime is 0 off Linux, where Wings doesn't run as a daemon; the
// modification time, size and inode still change on an edit.
func changeTime(fs.FileInfo) int64 { return 0 }
