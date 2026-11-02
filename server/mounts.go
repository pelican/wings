package server

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	"github.com/apex/log"

	"github.com/pelican/wings/config"
	"github.com/pelican/wings/environment"
)

// To avoid confusion when working with mounts, assume that a server.Mount has not been properly
// cleaned up and had the paths set. An environment.Mount should only be returned with valid paths
// that have been checked.
type Mount environment.Mount

// Returns the default container mounts for the server instance. This includes the data directory
// for the server. Previously this would also mount in host timezone files, however we've moved from
// that approach to just setting `TZ=Timezone` environment values in containers which should work
// in most scenarios.
func (s *Server) Mounts() []environment.Mount {
	m := []environment.Mount{
		{
			Default:  true,
			Target:   "/home/container",
			Source:   s.Filesystem().Path(),
			ReadOnly: false,
		},
	}

	if config.Get().System.User.Passwd.Enable {
		passwdMount := environment.Mount{
			Target:   "/etc/passwd",
			Source:   filepath.Join(config.Get().System.User.Passwd.Directory, "passwd"),
			ReadOnly: true,
		}

		m = append(m, passwdMount)

		groupMount := environment.Mount{
			Target:   "/etc/group",
			Source:   filepath.Join(config.Get().System.User.Passwd.Directory, "group"),
			ReadOnly: true,
		}

		m = append(m, groupMount)
	}

	if config.Get().System.MachineID.Enable {
		machineIDMount := environment.Mount{
			Target:   "/etc/machine-id",
			Source:   filepath.Join(config.Get().System.MachineID.Directory, s.ID()),
			ReadOnly: true,
		}

		m = append(m, machineIDMount)
	}
	// Also include any of this server's custom mounts when returning them.
	return append(m, s.customMounts()...)
}

// Returns the custom mounts for a given server after verifying that they are within a list of
// allowed mount points for the node.
func (s *Server) customMounts() []environment.Mount {
	var mounts []environment.Mount

	for _, m := range s.Config().Mounts {
		source := filepath.Clean(m.Source)
		target := filepath.Clean(m.Target)

		logger := s.Log().WithFields(log.Fields{
			"source_path": source,
			"target_path": target,
			"read_only":   m.ReadOnly,
		})

		// Check if the source path exists
		if _, err := os.Stat(source); os.IsNotExist(err) {
			logger.WithField("missing_source_path", source).Warn("skipping custom server mount, source path does not exist")
			continue
		}

		// Check if the target path includes /home/container
		if strings.HasPrefix(target, "/home/container") && config.Get().BlockBaseDirMount {
			logger.WithField("invalid_target_path", target).Warn("Skipping custom server mount; target path includes /home/container")
			continue
		}

		mounted := false
		for _, allowed := range config.Get().AllowedMounts {
			// Check if the source path is included in the allowed mounts list.
			// filepath.Clean will strip all trailing slashes (unless the path is a root directory).
			if !strings.HasPrefix(source, filepath.Clean(allowed)) {
				continue
			}

			mounted = true
			device, err := isDevicePath(source)
			if err != nil {
				logger.WithField("error", err).Warn("failed to check if custom server mount is a device, mounting it without device access")
			}
			mounts = append(mounts, environment.Mount{
				Source:   source,
				Target:   target,
				ReadOnly: m.ReadOnly,
				Device:   device,
			})

			break
		}

		if !mounted {
			logger.Warn("skipping custom server mount, not in list of allowed mount points")
		}
	}

	return mounts
}

// isDevicePath returns true if the path is a device node, or a directory under /dev
// containing at least one device node (e.g. /dev/dri). A symlinked path is resolved
// first, but symlinks inside a directory are not followed. Directories outside of /dev
// are never walked, so that regular data mounts are not scanned on every sync. A missing
// path is not a device; any other error while inspecting the path is returned.
func isDevicePath(path string) (bool, error) {
	// WalkDir does not follow a symlinked root, so resolve it before walking.
	path, err := filepath.EvalSymlinks(path)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return false, nil
		}
		return false, err
	}
	st, err := os.Stat(path)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return false, nil
		}
		return false, err
	}
	if st.Mode()&os.ModeDevice != 0 {
		return true, nil
	}
	if !st.IsDir() || (path != "/dev" && !strings.HasPrefix(path, "/dev/")) {
		return false, nil
	}

	found := false
	err = filepath.WalkDir(path, func(_ string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.Type()&os.ModeDevice != 0 {
			found = true
			return filepath.SkipAll
		}
		return nil
	})
	return found, err
}
