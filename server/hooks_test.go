package server

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"testing"

	"github.com/pelican/wings/config"
	"github.com/pelican/wings/environment"
	"github.com/pelican/wings/events"
	"github.com/pelican/wings/remote"
)

// hookClient returns a fixed installation script and records the install
// statuses reported to the Panel. Calls to any other method of the embedded nil
// remote.Client panic.
type hookClient struct {
	remote.Client
	script   remote.InstallationScript
	statuses []remote.InstallStatusRequest
}

func (c *hookClient) GetInstallationScript(context.Context, string) (remote.InstallationScript, error) {
	return c.script, nil
}

func (c *hookClient) SetInstallationStatus(_ context.Context, _ string, r remote.InstallStatusRequest) error {
	c.statuses = append(c.statuses, r)
	return nil
}

// hookInstaller records how it was called and returns err.
type hookInstaller struct {
	err        error
	calls      int
	script     *remote.InstallationScript
	installing bool
}

func (i *hookInstaller) Run(s *Server, script *remote.InstallationScript) error {
	i.calls++
	i.script = script
	i.installing = s.IsInstalling()
	return i.err
}

// hookEnvironment implements only the methods InitServer and
// SyncWithEnvironment call, plus ImageAndStopConfigurable. Calls to any other
// method of the embedded nil environment.ProcessEnvironment panic.
type hookEnvironment struct {
	environment.ProcessEnvironment
	cfg   *environment.Configuration
	bus   *events.Bus
	image string
	stop  remote.ProcessStopConfiguration
}

func (e *hookEnvironment) Config() *environment.Configuration { return e.cfg }
func (e *hookEnvironment) Events() *events.Bus                { return e.bus }
func (e *hookEnvironment) SetLogCallback(func([]byte))        {}
func (e *hookEnvironment) InSituUpdate() error                { return nil }
func (e *hookEnvironment) SetImage(i string)                  { e.image = i }
func (e *hookEnvironment) SetState(string)                    {}
func (e *hookEnvironment) SetStopConfiguration(c remote.ProcessStopConfiguration) {
	e.stop = c
}

// setHookConfig sets a global configuration that InitServer can run with.
func setHookConfig(t *testing.T) {
	config.Set(&config.Configuration{
		AuthenticationToken: "test",
		System:              config.SystemConfiguration{Data: t.TempDir()},
	})
}

// hookServerSettings returns the minimal Panel settings InitServer needs.
func hookServerSettings(uuid string) json.RawMessage {
	return json.RawMessage(`{"uuid":"` + uuid + `","allocations":{"default":{"ip":"0.0.0.0","port":25565}}}`)
}

// newInstallHookServer returns a server that installs with installer and
// reports its install status to the returned client.
func newInstallHookServer(t *testing.T, installer Installer) (*Server, *hookClient) {
	t.Helper()
	client := &hookClient{script: remote.InstallationScript{ContainerImage: "installer:latest"}}
	s, err := New(client)
	if err != nil {
		t.Fatalf("New() returned unexpected error: %v", err)
	}
	s.Environment = &hookEnvironment{}
	s.installer = installer
	return s, client
}

// TestInstallRunsInstallerUnderLock checks that install passes the Panel's
// script to the configured installer, holds the installation lock while it runs,
// releases the lock afterwards and reports a successful install.
func TestInstallRunsInstallerUnderLock(t *testing.T) {
	installer := &hookInstaller{}
	s, client := newInstallHookServer(t, installer)

	if err := s.install(false); err != nil {
		t.Fatalf("install() returned unexpected error: %v", err)
	}
	if installer.calls != 1 {
		t.Fatalf("installer called %d times, expected 1", installer.calls)
	}
	if installer.script == nil || installer.script.ContainerImage != "installer:latest" {
		t.Fatalf("installer received script %+v, expected the Panel's script", installer.script)
	}
	if !installer.installing {
		t.Fatal("installer ran without the installation lock held")
	}
	if s.IsInstalling() {
		t.Fatal("installation lock still held after install() returned")
	}
	if len(client.statuses) != 1 || !client.statuses[0].Successful {
		t.Fatalf("install statuses = %+v, expected one successful", client.statuses)
	}
}

// TestInstallReturnsInstallerError checks that an installer error is returned,
// reported to the Panel and the installation lock is still released.
func TestInstallReturnsInstallerError(t *testing.T) {
	installErr := errors.New("script failed")
	s, client := newInstallHookServer(t, &hookInstaller{err: installErr})

	if err := s.install(false); !errors.Is(err, installErr) {
		t.Fatalf("install() error = %v, expected %v", err, installErr)
	}
	if s.IsInstalling() {
		t.Fatal("installation lock still held after a failed install")
	}
	if len(client.statuses) != 1 || client.statuses[0].Successful {
		t.Fatalf("install statuses = %+v, expected one unsuccessful", client.statuses)
	}
}

// TestInstallRefusesWhileInstalling checks that install does not call the
// installer while another installation holds the lock, and leaves that lock
// alone.
func TestInstallRefusesWhileInstalling(t *testing.T) {
	installer := &hookInstaller{}
	s, _ := newInstallHookServer(t, installer)
	s.installing.Store(true)

	if err := s.install(false); err == nil {
		t.Fatal("install() returned nil while another install held the lock")
	}
	if installer.calls != 0 {
		t.Fatalf("installer called %d times while the lock was held, expected 0", installer.calls)
	}
	if !s.IsInstalling() {
		t.Fatal("install() released a lock it did not hold")
	}
}

// through the manager's factory and hands the manager's installer to the server.
func TestInitServerUsesManagerHooks(t *testing.T) {
	setHookConfig(t)

	installer := &hookInstaller{}
	var factoryCalls int
	env := &hookEnvironment{bus: events.NewBus()}
	factory := func(s *Server, cfg *environment.Configuration) (environment.ProcessEnvironment, error) {
		factoryCalls++
		env.cfg = cfg
		return env, nil
	}
	m := NewEmptyManager(nil, WithEnvironmentFactory(factory), WithInstaller(installer))

	s, err := m.InitServer("2d9b0c7e-0000-4000-8000-000000000000", remote.ServerConfigurationResponse{
		Settings:             hookServerSettings("2d9b0c7e-0000-4000-8000-000000000000"),
		ProcessConfiguration: &remote.ProcessConfiguration{},
	})
	if err != nil {
		t.Fatalf("InitServer() returned unexpected error: %v", err)
	}
	defer s.CtxCancel()

	if factoryCalls != 1 {
		t.Fatalf("environment factory called %d times, expected 1", factoryCalls)
	}
	if s.Environment != env {
		t.Fatal("server environment is not the one returned by the factory")
	}
	if s.installer != installer {
		t.Fatal("server installer is not the one set on the manager")
	}
}

// TestInitServerFactoryError checks that InitServer returns the error of the
// environment factory.
func TestInitServerFactoryError(t *testing.T) {
	setHookConfig(t)

	factoryErr := errors.New("no runtime")
	factory := func(*Server, *environment.Configuration) (environment.ProcessEnvironment, error) {
		return nil, factoryErr
	}
	m := NewEmptyManager(nil, WithEnvironmentFactory(factory))

	_, err := m.InitServer("2d9b0c7e-0000-4000-8000-000000000001", remote.ServerConfigurationResponse{
		Settings:             hookServerSettings("2d9b0c7e-0000-4000-8000-000000000001"),
		ProcessConfiguration: &remote.ProcessConfiguration{},
	})
	if !errors.Is(err, factoryErr) {
		t.Fatalf("InitServer() error = %v, expected %v", err, factoryErr)
	}
}

// TestSyncWithEnvironmentConfiguresImageAndStop checks that
// SyncWithEnvironment passes the image and stop configuration to an
// environment implementing ImageAndStopConfigurable.
func TestSyncWithEnvironmentConfiguresImageAndStop(t *testing.T) {
	setHookConfig(t)

	env := &hookEnvironment{bus: events.NewBus()}
	factory := func(_ *Server, cfg *environment.Configuration) (environment.ProcessEnvironment, error) {
		env.cfg = cfg
		return env, nil
	}
	m := NewEmptyManager(nil, WithEnvironmentFactory(factory))
	s, err := m.InitServer("2d9b0c7e-0000-4000-8000-000000000002", remote.ServerConfigurationResponse{
		Settings: hookServerSettings("2d9b0c7e-0000-4000-8000-000000000002"),
		ProcessConfiguration: &remote.ProcessConfiguration{
			Stop: remote.ProcessStopConfiguration{Type: remote.ProcessStopCommand, Value: "stop"},
		},
	})
	if err != nil {
		t.Fatalf("InitServer() returned unexpected error: %v", err)
	}
	defer s.CtxCancel()
	s.cfg.Container.Image = "game:1"

	s.SyncWithEnvironment()

	if env.image != "game:1" {
		t.Fatalf("environment image = %q, expected %q", env.image, "game:1")
	}
	if env.stop.Type != remote.ProcessStopCommand || env.stop.Value != "stop" {
		t.Fatalf("environment stop configuration = %+v, expected the server's", env.stop)
	}
}

func TestWithRestoreTLSConfig(t *testing.T) {
	if cfg := NewEmptyManager(nil).RestoreTLSConfig(); cfg != nil {
		t.Fatalf("RestoreTLSConfig() = %v without the option, expected nil", cfg)
	}
	calls := 0
	m := NewEmptyManager(nil, WithRestoreTLSConfig(func() *tls.Config {
		calls++
		return &tls.Config{ServerName: "gateway"}
	}))
	for i := 0; i < 2; i++ {
		if cfg := m.RestoreTLSConfig(); cfg == nil || cfg.ServerName != "gateway" {
			t.Fatalf("RestoreTLSConfig() = %v, expected the supplied configuration", cfg)
		}
	}
	if calls != 2 {
		t.Fatalf("the configuration function was called %d times, expected once per restore", calls)
	}
}
