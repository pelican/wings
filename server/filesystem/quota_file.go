package filesystem

import (
	"io"
	"math"
	"os"
	"sync"
)

// File is the handle returned by [Filesystem.Touch]. Writes made through it are
// accounted against the disk quota of the Filesystem that opened it.
type File interface {
	io.Reader
	io.ReaderAt
	io.Writer
	io.WriterAt
	io.Seeker
	io.Closer
	io.ReaderFrom
	Name() string
	Stat() (os.FileInfo, error)
	Truncate(size int64) error
	Sync() error
}

var _ File = (*quotaFile)(nil)

type quotaFile struct {
	*os.File

	fs   *Filesystem
	mu   sync.Mutex
	size int64
}

func newQuotaFile(fs *Filesystem, file *os.File, size int64) *quotaFile {
	return &quotaFile{File: file, fs: fs, size: size}
}

func (f *quotaFile) Write(p []byte) (int, error) {
	if len(p) == 0 {
		return f.File.Write(p)
	}

	f.mu.Lock()
	defer f.mu.Unlock()

	off, err := f.File.Seek(0, io.SeekCurrent)
	if err != nil {
		return 0, err
	}

	return f.writeAtLocked(p, off, func() (int, error) {
		return f.File.Write(p)
	})
}

func (f *quotaFile) WriteAt(p []byte, off int64) (int, error) {
	if off < 0 || len(p) == 0 {
		return f.File.WriteAt(p, off)
	}

	f.mu.Lock()
	defer f.mu.Unlock()

	return f.writeAtLocked(p, off, func() (int, error) {
		return f.File.WriteAt(p, off)
	})
}

// WriteString makes sure io.WriteString goes through the quota accounting
// instead of the method promoted from the embedded *os.File.
func (f *quotaFile) WriteString(s string) (int, error) {
	return f.Write([]byte(s))
}

func (f *quotaFile) writeAtLocked(p []byte, off int64, write func() (int, error)) (int, error) {
	previousSize := f.size
	end, ok := quotaWriteEnd(off, len(p))
	if !ok {
		return 0, newFilesystemError(ErrCodeDiskSpace, nil)
	}
	if growth := end - previousSize; growth > 0 {
		if err := f.fs.reserveDisk(growth); err != nil {
			return 0, err
		}
	}

	n, err := write()
	writtenEnd := previousSize
	if n > 0 {
		writtenEnd, _ = quotaWriteEnd(off, n)
		if writtenEnd > previousSize {
			f.size = writtenEnd
		}
	}

	if reserved := end - previousSize; reserved > 0 {
		actual := int64(0)
		if writtenEnd > previousSize {
			actual = writtenEnd - previousSize
		}
		if actual < reserved {
			f.fs.adjustDisk(actual - reserved)
		}
	}

	return n, err
}

func quotaWriteEnd(off int64, size int) (int64, bool) {
	if size < 0 || off > math.MaxInt64-int64(size) {
		return 0, false
	}
	return off + int64(size), true
}

// ReadFrom must be implemented so that io.Copy does not bypass the quota by
// using the io.ReaderFrom implementation of the embedded *os.File.
func (f *quotaFile) ReadFrom(r io.Reader) (int64, error) {
	return io.Copy(quotaFileWriter{file: f}, r)
}

// Truncate changes the size of the file, accounting for the change against
// the quota. Growing a file is checked against the limit like any other write.
func (f *quotaFile) Truncate(size int64) error {
	f.mu.Lock()
	defer f.mu.Unlock()

	if size < 0 {
		return f.File.Truncate(size)
	}
	growth := size - f.size
	if growth > 0 {
		if err := f.fs.reserveDisk(growth); err != nil {
			return err
		}
	}
	if err := f.File.Truncate(size); err != nil {
		if growth > 0 {
			f.fs.adjustDisk(-growth)
		}
		return err
	}
	if growth < 0 {
		f.fs.adjustDisk(growth)
	}
	f.size = size
	return nil
}

func (f *quotaFile) Close() error {
	f.mu.Lock()
	defer f.mu.Unlock()

	st, statErr := f.File.Stat()
	closeErr := f.File.Close()
	if statErr == nil {
		f.fs.adjustDisk(st.Size() - f.size)
	}
	if statErr != nil {
		return statErr
	}
	return closeErr
}

// quotaFileWriter hides every method of quotaFile except Write so that io.Copy
// does not recurse back into quotaFile.ReadFrom.
type quotaFileWriter struct {
	file *quotaFile
}

func (w quotaFileWriter) Write(p []byte) (int, error) {
	return w.file.Write(p)
}
