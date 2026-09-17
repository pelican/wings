package config

// KubernetesStorageMode defines how server data volumes are provisioned.
type KubernetesStorageMode string

const (
	// KubeStorageHostPath uses HostPath volumes (node-local, default).
	KubeStorageHostPath KubernetesStorageMode = "hostpath"

	// KubeStoragePVC uses PersistentVolumeClaims for server data directories.
	KubeStoragePVC KubernetesStorageMode = "pvc"
)

// KubernetesNetworkMode defines how game server ports are exposed.
type KubernetesNetworkMode string

const (
	// KubeNetworkHostPort uses hostNetwork + hostPort for direct port binding
	// (closest to Docker behavior).
	KubeNetworkHostPort KubernetesNetworkMode = "hostport"

	// KubeNetworkNodePort uses Kubernetes Services with NodePort to expose game
	// server ports via the node's IP with auto-assigned external ports.
	KubeNetworkNodePort KubernetesNetworkMode = "nodeport"

	// KubeNetworkLoadBalancer uses Kubernetes Services with type LoadBalancer
	// to assign a dedicated external IP per game server. Requires a load
	// balancer provisioner (MetalLB, cloud controller, kube-vip, etc.).
	KubeNetworkLoadBalancer KubernetesNetworkMode = "loadbalancer"
)

// KubernetesConfiguration defines the Kubernetes configuration used by the
// daemon when scheduling game server workloads as Pods.
type KubernetesConfiguration struct {
	// Enabled controls whether this Wings node uses Kubernetes to run game
	// server workloads instead of Docker.
	Enabled bool `default:"false" json:"enabled" yaml:"enabled"`

	// Namespace is the Kubernetes namespace where game server Pods and related
	// resources are created.
	Namespace string `default:"pelican" json:"namespace" yaml:"namespace"`

	// Kubeconfig is the path to a kubeconfig file. If empty, in-cluster
	// configuration is used (suitable when Wings itself runs inside K8s).
	Kubeconfig string `default:"" json:"kubeconfig" yaml:"kubeconfig"`

	// ImagePullSecrets is a list of Secret names used for pulling container
	// images in game server Pods.
	ImagePullSecrets []string `json:"image_pull_secrets" yaml:"image_pull_secrets"`

	// ImagePullPolicy overrides the pull policy applied to game server Pods
	// and installation Jobs. Valid values are "Always", "IfNotPresent", and
	// "Never". When empty, remote images use "Always" (so updated tags are
	// re-pulled, matching the Docker backend) and ~-prefixed local images use
	// "IfNotPresent". Set to "IfNotPresent" or "Never" for air-gapped clusters.
	ImagePullPolicy string `default:"" json:"image_pull_policy" yaml:"image_pull_policy"`

	// ServiceAccount is the name of the Kubernetes ServiceAccount assigned to
	// game server Pods.
	ServiceAccount string `default:"" json:"service_account" yaml:"service_account"`

	// StorageMode controls how server data volumes are provisioned.
	// "hostpath" uses HostPath volumes (default, node-local).
	// "pvc" creates PersistentVolumeClaims for each server.
	StorageMode KubernetesStorageMode `default:"hostpath" json:"storage_mode" yaml:"storage_mode"`

	// StorageClass is the StorageClass used for PersistentVolumeClaims that
	// back server data directories. Leave empty to use the cluster default.
	StorageClass string `default:"" json:"storage_class" yaml:"storage_class"`

	// StorageSize is the default size for server PVCs (e.g. "10Gi", "50Gi").
	// Only used when StorageMode is "pvc".
	StorageSize string `default:"10Gi" json:"storage_size" yaml:"storage_size"`

	// StorageAccessMode is the access mode for PVCs. Defaults to
	// ReadWriteOnce. Options: ReadWriteOnce, ReadWriteMany.
	StorageAccessMode string `default:"ReadWriteOnce" json:"storage_access_mode" yaml:"storage_access_mode"`

	// NodeSelector is a set of key-value pairs used for scheduling Pods onto
	// specific nodes.
	NodeSelector map[string]string `json:"node_selector" yaml:"node_selector"`

	// DNSPolicy sets the DNS policy for game server Pods.
	DNSPolicy string `default:"ClusterFirst" json:"dns_policy" yaml:"dns_policy"`

	// NetworkMode controls how game server ports are exposed to clients.
	NetworkMode KubernetesNetworkMode `default:"hostport" json:"network_mode" yaml:"network_mode"`

	// NodePortPreserve attempts to use the game server's allocated port as
	// the NodePort value (only works if the port falls within the cluster's
	// NodePort range). When false, Kubernetes auto-assigns NodePorts.
	NodePortPreserve bool `default:"false" json:"nodeport_preserve" yaml:"nodeport_preserve"`

	// NodePortRangeMin is the lower bound of the Kubernetes NodePort range.
	// Defaults to 30000 if unset. Used when NodePortPreserve is true.
	NodePortRangeMin int32 `default:"30000" json:"nodeport_range_min" yaml:"nodeport_range_min"`

	// NodePortRangeMax is the upper bound of the Kubernetes NodePort range.
	// Defaults to 32767 if unset. Used when NodePortPreserve is true.
	NodePortRangeMax int32 `default:"32767" json:"nodeport_range_max" yaml:"nodeport_range_max"`

	// LBAnnotations is a map of annotations applied to LoadBalancer Services.
	// Use this for provider-specific configuration (e.g. MetalLB address pool,
	// cloud LB options). Example:
	//   lb_annotations:
	//     metallb.universe.tf/address-pool: "game-servers"
	LBAnnotations map[string]string `json:"lb_annotations" yaml:"lb_annotations"`

	// LBIPAnnotation is the annotation key that Wings sets to the server's
	// allocation IP on each LoadBalancer Service. This pins the LB to the IP
	// the user selected in the Panel, enabling multiple game servers to share
	// the same external IP (on different ports).
	//
	// Common values:
	//   Cilium:  "lbipam.cilium.io/ips"
	//   MetalLB: "metallb.universe.tf/loadBalancerIPs"
	//
	// When empty (default), no IP-pinning annotation is added.
	LBIPAnnotation string `default:"" json:"lb_ip_annotation" yaml:"lb_ip_annotation"`

	// LBSharingKey is the annotation key used to allow multiple Services to
	// share the same external IP. Wings sets this annotation to the allocation
	// IP value, grouping all game servers on the same IP under one sharing key.
	//
	// Common values:
	//   Cilium:  "lbipam.cilium.io/sharing-key"
	//   MetalLB: "metallb.universe.tf/allow-shared-ip"
	//
	// When empty (default), no sharing-key annotation is added.
	LBSharingKey string `default:"" json:"lb_sharing_key" yaml:"lb_sharing_key"`

	// Tolerations allow game server Pods to be scheduled on tainted nodes.
	Tolerations []KubeToleration `json:"tolerations" yaml:"tolerations"`

	// NodeName is the Kubernetes node name where Wings is running. Used to
	// query node addresses for the IP allocation endpoint. If empty, Wings
	// reads the NODE_NAME environment variable (typically set via the
	// Kubernetes downward API).
	NodeName string `default:"" json:"node_name" yaml:"node_name"`

	// SystemIPs is a list of additional IP addresses to expose in the
	// allocation endpoint. These are appended to the auto-discovered node
	// addresses and can be used to advertise public IPs that are not
	// directly assigned to the node (e.g. floating IPs, load balancer VIPs).
	SystemIPs []string `json:"system_ips" yaml:"system_ips"`

	// DataPVC is the name of a shared PersistentVolumeClaim that Wings and game
	// server Pods both mount for server data. When set, game servers use this PVC
	// with a subPath instead of creating individual PVCs per server. This enables
	// the Panel file browser and SFTP access, since Wings can read/write the same
	// storage as the game server Pods.
	//
	// The PVC must already exist and be mounted into the Wings Pod at the
	// configured rootDirectory. Typically this is the Wings data PVC created
	// by the Helm chart.
	//
	// When empty (default), each server gets its own PVC (file browser won't work).
	DataPVC string `default:"" json:"data_pvc" yaml:"data_pvc"`

	// ResourceQuota configures namespace-level resource limits.
	// When enabled, Wings creates/updates a ResourceQuota in the game server
	// namespace to cap aggregate resource usage across all Pods.
	ResourceQuota KubeResourceQuota `json:"resource_quota" yaml:"resource_quota"`

	// LimitRange configures default resource limits and requests for Pods
	// that do not specify their own. This prevents unbounded resource usage
	// from misconfigured servers.
	LimitRange KubeLimitRange `json:"limit_range" yaml:"limit_range"`
}

// KubeResourceQuota defines namespace-level resource quotas.
type KubeResourceQuota struct {
	// Enabled controls whether a ResourceQuota is created/enforced.
	Enabled bool `default:"false" json:"enabled" yaml:"enabled"`

	// CPULimit is the total CPU limit across all Pods (e.g. "16", "8000m").
	CPULimit string `default:"" json:"cpu_limit" yaml:"cpu_limit"`

	// MemoryLimit is the total memory limit across all Pods (e.g. "32Gi").
	MemoryLimit string `default:"" json:"memory_limit" yaml:"memory_limit"`

	// CPURequest is the total CPU request across all Pods (e.g. "8", "4000m").
	CPURequest string `default:"" json:"cpu_request" yaml:"cpu_request"`

	// MemoryRequest is the total memory request across all Pods (e.g. "16Gi").
	MemoryRequest string `default:"" json:"memory_request" yaml:"memory_request"`

	// MaxPods is the maximum number of Pods allowed in the namespace.
	MaxPods int64 `default:"0" json:"max_pods" yaml:"max_pods"`

	// MaxPVCs is the maximum number of PersistentVolumeClaims in the namespace.
	MaxPVCs int64 `default:"0" json:"max_pvcs" yaml:"max_pvcs"`

	// MaxStorage is the total storage allowed across all PVCs (e.g. "500Gi").
	MaxStorage string `default:"" json:"max_storage" yaml:"max_storage"`
}

// KubeLimitRange defines default resource limits and requests for containers.
type KubeLimitRange struct {
	// Enabled controls whether a LimitRange is created.
	Enabled bool `default:"false" json:"enabled" yaml:"enabled"`

	// DefaultCPULimit is the default CPU limit for containers (e.g. "2", "1000m").
	DefaultCPULimit string `default:"" json:"default_cpu_limit" yaml:"default_cpu_limit"`

	// DefaultMemoryLimit is the default memory limit for containers (e.g. "4Gi").
	DefaultMemoryLimit string `default:"" json:"default_memory_limit" yaml:"default_memory_limit"`

	// DefaultCPURequest is the default CPU request for containers (e.g. "500m").
	DefaultCPURequest string `default:"" json:"default_cpu_request" yaml:"default_cpu_request"`

	// DefaultMemoryRequest is the default memory request for containers (e.g. "1Gi").
	DefaultMemoryRequest string `default:"" json:"default_memory_request" yaml:"default_memory_request"`

	// MaxCPU is the maximum CPU any single container can request (e.g. "4").
	MaxCPU string `default:"" json:"max_cpu" yaml:"max_cpu"`

	// MaxMemory is the maximum memory any single container can request (e.g. "8Gi").
	MaxMemory string `default:"" json:"max_memory" yaml:"max_memory"`
}

// KubeToleration mirrors corev1.Toleration for YAML configuration.
type KubeToleration struct {
	Key               string `json:"key" yaml:"key"`
	Operator          string `default:"Equal" json:"operator" yaml:"operator"`
	Value             string `json:"value" yaml:"value"`
	Effect            string `json:"effect" yaml:"effect"`
	TolerationSeconds *int64 `json:"toleration_seconds" yaml:"toleration_seconds"`
}
