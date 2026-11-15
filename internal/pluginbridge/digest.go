package pluginbridge

import (
	"crypto/md5"
	"crypto/sha1"
	"crypto/sha256"
	"crypto/sha512"
	"encoding/hex"
	"hash"
	"hash/crc32"
	"io"
	"strconv"
	"strings"

	"emperror.dev/errors"

	"github.com/pelican/wings/plugins/api"
	"github.com/pelican/wings/server"
)

// digestBufferSize is the chunk size used while streaming a file through the
// hashers. Large enough that the per-read overhead disappears against the cost
// of hashing, small enough that a dozen concurrent digests are not a memory
// problem.
const digestBufferSize = 512 * 1024

// FileDigest hashes a file on the node and returns the results by name.
//
// This exists because plugin code is interpreted. A plugin hashing a few dozen
// files by reading them and doing the arithmetic itself runs at a small
// fraction of native speed, which for the obvious uses was the slowest thing
// in the operation. Doing it here is native, and streams rather than holding
// whole files in memory.
//
// Wings supplies primitives and nothing above them. A caller that wants some
// ecosystem's particular file fingerprint describes it as an algorithm plus
// options and names the result itself, so no format knowledge lives here.
func (b *Bridge) FileDigest(uuid, path string, requests []api.DigestRequest) (map[string]string, error) {
	s, err := b.find(uuid)
	if err != nil {
		return nil, err
	}

	prepared, err := prepareDigests(requests)
	if err != nil {
		return nil, err
	}

	// MurmurHash2 mixes the input length into its initial state, so the length
	// has to be known before hashing starts. Rather than buffer a whole file
	// to find it, count in one pass and hash in a second. Reading twice off
	// local disk costs far less than holding every file being hashed in
	// memory, and nothing else needs the extra pass.
	if needsLength(prepared) {
		raw, stripped, err := b.measure(s, path)
		if err != nil {
			return nil, err
		}

		for i := range prepared {
			length := raw
			if prepared[i].ignoreWhitespace {
				length = stripped
			}
			prepared[i].start(length)
		}
	} else {
		for i := range prepared {
			prepared[i].start(0)
		}
	}

	if err := b.streamFile(s, path, digestWriter(prepared)); err != nil {
		return nil, err
	}

	out := make(map[string]string, len(prepared))
	for _, p := range prepared {
		out[p.name] = p.result()
	}
	return out, nil
}

// preparedDigest is one requested hash, ready to be fed.
type preparedDigest struct {
	name             string
	algorithm        string
	ignoreWhitespace bool
	seed             uint32

	h      hash.Hash
	murmur *murmur2Writer
}

// start initialises the hash now that the content length is known, which only
// matters for murmur2.
func (p *preparedDigest) start(length uint32) {
	if p.algorithm == "murmur2" {
		p.murmur = newMurmur2(p.seed, length)
		return
	}
	p.h = newHasher(p.algorithm)
}

func (p *preparedDigest) writer() io.Writer {
	if p.murmur != nil {
		return p.murmur
	}
	return p.h
}

func (p *preparedDigest) result() string {
	if p.murmur != nil {
		return strconv.FormatUint(uint64(p.murmur.sum()), 10)
	}

	// crc32 is reported as a decimal number, the way callers of a checksum
	// expect it, rather than as hex.
	if h32, ok := p.h.(hash.Hash32); ok && p.algorithm == "crc32" {
		return strconv.FormatUint(uint64(h32.Sum32()), 10)
	}
	return hex.EncodeToString(p.h.Sum(nil))
}

// prepareDigests validates the requests and resolves their names.
func prepareDigests(requests []api.DigestRequest) ([]preparedDigest, error) {
	if len(requests) == 0 {
		return nil, errors.New("pluginbridge: no digests were requested")
	}

	out := make([]preparedDigest, 0, len(requests))
	used := map[string]bool{}

	for _, req := range requests {
		algorithm := strings.ToLower(strings.TrimSpace(req.Algorithm))

		switch algorithm {
		case "md5", "sha1", "sha256", "sha512", "crc32", "murmur2":
		default:
			// An unknown name is an error rather than a missing key, so a typo
			// shows up instead of quietly producing nothing.
			return nil, errors.Errorf("pluginbridge: %q is not a digest algorithm Wings knows", req.Algorithm)
		}

		name := strings.TrimSpace(req.Name)
		if name == "" {
			name = algorithm
		}
		if used[name] {
			return nil, errors.Errorf("pluginbridge: two digests were both requested under the name %q", name)
		}
		used[name] = true

		out = append(out, preparedDigest{
			name:             name,
			algorithm:        algorithm,
			ignoreWhitespace: req.IgnoreWhitespace,
			seed:             req.Seed,
		})
	}

	return out, nil
}

// needsLength reports whether any requested digest has to know the content
// length before it can start.
func needsLength(prepared []preparedDigest) bool {
	for _, p := range prepared {
		if p.algorithm == "murmur2" {
			return true
		}
	}
	return false
}

// digestWriter fans the file out to every hash, sending the ones that asked to
// ignore whitespace through a filter first.
func digestWriter(prepared []preparedDigest) io.Writer {
	var raw, stripped []io.Writer

	for i := range prepared {
		if prepared[i].ignoreWhitespace {
			stripped = append(stripped, prepared[i].writer())
			continue
		}
		raw = append(raw, prepared[i].writer())
	}

	writers := make([]io.Writer, 0, len(raw)+1)
	writers = append(writers, raw...)

	if len(stripped) > 0 {
		writers = append(writers, &whitespaceFilter{to: io.MultiWriter(stripped...)})
	}

	return io.MultiWriter(writers...)
}

// measure counts a file's bytes, both as-is and with whitespace removed.
func (b *Bridge) measure(s *server.Server, path string) (raw, stripped uint32, err error) {
	counter := &byteCounter{}
	if err := b.streamFile(s, path, counter); err != nil {
		return 0, 0, err
	}
	return counter.raw, counter.stripped, nil
}

// byteCounter counts bytes with and without whitespace in a single pass.
type byteCounter struct {
	raw      uint32
	stripped uint32
}

func (c *byteCounter) Write(p []byte) (int, error) {
	c.raw += uint32(len(p))
	for _, ch := range p {
		if !isASCIIWhitespace(ch) {
			c.stripped++
		}
	}
	return len(p), nil
}

// whitespaceFilter forwards everything but tabs, newlines, carriage returns
// and spaces.
type whitespaceFilter struct {
	to  io.Writer
	buf []byte
}

func (f *whitespaceFilter) Write(p []byte) (int, error) {
	if cap(f.buf) < len(p) {
		f.buf = make([]byte, 0, len(p))
	}
	f.buf = f.buf[:0]

	for _, ch := range p {
		if isASCIIWhitespace(ch) {
			continue
		}
		f.buf = append(f.buf, ch)
	}

	if len(f.buf) > 0 {
		if _, err := f.to.Write(f.buf); err != nil {
			return 0, err
		}
	}

	// Report the whole input as written. The caller is a MultiWriter, which
	// treats a short write as an error, and the bytes dropped here were never
	// meant to reach the hash.
	return len(p), nil
}

func isASCIIWhitespace(ch byte) bool {
	switch ch {
	case 0x09, 0x0a, 0x0d, 0x20:
		return true
	}
	return false
}

func newHasher(name string) hash.Hash {
	switch name {
	case "md5":
		return md5.New()
	case "sha256":
		return sha256.New()
	case "sha512":
		return sha512.New()
	case "crc32":
		return crc32.NewIEEE()
	default:
		return sha1.New()
	}
}

// streamFile copies a server file through w, with the same path checks any
// other read of a server's files gets.
func (b *Bridge) streamFile(s *server.Server, path string, w io.Writer) error {
	f, st, err := s.Filesystem().File(path)
	if err != nil {
		return err
	}
	defer f.Close()

	if st.IsDir() {
		return errors.Errorf("pluginbridge: %s is a directory", path)
	}

	buf := make([]byte, digestBufferSize)
	if _, err := io.CopyBuffer(w, f, buf); err != nil {
		return errors.Wrapf(err, "pluginbridge: could not read %s", path)
	}
	return nil
}

// murmur2Writer computes MurmurHash2 over a stream, carrying the up-to-three
// byte remainder between writes so that chunk boundaries do not change the
// result.
type murmur2Writer struct {
	h    uint32
	tail [4]byte
	used int
}

func newMurmur2(seed, length uint32) *murmur2Writer {
	return &murmur2Writer{h: seed ^ length}
}

const (
	murmurM uint32 = 0x5bd1e995
	murmurR        = 24
)

func (w *murmur2Writer) Write(p []byte) (int, error) {
	for _, ch := range p {
		w.tail[w.used] = ch
		w.used++

		if w.used == 4 {
			k := uint32(w.tail[0]) | uint32(w.tail[1])<<8 | uint32(w.tail[2])<<16 | uint32(w.tail[3])<<24

			k *= murmurM
			k ^= k >> murmurR
			k *= murmurM

			w.h *= murmurM
			w.h ^= k

			w.used = 0
		}
	}
	return len(p), nil
}

// sum finishes the hash, folding in whatever is left in the tail.
func (w *murmur2Writer) sum() uint32 {
	h := w.h

	switch w.used {
	case 3:
		h ^= uint32(w.tail[2]) << 16
		fallthrough
	case 2:
		h ^= uint32(w.tail[1]) << 8
		fallthrough
	case 1:
		h ^= uint32(w.tail[0])
		h *= murmurM
	}

	h ^= h >> 13
	h *= murmurM
	h ^= h >> 15

	return h
}
