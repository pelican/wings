package filesystem

import (
	"errors"
	"os"
	"path"

	"golang.org/x/sys/unix"
)

// renameNoReplace renames from to to inside root, failing with an error that
// matches os.ErrExist if something is already at to, even if it was created
// after the caller checked. It uses renameat2(RENAME_NOREPLACE) on the parent
// folders, opened through root so that they stay inside it, and falls back to
// root.Rename where the kernel or filesystem does not support that flag.
func renameNoReplace(root *os.Root, from, to string) error {
	fromBase, toBase := path.Base(from), path.Base(to)
	if !plainName(fromBase) || !plainName(toBase) {
		return root.Rename(from, to)
	}
	// Errors are returned as an *os.LinkError, as root.Rename returns them.
	linkErr := func(err error) error {
		var pe *os.PathError
		if errors.As(err, &pe) {
			err = pe.Err
		}
		return &os.LinkError{Op: "renameat", Old: from, New: to, Err: err}
	}
	fromDir, err := root.Open(path.Dir(from))
	if err != nil {
		return linkErr(err)
	}
	defer fromDir.Close()
	toDir, err := root.Open(path.Dir(to))
	if err != nil {
		return linkErr(err)
	}
	defer toDir.Close()

	err = unix.Renameat2(int(fromDir.Fd()), fromBase, int(toDir.Fd()), toBase, unix.RENAME_NOREPLACE)
	if errors.Is(err, unix.ENOSYS) || errors.Is(err, unix.EINVAL) {
		return root.Rename(from, to)
	}
	if err != nil {
		return linkErr(err)
	}
	return nil
}

// plainName reports whether name is a single path element that renameat2 can
// be given relative to its folder.
func plainName(name string) bool {
	return name != "." && name != ".." && name != "/" && name != ""
}
