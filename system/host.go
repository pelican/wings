package system

import (
	"strings"

	"github.com/acobaugh/osrelease"
	"github.com/shirou/gopsutil/v3/host"
)

// KernelVersion returns the release of the running kernel, for example
// "6.8.0-45-generic".
func KernelVersion() (string, error) {
	return host.KernelVersion()
}

// OperatingSystem returns a human-readable name for the host operating system.
// It prefers PRETTY_NAME from os-release and falls back to the platform name
// and version that gopsutil reports.
func OperatingSystem() (string, error) {
	if release, err := osrelease.Read(); err == nil {
		if v := release["PRETTY_NAME"]; v != "" {
			return v, nil
		}
		if v := release["NAME"]; v != "" {
			return v, nil
		}
	}

	platform, _, version, err := host.PlatformInformation()
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(platform + " " + version), nil
}
