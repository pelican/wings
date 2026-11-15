package install

import (
	"context"
	"crypto/sha256"
	"fmt"
	"io"
	"os"
	"path/filepath"

	"github.com/pelican/wings/plugin"
)

// MaxArchiveBytes is the largest plugin archive Wings downloads or accepts as
// an upload.
const MaxArchiveBytes = maxUpdateArchiveBytes

// StageURL downloads a plugin archive from an https URL and stages it in
// pluginsDir. Unlike StageAdd there is no registry entry: the plugin's name
// comes from the archive's own manifest, and DestDir is set from it.
// wantSHA256 and signature are optional; the signing policy applies as for a
// registry install (plugins.require_signatures, or RequireTrustedSignature).
func StageURL(ctx context.Context, rawURL, wantSHA256, signature, pluginsDir string, opts ...StageOption) (*StagedInstall, error) {
	archivePath, digest, err := downloadAndVerify(ctx, rawURL, wantSHA256, auth{})
	if err != nil {
		return nil, err
	}
	defer os.Remove(archivePath)
	if info, err := os.Stat(archivePath); err != nil {
		return nil, err
	} else if info.Size() >= maxUpdateArchiveBytes {
		// httpsGet stops reading at the cap, so an archive this size was
		// probably cut short.
		return nil, fmt.Errorf("plugin: archive is larger than %d bytes", maxUpdateArchiveBytes)
	}
	return stageNamedByManifest(ctx, archivePath, digest, signature, pluginsDir, applyStageOptions(opts))
}

// StageFile stages a plugin archive that is already on this machine, such as
// an upload. Like StageURL, the plugin's name comes from its manifest.
func StageFile(ctx context.Context, archivePath, signature, pluginsDir string, opts ...StageOption) (*StagedInstall, error) {
	f, err := os.Open(archivePath)
	if err != nil {
		return nil, err
	}
	h := sha256.New()
	n, err := io.Copy(h, io.LimitReader(f, maxUpdateArchiveBytes+1))
	f.Close()
	if err != nil {
		return nil, err
	}
	if n > maxUpdateArchiveBytes {
		return nil, fmt.Errorf("plugin: archive is larger than %d bytes", maxUpdateArchiveBytes)
	}
	return stageNamedByManifest(ctx, archivePath, h.Sum(nil), signature, pluginsDir, applyStageOptions(opts))
}

func stageNamedByManifest(ctx context.Context, archivePath string, digest []byte, signature, pluginsDir string, o stageOptions) (*StagedInstall, error) {
	if err := os.MkdirAll(pluginsDir, 0o755); err != nil {
		return nil, fmt.Errorf("plugin: creating plugins directory: %w", err)
	}
	staging, err := extractToStaging(ctx, archivePath, digest, signature, pluginsDir, o)
	if err != nil {
		return nil, err
	}
	// DestDir is set below; until then Cleanup only needs the plugins
	// directory, which is the staging directory's parent.
	stage := &StagedInstall{DestDir: filepath.Join(pluginsDir, ".unnamed"), StagingDir: staging}
	m, err := stage.Manifest()
	if err != nil {
		_ = stage.Cleanup()
		return nil, err
	}
	// LoadManifest has checked the name; check again here, where it becomes
	// a path, so a change there can never point DestDir at the plugins
	// directory itself or outside it.
	if plugin.InvalidName(m.Name) {
		_ = stage.Cleanup()
		return nil, fmt.Errorf("plugin: invalid plugin name %q", m.Name)
	}
	stage.DestDir = filepath.Join(pluginsDir, m.Name)
	return stage, nil
}
