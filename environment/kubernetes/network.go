package kubernetes

import (
	"context"
	"fmt"
	"strings"
	"time"

	"emperror.dev/errors"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/util/intstr"
	"k8s.io/apimachinery/pkg/watch"
	corev1apply "k8s.io/client-go/applyconfigurations/core/v1"

	"github.com/pelican/wings/config"
)

// serviceName returns the Kubernetes Service name for this server.
func (e *Environment) serviceName() string {
	return fmt.Sprintf("gs-%s", e.Id)
}

// EnsureService creates or updates the Kubernetes Service that exposes game
// server ports. In NodePort mode, ports are exposed via auto-assigned NodePorts.
// In LoadBalancer mode, each server gets a dedicated external IP with 1:1 port
// mapping. In HostPort mode this is a no-op.
func (e *Environment) EnsureService(ctx context.Context) error {
	cfg := config.Get()
	if cfg.Kubernetes.NetworkMode == config.KubeNetworkHostPort {
		return nil
	}

	allocs := e.Configuration.Allocations()
	if len(allocs.Mappings) == 0 {
		return nil
	}

	svcName := e.serviceName()
	ns := e.namespace()

	isLB := cfg.Kubernetes.NetworkMode == config.KubeNetworkLoadBalancer

	// Build the Service port list from allocations. A port may appear under
	// multiple allocation IPs, but the Service only needs it once. nodePort is
	// only set when an explicit port was resolved; omitting it lets the
	// apiserver keep any already-allocated NodePort across applies.
	seenPorts := make(map[int]bool)
	var servicePorts []*corev1apply.ServicePortApplyConfiguration
	for _, ports := range allocs.Mappings {
		for _, port := range ports {
			if port < 1 || port > 65535 || seenPorts[port] {
				continue
			}
			seenPorts[port] = true

			tcp := corev1apply.ServicePort().
				WithName(portName("tcp", port)).
				WithProtocol(corev1.ProtocolTCP).
				WithPort(int32(port)).
				WithTargetPort(intstr.FromInt32(int32(port)))
			udp := corev1apply.ServicePort().
				WithName(portName("udp", port)).
				WithProtocol(corev1.ProtocolUDP).
				WithPort(int32(port)).
				WithTargetPort(intstr.FromInt32(int32(port)))

			if !isLB {
				if np := e.resolveNodePort(cfg, port); np != 0 {
					tcp.WithNodePort(np)
					udp.WithNodePort(np)
				}
			}

			servicePorts = append(servicePorts, tcp, udp)
		}
	}

	if len(servicePorts) == 0 {
		return nil
	}

	// Labels to match the Pod.
	selector := map[string]string{
		"pelican.dev/server-id": e.Id,
	}

	labels := map[string]string{
		"app.kubernetes.io/managed-by": "pelican-wings",
		"pelican.dev/server-id":        e.Id,
		"pelican.dev/resource-type":    "service",
	}

	svcType := corev1.ServiceTypeNodePort
	var annotations map[string]string
	if isLB {
		svcType = corev1.ServiceTypeLoadBalancer
		annotations = make(map[string]string)
		for k, v := range cfg.Kubernetes.LBAnnotations {
			annotations[k] = v
		}

		// Auto-set IP-pinning and sharing-key annotations from the
		// server's allocation IP so the LB is bound to the IP the user
		// selected in the Panel.
		if allocIP := e.allocationIP(); allocIP != "" {
			if cfg.Kubernetes.LBIPAnnotation != "" {
				annotations[cfg.Kubernetes.LBIPAnnotation] = allocIP
			}
			if cfg.Kubernetes.LBSharingKey != "" {
				annotations[cfg.Kubernetes.LBSharingKey] = allocIP
			}
		}
	}

	apply := corev1apply.Service(svcName, ns).
		WithLabels(labels).
		WithSpec(corev1apply.ServiceSpec().
			WithType(svcType).
			WithSelector(selector).
			WithPorts(servicePorts...))
	if len(annotations) > 0 {
		// Wings owns the annotations field via server-side apply, so
		// annotations omitted here (e.g. stale LB annotations after a
		// networkMode switch) are removed from the live object.
		apply.WithAnnotations(annotations)
	}

	e.log().WithField("service", svcName).Infof("applying %s service for server", svcType)
	_, err := e.client.CoreV1().Services(ns).Apply(ctx, apply, applyOptions())
	if err != nil {
		return errors.Wrap(err, "environment/kubernetes: failed to apply service")
	}

	return nil
}

// DeleteService removes the Kubernetes Service associated with this server.
func (e *Environment) DeleteService(ctx context.Context) error {
	cfg := config.Get()
	if cfg.Kubernetes.NetworkMode == config.KubeNetworkHostPort {
		return nil
	}

	svcName := e.serviceName()
	ns := e.namespace()

	err := e.client.CoreV1().Services(ns).Delete(ctx, svcName, metav1.DeleteOptions{})
	if err != nil && !isNotFound(err) {
		return errors.Wrap(err, "environment/kubernetes: failed to delete service")
	}

	e.log().WithField("service", svcName).Debug("deleted service for server")
	return nil
}

// GetServiceNodePorts returns a map of container port → assigned NodePort for
// this server's Service. Returns nil if no Service exists or we're in HostPort
// mode.
func (e *Environment) GetServiceNodePorts(ctx context.Context) (map[int32]int32, error) {
	cfg := config.Get()
	if cfg.Kubernetes.NetworkMode == config.KubeNetworkHostPort {
		return nil, nil
	}

	svc, err := e.client.CoreV1().Services(e.namespace()).Get(ctx, e.serviceName(), metav1.GetOptions{})
	if err != nil {
		if isNotFound(err) {
			return nil, nil
		}
		return nil, errors.Wrap(err, "environment/kubernetes: failed to get service")
	}

	result := make(map[int32]int32)
	for _, p := range svc.Spec.Ports {
		if p.NodePort > 0 {
			result[p.Port] = p.NodePort
		}
	}
	return result, nil
}

// GetServiceExternalIP returns the external IP assigned to this server's
// LoadBalancer Service. Returns an empty string if no IP is assigned yet,
// or if the network mode is not LoadBalancer.
func (e *Environment) GetServiceExternalIP(ctx context.Context) (string, error) {
	cfg := config.Get()
	if cfg.Kubernetes.NetworkMode != config.KubeNetworkLoadBalancer {
		return "", nil
	}

	svc, err := e.client.CoreV1().Services(e.namespace()).Get(ctx, e.serviceName(), metav1.GetOptions{})
	if err != nil {
		if isNotFound(err) {
			return "", nil
		}
		return "", errors.Wrap(err, "environment/kubernetes: failed to get service")
	}

	for _, ingress := range svc.Status.LoadBalancer.Ingress {
		if ingress.IP != "" {
			return ingress.IP, nil
		}
		if ingress.Hostname != "" {
			return ingress.Hostname, nil
		}
	}
	return "", nil
}

// WaitForLoadBalancerIP watches the Service until an external IP is assigned
// by the load balancer provisioner, or until the timeout (2 minutes) is
// reached. The watch is re-established if the apiserver closes the result
// channel before the deadline.
func (e *Environment) WaitForLoadBalancerIP(ctx context.Context) (string, error) {
	cfg := config.Get()
	if cfg.Kubernetes.NetworkMode != config.KubeNetworkLoadBalancer {
		return "", nil
	}

	dctx, cancel := context.WithTimeout(ctx, 2*time.Minute)
	defer cancel()

	ingressIP := func(svc *corev1.Service) string {
		for _, ingress := range svc.Status.LoadBalancer.Ingress {
			if ingress.IP != "" {
				return ingress.IP
			}
			if ingress.Hostname != "" {
				return ingress.Hostname
			}
		}
		return ""
	}

	var ip string
	err := watchUntil(dctx,
		func(ctx context.Context) (runtime.Object, error) {
			svc, err := e.client.CoreV1().Services(e.namespace()).Get(ctx, e.serviceName(), metav1.GetOptions{})
			if err != nil {
				if isNotFound(err) {
					// No Service exists; signal done via a nil object.
					return nil, nil
				}
				return nil, errors.Wrap(err, "environment/kubernetes: failed to get service")
			}
			return svc, nil
		},
		func(ctx context.Context) (watch.Interface, error) {
			w, err := e.client.CoreV1().Services(e.namespace()).Watch(ctx, metav1.ListOptions{
				FieldSelector: "metadata.name=" + e.serviceName(),
			})
			if err != nil {
				return nil, errors.Wrap(err, "environment/kubernetes: failed to watch service")
			}
			return w, nil
		},
		func(evt watch.EventType, obj runtime.Object) (bool, error) {
			if obj == nil {
				return true, nil
			}
			if evt == watch.Deleted {
				return false, nil
			}
			svc, ok := obj.(*corev1.Service)
			if !ok {
				return false, nil
			}
			if ip = ingressIP(svc); ip != "" {
				e.log().WithField("external_ip", ip).Info("load balancer IP assigned")
				return true, nil
			}
			return false, nil
		},
	)
	if err != nil {
		if ctx.Err() != nil {
			return "", ctx.Err()
		}
		if errors.Is(err, context.DeadlineExceeded) {
			e.log().Warn("timed out waiting for load balancer IP assignment")
			return "", nil
		}
		return "", err
	}
	return ip, nil
}

// resolveNodePort determines the NodePort to request for a given game server
// port. If PreserveNodePorts is enabled and the port falls within the
// Kubernetes NodePort range (default 30000-32767), use it directly. Otherwise,
// return 0 to let Kubernetes assign automatically.
func (e *Environment) resolveNodePort(cfg *config.Configuration, port int) int32 {
	if !cfg.Kubernetes.NodePortPreserve {
		return 0
	}
	// Only request a specific NodePort if the port is within the valid range.
	min := int(cfg.Kubernetes.NodePortRangeMin)
	max := int(cfg.Kubernetes.NodePortRangeMax)
	if min == 0 {
		min = 30000
	}
	if max == 0 {
		max = 32767
	}
	if port >= min && port <= max {
		return int32(port)
	}
	return 0
}

// sanitizePortName ensures a Kubernetes Service port name is valid (lowercase,
// alphanumeric, hyphens, max 15 chars, must start/end with alphanumeric).
func sanitizePortName(name string) string {
	name = strings.ToLower(name)
	name = strings.ReplaceAll(name, ".", "-")
	name = strings.ReplaceAll(name, ":", "-")

	// Remove invalid characters.
	var cleaned []byte
	for i, c := range []byte(name) {
		if (c >= 'a' && c <= 'z') || (c >= '0' && c <= '9') || (c == '-' && i > 0) {
			cleaned = append(cleaned, c)
		}
	}
	name = string(cleaned)

	// Trim trailing hyphens.
	name = strings.TrimRight(name, "-")

	// Max 15 characters for port names.
	if len(name) > 15 {
		name = name[:15]
	}

	// Must not end with hyphen after truncation.
	name = strings.TrimRight(name, "-")

	if name == "" {
		name = "port"
	}

	return name
}

// portName builds a unique, K8s-valid Service port name. The format is
// "{proto}-{port}" (e.g. "tcp-27015"), which fits within the 15-char limit
// and avoids collisions that occur when long IP strings are truncated.
func portName(proto string, port int) string {
	return sanitizePortName(fmt.Sprintf("%s-%d", proto, port))
}

// allocationIP returns the server's default allocation IP if it is a usable
// public/external address. Returns empty for 0.0.0.0, 127.0.0.1, or when
// no default allocation is configured.
func (e *Environment) allocationIP() string {
	allocs := e.Configuration.Allocations()
	if allocs.DefaultMapping == nil {
		return ""
	}
	ip := allocs.DefaultMapping.Ip
	if ip == "" || ip == "0.0.0.0" || ip == "127.0.0.1" {
		return ""
	}
	return ip
}
