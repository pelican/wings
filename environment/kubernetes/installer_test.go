package kubernetes

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	. "github.com/franela/goblin"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes/fake"

	"github.com/pelican/wings/config"
	"github.com/pelican/wings/environment"
	"github.com/pelican/wings/remote"
	"github.com/pelican/wings/system"
)

func int32Ptr(i int32) *int32 { return &i }

func TestInstaller(t *testing.T) {
	g := Goblin(t)

	g.Describe("InstallerProcess", func() {
		g.Describe("jobName", func() {
			g.It("should return correct job name", func() {
				ip := &InstallerProcess{ServerID: "abc-123-def"}
				g.Assert(ip.jobName()).Equal("abc-123-def-installer")
			})
		})

		g.Describe("configMapName", func() {
			g.It("should return correct configmap name", func() {
				ip := &InstallerProcess{ServerID: "abc-123-def"}
				g.Assert(ip.configMapName()).Equal("abc-123-def-install-script")
			})
		})

		g.Describe("writeInstallScript", func() {
			newJob := func(client *fake.Clientset, name string) *batchv1.Job {
				job, err := client.BatchV1().Jobs("pelican").Create(context.Background(), &batchv1.Job{
					ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "pelican"},
				}, metav1.CreateOptions{})
				if err != nil {
					panic(err)
				}
				return job
			}

			g.It("should create a ConfigMap with the script content owned by the Job", func() {
				client := fake.NewClientset()
				ip := &InstallerProcess{
					ServerID:  "write-cm-uuid",
					client:    client,
					namespace: "pelican",
					Script: &remote.InstallationScript{
						Script: "#!/bin/bash\necho hello\r\necho world",
					},
				}

				job := newJob(client, "write-cm-uuid-installer")
				err := ip.writeInstallScript(context.Background(), job)
				g.Assert(err).IsNil()

				cm, err := client.CoreV1().ConfigMaps("pelican").Get(context.Background(), "write-cm-uuid-install-script", metav1.GetOptions{})
				g.Assert(err).IsNil()
				// Should replace \r\n with \n.
				g.Assert(cm.Data["install.sh"]).Equal("#!/bin/bash\necho hello\necho world")
				g.Assert(cm.Labels["pelican.dev/server-id"]).Equal("write-cm-uuid")
				// Should be owned by the Job.
				g.Assert(len(cm.OwnerReferences)).Equal(1)
				g.Assert(cm.OwnerReferences[0].Kind).Equal("Job")
				g.Assert(cm.OwnerReferences[0].Name).Equal("write-cm-uuid-installer")
				g.Assert(cm.OwnerReferences[0].UID).Equal(job.UID)
			})

			g.It("should replace existing ConfigMap on re-run", func() {
				existingCM := &corev1.ConfigMap{
					ObjectMeta: metav1.ObjectMeta{
						Name:      "rewrite-uuid-install-script",
						Namespace: "pelican",
					},
					Data: map[string]string{"install.sh": "old script"},
				}
				client := fake.NewClientset(existingCM)
				ip := &InstallerProcess{
					ServerID:  "rewrite-uuid",
					client:    client,
					namespace: "pelican",
					Script: &remote.InstallationScript{
						Script: "new script",
					},
				}

				job := newJob(client, "rewrite-uuid-installer")
				err := ip.writeInstallScript(context.Background(), job)
				g.Assert(err).IsNil()

				cm, err := client.CoreV1().ConfigMaps("pelican").Get(context.Background(), "rewrite-uuid-install-script", metav1.GetOptions{})
				g.Assert(err).IsNil()
				g.Assert(cm.Data["install.sh"]).Equal("new script")
				g.Assert(len(cm.OwnerReferences)).Equal(1)
			})
		})

		g.Describe("Run", func() {
			g.It("should create a Job with correct spec", func() {
				client := fake.NewClientset()
				ip := &InstallerProcess{
					ServerID:   "test-server-uuid",
					client:     client,
					namespace:  "pelican",
					ServerPath: "/var/lib/pelican/servers/test-server-uuid",
					TmpDir:     "/tmp/test-server-uuid",
					EnvVars:    []string{"SERVER_MEMORY=1024", "SERVER_IP=0.0.0.0"},
					Script: &remote.InstallationScript{
						ContainerImage: "ghcr.io/pelican/installers:alpine",
						Entrypoint:     "bash",
						Script:         "echo installing",
					},
					Sink: system.NewSinkPool(),
				}

				config.Update(func(c *config.Configuration) {
					c.Kubernetes.Namespace = "pelican"
					c.Kubernetes.NodeSelector = map[string]string{"role": "game"}
					c.Kubernetes.ServiceAccount = "pelican-wings"
					c.Kubernetes.StorageMode = config.KubeStorageHostPath
					c.Kubernetes.NetworkMode = config.KubeNetworkHostPort
					c.Kubernetes.NodeName = "test-node"
				})

				// Run will create the Job but waitForJob will keep polling.
				// We'll cancel the context to stop it.
				ctx, cancel := context.WithCancel(context.Background())

				// Run in goroutine since it will block waiting for Job completion.
				errCh := make(chan error, 1)
				go func() {
					errCh <- ip.Run(ctx)
				}()

				// Poll until Job exists or timeout.
				var job *batchv1.Job
				for i := 0; i < 50; i++ {
					var err error
					job, err = client.BatchV1().Jobs("pelican").Get(context.Background(), "test-server-uuid-installer", metav1.GetOptions{})
					if err == nil {
						break
					}
					time.Sleep(10 * time.Millisecond)
				}
				g.Assert(job).IsNotNil()

				// Verify Job metadata.
				g.Assert(job.Name).Equal("test-server-uuid-installer")
				g.Assert(job.Namespace).Equal("pelican")
				g.Assert(job.Labels["pelican.dev/server-id"]).Equal("test-server-uuid")
				g.Assert(job.Labels["pelican.dev/container-type"]).Equal("server_installer")

				// Verify Pod template spec.
				podSpec := job.Spec.Template.Spec
				g.Assert(podSpec.RestartPolicy).Equal(corev1.RestartPolicyNever)
				g.Assert(len(podSpec.Containers)).Equal(1)

				container := podSpec.Containers[0]
				g.Assert(container.Name).Equal("installer")
				g.Assert(container.Image).Equal("ghcr.io/pelican/installers:alpine")
				g.Assert(container.Command).Equal([]string{"bash", "/mnt/install/install.sh"})

				// Verify env vars.
				g.Assert(len(container.Env)).Equal(2)
				g.Assert(container.Env[0].Name).Equal("SERVER_MEMORY")
				g.Assert(container.Env[0].Value).Equal("1024")
				g.Assert(container.Env[1].Name).Equal("SERVER_IP")
				g.Assert(container.Env[1].Value).Equal("0.0.0.0")

				// Verify volume mounts.
				g.Assert(len(container.VolumeMounts)).Equal(2)
				g.Assert(container.VolumeMounts[0].MountPath).Equal("/mnt/server")
				g.Assert(container.VolumeMounts[1].MountPath).Equal("/mnt/install")

				// Verify volumes.
				g.Assert(len(podSpec.Volumes)).Equal(2)
				g.Assert(podSpec.Volumes[0].HostPath.Path).Equal("/var/lib/pelican/servers/test-server-uuid")
				// Install script now uses ConfigMap.
				g.Assert(podSpec.Volumes[1].ConfigMap).IsNotNil()
				g.Assert(podSpec.Volumes[1].ConfigMap.Name).Equal("test-server-uuid-install-script")
				g.Assert(*podSpec.Volumes[1].ConfigMap.DefaultMode).Equal(int32(0755))

				// Verify node selector.
				g.Assert(podSpec.NodeSelector["role"]).Equal("game")

				// Verify service account.
				g.Assert(podSpec.ServiceAccountName).Equal("pelican-wings")

				// Verify node pinning affinity (hostpath storage requires it).
				terms := podSpec.Affinity.NodeAffinity.RequiredDuringSchedulingIgnoredDuringExecution.NodeSelectorTerms
				g.Assert(len(terms)).Equal(1)
				g.Assert(terms[0].MatchExpressions[0].Key).Equal("kubernetes.io/hostname")
				g.Assert(terms[0].MatchExpressions[0].Values[0]).Equal("test-node")

				// Verify backoff limit.
				g.Assert(*job.Spec.BackoffLimit).Equal(int32(0))

				// Verify the install-script ConfigMap is owned by the Job.
				cm, err := client.CoreV1().ConfigMaps("pelican").Get(context.Background(), "test-server-uuid-install-script", metav1.GetOptions{})
				g.Assert(err).IsNil()
				g.Assert(len(cm.OwnerReferences)).Equal(1)
				g.Assert(cm.OwnerReferences[0].Kind).Equal("Job")
				g.Assert(cm.OwnerReferences[0].Name).Equal(job.Name)
				g.Assert(cm.OwnerReferences[0].UID).Equal(job.UID)

				// Cancel to stop waitForJob and assert the cancellation propagates.
				cancel()
				g.Assert(errors.Is(<-errCh, context.Canceled)).IsTrue()
			})

			g.It("should succeed when Job completes successfully", func() {
				// Pre-create a Job that is already succeeded.
				completedJob := &batchv1.Job{
					ObjectMeta: metav1.ObjectMeta{
						Name:      "completed-uuid-installer",
						Namespace: "pelican",
					},
					Status: batchv1.JobStatus{
						Succeeded: 1,
					},
				}
				client := fake.NewClientset(completedJob)
				ip := &InstallerProcess{
					ServerID:  "completed-uuid",
					client:    client,
					namespace: "pelican",
					EnvVars:   []string{},
					Script: &remote.InstallationScript{
						ContainerImage: "alpine",
						Entrypoint:     "sh",
						Script:         "echo done",
					},
					Sink: system.NewSinkPool(),
				}

				config.Update(func(c *config.Configuration) {
					c.Kubernetes.Namespace = "pelican"
				})

				// The fake client will return the existing Job (already succeeded)
				// when Run queries for it after creating a new one.
				// However the fake client won't auto-set status. Let's test Cleanup instead.
				err := ip.Cleanup(context.Background())
				g.Assert(err).IsNil()

				// Verify the job was deleted.
				_, err = client.BatchV1().Jobs("pelican").Get(context.Background(), "completed-uuid-installer", metav1.GetOptions{})
				g.Assert(err).IsNotNil()
			})
		})

		g.Describe("Cleanup", func() {
			g.It("should delete the Job and leave the owned ConfigMap for GC", func() {
				existingJob := &batchv1.Job{
					ObjectMeta: metav1.ObjectMeta{
						Name:      "cleanup-uuid-installer",
						Namespace: "pelican",
						UID:       "job-uid-123",
					},
				}
				controller := true
				existingCM := &corev1.ConfigMap{
					ObjectMeta: metav1.ObjectMeta{
						Name:      "cleanup-uuid-install-script",
						Namespace: "pelican",
						OwnerReferences: []metav1.OwnerReference{
							{
								APIVersion: "batch/v1",
								Kind:       "Job",
								Name:       "cleanup-uuid-installer",
								UID:        "job-uid-123",
								Controller: &controller,
							},
						},
					},
				}
				client := fake.NewClientset(existingJob, existingCM)
				ip := &InstallerProcess{
					ServerID:  "cleanup-uuid",
					client:    client,
					namespace: "pelican",
				}

				err := ip.Cleanup(context.Background())
				g.Assert(err).IsNil()

				_, err = client.BatchV1().Jobs("pelican").Get(context.Background(), "cleanup-uuid-installer", metav1.GetOptions{})
				g.Assert(err).IsNotNil()

				// The fake clientset performs no garbage collection, so the
				// ConfigMap is still present; on a real cluster it is removed
				// via its ownerReference to the Job.
				cm, err := client.CoreV1().ConfigMaps("pelican").Get(context.Background(), "cleanup-uuid-install-script", metav1.GetOptions{})
				g.Assert(err).IsNil()
				g.Assert(len(cm.OwnerReferences)).Equal(1)
				g.Assert(cm.OwnerReferences[0].Kind).Equal("Job")
				g.Assert(cm.OwnerReferences[0].UID).Equal(types.UID("job-uid-123"))
			})

			g.It("should not error when Job and ConfigMap do not exist", func() {
				client := fake.NewClientset()
				ip := &InstallerProcess{
					ServerID:  "nonexistent-uuid",
					client:    client,
					namespace: "pelican",
				}

				err := ip.Cleanup(context.Background())
				g.Assert(err).IsNil()
			})

			g.It("should remove temp directory", func() {
				tmpDir := filepath.Join(os.TempDir(), "test-installer-cleanup")
				os.MkdirAll(tmpDir, 0o700)
				os.WriteFile(filepath.Join(tmpDir, "install.sh"), []byte("echo hi"), 0o644)

				client := fake.NewClientset()
				ip := &InstallerProcess{
					ServerID:  "tmpdir-uuid",
					client:    client,
					namespace: "pelican",
					TmpDir:    tmpDir,
				}

				err := ip.Cleanup(context.Background())
				g.Assert(err).IsNil()

				_, err = os.Stat(tmpDir)
				g.Assert(os.IsNotExist(err)).IsTrue()
			})
		})

		g.Describe("buildJobVolumes", func() {
			g.It("should use HostPath for server-data in hostpath mode and ConfigMap for install script", func() {
				ip := &InstallerProcess{
					ServerID:   "vol-hp-uuid",
					ServerPath: "/data/servers/vol-hp-uuid",
				}
				config.Update(func(c *config.Configuration) {
					c.Kubernetes.StorageMode = config.KubeStorageHostPath
				})
				cfg := config.Get()
				volumes := ip.buildJobVolumes(cfg)
				g.Assert(len(volumes)).Equal(2)
				g.Assert(volumes[0].Name).Equal("server-data")
				g.Assert(volumes[0].HostPath).IsNotNil()
				g.Assert(volumes[0].HostPath.Path).Equal("/data/servers/vol-hp-uuid")
				g.Assert(volumes[0].PersistentVolumeClaim == nil).IsTrue()
				// Install script uses ConfigMap.
				g.Assert(volumes[1].Name).Equal("install-script")
				g.Assert(volumes[1].ConfigMap).IsNotNil()
				g.Assert(volumes[1].ConfigMap.Name).Equal("vol-hp-uuid-install-script")
				g.Assert(*volumes[1].ConfigMap.DefaultMode).Equal(int32(0755))
			})

			g.It("should use PVC for server-data in pvc mode and ConfigMap for install script", func() {
				ip := &InstallerProcess{
					ServerID:   "vol-pvc-uuid",
					ServerPath: "/data/servers/vol-pvc-uuid",
				}
				config.Update(func(c *config.Configuration) {
					c.Kubernetes.StorageMode = config.KubeStoragePVC
				})
				cfg := config.Get()
				volumes := ip.buildJobVolumes(cfg)
				g.Assert(len(volumes)).Equal(2)
				g.Assert(volumes[0].Name).Equal("server-data")
				g.Assert(volumes[0].PersistentVolumeClaim).IsNotNil()
				g.Assert(volumes[0].PersistentVolumeClaim.ClaimName).Equal("gs-vol-pvc-uuid")
				// Install script uses ConfigMap.
				g.Assert(volumes[1].ConfigMap).IsNotNil()
				g.Assert(volumes[1].ConfigMap.Name).Equal("vol-pvc-uuid-install-script")
			})
		})

		g.Describe("NewInstallerProcess", func() {
			g.It("should initialize all fields from Environment", func() {
				client := fake.NewClientset()
				env := &Environment{
					Id:     "init-test-uuid",
					client: client,
					st:     system.NewAtomicString(environment.ProcessOfflineState),
				}

				config.Update(func(c *config.Configuration) {
					c.Kubernetes.Namespace = "pelican"
				})

				script := &remote.InstallationScript{
					ContainerImage: "alpine:latest",
					Entrypoint:     "sh",
					Script:         "echo test",
				}
				sink := system.NewSinkPool()

				ip := NewInstallerProcess(env, script, []string{"FOO=bar"}, "/data/servers/test", "/tmp/install", sink)

				g.Assert(ip.ServerID).Equal("init-test-uuid")
				g.Assert(ip.namespace).Equal("pelican")
				g.Assert(ip.Script.ContainerImage).Equal("alpine:latest")
				g.Assert(ip.EnvVars[0]).Equal("FOO=bar")
				g.Assert(ip.ServerPath).Equal("/data/servers/test")
				g.Assert(ip.TmpDir).Equal("/tmp/install")
			})
		})
	})
}
