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

	// quota tracks the disk space used by this Filesystem instance along with
	// the maximum amount of disk space (in bytes) that it is allowed to use.
	quota diskQuota

	// The root data directory path for this Filesystem instance.
	root     *os.Root
	rootPath string

	isTest bool
}

// New creates a new Filesystem instance for a given server.
func New(path string, size int64, denylist []string) (*Filesystem, error) {
	r, err := os.OpenRoot(path)
	if err != nil {
		return nil, errors.Wrap(err, "server/filesystem: failed to open root")
	}

	fs := &Filesystem{
		root:              r,
		rootPath:          path,
		diskCheckInterval: time.Duration(config.Get().System.DiskCheckInterval),
		lastLookupTime:    &usageLookupTime{},
		lookupInProgress:  system.NewAtomicBool(false),
		denylist:          ignore.CompileIgnoreLines(denylist...),
	}
	fs.quota.SetLimit(size)

	return fs, nil
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

// Close closes the underlying os.Root instance for the server.
func (fs *Filesystem) Close() error {
	if err := fs.root.Close(); err != nil {
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
	p = normalize(p)
	st, err := fs.root.Stat(p)
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
	f, err := fs.root.Open(p)
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
	p = normalize(p)
	var currentSize int64
	st, err := fs.root.Stat(p)
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
	p = normalize(p)
	if p == "." {
		return nil, &Error{code: ErrCodeIsDirectory, resolved: p}
	}
	// Refuse to open anything that already exists and is not a regular file (or a
	// symlink to one, which os.Root keeps inside the server directory). Opening a
	// named pipe for writing would block, and writing to a device is never wanted.
	if st, err := fs.root.Stat(p); err == nil {
		if st.IsDir() {
			return nil, &Error{code: ErrCodeIsDirectory, resolved: p}
		}
		if !st.Mode().IsRegular() {
			return nil, &Error{code: ErrCodeNotRegular, resolved: p}
		}
	}

	o := &fileOpener{root: fs.root}
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
	p = normalize(p)
	if err := fs.root.Mkdir(p, mode); err != nil {
		return errors.Wrap(err, "server/filesystem: mkdir: failed to make directory")
	}
	return fs.chownFile(p)
}

// mkdirAll creates the directory p along with any missing parents, chowning
// every directory it creates to the server user so they are not left owned by
// the user Wings runs as.
func (fs *Filesystem) mkdirAll(p string, mode os.FileMode) error {
	p = normalize(p)
	if p == "." {
		return nil
	}

	// Find the directories that do not exist yet before creating them, so that
	// only those get their owner changed.
	var missing []string
	for dir := p; dir != "." && dir != string(filepath.Separator); dir = filepath.Dir(dir) {
		if _, err := fs.root.Lstat(dir); err == nil {
			break
		} else if !errors.Is(err, os.ErrNotExist) {
			return errors.Wrap(err, "server/filesystem: mkdir: failed to stat directory")
		}
		missing = append(missing, dir)
	}

	if err := fs.root.MkdirAll(p, mode); err != nil {
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
	p = normalize(p)
	st, err := fs.root.Stat(p)
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
func (fs *Filesystem) CreateDirectory(name string, p string) error {
	return fs.mkdirAll(path.Join(normalize(p), name), 0o755)
}

// Rename moves (or renames) a file or directory.
func (fs *Filesystem) Rename(from string, to string) error {
	to = normalize(to)
	from = normalize(from)

	if from == "." || to == "." {
		return os.ErrExist
	}

	// If the target file or directory already exists the rename function will
	// fail, so just bail out now. Lstat is used so that a dangling symlink at
	// the destination is not silently replaced.
	if _, err := fs.root.Lstat(to); err == nil {
		return os.ErrExist
	}

	// Ensure that the directory we're moving into exists correctly on the system. Only do this if
	// we're not at the root directory level.
	if err := fs.mkdirAll(filepath.Dir(to), 0o755); err != nil {
		return errors.Wrap(err, "server/filesystem: failed to create directory tree")
	}

	return fs.root.Rename(from, to)
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
		if _, err := fs.root.Lstat(normalize(path.Join(dir, n))); err != nil {
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
	p = normalize(p)
	s, err := fs.root.Stat(p)
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

	source, err := fs.root.Open(p)
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

// Symlink creates a symbolic link at "target" pointing to "source". Both paths
// are relative to the root of the server; the link is written as a path relative
// to the directory it is created in so that it resolves to the same file both on
// the host and inside the server container.
//
// [os.Root].Symlink allows for the creation of a symlink that targets a file
// outside the root directory. This isn't the end of the world because every read
// and write in this package goes through os.Root, which refuses to follow it, and
// within a container it would just point to something in the container filesystem.
func (fs *Filesystem) Symlink(source, target string) error {
	source = normalize(source)
	target = normalize(target)

	// Just to avoid this propagating everywhere, *attempt* to block anything that
	// would be pointing to a location outside the root directory.
	if _, err := fs.root.Stat(source); err != nil {
		return errors.Wrap(err, "server/filesystem: symlink: failed to stat source")
	}

	// Yes -- this gap between the stat and symlink allows a TOCTOU race to exist,
	// but again we're layering this with the remaining logic that prevents this filesystem
	// from reading any symlinks or acting on any file that points outside the root as defined
	// by os.Root. The check above is mostly to prevent stupid mistakes or basic attempts to
	// get around this. Running server processes can create any symlink they want anyway.
	link := source
	if rel, err := filepath.Rel(filepath.Dir(target), source); err == nil {
		link = rel
	}
	if err := fs.root.Symlink(link, target); err != nil {
		return errors.Wrap(err, "server/filesystem: symlink: failed to create symlink")
	}

	return nil
}

// ReadDir returns all the contents of the given directory.
func (fs *Filesystem) ReadDir(p string) ([]fs2.DirEntry, error) {
	e, err := fs2.ReadDir(fs.root.FS(), normalize(p))
	if err != nil {
		return []fs2.DirEntry{}, errors.Wrap(err, "server/filesystem: readdir: failed to read directory")
	}

	return e, nil
}

// TruncateRootDirectory removes _all_ files and directories from a server's
// data directory and resets the used disk space to zero.
func (fs *Filesystem) TruncateRootDirectory() error {
	entries, err := fs2.ReadDir(fs.root.FS(), ".")
	if err == nil {
		for _, e := range entries {
			if err = fs.root.RemoveAll(e.Name()); err != nil {
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
	p = normalize(p)
	if p == "." {
		return errors.New("server/filesystem: delete: cannot delete root directory")
	}

	st, err := fs.root.Lstat(p)
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

	if err := fs.root.RemoveAll(p); err != nil {
		return errors.Wrap(err, "server/filesystem: delete: failed to remove path")
	}
	fs.addDisk(-size)

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

	// You must initialize the output of this directory as a non-nil value otherwise
	// when it is marshaled into a JSON object you'll just get 'null' back, which will
	// break the panel badly.
	out := make([]Stat, len(files))

	// Iterate over all the files and directories returned and perform an async process
	// to get the mime-type for them all.
	for i, file := range files {
		wg.Add(1)

		go func(idx int, d fs2.DirEntry) {
			defer wg.Done()

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
				if f, err := fs.root.Open(normalize(filepath.Join(p, d.Name()))); err != nil {
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
