package filesystem

import (
	"encoding/json"
	"os"
	"strconv"
	"syscall"
	"time"

	"emperror.dev/errors"
	"github.com/gabriel-vasile/mimetype"
)

type Stat struct {
	os.FileInfo
	Mimetype string
}

func (s *Stat) MarshalJSON() ([]byte, error) {
	return json.Marshal(struct {
		Name      string `json:"name"`
		Created   string `json:"created"`
		Modified  string `json:"modified"`
		Mode      string `json:"mode"`
		ModeBits  string `json:"mode_bits"`
		Size      int64  `json:"size"`
		Directory bool   `json:"directory"`
		File      bool   `json:"file"`
		Symlink   bool   `json:"symlink"`
		Mime      string `json:"mime"`
	}{
		Name:     s.Name(),
		Created:  s.CTime().Format(time.RFC3339),
		Modified: s.ModTime().Format(time.RFC3339),
		Mode:     s.Mode().String(),
		// Using `&ModePerm` on the file's mode will cause the mode to only have the permission values, and nothing else.
		ModeBits:  strconv.FormatUint(uint64(s.Mode()&os.ModePerm), 8),
		Size:      s.Size(),
		Directory: s.IsDir(),
		File:      !s.IsDir(),
		Symlink:   s.Mode().Type()&os.ModeSymlink != 0,
		Mime:      s.Mimetype,
	})
}

// Stat stats a file or folder and returns the base stat object from go along
// with the MIME data that can be used for editing files.
func (fs *Filesystem) Stat(p string) (Stat, error) {
	root, err := fs.openRoot()
	if err != nil {
		return Stat{}, err
	}
	p = normalize(p)
	s, err := root.Stat(p)
	if err != nil {
		return Stat{}, errors.Wrap(err, "server/filesystem: stat: failed to stat file")
	}

	if s.IsDir() {
		return Stat{FileInfo: s, Mimetype: "inode/directory"}, nil
	}

	st := Stat{FileInfo: s, Mimetype: "application/octet-stream"}
	// Only open regular files for mimetype detection; opening a named pipe for
	// reading blocks until a writer appears, which would hang the caller.
	if !s.Mode().IsRegular() {
		return st, nil
	}

	f, err := fs.openRegular(p)
	if err != nil {
		return Stat{}, errors.Wrap(err, "server/filesystem: stat: failed to open file")
	}
	defer f.Close()
	m, err := mimetype.DetectReader(f)
	if err != nil {
		return Stat{}, errors.Wrap(err, "server/filesystem: stat: failed to detect mimetype")
	}
	st.Mimetype = m.String()

	return st, nil
}

// openRegular opens a file for reading, returning an ErrCodeNotRegular error if
// it is not a regular file. It never blocks, even if the file is a named pipe.
func (fs *Filesystem) openRegular(p string) (*os.File, error) {
	root, err := fs.openRoot()
	if err != nil {
		return nil, err
	}
	f, err := root.OpenFile(p, os.O_RDONLY|syscall.O_NONBLOCK, 0)
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
	return f, nil
}

func (fs *Filesystem) Stat2(p string) (os.FileInfo, error) {
	root, err := fs.openRoot()
	if err != nil {
		return nil, err
	}
	st, err := root.Stat(normalize(p))
	if err != nil {
		return st, errors.Wrap(err, "server/filesystem: stat2: failed to stat file")
	}
	return st, nil
}
