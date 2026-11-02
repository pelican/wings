package filesystem

import (
	fs2 "io/fs"
	"os"
	"path"
	"time"

	"emperror.dev/errors"

	"github.com/pelican/wings/config"
)

// Chmod changes the mode of the given file or directory.
func (fs *Filesystem) Chmod(p string, mode os.FileMode) error {
	if err := fs.root.Chmod(normalize(p), mode); err != nil {
		return errors.Wrap(err, "server/filesystem: chmod: failed to chmod path")
	}

	return nil
}

// chownFile sets the owner of a single file or directory to the server user. If
// the path is a symlink the link itself is changed, not the file it points to.
func (fs *Filesystem) chownFile(p string) error {
	if fs.isTest {
		return nil
	}

	uid := config.Get().System.User.Uid
	gid := config.Get().System.User.Gid
	if err := fs.root.Lchown(normalize(p), uid, gid); err != nil {
		return errors.Wrap(err, "server/filesystem: chown: failed to chown path")
	}
	return nil
}

// Chown recursively iterates over a file or directory and sets the owner on all the
// underlying files. Iterate over all the files and directories. If it is a file go ahead
// and perform the chown operation. Otherwise dig deeper into the directory until we've run
// out of directories to dig into.
//
// The walk happens through the os.Root instance and never follows symlinks, so it
// cannot change the owner of anything outside the server's data directory.
func (fs *Filesystem) Chown(p string) error {
	p = normalize(p)
	st, err := fs.root.Lstat(p)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		return errors.Wrap(err, "server/filesystem: chown: failed to stat path")
	}

	if err := fs.chownFile(p); err != nil {
		return err
	}

	// If this is not a directory, we can now return from the function; there is nothing
	// left that we need to do.
	if !st.IsDir() {
		return nil
	}

	// If this was a directory, begin walking over its contents recursively and ensure that all
	// the subfiles and directories get their owner updated as well.
	return fs2.WalkDir(fs.root.FS(), p, func(name string, _ fs2.DirEntry, err error) error {
		if err != nil {
			if errors.Is(err, fs2.ErrNotExist) {
				return nil
			}
			return errors.Wrap(err, "server/filesystem: chown: failed to walk directory")
		}
		if path.Clean(name) == p {
			return nil
		}
		if err := fs.chownFile(name); err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
		return nil
	})
}

// Chtimes changes the access and modification times of the given file.
func (fs *Filesystem) Chtimes(p string, atime, mtime time.Time) error {
	if err := fs.root.Chtimes(normalize(p), atime, mtime); err != nil {
		return errors.Wrap(err, "server/filesystem: chtimes: failed to chtimes path")
	}

	return nil
}
