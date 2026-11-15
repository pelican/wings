package docker

import (
	"context"
	"os"
	"path"
	"strconv"
	"strings"
	"sync"

	"emperror.dev/errors"
	"github.com/apex/log"
	"github.com/docker/docker/client"

	"github.com/pelican/wings/config"
)

// cgroupV2 reports whether the host uses the unified cgroup v2 hierarchy.
var cgroupV2 = sync.OnceValue(func() bool {
	_, err := os.Stat("/sys/fs/cgroup/cgroup.controllers")
	return err == nil
})

var burstWarning sync.Once

// cpuBurstMicroseconds returns the burst allowance in microseconds for the given
// CFS quota and configured percentage. The kernel rejects a burst larger than the
// quota, so the value is clamped to it.
func cpuBurstMicroseconds(quota int64, percent int64) int64 {
	if quota <= 0 || percent <= 0 {
		return 0
	}
	if percent > 100 {
		percent = 100
	}
	return quota * percent / 100
}

// resolveCgroupCpuFile parses the contents of a /proc/<pid>/cgroup file and
// returns the absolute path of the CFS burst file for that process's cgroup.
// The cgroup path must contain the container ID so that a recycled PID, which
// would belong to an unrelated cgroup, is never written to. This holds for both
// the /docker/<id> and docker-<id>.scope layouts.
func resolveCgroupCpuFile(procCgroup string, v2 bool, containerID string) (string, error) {
	if containerID == "" {
		return "", errors.New("environment/docker: container ID is required to resolve cgroup path")
	}
	outsideNamespace := false
	for _, line := range strings.Split(procCgroup, "\n") {
		parts := strings.SplitN(line, ":", 3)
		if len(parts) != 3 || !strings.HasPrefix(parts[2], "/") {
			continue
		}
		// Paths are rendered relative to the reader's cgroup namespace. A target
		// outside of it shows up as /../.. and cannot be resolved safely.
		if strings.Contains(parts[2], "..") {
			outsideNamespace = true
			continue
		}

		var file string
		if v2 {
			if parts[0] != "0" || parts[1] != "" {
				continue
			}
			file = path.Join("/sys/fs/cgroup", parts[2], "cpu.max.burst")
		} else {
			for _, controller := range strings.Split(parts[1], ",") {
				if controller == "cpu" {
					file = path.Join("/sys/fs/cgroup/cpu", parts[2], "cpu.cfs_burst_us")
					break
				}
			}
			if file == "" {
				continue
			}
		}

		if !strings.Contains(parts[2], containerID) {
			return "", errors.Errorf("environment/docker: cgroup path %q does not belong to container %s", parts[2], containerID)
		}
		return file, nil
	}
	if outsideNamespace {
		return "", errors.New("environment/docker: container cgroup is not visible from this cgroup namespace")
	}
	return "", errors.New("environment/docker: no cpu controller found in cgroup file")
}


// writeCpuBurst writes a burst value in microseconds into the cpu cgroup of the
// given process. This is expected to fail on kernels older than 5.14 or when the
// cgroup hierarchy is not writable by Wings, so failures are only logged.
func writeCpuBurst(l *log.Entry, containerID string, pid int, burst int64) {
	if pid <= 0 {
		return
	}
	if err := writeBurstFile(containerID, pid, burst); err != nil {
		logBurstFailure(l.WithField("error", err), burst)
		return
	}
	l.WithField("burst_us", burst).Debug("updated container cpu burst")
}

func writeBurstFile(containerID string, pid int, burst int64) error {
	b, err := os.ReadFile("/proc/" + strconv.Itoa(pid) + "/cgroup")
	if err != nil {
		return err
	}
	f, err := resolveCgroupCpuFile(string(b), cgroupV2(), containerID)
	if err != nil {
		return err
	}
	return os.WriteFile(f, []byte(strconv.FormatInt(burst, 10)), 0o644)
}

// logBurstFailure warns the first time a burst cannot be applied and stays at
// debug otherwise. Failed clears are always quiet since a host that never
// accepted a burst has nothing to clear.
func logBurstFailure(l *log.Entry, burst int64) {
	if burst > 0 {
		first := false
		burstWarning.Do(func() { first = true })
		if first {
			l.Warn("failed to set cpu burst, this requires Linux 5.14 or newer and a writable cgroup hierarchy")
			return
		}
	}
	l.Debug("failed to set cpu burst")
}

// SetCpuBurst applies the configured CFS burst to a running container based on
// the CFS quota in microseconds it was created with. When bursting is disabled
// or the container has no CPU limit, the burst is cleared instead, so a value
// left over from an earlier configuration does not outlive the setting.
func SetCpuBurst(ctx context.Context, cli *client.Client, containerID string, quota int64) {
	cfg := config.Get().Docker.CpuBurst

	var burst int64
	if cfg.Enabled {
		// Returns 0 when quota <= 0 or percent <= 0.
		burst = cpuBurstMicroseconds(quota, cfg.Percent)
	}

	c, err := cli.ContainerInspect(ctx, containerID)
	if err != nil || c.State == nil {
		return
	}
	writeCpuBurst(log.WithField("container_id", containerID), c.ID, c.State.Pid, burst)
}

// applyCpuBurst applies the configured CFS burst to the environment's container
// using its current CPU limit.
func (e *Environment) applyCpuBurst(ctx context.Context) {
	//quota := e.Configuration.Limits().CpuLimit * config.Get().Docker.CpuPeriodMicroseconds() / 100
	
	// applyCpuBurst applies the configured CFS burst to the environment's container
	// using the same CPU quota that is applied to the container itself.
	quota := e.Configuration.Limits().AsContainerResources().CPUQuota
	SetCpuBurst(ctx, e.client, e.Id, quota)
}

// clearCpuBurst zeroes the CFS burst for the given container process. This must
// happen before a quota change is applied since the kernel rejects a quota lower
// than the current burst. It runs even when bursting is disabled so a value set
// before the feature was turned off cannot block future quota changes.
func (e *Environment) clearCpuBurst(containerID string, pid int) {
	writeCpuBurst(e.log(), containerID, pid, 0)
}