package pluginbridge

import (
	"testing"

	"github.com/pelican/wings/plugins/api"
)

// murmurVectors are MurmurHash2 with seed 1 over whitespace-stripped content,
// cross-checked against an independent PHP implementation of the same
// primitive.
//
// A plugin composes this into whatever file fingerprint it needs, so being
// off by a bit here would silently produce hashes that match nothing. The
// cases cover the whitespace stripping and every tail length, which is where
// a MurmurHash port goes wrong.
var murmurVectors = []struct {
	name string
	data []byte
	want uint32
}{
	{"empty", []byte(""), 1540447798},
	{"one byte", []byte("a"), 626045324},
	{"two bytes", []byte("ab"), 1692487918},
	{"three bytes", []byte("abc"), 1621425345},
	{"four bytes", []byte("abcd"), 3376380438},
	{"five bytes", []byte("abcde"), 3469237630},
	{"with a space", []byte("hello world"), 2824650221},
	// Whitespace only strips to nothing, so it must hash as the empty input.
	{"whitespace only", []byte("  \t\r\n  "), 1540447798},
	// And this strips to exactly "abcde".
	{"whitespace between", []byte("a\tb\nc\rd e"), 3469237630},
	{"high bytes", []byte{0x00, 0x01, 0x02, 0xfe, 0xff}, 2517207944},
}

// fingerprint runs the streaming hasher the way FileDigest does: count the
// stripped length first, then hash, feeding the data in the given chunk sizes.
func fingerprint(data []byte, chunk int) uint32 {
	counter := &byteCounter{}
	writeInChunks(counter, data, chunk)

	hasher := newMurmur2(1, counter.stripped)
	writeInChunks(&whitespaceFilter{to: hasher}, data, chunk)

	return hasher.sum()
}

func writeInChunks(w interface{ Write([]byte) (int, error) }, data []byte, chunk int) {
	if chunk <= 0 {
		chunk = len(data)
		if chunk == 0 {
			chunk = 1
		}
	}
	for i := 0; i < len(data); i += chunk {
		end := i + chunk
		if end > len(data) {
			end = len(data)
		}
		_, _ = w.Write(data[i:end])
	}
}

func TestMurmur2MatchesAnIndependentImplementation(t *testing.T) {
	for _, v := range murmurVectors {
		if got := fingerprint(v.data, 0); got != v.want {
			t.Errorf("%s: got %d, want %d", v.name, got, v.want)
		}
	}
}

// TestMurmur2IsChunkIndependent is the risk specific to doing this as a
// stream. MurmurHash2 consumes four bytes at a time, so a chunk
// boundary falling mid-block would change the result unless the remainder is
// carried across writes.
func TestMurmur2IsChunkIndependent(t *testing.T) {
	for _, v := range murmurVectors {
		for _, chunk := range []int{1, 2, 3, 4, 5, 7, 8, 64} {
			if got := fingerprint(v.data, chunk); got != v.want {
				t.Errorf("%s in %d-byte chunks: got %d, want %d", v.name, chunk, got, v.want)
			}
		}
	}
}

// TestMurmur2SplitsWhitespaceAcrossChunks checks the other boundary case:
// whitespace is filtered before the four-byte blocking, so a chunk that is
// entirely whitespace must not disturb the tail being gathered.
func TestMurmur2SplitsWhitespaceAcrossChunks(t *testing.T) {
	// Strips to "abcdef", with the whitespace landing between blocks.
	data := []byte("ab \t\n cd\r\r ef")

	want := fingerprint([]byte("abcdef"), 0)

	for _, chunk := range []int{1, 2, 3, 4, 5, 6, 13} {
		if got := fingerprint(data, chunk); got != want {
			t.Errorf("in %d-byte chunks: got %d, want %d", chunk, got, want)
		}
	}
}

func TestPrepareDigests(t *testing.T) {
	prepared, err := prepareDigests([]api.DigestRequest{
		{Algorithm: "SHA1"},
		{Algorithm: " sha512 "},
		// The same algorithm twice with different options, which is the whole
		// reason a request carries its own name.
		{Algorithm: "murmur2", Name: "raw", Seed: 1},
		{Algorithm: "murmur2", Name: "stripped", Seed: 1, IgnoreWhitespace: true},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(prepared) != 4 {
		t.Fatalf("expected four digests, got %d", len(prepared))
	}

	// Case and surrounding whitespace normalised, and an unnamed request keyed
	// by its algorithm.
	if prepared[0].name != "sha1" || prepared[1].name != "sha512" {
		t.Errorf("names resolved wrongly: %q %q", prepared[0].name, prepared[1].name)
	}

	// A typo must surface rather than silently producing no key.
	if _, err := prepareDigests([]api.DigestRequest{{Algorithm: "sha-1"}}); err == nil {
		t.Error("an unknown algorithm should be an error")
	}
	if _, err := prepareDigests(nil); err == nil {
		t.Error("requesting nothing should be an error")
	}
	// Two results under one key would silently lose one of them.
	if _, err := prepareDigests([]api.DigestRequest{
		{Algorithm: "sha1", Name: "x"},
		{Algorithm: "md5", Name: "x"},
	}); err == nil {
		t.Error("a duplicate name should be an error")
	}
}
