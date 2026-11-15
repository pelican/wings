package install

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
)

// Digest hashes the staged directory: every path, its type and mode, file
// contents and link targets. The Panel API ties an approval token to it, so
// what is committed is what was shown when the token was issued.
func (s *StagedInstall) Digest() (string, error) {
	return dirDigest(s.StagingDir)
}

// DestinationState hashes what is at DestDir now, like Digest, or returns ""
// when nothing is there. Comparing it with the value at staging time tells
// whether the plugin was installed, removed or changed in between.
func (s *StagedInstall) DestinationState() (string, error) {
	if _, err := os.Lstat(s.DestDir); errors.Is(err, fs.ErrNotExist) {
		return "", nil
	} else if err != nil {
		return "", err
	}
	return dirDigest(s.DestDir)
}

func dirDigest(dir string) (string, error) {
	root, err := os.OpenRoot(dir)
	if err != nil {
		return "", err
	}
	defer root.Close()
	h := sha256.New()
	write := func(parts ...string) {
		for _, p := range parts {
			io.WriteString(h, p)
			h.Write([]byte{0})
		}
	}
	err = fs.WalkDir(root.FS(), ".", func(name string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		info, err := d.Info()
		if err != nil {
			return err
		}
		mode := info.Mode()
		switch {
		case d.IsDir():
			write(name, "dir", mode.String())
		case mode&fs.ModeSymlink != 0:
			target, err := root.Readlink(name)
			if err != nil {
				return err
			}
			write(name, "symlink", target)
		case mode.IsRegular():
			f, err := root.Open(name)
			if err != nil {
				return err
			}
			fh := sha256.New()
			_, err = io.Copy(fh, f)
			f.Close()
			if err != nil {
				return err
			}
			write(name, "file", mode.String(), hex.EncodeToString(fh.Sum(nil)))
		default:
			return fmt.Errorf("plugin: unexpected file type in plugin directory: %s", name)
		}
		return nil
	})
	if err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}
