package server

import (
	"sync"
	"sync/atomic"
	"testing"

	"context"

	"github.com/pelican/wings/config"
	"github.com/pelican/wings/environment"
	"github.com/pelican/wings/remote"
)

// installStateClient records the install-state notifications sent to the Panel.
type installStateClient struct {
	remote.Client
	statuses []bool
}

func (c *installStateClient) SetInstallationStatus(_ context.Context, _ string, data remote.InstallStatusRequest) error {
	c.statuses = append(c.statuses, data.Successful)
	return nil
}

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

// Restore and transfer each replace the server's files, so no two lifecycle
// operations may start at once even when many requests race (GHSA-6pfj-adjacent).
func TestLifecycleClaimsAreMutuallyExclusiveUnderRace(t *testing.T) {
	for iter := 0; iter < 200; iter++ {
		s := newConfiguredServer(t)
		var wg sync.WaitGroup
		var transfers, restores atomic.Int32
		for i := 0; i < 8; i++ {
			wg.Add(2)
			go func() {
				defer wg.Done()
				if s.StartTransferring() {
					transfers.Add(1)
				}
			}()
			go func() {
				defer wg.Done()
				if s.StartRestoring() {
					restores.Add(1)
				}
			}()
		}
		wg.Wait()
		if transfers.Load()+restores.Load() != 1 {
			t.Fatalf("iteration %d: expected exactly one lifecycle op to start, got %d transfers and %d restores", iter, transfers.Load(), restores.Load())
		}
		if s.IsTransferring() && s.IsRestoring() {
			t.Fatalf("iteration %d: server is both transferring and restoring", iter)
		}
	}
}

// A server that is being restored (or transferred) must refuse an install and,
// crucially, leave the environment state alone. Otherwise install() would mark
// the environment offline while the container is still running, and the
// concurrent restore would then skip waiting for it to stop before truncating
// files.
func TestInstallIsRefusedAndInertWhileRestoring(t *testing.T) {
	setTestConfig(t)
	client := &installStateClient{}
	s, err := New(client)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.SyncWithConfiguration(serverConfiguration(t, testServerUuid, 1)); err != nil {
		t.Fatal(err)
	}
	env := &recordingStateEnvironment{state: environment.ProcessRunningState}
	s.Environment = env

	if !s.StartRestoring() {
		t.Fatal("expected the restore to start")
	}

	if err := s.Install(); err == nil {
		t.Fatal("expected install to be refused while a restore is in progress")
	}

	env.mu.Lock()
	if len(env.states) != 0 {
		env.mu.Unlock()
		t.Fatalf("expected a refused install not to change the environment state, got %v", env.states)
	}
	env.mu.Unlock()
	if !s.IsRestoring() {
		t.Fatal("expected the server to still be restoring")
	}
	// The Panel must still be told the install did not succeed, since the request
	// was already answered with a 202.
	if len(client.statuses) != 1 || client.statuses[0] {
		t.Fatalf("expected one unsuccessful install-state notification to the Panel, got %v", client.statuses)
	}
}
