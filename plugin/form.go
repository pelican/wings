package plugin

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"sync"
	"sync/atomic"
	"syscall"
	"unicode/utf8"
)

// Form says whether a plugin's code can be read (contract S, "Disclosure at
// approval").
type Form string

const (
	// FormSource: every file in the plugin's directory is text.
	FormSource Form = "source"
	// FormBinary: at least one file isn't text, such as a compiled program.
	FormBinary Form = "binary"
	// FormNative: compiled into Wings.
	FormNative Form = "native"
)

// MaxBinaryFiles is how many binary files FormInfo lists.
const MaxBinaryFiles = 20

// FormInfo is a plugin directory's form and its binary files.
type FormInfo struct {
	Form Form
	// BinaryFiles are the first MaxBinaryFiles non-text files, as
	// slash-separated paths relative to the plugin's directory, in path
	// order. Empty (not nil) for a source plugin.
	BinaryFiles []string
	// BinaryCount is how many non-text files there are in all.
	BinaryCount int
}

// DetectForm classifies the plugin in dir. A file is text when it is valid
// UTF-8 and has no NUL bytes; a plugin with any other regular file, or any
// special file, is binary. Symlinks are not followed: the files they point to
// inside dir are classified where they are, and the approval refuses links
// that leave it. Reading stops at a file's first byte that isn't text, so a
// compiled program costs a few bytes; text files are read whole.
func DetectForm(dir string) (FormInfo, error) {
	formDetections.Add(1)
	root, err := os.OpenRoot(dir)
	if err != nil {
		return FormInfo{}, err
	}
	defer root.Close()
	out := FormInfo{Form: FormSource, BinaryFiles: []string{}}
	err = fs.WalkDir(root.FS(), ".", func(name string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		switch t := d.Type(); {
		case d.IsDir(), t&fs.ModeSymlink != 0:
			return nil
		case t.IsRegular():
			text, err := fileIsText(root, name)
			if err != nil || text {
				return err
			}
		}
		out.Form = FormBinary
		out.BinaryCount++
		if len(out.BinaryFiles) < MaxBinaryFiles {
			out.BinaryFiles = append(out.BinaryFiles, name)
		}
		return nil
	})
	if err != nil {
		return FormInfo{}, fmt.Errorf("plugin: reading the plugin's files: %w", err)
	}
	return out, nil
}

// formDetections counts DetectForm calls, for tests of the cache.
var formDetections atomic.Int64

func fileIsText(root *os.Root, name string) (bool, error) {
	f, err := root.Open(name)
	if err != nil {
		return false, err
	}
	defer f.Close()
	return isText(f)
}

// isText reports whether r is valid UTF-8 with no NUL bytes. It reads in
// blocks, carrying a rune cut off at the end of one block over to the next.
func isText(r io.Reader) (bool, error) {
	buf := make([]byte, 64<<10)
	carry := 0
	for {
		n, err := r.Read(buf[carry:])
		data := buf[:carry+n]
		// The carried bytes are the start of a multi-byte rune: never NUL.
		if bytes.IndexByte(data[carry:], 0) >= 0 {
			return false, nil
		}
		end := len(data)
		if err == nil {
			end = completeRunes(data)
		}
		if !utf8.Valid(data[:end]) {
			return false, nil
		}
		carry = copy(buf, data[end:])
		if errors.Is(err, io.EOF) {
			return true, nil
		}
		if err != nil {
			return false, err
		}
	}
}

// completeRunes returns how much of b ends on a rune boundary: all of it,
// unless its last rune is cut off.
func completeRunes(b []byte) int {
	for i := len(b) - 1; i >= 0 && i >= len(b)-utf8.UTFMax; i-- {
		if utf8.RuneStart(b[i]) {
			if utf8.FullRune(b[i:]) {
				return len(b)
			}
			return i
		}
	}
	return len(b)
}

// CachedForm is DetectForm, remembered per directory until any file or
// directory in it changes. Whether one changed is judged from metadata only
// (path, type, mode, size, inode, modification and change time), so listing
// unchanged plugins costs one lstat per file rather than reading them.
func CachedForm(dir string) (FormInfo, error) {
	sig, err := statSignature(dir)
	if err != nil {
		return FormInfo{}, err
	}
	formCache.Lock()
	e, ok := formCache.entries[dir]
	formCache.Unlock()
	if ok && e.sig == sig {
		return e.info.clone(), nil
	}
	info, err := DetectForm(dir)
	if err != nil {
		return FormInfo{}, err
	}
	formCache.Lock()
	defer formCache.Unlock()
	if formCache.entries == nil || len(formCache.entries) >= maxFormCache {
		formCache.entries = map[string]formCacheEntry{}
	}
	formCache.entries[dir] = formCacheEntry{sig: sig, info: info}
	return info.clone(), nil
}

// maxFormCache bounds the cache; it starts over when full.
const maxFormCache = 256

var formCache struct {
	sync.Mutex
	entries map[string]formCacheEntry
}

type formCacheEntry struct {
	sig  string
	info FormInfo
}

func (f FormInfo) clone() FormInfo {
	f.BinaryFiles = append([]string{}, f.BinaryFiles...)
	return f
}

// statSignature hashes the metadata of every entry under dir.
func statSignature(dir string) (string, error) {
	root, err := os.OpenRoot(dir)
	if err != nil {
		return "", err
	}
	defer root.Close()
	h := sha256.New()
	err = fs.WalkDir(root.FS(), ".", func(name string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		info, err := d.Info()
		if err != nil {
			return err
		}
		var ino uint64
		if st, ok := info.Sys().(*syscall.Stat_t); ok {
			ino = uint64(st.Ino)
		}
		fmt.Fprintf(h, "%s\x00%d\x00%d\x00%d\x00%d\x00%d\x00", name, info.Mode(), info.Size(), ino, info.ModTime().UnixNano(), changeTime(info))
		return nil
	})
	if err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}
