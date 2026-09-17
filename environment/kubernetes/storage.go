package kubernetes

import (
	"context"
	"fmt"

	"emperror.dev/errors"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/pelican/wings/config"
)

// pvcName returns the PersistentVolumeClaim name for this server.
func (e *Environment) pvcName() string {
	return fmt.Sprintf("gs-%s", e.Id)
}

// EnsurePVC creates the PersistentVolumeClaim for this server if it does not
// already exist. When StorageMode is "hostpath" or DataPVC is set (shared PVC
// mode), this is a no-op.
func (e *Environment) EnsurePVC(ctx context.Context) error {
	cfg := config.Get()
	if cfg.Kubernetes.StorageMode != config.KubeStoragePVC {
		return nil
	}
	// Shared PVC mode: the Wings data PVC is already mounted. No per-server
	// PVC is needed; the server data directory is a subPath of the shared PVC.
	if cfg.Kubernetes.DataPVC != "" {
		return nil
	}

	ns := e.namespace()
	name := e.pvcName()

	// Check if PVC already exists.
	_, err := e.client.CoreV1().PersistentVolumeClaims(ns).Get(ctx, name, metav1.GetOptions{})
	if err == nil {
		return nil // Already exists.
	}
	if !isNotFound(err) {
		return errors.Wrap(err, "environment/kubernetes: failed to check existing PVC")
	}

	// Parse storage size.
	storageSize, err := resource.ParseQuantity(cfg.Kubernetes.StorageSize)
	if err != nil {
		return errors.Wrap(err, "environment/kubernetes: invalid storage_size in config")
	}

	// Determine access mode.
	accessMode := corev1.ReadWriteOnce
	if cfg.Kubernetes.StorageAccessMode == "ReadWriteMany" {
		accessMode = corev1.ReadWriteMany
	}

	pvc := &corev1.PersistentVolumeClaim{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: ns,
			Labels: map[string]string{
				"app.kubernetes.io/managed-by": "pelican-wings",
				"pelican.dev/server-id":        e.Id,
			},
		},
		Spec: corev1.PersistentVolumeClaimSpec{
			AccessModes: []corev1.PersistentVolumeAccessMode{accessMode},
			Resources: corev1.VolumeResourceRequirements{
				Requests: corev1.ResourceList{
					corev1.ResourceStorage: storageSize,
				},
			},
		},
	}

	// Set storage class if configured.
	if cfg.Kubernetes.StorageClass != "" {
		pvc.Spec.StorageClassName = &cfg.Kubernetes.StorageClass
	}

	e.log().WithField("pvc", name).Info("creating PersistentVolumeClaim for server")
	_, err = e.client.CoreV1().PersistentVolumeClaims(ns).Create(ctx, pvc, metav1.CreateOptions{})
	if err != nil {
		return errors.Wrap(err, "environment/kubernetes: failed to create PVC")
	}

	return nil
}

// DeletePVC removes the PersistentVolumeClaim for this server. This
// permanently deletes the server's data. When StorageMode is "hostpath" or
// DataPVC is set (shared PVC mode), this is a no-op.
func (e *Environment) DeletePVC(ctx context.Context) error {
	cfg := config.Get()
	if cfg.Kubernetes.StorageMode != config.KubeStoragePVC {
		return nil
	}
	if cfg.Kubernetes.DataPVC != "" {
		return nil
	}

	ns := e.namespace()
	name := e.pvcName()

	err := e.client.CoreV1().PersistentVolumeClaims(ns).Delete(ctx, name, metav1.DeleteOptions{})
	if err != nil && !isNotFound(err) {
		return errors.Wrap(err, "environment/kubernetes: failed to delete PVC")
	}

	if err == nil {
		e.log().WithField("pvc", name).Info("deleted PersistentVolumeClaim for server")
	}

	return nil
}

// GetPVCStatus returns the current status of the server's PVC, or nil if not
// in PVC mode or if the PVC doesn't exist.
func (e *Environment) GetPVCStatus(ctx context.Context) (*corev1.PersistentVolumeClaimPhase, error) {
	cfg := config.Get()
	if cfg.Kubernetes.StorageMode != config.KubeStoragePVC {
		return nil, nil
	}

	ns := e.namespace()
	name := e.pvcName()

	pvc, err := e.client.CoreV1().PersistentVolumeClaims(ns).Get(ctx, name, metav1.GetOptions{})
	if err != nil {
		if isNotFound(err) {
			return nil, nil
		}
		return nil, errors.Wrap(err, "environment/kubernetes: failed to get PVC status")
	}

	return &pvc.Status.Phase, nil
}

// ResizePVC updates the storage request on the PVC to the given size. The
// underlying StorageClass must support volume expansion.
func (e *Environment) ResizePVC(ctx context.Context, newSize string) error {
	cfg := config.Get()
	if cfg.Kubernetes.StorageMode != config.KubeStoragePVC {
		return nil
	}
	// When a shared DataPVC is configured, per-server PVCs are not created, so
	// there is nothing to resize (mirrors EnsurePVC/DeletePVC).
	if cfg.Kubernetes.DataPVC != "" {
		return nil
	}

	ns := e.namespace()
	name := e.pvcName()

	quantity, err := resource.ParseQuantity(newSize)
	if err != nil {
		return errors.Wrap(err, "environment/kubernetes: invalid size for PVC resize")
	}

	pvc, err := e.client.CoreV1().PersistentVolumeClaims(ns).Get(ctx, name, metav1.GetOptions{})
	if err != nil {
		return errors.Wrap(err, "environment/kubernetes: failed to get PVC for resize")
	}

	pvc.Spec.Resources.Requests[corev1.ResourceStorage] = quantity

	_, err = e.client.CoreV1().PersistentVolumeClaims(ns).Update(ctx, pvc, metav1.UpdateOptions{})
	if err != nil {
		return errors.Wrap(err, "environment/kubernetes: failed to resize PVC")
	}

	e.log().WithField("pvc", name).WithField("new_size", newSize).Info("resized PersistentVolumeClaim")
	return nil
}
