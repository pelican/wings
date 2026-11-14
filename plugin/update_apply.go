package plugin

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/mholt/archives"
)

// ApplyUpdate downloads the archive named in a prior CheckUpdate, verifies its
// sha256, extracts it to a staging directory next to the plugin, then atomically
// swaps it into place (keeping a ".bak" of the old version, restored on failure).
// The archive is expected to contain the plugin's files at its root (plugin.yaml
// at the top). A running plugin process must be restarted to pick up the change.
func ApplyUpdate(ctx context.Context, st UpdateStatus) error {
	stage, err := StageUpdate(ctx, st)
	if err != nil {
		return err
	}
	defer stage.Cleanup()
	return stage.Commit()
}

// StageUpdate downloads, verifies, and extracts an update without replacing the
// live plugin directory. Callers can inspect stage.Manifest before Commit.
func StageUpdate(ctx context.Context, st UpdateStatus) (*StagedInstall, error) {
	if st.download == "" || st.sha256 == "" || st.dir == "" {
		return nil, errors.New("plugin: incomplete update status; run CheckUpdate first")
	}
	return stageArchive(ctx, st.download, st.sha256, st.signature, st.dir, auth{})
}

func stageArchive(ctx context.Context, download, wantSHA256, signature, destDir string, a auth) (*StagedInstall, error) {
	archivePath, digest, err := downloadAndVerify(ctx, download, wantSHA256, a)
	if err != nil {
		return nil, err
	}
	defer os.Remove(archivePath)

	if err := enforceSignature(digest, signature); err != nil {
		return nil, err
	}

	// Stage inside destDir's parent so the final swap is an atomic
	// same-filesystem rename (avoids a cross-device copy).
	parent := filepath.Dir(destDir)
	if err := os.MkdirAll(parent, 0o755); err != nil {
		return nil, err
	}
	staging, err := os.MkdirTemp(parent, ".wings-install-*")
	if err != nil {
		return nil, err
	}

	if err := extractArchive(ctx, archivePath, staging); err != nil {
		_ = os.RemoveAll(staging)
		return nil, err
	}
	return &StagedInstall{DestDir: destDir, StagingDir: staging}, nil
}

// StagedInstall is an extracted, verified plugin archive waiting to be swapped
// into its final directory.
type StagedInstall struct {
	DestDir    string
	StagingDir string
	committed  bool
}

// Manifest loads the staged plugin manifest without executing plugin code.
func (s *StagedInstall) Manifest() (Manifest, error) {
	return LoadManifest(filepath.Join(s.StagingDir, ManifestFile))
}

// Commit atomically replaces DestDir with StagingDir.
func (s *StagedInstall) Commit() error {
	if s.committed {
		return nil
	}
	if err := swapDir(s.DestDir, s.StagingDir); err != nil {
		return err
	}
	s.committed = true
	return nil
}

// Cleanup removes the staging directory when it has not been committed.
func (s *StagedInstall) Cleanup() error {
	if s == nil || s.committed {
		return nil
	}
	return os.RemoveAll(s.StagingDir)
}

// downloadAndVerify streams the archive to a temp file while hashing it, checks
// the digest against the expected value, and returns the temp path plus the raw
// digest (used for signature verification).
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
	if got := hex.EncodeToString(digest); !strings.EqualFold(got, wantSHA256) {
		os.Remove(tmp.Name())
		return "", nil, fmt.Errorf("plugin: checksum mismatch (want %s, got %s)", wantSHA256, got)
	}
	return tmp.Name(), digest, nil
}

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
	var entries int
	remaining := maxUpdateExtractBytes
	return ex.Extract(ctx, input, func(ctx context.Context, af archives.FileInfo) error {
		entries++
		if entries > maxUpdateExtractEntries {
			return fmt.Errorf("plugin: archive has more than %d entries", maxUpdateExtractEntries)
		}
		target, err := safeJoin(dest, af.NameInArchive)
		if err != nil {
			return err
		}
		if af.IsDir() {
			return os.MkdirAll(target, 0o755)
		}
		if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
			return err
		}
		r, err := af.Open()
		if err != nil {
			return err
		}
		defer r.Close()
		out, err := os.OpenFile(target, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o755)
		if err != nil {
			return err
		}
		defer out.Close()
		limit := int64(maxUpdateFileBytes)
		if remaining < limit {
			limit = remaining
		}
		n, err := copyLimited(out, r, limit)
		remaining -= n
		if err != nil {
			if limit < maxUpdateFileBytes {
				return fmt.Errorf("plugin: archive expands to more than %d bytes", maxUpdateExtractBytes)
			}
			return err
		}
		return nil
	})
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

// safeJoin joins name onto base and guarantees the result stays within base,
// guarding against archive path traversal (zip-slip).
func safeJoin(base, name string) (string, error) {
	target := filepath.Join(base, name)
	if target != base && !strings.HasPrefix(target, base+string(os.PathSeparator)) {
		return "", fmt.Errorf("plugin: archive entry escapes destination: %q", name)
	}
	return target, nil
}

// swapDir replaces dst with the freshly-extracted staging directory, keeping a
// ".bak" of the old one that is restored if the swap fails.
func swapDir(dst, staging string) error {
	backup := dst + ".bak"
	_ = os.RemoveAll(backup)

	if _, err := os.Stat(dst); err == nil {
		if err := os.Rename(dst, backup); err != nil {
			return fmt.Errorf("plugin: backing up current plugin: %w", err)
		}
	}
	if err := os.Rename(staging, dst); err != nil {
		_ = os.Rename(backup, dst) // roll back
		return fmt.Errorf("plugin: installing new plugin: %w", err)
	}
	_ = os.RemoveAll(backup)
	return nil
}
