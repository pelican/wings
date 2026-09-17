package kubernetes

import (
	"sync"

	"emperror.dev/errors"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"

	"github.com/pelican/wings/config"
)

var (
	_konce  sync.Once
	_client kubernetes.Interface
	_kerr   error
)

// Client returns a shared Kubernetes clientset. The client is created once
// and reused for all subsequent calls. It uses in-cluster config when no
// kubeconfig path is specified, falling back to the provided kubeconfig file.
func Client() (kubernetes.Interface, error) {
	_konce.Do(func() {
		var cfg *rest.Config
		kubeconfig := config.Get().Kubernetes.Kubeconfig
		if kubeconfig != "" {
			cfg, _kerr = clientcmd.BuildConfigFromFlags("", kubeconfig)
		} else {
			cfg, _kerr = rest.InClusterConfig()
		}
		if _kerr != nil {
			_kerr = errors.Wrap(_kerr, "environment/kubernetes: failed to build config")
			return
		}
		_client, _kerr = kubernetes.NewForConfig(cfg)
		if _kerr != nil {
			_kerr = errors.Wrap(_kerr, "environment/kubernetes: failed to create clientset")
		}
	})
	return _client, _kerr
}
