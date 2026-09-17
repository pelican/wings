package kubernetes

import (
	"os"
	"testing"

	"github.com/pelican/wings/config"
)

func TestMain(m *testing.M) {
	// Initialize a minimal config so config.Get() doesn't panic.
	c := &config.Configuration{}
	c.AuthenticationToken = "test-token-for-testing"
	c.Kubernetes.Namespace = "pelican"
	c.Kubernetes.NetworkMode = config.KubeNetworkHostPort
	c.Kubernetes.NodeName = "test-node"
	c.Kubernetes.NodePortRangeMin = 30000
	c.Kubernetes.NodePortRangeMax = 32767
	config.Set(c)

	os.Exit(m.Run())
}
