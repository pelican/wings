package environment

import (
	"net/netip"
	"path/filepath"
	"testing"

	"github.com/moby/moby/api/types/network"
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
	tcp := func(p uint16) network.Port {
		port, _ := network.PortFrom(p, network.TCP)
		return port
	}
	addr := netip.MustParseAddr

	tests := []struct {
		name      string
		iface     string
		ispn      bool
		mappings  map[string][]int
		wantErr   bool
		wantTCP   map[network.Port][]network.PortBinding
		wantPorts int
	}{
		{
			name:      "no allocations",
			iface:     "172.18.0.1",
			mappings:  nil,
			wantPorts: 0,
		},
		{
			name:      "no allocations with a malformed interface",
			iface:     "not-an-ip",
			mappings:  map[string][]int{},
			wantPorts: 0,
		},
		{
			name:     "loopback rewritten to the interface",
			iface:    "172.18.0.1",
			mappings: map[string][]int{"127.0.0.1": {25565}},
			wantTCP: map[network.Port][]network.PortBinding{
				tcp(25565): {{HostIP: addr("172.18.0.1"), HostPort: "25565"}},
			},
			wantPorts: 2,
		},
		{
			name:     "empty interface binds loopback on all interfaces",
			iface:    "",
			mappings: map[string][]int{"127.0.0.1": {25565}},
			wantTCP: map[network.Port][]network.PortBinding{
				tcp(25565): {{HostPort: "25565"}},
			},
			wantPorts: 2,
		},
		{
			name:     "malformed interface rejects loopback allocations",
			iface:    "not-an-ip",
			mappings: map[string][]int{"127.0.0.1": {25565}},
			wantErr:  true,
		},
		{
			name:     "malformed interface leaves public allocations alone",
			iface:    "not-an-ip",
			mappings: map[string][]int{"10.0.0.5": {25565}},
			wantTCP: map[network.Port][]network.PortBinding{
				tcp(25565): {{HostIP: addr("10.0.0.5"), HostPort: "25565"}},
			},
			wantPorts: 2,
		},
		{
			name:     "ISPN drops loopback allocations",
			iface:    "not-an-ip",
			ispn:     true,
			mappings: map[string][]int{"127.0.0.1": {25565}},
			wantTCP: map[network.Port][]network.PortBinding{
				tcp(25565): {},
			},
			wantPorts: 2,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			setDockerNetwork(t, tc.iface, tc.ispn)
			a := &Allocations{Mappings: tc.mappings}

			got, err := a.DockerBindings()
			if tc.wantErr {
				require.Error(t, err)
				require.Nil(t, got)
				return
			}
			require.NoError(t, err)
			require.Len(t, got, tc.wantPorts)
			for port, want := range tc.wantTCP {
				require.Equal(t, want, got[port])
			}
		})
	}
}
