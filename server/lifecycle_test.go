package server

import (
	"sync"
	"sync/atomic"
	"testing"

	"github.com/pelican/wings/config"
)

// Only one of many concurrent requests can start a transfer.
func TestStartTransferringIsExclusive(t *testing.T) {
	s := newConfiguredServer(t)

	var started atomic.Int32
	var wg sync.WaitGroup
	for i := 0; i < 50; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if s.StartTransferring() {
				started.Add(1)
			}
		}()
	}
	wg.Wait()

	if n := started.Load(); n != 1 {
		t.Fatalf("expected exactly one transfer to start, %d did", n)
	}
	if !s.IsTransferring() {
		t.Fatal("expected the server to be transferring")
	}
}

// A server that is being installed, restored or transferred cannot start one of
// the others, since each of them replaces the server's files.
func TestProtectedStatesExcludeEachOther(t *testing.T) {
	s := newConfiguredServer(t)

	s.SetInstalling(true)
	if s.StartTransferring() || s.StartRestoring() {
		t.Fatal("expected an installing server to refuse transfers and restores")
	}
	s.SetInstalling(false)

	if !s.StartRestoring() {
		t.Fatal("expected a restore to start")
	}
	if s.StartTransferring() || s.StartRestoring() {
		t.Fatal("expected a restoring server to refuse transfers and another restore")
	}
	s.SetRestoring(false)

	if !s.StartTransferring() {
		t.Fatal("expected a transfer to start")
	}
	if s.StartRestoring() || s.StartTransferring() {
		t.Fatal("expected a transferring server to refuse restores and another transfer")
	}
}

// Installation containers always have a limit on the number of processes,
// which is at least as high as the limit for the server.
func TestInstallerHasPidLimit(t *testing.T) {
	for _, serverLimit := range []int64{0, 512, 10000} {
		setTestConfig(t, func(c *config.Configuration) { c.Docker.ContainerPidLimit = serverLimit })
		s, err := New(nil)
		if err != nil {
			t.Fatal(err)
		}
		ip := &InstallationProcess{Server: s}

		limit := ip.resourceLimits().PidsLimit
		if limit == nil {
			t.Fatalf("server limit %d: expected the installer to have a process limit", serverLimit)
		}
		if want := max(serverLimit, installerPidLimit); *limit != want {
			t.Fatalf("server limit %d: expected an installer process limit of %d, got %d", serverLimit, want, *limit)
		}
	}
}
