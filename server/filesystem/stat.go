package filesystem

import (
	"encoding/json/jsontext"
	jsonv2 "encoding/json/v2"
	"os"
	"strconv"
	"strings"
	"syscall"
	"time"
	"unicode/utf8"

	"emperror.dev/errors"
	"github.com/gabriel-vasile/mimetype"
)

type Stat struct {
	os.FileInfo
	Mimetype string
}

// MarshalJSONTo writes the stat as a JSON object straight to the encoder. It is
// used by encoding/json/v2 and, on Go 1.27+, by encoding/json as well, and skips
// the intermediate struct and buffer copies of the old MarshalJSON path.
func (s *Stat) MarshalJSONTo(enc *jsontext.Encoder) error {
	// Linux filenames can contain any bytes. encoding/json/v2 rejects invalid
	// UTF-8 by default, which would fail a whole directory listing, so replace
	// it here the same way encoding/json v1 always has: one U+FFFD per invalid
	// byte. Ranging over a string yields RuneError for each invalid byte, and
	// strings.Map writes it back out as a real U+FFFD.
	name := s.Name()
	if !utf8.ValidString(name) {
		name = strings.Map(func(r rune) rune { return r }, name)
	}
	mode := s.Mode()
	isDir := s.IsDir()

	if err := enc.WriteToken(jsontext.BeginObject); err != nil {
		return err
	}
	if err := writeTokens(enc, jsontext.String("name"), jsontext.String(name)); err != nil {
		return err
	}
	// The timestamps and mode bits are formatted straight into the encoder's
	// buffer. They only ever contain [0-9A-Z:+-], so they need no escaping.
	if err := writeQuoted(enc, "created", func(b []byte) []byte { return s.CTime().AppendFormat(b, time.RFC3339) }); err != nil {
		return err
	}
	if err := writeQuoted(enc, "modified", func(b []byte) []byte { return s.ModTime().AppendFormat(b, time.RFC3339) }); err != nil {
		return err
	}
	if err := writeTokens(enc, jsontext.String("mode"), jsontext.String(mode.String())); err != nil {
		return err
	}
	// Using `&ModePerm` on the file's mode will cause the mode to only have the permission values, and nothing else.
	if err := writeQuoted(enc, "mode_bits", func(b []byte) []byte { return strconv.AppendUint(b, uint64(mode&os.ModePerm), 8) }); err != nil {
		return err
	}
	return writeTokens(enc,
		jsontext.String("size"), jsontext.Int(s.Size()),
		jsontext.String("directory"), jsontext.Bool(isDir),
		jsontext.String("file"), jsontext.Bool(!isDir),
		jsontext.String("symlink"), jsontext.Bool(mode.Type()&os.ModeSymlink != 0),
		jsontext.String("mime"), jsontext.String(s.Mimetype),
		jsontext.EndObject,
	)
}

func writeTokens(enc *jsontext.Encoder, toks ...jsontext.Token) error {
	for _, tok := range toks {
		if err := enc.WriteToken(tok); err != nil {
			return err
		}
	}
	return nil
}

// writeQuoted writes key followed by a JSON string whose contents fn appends.
// fn must only append characters that never need escaping in JSON.
func writeQuoted(enc *jsontext.Encoder, key string, fn func([]byte) []byte) error {
	if err := enc.WriteToken(jsontext.String(key)); err != nil {
		return err
	}
	b := append(enc.AvailableBuffer(), '"')
	b = append(fn(b), '"')
	return enc.WriteValue(b)
}

// MarshalJSON keeps Stat working with encoders that only know the v1 interface,
// such as goccy/go-json. encoding/json/v2 prefers MarshalJSONTo, so this does
// not recurse. The escaping options keep the output byte-identical to what
// encoding/json v1 has always produced, and since MarshalJSONTo always writes
// a fixed set of unique keys, the encoder's duplicate-key check is skipped.
func (s *Stat) MarshalJSON() ([]byte, error) {
	return jsonv2.Marshal(s, statMarshalOptions)
}

var statMarshalOptions = jsonv2.JoinOptions(
	jsontext.EscapeForHTML(true),
	jsontext.EscapeForJS(true),
	jsontext.AllowDuplicateNames(true),
)

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
