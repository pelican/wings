package server

import (
	"path/filepath"

	"emperror.dev/errors"

	"github.com/pelican/wings/config"
	"github.com/pelican/wings/environment/kubernetes"
	"github.com/pelican/wings/remote"
	"github.com/pelican/wings/system"
)

// internalInstallKubernetes runs the installation process using a Kubernetes
// Job instead of a Docker container. This is called when K8s mode is enabled.
func (s *Server) internalInstallKubernetes(script *remote.InstallationScript) error {
	env, ok := s.Environment.(*kubernetes.Environment)
	if !ok {
		return errors.New("install: kubernetes enabled but environment is not kubernetes")
	}

	tmpDir := filepath.Join(config.Get().System.TmpDirectory, s.ID())

	ip := kubernetes.NewInstallerProcess(
		env,
		script,
		s.GetEnvironmentVariables(),
		s.Filesystem().Path(),
		tmpDir,
		s.Sink(system.InstallSink),
	)

	if !s.installing.SwapIf(true) {
		return errors.New("install: cannot obtain installation lock")
	}
	defer s.installing.Store(false)

	s.Log().Info("beginning kubernetes job-based installation process for server")
	s.Events().Publish(DaemonMessageEvent, "Starting installation process via Kubernetes Job, this could take a few minutes...")

	// Run the Job and wait for completion.
	if err := ip.Run(s.Context()); err != nil {
		_ = ip.Cleanup(s.Context())
		return errors.WithMessage(err, "install: kubernetes installer job failed")
	}

	s.Events().Publish(DaemonMessageEvent, "Installation process completed.")
	s.Log().Info("completed kubernetes job-based installation process for server")

	// Cleanup the Job and temp files.
	if err := ip.Cleanup(s.Context()); err != nil {
		s.Log().WithField("error", err).Warn("failed to clean up installer job resources")
	}

	return nil
}
