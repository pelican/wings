package docker

import (
	"context"
	"io"
	"math"
	"strings"
	"time"

	"emperror.dev/errors"
	"github.com/docker/docker/api/types/container"
	"github.com/goccy/go-json"

	"github.com/pelican/wings/environment"
)

// Uptime returns the current uptime of the container in milliseconds. If the
// container is not currently running this will return 0.
func (e *Environment) Uptime(ctx context.Context) (int64, error) {
	ins, err := e.ContainerInspect(ctx)
	if err != nil {
		return 0, errors.Wrap(err, "environment: could not inspect container")
	}
	if !ins.State.Running {
		return 0, nil
	}
	started, err := time.Parse(time.RFC3339, ins.State.StartedAt)
	if err != nil {
		return 0, errors.Wrap(err, "environment: failed to parse container start time")
	}
	return time.Since(started).Milliseconds(), nil
}

// Attach to the instance and then automatically emit an event whenever the resource usage for the
// server process changes.
func (e *Environment) pollResources(ctx context.Context) error {
	if e.st.Load() == environment.ProcessOfflineState {
		return errors.New("cannot enable resource polling on a stopped server")
	}

	e.log().Info("starting resource polling for container")
	defer e.log().Debug("stopped resource polling for container")

	stats, err := e.client.ContainerStats(ctx, e.Id, true)
	if err != nil {
		return err
	}
	defer stats.Body.Close()

	uptime, err := e.Uptime(ctx)
	if err != nil {
		e.log().WithField("error", err).Warn("failed to calculate container uptime")
	}

	dec := json.NewDecoder(stats.Body)
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		default:
			var v container.StatsResponse
			if err := dec.Decode(&v); err != nil {
				if err != io.EOF && !errors.Is(err, context.Canceled) {
					e.log().WithField("error", err).Warn("error while processing Docker stats output for container")
				} else {
					e.log().Debug("io.EOF encountered during stats decode, stopping polling...")
				}
				return nil
			}

			// Disable collection if the server is in an offline state and this process is still running.
			if e.st.Load() == environment.ProcessOfflineState {
				e.log().Debug("process in offline state while resource polling is still active; stopping poll")
				return nil
			}

			if !v.PreRead.IsZero() {
				uptime = uptime + v.Read.Sub(v.PreRead).Milliseconds()
			}

			st := environment.Stats{
				Uptime:      uptime,
				Memory:      calculateDockerMemory(v.MemoryStats),
				MemoryLimit: v.MemoryStats.Limit,
				CpuAbsolute: calculateDockerAbsoluteCpu(v),
				Network:     environment.NetworkStats{},
			}

			for _, nw := range v.Networks {
				st.Network.RxBytes += nw.RxBytes
				st.Network.TxBytes += nw.TxBytes
			}

			// Docker surfaces cgroup block I/O counters through the blkio
			// recursive list on both cgroup v1 ("Read"/"Write") and v2
			// ("read"/"write" from io.stat), so match the op case-insensitively.
			for _, bio := range v.BlkioStats.IoServiceBytesRecursive {
				if strings.EqualFold(bio.Op, "read") {
					st.DiskIo.ReadBytes += bio.Value
				} else if strings.EqualFold(bio.Op, "write") {
					st.DiskIo.WriteBytes += bio.Value
				}
			}

			e.Events().Publish(environment.ResourceEvent, st)
		}
	}
}

// The "docker stats" CLI call does not return the same value as the types.MemoryStats.Usage
// value which can be rather confusing to people trying to compare panel usage to
// their stats output.
//
// This math is from their CLI repository in order to show the same values to avoid people
// bothering me about it. It should also reflect a slightly more correct memory value anyways.
//
// @see https://github.com/docker/cli/blob/96e1d1d6/cli/command/container/stats_helpers.go#L227-L249
func calculateDockerMemory(stats container.MemoryStats) uint64 {
	if v, ok := stats.Stats["total_inactive_file"]; ok && v < stats.Usage {
		return stats.Usage - v
	}

	if v := stats.Stats["inactive_file"]; v < stats.Usage {
		return stats.Usage - v
	}

	return stats.Usage
}

// Calculates the absolute CPU usage used by the server process on the system, not constrained
// by the defined CPU limits on the container.
//
// CPU time is compared to the elapsed time between samples because Podman's Docker-compatible API
// does not provide Docker-equivalent SystemUsage values.
func calculateDockerAbsoluteCpu(stats container.StatsResponse) float64 {
	current := stats.CPUStats.CPUUsage.TotalUsage
	previous := stats.PreCPUStats.CPUUsage.TotalUsage
	if current <= previous || stats.PreRead.IsZero() || !stats.Read.After(stats.PreRead) {
		return 0
	}

	cpuDelta := float64(current - previous)
	timeDelta := float64(stats.Read.Sub(stats.PreRead).Nanoseconds())
	return math.Round((cpuDelta/timeDelta)*100*1000) / 1000
}
