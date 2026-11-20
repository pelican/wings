package config

import (
	"errors"
	"fmt"
	"net"
	"path"
	"regexp"
	"slices"
	"strings"
	"time"
	"unicode"

	"github.com/asaskevich/govalidator"
)

// PanelConfiguration contains the only configuration values the Panel is allowed
// to set on a node through POST /api/update. Everything else in the configuration
// controls how Wings itself runs on the host, such as the user it runs containers
// as, the directories it writes to as root and the container registries, and can
// only be changed by the node operator in config.yml (GHSA-8rw4-fw83-3rv7).
//
// The keys match what the Panel sends: the values it always manages, and the
// optional overrides from the "Wings config options" on the Edit Node page
// (Node::DAEMON_CONFIG_KEYS). A key that is missing, or null, keeps its current
// value. Values are validated before any of them are applied.
type PanelConfiguration struct {
	Debug                   *bool     `json:"debug"`
	AppName                 *string   `json:"app_name"`
	Uuid                    *string   `json:"uuid"`
	TokenId                 *string   `json:"token_id"`
	Token                   *string   `json:"token"`
	AllowedOrigins          *[]string `json:"allowed_origins"`
	AllowCORSPrivateNetwork *bool     `json:"allow_cors_private_network"`

	Api struct {
		Host *string `json:"host"`
		Port *int    `json:"port"`
		Ssl  struct {
			Enabled         *bool   `json:"enabled"`
			CertificateFile *string `json:"cert"`
			KeyFile         *string `json:"key"`
		} `json:"ssl"`
		UploadLimit    *int64    `json:"upload_limit"`
		TrustedProxies *[]string `json:"trusted_proxies"`
		RemoteDownload struct {
			MaxRedirects *int `json:"max_redirects"`
		} `json:"remote_download"`
	} `json:"api"`

	RemoteQuery struct {
		Timeout *int `json:"timeout"`
	} `json:"remote_query"`

	System struct {
		Timezone *string `json:"timezone"`
		Quotas   struct {
			Enabled *bool `json:"enabled"`
		} `json:"quotas"`
		Sftp struct {
			Address *string `json:"bind_address"`
			Port    *int    `json:"bind_port"`
		} `json:"sftp"`
		User struct {
			Passwd struct {
				Enable *bool `json:"enable"`
			} `json:"passwd"`
		} `json:"user"`
		MachineID struct {
			Enable *bool `json:"enable"`
		} `json:"machine_id"`
	} `json:"system"`

	Docker struct {
		Network struct {
			Name       *string   `json:"name"`
			Driver     *string   `json:"driver"`
			Interface  *string   `json:"interface"`
			Dns        *[]string `json:"dns"`
			ISPN       *bool     `json:"ispn"`
			IPv6       *bool     `json:"IPv6"`
			Interfaces struct {
				V4 struct {
					Subnet  *string `json:"subnet"`
					Gateway *string `json:"gateway"`
				} `json:"v4"`
				V6 struct {
					Subnet  *string `json:"subnet"`
					Gateway *string `json:"gateway"`
				} `json:"v6"`
			} `json:"interfaces"`
		} `json:"network"`
		Domainname        *string `json:"domainname"`
		TmpfsSize         *uint   `json:"tmpfs_size"`
		ContainerPidLimit *int64  `json:"container_pid_limit"`
		InstallerLimits   struct {
			Memory *int64 `json:"memory"`
			Cpu    *int64 `json:"cpu"`
		} `json:"installer_limits"`
		Overhead struct {
			Override          *bool    `json:"override"`
			DefaultMultiplier *float64 `json:"default_multiplier"`
		} `json:"overhead"`
		UsePerformantInspect *bool     `json:"use_performant_inspect"`
		UsernsMode           *string   `json:"userns_mode"`
		SystemIps            *[]string `json:"system_ips"`
		LogConfig            struct {
			Type *string `json:"type"`
		} `json:"log_config"`
	} `json:"docker"`

	Throttles struct {
		Enabled *bool   `json:"enabled"`
		Lines   *uint64 `json:"lines"`
		Period  *uint64 `json:"line_reset_interval"`
	} `json:"throttles"`
}

var (
	// A Docker network or driver name.
	dockerNameRegex = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9_.-]{0,127}$`)
	// A Docker log driver, which may be a plugin such as "grafana/loki:latest".
	logDriverRegex = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9_./:-]{0,127}$`)
)

// Validate checks every value that is set. The returned error describes the
// first invalid value.
func (p *PanelConfiguration) Validate() error {
	var errs []error
	check := func(key string, ok bool, format string) {
		if !ok {
			errs = append(errs, fmt.Errorf("%s: %s", key, format))
		}
	}

	if v := p.AppName; v != nil {
		check("app_name", len(*v) <= 64 && isPrintable(*v), "must be at most 64 printable characters")
	}
	if v := p.Uuid; v != nil {
		check("uuid", *v == "" || govalidator.IsUUID(*v), "must be a UUID")
	}
	if v := p.TokenId; v != nil {
		check("token_id", len(*v) <= 255 && isPrintable(*v), "must be at most 255 printable characters")
	}
	if v := p.Token; v != nil {
		check("token", len(*v) <= 4096 && isPrintable(*v), "must be at most 4096 printable characters")
	}
	if v := p.AllowedOrigins; v != nil {
		check("allowed_origins", all(*v, func(s string) bool { return s != "" && len(s) <= 2048 && isPrintable(s) }), "must only contain origins")
	}

	if v := p.Api.Host; v != nil {
		check("api.host", isIP(*v) || govalidator.IsDNSName(*v), "must be an IP address or hostname")
	}
	if v := p.Api.Port; v != nil {
		check("api.port", isPort(*v), "must be a port number")
	}
	if v := p.Api.Ssl.CertificateFile; v != nil {
		check("api.ssl.cert", *v == "" || isAbsolutePath(*v), "must be an absolute path")
	}
	if v := p.Api.Ssl.KeyFile; v != nil {
		check("api.ssl.key", *v == "" || isAbsolutePath(*v), "must be an absolute path")
	}
	if v := p.Api.UploadLimit; v != nil {
		check("api.upload_limit", *v >= 0, "must not be negative")
	}
	if v := p.Api.TrustedProxies; v != nil {
		check("api.trusted_proxies", all(*v, isIPOrCIDR), "must only contain IP addresses or CIDR ranges")
	}
	if v := p.Api.RemoteDownload.MaxRedirects; v != nil {
		check("api.remote_download.max_redirects", *v >= 0, "must not be negative")
	}
	if v := p.RemoteQuery.Timeout; v != nil {
		check("remote_query.timeout", *v >= 0, "must not be negative")
	}

	if v := p.System.Timezone; v != nil && *v != "" {
		_, err := time.LoadLocation(*v)
		check("system.timezone", err == nil && !strings.ContainsAny(*v, `\`) && !strings.Contains(*v, ".."), "must be a valid time zone")
	}
	if v := p.System.Sftp.Address; v != nil {
		check("system.sftp.bind_address", isIP(*v), "must be an IP address")
	}
	if v := p.System.Sftp.Port; v != nil {
		check("system.sftp.bind_port", isPort(*v), "must be a port number")
	}

	n := &p.Docker.Network
	if v := n.Name; v != nil {
		check("docker.network.name", dockerNameRegex.MatchString(*v), "must be a valid Docker network name")
	}
	if v := n.Driver; v != nil {
		check("docker.network.driver", dockerNameRegex.MatchString(*v), "must be a valid Docker network driver")
	}
	if v := n.Interface; v != nil {
		check("docker.network.interface", isIP(*v), "must be an IP address")
	}
	if v := n.Dns; v != nil {
		check("docker.network.dns", all(*v, isIP), "must only contain IP addresses")
	}
	if v := n.Interfaces.V4.Subnet; v != nil {
		check("docker.network.interfaces.v4.subnet", isCIDR(*v, false), "must be an IPv4 CIDR range")
	}
	if v := n.Interfaces.V4.Gateway; v != nil {
		check("docker.network.interfaces.v4.gateway", isIPVersion(*v, false), "must be an IPv4 address")
	}
	if v := n.Interfaces.V6.Subnet; v != nil {
		check("docker.network.interfaces.v6.subnet", isCIDR(*v, true), "must be an IPv6 CIDR range")
	}
	if v := n.Interfaces.V6.Gateway; v != nil {
		check("docker.network.interfaces.v6.gateway", isIPVersion(*v, true), "must be an IPv6 address")
	}

	if v := p.Docker.Domainname; v != nil {
		check("docker.domainname", *v == "" || govalidator.IsDNSName(*v), "must be a domain name")
	}
	if v := p.Docker.ContainerPidLimit; v != nil {
		check("docker.container_pid_limit", *v >= 0, "must not be negative")
	}
	if v := p.Docker.InstallerLimits.Memory; v != nil {
		check("docker.installer_limits.memory", *v >= 0, "must not be negative")
	}
	if v := p.Docker.InstallerLimits.Cpu; v != nil {
		check("docker.installer_limits.cpu", *v >= 0, "must not be negative")
	}
	if v := p.Docker.Overhead.DefaultMultiplier; v != nil {
		check("docker.overhead.default_multiplier", *v >= 0 && *v <= 100, "must be between 0 and 100")
	}
	if v := p.Docker.UsernsMode; v != nil {
		// Only the two modes Wings supports. Anything else, such as a custom user
		// namespace, has to be configured by the node operator.
		check("docker.userns_mode", *v == "" || *v == "host", `must be empty or "host"`)
	}
	if v := p.Docker.SystemIps; v != nil {
		check("docker.system_ips", all(*v, isIP), "must only contain IP addresses")
	}
	if v := p.Docker.LogConfig.Type; v != nil {
		check("docker.log_config.type", logDriverRegex.MatchString(*v), "must be a valid Docker log driver")
	}

	return errors.Join(errs...)
}

// ApplyPanelConfiguration sets the values from the Panel that are present on the
// configuration. Slices are copied, so the configuration never shares memory with
// the update or with another copy of the configuration.
func (c *Configuration) ApplyPanelConfiguration(p PanelConfiguration) {
	set(&c.Debug, p.Debug)
	set(&c.AppName, p.AppName)
	set(&c.Uuid, p.Uuid)
	set(&c.AuthenticationTokenId, p.TokenId)
	set(&c.AuthenticationToken, p.Token)
	setSlice(&c.AllowedOrigins, p.AllowedOrigins)
	set(&c.AllowCORSPrivateNetwork, p.AllowCORSPrivateNetwork)

	set(&c.Api.Host, p.Api.Host)
	set(&c.Api.Port, p.Api.Port)
	set(&c.Api.Ssl.Enabled, p.Api.Ssl.Enabled)
	set(&c.Api.Ssl.CertificateFile, p.Api.Ssl.CertificateFile)
	set(&c.Api.Ssl.KeyFile, p.Api.Ssl.KeyFile)
	set(&c.Api.UploadLimit, p.Api.UploadLimit)
	setSlice(&c.Api.TrustedProxies, p.Api.TrustedProxies)
	set(&c.Api.RemoteDownload.MaxRedirects, p.Api.RemoteDownload.MaxRedirects)
	set(&c.RemoteQuery.Timeout, p.RemoteQuery.Timeout)

	set(&c.System.Timezone, p.System.Timezone)
	set(&c.System.Quotas.Enabled, p.System.Quotas.Enabled)
	set(&c.System.Sftp.Address, p.System.Sftp.Address)
	set(&c.System.Sftp.Port, p.System.Sftp.Port)
	set(&c.System.User.Passwd.Enable, p.System.User.Passwd.Enable)
	set(&c.System.MachineID.Enable, p.System.MachineID.Enable)

	n, pn := &c.Docker.Network, &p.Docker.Network
	set(&n.Name, pn.Name)
	set(&n.Driver, pn.Driver)
	set(&n.Interface, pn.Interface)
	setSlice(&n.Dns, pn.Dns)
	set(&n.ISPN, pn.ISPN)
	set(&n.IPv6, pn.IPv6)
	set(&n.Interfaces.V4.Subnet, pn.Interfaces.V4.Subnet)
	set(&n.Interfaces.V4.Gateway, pn.Interfaces.V4.Gateway)
	set(&n.Interfaces.V6.Subnet, pn.Interfaces.V6.Subnet)
	set(&n.Interfaces.V6.Gateway, pn.Interfaces.V6.Gateway)

	set(&c.Docker.Domainname, p.Docker.Domainname)
	set(&c.Docker.TmpfsSize, p.Docker.TmpfsSize)
	set(&c.Docker.ContainerPidLimit, p.Docker.ContainerPidLimit)
	set(&c.Docker.InstallerLimits.Memory, p.Docker.InstallerLimits.Memory)
	set(&c.Docker.InstallerLimits.Cpu, p.Docker.InstallerLimits.Cpu)
	set(&c.Docker.Overhead.Override, p.Docker.Overhead.Override)
	set(&c.Docker.Overhead.DefaultMultiplier, p.Docker.Overhead.DefaultMultiplier)
	set(&c.Docker.UsePerformantInspect, p.Docker.UsePerformantInspect)
	set(&c.Docker.UsernsMode, p.Docker.UsernsMode)
	setSlice(&c.Docker.SystemIps, p.Docker.SystemIps)
	set(&c.Docker.LogConfig.Type, p.Docker.LogConfig.Type)

	set(&c.Throttles.Enabled, p.Throttles.Enabled)
	set(&c.Throttles.Lines, p.Throttles.Lines)
	set(&c.Throttles.Period, p.Throttles.Period)
}

func set[T any](dst *T, v *T) {
	if v != nil {
		*dst = *v
	}
}

func setSlice[T any](dst *[]T, v *[]T) {
	if v != nil {
		*dst = slices.Clone(*v)
	}
}

func all(v []string, fn func(string) bool) bool {
	for _, s := range v {
		if !fn(s) {
			return false
		}
	}
	return true
}

func isPrintable(s string) bool {
	for _, r := range s {
		if !unicode.IsPrint(r) {
			return false
		}
	}
	return true
}

func isPort(v int) bool { return v > 0 && v <= 65535 }

func isIP(s string) bool { return net.ParseIP(s) != nil }

func isIPVersion(s string, v6 bool) bool {
	ip := net.ParseIP(s)
	return ip != nil && (ip.To4() == nil) == v6
}

func isCIDR(s string, v6 bool) bool {
	ip, _, err := net.ParseCIDR(s)
	return err == nil && (ip.To4() == nil) == v6
}

func isIPOrCIDR(s string) bool {
	if isIP(s) {
		return true
	}
	_, _, err := net.ParseCIDR(s)
	return err == nil
}

func isAbsolutePath(s string) bool {
	return strings.HasPrefix(s, "/") && path.Clean(s) == s && !strings.ContainsRune(s, 0) && isPrintable(s)
}
