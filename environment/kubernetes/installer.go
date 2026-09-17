package kubernetes

import (
	"bufio"
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"emperror.dev/errors"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/watch"
	"k8s.io/client-go/kubernetes"

	"github.com/pelican/wings/config"
	"github.com/pelican/wings/remote"
	"github.com/pelican/wings/system"
)

// InstallerProcess handles running server installation scripts as Kubernetes
// Jobs. It mirrors the Docker-based InstallationProcess but uses Jobs instead
// of standalone containers.
type InstallerProcess struct {
	ServerID   string
	Script     *remote.InstallationScript
	EnvVars    []string
	ServerPath string
	TmpDir     string
	Sink       *system.SinkPool
	client     kubernetes.Interface
	namespace  string
}

// NewInstallerProcess creates a new Kubernetes-based installer process for a
// server. The caller is responsible for writing the install script to tmpDir.
func NewInstallerProcess(env *Environment, script *remote.InstallationScript, envVars []string, serverPath, tmpDir string, sink *system.SinkPool) *InstallerProcess {
	return &InstallerProcess{
		ServerID:   env.Id,
		Script:     script,
		EnvVars:    envVars,
		ServerPath: serverPath,
		TmpDir:     tmpDir,
		Sink:       sink,
		client:     env.client,
		namespace:  env.namespace(),
	}
}

// jobName returns the name for the installation Job.
func (ip *InstallerProcess) jobName() string {
	return fmt.Sprintf("%s-installer", ip.ServerID)
}

// configMapName returns the name for the install script ConfigMap.
func (ip *InstallerProcess) configMapName() string {
	return fmt.Sprintf("%s-install-script", ip.ServerID)
}

// Run executes the full installation process: creates a Job, streams its logs,
// waits for completion, and cleans up.
func (ip *InstallerProcess) Run(ctx context.Context) error {
	// Clean up any existing installer Job from a previous run. Use foreground
	// propagation and wait for the Job to disappear so the Create below does not
	// race a still-terminating Job and fail with AlreadyExists.
	if err := ip.client.BatchV1().Jobs(ip.namespace).Delete(ctx, ip.jobName(), metav1.DeleteOptions{
		PropagationPolicy: propagationForeground(),
	}); err != nil && !isNotFound(err) {
		return errors.Wrap(err, "environment/kubernetes: failed to delete existing installer job")
	}
	if err := ip.waitForJobDeletion(ctx, 30*time.Second); err != nil {
		return errors.Wrap(err, "environment/kubernetes: timed out waiting for old installer job deletion")
	}

	// Build environment variables.
	var envVars []corev1.EnvVar
	for _, ev := range ip.EnvVars {
		parts := strings.SplitN(ev, "=", 2)
		if len(parts) == 2 {
			envVars = append(envVars, corev1.EnvVar{
				Name:  parts[0],
				Value: parts[1],
			})
		}
	}

	cfg := config.Get()

	// Determine install image and pull policy (mirrors the Docker backend).
	installImage, installPullPolicy := resolveImagePullPolicy(ip.Script.ContainerImage)

	// Build the Job spec.
	backoffLimit := int32(0)
	ttl := int32(300) // Auto-clean Job after 5 minutes.
	job := &batchv1.Job{
		ObjectMeta: metav1.ObjectMeta{
			Name:      ip.jobName(),
			Namespace: ip.namespace,
			Labels: map[string]string{
				"app.kubernetes.io/managed-by": "pelican-wings",
				"pelican.dev/server-id":        ip.ServerID,
				"pelican.dev/container-type":   "server_installer",
			},
		},
		Spec: batchv1.JobSpec{
			BackoffLimit:            &backoffLimit,
			TTLSecondsAfterFinished: &ttl,
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{
					Labels: map[string]string{
						"app.kubernetes.io/managed-by": "pelican-wings",
						"pelican.dev/server-id":        ip.ServerID,
						"pelican.dev/container-type":   "server_installer",
						"job-name":                     ip.jobName(),
					},
				},
				Spec: corev1.PodSpec{
					RestartPolicy: corev1.RestartPolicyNever,
					Containers: []corev1.Container{
						{
							Name:    "installer",
							Image:   installImage,
							Command: []string{ip.Script.Entrypoint, "/mnt/install/install.sh"},
							Env:     envVars,
							VolumeMounts: []corev1.VolumeMount{
								ip.buildServerDataMount(),
								{
									Name:      "install-script",
									MountPath: "/mnt/install",
								},
							},
							ImagePullPolicy: installPullPolicy,
						},
					},
					Volumes: ip.buildJobVolumes(cfg),
				},
			},
		},
	}

	// Pin the installer Pod to the Wings node when it depends on node-local
	// resources (HostPath storage or hostPort networking). The installer only
	// mounts the default data volume, so no extra mounts are considered.
	if requiresNodePinning(cfg, nil) {
		nodeName := resolveNodeName()
		if nodeName == "" {
			return errors.New("environment/kubernetes: node pinning required (hostpath storage / hostport networking / hostPath mounts) but node name is unknown; set kubernetes.node_name or the NODE_NAME env var")
		}
		job.Spec.Template.Spec.Affinity = nodeAffinityFor(nodeName)
	}

	// Apply node selector from config.
	if len(cfg.Kubernetes.NodeSelector) > 0 {
		job.Spec.Template.Spec.NodeSelector = cfg.Kubernetes.NodeSelector
	}

	// Apply service account.
	if cfg.Kubernetes.ServiceAccount != "" {
		job.Spec.Template.Spec.ServiceAccountName = cfg.Kubernetes.ServiceAccount
	}

	// Apply image pull secrets.
	for _, secret := range cfg.Kubernetes.ImagePullSecrets {
		job.Spec.Template.Spec.ImagePullSecrets = append(
			job.Spec.Template.Spec.ImagePullSecrets,
			corev1.LocalObjectReference{Name: secret},
		)
	}

	// Apply tolerations.
	for _, t := range cfg.Kubernetes.Tolerations {
		toleration := corev1.Toleration{
			Key:      t.Key,
			Operator: corev1.TolerationOperator(t.Operator),
			Value:    t.Value,
			Effect:   corev1.TaintEffect(t.Effect),
		}
		if t.TolerationSeconds != nil {
			toleration.TolerationSeconds = t.TolerationSeconds
		}
		job.Spec.Template.Spec.Tolerations = append(job.Spec.Template.Spec.Tolerations, toleration)
	}

	// Create the Job first so the install-script ConfigMap can be owned by it
	// and garbage-collected with it.
	createdJob, err := ip.client.BatchV1().Jobs(ip.namespace).Create(ctx, job, metav1.CreateOptions{})
	if err != nil {
		return errors.Wrap(err, "environment/kubernetes: failed to create installer job")
	}

	// Create the script ConfigMap with an ownerReference to the Job. The Job's
	// pod may sit in ContainerCreating until the ConfigMap exists; that is
	// expected.
	if err := ip.writeInstallScript(ctx, createdJob); err != nil {
		return err
	}

	// Stream logs in the background.
	go ip.streamJobLogs(ctx)

	// Wait for the Job to complete.
	if err := ip.waitForJob(ctx); err != nil {
		return err
	}

	return nil
}

// streamJobLogs finds the Pod created by the Job and streams its logs to the
// install sink.
func (ip *InstallerProcess) streamJobLogs(ctx context.Context) {
	// Wait a moment for the Pod to be created by the Job controller.
	time.Sleep(2 * time.Second)

	for i := 0; i < 30; i++ {
		pods, err := ip.client.CoreV1().Pods(ip.namespace).List(ctx, metav1.ListOptions{
			LabelSelector: fmt.Sprintf("job-name=%s", ip.jobName()),
		})
		if err != nil || len(pods.Items) == 0 {
			time.Sleep(2 * time.Second)
			continue
		}

		pod := pods.Items[0]
		// Wait for the pod to be running or completed.
		if pod.Status.Phase == corev1.PodPending {
			time.Sleep(2 * time.Second)
			continue
		}

		follow := pod.Status.Phase == corev1.PodRunning
		stream, err := ip.client.CoreV1().Pods(ip.namespace).GetLogs(pod.Name, &corev1.PodLogOptions{
			Container: "installer",
			Follow:    follow,
		}).Stream(ctx)
		if err != nil {
			return
		}
		defer stream.Close()

		scanner := bufio.NewScanner(stream)
		for scanner.Scan() {
			line := scanner.Text()
			if ip.Sink != nil {
				ip.Sink.Push([]byte(line))
			}
		}
		return
	}
}

// waitForJob watches the Job until it succeeds, fails, or the context is
// canceled. The watch is re-established when the apiserver closes the result
// channel (e.g. on watch timeouts) until the overall 30-minute deadline.
func (ip *InstallerProcess) waitForJob(ctx context.Context) error {
	dctx, cancel := context.WithTimeout(ctx, 30*time.Minute)
	defer cancel()

	err := watchUntil(dctx,
		func(ctx context.Context) (runtime.Object, error) {
			job, err := ip.client.BatchV1().Jobs(ip.namespace).Get(ctx, ip.jobName(), metav1.GetOptions{})
			if err != nil {
				return nil, errors.Wrap(err, "environment/kubernetes: failed to get installer job status")
			}
			return job, nil
		},
		func(ctx context.Context) (watch.Interface, error) {
			w, err := ip.client.BatchV1().Jobs(ip.namespace).Watch(ctx, metav1.ListOptions{
				FieldSelector: "metadata.name=" + ip.jobName(),
			})
			if err != nil {
				return nil, errors.Wrap(err, "environment/kubernetes: failed to watch installer job")
			}
			return w, nil
		},
		func(evt watch.EventType, obj runtime.Object) (bool, error) {
			if evt == watch.Deleted {
				return true, errors.New("environment/kubernetes: installer job was deleted before completion")
			}
			job, ok := obj.(*batchv1.Job)
			if !ok {
				return false, nil
			}
			return jobTerminal(job)
		},
	)
	if err != nil && ctx.Err() == nil && errors.Is(err, context.DeadlineExceeded) {
		return errors.New("environment/kubernetes: installer job timed out after 30 minutes")
	}
	return err
}

// jobTerminal reports whether the Job has reached a terminal state: true with
// a nil error on success, true with an error on failure.
func jobTerminal(job *batchv1.Job) (bool, error) {
	if job.Status.Succeeded > 0 {
		return true, nil
	}
	if job.Status.Failed > 0 {
		return true, errors.New("environment/kubernetes: installer job failed")
	}
	for _, c := range job.Status.Conditions {
		if c.Type == batchv1.JobFailed && c.Status == corev1.ConditionTrue {
			return true, errors.New("environment/kubernetes: installer job failed")
		}
	}
	return false, nil
}

// Cleanup removes the installer Job and associated resources. The
// install-script ConfigMap is owned by the Job and garbage-collected with it.
func (ip *InstallerProcess) Cleanup(ctx context.Context) error {
	err := ip.client.BatchV1().Jobs(ip.namespace).Delete(ctx, ip.jobName(), metav1.DeleteOptions{
		PropagationPolicy: propagationBackground(),
	})
	if err != nil && !isNotFound(err) {
		return errors.Wrap(err, "environment/kubernetes: failed to delete installer job")
	}

	// The install script ConfigMap is owned by the Job and is removed by
	// background garbage collection when the Job is deleted.

	// Remove temporary install script directory (legacy cleanup).
	if ip.TmpDir != "" {
		os.RemoveAll(ip.TmpDir)
	}

	return nil
}

// writeInstallScript creates the install-script ConfigMap with an
// ownerReference to the given Job so it is garbage-collected together with
// it. The ConfigMap is mounted into the Job Pod at /mnt/install, eliminating
// the need for a shared filesystem between Wings and the Job Pod.
func (ip *InstallerProcess) writeInstallScript(ctx context.Context, job *batchv1.Job) error {
	content := strings.ReplaceAll(ip.Script.Script, "\r\n", "\n")

	cm := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{
			Name:      ip.configMapName(),
			Namespace: ip.namespace,
			Labels: map[string]string{
				"app.kubernetes.io/managed-by": "pelican-wings",
				"pelican.dev/server-id":        ip.ServerID,
				"pelican.dev/container-type":   "server_installer",
			},
		},
		Data: map[string]string{
			"install.sh": content,
		},
	}

	controller, blockOwnerDeletion := true, true
	cm.OwnerReferences = []metav1.OwnerReference{
		{
			APIVersion:         "batch/v1",
			Kind:               "Job",
			Name:               job.Name,
			UID:                job.UID,
			Controller:         &controller,
			BlockOwnerDeletion: &blockOwnerDeletion,
		},
	}

	// Delete any existing ConfigMap from a previous run.
	_ = ip.client.CoreV1().ConfigMaps(ip.namespace).Delete(ctx, ip.configMapName(), metav1.DeleteOptions{})

	_, err := ip.client.CoreV1().ConfigMaps(ip.namespace).Create(ctx, cm, metav1.CreateOptions{})
	if err != nil {
		return errors.Wrap(err, "environment/kubernetes: failed to create install script configmap")
	}
	return nil
}

// buildJobVolumes constructs the volume list for the installer Job. When
// DataPVC is set, the shared Wings data PVC is used. Otherwise falls back to
// per-server PVC (StorageMode "pvc") or HostPath. The install script is always
// mounted from a ConfigMap.
func (ip *InstallerProcess) buildJobVolumes(cfg *config.Configuration) []corev1.Volume {
	var serverDataSource corev1.VolumeSource
	if cfg.Kubernetes.DataPVC != "" {
		serverDataSource = corev1.VolumeSource{
			PersistentVolumeClaim: &corev1.PersistentVolumeClaimVolumeSource{
				ClaimName: cfg.Kubernetes.DataPVC,
			},
		}
	} else if cfg.Kubernetes.StorageMode == config.KubeStoragePVC {
		serverDataSource = corev1.VolumeSource{
			PersistentVolumeClaim: &corev1.PersistentVolumeClaimVolumeSource{
				ClaimName: fmt.Sprintf("gs-%s", ip.ServerID),
			},
		}
	} else {
		serverDataSource = corev1.VolumeSource{
			HostPath: &corev1.HostPathVolumeSource{
				Path: ip.ServerPath,
			},
		}
	}

	// Script volume uses ConfigMap — works across multi-node clusters.
	defaultMode := int32(0755)

	return []corev1.Volume{
		{
			Name:         "server-data",
			VolumeSource: serverDataSource,
		},
		{
			Name: "install-script",
			VolumeSource: corev1.VolumeSource{
				ConfigMap: &corev1.ConfigMapVolumeSource{
					LocalObjectReference: corev1.LocalObjectReference{
						Name: ip.configMapName(),
					},
					DefaultMode: &defaultMode,
				},
			},
		},
	}
}

// buildServerDataMount returns the VolumeMount for the server-data volume.
// When DataPVC is set, the mount includes a subPath to the server's data
// directory within the shared PVC.
func (ip *InstallerProcess) buildServerDataMount() corev1.VolumeMount {
	vm := corev1.VolumeMount{
		Name:      "server-data",
		MountPath: "/mnt/server",
	}
	cfg := config.Get()
	if cfg.Kubernetes.DataPVC != "" {
		serverPath := filepath.Join(cfg.System.Data, ip.ServerID)
		rel, err := filepath.Rel(cfg.System.RootDirectory, serverPath)
		// filepath.Rel can yield a path escaping the volume root ("..") when
		// System.Data is outside RootDirectory; such a SubPath is rejected by
		// Kubernetes, so fall back to the default layout in that case.
		if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
			vm.SubPath = filepath.Join("volumes", ip.ServerID)
		} else {
			vm.SubPath = rel
		}
	}
	return vm
}

// waitForJobDeletion blocks until the installer Job is fully deleted or the
// timeout elapses.
func (ip *InstallerProcess) waitForJobDeletion(ctx context.Context, timeout time.Duration) error {
	dctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	return watchUntil(dctx,
		func(ctx context.Context) (runtime.Object, error) {
			job, err := ip.client.BatchV1().Jobs(ip.namespace).Get(ctx, ip.jobName(), metav1.GetOptions{})
			if err != nil {
				if isNotFound(err) {
					// Already gone; signal done via a nil object.
					return nil, nil
				}
				return nil, errors.Wrap(err, "environment/kubernetes: failed to get installer job")
			}
			return job, nil
		},
		func(ctx context.Context) (watch.Interface, error) {
			w, err := ip.client.BatchV1().Jobs(ip.namespace).Watch(ctx, metav1.ListOptions{
				FieldSelector: "metadata.name=" + ip.jobName(),
			})
			if err != nil {
				return nil, errors.Wrap(err, "environment/kubernetes: failed to watch installer job")
			}
			return w, nil
		},
		func(evt watch.EventType, obj runtime.Object) (bool, error) {
			return obj == nil || evt == watch.Deleted, nil
		},
	)
}

// propagationForeground returns a pointer to the Foreground propagation policy.
func propagationForeground() *metav1.DeletionPropagation {
	p := metav1.DeletePropagationForeground
	return &p
}

// propagationBackground returns a pointer to the Background propagation policy.
func propagationBackground() *metav1.DeletionPropagation {
	p := metav1.DeletePropagationBackground
	return &p
}
