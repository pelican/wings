package kubernetes

import (
	"context"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
	"time"

	"emperror.dev/errors"

	"github.com/pelican/wings/environment"
)

// statsRequestTimeout bounds each individual metrics/stats API call so a slow
// or hung endpoint cannot stall the polling loop until the next tick.
const statsRequestTimeout = 3 * time.Second

// podStats holds parsed resource metrics for a pod's "server" container.
type podStats struct {
	cpuPercent  float64
	memoryBytes int64
	rxBytes     uint64
	txBytes     uint64
}

// Uptime returns the uptime of the server Pod in milliseconds. If the Pod is
// not running, returns 0.
func (e *Environment) Uptime(ctx context.Context) (int64, error) {
	pod, err := e.getPod(ctx)
	if err != nil {
		return 0, errors.Wrap(err, "environment/kubernetes: could not get pod")
	}
	if !isPodRunning(pod) {
		return 0, nil
	}
	if pod.Status.StartTime == nil {
		return 0, nil
	}
	return time.Since(pod.Status.StartTime.Time).Milliseconds(), nil
}

// withStatsTimeout invokes fn with a child context bounded by
// statsRequestTimeout so a single slow metrics/stats call cannot stall polling.
func withStatsTimeout[T any](ctx context.Context, fn func(context.Context) (T, error)) (T, error) {
	tctx, cancel := context.WithTimeout(ctx, statsRequestTimeout)
	defer cancel()
	return fn(tctx)
}

// pollResources periodically fetches resource usage and publishes resource
// events. It tries the Kubernetes Metrics API first (requires metrics-server)
// and falls back to the kubelet stats/summary API (always available).
func (e *Environment) pollResources(ctx context.Context) error {
	if e.st.Load() == environment.ProcessOfflineState {
		return errors.New("cannot enable resource polling on a stopped server")
	}

	e.log().Info("starting resource polling for pod")
	defer e.log().Debug("stopped resource polling for pod")

	uptime, err := e.Uptime(ctx)
	if err != nil {
		e.log().WithField("error", err).Warn("failed to calculate pod uptime")
	}

	ticker := time.NewTicker(5 * time.Second)
	defer ticker.Stop()

	lastCheck := time.Now()
	loggedSource := false

	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
			if e.st.Load() == environment.ProcessOfflineState {
				return nil
			}

			now := time.Now()
			uptime += now.Sub(lastCheck).Milliseconds()
			lastCheck = now

			st := environment.Stats{
				Uptime: uptime,
			}

			var stats *podStats

			// Prefer the metrics API and fall back to kubelet stats. The metrics
			// API is retried on every tick (the fallback is not sticky) so a
			// transiently-unavailable metrics-server is picked back up once it
			// recovers. Each call is bounded by statsRequestTimeout.
			usingKubelet := false
			stats, err = withStatsTimeout(ctx, e.getMetricsAPIStats)
			if err != nil {
				usingKubelet = true
				stats, err = withStatsTimeout(ctx, e.getKubeletPodStats)
			}

			if stats != nil {
				if !loggedSource {
					src := "metrics API (metrics-server)"
					if usingKubelet {
						src = "kubelet stats/summary"
					}
					e.log().WithField("source", src).Info("collecting pod resource metrics")
					loggedSource = true
				}
				st.Memory = uint64(stats.memoryBytes)
				st.CpuAbsolute = stats.cpuPercent
				st.Network = environment.NetworkStats{
					RxBytes: stats.rxBytes,
					TxBytes: stats.txBytes,
				}
			} else if !loggedSource {
				e.log().WithField("error", err).Warn("pod resource metrics unavailable from both metrics API and kubelet")
				loggedSource = true
			}

			// Get memory limit from pod spec.
			pod, err := withStatsTimeout(ctx, e.getPod)
			if err == nil && pod != nil {
				for _, c := range pod.Spec.Containers {
					if c.Name == "server" {
						if mem, ok := c.Resources.Limits["memory"]; ok {
							st.MemoryLimit = uint64(mem.Value())
						}
					}
				}
			}

			e.Events().Publish(environment.ResourceEvent, st)
		}
	}
}

// ---------------------------------------------------------------------------
// Source 1: Kubernetes Metrics API (requires metrics-server)
// ---------------------------------------------------------------------------

// getMetricsAPIStats fetches container metrics from the Kubernetes Metrics API
// (metrics.k8s.io/v1beta1). Returns an error if the API is unavailable.
func (e *Environment) getMetricsAPIStats(ctx context.Context) (*podStats, error) {
	result := e.client.CoreV1().RESTClient().Get().
		AbsPath("/apis/metrics.k8s.io/v1beta1").
		Resource("pods").
		Namespace(e.namespace()).
		Name(e.Id).
		Do(ctx)

	if result.Error() != nil {
		return nil, result.Error()
	}

	raw, err := result.Raw()
	if err != nil {
		return nil, err
	}

	return parseMetricsAPIPodStats(raw)
}

// podMetricsResponse represents the relevant fields of the Kubernetes Metrics
// API PodMetrics response (metrics.k8s.io/v1beta1).
type podMetricsResponse struct {
	Containers []containerMetricsEntry `json:"containers"`
}

// containerMetricsEntry represents a single container's usage in the Metrics
// API response.
type containerMetricsEntry struct {
	Name  string            `json:"name"`
	Usage map[string]string `json:"usage"`
}

// parseMetricsAPIPodStats extracts the "server" container's CPU/memory from a
// PodMetrics JSON response. Network stats are not available from this API.
func parseMetricsAPIPodStats(raw []byte) (*podStats, error) {
	var resp podMetricsResponse
	if err := json.Unmarshal(raw, &resp); err != nil {
		return nil, errors.Wrap(err, "environment/kubernetes: failed to parse pod metrics")
	}

	for _, c := range resp.Containers {
		if c.Name != "server" {
			continue
		}

		s := &podStats{}

		if memStr, ok := c.Usage["memory"]; ok {
			s.memoryBytes = parseResourceQuantity(memStr)
		}
		if cpuStr, ok := c.Usage["cpu"]; ok {
			s.cpuPercent = parseCPUToPercent(cpuStr)
		}

		return s, nil
	}

	return nil, nil
}

// ---------------------------------------------------------------------------
// Source 2: Kubelet stats/summary API (always available, no metrics-server)
// ---------------------------------------------------------------------------

// getKubeletPodStats fetches resource metrics from the kubelet stats/summary
// endpoint via the K8s API server proxy. This works on any cluster without
// requiring metrics-server.
func (e *Environment) getKubeletPodStats(ctx context.Context) (*podStats, error) {
	pod, err := e.getPod(ctx)
	if err != nil {
		return nil, errors.Wrap(err, "failed to get pod for kubelet stats")
	}
	if pod.Spec.NodeName == "" {
		return nil, errors.New("pod has no node assignment yet")
	}

	result := e.client.CoreV1().RESTClient().Get().
		AbsPath("/api/v1/nodes", pod.Spec.NodeName, "proxy", "stats", "summary").
		Do(ctx)

	if result.Error() != nil {
		return nil, errors.Wrap(result.Error(), "kubelet stats/summary request failed")
	}

	raw, err := result.Raw()
	if err != nil {
		return nil, errors.Wrap(err, "failed to read kubelet stats response")
	}

	return parseKubeletPodStats(raw, e.Id, e.namespace())
}

// kubelet stats/summary response types (only the fields we need).
type kubeletStatsSummary struct {
	Pods []kubeletPodStats `json:"pods"`
}

type kubeletPodStats struct {
	PodRef struct {
		Name      string `json:"name"`
		Namespace string `json:"namespace"`
	} `json:"podRef"`
	Containers []kubeletContainerStats `json:"containers"`
	Network    *kubeletNetworkStats    `json:"network,omitempty"`
}

type kubeletContainerStats struct {
	Name   string              `json:"name"`
	CPU    *kubeletCPUStats    `json:"cpu,omitempty"`
	Memory *kubeletMemoryStats `json:"memory,omitempty"`
}

type kubeletCPUStats struct {
	UsageNanoCores *uint64 `json:"usageNanoCores,omitempty"`
}

type kubeletMemoryStats struct {
	WorkingSetBytes *uint64 `json:"workingSetBytes,omitempty"`
}

type kubeletNetworkStats struct {
	RxBytes *uint64 `json:"rxBytes,omitempty"`
	TxBytes *uint64 `json:"txBytes,omitempty"`
}

// parseKubeletPodStats finds our pod in a kubelet stats/summary response and
// extracts CPU, memory, and network metrics for the "server" container.
func parseKubeletPodStats(raw []byte, podName, namespace string) (*podStats, error) {
	var summary kubeletStatsSummary
	if err := json.Unmarshal(raw, &summary); err != nil {
		return nil, errors.Wrap(err, "failed to parse kubelet stats summary")
	}

	for _, p := range summary.Pods {
		if p.PodRef.Name != podName || p.PodRef.Namespace != namespace {
			continue
		}

		s := &podStats{}

		for _, c := range p.Containers {
			if c.Name != "server" {
				continue
			}
			if c.CPU != nil && c.CPU.UsageNanoCores != nil {
				s.cpuPercent = float64(*c.CPU.UsageNanoCores) / 1e9 * 100
			}
			if c.Memory != nil && c.Memory.WorkingSetBytes != nil {
				s.memoryBytes = int64(*c.Memory.WorkingSetBytes)
			}
		}

		if p.Network != nil {
			if p.Network.RxBytes != nil {
				s.rxBytes = *p.Network.RxBytes
			}
			if p.Network.TxBytes != nil {
				s.txBytes = *p.Network.TxBytes
			}
		}

		return s, nil
	}

	return nil, fmt.Errorf("pod %s/%s not found in kubelet stats", namespace, podName)
}

// ---------------------------------------------------------------------------
// Parsing helpers
// ---------------------------------------------------------------------------

// parseResourceQuantity parses a Kubernetes resource quantity string for memory
// and returns the value in bytes. Supports suffixes: Ki, Mi, Gi, Ti, k, M, G, T,
// or plain bytes.
func parseResourceQuantity(s string) int64 {
	s = strings.TrimSpace(s)
	if s == "" {
		return 0
	}

	// Binary suffixes (Ki, Mi, Gi, Ti).
	binarySuffixes := map[string]int64{
		"Ki": 1024,
		"Mi": 1024 * 1024,
		"Gi": 1024 * 1024 * 1024,
		"Ti": 1024 * 1024 * 1024 * 1024,
	}
	for suffix, multiplier := range binarySuffixes {
		if strings.HasSuffix(s, suffix) {
			val, err := strconv.ParseInt(strings.TrimSuffix(s, suffix), 10, 64)
			if err != nil {
				return 0
			}
			return val * multiplier
		}
	}

	// Decimal suffixes (k, M, G, T).
	decimalSuffixes := map[string]int64{
		"T": 1000000000000,
		"G": 1000000000,
		"M": 1000000,
		"k": 1000,
	}
	for suffix, multiplier := range decimalSuffixes {
		if strings.HasSuffix(s, suffix) {
			val, err := strconv.ParseInt(strings.TrimSuffix(s, suffix), 10, 64)
			if err != nil {
				return 0
			}
			return val * multiplier
		}
	}

	// Plain bytes (no suffix).
	val, err := strconv.ParseInt(s, 10, 64)
	if err != nil {
		return 0
	}
	return val
}

// parseCPUToPercent converts a Kubernetes CPU quantity string to a percentage
// of a single core. Examples: "250m" → 25.0, "1" → 100.0, "0.5" → 50.0,
// "1500m" → 150.0, "100n" → 0.0001.
func parseCPUToPercent(s string) float64 {
	s = strings.TrimSpace(s)
	if s == "" {
		return 0
	}

	// Nanocores suffix "n" (e.g. "250000000n" = 0.25 cores).
	if strings.HasSuffix(s, "n") {
		val, err := strconv.ParseFloat(strings.TrimSuffix(s, "n"), 64)
		if err != nil {
			return 0
		}
		return (val / 1000000000) * 100
	}

	// Millicores suffix "m" (e.g. "250m" = 0.25 cores).
	if strings.HasSuffix(s, "m") {
		val, err := strconv.ParseFloat(strings.TrimSuffix(s, "m"), 64)
		if err != nil {
			return 0
		}
		return (val / 1000) * 100
	}

	// Whole/decimal cores (e.g. "1" or "0.5").
	val, err := strconv.ParseFloat(s, 64)
	if err != nil {
		return 0
	}
	return val * 100
}

// podLabelSelector returns a label selector for this server's Pod.
func (e *Environment) podLabelSelector() string {
	return "pelican.dev/server-id=" + e.Id
}
