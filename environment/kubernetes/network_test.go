package kubernetes

import (
	"context"
	"testing"

	. "github.com/franela/goblin"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"

	"github.com/pelican/wings/config"
	"github.com/pelican/wings/environment"
	"github.com/pelican/wings/events"
	"github.com/pelican/wings/system"
)

func newTestEnv(client *fake.Clientset, allocs environment.Allocations) *Environment {
	settings := environment.Settings{
		Allocations: allocs,
		Limits: environment.Limits{
			MemoryLimit: 512,
			CpuLimit:    100,
		},
	}
	cfg := environment.NewConfiguration(settings, []string{"FOO=bar"})

	return &Environment{
		Id:            "test-server-uuid",
		Configuration: cfg,
		meta:          &Metadata{Image: "nginx:latest"},
		client:        client,
		st:            system.NewAtomicString(environment.ProcessOfflineState),
		emitter:       events.NewBus(),
	}
}

func TestNetwork(t *testing.T) {
	g := Goblin(t)

	g.Describe("EnsureService", func() {
		g.It("should be a no-op in hostport mode", func() {
			client := fake.NewClientset()
			allocs := environment.Allocations{
				Mappings: map[string][]int{"0.0.0.0": {25565}},
			}
			env := newTestEnv(client, allocs)

			// Set hostport mode.
			config.Update(func(c *config.Configuration) {
				c.Kubernetes.NetworkMode = config.KubeNetworkHostPort
				c.Kubernetes.Namespace = "pelican"
			})

			err := env.EnsureService(context.Background())
			g.Assert(err).IsNil()

			// No Service should be created.
			svcs, _ := client.CoreV1().Services("pelican").List(context.Background(), metav1.ListOptions{})
			g.Assert(len(svcs.Items)).Equal(0)
		})

		g.It("should create a NodePort service in nodeport mode", func() {
			client := fake.NewClientset()
			allocs := environment.Allocations{
				Mappings: map[string][]int{"0.0.0.0": {25565, 25575}},
			}
			env := newTestEnv(client, allocs)

			config.Update(func(c *config.Configuration) {
				c.Kubernetes.NetworkMode = config.KubeNetworkNodePort
				c.Kubernetes.Namespace = "pelican"
			})

			err := env.EnsureService(context.Background())
			g.Assert(err).IsNil()

			svc, err := client.CoreV1().Services("pelican").Get(context.Background(), "gs-test-server-uuid", metav1.GetOptions{})
			g.Assert(err).IsNil()
			g.Assert(svc.Spec.Type).Equal(corev1.ServiceTypeNodePort)
			// Should have TCP+UDP for each port = 4 ports total.
			g.Assert(len(svc.Spec.Ports)).Equal(4)

			// Verify selector targets our Pod.
			g.Assert(svc.Spec.Selector["pelican.dev/server-id"]).Equal("test-server-uuid")

			// Verify labels.
			g.Assert(svc.Labels["pelican.dev/server-id"]).Equal("test-server-uuid")
			g.Assert(svc.Labels["pelican.dev/resource-type"]).Equal("service")
		})

		g.It("should update an existing service", func() {
			// Pre-create a service with one port.
			existingSvc := &corev1.Service{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "gs-test-server-uuid",
					Namespace: "pelican",
				},
				Spec: corev1.ServiceSpec{
					Type: corev1.ServiceTypeNodePort,
					Ports: []corev1.ServicePort{
						{
							Name:     "tcp-0-0-0-0-255",
							Protocol: corev1.ProtocolTCP,
							Port:     25565,
							NodePort: 31000,
						},
					},
					Selector: map[string]string{"pelican.dev/server-id": "test-server-uuid"},
				},
			}
			client := fake.NewClientset(existingSvc)
			allocs := environment.Allocations{
				Mappings: map[string][]int{"0.0.0.0": {25565, 25575}},
			}
			env := newTestEnv(client, allocs)

			config.Update(func(c *config.Configuration) {
				c.Kubernetes.NetworkMode = config.KubeNetworkNodePort
				c.Kubernetes.Namespace = "pelican"
			})

			err := env.EnsureService(context.Background())
			g.Assert(err).IsNil()

			svc, err := client.CoreV1().Services("pelican").Get(context.Background(), "gs-test-server-uuid", metav1.GetOptions{})
			g.Assert(err).IsNil()
			// Should now have 4 ports (2 for each allocation, TCP+UDP).
			g.Assert(len(svc.Spec.Ports)).Equal(4)
		})

		g.It("should reconcile Service type and annotations when network mode changes", func() {
			client := fake.NewClientset()
			allocs := environment.Allocations{
				Mappings: map[string][]int{"0.0.0.0": {25565}},
			}
			env := newTestEnv(client, allocs)

			// Apply in LoadBalancer mode so Wings owns the LB annotations.
			config.Update(func(c *config.Configuration) {
				c.Kubernetes.NetworkMode = config.KubeNetworkLoadBalancer
				c.Kubernetes.Namespace = "pelican"
				c.Kubernetes.LBAnnotations = map[string]string{"lbipam.cilium.io/ips": "10.0.0.1"}
			})
			err := env.EnsureService(context.Background())
			g.Assert(err).IsNil()

			// Switch to NodePort mode and apply again.
			config.Update(func(c *config.Configuration) {
				c.Kubernetes.NetworkMode = config.KubeNetworkNodePort
				c.Kubernetes.LBAnnotations = nil
			})
			err = env.EnsureService(context.Background())
			g.Assert(err).IsNil()

			svc, err := client.CoreV1().Services("pelican").Get(context.Background(), "gs-test-server-uuid", metav1.GetOptions{})
			g.Assert(err).IsNil()
			// Type must be updated to NodePort and the stale LB annotation cleared.
			g.Assert(svc.Spec.Type).Equal(corev1.ServiceTypeNodePort)
			_, hasStale := svc.Annotations["lbipam.cilium.io/ips"]
			g.Assert(hasStale).IsFalse()
		})

		g.It("should be a no-op with empty allocations", func() {
			client := fake.NewClientset()
			allocs := environment.Allocations{
				Mappings: map[string][]int{},
			}
			env := newTestEnv(client, allocs)

			config.Update(func(c *config.Configuration) {
				c.Kubernetes.NetworkMode = config.KubeNetworkNodePort
				c.Kubernetes.Namespace = "pelican"
			})

			err := env.EnsureService(context.Background())
			g.Assert(err).IsNil()

			svcs, _ := client.CoreV1().Services("pelican").List(context.Background(), metav1.ListOptions{})
			g.Assert(len(svcs.Items)).Equal(0)
		})

		g.It("should auto-set LB IP and sharing-key annotations from allocation IP", func() {
			client := fake.NewClientset()
			allocs := environment.Allocations{
				DefaultMapping: &environment.DefaultAllocationMapping{
					Ip:   "23.227.184.222",
					Port: 27015,
				},
				Mappings: map[string][]int{"23.227.184.222": {27015}},
			}
			env := newTestEnv(client, allocs)

			config.Update(func(c *config.Configuration) {
				c.Kubernetes.NetworkMode = config.KubeNetworkLoadBalancer
				c.Kubernetes.Namespace = "pelican"
				c.Kubernetes.LBAnnotations = map[string]string{
					"io.cilium/lb-ipam-pool": "game-servers",
				}
				c.Kubernetes.LBIPAnnotation = "lbipam.cilium.io/ips"
				c.Kubernetes.LBSharingKey = "lbipam.cilium.io/sharing-key"
			})

			err := env.EnsureService(context.Background())
			g.Assert(err).IsNil()

			svc, err := client.CoreV1().Services("pelican").Get(context.Background(), "gs-test-server-uuid", metav1.GetOptions{})
			g.Assert(err).IsNil()
			g.Assert(svc.Spec.Type).Equal(corev1.ServiceTypeLoadBalancer)
			g.Assert(svc.Annotations["io.cilium/lb-ipam-pool"]).Equal("game-servers")
			g.Assert(svc.Annotations["lbipam.cilium.io/ips"]).Equal("23.227.184.222")
			g.Assert(svc.Annotations["lbipam.cilium.io/sharing-key"]).Equal("23.227.184.222")
		})

		g.It("should not set IP annotations when allocation IP is 0.0.0.0", func() {
			client := fake.NewClientset()
			allocs := environment.Allocations{
				DefaultMapping: &environment.DefaultAllocationMapping{
					Ip:   "0.0.0.0",
					Port: 27015,
				},
				Mappings: map[string][]int{"0.0.0.0": {27015}},
			}
			env := newTestEnv(client, allocs)

			config.Update(func(c *config.Configuration) {
				c.Kubernetes.NetworkMode = config.KubeNetworkLoadBalancer
				c.Kubernetes.Namespace = "pelican"
				c.Kubernetes.LBIPAnnotation = "lbipam.cilium.io/ips"
				c.Kubernetes.LBSharingKey = "lbipam.cilium.io/sharing-key"
			})

			err := env.EnsureService(context.Background())
			g.Assert(err).IsNil()

			svc, err := client.CoreV1().Services("pelican").Get(context.Background(), "gs-test-server-uuid", metav1.GetOptions{})
			g.Assert(err).IsNil()
			_, hasIP := svc.Annotations["lbipam.cilium.io/ips"]
			g.Assert(hasIP).IsFalse()
			_, hasKey := svc.Annotations["lbipam.cilium.io/sharing-key"]
			g.Assert(hasKey).IsFalse()
		})

		g.It("should not set IP annotations when config keys are empty", func() {
			client := fake.NewClientset()
			allocs := environment.Allocations{
				DefaultMapping: &environment.DefaultAllocationMapping{
					Ip:   "23.227.184.222",
					Port: 27015,
				},
				Mappings: map[string][]int{"23.227.184.222": {27015}},
			}
			env := newTestEnv(client, allocs)

			config.Update(func(c *config.Configuration) {
				c.Kubernetes.NetworkMode = config.KubeNetworkLoadBalancer
				c.Kubernetes.Namespace = "pelican"
				c.Kubernetes.LBIPAnnotation = ""
				c.Kubernetes.LBSharingKey = ""
			})

			err := env.EnsureService(context.Background())
			g.Assert(err).IsNil()

			svc, err := client.CoreV1().Services("pelican").Get(context.Background(), "gs-test-server-uuid", metav1.GetOptions{})
			g.Assert(err).IsNil()
			_, hasIP := svc.Annotations["lbipam.cilium.io/ips"]
			g.Assert(hasIP).IsFalse()
		})

		g.It("should remove stale IP annotations when allocation IP becomes invalid", func() {
			client := fake.NewClientset()

			config.Update(func(c *config.Configuration) {
				c.Kubernetes.NetworkMode = config.KubeNetworkLoadBalancer
				c.Kubernetes.Namespace = "pelican"
				c.Kubernetes.LBAnnotations = map[string]string{
					"io.cilium/lb-ipam-pool": "game-servers",
				}
				c.Kubernetes.LBIPAnnotation = "lbipam.cilium.io/ips"
				c.Kubernetes.LBSharingKey = "lbipam.cilium.io/sharing-key"
			})

			// Apply with a valid allocation IP so Wings owns the IP-pinning
			// annotations.
			withIP := newTestEnv(client, environment.Allocations{
				DefaultMapping: &environment.DefaultAllocationMapping{
					Ip:   "23.227.184.222",
					Port: 27015,
				},
				Mappings: map[string][]int{"23.227.184.222": {27015}},
			})
			err := withIP.EnsureService(context.Background())
			g.Assert(err).IsNil()

			svc, err := client.CoreV1().Services("pelican").Get(context.Background(), "gs-test-server-uuid", metav1.GetOptions{})
			g.Assert(err).IsNil()
			g.Assert(svc.Annotations["lbipam.cilium.io/ips"]).Equal("23.227.184.222")

			// Apply again with an unusable allocation IP.
			withoutIP := newTestEnv(client, environment.Allocations{
				DefaultMapping: &environment.DefaultAllocationMapping{
					Ip:   "0.0.0.0",
					Port: 27015,
				},
				Mappings: map[string][]int{"0.0.0.0": {27015}},
			})
			err = withoutIP.EnsureService(context.Background())
			g.Assert(err).IsNil()

			svc, err = client.CoreV1().Services("pelican").Get(context.Background(), "gs-test-server-uuid", metav1.GetOptions{})
			g.Assert(err).IsNil()
			// Pool annotation should remain.
			g.Assert(svc.Annotations["io.cilium/lb-ipam-pool"]).Equal("game-servers")
			// IP-pinning annotations should be removed.
			_, hasIP := svc.Annotations["lbipam.cilium.io/ips"]
			g.Assert(hasIP).IsFalse()
			_, hasKey := svc.Annotations["lbipam.cilium.io/sharing-key"]
			g.Assert(hasKey).IsFalse()
		})
	})

	g.Describe("DeleteService", func() {
		g.It("should delete an existing service", func() {
			existingSvc := &corev1.Service{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "gs-test-server-uuid",
					Namespace: "pelican",
				},
				Spec: corev1.ServiceSpec{
					Type: corev1.ServiceTypeNodePort,
				},
			}
			client := fake.NewClientset(existingSvc)
			allocs := environment.Allocations{}
			env := newTestEnv(client, allocs)

			config.Update(func(c *config.Configuration) {
				c.Kubernetes.NetworkMode = config.KubeNetworkNodePort
				c.Kubernetes.Namespace = "pelican"
			})

			err := env.DeleteService(context.Background())
			g.Assert(err).IsNil()

			svcs, _ := client.CoreV1().Services("pelican").List(context.Background(), metav1.ListOptions{})
			g.Assert(len(svcs.Items)).Equal(0)
		})

		g.It("should not error when service does not exist", func() {
			client := fake.NewClientset()
			allocs := environment.Allocations{}
			env := newTestEnv(client, allocs)

			config.Update(func(c *config.Configuration) {
				c.Kubernetes.NetworkMode = config.KubeNetworkNodePort
				c.Kubernetes.Namespace = "pelican"
			})

			err := env.DeleteService(context.Background())
			g.Assert(err).IsNil()
		})

		g.It("should be a no-op in hostport mode", func() {
			existingSvc := &corev1.Service{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "gs-test-server-uuid",
					Namespace: "pelican",
				},
			}
			client := fake.NewClientset(existingSvc)
			allocs := environment.Allocations{}
			env := newTestEnv(client, allocs)

			config.Update(func(c *config.Configuration) {
				c.Kubernetes.NetworkMode = config.KubeNetworkHostPort
				c.Kubernetes.Namespace = "pelican"
			})

			err := env.DeleteService(context.Background())
			g.Assert(err).IsNil()

			// Service should still exist since delete is no-op in hostport mode.
			svcs, _ := client.CoreV1().Services("pelican").List(context.Background(), metav1.ListOptions{})
			g.Assert(len(svcs.Items)).Equal(1)
		})
	})

	g.Describe("GetServiceNodePorts", func() {
		g.It("should return assigned NodePorts", func() {
			existingSvc := &corev1.Service{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "gs-test-server-uuid",
					Namespace: "pelican",
				},
				Spec: corev1.ServiceSpec{
					Type: corev1.ServiceTypeNodePort,
					Ports: []corev1.ServicePort{
						{Name: "tcp-25565", Port: 25565, NodePort: 31234, Protocol: corev1.ProtocolTCP},
						{Name: "udp-25575", Port: 25575, NodePort: 31235, Protocol: corev1.ProtocolUDP},
					},
				},
			}
			client := fake.NewClientset(existingSvc)
			allocs := environment.Allocations{}
			env := newTestEnv(client, allocs)

			config.Update(func(c *config.Configuration) {
				c.Kubernetes.NetworkMode = config.KubeNetworkNodePort
				c.Kubernetes.Namespace = "pelican"
			})

			ports, err := env.GetServiceNodePorts(context.Background())
			g.Assert(err).IsNil()
			g.Assert(ports[25565]).Equal(int32(31234))
			g.Assert(ports[25575]).Equal(int32(31235))
		})

		g.It("should return nil in hostport mode", func() {
			client := fake.NewClientset()
			allocs := environment.Allocations{}
			env := newTestEnv(client, allocs)

			config.Update(func(c *config.Configuration) {
				c.Kubernetes.NetworkMode = config.KubeNetworkHostPort
			})

			ports, err := env.GetServiceNodePorts(context.Background())
			g.Assert(err).IsNil()
			g.Assert(ports == nil).IsTrue()
		})
	})

	g.Describe("sanitizePortName", func() {
		g.It("should lowercase and replace dots with hyphens", func() {
			g.Assert(sanitizePortName("TCP-192.168.1.1-25565")).Equal("tcp-192-168-1-1")
		})

		g.It("should truncate to 15 characters", func() {
			result := sanitizePortName("very-long-port-name-that-exceeds")
			g.Assert(len(result) <= 15).IsTrue()
		})

		g.It("should not start or end with a hyphen", func() {
			result := sanitizePortName("-invalid-name-")
			g.Assert(result[0] != '-').IsTrue()
			g.Assert(result[len(result)-1] != '-').IsTrue()
		})

		g.It("should return 'port' for empty input", func() {
			g.Assert(sanitizePortName("")).Equal("port")
		})
	})

	g.Describe("portName", func() {
		g.It("should produce unique names for different ports", func() {
			g.Assert(portName("tcp", 27015)).Equal("tcp-27015")
			g.Assert(portName("tcp", 27016)).Equal("tcp-27016")
			g.Assert(portName("udp", 27015)).Equal("udp-27015")
		})

		g.It("should fit within 15-char limit for 5-digit ports", func() {
			name := portName("tcp", 65535)
			g.Assert(len(name) <= 15).IsTrue()
			g.Assert(name).Equal("tcp-65535")
		})
	})

}
