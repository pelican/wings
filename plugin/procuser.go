package plugin

import (
	"errors"
	"fmt"
	"io/fs"
	"math"
	"os"
	"os/user"
	"path/filepath"
	"strconv"
	"sync"
	"syscall"

	"github.com/pelican/wings/config"
)

// ProcessUser is the OS user an external plugin's process runs as (contract
// S, "Process user"). The process has that user's access to the machine;
// granted host access only limits what it can ask Wings.
type ProcessUser struct {
	// Name is the user's name, or its uid as a string when it has none.
	Name string
	Uid  int
	Gid  int
	// Switch is set when Wings, running as root, starts the process as this
	// other user.
	Switch bool
}

// IDs returns the uid and gid for syscall credentials, refusing values that
// don't fit a uint32 (a negative or oversized id in config.yml would otherwise
// wrap to some other user).
func (u ProcessUser) IDs() (uid, gid uint32, err error) {
	if u.Uid < 0 || int64(u.Uid) > math.MaxUint32 || u.Gid < 0 || int64(u.Gid) > math.MaxUint32 {
		return 0, 0, fmt.Errorf("plugin: user %s has an out-of-range uid %d or gid %d", u.Name, u.Uid, u.Gid)
	}
	return uint32(u.Uid), uint32(u.Gid), nil
}

// ExternalProcessUser returns the user external plugins run as:
//   - Wings is not root (rootless, a developer machine): Wings' own user.
//   - Wings is root and plugins.run_as_root is true: root.
//   - Wings is root otherwise: system.user (pelican by default), never root.
//
// It reads the configuration only when Wings is root.
func ExternalProcessUser() (ProcessUser, error) {
	if os.Geteuid() != 0 {
		return WingsUser(), nil
	}
	c := config.Get()
	if c.Plugins.RunAsRoot {
		return ProcessUser{Name: userName(0), Uid: 0, Gid: 0}, nil
	}
	uid, gid := c.System.User.Uid, c.System.User.Gid
	if uid == 0 && c.System.Username != "" {
		// The daemon fills these in at startup (config.EnsurePelicanUser);
		// `wings plugins` commands don't, so look the user up by name.
		if u, err := user.Lookup(c.System.Username); err == nil {
			// 32 bits signed, so the value fits an int on every platform.
			if n, err := strconv.ParseInt(u.Uid, 10, 32); err == nil && n >= 0 {
				uid = int(n)
			}
			if n, err := strconv.ParseInt(u.Gid, 10, 32); err == nil && n >= 0 {
				gid = int(n)
			}
		}
	}
	if uid == 0 || gid == 0 {
		return ProcessUser{}, fmt.Errorf("plugin: system.user (%q, uid %d, gid %d) is root or unknown, and plugins don't run as root unless plugins.run_as_root is true",
			c.System.Username, uid, gid)
	}
	return ProcessUser{Name: userName(uid), Uid: uid, Gid: gid, Switch: true}, nil
}

// WingsUser is the user Wings itself runs as, which native plugins (compiled
// into Wings) run as too.
func WingsUser() ProcessUser {
	uid := os.Geteuid()
	return ProcessUser{Name: userName(uid), Uid: uid, Gid: os.Getegid()}
}

var userNames sync.Map // uid -> name

// userName looks up uid's user name, falling back to the uid as a string.
func userName(uid int) string {
	if n, ok := userNames.Load(uid); ok {
		return n.(string)
	}
	name := strconv.Itoa(uid)
	if u, err := user.LookupId(name); err == nil && u.Username != "" {
		name = u.Username
	}
	userNames.Store(uid, name)
	return name
}

// PrepareDataDir creates a plugin's data directory, dir (Manifest.DataDir),
// for u. When Wings is root, dir and everything in it is owned by u, with
// dir's mode 0700, and its parent (<plugins>/.data) is 0711 so u can reach
// dir but not list the other plugins' directories. That also moves data an
// earlier version left owned by root (when plugins ran as root) over to u,
// and back to root when plugins.run_as_root is set. When Wings isn't root,
// dir is only created.
//
// As root, every change goes through an os.Root on the plugins directory,
// and neither .data nor dir may be a symlink: someone able to write there
// could otherwise point .data at / and have Wings chmod it and give a
// directory such as /etc to u.
func PrepareDataDir(dir string, u ProcessUser) error {
	if os.Geteuid() != 0 {
		return os.MkdirAll(dir, 0o700)
	}
	parent := filepath.Dir(dir)
	data, name := filepath.Base(parent), filepath.Base(dir)
	root, err := os.OpenRoot(filepath.Dir(parent))
	if err != nil {
		return err
	}
	defer root.Close()
	// An earlier Wings created .data 0700 (mode bits are also cut by the umask).
	if err := mkdirNoSymlink(root, data, 0o711); err != nil {
		return err
	}
	if err := root.Chmod(data, 0o711); err != nil {
		return err
	}
	rel := filepath.Join(data, name)
	if err := mkdirNoSymlink(root, rel, 0o700); err != nil {
		return err
	}
	sub, err := root.OpenRoot(rel)
	if err != nil {
		return err
	}
	defer sub.Close()
	if err := chownTree(sub, u.Uid, u.Gid); err != nil {
		return fmt.Errorf("plugin: giving the data directory to %s: %w", u.Name, err)
	}
	return root.Chmod(rel, 0o700)
}

// mkdirNoSymlink creates name under root if it's missing, and refuses it when
// it exists as anything but a directory, a symlink included.
func mkdirNoSymlink(root *os.Root, name string, perm fs.FileMode) error {
	if err := root.Mkdir(name, perm); err != nil && !errors.Is(err, fs.ErrExist) {
		return err
	}
	info, err := root.Lstat(name)
	if err != nil {
		return err
	}
	if !info.IsDir() {
		return fmt.Errorf("plugin: %s in %s is not a directory (a symlink is refused)", name, root.Name())
	}
	return nil
}

// chownTree gives the directory root is opened on, and the files and
// directories in it, to uid:gid. It runs as root on a directory the plugin's
// user can write while it runs, so every change goes through a file
// descriptor opened under root: a path swapped for a symlink in between can't
// redirect it outside, and the checks are made on what was actually opened.
// Symlinks, special files and files with more than one hard link are
// skipped: a hard link could be any file on the system that the plugin's user
// linked in.
func chownTree(root *os.Root, uid, gid int) error {
	return fs.WalkDir(root.FS(), ".", func(name string, d fs.DirEntry, err error) error {
		if err != nil {
			if errors.Is(err, fs.ErrNotExist) {
				return nil // removed while walking
			}
			return err
		}
		if !d.IsDir() && !d.Type().IsRegular() {
			return nil
		}
		if info, err := d.Info(); err == nil && ownedBy(info, uid, gid) {
			return nil
		}
		return chownOpened(root, name, uid, gid)
	})
}

func chownOpened(root *os.Root, name string, uid, gid int) error {
	// O_NONBLOCK: a file swapped for a FIFO must not block the open.
	f, err := root.OpenFile(name, os.O_RDONLY|syscall.O_NONBLOCK, 0)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) || errors.Is(err, syscall.ELOOP) || errors.Is(err, syscall.ENXIO) {
			return nil // gone, or no longer a file or directory
		}
		return err
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return err
	}
	st, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return nil
	}
	if !info.IsDir() && (!info.Mode().IsRegular() || st.Nlink != 1) {
		return nil
	}
	return f.Chown(uid, gid)
}

func ownedBy(info fs.FileInfo, uid, gid int) bool {
	st, ok := info.Sys().(*syscall.Stat_t)
	return ok && int(st.Uid) == uid && int(st.Gid) == gid
}

// CheckReadOnlyTo checks that u can't change the plugin in dir: nothing in
// dir, dir itself, or any directory above it (the plugins directory also holds
// the trust approvals) is owned by u or writable through u's group or by
// everyone. A sticky directory above dir (such as /tmp) may be writable: its
// sticky bit keeps u from moving entries u doesn't own. Wings starts the
// process with no supplementary groups, so u's primary group is the only one
// that counts. Symlinks are not checked: their own mode is never used, and
// what they point to inside dir is checked there.
func CheckReadOnlyTo(dir string, u ProcessUser) error {
	dir, err := filepath.Abs(dir)
	if err != nil {
		return err
	}
	check := func(path string, info fs.FileInfo, above bool) error {
		if info.Mode()&fs.ModeSymlink != 0 {
			return nil
		}
		st, ok := info.Sys().(*syscall.Stat_t)
		if !ok {
			return nil
		}
		perm := info.Mode().Perm()
		sticky := above && info.Mode()&fs.ModeSticky != 0
		switch {
		case int(st.Uid) == u.Uid:
			return fmt.Errorf("plugin: %s is owned by %s, the user the plugin runs as; the plugin's files must be read-only to it (for example chown -R root:root and chmod -R go-w)", path, u.Name)
		case !sticky && (int(st.Gid) == u.Gid && perm&0o020 != 0 || perm&0o002 != 0):
			return fmt.Errorf("plugin: %s is writable by %s, the user the plugin runs as; the plugin's files must be read-only to it (for example chmod -R go-w)", path, u.Name)
		}
		return nil
	}
	// The ancestors of both the path as given and the real path: a symlink
	// in the path leads to directories its lexical parents don't cover, and
	// replacing the symlink itself needs write access to its lexical parent.
	real, err := filepath.EvalSymlinks(dir)
	if err != nil {
		return err
	}
	ancestors := func(from string) error {
		for p := filepath.Dir(from); ; p = filepath.Dir(p) {
			info, err := os.Stat(p)
			if err != nil {
				return err
			}
			if err := check(p, info, true); err != nil {
				return err
			}
			if p == filepath.Dir(p) {
				return nil
			}
		}
	}
	if err := ancestors(dir); err != nil {
		return err
	}
	if real != dir {
		if err := ancestors(real); err != nil {
			return err
		}
	}
	return filepath.WalkDir(real, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		info, err := d.Info()
		if err != nil {
			return err
		}
		return check(path, info, false)
	})
}

// CheckReachableBy checks that u can enter dir and every directory above it,
// on both the path as given and the real path. Wings creates its own root
// directory (system.root_directory) with mode 0700, so a plugin directory
// inside it can't be reached by system.user, and exec would fail with a bare
// "permission denied". The owner's, group's or everyone's search bit applies,
// as the kernel picks them; ACLs are not read.
func CheckReachableBy(dir string, u ProcessUser) error {
	dir, err := filepath.Abs(dir)
	if err != nil {
		return err
	}
	real, err := filepath.EvalSymlinks(dir)
	if err != nil {
		return err
	}
	check := func(from string) error {
		for p := from; ; p = filepath.Dir(p) {
			info, err := os.Stat(p)
			if err != nil {
				return err
			}
			if st, ok := info.Sys().(*syscall.Stat_t); ok {
				perm := info.Mode().Perm()
				var bit fs.FileMode = 0o001
				switch {
				case int(st.Uid) == u.Uid:
					bit = 0o100
				case int(st.Gid) == u.Gid:
					bit = 0o010
				}
				if perm&bit == 0 {
					return fmt.Errorf("plugin: %s (mode %04o) can't be entered by %s, the user the plugin runs as, so the plugin can't start; give that directory search permission (chmod o+x) or set plugins.directory outside it",
						p, perm, u.Name)
				}
			}
			if p == filepath.Dir(p) {
				return nil
			}
		}
	}
	if err := check(dir); err != nil {
		return err
	}
	if real != dir {
		return check(real)
	}
	return nil
}

// ProcessDisclosure is the sentence every approval shows (contract S,
// "Disclosure at approval"), for a plugin that runs as user.
func ProcessDisclosure(user string) string {
	return "This plugin runs on this node as " + user + ", with that user's access to the machine. " +
		"The host access listed only limits what it can ask Wings."
}
