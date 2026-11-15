package plugins

import (
	"archive/zip"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"emperror.dev/errors"
	"github.com/apex/log"
	"github.com/goccy/go-json"

	"github.com/pelican/wings/system"
)

// InstallFromArchive unpacks a plugin zip into the plugin directory and returns
// the id of the plugin it installed.
//
// The archive may hold the plugin at its root or inside a single top level
// folder, because both are what people actually produce: the first is what you
// get zipping a directory's contents, the second what you get zipping the
// directory. Whichever it is, the plugin lands at <directory>/<id>, with id
// taken from the manifest rather than from the file name, so a renamed download
// still installs correctly.
//
// Passing expectedID restricts the archive to that one plugin, which is what an
// update does: without it, an update fetched from a compromised update_url
// could replace a different plugin than the one being updated.
func (m *Manager) InstallFromArchive(path string, expectedID string) (string, error) {
	if !m.cfg.Enabled {
		return "", errors.New("plugins: the plugin subsystem is disabled on this node")
	}

	maxBytes := m.cfg.MaxInstallSize * 1024 * 1024
	if maxBytes <= 0 {
		maxBytes = 16 * 1024 * 1024
	}

	zr, err := zip.OpenReader(path)
	if err != nil {
		return "", errors.Wrap(err, "plugins: could not open the plugin archive")
	}
	defer zr.Close()

	// The compressed size says nothing about what the archive expands to, so
	// total the declared uncompressed sizes and refuse before writing
	// anything. A plugin directory sits on the node's root volume alongside
	// the database and every server's logs, so filling it is not a
	// plugin-shaped problem.
	var uncompressed uint64
	for _, f := range zr.File {
		name := f.Name

		// Reject traversal and absolute paths outright rather than
		// sanitising them. An archive containing "../../etc" is not a plugin
		// that needs fixing up.
		if strings.Contains(name, "..") || strings.HasPrefix(name, "/") || strings.HasPrefix(name, `\`) {
			return "", errors.Errorf("plugins: the archive contains an unsafe path: %s", name)
		}

		uncompressed += f.UncompressedSize64
		if uncompressed > uint64(maxBytes) {
			return "", errors.Errorf("plugins: the archive expands to more than the %d MiB limit", m.cfg.MaxInstallSize)
		}
	}

	// Stage inside the plugin directory so the final move is a rename on one
	// filesystem rather than a copy across two, and name it as a dotfile so
	// Discover ignores it while it is being assembled.
	staging, err := os.MkdirTemp(m.cfg.Directory, ".import-")
	if err != nil {
		return "", errors.Wrap(err, "plugins: could not create a staging directory")
	}
	defer os.RemoveAll(staging)

	for _, f := range zr.File {
		if err := extractZipEntry(f, staging); err != nil {
			return "", err
		}
	}

	manifestPath, err := locateManifest(staging)
	if err != nil {
		return "", err
	}

	source := filepath.Dir(manifestPath)

	manifest, err := LoadManifest(source)
	if err != nil {
		// LoadManifest validates the id against the directory name, which here
		// is the staging directory, so re-read just the id and report the real
		// problem rather than a confusing mismatch against ".import-xxxx".
		id, idErr := manifestID(manifestPath)
		if idErr != nil {
			return "", err
		}
		if !idPattern.MatchString(id) {
			return "", errors.Errorf("plugins: the archive declares an unusable plugin id %q", id)
		}
		// Re-validate against the id the manifest itself claims.
		if reErr := revalidateStaged(source, id); reErr != nil {
			return "", reErr
		}
		manifest, err = LoadManifest(source)
		if err != nil {
			return "", err
		}
	}

	id := manifest.ID
	if expectedID != "" && id != expectedID {
		return "", errors.Errorf("plugins: the archive contains %q but %q was being updated", id, expectedID)
	}

	target := filepath.Join(m.cfg.Directory, id)

	// A plugin being replaced is set aside rather than deleted, so a failed
	// move leaves the previous version in place instead of nothing at all.
	var rollback string
	if _, err := os.Stat(target); err == nil {
		rollback = filepath.Join(m.cfg.Directory, "."+id+".bak")
		_ = os.RemoveAll(rollback)

		if err := os.Rename(target, rollback); err != nil {
			return "", errors.Wrap(err, "plugins: could not set the existing plugin aside")
		}
	}

	if err := os.Rename(source, target); err != nil {
		if rollback != "" {
			if rbErr := os.Rename(rollback, target); rbErr != nil {
				// Both the move and the rollback failed, which leaves the
				// plugin only in the backup directory. Say so explicitly:
				// the operator needs to know where their plugin went.
				return "", errors.Wrapf(err, "plugins: could not install the plugin, and the previous version could not be restored either; it is at %s", rollback)
			}
		}
		return "", errors.Wrap(err, "plugins: could not move the plugin into place")
	}

	if rollback != "" {
		_ = os.RemoveAll(rollback)
	}

	// Pick the new plugin up straight away so the caller can install or enable
	// it without waiting for another scan.
	m.mu.Lock()
	delete(m.instances, id)
	m.mu.Unlock()

	if err := m.Discover(); err != nil {
		return id, errors.Wrap(err, "plugins: the plugin was installed but could not be read back")
	}

	m.log().WithFields(log.Fields{"plugin": id, "version": manifest.Version}).
		Info("imported plugin")

	return id, nil
}

// revalidateStaged renames the staged plugin folder to its declared id so the
// manifest's directory-name check lines up.
func revalidateStaged(source, id string) error {
	renamed := filepath.Join(filepath.Dir(source), id)
	if source == renamed {
		return nil
	}
	if err := os.Rename(source, renamed); err != nil {
		return errors.Wrap(err, "plugins: could not stage the plugin under its own id")
	}
	return nil
}

// manifestID reads only the id out of a manifest, for error reporting when the
// full parse has already failed.
func manifestID(path string) (string, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return "", errors.Wrap(err, "plugins: could not read the archive's plugin.json")
	}
	var probe struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal(b, &probe); err != nil {
		return "", errors.Wrap(err, "plugins: the archive's plugin.json is not valid JSON")
	}
	return strings.ToLower(strings.TrimSpace(probe.ID)), nil
}

// extractZipEntry writes one archive entry beneath root.
func extractZipEntry(f *zip.File, root string) error {
	// Resolve and re-check against the root. The name was already screened for
	// traversal, but doing the containment check on the joined path is what
	// actually guarantees nothing lands outside.
	dest := filepath.Join(root, filepath.FromSlash(f.Name))
	if !strings.HasPrefix(dest, filepath.Clean(root)+string(os.PathSeparator)) {
		return errors.Errorf("plugins: the archive entry %s would be written outside the staging directory", f.Name)
	}

	if f.FileInfo().IsDir() {
		return os.MkdirAll(dest, 0o700)
	}

	// Symlinks are not extracted at all. A plugin is source to be read, and a
	// link is a way to make the directory describe something other than what
	// it appears to.
	if f.Mode()&os.ModeSymlink != 0 {
		return errors.Errorf("plugins: the archive contains a symlink (%s), which plugins may not include", f.Name)
	}

	if err := os.MkdirAll(filepath.Dir(dest), 0o700); err != nil {
		return errors.Wrap(err, "plugins: could not create a directory while extracting")
	}

	rc, err := f.Open()
	if err != nil {
		return errors.Wrapf(err, "plugins: could not read %s from the archive", f.Name)
	}
	defer rc.Close()

	out, err := os.OpenFile(dest, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o600)
	if err != nil {
		return errors.Wrapf(err, "plugins: could not write %s", f.Name)
	}
	defer out.Close()

	// Copy with the declared size as the ceiling so an entry whose real
	// content exceeds its header cannot write past what was budgeted.
	if _, err := io.CopyN(out, rc, int64(f.UncompressedSize64)); err != nil && !errors.Is(err, io.EOF) {
		return errors.Wrapf(err, "plugins: could not extract %s", f.Name)
	}

	return nil
}

// locateManifest finds the plugin.json in an extracted archive, at its root or
// inside a single top level directory.
func locateManifest(root string) (string, error) {
	direct := filepath.Join(root, ManifestName)
	if _, err := os.Stat(direct); err == nil {
		return direct, nil
	}

	entries, err := os.ReadDir(root)
	if err != nil {
		return "", errors.Wrap(err, "plugins: could not read the extracted archive")
	}

	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		nested := filepath.Join(root, e.Name(), ManifestName)
		if _, err := os.Stat(nested); err == nil {
			return nested, nil
		}
	}

	return "", errors.New("plugins: the archive does not contain a plugin.json")
}

// InstallFromURL downloads a plugin archive and installs it.
func (m *Manager) InstallFromURL(url string, expectedID string) (string, error) {
	if !m.cfg.Enabled {
		return "", errors.New("plugins: the plugin subsystem is disabled on this node")
	}

	maxBytes := m.cfg.MaxInstallSize * 1024 * 1024
	if maxBytes <= 0 {
		maxBytes = 16 * 1024 * 1024
	}

	client := &http.Client{Timeout: 2 * time.Minute}

	res, err := client.Get(url)
	if err != nil {
		return "", errors.Wrap(err, "plugins: could not download the plugin archive")
	}
	defer res.Body.Close()

	if res.StatusCode < 200 || res.StatusCode > 299 {
		return "", errors.Errorf("plugins: downloading the plugin archive returned HTTP %d", res.StatusCode)
	}

	tmp, err := os.CreateTemp("", "wings-plugin-*.zip")
	if err != nil {
		return "", errors.Wrap(err, "plugins: could not create a temporary file for the download")
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName)

	// Cap the download itself rather than trusting Content-Length, which the
	// far end controls and may simply not send.
	written, err := io.Copy(tmp, io.LimitReader(res.Body, maxBytes+1))
	tmp.Close()
	if err != nil {
		return "", errors.Wrap(err, "plugins: could not save the downloaded archive")
	}
	if written > maxBytes {
		return "", errors.Errorf("plugins: the download is larger than the %d MiB limit", m.cfg.MaxInstallSize)
	}

	return m.InstallFromArchive(tmpName, expectedID)
}

// updateDocument is the shape of an update_url document. It matches what the
// Panel's plugin updater reads, so one document can describe both halves of a
// plugin that has a Panel side and a Wings side.
type updateDocument map[string]struct {
	Version     string `json:"version"`
	DownloadURL string `json:"download_url"`
}

// UpdateAvailable reports whether the plugin's update_url offers a newer
// version, and the URL to fetch it from.
func (m *Manager) UpdateAvailable(id string) (bool, string, error) {
	inst, ok := m.Get(id)
	if !ok {
		return false, "", errors.Errorf("plugins: %s is not on this node", id)
	}

	manifest := inst.Manifest()
	if manifest.UpdateURL == "" {
		return false, "", nil
	}

	client := &http.Client{Timeout: 10 * time.Second}
	res, err := client.Get(manifest.UpdateURL)
	if err != nil {
		return false, "", errors.Wrap(err, "plugins: could not reach the plugin's update url")
	}
	defer res.Body.Close()

	if res.StatusCode < 200 || res.StatusCode > 299 {
		return false, "", errors.Errorf("plugins: the plugin's update url returned HTTP %d", res.StatusCode)
	}

	body, err := io.ReadAll(io.LimitReader(res.Body, 1<<20))
	if err != nil {
		return false, "", errors.Wrap(err, "plugins: could not read the update document")
	}

	var doc updateDocument
	if err := json.Unmarshal(body, &doc); err != nil {
		return false, "", errors.Wrap(err, "plugins: the update document is not in the expected format")
	}

	// A document may describe several plugins keyed by id, or one plugin keyed
	// by the Wings version it targets, with "*" as the catch-all.
	if entry, ok := doc[id]; ok {
		return newerThan(entry.Version, manifest.Version), entry.DownloadURL, nil
	}

	if v := comparableVersion(system.Version); v != "" {
		if entry, ok := doc[v]; ok {
			return newerThan(entry.Version, manifest.Version), entry.DownloadURL, nil
		}
	}

	if entry, ok := doc["*"]; ok {
		return newerThan(entry.Version, manifest.Version), entry.DownloadURL, nil
	}

	return false, "", nil
}

// Update downloads and installs the newest version of a plugin, then restores
// the state it was in.
//
// A plugin that was enabled is re-enabled afterwards, because an update the
// operator asked for should not quietly leave the plugin switched off. If the
// new version fails to load, it is left errored with the reason rather than
// rolled back: the files on disk are the new version, and pretending otherwise
// would make the manifest disagree with the directory.
func (m *Manager) Update(id string) error {
	available, url, err := m.UpdateAvailable(id)
	if err != nil {
		return err
	}
	if !available || url == "" {
		return errors.Errorf("plugins: %s has no update available", id)
	}

	inst, ok := m.Get(id)
	if !ok {
		return errors.Errorf("plugins: %s is not on this node", id)
	}

	wasEnabled := inst.Status() == StatusEnabled
	if inst.Loaded() {
		m.unregisterExtensions(inst)
		inst.unload()
	}

	if _, err := m.InstallFromURL(url, id); err != nil {
		return err
	}

	updated, ok := m.Get(id)
	if !ok {
		return errors.Errorf("plugins: %s disappeared while being updated", id)
	}

	if wasEnabled {
		m.persistStatus(updated, StatusDisabled, "")
		return m.Enable(id)
	}

	m.refreshDispatch()
	return nil
}

// newerThan reports whether candidate is a later version than current.
func newerThan(candidate, current string) bool {
	if candidate == "" {
		return false
	}
	return compareVersions(candidate, current) > 0
}
