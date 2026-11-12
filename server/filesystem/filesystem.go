package filesystem

import (
	"io"
	fs2 "io/fs"
	"os"
	"path"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"emperror.dev/errors"
	"github.com/apex/log"
	"github.com/gabriel-vasile/mimetype"
	ignore "github.com/sabhiram/go-gitignore"

	"github.com/pelican/wings/config"
	"github.com/pelican/wings/system"
)

type Filesystem struct {
	mu                sync.RWMutex
	lastLookupTime    *usageLookupTime
	lookupInProgress  *system.AtomicBool
	diskCheckInterval time.Duration
	denylist          *ignore.GitIgnore

	// hasDenylist is false when the egg has no denylist, so that checks against it
	// can skip resolving symlinks.
	hasDenylist bool

	// quota tracks the disk space used by this Filesystem instance along with
	// the maximum amount of disk space (in bytes) that it is allowed to use.
	quota diskQuota

	// The root data directory path for this Filesystem instance, and the folder
	// open at that path. Use openRoot, not root, to get the folder.
	root     atomic.Pointer[os.Root]
	rootPath string
	// rootMu guards reopening the folder, and replaced holds the folders that
	// were open before, which stay open until Close.
	rootMu   sync.Mutex
	replaced []*os.Root
	// checked is when openRoot last found the open folder still at rootPath, in
	// Unix nanoseconds.
	checked atomic.Int64

	isTest bool
}

// New creates a new Filesystem instance for a given server.
func New(path string, size int64, denylist []string) (*Filesystem, error) {
	r, err := os.OpenRoot(path)
	if err != nil {
		return nil, errors.Wrap(err, "server/filesystem: failed to open root")
	}

	fs := &Filesystem{
		rootPath:          path,
		diskCheckInterval: time.Duration(config.Get().System.DiskCheckInterval),
		lastLookupTime:    &usageLookupTime{},
		lookupInProgress:  system.NewAtomicBool(false),
		denylist:          ignore.CompileIgnoreLines(denylist...),
		hasDenylist:       slices.ContainsFunc(denylist, func(l string) bool { return strings.TrimSpace(l) != "" }),
	}
	fs.root.Store(r)
	fs.quota.SetLimit(size)

	return fs, nil
}

// rootCheckInterval is how long openRoot trusts its last check of the server folder.
const rootCheckInterval = 50 * time.Millisecond

// openRoot returns the server's folder. Wings keeps it open, but Docker mounts it
// by path, so if the folder at rootPath is no longer the one that is open (it was
// moved or deleted and then recreated, or a disk was mounted over it) the new one
// is opened. internal/ufs got the same result by opening the path on every call.
// If nothing is at rootPath, this returns an os.ErrNotExist error.
//
// The check costs two stat calls, and an archive extraction or a recursive delete
// makes several calls per file, so it runs at most once per rootCheckInterval. A
// replaced folder is picked up within that time.
func (fs *Filesystem) openRoot() (*os.Root, error) {
	r := fs.root.Load()
	now := time.Now().UnixNano()
	if now-fs.checked.Load() < int64(rootCheckInterval) {
		return r, nil
	}
	open, err := r.Stat(".")
	if err != nil {
		return nil, errors.Wrap(err, "server/filesystem: failed to stat the open server folder")
	}
	current, err := os.Stat(fs.rootPath)
	if err != nil {
		return nil, errors.Wrap(err, "server/filesystem: failed to stat the server folder")
	}
	// A deleted folder's inode number can be given straight to the new one, so
	// SameFile alone can match a folder that is no longer at the path.
	if os.SameFile(open, current) && !isUnlinked(open) {
		fs.checked.Store(now)
		return r, nil
	}

	fs.rootMu.Lock()
	defer fs.rootMu.Unlock()
	if r2 := fs.root.Load(); r2 != r {
		return r2, nil // another call already reopened it
	}
	nr, err := os.OpenRoot(fs.rootPath)
	if err != nil {
		return nil, errors.Wrap(err, "server/filesystem: failed to reopen the server folder")
	}
	// Operations that started before may still be using the old folder, so it is
	// only closed by Close.
	fs.replaced = append(fs.replaced, r)
	fs.root.Store(nr)
	fs.checked.Store(now)
	log.WithField("path", fs.rootPath).Info("server/filesystem: server folder was replaced, opened the new one")
	return nr, nil
}

// normalize takes the input path, runs it through filepath.Clean and trims any
// leading forward slashes (since the os.Root method calls will fail otherwise).
// If the resulting path is an empty string, "." is returned which os.Root will
// understand as the base directory.
func normalize(path string) string {
	c := strings.TrimLeft(filepath.Clean(path), string(filepath.Separator))
	if c == "" {
		return "."
	}
	return c
}

// Path returns the root path for the Filesystem instance.
func (fs *Filesystem) Path() string {
	return fs.rootPath
}

// Close closes the underlying os.Root instance for the server, and any folder
// that was open before the server's folder was replaced.
func (fs *Filesystem) Close() error {
	fs.rootMu.Lock()
	defer fs.rootMu.Unlock()
	for _, r := range fs.replaced {
		_ = r.Close()
	}
	fs.replaced = nil
	if err := fs.root.Load().Close(); err != nil {
		return errors.Wrap(err, "server/filesystem: failed to close root")
	}
	return nil
}

// File returns a reader for a file instance as well as the stat information.
// Only regular files can be opened; directories return an ErrCodeIsDirectory
// error and any other type of file (named pipes, sockets, devices) returns an
// ErrCodeNotRegular error without the file ever being opened. Opening a named
// pipe for reading blocks until a writer appears, which would hang the caller.
func (fs *Filesystem) File(p string) (*os.File, Stat, error) {
	root, err := fs.openRoot()
	if err != nil {
		return nil, Stat{}, err
	}
	p = normalize(p)
	st, err := root.Stat(p)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, Stat{}, newFilesystemError(ErrNotExist, err)
		}
		return nil, Stat{}, errors.WithStackIf(err)
	}
	if st.IsDir() {
		return nil, Stat{}, &Error{code: ErrCodeIsDirectory, resolved: p}
	}
	if !st.Mode().IsRegular() {
		return nil, Stat{FileInfo: st}, &Error{code: ErrCodeNotRegular, resolved: p}
	}
	f, err := root.OpenFile(p, os.O_RDONLY|syscall.O_NONBLOCK, 0)
	if err != nil {
		return nil, Stat{}, errors.WithStackIf(err)
	}
	// Stat the opened handle so that the information returned always matches the
	// file being read, even if the path was swapped out after the check above.
	st, err = f.Stat()
	if err != nil {
		_ = f.Close()
		return nil, Stat{}, errors.WithStackIf(err)
	}
	if !st.Mode().IsRegular() {
		_ = f.Close()
		return nil, Stat{FileInfo: st}, &Error{code: ErrCodeNotRegular, resolved: p}
	}
	out := Stat{FileInfo: st, Mimetype: "application/octet-stream"}
	if m, err := mimetype.DetectReader(f); err == nil {
		out.Mimetype = m.String()
	}
	if _, err := f.Seek(0, io.SeekStart); err != nil {
		_ = f.Close()
		return nil, Stat{}, errors.WithStackIf(err)
	}
	return f, out, nil
}

// Touch acts by creating the given file and path on the disk if it is not present
// already. If it is present, the file is opened using the defaults which will truncate
// the contents. The opened file is then returned to the caller.
//
// Writes made through the returned handle are accounted against the disk quota of
// the server, and will fail once the server runs out of space.
func (fs *Filesystem) Touch(p string, flag int, mode os.FileMode) (File, error) {
	root, err := fs.openRoot()
	if err != nil {
		return nil, err
	}
	p = normalize(p)
	var currentSize int64
	st, err := root.Stat(p)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return nil, errors.Wrap(err, "server/filesystem: touch: failed to stat file")
	} else if err == nil && st.Mode().IsRegular() {
		currentSize = st.Size()
	}

	f, err := fs.touch(p, flag, mode)
	if err != nil {
		return nil, err
	}

	// If the file was truncated when it was opened the space it was using has
	// been freed already.
	if flag&os.O_TRUNC != 0 && currentSize > 0 {
		fs.addDisk(-currentSize)
		currentSize = 0
	}
	return newQuotaFile(fs, f, currentSize), nil
}

// touch opens the given file, creating it and any parent directories that are
// missing. Writes to the returned handle are NOT accounted against the quota;
// callers must do that themselves.
func (fs *Filesystem) touch(p string, flag int, mode os.FileMode) (*os.File, error) {
	root, err := fs.openRoot()
	if err != nil {
		return nil, err
	}
	p = normalize(p)
	if p == "." {
		return nil, &Error{code: ErrCodeIsDirectory, resolved: p}
	}
	// Refuse to open anything that already exists and is not a regular file (or a
	// symlink to one, which os.Root keeps inside the server directory). Opening a
	// named pipe for writing would block, and writing to a device is never wanted.
	if st, err := root.Stat(p); err == nil {
		if st.IsDir() {
			return nil, &Error{code: ErrCodeIsDirectory, resolved: p}
		}
		if !st.Mode().IsRegular() {
			return nil, &Error{code: ErrCodeNotRegular, resolved: p}
		}
	}

	// O_NONBLOCK makes sure we never hang if the file is swapped for a named pipe
	// after the check above; the handle is rejected below if it is not a regular file.
	flag |= syscall.O_NONBLOCK
	o := &fileOpener{root: root}
	f, err := o.open(p, flag, mode)
	if err != nil {
		// If the error is not because it doesn't exist then we just need to bail at this point.
		if !errors.Is(err, os.ErrNotExist) || flag&os.O_CREATE == 0 {
			return nil, errors.Wrap(err, "server/filesystem: touch: failed to open file handle")
		}
		// Create the path leading up to the file we're trying to create, setting the
		// owner on the directories as we go.
		if err := fs.mkdirAll(filepath.Dir(p), 0o755); err != nil {
			return nil, errors.WrapIf(err, "server/filesystem: touch: failed to create directory tree")
		}
		// Try to open the file now that we have created the pathing necessary for it.
		f, err = o.open(p, flag, mode)
		if err != nil {
			return nil, errors.Wrap(err, "server/filesystem: touch: failed to open file handle")
		}
	}

	if st, err := f.Stat(); err != nil || !st.Mode().IsRegular() {
		_ = f.Close()
		if err != nil {
			return nil, errors.Wrap(err, "server/filesystem: touch: failed to stat file handle")
		}
		return nil, &Error{code: ErrCodeNotRegular, resolved: p}
	}

	if err := fs.chownFile(p); err != nil {
		_ = f.Close()
		return nil, errors.WrapIf(err, "server/filesystem: touch: failed to chown file")
	}
	return f, nil
}

// Writefile writes a file to the system. If the file does not already exist one
// will be created. This will also properly recalculate the disk space used by
// the server when writing new files or modifying existing ones.
//
// The size of the data is not known ahead of time, so it is written through a
// quota-tracked handle which fails as soon as the server runs out of space.
//
// Deprecated: prefer the use of [Filesystem.Write], which checks that the file
// fits within the disk limit before writing anything.
func (fs *Filesystem) Writefile(p string, r io.Reader) error {
	file, err := fs.Touch(p, os.O_RDWR|os.O_CREATE|os.O_TRUNC, 0o644)
	if err != nil {
		return err
	}

	_, err = io.Copy(file, r)
	if cerr := file.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		return errors.Wrap(err, "server/filesystem: writefile: failed to write file")
	}
	return nil
}

// Mkdir creates a single directory, owned by the server user.
func (fs *Filesystem) Mkdir(p string, mode os.FileMode) error {
	root, err := fs.openRoot()
	if err != nil {
		return err
	}
	p = normalize(p)
	if err := root.Mkdir(p, mode); err != nil {
		return errors.Wrap(err, "server/filesystem: mkdir: failed to make directory")
	}
	return fs.chownFile(p)
}

// mkdirAll creates the directory p along with any missing parents, chowning
// every directory it creates to the server user so they are not left owned by
// the user Wings runs as.
func (fs *Filesystem) mkdirAll(p string, mode os.FileMode) error {
	root, err := fs.openRoot()
	if err != nil {
		return err
	}
	p = normalize(p)
	if p == "." {
		return nil
	}

	// Find the directories that do not exist yet before creating them, so that
	// only those get their owner changed.
	var missing []string
	for dir := p; dir != "." && dir != string(filepath.Separator); dir = filepath.Dir(dir) {
		if _, err := root.Lstat(dir); err == nil {
			break
		} else if !errors.Is(err, os.ErrNotExist) {
			return errors.Wrap(err, "server/filesystem: mkdir: failed to stat directory")
		}
		missing = append(missing, dir)
	}

	if err := root.MkdirAll(p, mode); err != nil {
		return errors.Wrap(err, "server/filesystem: mkdir: failed to create directory tree")
	}

	for i := len(missing) - 1; i >= 0; i-- {
		if err := fs.chownFile(missing[i]); err != nil {
			return err
		}
	}
	return nil
}

// Write writes a file to the disk.
func (fs *Filesystem) Write(p string, r io.Reader, newSize int64, mode os.FileMode) error {
	root, err := fs.openRoot()
	if err != nil {
		return err
	}
	p = normalize(p)
	st, err := root.Stat(p)
	if err != nil {
		if !errors.Is(err, os.ErrNotExist) {
			return errors.Wrap(err, "server/filesystem: write: failed to stat file")
		}
	}

	var c int64
	if err == nil {
		if st.IsDir() {
			return errors.WithStack(&Error{code: ErrCodeIsDirectory, resolved: p})
		}
		c = st.Size()
	}

	// Check that the new size we're writing to the disk can fit. If there is currently
	// a file we'll subtract that current file size from the size of the buffer to determine
	// the amount of new data we're writing (or amount we're removing if smaller).
	if err := fs.HasSpaceFor(newSize - c); err != nil {
		return err
	}

	f, err := fs.touch(p, os.O_RDWR|os.O_CREATE|os.O_TRUNC, mode)
	if err != nil {
		return errors.Wrap(err, "server/filesystem: write: failed to touch file")
	}
	defer f.Close()

	if newSize == 0 {
		fs.addDisk(-c)
		return nil
	}

	// Do not use CopyBuffer here; it is wasteful as the file implements
	// io.ReaderFrom, which causes it to not use the buffer anyway.
	n, err := io.Copy(f, io.LimitReader(r, newSize))
	// Always adjust the disk to account for cases where a partial copy occurs
	// and there is some new content on the disk.
	fs.addDisk(n - c)
	if err != nil {
		return errors.Wrap(err, "server/filesystem: write: failed to write file")
	}
	return nil
}

// CreateDirectory creates a new directory ("name") at a specified path ("p") for the server.
//
// p and name are joined before being cleaned, as main does, so a name such as
// "../name" under "/" stays inside the server, while "e/../../name" still fails.
func (fs *Filesystem) CreateDirectory(name string, p string) error {
	return fs.mkdirAll(path.Join(p, name), 0o755)
}

// Rename moves (or renames) a file or directory.
func (fs *Filesystem) Rename(from string, to string) error {
	root, err := fs.openRoot()
	if err != nil {
		return err
	}
	to = normalize(to)
	from = normalize(from)

	// Renaming something to its own name does nothing, as on main.
	if from == to {
		return nil
	}

	if from == "." || to == "." {
		return os.ErrExist
	}

	// If the target file or directory already exists the rename function will
	// fail, so just bail out now. Lstat is used so that a dangling symlink at
	// the destination is not silently replaced.
	if _, err := root.Lstat(to); err == nil {
		return os.ErrExist
	}

	// Ensure that the directory we're moving into exists correctly on the system. Only do this if
	// we're not at the root directory level.
	if err := fs.mkdirAll(filepath.Dir(to), 0o755); err != nil {
		return errors.Wrap(err, "server/filesystem: failed to create directory tree")
	}

	return root.Rename(from, to)
}

// Begin looping up to 50 times to try and create a unique file name. This will take an input
// of "file.txt" and use it as-is if it is not taken, otherwise it will generate "file copy.txt".
// If that name is already taken, it will then try to write "file copy 2.txt" and so on, until
// reaching 50 loops. At that point we won't waste anymore time, just use the current timestamp
// and make that copy.
//
// Could probably make this more efficient by checking if there are any files matching the copy
// pattern, and trying to find the highest number and then incrementing it by one rather than
// looping endlessly.
func (fs *Filesystem) findCopySuffix(dir string, name string, extension string) (string, error) {
	root, err := fs.openRoot()
	if err != nil {
		return "", err
	}
	var i int
	suffix := ""

	for i = 0; i < 51; i++ {
		if i == 1 {
			suffix = " copy"
		} else if i == 50 {
			suffix = " copy." + time.Now().Format(time.RFC3339)
		} else if i > 1 {
			suffix = " copy " + strconv.Itoa(i-1)
		}

		n := name + suffix + extension
		// If we stat the file and it does not exist that means we're good to create the copy. If it
		// does exist, we'll just continue to the next loop and try again.
		if _, err := root.Lstat(normalize(path.Join(dir, n))); err != nil {
			if !errors.Is(err, os.ErrNotExist) {
				return "", err
			}

			break
		}
	}

	return name + suffix + extension, nil
}

// Copies a given file to the same location and appends a suffix to the file to indicate that
// it has been copied.
func (fs *Filesystem) Copy(p string) error {
	root, err := fs.openRoot()
	if err != nil {
		return err
	}
	p = normalize(p)
	s, err := root.Stat(p)
	if err != nil {
		return err
	} else if s.IsDir() || !s.Mode().IsRegular() {
		// If this is a directory or not a regular file, just throw a not-exist error
		// since anything calling this function should understand what that means.
		return os.ErrNotExist
	}

	// Check that copying this file wouldn't put the server over its limit.
	if err := fs.HasSpaceFor(s.Size()); err != nil {
		return err
	}

	dir := filepath.Dir(p)
	base := filepath.Base(p)
	extension := filepath.Ext(base)
	name := strings.TrimSuffix(base, extension)

	// Ensure that ".tar" is also counted as apart of the file extension.
	// There might be a better way to handle this for other double file extensions,
	// but this is a good workaround for now.
	if strings.HasSuffix(name, ".tar") {
		extension = ".tar" + extension
		name = strings.TrimSuffix(name, ".tar")
	}

	source, err := fs.openRegular(p)
	if err != nil {
		return err
	}
	defer source.Close()

	n, err := fs.findCopySuffix(dir, name, extension)
	if err != nil {
		return err
	}

	return fs.Write(path.Join(dir, n), source, s.Size(), s.Mode().Perm())
}

// Symlink creates a symbolic link at "target" pointing to "source". "target" is
// relative to the root of the server. A relative "source" is relative to the
// directory the link is created in, as with symlink(2); an absolute "source" is
// relative to the root of the server. The link is written as a path relative to
// the directory it is created in so that it resolves to the same file both on the
// host and inside the server container. Sources that do not exist, or that are
// outside the root of the server, are refused.
//
// [os.Root].Symlink allows for the creation of a symlink that targets a file
// outside the root directory. This isn't the end of the world because every read
// and write in this package goes through os.Root, which refuses to follow it, and
// within a container it would just point to something in the container filesystem.
func (fs *Filesystem) Symlink(source, target string) error {
	root, err := fs.openRoot()
	if err != nil {
		return err
	}
	target = normalize(target)
	// Refuse a link outside the root before resolving the source against its
	// directory, with the same error os.Root returns when creating it.
	if !filepath.IsLocal(target) {
		return &os.LinkError{Op: "symlinkat", Old: source, New: target, Err: errors.New("path escapes from parent")}
	}
	// A relative source is relative to the directory the link is created in, like
	// symlink(2) and `ln -s`, and like the target an SFTP client sends. An absolute
	// source is relative to the server root.
	if filepath.IsAbs(source) {
		source = normalize(source)
	} else {
		source = normalize(filepath.Join(filepath.Dir(target), source))
	}

	// Just to avoid this propagating everywhere, *attempt* to block anything that
	// would be pointing to a location outside the root directory.
	if _, err := root.Stat(source); err != nil {
		return errors.Wrap(err, "server/filesystem: symlink: failed to stat source")
	}

	// Yes -- this gap between the stat and symlink allows a TOCTOU race to exist,
	// but again we're layering this with the remaining logic that prevents this filesystem
	// from reading any symlinks or acting on any file that points outside the root as defined
	// by os.Root. The check above is mostly to prevent stupid mistakes or basic attempts to
	// get around this. Running server processes can create any symlink they want anyway.
	// The relative link below is worked out from the directory names as given, but
	// os.Root follows symlinks in those directories when it creates the link. If one
	// of them is a symlink the link would be relative to the wrong directory, so it
	// is refused rather than created pointing somewhere else.
	for d := filepath.Dir(target); d != "."; d = filepath.Dir(d) {
		if st, err := root.Lstat(d); err == nil && st.Mode()&os.ModeSymlink != 0 {
			return errors.New("server/filesystem: symlink: cannot create a symlink inside a symlinked directory")
		}
	}

	link := source
	if rel, err := filepath.Rel(filepath.Dir(target), source); err == nil {
		link = rel
	}
	if err := root.Symlink(link, target); err != nil {
		return errors.Wrap(err, "server/filesystem: symlink: failed to create symlink")
	}

	return nil
}

// rootDirFS is the [fs2.FS] used to list and walk directories inside an os.Root.
//
// The FS from [os.Root.FS] opens every path with a plain O_RDONLY, and opening a
// named pipe that way blocks until something writes to it. A server process can
// put a pipe wherever it likes, including in place of a directory that is about
// to be walked. rootDirFS opens directories with O_DIRECTORY and everything with
// O_NONBLOCK, so a pipe fails straight away instead.
type rootDirFS struct {
	root *os.Root
}

func (r rootDirFS) Open(name string) (fs2.File, error) {
	if !fs2.ValidPath(name) {
		return nil, &os.PathError{Op: "open", Path: name, Err: os.ErrInvalid}
	}
	return r.root.OpenFile(name, os.O_RDONLY|syscall.O_NONBLOCK, 0)
}

func (r rootDirFS) Stat(name string) (fs2.FileInfo, error) {
	if !fs2.ValidPath(name) {
		return nil, &os.PathError{Op: "stat", Path: name, Err: os.ErrInvalid}
	}
	return r.root.Stat(name)
}

// ReadDir returns the entries of the directory sorted by name, as [fs2.ReadDir] does.
func (r rootDirFS) ReadDir(name string) ([]fs2.DirEntry, error) {
	if !fs2.ValidPath(name) {
		return nil, &os.PathError{Op: "readdir", Path: name, Err: os.ErrInvalid}
	}
	f, err := r.root.OpenFile(name, os.O_RDONLY|syscall.O_DIRECTORY|syscall.O_NONBLOCK, 0)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	entries, err := f.ReadDir(-1)
	slices.SortFunc(entries, func(a, b fs2.DirEntry) int { return strings.Compare(a.Name(), b.Name()) })
	return entries, err
}

// errWriter passes writes through to w and remembers the first error, for when
// the code doing the writing does not pass every error back.
type errWriter struct {
	w   io.Writer
	err error
}

func (e *errWriter) Write(p []byte) (int, error) {
	if e.err != nil {
		return 0, e.err
	}
	n, err := e.w.Write(p)
	if err != nil {
		e.err = err
	}
	return n, err
}

// ReadDir returns all the contents of the given directory.
func (fs *Filesystem) ReadDir(p string) ([]fs2.DirEntry, error) {
	root, err := fs.openRoot()
	if err != nil {
		return nil, err
	}
	e, err := fs2.ReadDir(rootDirFS{root}, normalize(p))
	if err != nil {
		return []fs2.DirEntry{}, errors.Wrap(err, "server/filesystem: readdir: failed to read directory")
	}

	return e, nil
}

// TruncateRootDirectory removes _all_ files and directories from a server's
// data directory and resets the used disk space to zero.
func (fs *Filesystem) TruncateRootDirectory() error {
	root, err := fs.openRoot()
	if err != nil {
		return err
	}
	entries, err := fs2.ReadDir(rootDirFS{root}, ".")
	if err == nil {
		for _, e := range entries {
			if err = root.RemoveAll(e.Name()); err != nil {
				break
			}
		}
	}

	if err != nil {
		go func() {
			// If there was an error, re-calculate the disk usage right away to account
			// for any partially removed files.
			_, _ = fs.updateCachedDiskUsage()
		}()

		return errors.Wrap(err, "server/filesystem: truncate: failed to remove root directory contents")
	}

	// Set the disk space back to zero.
	fs.quota.SetUsage(0)

	return nil
}

// Delete removes a file or folder from the system. Prevents the user from
// accidentally (or maliciously) removing their root server data directory.
func (fs *Filesystem) Delete(p string) error {
	root, err := fs.openRoot()
	if err != nil {
		return err
	}
	p = normalize(p)
	if p == "." {
		return errors.New("server/filesystem: delete: cannot delete root directory")
	}

	st, err := root.Lstat(p)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return errors.Wrap(err, "server/filesystem: delete: failed to stat file")
	}

	var size int64
	if st.IsDir() {
		size, _ = fs.DirectorySize(p)
	} else if st.Mode().IsRegular() {
		size = st.Size()
	}

	if err := root.RemoveAll(p); err != nil {
		// Part of the tree may have been removed before the error. Stop counting
		// what is gone, so the server is not refused writes for space it has back.
		var remaining int64
		if st, serr := root.Lstat(p); serr == nil {
			if st.IsDir() {
				remaining, _ = fs.DirectorySize(p)
			} else if st.Mode().IsRegular() {
				remaining = st.Size()
			}
		}
		if remaining < size {
			fs.addDisk(remaining - size)
		}
		return errors.Wrap(err, "server/filesystem: delete: failed to remove path")
	}
	fs.addDisk(-size)

	return nil
}

// SafeDeleteRecursively deletes a file or directory while respecting the denylist.
// Denylisted files and directories are left in place, as is every directory that
// still contains one of them.
func (fs *Filesystem) SafeDeleteRecursively(p string) error {
	root, err := fs.openRoot()
	if err != nil {
		return err
	}
	p = normalize(p)
	if p == "." {
		return errors.New("server/filesystem: delete: cannot delete root directory")
	}

	info, err := root.Lstat(p)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		return errors.Wrap(err, "server/filesystem: delete: failed to stat file")
	}

	// Skip denylisted files and directories entirely.
	if fs.IsIgnoredNoFollow(p) != nil {
		return nil
	}

	if !info.IsDir() {
		return fs.Delete(p)
	}

	entries, err := fs.ReadDir(p)
	if err != nil {
		return err
	}

	remaining := false
	for _, e := range entries {
		child := path.Join(p, e.Name())
		if err := fs.SafeDeleteRecursively(child); err != nil {
			return err
		}
		if _, err := root.Lstat(child); err == nil {
			remaining = true
		}
	}

	// Only remove the directory itself if nothing was left behind in it.
	if remaining {
		return nil
	}
	if err := root.Remove(p); err != nil && !errors.Is(err, os.ErrNotExist) {
		return errors.Wrap(err, "server/filesystem: delete: failed to remove directory")
	}
	return nil
}

type fileOpener struct {
	busy uint
	root *os.Root
}

// Attempts to open a given file up to "attempts" number of times, using a backoff. If the file
// cannot be opened because of a "text file busy" error, we will attempt until the number of attempts
// has been exhaused, at which point we will abort with an error.
func (fo *fileOpener) open(path string, flags int, mode os.FileMode) (*os.File, error) {
	for {
		f, err := fo.root.OpenFile(path, flags, mode)

		// If there is an error because the text file is busy, go ahead and sleep for a few
		// hundred milliseconds and then try again up to three times before just returning the
		// error back to the caller.
		//
		// Based on code from: https://github.com/golang/go/issues/22220#issuecomment-336458122
		if err != nil && fo.busy < 3 && errors.Is(err, syscall.ETXTBSY) {
			time.Sleep(100 * time.Millisecond << fo.busy)
			fo.busy++
			continue
		}

		return f, err
	}
}

// ListDirectory lists the contents of a given directory and returns stat information
// about each file and folder within it. If you only need to know the contents of the
// directory and do not need mimetype information, call [Filesystem.ReadDir] directly
// instead.
func (fs *Filesystem) ListDirectory(p string) ([]Stat, error) {
	files, err := fs.ReadDir(p)
	if err != nil {
		return []Stat{}, err
	}

	var wg sync.WaitGroup
	// Bound the number of files inspected at once so that listing a directory with a
	// huge number of entries does not spawn an equally huge number of goroutines and
	// open file descriptors.
	sem := make(chan struct{}, 16)

	// You must initialize the output of this directory as a non-nil value otherwise
	// when it is marshaled into a JSON object you'll just get 'null' back, which will
	// break the panel badly.
	out := make([]Stat, len(files))

	// Iterate over all the files and directories returned and perform an async process
	// to get the mime-type for them all.
	for i, file := range files {
		wg.Add(1)

		sem <- struct{}{}
		go func(idx int, d fs2.DirEntry) {
			defer wg.Done()
			defer func() { <-sem }()

			fi, err := d.Info()
			if err != nil {
				log.WithField("error", err).WithField("path", filepath.Join(p, d.Name())).Warn("failed to retrieve directory entry info")
				return
			}

			if fi.IsDir() {
				out[idx] = Stat{FileInfo: fi, Mimetype: "inode/directory"}
				return
			}

			st := Stat{FileInfo: fi, Mimetype: "application/octet-stream"}

			// Only detect the type of regular files. Opening a named pipe will hang the
			// application, and you'll never get a response back.
			//
			// @see https://github.com/pterodactyl/panel/issues/4059
			if fi.Mode().IsRegular() {
				if f, err := fs.openRegular(normalize(filepath.Join(p, d.Name()))); err != nil {
					if !IsPathError(err) && !IsLinkError(err) {
						log.WithField("error", err).WithField("path", filepath.Join(p, d.Name())).Warn("error opening file for mimetype detection")
					}
				} else {
					if m, err := mimetype.DetectReader(f); err == nil {
						st.Mimetype = m.String()
					} else {
						log.WithField("error", err).WithField("path", filepath.Join(p, d.Name())).Warn("failed to detect mimetype for file")
					}
					_ = f.Close()
				}
			}

			out[idx] = st
		}(i, file)
	}

	wg.Wait()

	// Drop any entries that we failed to get information about, otherwise they
	// would be marshaled with a nil FileInfo.
	out = slices.DeleteFunc(out, func(s Stat) bool { return s.FileInfo == nil })

	// Sort entries alphabetically.
	slices.SortStableFunc(out, func(a, b Stat) int {
		return strings.Compare(a.Name(), b.Name())
	})

	// Sort folders before other file types.
	slices.SortStableFunc(out, func(a, b Stat) int {
		switch {
		case a.IsDir() == b.IsDir():
			return 0
		case a.IsDir():
			return -1
		default:
			return 1
		}
	})

	return out, nil
}
