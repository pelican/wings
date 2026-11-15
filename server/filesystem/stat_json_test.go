package filesystem

import (
	"bytes"
	jsonv1 "encoding/json"
	jsonv2 "encoding/json/v2"
	"io/fs"
	"os"
	"reflect"
	"strconv"
	"testing"
	"time"

	goccy "github.com/goccy/go-json"
)

type fakeFileInfo struct {
	name string
	size int64
	mode fs.FileMode
	mod  time.Time
}

func (f fakeFileInfo) Name() string       { return f.name }
func (f fakeFileInfo) Size() int64        { return f.size }
func (f fakeFileInfo) Mode() fs.FileMode  { return f.mode }
func (f fakeFileInfo) ModTime() time.Time { return f.mod }
func (f fakeFileInfo) IsDir() bool        { return f.mode.IsDir() }
func (f fakeFileInfo) Sys() any           { return nil }

// legacyStatJSON is the MarshalJSON implementation Wings shipped before the
// move to MarshalJSONTo. New output must match it.
func legacyStatJSON(s *Stat) ([]byte, error) {
	return jsonv1.Marshal(struct {
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
		Name:      s.Name(),
		Created:   s.CTime().Format(time.RFC3339),
		Modified:  s.ModTime().Format(time.RFC3339),
		Mode:      s.Mode().String(),
		ModeBits:  strconv.FormatUint(uint64(s.Mode()&os.ModePerm), 8),
		Size:      s.Size(),
		Directory: s.IsDir(),
		File:      !s.IsDir(),
		Symlink:   s.Mode().Type()&os.ModeSymlink != 0,
		Mime:      s.Mimetype,
	})
}

// statEncoders lists every way Wings can encode a Stat. byteExact marks the
// ones that must match the legacy output byte for byte; plain json/v2 does not
// escape <, > and & for HTML, so it is only compared by value.
func statEncoders() map[string]struct {
	enc       func(*Stat) ([]byte, error)
	byteExact bool
} {
	return map[string]struct {
		enc       func(*Stat) ([]byte, error)
		byteExact bool
	}{
		"MarshalJSON": {func(s *Stat) ([]byte, error) { return s.MarshalJSON() }, true},
		"json/v1":     {func(s *Stat) ([]byte, error) { return jsonv1.Marshal(s) }, true},
		"goccy":       {func(s *Stat) ([]byte, error) { return goccy.Marshal(s) }, true},
		"json/v2":     {func(s *Stat) ([]byte, error) { return jsonv2.Marshal(s) }, false},
	}
}

func decodeStatJSON(t *testing.T, b []byte) map[string]any {
	t.Helper()
	var m map[string]any
	if err := jsonv2.Unmarshal(b, &m); err != nil {
		t.Fatalf("output is not valid JSON: %v\n%s", err, b)
	}
	return m
}

func TestStatMarshalJSONToMatchesLegacy(t *testing.T) {
	mod := time.Date(2026, 11, 9, 12, 30, 0, 0, time.UTC)
	cases := []*Stat{
		{FileInfo: fakeFileInfo{name: "server.properties", size: 1234, mode: 0o644, mod: mod}, Mimetype: "text/plain; charset=utf-8"},
		{FileInfo: fakeFileInfo{name: "world", mode: fs.ModeDir | 0o755, mod: mod}, Mimetype: "inode/directory"},
		{FileInfo: fakeFileInfo{name: "latest.log", mode: fs.ModeSymlink | 0o777, mod: mod}, Mimetype: "application/octet-stream"},
		{FileInfo: fakeFileInfo{name: `quote"back\slash<tag>&§ü 日本.txt`, size: 1 << 40, mode: 0o600, mod: mod}, Mimetype: ""},
	}
	for _, st := range cases {
		want, err := legacyStatJSON(st)
		if err != nil {
			t.Fatal(err)
		}
		for name, e := range statEncoders() {
			got, err := e.enc(st)
			if err != nil {
				t.Fatalf("%s(%q): %v", name, st.Name(), err)
			}
			if e.byteExact {
				if !bytes.Equal(got, want) {
					t.Errorf("%s(%q):\n got %s\nwant %s", name, st.Name(), got, want)
				}
			} else if !reflect.DeepEqual(decodeStatJSON(t, got), decodeStatJSON(t, want)) {
				t.Errorf("%s(%q) decodes differently:\n got %s\nwant %s", name, st.Name(), got, want)
			}
		}
	}
}

func TestStatMarshalJSONInvalidUTF8(t *testing.T) {
	// A Latin-1 filename, plus a run of two invalid bytes.
	st := &Stat{FileInfo: fakeFileInfo{name: "caf\xe9\xff\xfe.txt", mode: 0o644}, Mimetype: "text/plain"}

	legacy, err := legacyStatJSON(st)
	if err != nil {
		t.Fatal(err)
	}
	var want map[string]any
	if err := jsonv1.Unmarshal(legacy, &want); err != nil {
		t.Fatal(err)
	}
	if want["name"] != "caf���.txt" {
		t.Fatalf("legacy name = %q", want["name"])
	}

	for name, e := range statEncoders() {
		b, err := e.enc(st)
		if err != nil {
			t.Fatalf("%s: invalid UTF-8 filename must not fail: %v", name, err)
		}
		got := decodeStatJSON(t, b)
		if got["name"] != want["name"] {
			t.Errorf("%s: name = %q, want %q", name, got["name"], want["name"])
		}
	}
}

// legacyStat marshals with the pre-MarshalJSONTo implementation, so a slice of
// them reproduces how Gin encoded a directory listing before this change.
// It must not embed *Stat, or it would inherit MarshalJSONTo.
type legacyStat struct{ s *Stat }

func (l legacyStat) MarshalJSON() ([]byte, error) { return legacyStatJSON(l.s) }

func BenchmarkStatMarshal(b *testing.B) {
	stats := make([]*Stat, 1000)
	legacy := make([]legacyStat, len(stats))
	for i := range stats {
		stats[i] = &Stat{FileInfo: fakeFileInfo{name: "file-" + strconv.Itoa(i) + ".txt", size: int64(i), mode: 0o644, mod: time.Now()}, Mimetype: "text/plain"}
		legacy[i] = legacyStat{stats[i]}
	}
	run := func(name string, v any, marshal func(any) ([]byte, error)) {
		b.Run(name, func(b *testing.B) {
			b.ReportAllocs()
			for b.Loop() {
				if _, err := marshal(v); err != nil {
					b.Fatal(err)
				}
			}
		})
	}
	run("legacy", legacy, jsonv1.Marshal)
	run("json/v1", stats, jsonv1.Marshal)
	run("json/v2", stats, func(v any) ([]byte, error) { return jsonv2.Marshal(v) })
	run("goccy", stats, goccy.Marshal)
}
