package kubernetes

import (
	"context"
	"os"
	"strings"
	"testing"

	. "github.com/franela/goblin"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"

	"github.com/pelican/wings/config"
	"github.com/pelican/wings/environment"
	"github.com/pelican/wings/events"
	"github.com/pelican/wings/system"
)

func TestEnvironment(t *testing.T) {
	g := Goblin(t)

	g.Describe("Environment", func() {
		g.Describe("Type", func() {
			g.It("should return 'kubernetes'", func() {
				env := &Environment{st: system.NewAtomicString(environment.ProcessOfflineState)}
				g.Assert(env.Type()).Equal("kubernetes")
			})
		})

		g.Describe("State", func() {
			g.It("should return the current state", func() {
				env := &Environment{
					st:      system.NewAtomicString(environment.ProcessOfflineState),
					emitter: events.NewBus(),
				}
				g.Assert(env.State()).Equal(environment.ProcessOfflineState)
			})

			g.It("should update state via SetState", func() {
				env := &Environment{
					st:      system.NewAtomicString(environment.ProcessOfflineState),
					emitter: events.NewBus(),
				}
				env.SetState(environment.ProcessRunningState)
				g.Assert(env.State()).Equal(environment.ProcessRunningState)
			})

			g.It("should panic on invalid state", func() {
				env := &Environment{
					st:      system.NewAtomicString(environment.ProcessOfflineState),
					emitter: events.NewBus(),
				}
				panicked := false
				func() {
					defer func() {
						if r := recover(); r != nil {
							panicked = true
						}
					}()
					env.SetState("bogus")
				}()
				g.Assert(panicked).IsTrue()
			})
		})

		g.Describe("Exists", func() {
			g.It("should return true when Pod exists", func() {
				pod := &corev1.Pod{
					ObjectMeta: metav1.ObjectMeta{
						Name:      "test-uuid",
						Namespace: "pelican",
					},
				}
				client := fake.NewClientset(pod)
				env := &Environment{
					Id:     "test-uuid",
					client: client,
					st:     system.NewAtomicString(environment.ProcessOfflineState),
				}

				config.Update(func(c *config.Configuration) {
					c.Kubernetes.Namespace = "pelican"
				})

				exists, err := env.Exists()
				g.Assert(err).IsNil()
				g.Assert(exists).IsTrue()
			})

			g.It("should return false when Pod does not exist", func() {
				client := fake.NewClientset()
				env := &Environment{
					Id:     "nonexistent-uuid",
					client: client,
					st:     system.NewAtomicString(environment.ProcessOfflineState),
				}

				config.Update(func(c *config.Configuration) {
					c.Kubernetes.Namespace = "pelican"
				})

				exists, err := env.Exists()
				g.Assert(err).IsNil()
				g.Assert(exists).IsFalse()
			})
		})

		g.Describe("IsRunning", func() {
			g.It("should return true when Pod is in Running phase", func() {
				pod := &corev1.Pod{
					ObjectMeta: metav1.ObjectMeta{
						Name:      "test-uuid",
						Namespace: "pelican",
					},
					Status: corev1.PodStatus{
						Phase: corev1.PodRunning,
						ContainerStatuses: []corev1.ContainerStatus{
							{
								Name:  "server",
								State: corev1.ContainerState{Running: &corev1.ContainerStateRunning{}},
							},
						},
					},
				}
				client := fake.NewClientset(pod)
				env := &Environment{
					Id:     "test-uuid",
					client: client,
					st:     system.NewAtomicString(environment.ProcessOfflineState),
				}

				config.Update(func(c *config.Configuration) {
					c.Kubernetes.Namespace = "pelican"
				})

				running, err := env.IsRunning(context.Background())
				g.Assert(err).IsNil()
				g.Assert(running).IsTrue()
			})

			g.It("should return false when Pod is Pending", func() {
				pod := &corev1.Pod{
					ObjectMeta: metav1.ObjectMeta{
						Name:      "test-uuid",
						Namespace: "pelican",
					},
					Status: corev1.PodStatus{
						Phase: corev1.PodPending,
					},
				}
				client := fake.NewClientset(pod)
				env := &Environment{
					Id:     "test-uuid",
					client: client,
					st:     system.NewAtomicString(environment.ProcessOfflineState),
				}

				config.Update(func(c *config.Configuration) {
					c.Kubernetes.Namespace = "pelican"
				})

				running, err := env.IsRunning(context.Background())
				g.Assert(err).IsNil()
				g.Assert(running).IsFalse()
			})
		})

		g.Describe("ExitState", func() {
			g.It("should return exit code and OOM status from terminated container", func() {
				pod := &corev1.Pod{
					ObjectMeta: metav1.ObjectMeta{
						Name:      "test-uuid",
						Namespace: "pelican",
					},
					Status: corev1.PodStatus{
						ContainerStatuses: []corev1.ContainerStatus{
							{
								Name: "server",
								State: corev1.ContainerState{
									Terminated: &corev1.ContainerStateTerminated{
										ExitCode: 137,
										Reason:   "OOMKilled",
									},
								},
							},
						},
					},
				}
				client := fake.NewClientset(pod)
				env := &Environment{
					Id:     "test-uuid",
					client: client,
					st:     system.NewAtomicString(environment.ProcessOfflineState),
				}

				config.Update(func(c *config.Configuration) {
					c.Kubernetes.Namespace = "pelican"
				})

				code, oom, err := env.ExitState()
				g.Assert(err).IsNil()
				g.Assert(code).Equal(uint32(137))
				g.Assert(oom).IsTrue()
			})

			g.It("should return exit code 1 when Pod does not exist", func() {
				client := fake.NewClientset()
				env := &Environment{
					Id:     "nonexistent-uuid",
					client: client,
					st:     system.NewAtomicString(environment.ProcessOfflineState),
				}

				config.Update(func(c *config.Configuration) {
					c.Kubernetes.Namespace = "pelican"
				})

				code, oom, err := env.ExitState()
				g.Assert(err).IsNil()
				g.Assert(code).Equal(uint32(1))
				g.Assert(oom).IsFalse()
			})
		})
	})

	g.Describe("Create", func() {
		g.It("should create a Pod with correct spec", func() {
			client := fake.NewClientset()
			allocs := environment.Allocations{
				Mappings: map[string][]int{"0.0.0.0": {25565}},
			}
			settings := environment.Settings{
				Allocations: allocs,
				Limits: environment.Limits{
					MemoryLimit: 1024,
					CpuLimit:    200,
				},
				Mounts: []environment.Mount{
					{Default: true, Source: "/var/lib/pelican/servers/test", Target: "/home/container"},
				},
			}
			cfg := environment.NewConfiguration(settings, []string{"JAVA_OPTS=-Xmx512M"})
			env := &Environment{
				Id:            "create-test-uuid",
				Configuration: cfg,
				meta:          &Metadata{Image: "itzg/minecraft-server:latest"},
				client:        client,
				st:            system.NewAtomicString(environment.ProcessOfflineState),
				emitter:       events.NewBus(),
			}

			config.Update(func(c *config.Configuration) {
				c.Kubernetes.Namespace = "pelican"
				c.Kubernetes.NetworkMode = config.KubeNetworkHostPort
			})

			err := env.Create()
			g.Assert(err).IsNil()

			pod, err := client.CoreV1().Pods("pelican").Get(context.Background(), "create-test-uuid", metav1.GetOptions{})
			g.Assert(err).IsNil()
			g.Assert(pod.Name).Equal("create-test-uuid")
			g.Assert(pod.Namespace).Equal("pelican")

			// Verify container.
			g.Assert(len(pod.Spec.Containers)).Equal(1)
			c := pod.Spec.Containers[0]
			g.Assert(c.Name).Equal("server")
			g.Assert(c.Image).Equal("itzg/minecraft-server:latest")
			g.Assert(c.Stdin).IsTrue()
			g.Assert(c.TTY).IsTrue()

			// Verify resource limits.
			memLimit := c.Resources.Limits[corev1.ResourceMemory]
			g.Assert(memLimit.Equal(resource.MustParse("1024Mi"))).IsTrue()
			cpuLimit := c.Resources.Limits[corev1.ResourceCPU]
			g.Assert(cpuLimit.Equal(resource.MustParse("2000m"))).IsTrue()

			// Verify environment variables.
			g.Assert(len(c.Env) >= 1).IsTrue()
			found := false
			for _, ev := range c.Env {
				if ev.Name == "JAVA_OPTS" && ev.Value == "-Xmx512M" {
					found = true
				}
			}
			g.Assert(found).IsTrue()

			// Verify volumes.
			g.Assert(len(pod.Spec.Volumes)).Equal(1)
			g.Assert(pod.Spec.Volumes[0].Name).Equal("server-data")
			g.Assert(pod.Spec.Volumes[0].HostPath.Path).Equal("/var/lib/pelican/servers/test")

			// Verify labels.
			g.Assert(pod.Labels["pelican.dev/server-id"]).Equal("create-test-uuid")
			g.Assert(pod.Labels["app.kubernetes.io/managed-by"]).Equal("pelican-wings")
		})

		g.It("should not recreate existing Pod", func() {
			existingPod := &corev1.Pod{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "existing-uuid",
					Namespace: "pelican",
				},
			}
			client := fake.NewClientset(existingPod)
			allocs := environment.Allocations{}
			settings := environment.Settings{
				Allocations: allocs,
				Limits:      environment.Limits{MemoryLimit: 512},
			}
			cfg := environment.NewConfiguration(settings, nil)
			env := &Environment{
				Id:            "existing-uuid",
				Configuration: cfg,
				meta:          &Metadata{Image: "nginx"},
				client:        client,
				st:            system.NewAtomicString(environment.ProcessOfflineState),
				emitter:       events.NewBus(),
			}

			config.Update(func(c *config.Configuration) {
				c.Kubernetes.Namespace = "pelican"
			})

			err := env.Create()
			g.Assert(err).IsNil()
		})

		g.It("should strip ~ prefix from image", func() {
			client := fake.NewClientset()
			allocs := environment.Allocations{}
			settings := environment.Settings{Allocations: allocs}
			cfg := environment.NewConfiguration(settings, nil)
			env := &Environment{
				Id:            "tilde-test-uuid",
				Configuration: cfg,
				meta:          &Metadata{Image: "~local/myimage:latest"},
				client:        client,
				st:            system.NewAtomicString(environment.ProcessOfflineState),
				emitter:       events.NewBus(),
			}

			config.Update(func(c *config.Configuration) {
				c.Kubernetes.Namespace = "pelican"
			})

			err := env.Create()
			g.Assert(err).IsNil()

			pod, _ := client.CoreV1().Pods("pelican").Get(context.Background(), "tilde-test-uuid", metav1.GetOptions{})
			g.Assert(pod.Spec.Containers[0].Image).Equal("local/myimage:latest")
		})

		g.It("should pin the Pod to the Wings node in hostpath/hostport mode", func() {
			client := fake.NewClientset()
			cfg := environment.NewConfiguration(environment.Settings{
				Mounts: []environment.Mount{
					{Default: true, Source: "/var/lib/pelican/servers/pin", Target: "/home/container"},
				},
			}, nil)
			env := &Environment{
				Id:            "pin-test-uuid",
				Configuration: cfg,
				meta:          &Metadata{Image: "nginx"},
				client:        client,
				st:            system.NewAtomicString(environment.ProcessOfflineState),
				emitter:       events.NewBus(),
			}

			config.Update(func(c *config.Configuration) {
				c.Kubernetes.Namespace = "pelican"
				c.Kubernetes.NetworkMode = config.KubeNetworkHostPort
				c.Kubernetes.StorageMode = config.KubeStorageHostPath
				c.Kubernetes.NodeName = "test-node"
				c.Kubernetes.DataPVC = ""
			})

			err := env.Create()
			g.Assert(err).IsNil()

			pod, err := client.CoreV1().Pods("pelican").Get(context.Background(), "pin-test-uuid", metav1.GetOptions{})
			g.Assert(err).IsNil()
			terms := pod.Spec.Affinity.NodeAffinity.RequiredDuringSchedulingIgnoredDuringExecution.NodeSelectorTerms
			g.Assert(len(terms)).Equal(1)
			expr := terms[0].MatchExpressions[0]
			g.Assert(expr.Key).Equal("kubernetes.io/hostname")
			g.Assert(expr.Operator).Equal(corev1.NodeSelectorOpIn)
			g.Assert(expr.Values).Equal([]string{"test-node"})
		})

		g.It("should not pin the Pod in pvc/nodeport mode with only the default mount", func() {
			client := fake.NewClientset()
			cfg := environment.NewConfiguration(environment.Settings{
				Mounts: []environment.Mount{
					{Default: true, Source: "/var/lib/pelican/servers/nopin", Target: "/home/container"},
				},
			}, nil)
			env := &Environment{
				Id:            "nopin-test-uuid",
				Configuration: cfg,
				meta:          &Metadata{Image: "nginx"},
				client:        client,
				st:            system.NewAtomicString(environment.ProcessOfflineState),
				emitter:       events.NewBus(),
			}

			config.Update(func(c *config.Configuration) {
				c.Kubernetes.Namespace = "pelican"
				c.Kubernetes.NetworkMode = config.KubeNetworkNodePort
				c.Kubernetes.StorageMode = config.KubeStoragePVC
				c.Kubernetes.StorageSize = "10Gi"
				c.Kubernetes.NodeName = "test-node"
				c.Kubernetes.DataPVC = ""
			})

			err := env.Create()
			g.Assert(err).IsNil()

			pod, err := client.CoreV1().Pods("pelican").Get(context.Background(), "nopin-test-uuid", metav1.GetOptions{})
			g.Assert(err).IsNil()
			g.Assert(pod.Spec.Affinity == nil).IsTrue()
		})

		g.It("should pin the Pod in pvc/nodeport mode when a non-default mount exists", func() {
			client := fake.NewClientset()
			cfg := environment.NewConfiguration(environment.Settings{
				Mounts: []environment.Mount{
					{Default: true, Source: "/var/lib/pelican/servers/pinm", Target: "/home/container"},
					{Default: false, Source: "/shared/plugins", Target: "/plugins", ReadOnly: true},
				},
			}, nil)
			env := &Environment{
				Id:            "pinm-test-uuid",
				Configuration: cfg,
				meta:          &Metadata{Image: "nginx"},
				client:        client,
				st:            system.NewAtomicString(environment.ProcessOfflineState),
				emitter:       events.NewBus(),
			}

			config.Update(func(c *config.Configuration) {
				c.Kubernetes.Namespace = "pelican"
				c.Kubernetes.NetworkMode = config.KubeNetworkNodePort
				c.Kubernetes.StorageMode = config.KubeStoragePVC
				c.Kubernetes.StorageSize = "10Gi"
				c.Kubernetes.NodeName = "test-node"
				c.Kubernetes.DataPVC = ""
			})

			err := env.Create()
			g.Assert(err).IsNil()

			pod, err := client.CoreV1().Pods("pelican").Get(context.Background(), "pinm-test-uuid", metav1.GetOptions{})
			g.Assert(err).IsNil()
			g.Assert(pod.Spec.Affinity).IsNotNil()
			terms := pod.Spec.Affinity.NodeAffinity.RequiredDuringSchedulingIgnoredDuringExecution.NodeSelectorTerms
			g.Assert(terms[0].MatchExpressions[0].Values[0]).Equal("test-node")
		})

		g.It("should fail to create the Pod when pinning is required but the node name is unknown", func() {
			client := fake.NewClientset()
			cfg := environment.NewConfiguration(environment.Settings{}, nil)
			env := &Environment{
				Id:            "noname-test-uuid",
				Configuration: cfg,
				meta:          &Metadata{Image: "nginx"},
				client:        client,
				st:            system.NewAtomicString(environment.ProcessOfflineState),
				emitter:       events.NewBus(),
			}

			oldNodeName, hadNodeName := os.LookupEnv("NODE_NAME")
			os.Unsetenv("NODE_NAME")
			defer func() {
				if hadNodeName {
					os.Setenv("NODE_NAME", oldNodeName)
				}
			}()

			config.Update(func(c *config.Configuration) {
				c.Kubernetes.Namespace = "pelican"
				c.Kubernetes.NetworkMode = config.KubeNetworkHostPort
				c.Kubernetes.StorageMode = config.KubeStorageHostPath
				c.Kubernetes.NodeName = ""
				c.Kubernetes.DataPVC = ""
			})
			defer config.Update(func(c *config.Configuration) {
				c.Kubernetes.NodeName = "test-node"
			})

			err := env.Create()
			g.Assert(err).IsNotNil()
			g.Assert(strings.Contains(err.Error(), "node pinning required")).IsTrue()

			_, err = client.CoreV1().Pods("pelican").Get(context.Background(), "noname-test-uuid", metav1.GetOptions{})
			g.Assert(err).IsNotNil()
		})
	})

	g.Describe("Destroy", func() {
		g.It("should delete the Pod and transition to offline state", func() {
			existingPod := &corev1.Pod{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "destroy-uuid",
					Namespace: "pelican",
				},
			}
			client := fake.NewClientset(existingPod)
			env := &Environment{
				Id:            "destroy-uuid",
				client:        client,
				st:            system.NewAtomicString(environment.ProcessRunningState),
				emitter:       events.NewBus(),
				Configuration: environment.NewConfiguration(environment.Settings{}, nil),
			}

			config.Update(func(c *config.Configuration) {
				c.Kubernetes.Namespace = "pelican"
				c.Kubernetes.NetworkMode = config.KubeNetworkHostPort
			})

			err := env.Destroy()
			g.Assert(err).IsNil()
			g.Assert(env.State()).Equal(environment.ProcessOfflineState)

			// Pod should be gone.
			_, err = client.CoreV1().Pods("pelican").Get(context.Background(), "destroy-uuid", metav1.GetOptions{})
			g.Assert(err).IsNotNil()
		})

		g.It("should not error when Pod does not exist", func() {
			client := fake.NewClientset()
			env := &Environment{
				Id:            "nonexistent-uuid",
				client:        client,
				st:            system.NewAtomicString(environment.ProcessRunningState),
				emitter:       events.NewBus(),
				Configuration: environment.NewConfiguration(environment.Settings{}, nil),
			}

			config.Update(func(c *config.Configuration) {
				c.Kubernetes.Namespace = "pelican"
				c.Kubernetes.NetworkMode = config.KubeNetworkHostPort
			})

			err := env.Destroy()
			g.Assert(err).IsNil()
			g.Assert(env.State()).Equal(environment.ProcessOfflineState)
		})

		g.It("should also delete the NodePort Service", func() {
			existingPod := &corev1.Pod{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "svc-destroy-uuid",
					Namespace: "pelican",
				},
			}
			existingSvc := &corev1.Service{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "gs-svc-destroy-uuid",
					Namespace: "pelican",
				},
			}
			client := fake.NewClientset(existingPod, existingSvc)
			env := &Environment{
				Id:            "svc-destroy-uuid",
				client:        client,
				st:            system.NewAtomicString(environment.ProcessRunningState),
				emitter:       events.NewBus(),
				Configuration: environment.NewConfiguration(environment.Settings{}, nil),
			}

			config.Update(func(c *config.Configuration) {
				c.Kubernetes.Namespace = "pelican"
				c.Kubernetes.NetworkMode = config.KubeNetworkNodePort
			})

			err := env.Destroy()
			g.Assert(err).IsNil()

			// Both Pod and Service should be gone.
			_, err = client.CoreV1().Pods("pelican").Get(context.Background(), "svc-destroy-uuid", metav1.GetOptions{})
			g.Assert(err).IsNotNil()
			_, err = client.CoreV1().Services("pelican").Get(context.Background(), "gs-svc-destroy-uuid", metav1.GetOptions{})
			g.Assert(err).IsNotNil()
		})
	})

	g.Describe("buildResources", func() {
		g.It("should convert memory and CPU limits correctly", func() {
			env := &Environment{}
			limits := environment.Limits{
				MemoryLimit: 2048,
				CpuLimit:    150,
			}
			resources := env.buildResources(limits)

			memLimit := resources.Limits[corev1.ResourceMemory]
			g.Assert(memLimit.Equal(resource.MustParse("2048Mi"))).IsTrue()

			memReq := resources.Requests[corev1.ResourceMemory]
			g.Assert(memReq.Equal(resource.MustParse("1024Mi"))).IsTrue()

			cpuLimit := resources.Limits[corev1.ResourceCPU]
			g.Assert(cpuLimit.Equal(resource.MustParse("1500m"))).IsTrue()

			cpuReq := resources.Requests[corev1.ResourceCPU]
			g.Assert(cpuReq.Equal(resource.MustParse("375m"))).IsTrue()
		})

		g.It("should set minimum CPU request of 50m", func() {
			env := &Environment{}
			limits := environment.Limits{
				CpuLimit: 10, // 10% = 100m limit, 25m request would be below 50m
			}
			resources := env.buildResources(limits)
			cpuReq := resources.Requests[corev1.ResourceCPU]
			g.Assert(cpuReq.Equal(resource.MustParse("50m"))).IsTrue()
		})

		g.It("should handle zero limits gracefully", func() {
			env := &Environment{}
			limits := environment.Limits{}
			resources := env.buildResources(limits)
			g.Assert(len(resources.Limits)).Equal(0)
			g.Assert(len(resources.Requests)).Equal(0)
		})
	})

	g.Describe("buildVolumes", func() {
		g.It("should create volumes with correct names", func() {
			env := &Environment{}
			mounts := []environment.Mount{
				{Default: true, Source: "/data/server1", Target: "/home/container", ReadOnly: false},
				{Default: false, Source: "/shared/plugins", Target: "/plugins", ReadOnly: true},
			}
			volumes, volumeMounts := env.buildVolumes(mounts)

			g.Assert(len(volumes)).Equal(2)
			g.Assert(volumes[0].Name).Equal("server-data")
			g.Assert(volumes[0].HostPath.Path).Equal("/data/server1")
			g.Assert(volumes[1].Name).Equal("mount-1")
			g.Assert(volumes[1].HostPath.Path).Equal("/shared/plugins")

			g.Assert(len(volumeMounts)).Equal(2)
			g.Assert(volumeMounts[0].MountPath).Equal("/home/container")
			g.Assert(volumeMounts[0].ReadOnly).IsFalse()
			g.Assert(volumeMounts[1].MountPath).Equal("/plugins")
			g.Assert(volumeMounts[1].ReadOnly).IsTrue()
		})
	})

	g.Describe("buildContainerPorts", func() {
		g.It("should create TCP and UDP ports for each allocation", func() {
			allocs := environment.Allocations{
				Mappings: map[string][]int{"0.0.0.0": {25565, 25575}},
			}
			settings := environment.Settings{Allocations: allocs}
			cfg := environment.NewConfiguration(settings, nil)
			env := &Environment{Configuration: cfg}

			config.Update(func(c *config.Configuration) {
				c.Kubernetes.NetworkMode = config.KubeNetworkHostPort
			})

			ports := env.buildContainerPorts()
			g.Assert(len(ports)).Equal(4) // 2 ports × (TCP + UDP)

			// Verify hostPort is set in hostport mode.
			for _, p := range ports {
				g.Assert(p.HostPort).Equal(p.ContainerPort)
			}
		})

		g.It("should not set hostPort in nodeport mode", func() {
			allocs := environment.Allocations{
				Mappings: map[string][]int{"0.0.0.0": {25565}},
			}
			settings := environment.Settings{Allocations: allocs}
			cfg := environment.NewConfiguration(settings, nil)
			env := &Environment{Configuration: cfg}

			config.Update(func(c *config.Configuration) {
				c.Kubernetes.NetworkMode = config.KubeNetworkNodePort
			})

			ports := env.buildContainerPorts()
			g.Assert(len(ports)).Equal(2)
			for _, p := range ports {
				g.Assert(p.HostPort).Equal(int32(0))
			}
		})

		g.It("should skip invalid ports", func() {
			allocs := environment.Allocations{
				Mappings: map[string][]int{"0.0.0.0": {0, -1, 70000, 8080}},
			}
			settings := environment.Settings{Allocations: allocs}
			cfg := environment.NewConfiguration(settings, nil)
			env := &Environment{Configuration: cfg}

			config.Update(func(c *config.Configuration) {
				c.Kubernetes.NetworkMode = config.KubeNetworkNodePort
			})

			ports := env.buildContainerPorts()
			// Only 8080 is valid, so 2 ports (TCP + UDP).
			g.Assert(len(ports)).Equal(2)
		})

		g.It("should deduplicate ports shared across allocation IPs", func() {
			allocs := environment.Allocations{
				Mappings: map[string][]int{
					"1.1.1.1": {25565},
					"2.2.2.2": {25565},
				},
			}
			settings := environment.Settings{Allocations: allocs}
			cfg := environment.NewConfiguration(settings, nil)
			env := &Environment{Configuration: cfg}

			config.Update(func(c *config.Configuration) {
				c.Kubernetes.NetworkMode = config.KubeNetworkNodePort
			})

			ports := env.buildContainerPorts()
			// 25565 appears under two IPs but must yield a single TCP + UDP pair.
			g.Assert(len(ports)).Equal(2)
		})
	})
}
