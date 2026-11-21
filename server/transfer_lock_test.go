package server

import (
	"errors"
	"testing"

	"github.com/pelican/wings/environment"
)

// stateEnvironment reports a fixed process state. Calls to any other method of
// the embedded nil environment.ProcessEnvironment panic.
type stateEnvironment struct {
	environment.ProcessEnvironment
	state string
}

func (e *stateEnvironment) State() string { return e.state }

func newTransferLockServer(t *testing.T, state string) *Server {
	t.Helper()
	s, err := New(nil)
	if err != nil {
		t.Fatal(err)
	}
	s.Environment = &stateEnvironment{state: state}
	return s
}

// TestLockForIncomingTransferExcludesPowerActions checks that a transfer holds
// the power lock while it runs, so no start can begin, and that the server is
// released again afterwards.
func TestLockForIncomingTransferExcludesPowerActions(t *testing.T) {
	s := newTransferLockServer(t, environment.ProcessOfflineState)

	release, err := s.LockForIncomingTransfer()
	if err != nil {
		t.Fatalf("LockForIncomingTransfer() returned unexpected error: %v", err)
	}
	if !s.IsTransferring() || !s.ExecutingPowerAction() {
		t.Fatal("expected the server to be transferring and its power lock held")
	}
	if err := s.HandlePowerAction(PowerActionStart); !errors.Is(err, ErrServerIsTransferring) {
		t.Fatalf("expected a start during the transfer to be refused, got %v", err)
	}
	if _, err := s.LockForIncomingTransfer(); err == nil {
		t.Fatal("expected a second transfer to be refused")
	}

	release()
	if s.ExecutingPowerAction() {
		t.Fatal("expected release to free the power lock")
	}
}

// TestLockForIncomingTransferWaitsForNoPowerAction checks that a power action
// already holding the lock, such as a start still in its pre-boot steps while
// the process is offline, refuses the transfer.
func TestLockForIncomingTransferWaitsForNoPowerAction(t *testing.T) {
	s := newTransferLockServer(t, environment.ProcessOfflineState)
	if err := s.powerLock.Acquire(); err != nil {
		t.Fatal(err)
	}
	defer s.powerLock.Release()

	if _, err := s.LockForIncomingTransfer(); err == nil {
		t.Fatal("expected the transfer to be refused while a power action holds the lock")
	}
	if s.IsTransferring() {
		t.Fatal("a refused transfer must not mark the server as transferring")
	}
}

// TestLockForIncomingTransferRefusesBusyServer checks the server states that
// refuse a transfer, and that each leaves the power lock free.
func TestLockForIncomingTransferRefusesBusyServer(t *testing.T) {
	cases := map[string]struct {
		state string
		setup func(*Server)
		want  error
	}{
		"running":      {state: environment.ProcessRunningState, want: ErrIsRunning},
		"starting":     {state: environment.ProcessStartingState, want: ErrIsRunning},
		"installing":   {state: environment.ProcessOfflineState, setup: func(s *Server) { s.installing.Store(true) }, want: ErrServerIsInstalling},
		"restoring":    {state: environment.ProcessOfflineState, setup: func(s *Server) { s.SetRestoring(true) }, want: ErrServerIsRestoring},
		"transferring": {state: environment.ProcessOfflineState, setup: func(s *Server) { s.SetTransferring(true) }, want: ErrServerIsTransferring},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			s := newTransferLockServer(t, tc.state)
			if tc.setup != nil {
				tc.setup(s)
			}
			if _, err := s.LockForIncomingTransfer(); !errors.Is(err, tc.want) {
				t.Fatalf("expected %v, got %v", tc.want, err)
			}
			if s.ExecutingPowerAction() {
				t.Fatal("a refused transfer must release the power lock")
			}
		})
	}
}
