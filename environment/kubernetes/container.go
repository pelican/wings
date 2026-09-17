package kubernetes

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"path/filepath"
	"strings"

	"emperror.dev/errors"
	"github.com/apex/log"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
	"k8s.io/client-go/tools/remotecommand"

	"github.com/pelican/wings/config"
	"github.com/pelican/wings/environment"
	"github.com/pelican/wings/system"
)

// resolveImagePullPolicy returns the cleaned image reference (without the ~
// local-image prefix) and the pull policy to apply. It mirrors the Docker
// backend, which always attempts to pull remote images before starting a
// server so updated tags are picked up, while never pulling ~-prefixed local
// images. A non-empty kubernetes.image_pull_policy config value overrides the
// default (useful for air-gapped clusters).
func resolveImagePullPolicy(image string) (string, corev1.PullPolicy) {
	cleaned := strings.TrimPrefix(image, "~")
	if override := config.Get().Kubernetes.ImagePullPolicy; override != "" {
		return cleaned, corev1.PullPolicy(override)
	}
	if strings.HasPrefix(image, "~") {
		return cleaned, corev1.PullIfNotPresent
	}
	return cleaned, corev1.PullAlways
}

// Create builds and creates the Pod for this game server in Kubernetes.
// If the Pod already exists this is a no-op.
func (e *Environment) Create() error {
	ctx := context.Background()

	// If the Pod already exists, return immediately.
	exists, err := e.Exists()
	if err != nil {
		return errors.Wrap(err, "environment/kubernetes: failed to check pod existence")
	}
	if exists {
		return nil
	}

	cfg := config.Get()
	limits := e.Configuration.Limits()
	evs := e.Configuration.EnvironmentVariables()
	mounts := e.Configuration.Mounts()

	// Build environment variables for the container.
	var envVars []corev1.EnvVar
	for _, ev := range evs {
		parts := strings.SplitN(ev, "=", 2)
		if len(parts) == 2 {
			envVars = append(envVars, corev1.EnvVar{
				Name:  parts[0],
				Value: parts[1],
			})
		}
	}

	// Build resource requirements from server limits.
	resources := e.buildResources(limits)

	// Build volume mounts and volumes. In K8s mode, identity files (passwd,
	// group, machine-id) are served from a ConfigMap instead of hostPath.
	identityMounts, regularMounts := e.splitIdentityMounts(mounts)
	volumes, volumeMounts := e.buildVolumes(regularMounts)

	if len(identityMounts) > 0 {
		idVol, idMounts, err := e.ensureIdentityConfigMap(ctx, identityMounts)
		if err != nil {
			return errors.Wrap(err, "environment/kubernetes: failed to create identity ConfigMap")
		}
		volumes = append(volumes, idVol)
		volumeMounts = append(volumeMounts, idMounts...)
	}

	// Determine image and pull policy (mirrors the Docker backend).
	image, pullPolicy := resolveImagePullPolicy(e.meta.Image)

	// Build container ports from allocations.
	containerPorts := e.buildContainerPorts()

	// Labels for identification.
	labels := map[string]string{
		"app.kubernetes.io/managed-by": "pelican-wings",
		"pelican.dev/server-id":        e.Id,
		"pelican.dev/container-type":   "server_process",
	}

	// Merge user-provided labels.
	for k, v := range e.Configuration.Labels() {
		labels[k] = v
	}

	// Determine the UID/GID for the pelican user.
	uid := int64(config.Get().System.User.Uid)
	gid := int64(config.Get().System.User.Gid)

	// Build the Pod spec.
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      e.Id,
			Namespace: e.namespace(),
			Labels:    labels,
		},
		Spec: corev1.PodSpec{
			RestartPolicy: corev1.RestartPolicyNever,
			SecurityContext: &corev1.PodSecurityContext{
				RunAsUser:  &uid,
				RunAsGroup: &gid,
				FSGroup:    &gid,
			},
			InitContainers: []corev1.Container{
				{
					Name:    "set-permissions",
					Image:   "busybox:1.36",
					Command: []string{"sh", "-c", fmt.Sprintf("chown -R %d:%d /home/container && chmod -R 755 /home/container", uid, gid)},
					VolumeMounts: []corev1.VolumeMount{
						{
							Name:      "server-data",
							MountPath: "/home/container",
							SubPath:   e.serverDataSubPathIfShared(),
						},
					},
					SecurityContext: &corev1.SecurityContext{
						RunAsUser:  func() *int64 { v := int64(0); return &v }(),
						RunAsGroup: func() *int64 { v := int64(0); return &v }(),
					},
				},
			},
			Containers: []corev1.Container{
				{
					Name:            "server",
					Image:           image,
					Env:             envVars,
					Resources:       resources,
					Ports:           containerPorts,
					VolumeMounts:    volumeMounts,
					Stdin:           true,
					TTY:             true,
					ImagePullPolicy: pullPolicy,
				},
			},
			Volumes: volumes,
		},
	}

	// Apply network mode configuration.
	if cfg.Kubernetes.NetworkMode == config.KubeNetworkHostPort {
		pod.Spec.HostNetwork = false // hostPort is set on containerPorts
	}

	// Apply DNS policy.
	if cfg.Kubernetes.DNSPolicy != "" {
		pod.Spec.DNSPolicy = corev1.DNSPolicy(cfg.Kubernetes.DNSPolicy)
	}

	// Pin the Pod to the Wings node when it depends on node-local resources
	// (HostPath storage, hostPort networking, or HostPath mounts).
	if requiresNodePinning(cfg, mounts) {
		nodeName := resolveNodeName()
		if nodeName == "" {
			return errors.New("environment/kubernetes: node pinning required (hostpath storage / hostport networking / hostPath mounts) but node name is unknown; set kubernetes.node_name or the NODE_NAME env var")
		}
		pod.Spec.Affinity = nodeAffinityFor(nodeName)
	}

	// Apply node selector.
	if len(cfg.Kubernetes.NodeSelector) > 0 {
		pod.Spec.NodeSelector = cfg.Kubernetes.NodeSelector
	}

	// Apply service account.
	if cfg.Kubernetes.ServiceAccount != "" {
		pod.Spec.ServiceAccountName = cfg.Kubernetes.ServiceAccount
	}

	// Apply image pull secrets.
	for _, secret := range cfg.Kubernetes.ImagePullSecrets {
		pod.Spec.ImagePullSecrets = append(pod.Spec.ImagePullSecrets, corev1.LocalObjectReference{
			Name: secret,
		})
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
		pod.Spec.Tolerations = append(pod.Spec.Tolerations, toleration)
	}

	// Ensure namespace-level resource constraints are applied.
	if err := e.EnsureResourceQuota(ctx); err != nil {
		e.log().WithField("error", err).Warn("failed to ensure ResourceQuota")
	}
	if err := e.EnsureLimitRange(ctx); err != nil {
		e.log().WithField("error", err).Warn("failed to ensure LimitRange")
	}

	// Ensure the PVC exists if using PVC storage mode.
	if err := e.EnsurePVC(ctx); err != nil {
		return errors.Wrap(err, "environment/kubernetes: failed to ensure PVC")
	}

	e.log().WithField("image", image).Info("creating pod for server")

	_, err = e.client.CoreV1().Pods(e.namespace()).Create(ctx, pod, metav1.CreateOptions{})
	if err != nil {
		return errors.Wrap(err, "environment/kubernetes: failed to create pod")
	}

	// Create or update the NodePort Service if using NodePort network mode.
	if err := e.EnsureService(ctx); err != nil {
		e.log().WithField("error", err).Warn("failed to create/update NodePort service")
	}

	return nil
}

// Destroy removes the Pod and associated Service from Kubernetes.
func (e *Environment) Destroy() error {
	e.SetState(environment.ProcessStoppingState)

	ctx := context.Background()

	gracePeriod := int64(0)
	err := e.client.CoreV1().Pods(e.namespace()).Delete(
		ctx,
		e.Id,
		metav1.DeleteOptions{GracePeriodSeconds: &gracePeriod},
	)

	// Clean up the associated NodePort Service.
	if svcErr := e.DeleteService(ctx); svcErr != nil {
		e.log().WithField("error", svcErr).Warn("failed to delete NodePort service during destroy")
	}

	// Clean up the PVC if using PVC storage mode.
	if pvcErr := e.DeletePVC(ctx); pvcErr != nil {
		e.log().WithField("error", pvcErr).Warn("failed to delete PVC during destroy")
	}

	// Clean up the identity ConfigMap.
	if cmErr := e.deleteIdentityConfigMap(ctx); cmErr != nil {
		e.log().WithField("error", cmErr).Warn("failed to delete identity ConfigMap during destroy")
	}

	if err != nil && !isNotFound(err) {
		return errors.Wrap(err, "environment/kubernetes: failed to delete pod")
	}

	e.SetState(environment.ProcessOfflineState)

	return nil
}

// Attach connects to the running Pod's stdin/stdout and begins streaming
// output to the log callback. This should be called before the server starts
// producing output.
func (e *Environment) Attach(ctx context.Context) error {
	if e.IsAttached() {
		return nil
	}

	restCfg, err := e.getRESTConfig()
	if err != nil {
		return errors.WrapIf(err, "environment/kubernetes: failed to get REST config")
	}

	req := e.client.CoreV1().RESTClient().Post().
		Resource("pods").
		Name(e.Id).
		Namespace(e.namespace()).
		SubResource("attach").
		VersionedParams(&corev1.PodAttachOptions{
			Container: "server",
			Stdin:     true,
			Stdout:    true,
			Stderr:    true,
			TTY:       true,
		}, scheme.ParameterCodec)

	exec, err := remotecommand.NewSPDYExecutor(restCfg, "POST", req.URL())
	if err != nil {
		return errors.WrapIf(err, "environment/kubernetes: failed to create SPDY executor")
	}

	// Create a pipe for stdin.
	stdinReader, stdinWriter := io.Pipe()
	e.setStream(stdinWriter)

	go func() {
		defer e.setStream(nil)
		defer func() {
			e.SetState(environment.ProcessOfflineState)
		}()

		// Start resource polling.
		pollCtx, cancel := context.WithCancel(context.Background())
		defer cancel()

		go func() {
			if err := e.pollResources(pollCtx); err != nil {
				if !errors.Is(err, context.Canceled) {
					e.log().WithField("error", err).Error("error during resource polling")
				}
			}
		}()

		// Stream stdout/stderr to the log callback via a pipe.
		stdoutReader, stdoutWriter := io.Pipe()

		go func() {
			if err := system.ScanReader(stdoutReader, func(v []byte) {
				e.logCallbackMx.Lock()
				defer e.logCallbackMx.Unlock()
				if e.logCallback != nil {
					e.logCallback(v)
				}
			}); err != nil && err != io.EOF {
				log.WithField("error", err).WithField("pod_id", e.Id).Warn("error processing output in console")
			}
		}()

		streamErr := exec.StreamWithContext(ctx, remotecommand.StreamOptions{
			Stdin:  stdinReader,
			Stdout: stdoutWriter,
			Stderr: stdoutWriter,
			Tty:    true,
		})
		stdoutWriter.Close()
		stdinReader.Close()

		if streamErr != nil && !errors.Is(streamErr, context.Canceled) {
			e.log().WithField("error", streamErr).Warn("pod attach stream ended")
		}
	}()

	return nil
}

// SendCommand writes a command string to the attached Pod's stdin.
func (e *Environment) SendCommand(c string) error {
	e.mu.RLock()
	defer e.mu.RUnlock()

	// Check the stream under the lock so it cannot be cleared between an
	// attachment check and the write below.
	if e.stream == nil {
		return errors.New("environment/kubernetes: not attached to pod")
	}

	// If this is the stop command, mark the server as stopping.
	if e.meta.Stop.Type == "command" && c == e.meta.Stop.Value {
		e.SetState(environment.ProcessStoppingState)
	}

	_, err := e.stream.Write([]byte(c + "\n"))
	return errors.Wrap(err, "environment/kubernetes: could not write to pod stream")
}

// Readlog reads the last N lines of log output from the Pod.
func (e *Environment) Readlog(lines int) ([]string, error) {
	tailLines := int64(lines)
	opts := &corev1.PodLogOptions{
		Container: "server",
		TailLines: &tailLines,
	}

	req := e.client.CoreV1().Pods(e.namespace()).GetLogs(e.Id, opts)
	stream, err := req.Stream(context.Background())
	if err != nil {
		return nil, errors.WithStack(err)
	}
	defer stream.Close()

	var out []string
	scanner := bufio.NewScanner(stream)
	for scanner.Scan() {
		out = append(out, scanner.Text())
	}

	return out, nil
}

// buildResources converts Wings Limits into Kubernetes resource requirements.
func (e *Environment) buildResources(limits environment.Limits) corev1.ResourceRequirements {
	resources := corev1.ResourceRequirements{
		Limits:   corev1.ResourceList{},
		Requests: corev1.ResourceList{},
	}

	// Memory: limits.MemoryLimit is in MiB.
	if limits.MemoryLimit > 0 {
		mem := resource.MustParse(fmt.Sprintf("%dMi", limits.MemoryLimit))
		resources.Limits[corev1.ResourceMemory] = mem
		// Request 50% of the limit as a baseline.
		memReq := resource.MustParse(fmt.Sprintf("%dMi", limits.MemoryLimit/2))
		resources.Requests[corev1.ResourceMemory] = memReq
	}

	// CPU: limits.CpuLimit is a percentage (100 = 1 core).
	if limits.CpuLimit > 0 {
		cpuMillis := limits.CpuLimit * 10 // 100% = 1000m
		cpu := resource.MustParse(fmt.Sprintf("%dm", cpuMillis))
		resources.Limits[corev1.ResourceCPU] = cpu
		// Request 25% of the limit.
		cpuReqMillis := cpuMillis / 4
		if cpuReqMillis < 50 {
			cpuReqMillis = 50
		}
		cpuReq := resource.MustParse(fmt.Sprintf("%dm", cpuReqMillis))
		resources.Requests[corev1.ResourceCPU] = cpuReq
	}

	return resources
}

// buildVolumes constructs Kubernetes volumes and mounts from the environment
// mount configuration. When StorageMode is "pvc", the default server-data
// volume uses a PersistentVolumeClaim instead of a HostPath.
//
// When DataPVC is set, the default mount uses the shared Wings data PVC with
// a subPath so that Wings and game server Pods share the same storage. This
// enables the Panel file browser and SFTP to work in K8s mode.
func (e *Environment) buildVolumes(mounts []environment.Mount) ([]corev1.Volume, []corev1.VolumeMount) {
	cfg := config.Get()
	var volumes []corev1.Volume
	var volumeMounts []corev1.VolumeMount

	for i, m := range mounts {
		volName := fmt.Sprintf("mount-%d", i)
		if m.Default {
			volName = "server-data"
		}

		var volSource corev1.VolumeSource
		var subPath string

		if m.Default && cfg.Kubernetes.DataPVC != "" {
			// Shared PVC mode: mount the Wings data PVC with a subPath
			// so both Wings and the game server see the same files.
			volSource = corev1.VolumeSource{
				PersistentVolumeClaim: &corev1.PersistentVolumeClaimVolumeSource{
					ClaimName: cfg.Kubernetes.DataPVC,
				},
			}
			subPath = e.serverDataSubPath()
		} else if m.Default && cfg.Kubernetes.StorageMode == config.KubeStoragePVC {
			// Per-server PVC mode (original behavior).
			volSource = corev1.VolumeSource{
				PersistentVolumeClaim: &corev1.PersistentVolumeClaimVolumeSource{
					ClaimName: e.pvcName(),
				},
			}
		} else {
			// Use HostPath for non-default mounts or when in hostpath mode.
			volSource = corev1.VolumeSource{
				HostPath: &corev1.HostPathVolumeSource{
					Path: m.Source,
				},
			}
		}

		volumes = append(volumes, corev1.Volume{
			Name:         volName,
			VolumeSource: volSource,
		})

		vm := corev1.VolumeMount{
			Name:      volName,
			MountPath: m.Target,
			ReadOnly:  m.ReadOnly,
		}
		if subPath != "" {
			vm.SubPath = subPath
		}
		volumeMounts = append(volumeMounts, vm)
	}

	return volumes, volumeMounts
}

// serverDataSubPath returns the subPath within the shared data PVC for this
// server's data directory. This is the relative path from the Wings root
// directory to the server's data directory.
func (e *Environment) serverDataSubPath() string {
	cfg := config.Get()
	serverPath := filepath.Join(cfg.System.Data, e.Id)
	rel, err := filepath.Rel(cfg.System.RootDirectory, serverPath)
	if err != nil {
		return filepath.Join("volumes", e.Id)
	}
	return rel
}

// serverDataSubPathIfShared returns the subPath for shared PVC mode, or an
// empty string if not using shared PVC mode.
func (e *Environment) serverDataSubPathIfShared() string {
	if config.Get().Kubernetes.DataPVC != "" {
		return e.serverDataSubPath()
	}
	return ""
}

// buildContainerPorts creates the container port list from allocations.
func (e *Environment) buildContainerPorts() []corev1.ContainerPort {
	cfg := config.Get()
	allocs := e.Configuration.Allocations()
	var ports []corev1.ContainerPort
	seen := make(map[int]struct{})

	for _, allocPorts := range allocs.Mappings {
		for _, port := range allocPorts {
			if port < 1 || port > 65535 {
				continue
			}
			if _, ok := seen[port]; ok {
				continue
			}
			seen[port] = struct{}{}

			tcpPort := corev1.ContainerPort{
				Name:          fmt.Sprintf("tcp-%d", port),
				ContainerPort: int32(port),
				Protocol:      corev1.ProtocolTCP,
			}
			udpPort := corev1.ContainerPort{
				Name:          fmt.Sprintf("udp-%d", port),
				ContainerPort: int32(port),
				Protocol:      corev1.ProtocolUDP,
			}

			// If using hostPort mode, bind directly to the host.
			if cfg.Kubernetes.NetworkMode == config.KubeNetworkHostPort {
				tcpPort.HostPort = int32(port)
				udpPort.HostPort = int32(port)
			}

			ports = append(ports, tcpPort, udpPort)
		}
	}

	return ports
}

// getRESTConfig returns the rest.Config used for exec/attach operations.
func (e *Environment) getRESTConfig() (*rest.Config, error) {
	kubeconfig := config.Get().Kubernetes.Kubeconfig
	if kubeconfig != "" {
		return clientcmd.BuildConfigFromFlags("", kubeconfig)
	}
	return rest.InClusterConfig()
}

// getPod fetches the Pod object from the Kubernetes API.
func (e *Environment) getPod(ctx context.Context) (*corev1.Pod, error) {
	return e.client.CoreV1().Pods(e.namespace()).Get(ctx, e.Id, metav1.GetOptions{})
}

// isNotFound checks if the error is a Kubernetes NotFound error.
func isNotFound(err error) bool {
	return apierrors.IsNotFound(err)
}

// isPodRunning checks if a Pod is in Running phase with a ready container.
func isPodRunning(pod *corev1.Pod) bool {
	if pod.Status.Phase != corev1.PodRunning {
		return false
	}
	for _, cs := range pod.Status.ContainerStatuses {
		if cs.Name == "server" && cs.State.Running != nil {
			return true
		}
	}
	return false
}
