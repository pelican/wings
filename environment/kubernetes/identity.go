package kubernetes

import (
	"context"
	"fmt"
	"os"
	"strings"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	corev1apply "k8s.io/client-go/applyconfigurations/core/v1"

	"github.com/pelican/wings/config"
	"github.com/pelican/wings/environment"
)

// identityConfigMapName returns the name for the identity ConfigMap.
func (e *Environment) identityConfigMapName() string {
	return fmt.Sprintf("gs-%s-identity", e.Id)
}

// isIdentityMount returns true if the mount targets one of the well-known
// identity file paths (/etc/passwd, /etc/group, /etc/machine-id).
func isIdentityMount(m environment.Mount) bool {
	switch m.Target {
	case "/etc/passwd", "/etc/group", "/etc/machine-id":
		return true
	}
	return false
}

// splitIdentityMounts partitions mounts into identity mounts (passwd, group,
// machine-id) and everything else. In Kubernetes mode, identity mounts are
// served from a ConfigMap instead of hostPath.
func (e *Environment) splitIdentityMounts(mounts []environment.Mount) (identity, regular []environment.Mount) {
	if !config.Get().Kubernetes.Enabled {
		return nil, mounts
	}
	for _, m := range mounts {
		if isIdentityMount(m) {
			identity = append(identity, m)
		} else {
			regular = append(regular, m)
		}
	}
	return
}

// ensureIdentityConfigMap creates or updates a ConfigMap containing the
// identity files (passwd, group, machine-id) for this game server. It returns
// the Volume and VolumeMounts to be added to the Pod spec.
func (e *Environment) ensureIdentityConfigMap(ctx context.Context, mounts []environment.Mount) (corev1.Volume, []corev1.VolumeMount, error) {
	cmName := e.identityConfigMapName()
	ns := e.namespace()

	data := make(map[string]string)
	var volumeMounts []corev1.VolumeMount

	for _, m := range mounts {
		var key, content string
		switch m.Target {
		case "/etc/passwd":
			key = "passwd"
			content = e.readFileOrGenerate(m.Source, e.generatePasswd())
		case "/etc/group":
			key = "group"
			content = e.readFileOrGenerate(m.Source, e.generateGroup())
		case "/etc/machine-id":
			key = "machine-id"
			content = strings.ReplaceAll(e.Id, "-", "")
		default:
			continue
		}
		data[key] = content
		volumeMounts = append(volumeMounts, corev1.VolumeMount{
			Name:      "identity",
			MountPath: m.Target,
			SubPath:   key,
			ReadOnly:  true,
		})
	}

	apply := corev1apply.ConfigMap(cmName, ns).
		WithLabels(map[string]string{
			"app.kubernetes.io/managed-by": "pelican-wings",
			"pelican.dev/server-id":        e.Id,
			"pelican.dev/resource-type":    "identity",
		}).
		WithData(data)

	if _, err := e.client.CoreV1().ConfigMaps(ns).Apply(ctx, apply, applyOptions()); err != nil {
		return corev1.Volume{}, nil, err
	}

	vol := corev1.Volume{
		Name: "identity",
		VolumeSource: corev1.VolumeSource{
			ConfigMap: &corev1.ConfigMapVolumeSource{
				LocalObjectReference: corev1.LocalObjectReference{
					Name: cmName,
				},
			},
		},
	}

	return vol, volumeMounts, nil
}

// deleteIdentityConfigMap removes the identity ConfigMap for this server.
func (e *Environment) deleteIdentityConfigMap(ctx context.Context) error {
	err := e.client.CoreV1().ConfigMaps(e.namespace()).Delete(ctx, e.identityConfigMapName(), metav1.DeleteOptions{})
	if err != nil && !isNotFound(err) {
		return err
	}
	return nil
}

// readFileOrGenerate tries to read the file at path. If it doesn't exist
// (common on immutable OSes like Talos), returns the fallback content.
func (e *Environment) readFileOrGenerate(path, fallback string) string {
	data, err := os.ReadFile(path)
	if err != nil {
		return fallback
	}
	return string(data)
}

// generatePasswd returns a passwd file matching the format from
// config.ConfigurePasswd().
func (e *Environment) generatePasswd() string {
	cfg := config.Get()
	return fmt.Sprintf("container:x:%d:%d::/home/container:/usr/sbin/nologin",
		cfg.System.User.Uid, cfg.System.User.Gid)
}

// generateGroup returns a group file matching the format from
// config.ConfigurePasswd().
func (e *Environment) generateGroup() string {
	cfg := config.Get()
	return fmt.Sprintf("container:x:%d:container", cfg.System.User.Gid)
}
