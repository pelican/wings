package filesystem

import (
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"testing/iotest"

	"emperror.dev/errors"
)

// noTempFiles fails the test if a temporary write file was left behind.
func noTempFiles(t *testing.T, dir string) {
	t.Helper()
	left, _ := filepath.Glob(filepath.Join(dir, ".wings-write-*"))
	if len(left) != 0 {
		t.Errorf("expected no temporary files, found %v", left)
	}
}

func readFile(t *testing.T, p string) string {
	t.Helper()
	b, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// An upload that fails part way, for example because the connection dropped,
// keeps the file it would have replaced, and its space is released.
func TestFilesystem_WriteFromFailureKeepsExistingFile(t *testing.T) {
	fs := NewFs()
	defer fs.reset()

	fs.write("world.dat", []byte("original contents"))
	before, err := fs.updateCachedDiskUsage()
	if err != nil {
		t.Fatal(err)
	}

	r := io.MultiReader(strings.NewReader(strings.Repeat("x", 64*1024)), iotest.ErrReader(errors.New("connection dropped")))
	if _, err := fs.WriteFrom("world.dat", r, -1); err == nil {
		t.Fatal("expected the failed upload to be reported")
	}
	if got := readFile(t, filepath.Join(fs.rootPath, "world.dat")); got != "original contents" {
		t.Errorf("expected the original file to be kept, got %q", got)
	}
	if got := fs.CachedUsage(); got != before {
		t.Errorf("expected usage to be back at %d, got %d", before, got)
	}
	noTempFiles(t, fs.rootPath)
}

// An upload over the size limit is refused without replacing the existing file.
func TestFilesystem_WriteFromOverLimitKeepsExistingFile(t *testing.T) {
	fs := NewFs()
	defer fs.reset()

	if err := os.MkdirAll(filepath.Join(fs.rootPath, "dir"), 0o755); err != nil {
		t.Fatal(err)
	}
	fs.write("dir/world.dat", []byte("original"))
	before, err := fs.updateCachedDiskUsage()
	if err != nil {
		t.Fatal(err)
	}

	_, err = fs.WriteFrom("dir/world.dat", strings.NewReader(strings.Repeat("x", 100)), 10)
	if !errors.Is(err, ErrWriteTooLarge) {
		t.Fatalf("expected ErrWriteTooLarge, got %v", err)
	}
	if got := readFile(t, filepath.Join(fs.rootPath, "dir/world.dat")); got != "original" {
		t.Errorf("expected the original file to be kept, got %q", got)
	}
	if got := fs.CachedUsage(); got != before {
		t.Errorf("expected usage to be back at %d, got %d", before, got)
	}
	noTempFiles(t, filepath.Join(fs.rootPath, "dir"))

	// A limit of zero refuses anything that is not empty.
	if _, err := fs.WriteFrom("dir/empty.dat", strings.NewReader("x"), 0); !errors.Is(err, ErrWriteTooLarge) {
		t.Errorf("expected ErrWriteTooLarge for a limit of zero, got %v", err)
	}
}

// Writing over the disk limit keeps the existing file.
func TestFilesystem_WritefileOverDiskLimitKeepsExistingFile(t *testing.T) {
	fs := NewFs()
	defer fs.reset()

	fs.write("world.dat", []byte("original"))
	if _, err := fs.updateCachedDiskUsage(); err != nil {
		t.Fatal(err)
	}
	fs.SetDiskLimit(1024)

	err := fs.Writefile("world.dat", strings.NewReader(strings.Repeat("x", 4096)))
	if !IsErrorCode(err, ErrCodeDiskSpace) {
		t.Fatalf("expected ErrCodeDiskSpace, got %v", err)
	}
	if got := readFile(t, filepath.Join(fs.rootPath, "world.dat")); got != "original" {
		t.Errorf("expected the original file to be kept, got %q", got)
	}
	noTempFiles(t, fs.rootPath)
}

// A successful write replaces the file, keeps its mode, and counts only the new
// contents against the disk.
func TestFilesystem_WriteFromReplacesFile(t *testing.T) {
	fs := NewFs()
	defer fs.reset()

	fs.write("run.sh", []byte(strings.Repeat("o", 1000)))
	if err := os.Chmod(filepath.Join(fs.rootPath, "run.sh"), 0o750); err != nil {
		t.Fatal(err)
	}
	before, err := fs.updateCachedDiskUsage()
	if err != nil {
		t.Fatal(err)
	}

	n, err := fs.WriteFrom("run.sh", strings.NewReader("new"), -1)
	if err != nil || n != 3 {
		t.Fatalf("expected 3 bytes written, got %d, %v", n, err)
	}
	if got := readFile(t, filepath.Join(fs.rootPath, "run.sh")); got != "new" {
		t.Errorf("expected the new contents, got %q", got)
	}
	st, err := os.Stat(filepath.Join(fs.rootPath, "run.sh"))
	if err != nil {
		t.Fatal(err)
	}
	if st.Mode().Perm() != 0o750 {
		t.Errorf("expected the mode to be kept, got %v", st.Mode().Perm())
	}
	if got, want := fs.CachedUsage(), before-1000+3; got != want {
		t.Errorf("expected usage %d, got %d", want, got)
	}
	noTempFiles(t, fs.rootPath)

	// New files, including in new directories, are created.
	if _, err := fs.WriteFrom("a/b/new.txt", strings.NewReader("hi"), -1); err != nil {
		t.Fatal(err)
	}
	if got := readFile(t, filepath.Join(fs.rootPath, "a/b/new.txt")); got != "hi" {
		t.Errorf("expected the new file, got %q", got)
	}
}

// Writing to a symlink inside the server replaces the file it points to and
// leaves the link in place.
func TestFilesystem_WriteFromThroughSymlink(t *testing.T) {
	fs := NewFs()
	defer fs.reset()

	if err := os.MkdirAll(filepath.Join(fs.rootPath, "config"), 0o755); err != nil {
		t.Fatal(err)
	}
	fs.write("config/real.properties", []byte("old"))
	if err := os.Symlink("config/real.properties", filepath.Join(fs.rootPath, "server.properties")); err != nil {
		t.Fatal(err)
	}

	if _, err := fs.WriteFrom("server.properties", strings.NewReader("new"), -1); err != nil {
		t.Fatal(err)
	}
	if got := readFile(t, filepath.Join(fs.rootPath, "config/real.properties")); got != "new" {
		t.Errorf("expected the link target to be replaced, got %q", got)
	}
	st, err := os.Lstat(filepath.Join(fs.rootPath, "server.properties"))
	if err != nil {
		t.Fatal(err)
	}
	if st.Mode()&os.ModeSymlink == 0 {
		t.Error("expected server.properties to still be a symlink")
	}
	noTempFiles(t, filepath.Join(fs.rootPath, "config"))
}

// linkingReader returns some data, then links the temporary file being written
// to another name, as a process in the server could, and then fails.
type linkingReader struct {
	t    *testing.T
	dir  string
	sent bool
}

func (r *linkingReader) Read(p []byte) (int, error) {
	if !r.sent {
		r.sent = true
		return copy(p, strings.Repeat("x", 4096)), nil
	}
	tmp, _ := filepath.Glob(filepath.Join(r.dir, ".wings-write-*"))
	if len(tmp) != 1 {
		r.t.Fatalf("expected one temporary file, found %v", tmp)
	}
	if err := os.Link(tmp[0], filepath.Join(r.dir, "kept.bin")); err != nil {
		r.t.Fatal(err)
	}
	return 0, errors.New("connection dropped")
}

// A failed write only releases the space of its temporary file once the file
// is actually freed. If the server linked it elsewhere, it is still counted.
func TestFilesystem_WriteFromFailureKeepsLinkedTempCounted(t *testing.T) {
	fs := NewFs()
	defer fs.reset()

	before, err := fs.updateCachedDiskUsage()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := fs.WriteFrom("upload.bin", &linkingReader{t: t, dir: fs.rootPath}, -1); err == nil {
		t.Fatal("expected the failed upload to be reported")
	}
	if _, err := os.Stat(filepath.Join(fs.rootPath, "kept.bin")); err != nil {
		t.Fatalf("expected the linked copy to remain: %v", err)
	}
	if got, want := fs.CachedUsage(), before+4096; got != want {
		t.Errorf("expected the linked file to stay counted at %d, got %d", want, got)
	}
	noTempFiles(t, fs.rootPath)
}

// Replacing the same file one write after another releases the space of each
// file replaced exactly once.
func TestFilesystem_WriteFromRepeatedReplaceAccounting(t *testing.T) {
	fs := NewFs()
	defer fs.reset()

	fs.write("other.bin", make([]byte, 1000))
	fs.write("target.bin", make([]byte, 100))
	if _, err := fs.updateCachedDiskUsage(); err != nil {
		t.Fatal(err)
	}
	for _, size := range []int{20, 30} {
		if _, err := fs.WriteFrom("target.bin", strings.NewReader(strings.Repeat("y", size)), -1); err != nil {
			t.Fatal(err)
		}
	}
	if got := fs.CachedUsage(); got != 1030 {
		t.Errorf("expected usage 1030, got %d", got)
	}
}
