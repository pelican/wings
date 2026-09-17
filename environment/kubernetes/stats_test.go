package kubernetes

import (
	"encoding/json"
	"testing"

	. "github.com/franela/goblin"
)

func mustMarshal(v interface{}) []byte {
	b, err := json.Marshal(v)
	if err != nil {
		panic(err)
	}
	return b
}

func TestStats(t *testing.T) {
	g := Goblin(t)

	g.Describe("parseMetricsAPIPodStats", func() {
		g.It("should parse a valid PodMetrics response", func() {
			raw := []byte(`{
				"kind": "PodMetrics",
				"metadata": {"name": "test-pod", "namespace": "pelican"},
				"containers": [
					{
						"name": "server",
						"usage": {
							"cpu": "250m",
							"memory": "134217728"
						}
					}
				]
			}`)

			metrics, err := parseMetricsAPIPodStats(raw)
			g.Assert(err).IsNil()
			g.Assert(metrics).IsNotNil()
			g.Assert(metrics.memoryBytes).Equal(int64(134217728))
			g.Assert(metrics.cpuPercent).Equal(25.0)
		})

		g.It("should handle nanocores CPU format", func() {
			raw := []byte(`{
				"containers": [
					{
						"name": "server",
						"usage": {
							"cpu": "500000000n",
							"memory": "67108864"
						}
					}
				]
			}`)

			metrics, err := parseMetricsAPIPodStats(raw)
			g.Assert(err).IsNil()
			g.Assert(metrics.cpuPercent).Equal(50.0)
		})

		g.It("should handle whole core CPU format", func() {
			raw := []byte(`{
				"containers": [
					{
						"name": "server",
						"usage": {
							"cpu": "2",
							"memory": "0"
						}
					}
				]
			}`)

			metrics, err := parseMetricsAPIPodStats(raw)
			g.Assert(err).IsNil()
			g.Assert(metrics.cpuPercent).Equal(200.0)
		})

		g.It("should handle memory with Ki suffix", func() {
			raw := []byte(`{
				"containers": [
					{
						"name": "server",
						"usage": {
							"cpu": "0",
							"memory": "131072Ki"
						}
					}
				]
			}`)

			metrics, err := parseMetricsAPIPodStats(raw)
			g.Assert(err).IsNil()
			g.Assert(metrics.memoryBytes).Equal(int64(131072 * 1024))
		})

		g.It("should handle memory with Mi suffix", func() {
			raw := []byte(`{
				"containers": [
					{
						"name": "server",
						"usage": {
							"cpu": "100m",
							"memory": "256Mi"
						}
					}
				]
			}`)

			metrics, err := parseMetricsAPIPodStats(raw)
			g.Assert(err).IsNil()
			g.Assert(metrics.memoryBytes).Equal(int64(256 * 1024 * 1024))
		})

		g.It("should return nil when server container is not found", func() {
			raw := []byte(`{
				"containers": [
					{
						"name": "sidecar",
						"usage": {
							"cpu": "50m",
							"memory": "32Mi"
						}
					}
				]
			}`)

			metrics, err := parseMetricsAPIPodStats(raw)
			g.Assert(err).IsNil()
			g.Assert(metrics == nil).IsTrue()
		})

		g.It("should return nil for empty containers list", func() {
			raw := []byte(`{"containers": []}`)
			metrics, err := parseMetricsAPIPodStats(raw)
			g.Assert(err).IsNil()
			g.Assert(metrics == nil).IsTrue()
		})

		g.It("should return error for invalid JSON", func() {
			raw := []byte(`not json at all`)
			_, err := parseMetricsAPIPodStats(raw)
			g.Assert(err).IsNotNil()
		})

		g.It("should handle multi-container pod and find server", func() {
			raw := []byte(`{
				"containers": [
					{
						"name": "init-container",
						"usage": {"cpu": "10m", "memory": "8Mi"}
					},
					{
						"name": "server",
						"usage": {"cpu": "750m", "memory": "512Mi"}
					},
					{
						"name": "sidecar-proxy",
						"usage": {"cpu": "20m", "memory": "16Mi"}
					}
				]
			}`)

			metrics, err := parseMetricsAPIPodStats(raw)
			g.Assert(err).IsNil()
			g.Assert(metrics).IsNotNil()
			g.Assert(metrics.cpuPercent).Equal(75.0)
			g.Assert(metrics.memoryBytes).Equal(int64(512 * 1024 * 1024))
		})
	})

	g.Describe("parseKubeletPodStats", func() {
		g.It("should parse kubelet stats for the target pod", func() {
			cpuNano := uint64(500000000)
			memBytes := uint64(268435456)
			rxBytes := uint64(1024)
			txBytes := uint64(2048)
			raw := mustMarshal(kubeletStatsSummary{
				Pods: []kubeletPodStats{
					{
						PodRef: struct {
							Name      string `json:"name"`
							Namespace string `json:"namespace"`
						}{Name: "other-pod", Namespace: "pelican"},
						Containers: []kubeletContainerStats{
							{Name: "server", CPU: &kubeletCPUStats{UsageNanoCores: &cpuNano}, Memory: &kubeletMemoryStats{WorkingSetBytes: &memBytes}},
						},
					},
					{
						PodRef: struct {
							Name      string `json:"name"`
							Namespace string `json:"namespace"`
						}{Name: "target-pod", Namespace: "pelican"},
						Containers: []kubeletContainerStats{
							{Name: "server", CPU: &kubeletCPUStats{UsageNanoCores: &cpuNano}, Memory: &kubeletMemoryStats{WorkingSetBytes: &memBytes}},
						},
						Network: &kubeletNetworkStats{RxBytes: &rxBytes, TxBytes: &txBytes},
					},
				},
			})

			stats, err := parseKubeletPodStats(raw, "target-pod", "pelican")
			g.Assert(err).IsNil()
			g.Assert(stats).IsNotNil()
			g.Assert(stats.cpuPercent).Equal(50.0)
			g.Assert(stats.memoryBytes).Equal(int64(268435456))
			g.Assert(stats.rxBytes).Equal(uint64(1024))
			g.Assert(stats.txBytes).Equal(uint64(2048))
		})

		g.It("should return error when pod not found", func() {
			raw := mustMarshal(kubeletStatsSummary{Pods: []kubeletPodStats{}})
			_, err := parseKubeletPodStats(raw, "missing", "pelican")
			g.Assert(err).IsNotNil()
		})

		g.It("should handle missing network stats", func() {
			cpuNano := uint64(100000000)
			memBytes := uint64(1024)
			raw := mustMarshal(kubeletStatsSummary{
				Pods: []kubeletPodStats{
					{
						PodRef: struct {
							Name      string `json:"name"`
							Namespace string `json:"namespace"`
						}{Name: "test", Namespace: "ns"},
						Containers: []kubeletContainerStats{
							{Name: "server", CPU: &kubeletCPUStats{UsageNanoCores: &cpuNano}, Memory: &kubeletMemoryStats{WorkingSetBytes: &memBytes}},
						},
					},
				},
			})

			stats, err := parseKubeletPodStats(raw, "test", "ns")
			g.Assert(err).IsNil()
			g.Assert(stats.rxBytes).Equal(uint64(0))
			g.Assert(stats.txBytes).Equal(uint64(0))
		})

		g.It("should handle nil CPU and memory pointers", func() {
			raw := mustMarshal(kubeletStatsSummary{
				Pods: []kubeletPodStats{
					{
						PodRef: struct {
							Name      string `json:"name"`
							Namespace string `json:"namespace"`
						}{Name: "test", Namespace: "ns"},
						Containers: []kubeletContainerStats{
							{Name: "server"},
						},
					},
				},
			})

			stats, err := parseKubeletPodStats(raw, "test", "ns")
			g.Assert(err).IsNil()
			g.Assert(stats.cpuPercent).Equal(0.0)
			g.Assert(stats.memoryBytes).Equal(int64(0))
		})
	})

	g.Describe("parseResourceQuantity", func() {
		g.It("should parse plain bytes", func() {
			g.Assert(parseResourceQuantity("134217728")).Equal(int64(134217728))
		})

		g.It("should parse Ki suffix", func() {
			g.Assert(parseResourceQuantity("128Ki")).Equal(int64(128 * 1024))
		})

		g.It("should parse Mi suffix", func() {
			g.Assert(parseResourceQuantity("512Mi")).Equal(int64(512 * 1024 * 1024))
		})

		g.It("should parse Gi suffix", func() {
			g.Assert(parseResourceQuantity("2Gi")).Equal(int64(2 * 1024 * 1024 * 1024))
		})

		g.It("should parse decimal k suffix", func() {
			g.Assert(parseResourceQuantity("500k")).Equal(int64(500000))
		})

		g.It("should parse decimal M suffix", func() {
			g.Assert(parseResourceQuantity("100M")).Equal(int64(100000000))
		})

		g.It("should return 0 for empty string", func() {
			g.Assert(parseResourceQuantity("")).Equal(int64(0))
		})

		g.It("should return 0 for invalid input", func() {
			g.Assert(parseResourceQuantity("not-a-number")).Equal(int64(0))
		})
	})

	g.Describe("parseCPUToPercent", func() {
		g.It("should parse millicores", func() {
			g.Assert(parseCPUToPercent("250m")).Equal(25.0)
			g.Assert(parseCPUToPercent("1000m")).Equal(100.0)
			g.Assert(parseCPUToPercent("1500m")).Equal(150.0)
		})

		g.It("should parse nanocores", func() {
			g.Assert(parseCPUToPercent("250000000n")).Equal(25.0)
			g.Assert(parseCPUToPercent("1000000000n")).Equal(100.0)
		})

		g.It("should parse whole cores", func() {
			g.Assert(parseCPUToPercent("1")).Equal(100.0)
			g.Assert(parseCPUToPercent("2")).Equal(200.0)
		})

		g.It("should parse decimal cores", func() {
			g.Assert(parseCPUToPercent("0.5")).Equal(50.0)
			g.Assert(parseCPUToPercent("0.25")).Equal(25.0)
		})

		g.It("should return 0 for empty string", func() {
			g.Assert(parseCPUToPercent("")).Equal(0.0)
		})

		g.It("should return 0 for invalid input", func() {
			g.Assert(parseCPUToPercent("invalid")).Equal(0.0)
		})
	})
}
