package install

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"strings"

	"github.com/mholt/archives"
	"golang.org/x/sys/unix"

	"github.com/pelican/wings/plugin"
)

// StageUpdate downloads, verifies, and extracts an update without replacing the
// live plugin directory. Callers can inspect stage.Manifest before Commit.
func StageUpdate(ctx context.Context, st UpdateStatus, opts ...StageOption) (*StagedInstall, error) {
	if st.download == "" || st.sha256 == "" || st.dir == "" {
		return nil, errors.New("plugin: incomplete update status; run CheckUpdate first")
	}
	return stageArchive(ctx, st.download, st.sha256, st.signature, st.dir, auth{}, applyStageOptions(opts))
}

func stageArchive(ctx context.Context, download, wantSHA256, signature, destDir string, a auth, o stageOptions) (*StagedInstall, error) {
	archivePath, digest, err := downloadAndVerify(ctx, download, wantSHA256, a)
	if err != nil {
		return nil, err
	}
	defer os.Remove(archivePath)
	staging, err := extractToStaging(ctx, archivePath, digest, signature, filepath.Dir(destDir), o)
	if err != nil {
		return nil, err
	}
	return &StagedInstall{DestDir: destDir, StagingDir: staging}, nil
}

// extractToStaging applies the signing policy to a verified archive and
// unpacks it into a new hidden staging directory in parent, returning its
// path. Staging inside the plugins directory makes the final swap an atomic
// same-filesystem rename (no cross-device copy).
func extractToStaging(ctx context.Context, archivePath string, digest []byte, signature, parent string, o stageOptions) (string, error) {
	if err := enforceSignature(digest, signature, o); err != nil {
		return "", err
	}
	if err := os.MkdirAll(parent, 0o755); err != nil {
		return "", err
	}
	root, err := os.OpenRoot(parent)
	if err != nil {
		return "", err
	}
	defer root.Close()
	staging, err := mkdirHidden(root, ".wings-install-")
	if err != nil {
		return "", err
	}
	if err := extractArchive(ctx, archivePath, filepath.Join(parent, staging)); err != nil {
		_ = root.RemoveAll(staging)
		return "", err
	}
	return filepath.Join(parent, staging), nil
}

// mkdirHidden creates a new, uniquely named directory "<prefix><random>" in
// root and returns its name.
func mkdirHidden(root *os.Root, prefix string) (string, error) {
	for i := 0; i < 10; i++ {
		b := make([]byte, 8)
		if _, err := rand.Read(b); err != nil {
			return "", err
		}
		name := prefix + hex.EncodeToString(b)
		err := root.Mkdir(name, 0o755)
		if err == nil {
			return name, nil
		}
		if !errors.Is(err, fs.ErrExist) {
			return "", err
		}
	}
	return "", errors.New("plugin: could not create a unique working directory")
}

// StagedInstall is an extracted, verified plugin archive waiting to be swapped
// into its final directory.
//
// Every change to the plugins directory below goes through an os.Root opened on
// it, so a plugin name or a path inside an archive can never reach outside it,
// whatever checks were missed earlier.
type StagedInstall struct {
	DestDir    string
	StagingDir string
	committed  bool
	// backupHolder is the hidden directory (relative to the plugins
	// directory) holding the previous version between Commit and Finish or
	// Rollback. Empty for a fresh install.
	backupHolder string
	// lock is the plugins directory's install lock, held from Commit until
	// Finish or Rollback.
	lock *os.File
}

// installLockName is the lock file installs take in the plugins directory.
// A hidden file, so discovery never looks at it.
const installLockName = ".install.lock"

// backupName is the previous version's name inside its backup holder.
const backupName = "previous"

// pluginsRoot opens the directory that holds DestDir and returns it with the
// names of the plugin and its staging directory inside it.
func (s *StagedInstall) pluginsRoot() (root *os.Root, name, staging string, err error) {
	root, err = os.OpenRoot(filepath.Dir(s.DestDir))
	if err != nil {
		return nil, "", "", err
	}
	return root, filepath.Base(s.DestDir), filepath.Base(s.StagingDir), nil
}

// lockInstalls takes the plugins directory's install lock, waiting for any
// other install (in this or another process) to finish or roll back first.
// Without it, a second install could swap in between another's Commit and its
// Rollback, and that Rollback would delete the second install's files.
func lockInstalls(root *os.Root) (*os.File, error) {
	f, err := root.OpenFile(installLockName, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, fmt.Errorf("plugin: opening install lock: %w", err)
	}
	if err := unix.Flock(int(f.Fd()), unix.LOCK_EX); err != nil {
		f.Close()
		return nil, fmt.Errorf("plugin: taking install lock: %w", err)
	}
	return f, nil
}

func (s *StagedInstall) unlock() {
	if s.lock != nil {
		_ = unix.Flock(int(s.lock.Fd()), unix.LOCK_UN)
		_ = s.lock.Close()
		s.lock = nil
	}
}

// Manifest loads the staged plugin manifest without executing plugin code.
func (s *StagedInstall) Manifest() (plugin.Manifest, error) {
	return plugin.LoadManifest(filepath.Join(s.StagingDir, plugin.ManifestFile))
}

// Commit moves StagingDir into place as DestDir. The previous version is kept
// until Finish discards it or Rollback restores it, so a caller that still has
// work to do after the swap (recording the operator's approval) can undo it.
// If the swap itself fails, the previous version is already back in place.
func (s *StagedInstall) Commit() error {
	if s.committed {
		return nil
	}
	root, name, staging, err := s.pluginsRoot()
	if err != nil {
		return err
	}
	defer root.Close()
	lock, err := lockInstalls(root)
	if err != nil {
		return err
	}
	holder, err := swapIn(root, name, staging)
	if err != nil {
		_ = unix.Flock(int(lock.Fd()), unix.LOCK_UN)
		_ = lock.Close()
		return err
	}
	s.lock = lock
	s.backupHolder = holder
	s.committed = true
	return nil
}

// Finish discards the previous version kept by Commit.
func (s *StagedInstall) Finish() error {
	if s == nil {
		return nil
	}
	defer s.unlock()
	if s.backupHolder == "" {
		return nil
	}
	root, _, _, err := s.pluginsRoot()
	if err != nil {
		return err
	}
	defer root.Close()
	err = root.RemoveAll(s.backupHolder)
	s.backupHolder = ""
	return err
}

// Rollback undoes a Commit: the new version is removed and the previous one,
// if there was one, is put back.
func (s *StagedInstall) Rollback() error {
	if s == nil || !s.committed {
		return nil
	}
	defer s.unlock()
	root, name, _, err := s.pluginsRoot()
	if err != nil {
		return err
	}
	defer root.Close()
	if err := root.RemoveAll(name); err != nil {
		return fmt.Errorf("plugin: removing new version: %w", err)
	}
	if s.backupHolder != "" {
		if err := root.Rename(path.Join(s.backupHolder, backupName), name); err != nil {
			return fmt.Errorf("plugin: restoring previous version from %s: %w", s.backupHolder, err)
		}
		_ = root.RemoveAll(s.backupHolder)
		s.backupHolder = ""
	}
	s.committed = false
	return nil
}

// Cleanup removes the staging directory when it has not been committed.
func (s *StagedInstall) Cleanup() error {
	if s == nil || s.committed {
		return nil
	}
	root, _, staging, err := s.pluginsRoot()
	if err != nil {
		return err
	}
	defer root.Close()
	return root.RemoveAll(staging)
}

// downloadAndVerify streams the archive to a temp file while hashing it, checks
// the digest against the expected value, and returns the temp path plus the raw
// digest (used for signature verification). An empty wantSHA256 skips the
// check; only StageURL, which has no registry entry to take one from, passes
// one, and its callers' policy decides whether that is acceptable.
func downloadAndVerify(ctx context.Context, rawURL, wantSHA256 string, a auth) (string, []byte, error) {
	body, err := httpsGet(ctx, rawURL, maxUpdateArchiveBytes, a)
	if err != nil {
		return "", nil, err
	}
	defer body.Close()

	tmp, err := os.CreateTemp("", "wings-plugin-*.archive")
	if err != nil {
		return "", nil, err
	}
	h := sha256.New()
	if _, err := io.Copy(io.MultiWriter(tmp, h), body); err != nil {
		tmp.Close()
		os.Remove(tmp.Name())
		return "", nil, err
	}
	tmp.Close()

	digest := h.Sum(nil)
	if got := hex.EncodeToString(digest); wantSHA256 != "" && !strings.EqualFold(got, wantSHA256) {
		os.Remove(tmp.Name())
		return "", nil, fmt.Errorf("plugin: checksum mismatch (want %s, got %s)", wantSHA256, got)
	}
	return tmp.Name(), digest, nil
}

// extractArchive unpacks archivePath into dest. Every write goes through an
// os.Root opened on dest, so no entry name, ".." or symlink can place a file
// outside it.
func extractArchive(ctx context.Context, archivePath, dest string) error {
	f, err := os.Open(archivePath)
	if err != nil {
		return err
	}
	defer f.Close()

	format, input, err := archives.Identify(ctx, filepath.Base(archivePath), f)
	if err != nil {
		return fmt.Errorf("plugin: unrecognized archive format: %w", err)
	}
	ex, ok := format.(archives.Extractor)
	if !ok {
		return errors.New("plugin: archive format is not extractable")
	}
	root, err := os.OpenRoot(dest)
	if err != nil {
		return err
	}
	defer root.Close()

	x := &extractor{root: root, known: map[string]bool{".": true}, remaining: maxUpdateExtractBytes}
	if err := ex.Extract(ctx, input, x.entry); err != nil {
		return err
	}
	return x.createLinks()
}

// extractor writes one archive into root, enforcing the extraction limits.
type extractor struct {
	root *os.Root

	// Symlinks are created only after every file and directory is written, so
	// no entry is ever written through one, and each must then resolve inside
	// the root. Checking each link's text on its own is not enough: a chain of
	// links that each look harmless can still lead outside.
	links []archiveLink

	entries   int             // archive entries read
	created   int             // files, directories and links created
	known     map[string]bool // directories known to exist
	remaining int64           // bytes the rest of the archive may expand to
}

type archiveLink struct{ name, target string }

// entry handles one archive entry: it checks the entry's type, then writes a
// file or directory, or records a symlink for createLinks.
func (x *extractor) entry(_ context.Context, af archives.FileInfo) error {
	x.entries++
	if x.entries > maxUpdateExtractEntries {
		return fmt.Errorf("plugin: archive has more than %d entries", maxUpdateExtractEntries)
	}
	name, err := entryName(af.NameInArchive)
	if err != nil {
		return err
	}
	switch {
	case name == ".":
		// The archive's own top-level directory. Anything else under that
		// name would be skipped unread, outside the size limits.
		if !af.IsDir() {
			return fmt.Errorf("plugin: archive entry %q names the root but is not a directory", af.NameInArchive)
		}
		return nil
	case af.Mode()&os.ModeSymlink != 0:
		if af.LinkTarget == "" || filepath.IsAbs(af.LinkTarget) {
			return fmt.Errorf("plugin: archive symlink %q must have a relative target", af.NameInArchive)
		}
		x.links = append(x.links, archiveLink{name: name, target: af.LinkTarget})
		return nil
	case af.LinkTarget != "":
		return fmt.Errorf("plugin: archive entry %q is a hard link, which is not supported", af.NameInArchive)
	case af.IsDir():
		return x.mkdirs(name)
	case !af.Mode().IsRegular():
		return fmt.Errorf("plugin: archive entry %q is not a regular file, directory or symlink", af.NameInArchive)
	default:
		return x.writeFile(name, af)
	}
}

// writeFile writes a regular file, within the per-file and total size limits.
func (x *extractor) writeFile(name string, af archives.FileInfo) error {
	if err := x.mkdirs(path.Dir(name)); err != nil {
		return err
	}
	if err := x.create(); err != nil {
		return err
	}
	r, err := af.Open()
	if err != nil {
		return err
	}
	defer r.Close()
	out, err := x.root.OpenFile(name, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o755)
	if err != nil {
		return err
	}
	defer out.Close()
	limit := min(int64(maxUpdateFileBytes), x.remaining)
	n, err := copyLimited(out, r, limit)
	x.remaining -= n
	if err != nil {
		if limit < maxUpdateFileBytes {
			return fmt.Errorf("plugin: archive expands to more than %d bytes", maxUpdateExtractBytes)
		}
		return err
	}
	return nil
}

// mkdirs creates dir and any missing parents. Each one it creates counts
// against maxUpdateExtractEntries: one entry "a/b/c/.../f" would otherwise
// create as many directories as its path has parts.
func (x *extractor) mkdirs(dir string) error {
	if x.known[dir] {
		return nil
	}
	if err := x.mkdirs(path.Dir(dir)); err != nil {
		return err
	}
	info, err := x.root.Lstat(dir)
	switch {
	case err == nil:
		if !info.IsDir() {
			return fmt.Errorf("plugin: archive path %q is not a directory", dir)
		}
	case errors.Is(err, fs.ErrNotExist):
		if err := x.create(); err != nil {
			return err
		}
		if err := x.root.Mkdir(dir, 0o755); err != nil {
			return err
		}
	default:
		return err
	}
	x.known[dir] = true
	return nil
}

// create counts one file, directory or symlink about to be created.
func (x *extractor) create() error {
	x.created++
	if x.created > maxUpdateExtractEntries {
		return fmt.Errorf("plugin: archive creates more than %d files and directories", maxUpdateExtractEntries)
	}
	return nil
}

// createLinks creates the recorded symlinks, then checks each resolves inside
// the root. os.Root follows a link only while it stays inside the root, so
// Stat fails for a link that leads outside, directly or through a chain, and
// for a dangling one.
func (x *extractor) createLinks() error {
	for _, l := range x.links {
		if err := x.mkdirs(path.Dir(l.name)); err != nil {
			return err
		}
		if err := x.create(); err != nil {
			return err
		}
		if err := x.root.Symlink(l.target, l.name); err != nil {
			return fmt.Errorf("plugin: creating archive symlink: %w", err)
		}
	}
	for _, l := range x.links {
		if _, err := x.root.Stat(l.name); err != nil {
			return fmt.Errorf("plugin: archive symlink %s points outside the plugin directory or nowhere: %w", l.name, err)
		}
	}
	return nil
}

// maxEntryNameBytes caps an archive entry's name, PATH_MAX on Linux.
const maxEntryNameBytes = 4096

// entryName turns an archive path into a clean, slash-separated name relative
// to the extraction root, refusing absolute paths and ".." (zip-slip). The
// os.Root writes would refuse those anyway; this gives a clearer error.
func entryName(nameInArchive string) (string, error) {
	// Longer names can't be created on Linux anyway (PATH_MAX), and refusing
	// them here bounds how deep mkdirs recurses for one entry.
	if len(nameInArchive) > maxEntryNameBytes {
		return "", fmt.Errorf("plugin: archive entry name is longer than %d bytes", maxEntryNameBytes)
	}
	n := filepath.ToSlash(nameInArchive)
	if path.IsAbs(n) {
		return "", fmt.Errorf("plugin: archive entry escapes destination: %q", nameInArchive)
	}
	n = path.Clean(n)
	if n == ".." || strings.HasPrefix(n, "../") {
		return "", fmt.Errorf("plugin: archive entry escapes destination: %q", nameInArchive)
	}
	return n, nil
}

// copyLimited copies at most max bytes and reports an error if src holds more.
// It returns how many bytes were written.
func copyLimited(dst io.Writer, src io.Reader, max int64) (int64, error) {
	n, err := io.Copy(dst, io.LimitReader(src, max+1))
	if err != nil {
		return n, err
	}
	if n > max {
		return n, fmt.Errorf("plugin: archive entry exceeds maximum file size of %d bytes", max)
	}
	return n, nil
}

// swapIn moves staging to name, both directories inside root. An existing
// name is first moved into a new hidden directory (".wings-backup-*"), whose
// name is returned so the caller can discard or restore it. The backup never
// takes a name another plugin could have: a fixed "<name>.bak" is a valid
// plugin directory, and clearing it out before the swap would delete that
// plugin.
func swapIn(root *os.Root, name, staging string) (string, error) {
	if _, err := root.Lstat(name); errors.Is(err, fs.ErrNotExist) {
		if err := root.Rename(staging, name); err != nil {
			return "", fmt.Errorf("plugin: installing new plugin: %w", err)
		}
		return "", nil
	} else if err != nil {
		return "", err
	}

	holder, err := mkdirHidden(root, ".wings-backup-")
	if err != nil {
		return "", fmt.Errorf("plugin: creating backup directory: %w", err)
	}
	backup := path.Join(holder, backupName)
	if err := root.Rename(name, backup); err != nil {
		_ = root.Remove(holder)
		return "", fmt.Errorf("plugin: backing up current plugin: %w", err)
	}
	if err := root.Rename(staging, name); err != nil {
		_ = root.Rename(backup, name) // roll back
		_ = root.Remove(holder)
		return "", fmt.Errorf("plugin: installing new plugin: %w", err)
	}
	return holder, nil
}
