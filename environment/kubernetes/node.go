package kubernetes

import (
	"context"
	"os"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/pelican/wings/config"
	"github.com/pelican/wings/environment"
)

// resolveNodeName returns the name of the Kubernetes node Wings is running
// on, from config or the NODE_NAME environment variable (set via the
// Kubernetes downward API). Returns an empty string when unknown.
func resolveNodeName() string {
	nodeName := config.Get().Kubernetes.NodeName
	if nodeName == "" {
		nodeName = os.Getenv("NODE_NAME")
	}
	return nodeName
}

// requiresNodePinning reports whether game server workloads must be scheduled
// onto the node Wings runs on. That is required whenever a workload relies on
// node-local resources: HostPath volumes (the default storage mode, including
// any non-default mount) or hostPort networking.
func requiresNodePinning(cfg *config.Configuration, mounts []environment.Mount) bool {
	if cfg.Kubernetes.StorageMode != config.KubeStoragePVC {
		return true
	}
	if cfg.Kubernetes.NetworkMode == config.KubeNetworkHostPort {
		return true
	}
	for _, m := range mounts {
		if !m.Default {
			return true
		}
	}
	return false
}

// nodeAffinityFor returns a required node affinity pinning a Pod to the named
// node. Unlike spec.nodeName this still lets the scheduler enforce resource
// requests and taints.
func nodeAffinityFor(nodeName string) *corev1.Affinity {
	return &corev1.Affinity{
		NodeAffinity: &corev1.NodeAffinity{
			RequiredDuringSchedulingIgnoredDuringExecution: &corev1.NodeSelector{
				NodeSelectorTerms: []corev1.NodeSelectorTerm{
					{
						MatchExpressions: []corev1.NodeSelectorRequirement{
							{
								Key:      "kubernetes.io/hostname",
								Operator: corev1.NodeSelectorOpIn,
								Values:   []string{nodeName},
							},
						},
					},
				},
			},
		},
	}
}

// GetNodeIPs returns the IP addresses of the Kubernetes node where Wings is
// running. It queries the node's status.addresses for ExternalIP and
// InternalIP entries. The node name is read from the config or the NODE_NAME
// environment variable (set via the Kubernetes downward API).
func GetNodeIPs(ctx context.Context) ([]string, error) {
	nodeName := resolveNodeName()
	if nodeName == "" {
		return nil, nil
	}

	c, err := Client()
	if err != nil {
		return nil, err
	}

	node, err := c.CoreV1().Nodes().Get(ctx, nodeName, metav1.GetOptions{})
	if err != nil {
		return nil, err
	}

	var ips []string
	for _, addr := range node.Status.Addresses {
		if addr.Type == corev1.NodeExternalIP || addr.Type == corev1.NodeInternalIP {
			ips = append(ips, addr.Address)
		}
	}
	return ips, nil
}

// GetLoadBalancerIPs returns the external IPs assigned to all game server
// LoadBalancer Services in the configured namespace. This is used by the
// allocation endpoint to show available IPs when in LoadBalancer mode.
func GetLoadBalancerIPs(ctx context.Context) ([]string, error) {
	cfg := config.Get()
	c, err := Client()
	if err != nil {
		return nil, err
	}

	svcs, err := c.CoreV1().Services(cfg.Kubernetes.Namespace).List(ctx, metav1.ListOptions{
		LabelSelector: "app.kubernetes.io/managed-by=pelican-wings,pelican.dev/resource-type=service",
	})
	if err != nil {
		return nil, err
	}

	seen := make(map[string]bool)
	var ips []string
	for _, svc := range svcs.Items {
		for _, ingress := range svc.Status.LoadBalancer.Ingress {
			addr := ingress.IP
			if addr == "" {
				addr = ingress.Hostname
			}
			if addr != "" && !seen[addr] {
				seen[addr] = true
				ips = append(ips, addr)
			}
		}
	}
	return ips, nil
}
