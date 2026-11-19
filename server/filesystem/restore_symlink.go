package filesystem

import (
	"crypto/rand"
	"encoding/hex"
	"os"
	"path/filepath"
	"strings"

	"emperror.dev/errors"
)

// RestoreSymlink creates name as a symbolic link to target, creating any missing
// parent directories and replacing an existing file or link at that path. It is
// used when restoring archives that contain symbolic links. The target is stored
// as-is: every path Wings opens is resolved inside the server's os.Root, so a
// link pointing outside of it cannot be followed by Wings.
//
// The link is created and chowned under a temporary name and then renamed into
// place, so an existing entry is only replaced once the new link is ready. A
// directory is never replaced.
func (fs *Filesystem) RestoreSymlink(name, target string) error {
	root, err := fs.openRoot()
	if err != nil {
		return err
	}
	name = normalize(name)
	if name == "." {
		return &Error{code: ErrCodeIsDirectory, resolved: name}
	}
	if target == "" {
		return errors.New("server/filesystem: restore symlink: empty link target")
	}

	// os.Root follows symlinks in the parent directories when it creates
	// directories and the link, so a link restored through a symlinked directory
	// would land somewhere other than the archive says. Refuse that, as Symlink
	// does. Check before creating the missing parents, so a refused restore does
	// not leave directories behind at the link's target, and again afterwards in
	// case a parent was swapped for a link in between.
	dir := filepath.Dir(name)
	if err := checkSymlinkedParent(root, dir); err != nil {
		return err
	}
	if err := fs.mkdirAll(dir, 0o755); err != nil {
		return err
	}
	if err := checkSymlinkedParent(root, dir); err != nil {
		return err
	}

	previous, err := root.Lstat(name)
	switch {
	case err == nil:
		if previous.IsDir() {
			return errors.Errorf("server/filesystem: restore symlink: cannot replace directory '%s' with a symlink", name)
		}
	case errors.Is(err, os.ErrNotExist):
		previous = nil
	default:
		return errors.Wrap(err, "server/filesystem: restore symlink: failed to stat path")
	}

	// Create the link under a temporary name in the same directory, so that the
	// rename is atomic and never leaves name missing.
	var tmp string
	for attempt := 0; ; attempt++ {
		suffix := make([]byte, 6)
		if _, err := rand.Read(suffix); err != nil {
			return errors.WithStack(err)
		}
		tmp = filepath.Join(dir, ".wings-"+hex.EncodeToString(suffix))
		err := root.Symlink(target, tmp)
		if err == nil {
			break
		}
		if !errors.Is(err, os.ErrExist) || attempt >= 2 {
			return errors.Wrap(err, "server/filesystem: restore symlink: failed to create symlink")
		}
	}

	if err := fs.chownFile(tmp); err != nil {
		_ = root.Remove(tmp)
		return err
	}
	if err := root.Rename(tmp, name); err != nil {
		_ = root.Remove(tmp)
		return errors.Wrap(err, "server/filesystem: restore symlink: failed to move symlink into place")
	}

	// The replaced regular file no longer counts towards the disk usage.
	if previous != nil && previous.Mode().IsRegular() {
		fs.addDisk(-previous.Size())
	}
	return nil
}

// checkSymlinkedParent returns an error if any existing component of dir is a
// symbolic link. Components are checked from the top down, because os.Root
// resolves links in the leading components of the path it is given: only the
// component that is itself a link reports as one. Checking stops at the first
// component that does not exist, since nothing below it can exist either.
func checkSymlinkedParent(root *os.Root, dir string) error {
	if dir == "." {
		return nil
	}
	var p string
	for _, part := range strings.Split(dir, string(filepath.Separator)) {
		p = filepath.Join(p, part)
		st, err := root.Lstat(p)
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		if err != nil {
			return errors.Wrap(err, "server/filesystem: restore symlink: failed to stat parent directory")
		}
		if st.Mode()&os.ModeSymlink != 0 {
			return errors.New("server/filesystem: restore symlink: cannot create a symlink inside a symlinked directory")
		}
	}
	return nil
}
