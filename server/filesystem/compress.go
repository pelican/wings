package filesystem

import (
	"context"
	"fmt"
	"io"
	iofs "io/fs"
	"os"
	"path"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"emperror.dev/errors"
	"github.com/mholt/archives"
)

type extractOptions struct {
	dir    string
	file   string
	format archives.Format
	r      io.Reader
}

// CompressFiles compresses all the files matching the given paths in the
// specified directory. This function also supports passing nested paths to only
// compress certain files and folders when working in a larger directory. This
// effectively creates a local backup, but rather than ignoring specific files
// and folders, it takes an allowlist of files and folders.
//
// All paths are relative to the dir that is passed in as the first argument,
// and the compressed file will be placed at that location named
// `archive-{date}.tar.gz`.
func (fs *Filesystem) CompressFiles(ctx context.Context, dir string, name string, paths []string, extension string) (os.FileInfo, string, error) {
	dir = normalize(dir)

	// Normalize extension & assign MIME type
	extension = strings.ToLower(strings.TrimPrefix(extension, "."))
	var (
		ext      string
		mimetype string
		format   archives.Archiver
	)
	switch extension {
	case "zip":
		ext = ".zip"
		mimetype = "application/zip"
		format = archives.Zip{}
	case "tar.bz2", "tbz2":
		ext = ".tar.bz2"
		mimetype = "application/x-bzip2"
		format = archives.CompressedArchive{Compression: archives.Bz2{}, Archival: archives.Tar{}}
	case "tar.xz", "txz":
		ext = ".tar.xz"
		mimetype = "application/x-xz"
		format = archives.CompressedArchive{Compression: archives.Xz{}, Archival: archives.Tar{}}
	default:
		// tar.gz, tgz, and the fallback for anything we do not recognize.
		ext = ".tar.gz"
		mimetype = "application/gzip"
		format = archives.CompressedArchive{Compression: archives.Gz{}, Archival: archives.Tar{}}
	}

	files, err := fs.filesForArchive(dir, paths)
	if err != nil {
		return nil, "", err
	}
	if len(files) == 0 {
		return nil, "", fmt.Errorf("no valid files to compress")
	}

	if name == "" {
		name = fmt.Sprintf("archive-%s%s", strings.ReplaceAll(time.Now().Format(time.RFC3339), ":", ""), ext)
	} else {
		name, err = fs.findCopySuffix(dir, path.Base(normalize(name)), ext)
		if err != nil {
			return nil, "", errors.Wrap(err, "server/filesystem: compress: failed to find unique archive name")
		}
	}
	destPath := normalize(path.Join(dir, name))

	// The archive is written through a quota-tracked handle, so writing it stops as
	// soon as the server runs out of disk space rather than only being checked after
	// the entire archive has already been written.
	f, err := fs.Touch(destPath, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o644)
	if err != nil {
		return nil, "", errors.Wrap(err, "server/filesystem: compress: failed to open file for writing")
	}

	cleanup := func() {
		_ = f.Close()
		_ = fs.Delete(destPath)
	}

	// The archive libraries close their writers in a defer and drop the error, and
	// every format holds its last bytes until then. Without errWriter a write that
	// fails at that point (such as running out of disk space on a small archive)
	// would be reported as success with a truncated archive left behind.
	w := &errWriter{w: f}
	err = format.Archive(ctx, w, files)
	if w.err != nil {
		err = w.err
	}
	if err != nil {
		cleanup()
		if IsErrorCode(err, ErrCodeDiskSpace) {
			return nil, "", ErrNoSpaceAvailable
		}
		return nil, "", errors.Wrap(err, "server/filesystem: compress: failed to write archive")
	}

	info, err := f.Stat()
	if err != nil {
		cleanup()
		return nil, "", errors.Wrap(err, "server/filesystem: compress: failed to stat archive")
	}
	if err := f.Close(); err != nil {
		_ = fs.Delete(destPath)
		return nil, "", errors.Wrap(err, "server/filesystem: compress: failed to close archive")
	}
	return info, mimetype, nil
}

// filesForArchive returns the files to add to an archive for each of the given
// paths (relative to dir), recursing into directories. Every file is opened
// through the os.Root instance so nothing outside the server's data directory
// can be added, and symlinks are stored as links rather than followed. Files on
// the server's denylist are skipped.
func (fs *Filesystem) filesForArchive(dir string, paths []string) ([]archives.FileInfo, error) {
	var out []archives.FileInfo
	seen := make(map[string]struct{})

	for _, p := range paths {
		// "." selects everything in dir; its contents are stored at the top of the
		// archive (path.Join drops the "." below) and dir itself is not added.
		name := normalize(p)
		base := normalize(path.Join(dir, name))

		// io/fs.WalkDir follows the starting path if it is a symlink, so only walk
		// paths that really are directories.
		st, err := fs.root.Lstat(base)
		if err != nil {
			if errors.Is(err, os.ErrNotExist) {
				continue
			}
			return nil, errors.Wrap(err, "server/filesystem: compress: failed to stat file")
		}
		walk := iofs.WalkDir
		if !st.IsDir() {
			walk = func(_ iofs.FS, root string, fn iofs.WalkDirFunc) error {
				return fn(root, iofs.FileInfoToDirEntry(st), nil)
			}
		}

		err = walk(rootDirFS{fs.root}, base, func(full string, d iofs.DirEntry, err error) error {
			if err != nil {
				if errors.Is(err, iofs.ErrNotExist) {
					return nil
				}
				return err
			}
			if name == "." && full == base {
				return nil
			}
			if fs.IsIgnored(full) != nil {
				if d.IsDir() {
					return iofs.SkipDir
				}
				return nil
			}
			if _, ok := seen[full]; ok {
				return nil
			}
			seen[full] = struct{}{}

			info, err := d.Info()
			if err != nil {
				if errors.Is(err, iofs.ErrNotExist) {
					return nil
				}
				return err
			}

			nameInArchive := path.Join(name, strings.TrimPrefix(strings.TrimPrefix(full, base), "/"))
			fi := archives.FileInfo{FileInfo: info, NameInArchive: nameInArchive}

			switch mode := info.Mode(); {
			case mode.IsDir():
			case mode&os.ModeSymlink != 0:
				target, err := fs.root.Readlink(full)
				if err != nil {
					return nil
				}
				fi.LinkTarget = target
			case mode.IsRegular():
				size := info.Size()
				fi.Open = func() (iofs.File, error) {
					return fs.openForArchive(full, size)
				}
			default:
				// Named pipes, sockets and devices are never added to the archive.
				return nil
			}

			out = append(out, fi)
			return nil
		})
		if err != nil {
			return nil, errors.Wrap(err, "server/filesystem: compress: failed to walk files")
		}
	}

	return out, nil
}

// openForArchive opens a regular file through the os.Root instance for reading,
// limiting the amount of data read to the size it had when it was added to the
// archive's file list.
func (fs *Filesystem) openForArchive(p string, size int64) (iofs.File, error) {
	// O_NONBLOCK makes sure we never hang if the file was swapped for a named pipe.
	f, err := fs.root.OpenFile(p, os.O_RDONLY|syscall.O_NONBLOCK, 0)
	if err != nil {
		return nil, err
	}
	st, err := f.Stat()
	if err != nil {
		_ = f.Close()
		return nil, err
	}
	if !st.Mode().IsRegular() {
		_ = f.Close()
		return nil, &Error{code: ErrCodeNotRegular, resolved: p}
	}
	return &limitedFile{File: f, r: io.LimitReader(f, size)}, nil
}

type limitedFile struct {
	*os.File
	r io.Reader
}

func (f *limitedFile) Read(p []byte) (int, error) {
	return f.r.Read(p)
}

// DecompressFile will decompress a file in a given directory by using the
// archiver tool to infer the file type and go from there. This will walk over
// all the files within the given archive and ensure that there is not a
// zip-slip attack being attempted by validating that the final path is within
// the server data directory.
func (fs *Filesystem) DecompressFile(ctx context.Context, dir string, file string) error {
	f, _, err := fs.File(filepath.Join(dir, file))
	if err != nil {
		return errors.Wrap(err, "server/filesystem: decompress: failed to open file")
	}
	defer f.Close()

	format, input, err := archives.Identify(ctx, filepath.Base(file), f)
	if err != nil {
		if errors.Is(err, archives.NoMatch) {
			return newFilesystemError(ErrCodeUnknownArchive, err)
		}
		return errors.Wrap(err, "server/filesystem: decompress: failed to identify archive format")
	}

	return fs.extractStream(ctx, extractOptions{dir: dir, file: file, format: format, r: input})
}

func (fs *Filesystem) extractStream(ctx context.Context, opts extractOptions) error {
	// See if it's a compressed archive, such as TAR or a ZIP
	ex, ok := opts.format.(archives.Extractor)
	if !ok {
		// If not, check if it's a single-file compression, such as
		// .log.gz, .sql.gz, and so on
		de, ok := opts.format.(archives.Decompressor)
		if !ok {
			return nil
		}

		p := filepath.Join(opts.dir, strings.TrimSuffix(opts.file, opts.format.Extension()))
		if err := fs.IsIgnored(p); err != nil {
			return nil
		}

		reader, err := de.OpenReader(opts.r)
		if err != nil {
			return errors.Wrap(err, "server/filesystem: decompress: failed to open reader")
		}
		defer reader.Close()

		// The size of the decompressed file is not known ahead of time, so write it
		// through a quota-tracked handle which fails once the server runs out of space.
		f, err := fs.Touch(p, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o644)
		if err != nil {
			return errors.Wrap(err, "server/filesystem: decompress: failed to open file")
		}
		defer f.Close()

		if _, err := io.Copy(f, reader); err != nil {
			return errors.Wrap(err, "server/filesystem: decompress: failed to write file")
		}

		return nil
	}

	// Decompress and extract archive
	return ex.Extract(ctx, opts.r, func(ctx context.Context, f archives.FileInfo) error {
		p := filepath.Join(opts.dir, f.NameInArchive)
		if err := fs.IsIgnored(p); err != nil {
			return nil
		}
		// Create directories explicitly; an empty one has no file to create it
		// implicitly and would otherwise be dropped during extraction.
		if f.IsDir() {
			if err := fs.mkdirAll(p, 0o755); err != nil {
				return errors.Wrap(err, "server/filesystem: decompress: failed to create directory")
			}
			return nil
		}
		if f.Mode()&os.ModeSymlink != 0 {
			// Try to create the symlink if it is in the archive, but don't hold up the process
			// if the file cannot be created. In that case just skip over it entirely.
			//
			// We don't use [fs.Symlink] here because that rewrites the target relative to the
			// root of the server. When decompressing we want to keep the target as it was in
			// the archive. Whatever it points to, os.Root prevents Wings from following it
			// outside the server's data directory.
			if f.LinkTarget != "" {
				p2 := normalize(p)
				if err := fs.mkdirAll(filepath.Dir(p2), 0o755); err != nil {
					return nil
				}
				if err := fs.root.Symlink(f.LinkTarget, p2); err != nil {
					if errors.Is(err, os.ErrNotExist) || errors.Is(err, os.ErrExist) || IsPathError(err) || IsLinkError(err) {
						return nil
					}
					return errors.Wrap(err, "server/filesystem: decompress: failed to create symlink")
				}
				_ = fs.chownFile(p2)
			}
			return nil
		}
		if !f.Mode().IsRegular() {
			// Named pipes, devices and the like are never extracted.
			return nil
		}

		r, err := f.Open()
		if err != nil {
			return err
		}
		defer r.Close()

		if err := fs.Write(p, r, f.Size(), f.Mode().Perm()); err != nil {
			return errors.Wrap(err, "server/filesystem: decompress: failed to write file")
		}

		// Update the file modification time to the one set in the archive.
		if err := fs.Chtimes(p, f.ModTime(), f.ModTime()); err != nil {
			return errors.Wrap(err, "server/filesystem: decompress: failed to update file modification time")
		}

		return nil
	})
}

// ExtractStreamUnsafe .
func (fs *Filesystem) ExtractStreamUnsafe(ctx context.Context, dir string, r io.Reader) error {
	format, input, err := archives.Identify(ctx, "archive.tar.gz", r)
	if err != nil {
		if errors.Is(err, archives.NoMatch) {
			return newFilesystemError(ErrCodeUnknownArchive, err)
		}
		return err
	}
	return fs.extractStream(ctx, extractOptions{
		dir:    dir,
		format: format,
		r:      input,
	})
}
