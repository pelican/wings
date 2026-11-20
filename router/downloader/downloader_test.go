package downloader

import (
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/pelican/wings/config"
	"github.com/pelican/wings/remote"
	"github.com/pelican/wings/server"
	"github.com/pelican/wings/server/filesystem"
)

func TestMain(m *testing.M) {
	config.Set(&config.Configuration{AuthenticationToken: "test-token"})
	os.Exit(m.Run())
}

// setAllowlist sets the remote download allowlist for the duration of the test.
func setAllowlist(t *testing.T, entries ...string) {
	t.Helper()
	previous := config.Get()
	t.Cleanup(func() { config.Set(previous) })
	next := *previous
	next.Api.RemoteDownload.Allowlist = entries
	config.Set(&next)
}

// countingListener counts the connections it accepts.
type countingListener struct {
	net.Listener
	accepted atomic.Int64
}

func (l *countingListener) Accept() (net.Conn, error) {
	c, err := l.Listener.Accept()
	if err == nil {
		l.accepted.Add(1)
	}
	return c, err
}

func TestDownloadDoesNotConnectToInternalAddresses(t *testing.T) {
	inner, err := net.Listen("tcp", "0.0.0.0:0")
	if err != nil {
		t.Fatal(err)
	}
	l := &countingListener{Listener: inner}
	var requests atomic.Int64
	srv := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		_, _ = w.Write([]byte("internal"))
	})}
	go func() { _ = srv.Serve(l) }()
	t.Cleanup(func() { _ = srv.Close() })
	port := strconv.Itoa(inner.Addr().(*net.TCPAddr).Port)

	s, err := server.New(nil)
	if err != nil {
		t.Fatal(err)
	}

	// The shared address space and benchmarking ranges are not reachable on most
	// machines, but the download must be refused before any connection is made
	// whether or not they are.
	for _, host := range []string{"127.0.0.1", "[::1]", "[::ffff:127.0.0.1]", "100.64.0.5", "198.18.0.5", "0.0.0.0"} {
		t.Run(host, func(t *testing.T) {
			u, _ := url.Parse("http://" + host + ":" + port + "/file")
			err := New(s, DownloadRequest{Directory: "/", URL: u, FileName: "file"}).Execute()
			if !errors.Is(err, ErrInternalResolution) {
				t.Fatalf("expected download from %s to be refused, got %v", host, err)
			}
		})
	}

	// Give any connection that was made, and any request sent over it, time to
	// arrive before checking.
	time.Sleep(time.Millisecond * 100)
	if n := l.accepted.Load(); n != 0 {
		t.Fatalf("expected no connections to internal addresses, got %d", n)
	}
	if n := requests.Load(); n != 0 {
		t.Fatalf("expected no requests to internal addresses, got %d", n)
	}
}

// The address is checked again immediately before connecting, since the host is
// resolved again when dialing and could resolve to an internal address then.
func TestCheckConnectionRefusesInternalAddresses(t *testing.T) {
	for _, addr := range []string{"127.0.0.1:80", "[::1]:80", "10.0.0.1:443", "100.64.0.1:80", "[fe80::1%lo]:80", "[64:ff9b::7f00:1]:80"} {
		if err := checkConnection(context.Background(), "tcp", addr, nil); !errors.Is(err, ErrInternalResolution) {
			t.Errorf("expected connection to %s to be refused, got %v", addr, err)
		}
	}
	for _, addr := range []string{"1.1.1.1:443", "[2606:4700:4700::1111]:443"} {
		if err := checkConnection(context.Background(), "tcp", addr, nil); err != nil {
			t.Errorf("expected connection to %s to be allowed, got %v", addr, err)
		}
	}
}

// Operators can allow destinations that are otherwise refused, such as services
// on a Tailscale network or a cloud provider's internal endpoints.
func TestRemoteDownloadAllowlist(t *testing.T) {
	setAllowlist(t, "100.64.0.0/10", "internal.example.", "127.0.0.1")

	host := func(h string) context.Context { return context.WithValue(context.Background(), dialHostKey{}, h) }
	tests := []struct {
		ctx     context.Context
		addr    string
		allowed bool
	}{
		{context.Background(), "100.64.0.1:80", true},
		{context.Background(), "100.127.255.254:443", true},
		{context.Background(), "10.0.0.1:80", false},
		{host("internal.example"), "10.0.0.1:80", true},
		{host("other.example"), "10.0.0.1:80", false},
		{context.Background(), "1.1.1.1:443", true},
	}
	for _, tt := range tests {
		err := checkConnection(tt.ctx, "tcp", tt.addr, nil)
		if tt.allowed && err != nil {
			t.Errorf("expected connection to %s to be allowed, got %v", tt.addr, err)
		} else if !tt.allowed && !errors.Is(err, ErrInternalResolution) {
			t.Errorf("expected connection to %s to be refused, got %v", tt.addr, err)
		}
	}

	// A request to an allowed internal address is made through the client used
	// for downloads.
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	srv := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte("allowed"))
	})}
	go func() { _ = srv.Serve(l) }()
	t.Cleanup(func() { _ = srv.Close() })
	res, err := client.Get("http://" + l.Addr().String() + "/file")
	if err != nil {
		t.Fatalf("expected a request to an allowed address to succeed, got %v", err)
	}
	b, _ := io.ReadAll(res.Body)
	_ = res.Body.Close()
	if string(b) != "allowed" {
		t.Fatalf("expected the response from the allowed address, got %q", b)
	}
}

// Hosts that cannot be resolved are refused.
func TestDownloadRefusesUnresolvableHosts(t *testing.T) {
	s, err := server.New(nil)
	if err != nil {
		t.Fatal(err)
	}
	u, _ := url.Parse("http://does-not-exist.invalid/file")
	if err := New(s, DownloadRequest{Directory: "/", URL: u, FileName: "file"}).Execute(); !IsDownloadError(err) {
		t.Fatalf("expected an unresolvable host to be refused, got %v", err)
	}
}

// Downloads from internal addresses are refused, whether or not anything is
// listening on the port.
func TestDownloadsFromInternalPortsAreRefused(t *testing.T) {
	var wg sync.WaitGroup
	open, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer open.Close()
	wg.Add(1)
	go func() {
		defer wg.Done()
		if c, err := open.Accept(); err == nil {
			_ = c.Close()
		}
	}()
	closed, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	closedAddr := closed.Addr().String()
	_ = closed.Close()

	s, err := server.New(nil)
	if err != nil {
		t.Fatal(err)
	}
	var errs []error
	for _, addr := range []string{open.Addr().String(), closedAddr} {
		u, _ := url.Parse("http://" + addr + "/file")
		errs = append(errs, New(s, DownloadRequest{Directory: "/", URL: u, FileName: "file"}).Execute())
	}
	for _, err := range errs {
		if !errors.Is(err, ErrInternalResolution) {
			t.Fatalf("expected downloads from open and closed ports to be refused, got %v", errs)
		}
	}
	_ = open.Close()
	wg.Wait()
}

// newTestServer returns a server whose data directory lives in a temporary
// directory and whose disk is limited to diskSpaceMiB.
func newTestServer(t *testing.T, diskSpaceMiB int64) *server.Server {
	t.Helper()

	previous := config.Get()
	t.Cleanup(func() { config.Set(previous) })
	cfg := &config.Configuration{AuthenticationToken: "test-token"}
	cfg.System.Data = t.TempDir()
	cfg.System.User.Uid = os.Getuid()
	cfg.System.User.Gid = os.Getgid()
	config.Set(cfg)

	id := uuid.NewString()
	settings, err := json.Marshal(map[string]any{
		"uuid":  id,
		"build": map[string]any{"disk_space": diskSpaceMiB},
		"allocations": map[string]any{
			"default": map[string]any{"ip": "127.0.0.1", "port": 25565},
		},
	})
	if err != nil {
		t.Fatal(err)
	}

	s, err := server.NewEmptyManager(nil).InitServer(id, remote.ServerConfigurationResponse{Settings: settings})
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
