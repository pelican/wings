package docker

import (
	"strings"

	"github.com/apex/log"
	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/api/types/mount"

	"github.com/pelican/wings/config"
	"github.com/pelican/wings/plugins"
	"github.com/pelican/wings/plugins/api"
)

// applyPluginContainerPatch lets plugins adjust a container Wings is about to
// create, then puts back everything they are not allowed to change.
//
// The order matters and is the whole point. Plugins are asked first and their
// changes are merged in, but the security settings are reasserted afterwards,
// so a plugin cannot drop no-new-privileges, restore a dropped capability,
// make the root filesystem writable, or bind a host path the node has not
// allowed. Those are the invariants that keep one server from reaching another,
// and a plugin is arbitrary code the operator installed, not a reason to
// suspend them.
func applyPluginContainerPatch(uuid string, conf *container.Config, hostConf *container.HostConfig) {
	if !plugins.HasContainerHooks() {
		return
	}

	snapshot, ok := plugins.ServerSnapshot(uuid)
	if !ok {
		// The container is being built for a server the manager does not know
		// about yet, which happens during a transfer. Plugins are skipped
		// rather than handed a half-populated server.
		return
	}

	spec := api.ContainerSpec{
		Server: snapshot,
		Image:  conf.Image,
		Labels: copyMap(conf.Labels),
		Env:    envSliceToMap(conf.Env),
		Mounts: mountsToAPI(hostConf.Mounts),
	}

	patch := plugins.MutateContainer(spec)

	l := log.WithField("subsystem", "plugins").WithField("server", uuid)

	if patch.Image != "" && patch.Image != conf.Image {
		l.WithField("image", patch.Image).Info("a plugin changed the container image")
		conf.Image = patch.Image
	}

	if len(patch.Labels) > 0 {
		if conf.Labels == nil {
			conf.Labels = map[string]string{}
		}
		for k, v := range patch.Labels {
			// Wings' own labels identify what a container is and are what the
			// daemon uses to find its containers again, so a plugin does not
			// get to redefine them.
			if k == "Service" || k == "ContainerType" {
				l.WithField("label", k).Warn("a plugin tried to change a Wings container label; ignoring it")
				continue
			}
			conf.Labels[k] = v
		}
	}

	if len(patch.Env) > 0 {
		merged := envSliceToMap(conf.Env)
		for k, v := range patch.Env {
			merged[k] = v
		}
		conf.Env = envMapToSlice(merged)
	}

	for _, m := range patch.Mounts {
		if !mountAllowed(m.Source) {
			l.WithField("source", m.Source).
				Warn("a plugin asked for a mount that is not in the node's allowed_mounts; ignoring it")
			continue
		}
		hostConf.Mounts = append(hostConf.Mounts, mount.Mount{
			Type:     mount.TypeBind,
			Source:   m.Source,
			Target:   m.Target,
			ReadOnly: m.ReadOnly,
		})
	}

	if len(patch.Sysctls) > 0 {
		if hostConf.Sysctls == nil {
			hostConf.Sysctls = map[string]string{}
		}
		for k, v := range patch.Sysctls {
			hostConf.Sysctls[k] = v
		}
	}

	if len(patch.Devices) > 0 {
		if !config.Get().Plugins.AllowDevices {
			l.Warn("a plugin asked to pass through host devices, but allow_devices is off for this node; ignoring it")
		} else {
			for _, d := range patch.Devices {
				if parsed, ok := parseDevice(d); ok {
					hostConf.Devices = append(hostConf.Devices, parsed)
				} else {
					l.WithField("device", d).Warn("a plugin asked for a device in a format that could not be read; ignoring it")
				}
			}
		}
	}

	hostConf.ExtraHosts = append(hostConf.ExtraHosts, patch.ExtraHosts...)

	if patch.ShmSizeBytes > 0 {
		hostConf.ShmSize = patch.ShmSizeBytes
	}

	// Reassert the container's security posture. Everything above is a plugin
	// request; this is not negotiable.
	hostConf.SecurityOpt = []string{"no-new-privileges"}
	hostConf.ReadonlyRootfs = true
	hostConf.Privileged = false
	hostConf.CapAdd = nil
	hostConf.CapDrop = []string{
		"setpcap", "mknod", "audit_write", "net_raw", "dac_override",
		"fowner", "fsetid", "net_bind_service", "sys_chroot", "setfcap",
		"sys_ptrace",
	}
}

// mountAllowed reports whether the node's configuration permits binding a host
// path into a container. It is the same allowlist the Panel's mounts are held
// to, so a plugin cannot reach anywhere an administrator could not.
func mountAllowed(source string) bool {
	if source == "" {
		return false
	}

	clean := strings.TrimSuffix(source, "/")

	for _, allowed := range config.Get().AllowedMounts {
		allowed = strings.TrimSuffix(allowed, "/")
		if clean == allowed || strings.HasPrefix(clean, allowed+"/") {
			return true
		}
	}
	return false
}

// parseDevice reads Docker's "/dev/host:/dev/container:rwm" device syntax.
func parseDevice(spec string) (container.DeviceMapping, bool) {
	parts := strings.Split(spec, ":")

	switch len(parts) {
	case 1:
		return container.DeviceMapping{
			PathOnHost:        parts[0],
			PathInContainer:   parts[0],
			CgroupPermissions: "rwm",
		}, parts[0] != ""
	case 2:
		return container.DeviceMapping{
			PathOnHost:        parts[0],
			PathInContainer:   parts[1],
			CgroupPermissions: "rwm",
		}, parts[0] != "" && parts[1] != ""
	case 3:
		return container.DeviceMapping{
			PathOnHost:        parts[0],
			PathInContainer:   parts[1],
			CgroupPermissions: parts[2],
		}, parts[0] != "" && parts[1] != ""
	}
	return container.DeviceMapping{}, false
}

func copyMap(in map[string]string) map[string]string {
	out := make(map[string]string, len(in))
	for k, v := range in {
		out[k] = v
	}
	return out
}

// envSliceToMap turns Docker's "KEY=value" slice into a map. A variable whose
// value contains an equals sign keeps it, since only the first one separates.
func envSliceToMap(in []string) map[string]string {
	out := make(map[string]string, len(in))
	for _, entry := range in {
		k, v, found := strings.Cut(entry, "=")
		if !found {
			continue
		}
		out[k] = v
	}
	return out
}

func envMapToSlice(in map[string]string) []string {
	out := make([]string, 0, len(in))
	for k, v := range in {
		out = append(out, k+"="+v)
	}
	return out
}

func mountsToAPI(in []mount.Mount) []api.Mount {
	out := make([]api.Mount, 0, len(in))
	for _, m := range in {
		out = append(out, api.Mount{
			Source:   m.Source,
			Target:   m.Target,
			ReadOnly: m.ReadOnly,
		})
	}
	return out
}
