package environment

import (
	"context"
	"net/netip"
	"strconv"
	"strings"
	"sync"

	"emperror.dev/errors"
	"github.com/apex/log"
	cerrdefs "github.com/containerd/errdefs"
	"github.com/moby/moby/api/types/network"
	"github.com/moby/moby/client"

	"github.com/pelican/wings/config"
)

var (
	_conce  sync.Once
	_client *client.Client
)

// Docker returns a docker client to be used throughout the codebase. Once a
// client has been created it will be returned for all subsequent calls to this
// function.
func Docker() (*client.Client, error) {
	var err error
	_conce.Do(func() {
		_client, err = client.New(client.FromEnv)
	})
	return _client, errors.Wrap(err, "environment/docker: could not create client")
}

// ConfigureDocker configures the required network for the docker environment.
func ConfigureDocker(ctx context.Context) error {
	// Ensure the required docker network exists on the system.
	cli, err := Docker()
	if err != nil {
		return err
	}

	nw := config.Get().Docker.Network
	inspect, err := cli.NetworkInspect(ctx, nw.Name, client.NetworkInspectOptions{})
	if err != nil {
		if !cerrdefs.IsNotFound(err) {
			return err
		}

		log.Info("creating missing pelican0 interface, this could take a few seconds...")
		if err := createDockerNetwork(ctx, cli); err != nil {
			return err
		}

		// Re-inspect the network after creation to get the actual configuration
		inspect, err = cli.NetworkInspect(ctx, nw.Name, client.NetworkInspectOptions{})
		if err != nil {
			return errors.Wrap(err, "environment/docker: failed to inspect newly created network")
		}
	}
	resource := inspect.Network

	config.Update(func(c *config.Configuration) {
		c.Docker.Network.Driver = resource.Driver
		switch c.Docker.Network.Driver {
		case "host":
			c.Docker.Network.Interface = "127.0.0.1"
			c.Docker.Network.ISPN = false
		case "overlay":
			fallthrough
		case "weavemesh":
			c.Docker.Network.Interface = ""
			c.Docker.Network.ISPN = true
		default:
			c.Docker.Network.ISPN = false
		}

		// Update the interface configuration with the actual assigned values from Docker
		// Skip IPAM processing for special drivers that don't have normal IPAM configs
		if c.Docker.Network.Driver != "host" && c.Docker.Network.Driver != "overlay" && c.Docker.Network.Driver != "weavemesh" {
			for _, ipamCfg := range resource.IPAM.Config {
				if !ipamCfg.Subnet.IsValid() {
					continue
				}
				if ipamCfg.Subnet.Addr().Is6() {
					c.Docker.Network.Interfaces.V6.Subnet = ipamCfg.Subnet.String()
					if ipamCfg.Gateway.IsValid() {
						c.Docker.Network.Interfaces.V6.Gateway = ipamCfg.Gateway.String()
					}
				} else {
					c.Docker.Network.Interfaces.V4.Subnet = ipamCfg.Subnet.String()
					if ipamCfg.Gateway.IsValid() {
						c.Docker.Network.Interfaces.V4.Gateway = ipamCfg.Gateway.String()
						c.Docker.Network.Interface = ipamCfg.Gateway.String()
					}
				}
			}
		}
	})
	return nil
}

// Creates a new network on the machine if one does not exist already.
// If the configured subnet conflicts with existing networks, it will automatically
// retry with Docker auto-assigning the subnet to avoid "Pool overlaps" errors.
func createDockerNetwork(ctx context.Context, cli *client.Client) error {
	nw := config.Get().Docker.Network
	enableIPv6 := nw.IPv6

	// Build IPAM config with the configured subnets
	ipamConfigs := []network.IPAMConfig{}
	if nw.Interfaces.V4.Subnet != "" {
		cfg, err := ipamConfig(nw.Interfaces.V4.Subnet, nw.Interfaces.V4.Gateway)
		if err != nil {
			return errors.Wrap(err, "environment/docker: invalid IPv4 network interface configuration")
		}
		ipamConfigs = append(ipamConfigs, cfg)
	}
	if enableIPv6 && nw.Interfaces.V6.Subnet != "" {
		cfg, err := ipamConfig(nw.Interfaces.V6.Subnet, nw.Interfaces.V6.Gateway)
		if err != nil {
			return errors.Wrap(err, "environment/docker: invalid IPv6 network interface configuration")
		}
		ipamConfigs = append(ipamConfigs, cfg)
	}

	createOpts := client.NetworkCreateOptions{
		Driver:     nw.Driver,
		EnableIPv6: &enableIPv6,
		Internal:   nw.IsInternal,
		IPAM: &network.IPAM{
			Config: ipamConfigs,
		},
		Options: map[string]string{
			"encryption": "false",
			"com.docker.network.bridge.default_bridge":       "false",
			"com.docker.network.bridge.enable_icc":           strconv.FormatBool(nw.EnableICC),
			"com.docker.network.bridge.enable_ip_masquerade": "true",
			"com.docker.network.bridge.host_binding_ipv4":    "0.0.0.0",
			"com.docker.network.bridge.name":                 "pelican0",
			"com.docker.network.driver.mtu":                  strconv.FormatInt(nw.NetworkMTU, 10),
		},
	}

	// Try to create the network with the configured subnet
	_, err := cli.NetworkCreate(ctx, nw.Name, createOpts)
	if err != nil {
		// Check if the error is a pool overlap issue
		errStr := err.Error()
		if strings.Contains(errStr, "Pool overlaps") || strings.Contains(errStr, "invalid pool request") {
			log.Warn("configured subnet conflicts with existing network, letting Docker auto-assign subnet...")
			
			// Retry without specifying IPAM config - let Docker auto-assign
			createOpts.IPAM = &network.IPAM{
				Driver: "default",
				// Don't specify Config - let Docker choose available subnets
			}
			
			_, err = cli.NetworkCreate(ctx, nw.Name, createOpts)
			if err != nil {
				return errors.Wrap(err, "environment/docker: failed to create network even with auto-assigned subnet")
			}
			
			log.Info("network created successfully with Docker auto-assigned subnet")
		} else {
			return errors.Wrap(err, "environment/docker: failed to create network")
		}
	}

	return nil
}

// ipamConfig parses a subnet and optional gateway from the Wings configuration
// into the form expected by the Docker API.
func ipamConfig(subnet, gateway string) (network.IPAMConfig, error) {
	var cfg network.IPAMConfig
	prefix, err := netip.ParsePrefix(strings.TrimSpace(subnet))
	if err != nil {
		return cfg, errors.Wrapf(err, "invalid subnet %q", subnet)
	}
	cfg.Subnet = prefix
	if gateway = strings.TrimSpace(gateway); gateway != "" {
		addr, err := netip.ParseAddr(gateway)
		if err != nil {
			return cfg, errors.Wrapf(err, "invalid gateway %q", gateway)
		}
		cfg.Gateway = addr
	}
	return cfg, nil
}
