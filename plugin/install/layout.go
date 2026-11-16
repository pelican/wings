package install

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"strings"

	"github.com/pelican/wings/plugin"
)

// layoutHint says which archive layouts are accepted (contract R1).
const layoutHint = "put " + plugin.ManifestFile + " at the archive's root, or inside a single top-level directory with nothing else next to it"

// normalizeLayout makes an extracted archive's plugin files the top of root.
// The manifest is either at the root already, or inside a single top-level
// directory with nothing else at the top level (how GitHub release archives
// and the Pelican Hub package plugins); then that directory's contents are
// moved up and it is removed. Any other layout is refused.
//
// Everything happens through root, like the extraction, and every symlink is
// checked again afterwards: moving entries up a level changes where a link
// such as "dir/up -> .." leads.
func normalizeLayout(root *os.Root) error {
	entries, err := fs.ReadDir(root.FS(), ".")
	if err != nil {
		return err
	}
	for _, e := range entries {
		if e.Name() == plugin.ManifestFile {
			return nil
		}
	}
	switch {
	case len(entries) == 0:
		return fmt.Errorf("plugin: unsupported archive layout: the archive is empty; %s", layoutHint)
	case len(entries) > 1:
		return fmt.Errorf("plugin: unsupported archive layout: no %s at the archive's root, and it has %d entries at the top level (%s); %s",
			plugin.ManifestFile, len(entries), topLevelNames(entries), layoutHint)
	}
	top := entries[0]
	if !top.IsDir() {
		// fs.DirEntry.IsDir is false for a symlink, even to a directory.
		return fmt.Errorf("plugin: unsupported archive layout: no %s at the archive's root, and its only entry %q is not a directory; %s",
			plugin.ManifestFile, top.Name(), layoutHint)
	}
	if _, err := root.Lstat(path.Join(top.Name(), plugin.ManifestFile)); err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return fmt.Errorf("plugin: unsupported archive layout: no %s at the archive's root or in its top-level directory %q; %s",
				plugin.ManifestFile, top.Name(), layoutHint)
		}
		return err
	}
	if err := hoist(root, top.Name()); err != nil {
		return err
	}
	return checkLinks(root)
}

// hoist moves every entry of the directory dir in root up into root, then
// removes dir. dir is first renamed to a name nothing in it can have, so an
// entry named like dir itself ("demo/demo") can move up too.
func hoist(root *os.Root, dir string) error {
	b := make([]byte, 8)
	if _, err := rand.Read(b); err != nil {
		return err
	}
	tmp := ".wings-layout-" + hex.EncodeToString(b)
	if err := root.Rename(dir, tmp); err != nil {
		return fmt.Errorf("plugin: moving the archive's top-level directory: %w", err)
	}
	inner, err := fs.ReadDir(root.FS(), tmp)
	if err != nil {
		return err
	}
	for _, e := range inner {
		if err := root.Rename(path.Join(tmp, e.Name()), e.Name()); err != nil {
			return fmt.Errorf("plugin: moving %q out of the archive's top-level directory: %w", e.Name(), err)
		}
	}
	return root.Remove(tmp)
}

// checkLinks checks that every symlink in root resolves inside it. os.Root
// follows a link only while it stays inside the root, so Stat fails for one
// that leads outside, directly or through a chain, and for a dangling one.
func checkLinks(root *os.Root) error {
	return fs.WalkDir(root.FS(), ".", func(name string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.Type()&fs.ModeSymlink == 0 {
			return nil
		}
		if _, err := root.Stat(name); err != nil {
			return fmt.Errorf("plugin: archive symlink %s points outside the plugin directory or nowhere", name)
		}
		return nil
	})
}

// topLevelNames lists up to five entry names for an error message.
func topLevelNames(entries []fs.DirEntry) string {
	var names []string
	for i, e := range entries {
		if i == 5 {
			names = append(names, "…")
			break
		}
		names = append(names, e.Name())
	}
	return strings.Join(names, ", ")
}

// stagingError is an error from staging with internal paths removed from its
// message. The staging directory only exists while an install waits for
// approval, and its path means nothing to whoever reads the error (contract
// R1). The original error stays reachable through errors.Is and errors.As.
type stagingError struct {
	err error
	msg string
}

func (e *stagingError) Error() string { return e.msg }
func (e *stagingError) Unwrap() error { return e.err }

// hiddenPrefixes are the names of the hidden directories installs use in the
// plugins directory, followed by random hex.
var hiddenPrefixes = []string{".wings-install-", ".wings-backup-", ".wings-layout-"}

// hideStaging returns err with internal paths removed from its message: a
// path inside stagingDir becomes the path inside the archive, stagingDir
// itself becomes "the archive", and any other staging or backup directory
// name (from an os.Root error, which names it relative to the plugins
// directory) becomes "a temporary directory". stagingDir may be empty. It
// returns nil for nil.
func hideStaging(err error, stagingDir string) error {
	if err == nil {
		return nil
	}
	msg := err.Error()
	clean := msg
	if stagingDir != "" {
		for _, dir := range []string{stagingDir, filepath.Base(stagingDir)} {
			clean = strings.ReplaceAll(clean, dir+string(filepath.Separator), "")
			clean = strings.ReplaceAll(clean, dir, "the archive")
		}
	}
	for _, prefix := range hiddenPrefixes {
		for {
			i := strings.Index(clean, prefix)
			if i < 0 {
				break
			}
			j := i + len(prefix)
			for j < len(clean) && isHex(clean[j]) {
				j++
			}
			clean = clean[:i] + "a temporary directory" + clean[j:]
		}
	}
	if clean == msg {
		return err
	}
	return &stagingError{err: err, msg: clean}
}

func isHex(c byte) bool {
	return '0' <= c && c <= '9' || 'a' <= c && c <= 'f'
}
