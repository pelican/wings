package kubernetes

import (
	"context"
	"fmt"
	"io"
	"sync"

	"emperror.dev/errors"
	"github.com/apex/log"
	"k8s.io/client-go/kubernetes"

	"github.com/pelican/wings/config"
	"github.com/pelican/wings/environment"
	"github.com/pelican/wings/events"
	"github.com/pelican/wings/remote"
	"github.com/pelican/wings/system"
)

// Metadata holds runtime metadata for the Kubernetes environment that can be
// updated on the fly (e.g., image changes from the Panel).
type Metadata struct {
	Image string
	Stop  remote.ProcessStopConfiguration
}

// Ensure that the Kubernetes environment always implements the full
// ProcessEnvironment interface.
var _ environment.ProcessEnvironment = (*Environment)(nil)

// Environment is the Kubernetes implementation of ProcessEnvironment. It
// manages game server workloads as Pods within a configured namespace.
type Environment struct {
	mu sync.RWMutex

	// Id is the unique server identifier (UUID) used as the Pod name.
	Id string

	// Configuration holds the environment settings (limits, mounts, env vars).
	Configuration *environment.Configuration

	meta *Metadata

	// client is the Kubernetes clientset.
	client kubernetes.Interface

	// stream holds the attach connection to the running Pod's stdin.
	stream io.WriteCloser

	emitter *events.Bus

	logCallbackMx sync.Mutex
	logCallback   func([]byte)

	// st tracks the current process state.
	st *system.AtomicString
}

// New creates a new Kubernetes environment for the given server ID.
func New(id string, m *Metadata, c *environment.Configuration) (*Environment, error) {
	cli, err := Client()
	if err != nil {
		return nil, err
	}

	e := &Environment{
		Id:            id,
		Configuration: c,
		meta:          m,
		client:        cli,
		st:            system.NewAtomicString(environment.ProcessOfflineState),
		emitter:       events.NewBus(),
	}

	return e, nil
}

func (e *Environment) log() *log.Entry {
	return log.WithField("environment", e.Type()).WithField("pod_id", e.Id)
}

// Type returns the environment type identifier.
func (e *Environment) Type() string {
	return "kubernetes"
}

// Events returns the event bus for this environment.
func (e *Environment) Events() *events.Bus {
	return e.emitter
}

// Config returns the environment configuration.
func (e *Environment) Config() *environment.Configuration {
	e.mu.RLock()
	defer e.mu.RUnlock()
	return e.Configuration
}

// State returns the current process state string.
func (e *Environment) State() string {
	return e.st.Load()
}

// SetState updates the environment state and publishes a state change event.
func (e *Environment) SetState(state string) {
	if state != environment.ProcessOfflineState &&
		state != environment.ProcessStartingState &&
		state != environment.ProcessRunningState &&
		state != environment.ProcessStoppingState {
		panic(errors.New(fmt.Sprintf("invalid server state received: %s", state)))
	}

	if e.State() != state {
		e.st.Store(state)
		e.Events().Publish(environment.StateChangeEvent, state)
	}
}

// SetLogCallback sets the callback function for container log output.
func (e *Environment) SetLogCallback(f func([]byte)) {
	e.logCallbackMx.Lock()
	defer e.logCallbackMx.Unlock()
	e.logCallback = f
}

// SetStopConfiguration updates the stop configuration on the fly.
func (e *Environment) SetStopConfiguration(c remote.ProcessStopConfiguration) {
	e.mu.Lock()
	e.meta.Stop = c
	e.mu.Unlock()
}

// SetImage updates the container image for the server.
func (e *Environment) SetImage(i string) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.meta.Image = i
}

// IsAttached returns whether the environment is currently attached to the Pod.
func (e *Environment) IsAttached() bool {
	e.mu.RLock()
	defer e.mu.RUnlock()
	return e.stream != nil
}

// setStream updates the attach stream reference.
func (e *Environment) setStream(s io.WriteCloser) {
	e.mu.Lock()
	e.stream = s
	e.mu.Unlock()
}

// Exists checks whether the Pod for this server exists in the cluster.
func (e *Environment) Exists() (bool, error) {
	_, err := e.getPod(context.Background())
	if err != nil {
		if isNotFound(err) {
			return false, nil
		}
		return false, err
	}
	return true, nil
}

// IsRunning checks whether the Pod is in Running phase.
func (e *Environment) IsRunning(ctx context.Context) (bool, error) {
	pod, err := e.getPod(ctx)
	if err != nil {
		return false, err
	}
	return isPodRunning(pod), nil
}

// ExitState returns the exit code and OOM-killed status of the terminated
// container.
func (e *Environment) ExitState() (uint32, bool, error) {
	pod, err := e.getPod(context.Background())
	if err != nil {
		if isNotFound(err) {
			return 1, false, nil
		}
		return 0, false, errors.WrapIf(err, "environment/kubernetes: failed to get pod")
	}

	for _, cs := range pod.Status.ContainerStatuses {
		if cs.Name == "server" && cs.State.Terminated != nil {
			oom := cs.State.Terminated.Reason == "OOMKilled"
			return uint32(cs.State.Terminated.ExitCode), oom, nil
		}
	}

	return 0, false, nil
}

// InSituUpdate is a no-op for Kubernetes since Pod resource limits are
// immutable after creation. The Pod must be recreated to apply new limits.
func (e *Environment) InSituUpdate() error {
	return nil
}

// namespace returns the configured Kubernetes namespace.
func (e *Environment) namespace() string {
	ns := config.Get().Kubernetes.Namespace
	if ns == "" {
		return "pelican"
	}
	return ns
}
