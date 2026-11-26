//go:build !linux

package filesystem

import "os"

// renameNoReplace is root.Rename on platforms without renameat2. The caller's
// check that to does not exist is all that stops it being replaced.
func renameNoReplace(root *os.Root, from, to string) error {
	return root.Rename(from, to)
}
