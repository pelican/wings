package filesystem

import (
	"archive/tar"
	"compress/gzip"
	"context"
	"io"
	iofs "io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	. "github.com/franela/goblin"
	"github.com/mholt/archives"
)

func TestArchive_Stream(t *testing.T) {
	g := Goblin(t)
	fs, rfs := NewFs()

	g.Describe("Archive", func() {
		g.AfterEach(func() {
			// Reset the filesystem after each run.
			_ = fs.TruncateRootDirectory()
		})

		g.It("creates archive with intended files", func() {
			g.Assert(fs.CreateDirectory("test", "/")).IsNil()
			g.Assert(fs.CreateDirectory("test2", "/")).IsNil()

			r := strings.NewReader("hello, world!\n")
			err := fs.Write("test/file.txt", r, r.Size(), 0o644)
			g.Assert(err).IsNil()

			r = strings.NewReader("hello, world!\n")
			err = fs.Write("test2/file.txt", r, r.Size(), 0o644)
			g.Assert(err).IsNil()

			r = strings.NewReader("hello, world!\n")
			err = fs.Write("test_file.txt", r, r.Size(), 0o644)
			g.Assert(err).IsNil()

			r = strings.NewReader("hello, world!\n")
			err = fs.Write("test_file.txt.old", r, r.Size(), 0o644)
			g.Assert(err).IsNil()

			a := &Archive{
				Filesystem: fs,
				Files: []string{
					"test",
					"test_file.txt",
				},
			}

			// Create the archive.
			archivePath := filepath.Join(rfs.root, "archive.tar.gz")
			g.Assert(a.Create(context.Background(), archivePath)).IsNil()

			// Ensure the archive exists.
			_, err = os.Stat(archivePath)
			g.Assert(err).IsNil()

			// Open the archive.
			genericFs, err := archives.FileSystem(context.Background(), archivePath, nil)
			g.Assert(err).IsNil()

			// Assert that we are opening an archive.
			afs, ok := genericFs.(iofs.ReadDirFS)
			g.Assert(ok).IsTrue()

			// Get the names of the files recursively from the archive.
			files, err := getFiles(afs, ".")
			g.Assert(err).IsNil()

			// Ensure the files in the archive match what we are expecting.
			expected := []string{
				"test_file.txt",
				"test/file.txt",
			}

			// Sort the slices to ensure the comparison never fails if the
			// contents are sorted differently.
			sort.Strings(expected)
			sort.Strings(files)

			g.Assert(files).Equal(expected)
		})

		g.It("includes symlinks in the archive instead of silently skipping them", func() {
			r := strings.NewReader("hello, world!\n")
			err := fs.Write("real_file.txt", r, r.Size(), 0o644)
			g.Assert(err).IsNil()

			g.Assert(fs.Symlink("real_file.txt", "link_to_file.txt")).IsNil()
			g.Assert(fs.Symlink("/etc/passwd", "link_outside_root.txt")).IsNil()

			a := &Archive{
				Filesystem: fs,
			}

			archivePath := filepath.Join(rfs.root, "archive_symlinks.tar.gz")
			g.Assert(a.Create(context.Background(), archivePath)).IsNil()

			_, err = os.Stat(archivePath)
			g.Assert(err).IsNil()

			entries, err := readTarHeaders(archivePath)
			g.Assert(err).IsNil()

			link, ok := entries["link_to_file.txt"]
			g.Assert(ok).IsTrue()
			g.Assert(link.Typeflag).Equal(byte(tar.TypeSymlink))
			g.Assert(link.Linkname).Equal("real_file.txt")

			outside, ok := entries["link_outside_root.txt"]
			g.Assert(ok).IsTrue()
			g.Assert(outside.Typeflag).Equal(byte(tar.TypeSymlink))
			g.Assert(outside.Linkname).Equal("/etc/passwd")

			file, ok := entries["real_file.txt"]
			g.Assert(ok).IsTrue()
			g.Assert(file.Typeflag).Equal(byte(tar.TypeReg))
		})
	})
}

func getFiles(f iofs.ReadDirFS, name string) ([]string, error) {
	var v []string

	entries, err := f.ReadDir(name)
	if err != nil {
		return nil, err
	}

	for _, e := range entries {
		entryName := e.Name()
		if name != "." {
			entryName = filepath.Join(name, entryName)
		}

		if e.IsDir() {
			files, err := getFiles(f, entryName)
			if err != nil {
				return nil, err
			}

			if files == nil {
				return nil, nil
			}

			v = append(v, files...)
			continue
		}

		v = append(v, entryName)
	}

	return v, nil
}

func readTarHeaders(path string) (map[string]*tar.Header, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()

	gz, err := gzip.NewReader(f)
	if err != nil {
		return nil, err
	}
	defer gz.Close()

	tr := tar.NewReader(gz)
	entries := make(map[string]*tar.Header)
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, err
		}
		entries[hdr.Name] = hdr
	}
	return entries, nil
}