package environment

import (
	"path/filepath"
	"testing"

	"github.com/docker/go-connections/nat"
	"github.com/stretchr/testify/require"

	"github.com/pelican/wings/config"
)

func setDockerNetwork(t *testing.T, iface string, ispn bool) {
	t.Helper()

	cfg, err := config.NewAtPath(filepath.Join(t.TempDir(), "config.yml"))
	require.NoError(t, err)
	cfg.AuthenticationTokenId = "id"
	cfg.AuthenticationToken = "token"
	cfg.Docker.Network.Interface = iface
	cfg.Docker.Network.ISPN = ispn
	config.Set(cfg)
}

func TestDockerBindings(t *testing.T) {
	tests := []struct {
		name      string
		ispn      bool
		mappings  map[string][]int
		wantTCP   map[nat.Port][]nat.PortBinding
		wantPorts int
	}{
		{
			name:     "loopback rewritten to the interface",
			mappings: map[string][]int{"127.0.0.1": {25565}},
			wantTCP: map[nat.Port][]nat.PortBinding{
				"25565/tcp": {{HostIP: "172.18.0.1", HostPort: "25565"}},
			},
			wantPorts: 2,
		},
		{
			name:     "every loopback binding on a port is rewritten",
			mappings: map[string][]int{"127.0.0.1": {25565, 25565}},
			wantTCP: map[nat.Port][]nat.PortBinding{
				"25565/tcp": {
					{HostIP: "172.18.0.1", HostPort: "25565"},
					{HostIP: "172.18.0.1", HostPort: "25565"},
				},
			},
			wantPorts: 2,
		},
		{
			// The port must be removed, not left with an empty binding list,
			// which Docker would publish on a random host port.
			name:      "ISPN removes a loopback-only port",
			ispn:      true,
			mappings:  map[string][]int{"127.0.0.1": {25565}},
			wantPorts: 0,
		},
		{
			name:      "ISPN drops every loopback binding on a port",
			ispn:      true,
			mappings:  map[string][]int{"127.0.0.1": {25565, 25565}},
			wantPorts: 0,
		},
		{
			name:     "ISPN keeps a port that still has a public binding",
			ispn:     true,
			mappings: map[string][]int{"127.0.0.1": {25565}, "10.0.0.5": {25565}},
			wantTCP: map[nat.Port][]nat.PortBinding{
				"25565/tcp": {{HostIP: "10.0.0.5", HostPort: "25565"}},
			},
			wantPorts: 2,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			setDockerNetwork(t, "172.18.0.1", tc.ispn)
			a := &Allocations{Mappings: tc.mappings}

			got := a.DockerBindings()
			require.Len(t, got, tc.wantPorts)
			for port, want := range tc.wantTCP {
				require.Equal(t, want, got[port])
			}

			// Exposed is derived from DockerBindings, so it must match its ports.
			exposed := a.Exposed()
			require.Len(t, exposed, len(got))
			for port := range got {
				require.Contains(t, exposed, port)
			}
		})
	}
}
