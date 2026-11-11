package server

import (
	"github.com/pelican/wings/environment"
	"github.com/pelican/wings/environment/docker"
	"github.com/pelican/wings/remote"
)

// EnvironmentFactory builds the process environment that runs a server. The
// default factory creates a Docker environment; alternative implementations can
// be registered on the server manager with WithEnvironmentFactory so that Wings
// can be embedded into daemons that run server processes elsewhere.
type EnvironmentFactory func(s *Server, cfg *environment.Configuration) (environment.ProcessEnvironment, error)

// DockerEnvironmentFactory is the default EnvironmentFactory. It creates a
// Docker environment using the server's configured container image.
func DockerEnvironmentFactory(s *Server, cfg *environment.Configuration) (environment.ProcessEnvironment, error) {
	meta := docker.Metadata{
		Image: s.Config().Container.Image,
	}
	return docker.New(s.ID(), &meta, cfg)
}

// ImageAndStopConfigurable is implemented by environments that need to be told
// about the container image and the stop configuration of the server whenever
// the server configuration is synced with the Panel.
type ImageAndStopConfigurable interface {
	SetImage(string)
	SetStopConfiguration(remote.ProcessStopConfiguration)
}

// Attachable is implemented by environments that need to be attached to a
// process stream before commands can be sent to it.
type Attachable interface {
	IsAttached() bool
}

// The Docker environment must keep satisfying the optional interfaces, as the
// type assertions on them would otherwise silently stop matching.
var (
	_ ImageAndStopConfigurable = (*docker.Environment)(nil)
	_ Attachable               = (*docker.Environment)(nil)
)
