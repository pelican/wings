package downloader

import (
	"bytes"
	"compress/gzip"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/pelican/wings/config"
	"github.com/pelican/wings/remote"
	"github.com/pelican/wings/server"
	"github.com/pelican/wings/server/filesystem"
)

// newTestServer returns a server whose data directory lives in a temporary
// directory and whose disk is limited to diskSpaceMiB.
func newTestServer(t *testing.T, diskSpaceMiB int64) *server.Server {
	t.Helper()

	cfg := &config.Configuration{AuthenticationToken: "test-token"}
	cfg.System.Data = t.TempDir()
	cfg.System.User.Uid = os.Getuid()
	cfg.System.User.Gid = os.Getgid()
	config.Set(cfg)

	settings, err := json.Marshal(map[string]any{
		"uuid":  uuid.NewString(),
		"build": map[string]any{"disk_space": diskSpaceMiB},
		"allocations": map[string]any{
			"default": map[string]any{"ip": "127.0.0.1", "port": 25565},
		},
	})
	if err != nil {
		t.Fatal(err)
	}

	s, err := server.NewEmptyManager(nil).InitServer(remote.ServerConfigurationResponse{Settings: settings})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(s.CtxCancel)
	return s
}

// allowLoopback swaps out the transport of the package level client so that
// the downloader can reach an httptest server listening on 127.0.0.1.
func allowLoopback(t *testing.T) {
	t.Helper()

	previous := client.Transport
	client.Transport = http.DefaultTransport.(*http.Transport).Clone()
	t.Cleanup(func() {
		client.Transport = previous
	})
}

func download(t *testing.T, s *server.Server, rawURL string) (*Download, error) {
	t.Helper()

	u, err := url.Parse(rawURL)
	if err != nil {
		t.Fatal(err)
	}
	dl := New(s, DownloadRequest{Directory: "/", URL: u})
	return dl, dl.Execute()
}

func readServerFile(t *testing.T, s *server.Server, name string) []byte {
	t.Helper()

	contents, err := os.ReadFile(filepath.Join(s.Filesystem().Path(), name))
	if err != nil {
		t.Fatal(err)
	}
	return contents
}

func TestDownloadGzipEncodedResponse(t *testing.T) {
	allowLoopback(t)
	s := newTestServer(t, 1)

	payload := []byte(strings.Repeat("server.properties\n", 512))
	var compressed bytes.Buffer
	gz := gzip.NewWriter(&compressed)
	if _, err := gz.Write(payload); err != nil {
		t.Fatal(err)
	}
	if err := gz.Close(); err != nil {
		t.Fatal(err)
	}

	// Mirror raw.githubusercontent.com: the body is gzip encoded whenever the
	// client asks for it, which Go's transport does on its own and then hides
	// the Content-Length of the response from us after decompressing it.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.Contains(r.Header.Get("Accept-Encoding"), "gzip") {
			w.Header().Set("Content-Length", strconv.Itoa(len(payload)))
			_, _ = w.Write(payload)
			return
		}
		w.Header().Set("Content-Encoding", "gzip")
		w.Header().Set("Content-Length", strconv.Itoa(compressed.Len()))
		_, _ = w.Write(compressed.Bytes())
	}))
	defer srv.Close()

	if _, err := download(t, s, srv.URL+"/gzip.txt"); err != nil {
		t.Fatalf("expected download to succeed, got %v", err)
	}
	if got := readServerFile(t, s, "gzip.txt"); !bytes.Equal(got, payload) {
		t.Fatalf("expected %d decompressed bytes on disk, got %d", len(payload), len(got))
	}
	if got := s.Filesystem().CachedUsage(); got != int64(len(payload)) {
		t.Fatalf("expected disk usage of %d, got %d", len(payload), got)
	}
}

func TestDownloadChunkedResponse(t *testing.T) {
	allowLoopback(t)
	s := newTestServer(t, 1)

	chunk := []byte(strings.Repeat("a", 4096))
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		for i := 0; i < 8; i++ {
			_, _ = w.Write(chunk)
			w.(http.Flusher).Flush()
		}
	}))
	defer srv.Close()

	dl, err := download(t, s, srv.URL+"/chunked.txt")
	if err != nil {
		t.Fatalf("expected download to succeed, got %v", err)
	}
	if got := dl.Progress(); got != 0 {
		t.Fatalf("expected progress to stay at 0 without a known length, got %v", got)
	}
	if got := readServerFile(t, s, "chunked.txt"); !bytes.Equal(got, bytes.Repeat(chunk, 8)) {
		t.Fatalf("expected %d bytes on disk, got %d", len(chunk)*8, len(got))
	}
	if got := s.Filesystem().CachedUsage(); got != int64(len(chunk)*8) {
		t.Fatalf("expected disk usage of %d, got %d", len(chunk)*8, got)
	}
}

func TestDownloadKnownLengthResponse(t *testing.T) {
	allowLoopback(t)
	s := newTestServer(t, 1)

	payload := bytes.Repeat([]byte("a"), 64*1024)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Length", strconv.Itoa(len(payload)))
		_, _ = w.Write(payload)
	}))
	defer srv.Close()

	dl, err := download(t, s, srv.URL+"/known.bin")
	if err != nil {
		t.Fatalf("expected download to succeed, got %v", err)
	}
	if got := readServerFile(t, s, "known.bin"); !bytes.Equal(got, payload) {
		t.Fatalf("expected %d bytes on disk, got %d", len(payload), len(got))
	}
	if got := dl.Progress(); got != 1 {
		t.Fatalf("expected progress of 1, got %v", got)
	}
}

func TestDownloadChunkedResponseExceedingDiskLimit(t *testing.T) {
	allowLoopback(t)
	s := newTestServer(t, 1)

	chunk := []byte(strings.Repeat("a", 64*1024))
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		for i := 0; i < 32; i++ {
			if _, err := w.Write(chunk); err != nil {
				return
			}
			w.(http.Flusher).Flush()
		}
	}))
	defer srv.Close()

	_, err := download(t, s, srv.URL+"/too-big.bin")
	if !filesystem.IsErrorCode(err, filesystem.ErrCodeDiskSpace) {
		t.Fatalf("expected a disk space error, got %v", err)
	}
	if _, err := os.Stat(filepath.Join(s.Filesystem().Path(), "too-big.bin")); !os.IsNotExist(err) {
		t.Fatalf("expected the partial download to be removed, got %v", err)
	}
	if got := s.Filesystem().CachedUsage(); got != 0 {
		t.Fatalf("expected disk usage to be released, got %d", got)
	}
}

func TestDownloadChunkedResponseExceedingDiskLimitKeepsExistingFile(t *testing.T) {
	allowLoopback(t)
	s := newTestServer(t, 1)

	original := []byte("motd=A Minecraft Server\n")
	if err := s.Filesystem().Write("server.properties", bytes.NewReader(original), int64(len(original)), 0o644); err != nil {
		t.Fatal(err)
	}
	usage := s.Filesystem().CachedUsage()

	chunk := []byte(strings.Repeat("a", 64*1024))
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		for i := 0; i < 32; i++ {
			if _, err := w.Write(chunk); err != nil {
				return
			}
			w.(http.Flusher).Flush()
		}
	}))
	defer srv.Close()

	_, err := download(t, s, srv.URL+"/server.properties")
	if !filesystem.IsErrorCode(err, filesystem.ErrCodeDiskSpace) {
		t.Fatalf("expected a disk space error, got %v", err)
	}
	if got := readServerFile(t, s, "server.properties"); !bytes.Equal(got, original) {
		t.Fatalf("expected the existing file to be left untouched, got %d bytes", len(got))
	}
	if got := s.Filesystem().CachedUsage(); got != usage {
		t.Fatalf("expected disk usage to stay at %d, got %d", usage, got)
	}
	entries, err := os.ReadDir(s.Filesystem().Path())
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 {
		t.Fatalf("expected only the existing file to remain, got %d entries", len(entries))
	}
}

func TestDownloadKnownLengthExceedingDiskLimit(t *testing.T) {
	allowLoopback(t)
	s := newTestServer(t, 1)

	payload := bytes.Repeat([]byte("a"), 2*1024*1024)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Length", strconv.Itoa(len(payload)))
		_, _ = w.Write(payload)
	}))
	defer srv.Close()

	_, err := download(t, s, srv.URL+"/too-big.bin")
	if !filesystem.IsErrorCode(err, filesystem.ErrCodeDiskSpace) {
		t.Fatalf("expected a disk space error, got %v", err)
	}
	if _, err := os.Stat(filepath.Join(s.Filesystem().Path(), "too-big.bin")); !os.IsNotExist(err) {
		t.Fatalf("expected no file to be written, got %v", err)
	}
}
