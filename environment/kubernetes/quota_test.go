package kubernetes

import (
	"context"
	"errors"
	"testing"

	. "github.com/franela/goblin"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/kubernetes/fake"
	k8stesting "k8s.io/client-go/testing"

	"github.com/pelican/wings/config"
	"github.com/pelican/wings/environment"
	"github.com/pelican/wings/system"
)

// TestQuota covers building and reconciling ResourceQuota and LimitRange
// objects from configuration, including invalid-quantity error handling.
func TestQuota(t *testing.T) {
	g := Goblin(t)

	g.Describe("ResourceQuota", func() {
		g.Describe("EnsureResourceQuota", func() {
			g.It("should be a no-op when disabled", func() {
				client := fake.NewClientset()
				env := &Environment{
					Id:     "quota-noop-uuid",
					client: client,
					st:     system.NewAtomicString(environment.ProcessOfflineState),
				}
				config.Update(func(c *config.Configuration) {
					c.Kubernetes.Namespace = "pelican"
					c.Kubernetes.ResourceQuota.Enabled = false
				})

				err := env.EnsureResourceQuota(context.Background())
				g.Assert(err).IsNil()

				quotas, _ := client.CoreV1().ResourceQuotas("pelican").List(context.Background(), metav1.ListOptions{})
				g.Assert(len(quotas.Items)).Equal(0)
			})

			g.It("should create a ResourceQuota with CPU and memory limits", func() {
				client := fake.NewClientset()
				env := &Environment{
					Id:     "quota-create-uuid",
					client: client,
					st:     system.NewAtomicString(environment.ProcessOfflineState),
				}
				config.Update(func(c *config.Configuration) {
					c.Kubernetes.Namespace = "pelican"
					c.Kubernetes.ResourceQuota.Enabled = true
					c.Kubernetes.ResourceQuota.CPULimit = "16"
					c.Kubernetes.ResourceQuota.MemoryLimit = "32Gi"
					c.Kubernetes.ResourceQuota.CPURequest = "8"
					c.Kubernetes.ResourceQuota.MemoryRequest = "16Gi"
					c.Kubernetes.ResourceQuota.MaxPods = 20
				})

				err := env.EnsureResourceQuota(context.Background())
				g.Assert(err).IsNil()

				rq, err := client.CoreV1().ResourceQuotas("pelican").Get(context.Background(), "pelican-wings", metav1.GetOptions{})
				g.Assert(err).IsNil()
				g.Assert(rq.Name).Equal("pelican-wings")
				g.Assert(rq.Labels["app.kubernetes.io/managed-by"]).Equal("pelican-wings")

				cpuLimit := rq.Spec.Hard[corev1.ResourceLimitsCPU]
				g.Assert(cpuLimit.Cmp(resource.MustParse("16"))).Equal(0)

				memLimit := rq.Spec.Hard[corev1.ResourceLimitsMemory]
				g.Assert(memLimit.Cmp(resource.MustParse("32Gi"))).Equal(0)

				pods := rq.Spec.Hard[corev1.ResourcePods]
				g.Assert(pods.Value()).Equal(int64(20))
			})

			g.It("should create ResourceQuota with storage limits", func() {
				client := fake.NewClientset()
				env := &Environment{
					Id:     "quota-storage-uuid",
					client: client,
					st:     system.NewAtomicString(environment.ProcessOfflineState),
				}
				config.Update(func(c *config.Configuration) {
					c.Kubernetes.Namespace = "pelican"
					c.Kubernetes.ResourceQuota.Enabled = true
					c.Kubernetes.ResourceQuota.MaxPVCs = 50
					c.Kubernetes.ResourceQuota.MaxStorage = "500Gi"
					c.Kubernetes.ResourceQuota.CPULimit = ""
					c.Kubernetes.ResourceQuota.MemoryLimit = ""
					c.Kubernetes.ResourceQuota.CPURequest = ""
					c.Kubernetes.ResourceQuota.MemoryRequest = ""
					c.Kubernetes.ResourceQuota.MaxPods = 0
				})

				err := env.EnsureResourceQuota(context.Background())
				g.Assert(err).IsNil()

				rq, err := client.CoreV1().ResourceQuotas("pelican").Get(context.Background(), "pelican-wings", metav1.GetOptions{})
				g.Assert(err).IsNil()

				pvcs := rq.Spec.Hard[corev1.ResourcePersistentVolumeClaims]
				g.Assert(pvcs.Value()).Equal(int64(50))

				storage := rq.Spec.Hard[corev1.ResourceRequestsStorage]
				g.Assert(storage.Cmp(resource.MustParse("500Gi"))).Equal(0)
			})

			g.It("should update an existing ResourceQuota", func() {
				existingRQ := &corev1.ResourceQuota{
					ObjectMeta: metav1.ObjectMeta{
						Name:      "pelican-wings",
						Namespace: "pelican",
					},
					Spec: corev1.ResourceQuotaSpec{
						Hard: corev1.ResourceList{
							corev1.ResourceLimitsCPU: resource.MustParse("8"),
						},
					},
				}
				client := fake.NewClientset(existingRQ)
				env := &Environment{
					Id:     "quota-update-uuid",
					client: client,
					st:     system.NewAtomicString(environment.ProcessOfflineState),
				}
				config.Update(func(c *config.Configuration) {
					c.Kubernetes.Namespace = "pelican"
					c.Kubernetes.ResourceQuota.Enabled = true
					c.Kubernetes.ResourceQuota.CPULimit = "32"
					c.Kubernetes.ResourceQuota.MemoryLimit = "64Gi"
					c.Kubernetes.ResourceQuota.CPURequest = ""
					c.Kubernetes.ResourceQuota.MemoryRequest = ""
					c.Kubernetes.ResourceQuota.MaxPods = 0
					c.Kubernetes.ResourceQuota.MaxPVCs = 0
					c.Kubernetes.ResourceQuota.MaxStorage = ""
				})

				err := env.EnsureResourceQuota(context.Background())
				g.Assert(err).IsNil()

				rq, _ := client.CoreV1().ResourceQuotas("pelican").Get(context.Background(), "pelican-wings", metav1.GetOptions{})
				cpuLimit := rq.Spec.Hard[corev1.ResourceLimitsCPU]
				g.Assert(cpuLimit.Cmp(resource.MustParse("32"))).Equal(0)

				memLimit := rq.Spec.Hard[corev1.ResourceLimitsMemory]
				g.Assert(memLimit.Cmp(resource.MustParse("64Gi"))).Equal(0)
			})

			g.It("should propagate an Apply error", func() {
				client := fake.NewClientset()
				client.PrependReactor("patch", "resourcequotas", func(action k8stesting.Action) (bool, runtime.Object, error) {
					return true, nil, errors.New("boom: api server unavailable")
				})
				env := &Environment{
					Id:     "quota-geterr-uuid",
					client: client,
					st:     system.NewAtomicString(environment.ProcessOfflineState),
				}
				config.Update(func(c *config.Configuration) {
					c.Kubernetes.Namespace = "pelican"
					c.Kubernetes.ResourceQuota.Enabled = true
					c.Kubernetes.ResourceQuota.CPULimit = "16"
				})

				err := env.EnsureResourceQuota(context.Background())
				g.Assert(err != nil).IsTrue()
			})
		})

		g.Describe("EnsureLimitRange", func() {
			g.It("should be a no-op when disabled", func() {
				client := fake.NewClientset()
				env := &Environment{
					Id:     "lr-noop-uuid",
					client: client,
					st:     system.NewAtomicString(environment.ProcessOfflineState),
				}
				config.Update(func(c *config.Configuration) {
					c.Kubernetes.Namespace = "pelican"
					c.Kubernetes.LimitRange.Enabled = false
				})

				err := env.EnsureLimitRange(context.Background())
				g.Assert(err).IsNil()

				lrs, _ := client.CoreV1().LimitRanges("pelican").List(context.Background(), metav1.ListOptions{})
				g.Assert(len(lrs.Items)).Equal(0)
			})

			g.It("should create a LimitRange with defaults and max", func() {
				client := fake.NewClientset()
				env := &Environment{
					Id:     "lr-create-uuid",
					client: client,
					st:     system.NewAtomicString(environment.ProcessOfflineState),
				}
				config.Update(func(c *config.Configuration) {
					c.Kubernetes.Namespace = "pelican"
					c.Kubernetes.LimitRange.Enabled = true
					c.Kubernetes.LimitRange.DefaultCPULimit = "2"
					c.Kubernetes.LimitRange.DefaultMemoryLimit = "4Gi"
					c.Kubernetes.LimitRange.DefaultCPURequest = "500m"
					c.Kubernetes.LimitRange.DefaultMemoryRequest = "1Gi"
					c.Kubernetes.LimitRange.MaxCPU = "8"
					c.Kubernetes.LimitRange.MaxMemory = "16Gi"
				})

				err := env.EnsureLimitRange(context.Background())
				g.Assert(err).IsNil()

				lr, err := client.CoreV1().LimitRanges("pelican").Get(context.Background(), "pelican-wings", metav1.GetOptions{})
				g.Assert(err).IsNil()
				g.Assert(lr.Name).Equal("pelican-wings")
				g.Assert(len(lr.Spec.Limits)).Equal(1)

				item := lr.Spec.Limits[0]
				g.Assert(item.Type).Equal(corev1.LimitTypeContainer)

				defaultCPU := item.Default[corev1.ResourceCPU]
				g.Assert(defaultCPU.Cmp(resource.MustParse("2"))).Equal(0)

				defaultMem := item.Default[corev1.ResourceMemory]
				g.Assert(defaultMem.Cmp(resource.MustParse("4Gi"))).Equal(0)

				reqCPU := item.DefaultRequest[corev1.ResourceCPU]
				g.Assert(reqCPU.Cmp(resource.MustParse("500m"))).Equal(0)

				maxCPU := item.Max[corev1.ResourceCPU]
				g.Assert(maxCPU.Cmp(resource.MustParse("8"))).Equal(0)

				maxMem := item.Max[corev1.ResourceMemory]
				g.Assert(maxMem.Cmp(resource.MustParse("16Gi"))).Equal(0)
			})

			g.It("should update an existing LimitRange", func() {
				existingLR := &corev1.LimitRange{
					ObjectMeta: metav1.ObjectMeta{
						Name:      "pelican-wings",
						Namespace: "pelican",
					},
					Spec: corev1.LimitRangeSpec{
						Limits: []corev1.LimitRangeItem{
							{
								Type: corev1.LimitTypeContainer,
								Default: corev1.ResourceList{
									corev1.ResourceCPU: resource.MustParse("1"),
								},
							},
						},
					},
				}
				client := fake.NewClientset(existingLR)
				env := &Environment{
					Id:     "lr-update-uuid",
					client: client,
					st:     system.NewAtomicString(environment.ProcessOfflineState),
				}
				config.Update(func(c *config.Configuration) {
					c.Kubernetes.Namespace = "pelican"
					c.Kubernetes.LimitRange.Enabled = true
					c.Kubernetes.LimitRange.DefaultCPULimit = "4"
					c.Kubernetes.LimitRange.DefaultMemoryLimit = "8Gi"
					c.Kubernetes.LimitRange.DefaultCPURequest = ""
					c.Kubernetes.LimitRange.DefaultMemoryRequest = ""
					c.Kubernetes.LimitRange.MaxCPU = ""
					c.Kubernetes.LimitRange.MaxMemory = ""
				})

				err := env.EnsureLimitRange(context.Background())
				g.Assert(err).IsNil()

				lr, _ := client.CoreV1().LimitRanges("pelican").Get(context.Background(), "pelican-wings", metav1.GetOptions{})
				defaultCPU := lr.Spec.Limits[0].Default[corev1.ResourceCPU]
				g.Assert(defaultCPU.Cmp(resource.MustParse("4"))).Equal(0)
			})

			g.It("should propagate an Apply error", func() {
				client := fake.NewClientset()
				client.PrependReactor("patch", "limitranges", func(action k8stesting.Action) (bool, runtime.Object, error) {
					return true, nil, errors.New("boom: api server unavailable")
				})
				env := &Environment{
					Id:     "lr-geterr-uuid",
					client: client,
					st:     system.NewAtomicString(environment.ProcessOfflineState),
				}
				config.Update(func(c *config.Configuration) {
					c.Kubernetes.Namespace = "pelican"
					c.Kubernetes.LimitRange.Enabled = true
					c.Kubernetes.LimitRange.DefaultCPULimit = "2"
				})

				err := env.EnsureLimitRange(context.Background())
				g.Assert(err != nil).IsTrue()
			})
		})

		g.Describe("buildResourceQuota", func() {
			g.It("should only include non-empty fields", func() {
				cfg := &config.KubeResourceQuota{
					CPULimit:    "4",
					MemoryLimit: "",
					MaxPods:     10,
					MaxPVCs:     0,
				}
				rq, err := buildResourceQuota("test-ns", cfg)
				g.Assert(err).IsNil()
				g.Assert(rq.Namespace).Equal("test-ns")

				_, hasCPU := rq.Spec.Hard[corev1.ResourceLimitsCPU]
				g.Assert(hasCPU).IsTrue()

				_, hasMem := rq.Spec.Hard[corev1.ResourceLimitsMemory]
				g.Assert(hasMem).IsFalse()

				_, hasPods := rq.Spec.Hard[corev1.ResourcePods]
				g.Assert(hasPods).IsTrue()

				_, hasPVCs := rq.Spec.Hard[corev1.ResourcePersistentVolumeClaims]
				g.Assert(hasPVCs).IsFalse()
			})
		})

		g.Describe("buildLimitRange", func() {
			g.It("should only include non-empty fields", func() {
				cfg := &config.KubeLimitRange{
					DefaultCPULimit:    "2",
					DefaultMemoryLimit: "",
					MaxCPU:             "8",
					MaxMemory:          "",
				}
				lr, err := buildLimitRange("test-ns", cfg)
				g.Assert(err).IsNil()
				g.Assert(lr.Namespace).Equal("test-ns")
				g.Assert(len(lr.Spec.Limits)).Equal(1)

				item := lr.Spec.Limits[0]
				_, hasCPU := item.Default[corev1.ResourceCPU]
				g.Assert(hasCPU).IsTrue()

				_, hasMem := item.Default[corev1.ResourceMemory]
				g.Assert(hasMem).IsFalse()

				_, hasMaxCPU := item.Max[corev1.ResourceCPU]
				g.Assert(hasMaxCPU).IsTrue()

				_, hasMaxMem := item.Max[corev1.ResourceMemory]
				g.Assert(hasMaxMem).IsFalse()
			})

			g.It("should return an error for an invalid quantity instead of panicking", func() {
				lr, err := buildLimitRange("test-ns", &config.KubeLimitRange{DefaultCPULimit: "not-a-quantity"})
				g.Assert(err != nil).IsTrue()
				g.Assert(lr == nil).IsTrue()
			})
		})

		g.Describe("buildResourceQuota invalid input", func() {
			g.It("should return an error for an invalid quantity instead of panicking", func() {
				rq, err := buildResourceQuota("test-ns", &config.KubeResourceQuota{CPULimit: "not-a-quantity"})
				g.Assert(err != nil).IsTrue()
				g.Assert(rq == nil).IsTrue()
			})
		})
	})
}
