package filesystem

import (
	"archive/tar"
	"context"
	"io"
	"io/fs"
	"os"
	"strings"
	"sync"
	"syscall"
	"time"

	"emperror.dev/errors"
	"github.com/juju/ratelimit"
	"github.com/klauspost/pgzip"
	ignore "github.com/sabhiram/go-gitignore"

	"github.com/pelican/wings/config"
	"github.com/pelican/wings/internal/progress"
)

const memory = 4 * 1024

var ErrNoSpaceAvailable = errors.Sentinel("archive: no space available on disk")

var pool = sync.Pool{
	New: func() interface{} {
		b := make([]byte, memory)
		return b
	},
}

// TarProgress .
type TarProgress struct {
	*tar.Writer
	p *progress.Progress
}

// NewTarProgress .
func NewTarProgress(w *tar.Writer, p *progress.Progress) *TarProgress {
	if p != nil {
		p.Writer = w
	}
	return &TarProgress{
		Writer: w,
		p:      p,
	}
}

// Write .
func (p *TarProgress) Write(v []byte) (int, error) {
	if p.p == nil {
		return p.Writer.Write(v)
	}
	return p.p.Write(v)
}

type ArchiveOption func(a *Archive) error

type Archive struct {
	root     *os.Root
	dir      string
	pw       *TarProgress
	ignored  *ignore.GitIgnore
	matching []string
	p        *progress.Progress
}

// NewArchive returns a new archive instance that can be used for generating an
// archive of files and folders within the provided os.Root. The "dir" value is
// a child directory within the `os.Root` instance, and the paths stored in the
// archive are relative to it.
// Ex. os.Root = '/var/lib/pelican/volumes/{server-uuid}' and dir = "worlds"
func NewArchive(r *os.Root, dir string, opts ...ArchiveOption) (*Archive, error) {
	a := &Archive{root: r, dir: normalize(dir)}
	for _, opt := range opts {
		if err := opt(a); err != nil {
			return nil, errors.Wrap(err, "server/filesystem: archive: failed to apply callback option")
		}
	}
	return a, nil
}

// NewArchive returns a new archive of the given directory within the server's
// data directory.
func (fs *Filesystem) NewArchive(dir string, opts ...ArchiveOption) (*Archive, error) {
	root, err := fs.openRoot()
	if err != nil {
		return nil, err
	}
	return NewArchive(root, dir, opts...)
}

func WithProgress(p *progress.Progress) ArchiveOption {
	return func(a *Archive) error {
		a.p = p
		return nil
	}
}

// WithIgnored excludes every file matching the given gitignore-style pattern
// list from the archive. The list is validated against the limits defined in
// ignore.go, and an error is returned if it exceeds them.
func WithIgnored(patterns string) ArchiveOption {
	return func(a *Archive) error {
		if a.matching != nil {
			return errors.NewPlain("cannot create an archive with both ignored and matching configurations")
		}
		if strings.TrimSpace(patterns) == "" {
			return nil
		}

		i, err := compileIgnore(patterns)
		if err != nil {
			return errors.WrapIf(err, "server/filesystem: unable to compile archive ignore patterns")
		}
		a.ignored = i

		return nil
	}
}

// WithMatching limits the archive to the given files and directories (and
// everything within those directories). Paths are relative to the archive's
// base directory.
func WithMatching(files []string) ArchiveOption {
	return func(a *Archive) error {
		if a.ignored != nil {
			return errors.NewPlain("cannot create an archive with both ignored and matching configurations")
		}

		if len(files) == 0 {
			// No files means no restriction, the same as not using this option.
			return nil
		}

		a.matching = make([]string, 0, len(files))
		for _, f := range files {
			if f = normalize(f); f != "." {
				a.matching = append(a.matching, f)
			} else {
				// Matching the base directory itself means everything is included.
				a.matching = nil
				return nil
			}
		}

		return nil
	}
}

func (a *Archive) Progress() *progress.Progress {
	return a.p
}

// Create creates an archive at dst with all the files defined in the
// included Files array.
//
// THIS IS UNSAFE TO USE IF `dst` IS PROVIDED BY A USER! ONLY USE THIS WITH
// CONTROLLED PATHS!
func (a *Archive) Create(ctx context.Context, f *os.File) error {
	// Select a writer based off of the WriteLimit configuration option. If there is no
	// write limit use the file as the writer.
	var writer io.Writer
	if writeLimit := int64(config.Get().System.Backups.WriteLimit * 1024 * 1024); writeLimit > 0 {
		// Token bucket with a capacity of "writeLimit" MiB, adding "writeLimit" MiB/s
		// and then wrap the file writer with the token bucket limiter.
		writer = ratelimit.Writer(f, ratelimit.NewBucketWithRate(float64(writeLimit), writeLimit))
	} else {
		writer = f
	}

	return a.Stream(ctx, writer)
}

// Stream walks the given root directory and generates an archive from the
// provided files.
func (a *Archive) Stream(ctx context.Context, w io.Writer) error {
	// Choose which compression level to use based on the compression_level configuration option
	var compressionLevel int
	switch config.Get().System.Backups.CompressionLevel {
	case "none":
		compressionLevel = pgzip.NoCompression
	case "best_compression":
		compressionLevel = pgzip.BestCompression
	default:
		compressionLevel = pgzip.BestSpeed
	}

	// Make sure the base directory exists, and is a directory, before anything is written.
	if st, err := a.root.Stat(a.dir); err != nil {
		return errors.Wrap(err, "server/filesystem: archive: failed to stat base directory")
	} else if !st.IsDir() {
		return &Error{code: ErrCodeNotRegular, resolved: a.dir}
	}

	// Create a new gzip writer around the file.
	gw, _ := pgzip.NewWriterLevel(w, compressionLevel)
	_ = gw.SetConcurrency(1<<20, 1)

	// Create a new tar writer around the gzip writer.
	tw := tar.NewWriter(gw)

	a.pw = NewTarProgress(tw, a.p)

	// The walk is performed through the os.Root instance, so nothing outside the
	// server's data directory can ever be reached, and symlinks are not followed.
	if err := fs.WalkDir(rootDirFS{a.root}, a.dir, a.walker(ctx)); err != nil {
		_ = tw.Close()
		_ = gw.Close()
		return err
	}

	// Close both writers here rather than in a defer, so their errors are returned.
	// Up to 1 MiB of compressed data is only written out by gw.Close, so for a small
	// server a write failure (a full disk, a dropped connection) would otherwise
	// still be reported as a complete archive.
	if err := tw.Close(); err != nil {
		_ = gw.Close()
		return errors.Wrap(err, "server/filesystem: archive: failed to finish tar stream")
	}
	if err := gw.Close(); err != nil {
		return errors.Wrap(err, "server/filesystem: archive: failed to finish gzip stream")
	}
	return nil
}

// Callback function used to determine if a given file should be included in the archive
// being generated.
func (a *Archive) walker(ctx context.Context) fs.WalkDirFunc {
	// Time each evaluation of the ignore patterns and abort if they prove
	// disproportionately expensive for this server's file tree.
	budget := newIgnoreMatchBudget()

	return func(p string, de fs.DirEntry, err error) error {
		if ctx.Err() != nil {
			return ctx.Err()
		}

		if err != nil {
			// Files that disappear while the archive is being generated are skipped.
			if errors.Is(err, fs.ErrNotExist) {
				return nil
			}
			return errors.Wrap(err, "server/filesystem: archive: failed to walk directory")
		}

		rel := p
		if a.dir != "." {
			rel = strings.TrimPrefix(strings.TrimPrefix(p, a.dir), "/")
		}
		// The base directory itself is never added to the archive.
		if rel == "" || rel == "." {
			return nil
		}

		if a.ignored != nil {
			start := time.Now()
			skip := a.ignored.MatchesPath(rel) || (de.IsDir() && a.ignored.MatchesPath(rel+"/"))
			if err := budget.track(time.Since(start)); err != nil {
				return err
			}
			// Directories are still walked even when they match, the same as the
			// previous implementation, so that "!" rules can re-include files inside
			// them. They are just not added to the archive themselves.
			if skip {
				return nil
			}
		}

		if a.matching != nil {
			included, descend := a.matches(rel)
			if !included {
				if de.IsDir() && !descend {
					return fs.SkipDir
				}
				return nil
			}
		}

		// Add the file to the archive, if it is nested in a directory,
		// the directory will be automatically "created" in the archive.
		return a.addToArchive(p, rel)
	}
}

// matches reports whether rel is one of the matching paths (or is within one of
// them), and whether it is a parent directory of one of them.
func (a *Archive) matches(rel string) (included bool, parent bool) {
	for _, m := range a.matching {
		if rel == m || strings.HasPrefix(rel, m+"/") {
			return true, false
		}
		if strings.HasPrefix(m, rel+"/") {
			parent = true
		}
	}
	return false, parent
}

// Adds a given file path to the final archive being created. "p" is the path of
// the file within the os.Root instance and "rel" is the name it is stored as.
func (a *Archive) addToArchive(p, rel string) error {
	s, err := a.root.Lstat(p)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return errors.Wrap(err, "server/filesystem: archive: failed to stat file")
	}

	// Skip socket files as they are unsupported by archive/tar.
	// Error will come from tar#FileInfoHeader: "archive/tar: sockets not supported"
	if s.Mode()&fs.ModeSocket != 0 {
		return nil
	}

	// Resolve the symlink target if the file is a symlink.
	var target string
	if s.Mode()&fs.ModeSymlink != 0 {
		// We need to know the actual target of the symlink, even if outside the server
		// directory, so that we can restore it properly. This target is only stored in
		// the archive; we never read the target file contents.
		target, err = a.root.Readlink(p)
		if err != nil {
			if os.IsNotExist(err) {
				return nil
			}
			return errors.Wrap(err, "server/filesystem: archive: failed to read symlink")
		}
	}

	// Get the tar FileInfoHeader to add the file to the archive.
	header, err := tar.FileInfoHeader(s, target)
	if err != nil {
		return errors.Wrap(err, "server/filesystem: archive: failed to get file info header")
	}

	header.Name = rel
	if s.IsDir() {
		header.Name += "/"
	}

	// Only regular files have any content to copy. Open them before writing the header
	// so that a file which can no longer be opened is skipped rather than leaving an
	// entry in the archive with no content.
	if !s.Mode().IsRegular() || header.Size < 1 {
		return a.writeHeader(header)
	}

	// O_NONBLOCK makes sure we never hang if the file was swapped for a named pipe
	// after the Lstat call above. It has no effect on regular files.
	f, err := a.root.OpenFile(p, os.O_RDONLY|syscall.O_NONBLOCK, 0)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return errors.Wrap(err, "server/filesystem: archive: failed to open file for copying")
	}
	defer f.Close()

	if fst, err := f.Stat(); err != nil {
		return errors.Wrap(err, "server/filesystem: archive: failed to stat file for copying")
	} else if !fst.Mode().IsRegular() {
		return nil
	}

	if err := a.writeHeader(header); err != nil {
		return err
	}

	// If the buffer size is larger than the file size, create a smaller buffer to hold the file.
	var buf []byte
	if header.Size < memory {
		buf = make([]byte, header.Size)
	} else {
		// Get a fixed-size buffer from the pool to save on allocations.
		buf = pool.Get().([]byte)
		defer pool.Put(buf)
	}

	if _, err := io.CopyBuffer(a.pw, io.LimitReader(f, header.Size), buf); err != nil {
		return errors.Wrap(err, "server/filesystem: archive: failed to copy file to archive")
	}

	return nil
}

func (a *Archive) writeHeader(header *tar.Header) error {
	if err := a.pw.WriteHeader(header); err != nil {
		return errors.Wrap(err, "server/filesystem: archive: failed to write tar header")
	}
	return nil
}
