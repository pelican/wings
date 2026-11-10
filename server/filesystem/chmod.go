package filesystem

import (
	fs2 "io/fs"
	"os"
	"path"
	"time"

	"emperror.dev/errors"

	"github.com/pelican/wings/config"
)

// Chmod changes the mode of the given file or directory. os.Root follows a
// symlink at p, so the mode of a denylisted file cannot be changed through a
// link to it. Changing it by its own name is left to the caller to allow or
// refuse, as before.
func (fs *Filesystem) Chmod(p string, mode os.FileMode) error {
	if err := fs.isIgnoredThroughLink(p); err != nil {
		return err
	}
	root, err := fs.openRoot()
	if err != nil {
		return err
	}
	if err := root.Chmod(normalize(p), mode); err != nil {
		return errors.Wrap(err, "server/filesystem: chmod: failed to chmod path")
	}

	return nil
}

// FileModeFromUnix converts a Unix mode such as 0o4755, as sent by the Panel,
// to an os.FileMode. Go keeps setuid, setgid and sticky in other bits than Unix
// does, so a plain os.FileMode(0o4755) loses them. Bits above 0o7777 are
// ignored, as chmod(2) does.
func FileModeFromUnix(m uint64) os.FileMode {
	mode := os.FileMode(m & 0o777)
	if m&0o4000 != 0 {
		mode |= os.ModeSetuid
	}
	if m&0o2000 != 0 {
		mode |= os.ModeSetgid
	}
	if m&0o1000 != 0 {
		mode |= os.ModeSticky
	}
	return mode
}

// chownFile sets the owner of a single file or directory to the server user. If
// the path is a symlink the link itself is changed, not the file it points to.
func (fs *Filesystem) chownFile(p string) error {
	root, err := fs.openRoot()
	if err != nil {
		return err
	}
	if fs.isTest {
		return nil
	}

	uid := config.Get().System.User.Uid
	gid := config.Get().System.User.Gid
	if err := root.Lchown(normalize(p), uid, gid); err != nil {
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
	root, err := fs.openRoot()
	if err != nil {
		return err
	}
	p = normalize(p)
	st, err := root.Lstat(p)
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
	return fs2.WalkDir(rootDirFS{root}, p, func(name string, _ fs2.DirEntry, err error) error {
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
	root, err := fs.openRoot()
	if err != nil {
		return err
	}
	if err := root.Chtimes(normalize(p), atime, mtime); err != nil {
		return errors.Wrap(err, "server/filesystem: chtimes: failed to chtimes path")
	}

	return nil
}
