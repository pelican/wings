package docker

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"net/netip"
	"runtime"
	"strconv"
	"strings"
	"time"

	"emperror.dev/errors"
	"github.com/apex/log"
	"github.com/containerd/errdefs"
	"github.com/dustin/go-humanize"
	"github.com/goccy/go-json"
	"github.com/moby/moby/api/types/container"
	"github.com/moby/moby/api/types/mount"
	"github.com/moby/moby/api/types/network"
	"github.com/moby/moby/client"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"

	"github.com/pelican/wings/config"
	"github.com/pelican/wings/environment"
	"github.com/pelican/wings/system"
)

var ErrNotAttached = errors.Sentinel("not attached to instance")

// ErrCommandControlCharacters is returned when a command sent to a server
// contains control characters.
var ErrCommandControlCharacters = errors.Sentinel("environment/docker: command contains control characters")

// commandWriteTimeout is the amount of time allowed for a command to be written
// to the input of a server process.
var commandWriteTimeout = time.Second * 10

// A custom console writer that allows us to keep a function blocked until the
// given stream is properly closed. This does nothing special, only exists to
// make a noop io.Writer.
type noopWriter struct{}

var _ io.Writer = noopWriter{}

// Implement the required Write function to satisfy the io.Writer interface.
func (nw noopWriter) Write(b []byte) (int, error) {
	return len(b), nil
}

// Attach attaches to the docker container itself and ensures that we can pipe
// data in and out of the process stream. This should always be called before
// you have started the container, but after you've ensured it exists.
//
// Calling this function will poll resources for the container in the background
// until the container is stopped. The context provided to this function is used
// for the purposes of attaching to the container, a second context is created
// within the function for managing polling.
func (e *Environment) Attach(ctx context.Context) error {
	if e.IsAttached() {
		return nil
	}

	opts := client.ContainerAttachOptions{
		Stdin:  true,
		Stdout: true,
		Stderr: true,
		Stream: true,
	}

	// Set the stream again with the container.
	if st, err := e.client.ContainerAttach(ctx, e.Id, opts); err != nil {
		return errors.WrapIf(err, "environment/docker: error while attaching to container")
	} else {
		e.SetStream(&st.HijackedResponse)
	}

	go func() {
		// Don't use the context provided to the function, that'll cause the polling to
		// exit unexpectedly. We want a custom context for this, the one passed to the
		// function is to avoid a hang situation when trying to attach to a container.
		pollCtx, cancel := context.WithCancel(context.Background())
		defer cancel()
		defer e.stream.Close()
		defer func() {
			e.SetStream(nil)
			// The stream can end without the container stopping, in which case it is
			// attached to again rather than treating the server as stopped. A server
			// marked as stopped while its container keeps running would no longer be
			// managed, and its disk limit would not be enforced.
			if e.reattach() {
				return
			}
			e.SetState(environment.ProcessOfflineState)
		}()

		go func() {
			if err := e.pollResources(pollCtx); err != nil {
				if !errors.Is(err, context.Canceled) {
					e.log().WithField("error", err).Error("error during environment resource polling")
				} else {
					e.log().Warn("stopping server resource polling: context canceled")
				}
			}
		}()

		if err := system.ScanReader(e.stream.Reader, func(v []byte) {
			e.logCallbackMx.Lock()
			defer e.logCallbackMx.Unlock()
			e.logCallback(v)
		}); err != nil && err != io.EOF {
			log.WithField("error", err).WithField("container_id", e.Id).Warn("error processing scanner line in console output")
			return
		}
	}()

	return nil
}

// reattach is called when the attached stream ends, and attaches to the
// container again if it is still running, returning true if the container is
// still running. If that keeps happening, or attaching fails, the container is
// terminated rather than left running without being managed.
func (e *Environment) reattach() bool {
	ctx, cancel := context.WithTimeout(context.Background(), time.Second*30)
	defer cancel()

	c, err := e.ContainerInspect(ctx)
	if err != nil || c.State == nil || !c.State.Running {
		return false
	}

	e.mu.Lock()
	if time.Since(e.reattachedAt) > time.Minute*5 {
		e.reattachedAt = time.Now()
		e.reattachedCount = 0
	}
	e.reattachedCount++
	count := e.reattachedCount
	e.mu.Unlock()

	if count <= 3 {
		e.log().Warn("lost the attached stream for a running container, attaching again")
		err := e.Attach(ctx)
		if err == nil {
			return true
		}
		// The container may have only just stopped, in which case it is handled like
		// any other stop so that crash detection still applies.
		if c, ierr := e.ContainerInspect(ctx); ierr == nil && c.State != nil && !c.State.Running {
			return false
		}
		e.log().WithField("error", err).Error("failed to attach to running container")
	}

	e.log().Error("unable to stay attached to running container, terminating it")
	// Attaching may have used up the time allowed by ctx, which would leave the
	// container running without being managed.
	tctx, tcancel := context.WithTimeout(context.Background(), time.Second*30)
	defer tcancel()
	if err := e.Terminate(tctx, "SIGKILL"); err != nil {
		e.log().WithField("error", err).Error("failed to terminate container")
	}
	return true
}

// InSituUpdate performs an in-place update of the Docker container's resource
// limits without actually making any changes to the operational state of the
// container. This allows memory, cpu, and IO limitations to be adjusted on the
// fly for individual instances.
func (e *Environment) InSituUpdate() error {
	ctx, cancel := context.WithTimeout(context.Background(), time.Second*10)
	defer cancel()

	c, err := e.ContainerInspect(ctx)
	if err != nil {
		// If the container doesn't exist for some reason there really isn't anything
		// we can do to fix that in this process (it doesn't make sense at least). In those
		// cases just return without doing anything since we still want to save the configuration
		// to the disk.
		//
		// We'll let a boot process make modifications to the container if needed at this point.
		if errdefs.IsNotFound(err) {
			return nil
		}
		return errors.Wrap(err, "environment/docker: could not inspect container")
	}

	// The kernel rejects a CFS quota lower than the current burst, so remove the
	// burst before updating the limits and re-apply it afterwards.
	if c.State != nil {
		e.clearCpuBurst(c.ID, c.State.Pid)
	}

	// CPU pinning cannot be removed once it is applied to a container. The same is true
	// for removing memory limits, a container must be re-created.
	//
	// @see https://github.com/moby/moby/issues/41946
	if _, err := e.client.ContainerUpdate(ctx, e.Id, client.ContainerUpdateOptions{
		Resources: new(e.Configuration.Limits().AsContainerResources()),
	}); err != nil {
		return errors.Wrap(err, "environment/docker: could not update container")
	}
	e.applyCpuBurst(ctx)
	return nil
}

// Create creates a new container for the server using all the data that is
// currently available for it. If the container already exists it will be
// returned.
func (e *Environment) Create() error {
	ctx := context.Background()

	// If the container already exists don't hit the user with an error, just return
	// the current information about it which is what we would do when creating the
	// container anyways.
	if _, err := e.ContainerInspect(ctx); err == nil {
		return nil
	} else if !errdefs.IsNotFound(err) {
		return errors.WrapIf(err, "environment/docker: failed to inspect container")
	}

	// Try to pull the requested image before creating the container.
	if err := e.ensureImageExists(e.meta.Image); err != nil {
		return errors.WithStackIf(err)
	}

	cfg := config.Get()
	a := e.Configuration.Allocations()
	evs := e.Configuration.EnvironmentVariables()

	portBindings, err := a.DockerBindings()
	if err != nil {
		return errors.WithStackIf(err)
	}
	// DockerBindings never drops a port key, so its keys are the exposed ports.
	exposedPorts := network.PortSet{}
	for port := range portBindings {
		exposedPorts[port] = struct{}{}
	}

	// If port is 0 then we have a server with no allocation and this should stay 127.0.0.1 and not the docker network interface ip.
	if a.DefaultMapping.Port != 0 {
		for i, v := range evs {
			// Convert 127.0.0.1 to the pelican0 network interface if the environment is Docker
			// so that the server operates as expected.
			if v == "SERVER_IP=127.0.0.1" {
				evs[i] = "SERVER_IP=" + cfg.Docker.Network.Interface
			}
		}
	}

	// Merge user-provided labels with system labels
	confLabels := e.Configuration.Labels()
	labels := make(map[string]string, 2+len(confLabels))

	for key := range confLabels {
		labels[key] = confLabels[key]
	}
	labels["Service"] = "Pelican"
	labels["ContainerType"] = "server_process"

	conf := &container.Config{
		Hostname:     e.Id,
		Domainname:   cfg.Docker.Domainname,
		AttachStdin:  true,
		AttachStdout: true,
		AttachStderr: true,
		OpenStdin:    true,
		Tty:          true,
		ExposedPorts: exposedPorts,
		Image:        strings.TrimPrefix(e.meta.Image, "~"),
		Env:          e.Configuration.EnvironmentVariables(),
		Labels:       labels,
	}

	// Set the user running the container properly depending on what mode we are operating in.
	if cfg.System.User.Rootless.Enabled {
		conf.User = fmt.Sprintf("%d:%d", cfg.System.User.Rootless.ContainerUID, cfg.System.User.Rootless.ContainerGID)
	} else {
		conf.User = strconv.Itoa(cfg.System.User.Uid) + ":" + strconv.Itoa(cfg.System.User.Gid)
	}

	networkMode := container.NetworkMode(cfg.Docker.Network.Mode)
	if a.ForceOutgoingIP {
		// We can't use ForceOutgoingIP if we made a server with no allocation
		if a.DefaultMapping.Port != 0 {
			enableIPv6 := false
			e.log().Debug("environment/docker: forcing outgoing IP address")
			networkName := "ip-" + strings.ReplaceAll(strings.ReplaceAll(a.DefaultMapping.Ip, ".", "-"), ":", "-")
			networkMode = container.NetworkMode(networkName)

			if _, err := e.client.NetworkInspect(ctx, networkName, client.NetworkInspectOptions{}); err != nil {
				if !errdefs.IsNotFound(err) {
					return err
				}

				if _, err := e.client.NetworkCreate(ctx, networkName, client.NetworkCreateOptions{
					Driver:     "bridge",
					EnableIPv6: &enableIPv6,
					Internal:   false,
					Attachable: false,
					Ingress:    false,
					ConfigOnly: false,
					Options: map[string]string{
						"encryption": "false",
						"com.docker.network.bridge.default_bridge": "false",
						"com.docker.network.host_ipv4":             a.DefaultMapping.Ip,
					},
				}); err != nil {
					return err
				}
			}
		} else {
			e.log().Warn("environment/docker: Cannot force outgoing IP - server has no allocation")
		}
	}

	hostConf := &container.HostConfig{
		PortBindings: portBindings,

		// Configure the mounts for this container. First mount the server data directory
		// into the container as an r/w bind.
		Mounts: e.convertMounts(),

		// Configure the /tmp folder mapping in containers. This is necessary for some
		// games that need to make use of it for downloads and other installation processes.
		Tmpfs: map[string]string{
			"/tmp": "rw,exec,nosuid,size=" + strconv.Itoa(int(cfg.Docker.TmpfsSize)) + "M",
		},

		// Define resource limits for the container based on the data passed through
		// from the Panel.
		Resources: e.Configuration.Limits().AsContainerResources(),

		DNS: cfg.Docker.Network.DnsAddrs(),

		// Configure logging for the container to make it easier on the Daemon to grab
		// the server output. Ensure that we don't use too much space on the host machine
		// since we only need it for the last few hundred lines of output and don't care
		// about anything else in it.
		LogConfig: cfg.Docker.ContainerLogConfig(),

		SecurityOpt:    []string{"no-new-privileges"},
		ReadonlyRootfs: true,
		CapDrop: []string{
			"setpcap", "mknod", "audit_write", "net_raw", "dac_override",
			"fowner", "fsetid", "net_bind_service", "sys_chroot", "setfcap",
			"sys_ptrace",
		},
		NetworkMode: networkMode,
		UsernsMode:  container.UsernsMode(cfg.Docker.UsernsMode),
	}

	var netConf *network.NetworkingConfig // nil when no networking config is needed
	if serverNetConfig := config.Get().Docker.Network; serverNetConfig.Driver == "macvlan" {
		defaultMapping := e.Config().Allocations().DefaultMapping
		e.log().Debug("Set macvlan " + serverNetConfig.Name + " IP to " + defaultMapping.Ip)
		nc, err := MacvlanNetworkingConfig(serverNetConfig.Name, defaultMapping.Ip, serverNetConfig.Interfaces.V4.Gateway)
		if err != nil {
			return err
		}
		netConf = nc
	}
	// Pass the networkings configuration or nil if none required
	if _, err := e.client.ContainerCreate(ctx, client.ContainerCreateOptions{
		Config:           conf,
		HostConfig:       hostConf,
		NetworkingConfig: netConf,
		Name:             e.Id,
	}); err != nil {
		return errors.Wrap(err, "environment/docker: failed to create container")
	}

	return nil
}

// Destroy will remove the Docker container from the server. If the container
// is currently running it will be forcibly stopped by Docker.
func (e *Environment) Destroy() error {
	// We set it to stopping than offline to prevent crash detection from being triggered.
	e.SetState(environment.ProcessStoppingState)

	_, err := e.client.ContainerRemove(context.Background(), e.Id, client.ContainerRemoveOptions{
		RemoveVolumes: true,
		RemoveLinks:   false,
		Force:         true,
	})

	e.SetState(environment.ProcessOfflineState)

	// Don't trigger a destroy failure if we try to delete a container that does not
	// exist on the system. We're just a step ahead of ourselves in that case.
	//
	// @see https://github.com/pterodactyl/panel/issues/2001
	if err != nil && errdefs.IsNotFound(err) {
		return nil
	}

	return err
}

// SendCommand sends the specified command to the stdin of the running container
// instance. There is no confirmation that this data is sent successfully, only
// that it gets pushed into the stdin.
func (e *Environment) SendCommand(c string) error {
	// The container has a TTY, so control characters are interpreted rather than
	// passed to the process: ^C, ^\ and ^Z would signal the process directly.
	// Commands may not contain them, other than tabs and line endings.
	if strings.ContainsFunc(c, func(r rune) bool {
		return (r < 0x20 && r != '\t' && r != '\n' && r != '\r') || r == 0x7f
	}) {
		return ErrCommandControlCharacters
	}

	e.mu.RLock()
	stream := e.stream
	stop := e.meta.Stop
	e.mu.RUnlock()
	if stream == nil {
		return errors.Wrap(ErrNotAttached, "environment/docker: cannot send command to container")
	}

	// If the command being processed is the same as the process stop command then we
	// want to mark the server as entering the stopping state otherwise the process will
	// stop and Wings will think it has crashed and attempt to restart it.
	if stop.Type == "command" && c == stop.Value {
		e.SetState(environment.ProcessStoppingState)
	}

	// The process may never read its input, so don't wait forever for there to be
	// room for the command.
	_ = stream.Conn.SetWriteDeadline(time.Now().Add(commandWriteTimeout))
	_, err := stream.Conn.Write([]byte(c + "\n"))

	return errors.Wrap(err, "environment/docker: could not write to container stream")
}

// Readlog reads the log file for the server. This does not care if the server
// is running or not, it will simply try to read the last X bytes of the file
// and return them.
func (e *Environment) Readlog(lines int) ([]string, error) {
	r, err := e.client.ContainerLogs(context.Background(), e.Id, client.ContainerLogsOptions{
		ShowStdout: true,
		ShowStderr: true,
		Tail:       strconv.Itoa(lines),
	})
	if err != nil {
		return nil, errors.WithStack(err)
	}
	defer r.Close()

	var out []string
	scanner := bufio.NewScanner(r)
	for scanner.Scan() {
		out = append(out, scanner.Text())
	}

	return out, nil
}

// Pulls the image from Docker. If there is an error while pulling the image
// from the source but the image already exists locally, we will report that
// error to the logger but continue with the process.
//
// The reasoning behind this is that Quay has had some serious outages as of
// late, and we don't need to block all the servers from booting just because
// of that. I'd imagine in a lot of cases an outage shouldn't affect users too
// badly. It'll at least keep existing servers working correctly if anything.
func (e *Environment) ensureImageExists(image string) error {
	e.Events().Publish(environment.DockerImagePullStarted, "")
	defer e.Events().Publish(environment.DockerImagePullCompleted, "")

	// Images prefixed with a ~ are local images that we do not need to try and pull.
	if strings.HasPrefix(image, "~") {
		return nil
	}

	// Give it up to 15 minutes to pull the image. I think this should cover 99.8% of cases where an
	// image pull might fail. I can't imagine it will ever take more than 15 minutes to fully pull
	// an image. Let me know when I am inevitably wrong here...
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute*15)
	defer cancel()

	// Get a registry auth configuration from the config.
	registry, registryAuth := config.Get().Docker.RegistryCredentialsForImage(image)
	if registryAuth != nil {
		log.WithField("registry", registry).Debug("using authentication for registry")
	}

	imagePullOptions := client.ImagePullOptions{
		All:       false,
		Platforms: []ocispec.Platform{{OS: runtime.GOOS, Architecture: runtime.GOARCH}},
	}
	if registryAuth != nil {
		b64, err := registryAuth.Base64()
		if err != nil {
			log.WithError(err).Error("failed to get registry auth credentials")
		}

		// b64 is a string so if there is an error it will just be empty, not nil.
		imagePullOptions.RegistryAuth = b64
	}

	fallbackToLocal := func(pullErr error) error {
		images, err := e.client.ImageList(ctx, client.ImageListOptions{})
		if err != nil {
			return errors.Wrap(err, "environment/docker: failed to list images")
		}

		for _, img := range images.Items {
			for _, tag := range img.RepoTags {
				if tag != image {
					continue
				}

				log.WithFields(log.Fields{
					"image":        image,
					"container_id": e.Id,
					"err":          pullErr.Error(),
				}).Warn("unable to pull requested image from remote source, however the image exists locally")

				// Okay, we found a matching container image, in that case just go ahead and return
				// from this function, since there is nothing else we need to do here.
				return nil
			}
		}

		return errors.Wrapf(pullErr, "environment/docker: failed to pull %q image for server", image)
	}

	out, err := e.client.ImagePull(ctx, image, imagePullOptions)
	if err != nil {
		return fallbackToLocal(err)
	}
	defer out.Close()

	log.WithField("image", image).Debug("pulling docker image... this could take a bit of time")

	decoder := json.NewDecoder(out)
	for {
		var msg imagePullMessage
		if err := decoder.Decode(&msg); err != nil {
			if errors.Cause(err) == io.EOF || err == io.EOF {
				break
			}
			return errors.Wrap(err, "environment/docker: failed to read image pull progress")
		}

		if msg.ErrorDetail != nil && msg.ErrorDetail.Message != "" {
			return fallbackToLocal(fmt.Errorf("%s", msg.ErrorDetail.Message))
		}
		if msg.Error != "" {
			return fallbackToLocal(fmt.Errorf("%s", msg.Error))
		}
		if msg.ErrorDetail != nil {
			return fallbackToLocal(fmt.Errorf("image pull failed without an error message"))
		}

		progress := msg.Progress
		if p := msg.ProgressDetail; p != nil {
			progress = formatDockerProgress(p.Current, p.Total, p.Units)
		}

		status := strings.TrimSpace(strings.Join([]string{msg.ID, msg.Status, progress}, " "))
		if status != "" {
			e.Events().Publish(environment.DockerImagePullStatus, status)
		}
	}

	log.WithField("image", image).Debug("completed docker image pull")
	return nil
}

func (e *Environment) convertMounts() []mount.Mount {
	mounts := e.Configuration.Mounts()
	out := make([]mount.Mount, len(mounts))
	for i, m := range mounts {
		out[i] = mount.Mount{
			Type:     mount.TypeBind,
			Source:   m.Source,
			Target:   m.Target,
			ReadOnly: m.ReadOnly,
		}
	}
	return out
}

type imagePullMessage struct {
	ID             string `json:"id"`
	Status         string `json:"status"`
	Progress       string `json:"progress"` // Fallback for older daemons
	ProgressDetail *struct {
		Current int64  `json:"current"`
		Total   int64  `json:"total"`
		Units   string `json:"units"`
	} `json:"progressDetail"`
	Error       string `json:"error"`
	ErrorDetail *struct {
		Message string `json:"message"`
	} `json:"errorDetail"`
}

func formatDockerProgress(current, total int64, units string) string {
	const width = 30

	if current < 0 {
		current = 0
	}

	if units == "s" {
		return fmt.Sprintf("%ds elapsed", current)
	}
	if units != "" && units != "bytes" {
		return fmt.Sprintf("%d %s", current, units)
	}
	if total <= 0 {
		if current > 0 {
			return humanize.IBytes(uint64(current))
		}
		return ""
	}
	if current > total {
		current = total
	}

	ratio := float64(current) / float64(total)
	filled := int(ratio * width)
	bar := strings.Repeat("=", filled)
	if filled < width {
		bar += ">" + strings.Repeat(" ", width-filled-1)
	}

	return fmt.Sprintf("[%s] %d%% (%s/%s)",
		bar, int(ratio*100),
		humanize.IBytes(uint64(current)),
		humanize.IBytes(uint64(total)),
	)
}

// MacvlanNetworkingConfig returns the endpoint configuration that pins a
// container to ip on the named macvlan network. Wings only supports a single
// network per server, so the server's default allocation IP is used.
func MacvlanNetworkingConfig(networkName, ip, gateway string) (*network.NetworkingConfig, error) {
	addr, err := netip.ParseAddr(strings.TrimSpace(ip))
	if err != nil {
		return nil, errors.Wrapf(err, "environment/docker: invalid macvlan IP address %q", ip)
	}

	settings := &network.EndpointSettings{
		IPAMConfig: &network.EndpointIPAMConfig{IPv4Address: addr},
		IPAddress:  addr,
	}
	if gateway = strings.TrimSpace(gateway); gateway != "" {
		gw, err := netip.ParseAddr(gateway)
		if err != nil {
			return nil, errors.Wrapf(err, "environment/docker: invalid macvlan gateway %q", gateway)
		}
		settings.Gateway = gw
	}

	return &network.NetworkingConfig{
		EndpointsConfig: map[string]*network.EndpointSettings{networkName: settings},
	}, nil
}
