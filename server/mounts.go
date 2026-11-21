package server

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"syscall"

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
//
// Both the source of each mount and the allowed mount points are resolved before
// they are compared, and the resolved source is what is mounted. Otherwise a
// symlink, which a server could create through a writable parent directory,
// could point a mount anywhere on the host.
func (s *Server) customMounts() []environment.Mount {
	var mounts []environment.Mount

	// Resolving the allowed mounts touches the filesystem, which can block if one
	// of them is on a network share that is down, so only do it for servers that
	// have mounts.
	if len(s.Config().Mounts) == 0 {
		return nil
	}

	cfg := config.Get()
	var allowed []string
	for _, a := range cfg.AllowedMounts {
		resolved, err := resolveMountPath(a, cfg.System.User.Uid)
		if err != nil {
			s.Log().WithField("allowed_mount", a).WithField("error", err).Warn("ignoring allowed mount point")
			continue
		}
		// Only allow the root directory if it was configured as the root directory.
		if resolved == "/" && filepath.Clean(a) != "/" {
			s.Log().WithField("allowed_mount", a).Warn("ignoring allowed mount point, it resolves to the root directory")
			continue
		}
		allowed = append(allowed, resolved)
	}

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
			s.publishSkippedMount(target)
			continue
		}

		// Check if the target path includes /home/container
		if strings.HasPrefix(target, "/home/container") && cfg.BlockBaseDirMount {
			logger.WithField("invalid_target_path", target).Warn("Skipping custom server mount; target path includes /home/container")
			s.publishSkippedMount(target)
			continue
		}

		// The source is checked against the allowed mounts once it is resolved.
		resolved, err := resolveMountPath(source, cfg.System.User.Uid)
		if err != nil {
			logger.WithField("error", err).Warn("skipping custom server mount")
			s.publishSkippedMount(target)
			continue
		}

		mounted := false
		for _, a := range allowed {
			if !isAllowedMountSource(resolved, a) {
				continue
			}
			mounted = true
			mounts = append(mounts, environment.Mount{
				Source:   resolved,
				Target:   target,
				ReadOnly: m.ReadOnly,
			})
			break
		}

		if !mounted {
			logger.WithField("resolved_source_path", resolved).Warn("skipping custom server mount, not in list of allowed mount points")
			s.publishSkippedMount(target)
		}
	}

	return mounts
}

// publishSkippedMount tells the user in the server console that a mount was
// skipped, since anything written to its target would otherwise silently end up
// inside the container instead. The reason is only logged, as it includes paths
// on the host.
func (s *Server) publishSkippedMount(target string) {
	s.PublishConsoleOutputFromDaemon(fmt.Sprintf("Skipped the mount for %s, ask an administrator to check the Wings logs.", target))
}

// resolveMountPath resolves any symlinks in an absolute path that will be bind
// mounted into a container. If the path does not exist, the part of it that
// does is resolved.
//
// This returns an error if any directory above the path, before or after it is
// resolved, could be modified by the user containers run as.
func resolveMountPath(p string, containerUid int) (string, error) {
	p = filepath.Clean(p)
	if !filepath.IsAbs(p) {
		return "", errors.New("path is not absolute")
	}

	resolved, err := filepath.EvalSymlinks(p)
	if err != nil {
		if !errors.Is(err, fs.ErrNotExist) {
			return "", err
		}
		// Resolve the deepest part of the path that exists.
		existing, missing := p, ""
		for {
			existing, missing = filepath.Dir(existing), filepath.Join(filepath.Base(existing), missing)
			if resolved, err = filepath.EvalSymlinks(existing); err == nil {
				resolved = filepath.Join(resolved, missing)
				break
			}
			if !errors.Is(err, fs.ErrNotExist) {
				return "", err
			}
		}
	}

	for _, v := range []string{p, resolved} {
		if err := checkMountPathParents(v, containerUid); err != nil {
			return "", err
		}
	}
	return resolved, nil
}

// mountPathCheckRoot is the highest directory checked by checkMountPathParents.
// It is only changed by tests, which cannot control the permissions of the
// directories above their temporary directory.
var mountPathCheckRoot = "/"

// checkMountPathParents returns an error if any existing directory above the
// path is owned by the user containers run as, or is writable by its group or
// other users, since either could replace part of the path with a symlink.
func checkMountPathParents(p string, containerUid int) error {
	for d := filepath.Dir(p); ; d = filepath.Dir(d) {
		st, err := os.Lstat(d)
		if err != nil {
			if !errors.Is(err, fs.ErrNotExist) {
				return err
			}
		} else if st.IsDir() {
			sys, ok := st.Sys().(*syscall.Stat_t)
			if !ok {
				return fmt.Errorf("unable to determine owner of %s", d)
			}
			if containerUid != os.Geteuid() && int(sys.Uid) == containerUid {
				return fmt.Errorf("%s is owned by the user servers run as", d)
			}
			if st.Mode().Perm()&0o022 != 0 {
				return fmt.Errorf("%s is writable by users other than its owner", d)
			}
		}
		if d == mountPathCheckRoot || d == "/" {
			return nil
		}
	}
}

// isAllowedMountSource reports whether the resolved source path is the resolved
// allowed mount point itself or a path beneath it. A plain prefix check is not
// enough since an allowed mount of "/mnt" would otherwise also allow "/mnt2".
func isAllowedMountSource(source string, allowed string) bool {
	if source == allowed || allowed == string(filepath.Separator) {
		return true
	}
	return strings.HasPrefix(source, allowed+string(filepath.Separator))
}
