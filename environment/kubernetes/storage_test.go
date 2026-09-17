package kubernetes

import (
	"context"
	"testing"

	. "github.com/franela/goblin"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"

	"github.com/pelican/wings/config"
	"github.com/pelican/wings/environment"
	"github.com/pelican/wings/system"
)

func TestStorage(t *testing.T) {
	g := Goblin(t)

	g.Describe("PVC lifecycle", func() {
		g.Describe("pvcName", func() {
			g.It("should return correct PVC name", func() {
				env := &Environment{Id: "abc-123-def"}
				g.Assert(env.pvcName()).Equal("gs-abc-123-def")
			})
		})

		g.Describe("EnsurePVC", func() {
			g.It("should be a no-op in hostpath mode", func() {
				client := fake.NewClientset()
				env := &Environment{
					Id:     "hostpath-uuid",
					client: client,
					st:     system.NewAtomicString(environment.ProcessOfflineState),
				}

				config.Update(func(c *config.Configuration) {
					c.Kubernetes.StorageMode = config.KubeStorageHostPath
					c.Kubernetes.Namespace = "pelican"
				})

				err := env.EnsurePVC(context.Background())
				g.Assert(err).IsNil()

				// No PVC should exist.
				pvcs, _ := client.CoreV1().PersistentVolumeClaims("pelican").List(context.Background(), metav1.ListOptions{})
				g.Assert(len(pvcs.Items)).Equal(0)
			})

			g.It("should create a PVC in pvc mode", func() {
				client := fake.NewClientset()
				env := &Environment{
					Id:     "pvc-create-uuid",
					client: client,
					st:     system.NewAtomicString(environment.ProcessOfflineState),
				}

				config.Update(func(c *config.Configuration) {
					c.Kubernetes.StorageMode = config.KubeStoragePVC
					c.Kubernetes.Namespace = "pelican"
					c.Kubernetes.StorageSize = "20Gi"
					c.Kubernetes.StorageClass = "fast-ssd"
					c.Kubernetes.StorageAccessMode = "ReadWriteOnce"
				})

				err := env.EnsurePVC(context.Background())
				g.Assert(err).IsNil()

				pvc, err := client.CoreV1().PersistentVolumeClaims("pelican").Get(context.Background(), "gs-pvc-create-uuid", metav1.GetOptions{})
				g.Assert(err).IsNil()
				g.Assert(pvc.Name).Equal("gs-pvc-create-uuid")
				g.Assert(pvc.Labels["pelican.dev/server-id"]).Equal("pvc-create-uuid")
				g.Assert(pvc.Spec.AccessModes[0]).Equal(corev1.ReadWriteOnce)

				// Check storage size.
				storageReq := pvc.Spec.Resources.Requests[corev1.ResourceStorage]
				expected := resource.MustParse("20Gi")
				g.Assert(storageReq.Equal(expected)).IsTrue()

				// Check storage class.
				g.Assert(*pvc.Spec.StorageClassName).Equal("fast-ssd")
			})

			g.It("should create PVC with ReadWriteMany access mode", func() {
				client := fake.NewClientset()
				env := &Environment{
					Id:     "rwm-uuid",
					client: client,
					st:     system.NewAtomicString(environment.ProcessOfflineState),
				}

				config.Update(func(c *config.Configuration) {
					c.Kubernetes.StorageMode = config.KubeStoragePVC
					c.Kubernetes.Namespace = "pelican"
					c.Kubernetes.StorageSize = "5Gi"
					c.Kubernetes.StorageAccessMode = "ReadWriteMany"
					c.Kubernetes.StorageClass = ""
				})

				err := env.EnsurePVC(context.Background())
				g.Assert(err).IsNil()

				pvc, _ := client.CoreV1().PersistentVolumeClaims("pelican").Get(context.Background(), "gs-rwm-uuid", metav1.GetOptions{})
				g.Assert(pvc.Spec.AccessModes[0]).Equal(corev1.ReadWriteMany)
				// No storage class set — should be nil.
				g.Assert(pvc.Spec.StorageClassName == nil).IsTrue()
			})

			g.It("should not recreate existing PVC", func() {
				existingPVC := &corev1.PersistentVolumeClaim{
					ObjectMeta: metav1.ObjectMeta{
						Name:      "gs-existing-uuid",
						Namespace: "pelican",
					},
				}
				client := fake.NewClientset(existingPVC)
				env := &Environment{
					Id:     "existing-uuid",
					client: client,
					st:     system.NewAtomicString(environment.ProcessOfflineState),
				}

				config.Update(func(c *config.Configuration) {
					c.Kubernetes.StorageMode = config.KubeStoragePVC
					c.Kubernetes.Namespace = "pelican"
					c.Kubernetes.StorageSize = "10Gi"
				})

				err := env.EnsurePVC(context.Background())
				g.Assert(err).IsNil()
			})

			g.It("should error on invalid storage size", func() {
				client := fake.NewClientset()
				env := &Environment{
					Id:     "bad-size-uuid",
					client: client,
					st:     system.NewAtomicString(environment.ProcessOfflineState),
				}

				config.Update(func(c *config.Configuration) {
					c.Kubernetes.StorageMode = config.KubeStoragePVC
					c.Kubernetes.Namespace = "pelican"
					c.Kubernetes.StorageSize = "not-a-quantity"
				})

				err := env.EnsurePVC(context.Background())
				g.Assert(err).IsNotNil()
			})
		})

		g.Describe("DeletePVC", func() {
			g.It("should be a no-op in hostpath mode", func() {
				client := fake.NewClientset()
				env := &Environment{
					Id:     "hostpath-del-uuid",
					client: client,
					st:     system.NewAtomicString(environment.ProcessOfflineState),
				}

				config.Update(func(c *config.Configuration) {
					c.Kubernetes.StorageMode = config.KubeStorageHostPath
					c.Kubernetes.Namespace = "pelican"
				})

				err := env.DeletePVC(context.Background())
				g.Assert(err).IsNil()
			})

			g.It("should delete an existing PVC", func() {
				existingPVC := &corev1.PersistentVolumeClaim{
					ObjectMeta: metav1.ObjectMeta{
						Name:      "gs-del-uuid",
						Namespace: "pelican",
					},
				}
				client := fake.NewClientset(existingPVC)
				env := &Environment{
					Id:     "del-uuid",
					client: client,
					st:     system.NewAtomicString(environment.ProcessOfflineState),
				}

				config.Update(func(c *config.Configuration) {
					c.Kubernetes.StorageMode = config.KubeStoragePVC
					c.Kubernetes.Namespace = "pelican"
				})

				err := env.DeletePVC(context.Background())
				g.Assert(err).IsNil()

				_, err = client.CoreV1().PersistentVolumeClaims("pelican").Get(context.Background(), "gs-del-uuid", metav1.GetOptions{})
				g.Assert(err).IsNotNil()
			})

			g.It("should not error when PVC does not exist", func() {
				client := fake.NewClientset()
				env := &Environment{
					Id:     "nonexist-uuid",
					client: client,
					st:     system.NewAtomicString(environment.ProcessOfflineState),
				}

				config.Update(func(c *config.Configuration) {
					c.Kubernetes.StorageMode = config.KubeStoragePVC
					c.Kubernetes.Namespace = "pelican"
				})

				err := env.DeletePVC(context.Background())
				g.Assert(err).IsNil()
			})
		})

		g.Describe("ResizePVC", func() {
			g.It("should be a no-op in hostpath mode", func() {
				client := fake.NewClientset()
				env := &Environment{
					Id:     "resize-hp-uuid",
					client: client,
					st:     system.NewAtomicString(environment.ProcessOfflineState),
				}

				config.Update(func(c *config.Configuration) {
					c.Kubernetes.StorageMode = config.KubeStorageHostPath
					c.Kubernetes.Namespace = "pelican"
				})

				err := env.ResizePVC(context.Background(), "50Gi")
				g.Assert(err).IsNil()
			})

			g.It("should update PVC storage request", func() {
				existingPVC := &corev1.PersistentVolumeClaim{
					ObjectMeta: metav1.ObjectMeta{
						Name:      "gs-resize-uuid",
						Namespace: "pelican",
					},
					Spec: corev1.PersistentVolumeClaimSpec{
						Resources: corev1.VolumeResourceRequirements{
							Requests: corev1.ResourceList{
								corev1.ResourceStorage: resource.MustParse("10Gi"),
							},
						},
					},
				}
				client := fake.NewClientset(existingPVC)
				env := &Environment{
					Id:     "resize-uuid",
					client: client,
					st:     system.NewAtomicString(environment.ProcessOfflineState),
				}

				config.Update(func(c *config.Configuration) {
					c.Kubernetes.StorageMode = config.KubeStoragePVC
					c.Kubernetes.Namespace = "pelican"
				})

				err := env.ResizePVC(context.Background(), "50Gi")
				g.Assert(err).IsNil()

				pvc, _ := client.CoreV1().PersistentVolumeClaims("pelican").Get(context.Background(), "gs-resize-uuid", metav1.GetOptions{})
				storageReq := pvc.Spec.Resources.Requests[corev1.ResourceStorage]
				expected := resource.MustParse("50Gi")
				g.Assert(storageReq.Equal(expected)).IsTrue()
			})

			g.It("should error on invalid size", func() {
				client := fake.NewClientset()
				env := &Environment{
					Id:     "bad-resize-uuid",
					client: client,
					st:     system.NewAtomicString(environment.ProcessOfflineState),
				}

				config.Update(func(c *config.Configuration) {
					c.Kubernetes.StorageMode = config.KubeStoragePVC
					c.Kubernetes.Namespace = "pelican"
				})

				err := env.ResizePVC(context.Background(), "invalid")
				g.Assert(err).IsNotNil()
			})

			g.It("should be a no-op when a shared DataPVC is configured", func() {
				existingPVC := &corev1.PersistentVolumeClaim{
					ObjectMeta: metav1.ObjectMeta{
						Name:      "gs-shared-resize-uuid",
						Namespace: "pelican",
					},
					Spec: corev1.PersistentVolumeClaimSpec{
						Resources: corev1.VolumeResourceRequirements{
							Requests: corev1.ResourceList{
								corev1.ResourceStorage: resource.MustParse("10Gi"),
							},
						},
					},
				}
				client := fake.NewClientset(existingPVC)
				env := &Environment{
					Id:     "shared-resize-uuid",
					client: client,
					st:     system.NewAtomicString(environment.ProcessOfflineState),
				}

				config.Update(func(c *config.Configuration) {
					c.Kubernetes.StorageMode = config.KubeStoragePVC
					c.Kubernetes.Namespace = "pelican"
					c.Kubernetes.DataPVC = "shared-data"
				})
				defer config.Update(func(c *config.Configuration) {
					c.Kubernetes.DataPVC = ""
				})

				err := env.ResizePVC(context.Background(), "50Gi")
				g.Assert(err).IsNil()

				// The per-server PVC must be left untouched at its original size.
				pvc, _ := client.CoreV1().PersistentVolumeClaims("pelican").Get(context.Background(), "gs-shared-resize-uuid", metav1.GetOptions{})
				storageReq := pvc.Spec.Resources.Requests[corev1.ResourceStorage]
				original := resource.MustParse("10Gi")
				g.Assert(storageReq.Equal(original)).IsTrue()
			})
		})

		g.Describe("buildVolumes with PVC mode", func() {
			g.It("should use PVC for default mount in pvc mode", func() {
				client := fake.NewClientset()
				env := &Environment{
					Id:     "bv-pvc-uuid",
					client: client,
					st:     system.NewAtomicString(environment.ProcessOfflineState),
				}

				config.Update(func(c *config.Configuration) {
					c.Kubernetes.StorageMode = config.KubeStoragePVC
					c.Kubernetes.Namespace = "pelican"
				})

				mounts := []environment.Mount{
					{Default: true, Source: "/data/servers/bv-pvc-uuid", Target: "/home/container"},
					{Default: false, Source: "/shared/plugins", Target: "/plugins", ReadOnly: true},
				}

				volumes, volumeMounts := env.buildVolumes(mounts)

				g.Assert(len(volumes)).Equal(2)
				// First volume (default) should use PVC.
				g.Assert(volumes[0].Name).Equal("server-data")
				g.Assert(volumes[0].PersistentVolumeClaim).IsNotNil()
				g.Assert(volumes[0].PersistentVolumeClaim.ClaimName).Equal("gs-bv-pvc-uuid")
				g.Assert(volumes[0].HostPath == nil).IsTrue()

				// Second volume (non-default) should still use HostPath.
				g.Assert(volumes[1].Name).Equal("mount-1")
				g.Assert(volumes[1].HostPath).IsNotNil()
				g.Assert(volumes[1].HostPath.Path).Equal("/shared/plugins")

				g.Assert(len(volumeMounts)).Equal(2)
				g.Assert(volumeMounts[0].MountPath).Equal("/home/container")
				g.Assert(volumeMounts[1].MountPath).Equal("/plugins")
				g.Assert(volumeMounts[1].ReadOnly).IsTrue()
			})

			g.It("should use HostPath for default mount in hostpath mode", func() {
				client := fake.NewClientset()
				env := &Environment{
					Id:     "bv-hp-uuid",
					client: client,
					st:     system.NewAtomicString(environment.ProcessOfflineState),
				}

				config.Update(func(c *config.Configuration) {
					c.Kubernetes.StorageMode = config.KubeStorageHostPath
					c.Kubernetes.Namespace = "pelican"
				})

				mounts := []environment.Mount{
					{Default: true, Source: "/data/servers/test", Target: "/home/container"},
				}

				volumes, _ := env.buildVolumes(mounts)
				g.Assert(volumes[0].HostPath).IsNotNil()
				g.Assert(volumes[0].HostPath.Path).Equal("/data/servers/test")
				g.Assert(volumes[0].PersistentVolumeClaim == nil).IsTrue()
			})
		})
	})
}
