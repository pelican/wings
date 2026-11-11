package server

import (
	"errors"
	"testing"

	"github.com/docker/docker/api/types/container"

	"github.com/pelican/wings/events"
)

func TestInstallationWaitError(t *testing.T) {
	tests := []struct {
		name      string
		response  container.WaitResponse
		wantError string
	}{
		{
			name:     "successful installation",
			response: container.WaitResponse{StatusCode: 0},
		},
		{
			name:      "installation script failure",
			response:  container.WaitResponse{StatusCode: 1},
			wantError: "install: installation script exited with code 1",
		},
		{
			name:      "command not found",
			response:  container.WaitResponse{StatusCode: 127},
			wantError: "install: installation script exited with code 127",
		},
		{
			name: "container wait failure",
			response: container.WaitResponse{
				Error: &container.WaitExitError{Message: "daemon disconnected"},
			},
			wantError: "install: installation container wait failed: daemon disconnected",
		},
		{
			name: "container wait failure without message",
			response: container.WaitResponse{
				Error: &container.WaitExitError{},
			},
			wantError: "install: installation container wait failed: unknown container wait error",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := installationWaitError(tt.response)
			if tt.wantError == "" {
				if err != nil {
					t.Fatalf("installationWaitError() returned unexpected error: %v", err)
				}
				return
			}

			if err == nil {
				t.Fatalf("installationWaitError() returned nil, expected %q", tt.wantError)
			}
			if err.Error() != tt.wantError {
				t.Fatalf("installationWaitError() error = %q, expected %q", err.Error(), tt.wantError)
			}
		})
	}
}

func TestWaitForInstallationContainerPublishesWaitFailure(t *testing.T) {
	s, err := New(nil)
	if err != nil {
		t.Fatalf("New() returned unexpected error: %v", err)
	}

	listener := make(chan []byte, 1)
	s.Events().On(listener)
	defer s.Events().Off(listener)

	waitErr := errors.New("daemon disconnected")
	sChan := make(chan container.WaitResponse)
	eChan := make(chan error, 1)
	eChan <- waitErr

	ip := &InstallationProcess{Server: s}
	if err := ip.waitForInstallationContainer(sChan, eChan); !errors.Is(err, waitErr) {
		t.Fatalf("waitForInstallationContainer() error = %v, expected %v", err, waitErr)
	}

	select {
	case raw := <-listener:
		event := events.MustDecode(raw)
		if event.Topic != DaemonMessageEvent {
			t.Fatalf("event topic = %q, expected %q", event.Topic, DaemonMessageEvent)
		}
		if event.Data != "Installation process failed: daemon disconnected" {
			t.Fatalf("event data = %q, expected failure message", event.Data)
		}
	default:
		t.Fatal("waitForInstallationContainer() did not publish a failure event")
	}
}

func TestIsSensitiveVariable(t *testing.T) {
	sensitive := []string{"RCON_PASSWORD", "SERVER_PASSWORD", "STEAM_PASS", "STEAM_AUTH", "SRCDS_TOKEN", "API_KEY", "FIVEM_LICENSE", "DB_PWD", "client_secret", "SERVERPASSWORD", "STEAM_WEBAPIKEY", "STEAM_ACC", "GSLT"}
	for _, name := range sensitive {
		if !isSensitiveVariable(name) {
			t.Errorf("isSensitiveVariable(%q) = false, expected true", name)
		}
	}

	kept := []string{"SERVER_MEMORY", "SERVER_PORT", "STARTUP", "BYPASS_CHECK", "MONKEY_MODE", "MAX_PLAYERS", "PASSIVE_MODE", "ACCESS_MODE", "TZ"}
	for _, name := range kept {
		if isSensitiveVariable(name) {
			t.Errorf("isSensitiveVariable(%q) = true, expected false", name)
		}
	}
}

func TestRedactEnvironment(t *testing.T) {
	env := []string{
		"TZ=UTC",
		"STARTUP=./srcds_run +rcon_password hunter22 +sv_setsteamaccount ABCD1234TOKEN -port 27015",
		"SERVER_MEMORY=1024",
		"RCON_PASSWORD=hunter22",
		"STEAM_ACC=ABCD1234TOKEN",
		"SRCDS_TOKEN=ABCD1234TOKEN",
		"SERVER_PASSWORD=",
		"MAX_PLAYERS=16",
	}

	got := redactEnvironment(env)
	want := []string{
		"TZ=UTC",
		"STARTUP=./srcds_run +rcon_password ******** +sv_setsteamaccount ******** -port 27015",
		"SERVER_MEMORY=1024",
		"RCON_PASSWORD=********",
		"STEAM_ACC=********",
		"SRCDS_TOKEN=********",
		"SERVER_PASSWORD=",
		"MAX_PLAYERS=16",
	}

	if len(got) != len(want) {
		t.Fatalf("redactEnvironment() returned %d variables, expected %d", len(got), len(want))
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("redactEnvironment()[%d] = %q, expected %q", i, got[i], want[i])
		}
	}
	if env[3] != "RCON_PASSWORD=hunter22" {
		t.Errorf("redactEnvironment() changed its input: %q", env[3])
	}
}

func TestRedactEnvironmentLongestSecretFirst(t *testing.T) {
	got := redactEnvironment([]string{
		"STARTUP=run --key abcdef --password abcdefgh",
		"API_KEY=abcdef",
		"ADMIN_PASSWORD=abcdefgh",
	})
	if got[0] != "STARTUP=run --key ******** --password ********" {
		t.Errorf("redactEnvironment()[0] = %q", got[0])
	}
}

func TestRedactEnvironmentShortValues(t *testing.T) {
	got := redactEnvironment([]string{
		"STARTUP=./start --memory 1024 --auth true",
		"SERVER_MEMORY=1024",
		"AUTH_REQUIRED=true",
		"SERVER_PASSWORD=1",
	})
	want := []string{
		"STARTUP=./start --memory 1024 --auth true",
		"SERVER_MEMORY=1024",
		"AUTH_REQUIRED=********",
		"SERVER_PASSWORD=********",
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("redactEnvironment()[%d] = %q, expected %q", i, got[i], want[i])
		}
	}
}
