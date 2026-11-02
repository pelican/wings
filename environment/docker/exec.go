package docker

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"strconv"
	"time"

	"emperror.dev/errors"
	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/pkg/stdcopy"

	"github.com/pelican/wings/config"
)

// ExecResult is the outcome of running a command inside a container.
type ExecResult struct {
	Stdout   string
	Stderr   string
	ExitCode int

	// Truncated reports whether output was cut short at the requested limit.
	Truncated bool
}

// ExecOptions bounds one exec.
type ExecOptions struct {
	// Timeout stops waiting for the command. The process inside the container
	// is not killed, because Docker offers no way to signal an exec; it is
	// left running and its output discarded.
	Timeout time.Duration

	// MaxOutputBytes caps stdout and stderr together.
	MaxOutputBytes int64

	// WorkingDir runs the command somewhere other than the container's
	// default directory.
	WorkingDir string
}

// Exec runs a command inside the server's container and returns its output.
//
// This is deliberately not the console. Writing to a server's stdin means
// talking to whatever is already running and guessing at its replies from the
// console stream; an exec gets its own stdout, stderr and exit code, which is
// what makes a health check or a one-off query possible at all.
//
// The command runs as the same user the container runs as, never as root.
// There is no option for that: a container's user is the boundary that keeps a
// server's own files from being the host's, and an exec is not a reason to
// cross it.
func (e *Environment) Exec(ctx context.Context, cmd []string, opts ExecOptions) (ExecResult, error) {
	if len(cmd) == 0 {
		return ExecResult{}, errors.New("environment/docker: no command was given to exec")
	}

	if opts.Timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, opts.Timeout)
		defer cancel()
	}

	if running, err := e.IsRunning(ctx); err != nil {
		return ExecResult{}, errors.WrapIf(err, "environment/docker: could not determine whether the server is running")
	} else if !running {
		return ExecResult{}, errors.New("environment/docker: the server is not running")
	}

	created, err := e.client.ContainerExecCreate(ctx, e.Id, container.ExecOptions{
		Cmd:          cmd,
		AttachStdout: true,
		AttachStderr: true,
		AttachStdin:  false,
		Tty:          false,
		WorkingDir:   opts.WorkingDir,
		User:         e.containerUser(),
	})
	if err != nil {
		return ExecResult{}, errors.WrapIf(err, "environment/docker: could not create exec")
	}

	attached, err := e.client.ContainerExecAttach(ctx, created.ID, container.ExecAttachOptions{})
	if err != nil {
		return ExecResult{}, errors.WrapIf(err, "environment/docker: could not attach to exec")
	}
	defer attached.Close()

	limit := opts.MaxOutputBytes
	if limit <= 0 {
		limit = 256 * 1024
	}

	var stdout, stderr bytes.Buffer

	// Docker multiplexes the two streams over one connection unless a TTY was
	// requested, and a TTY would merge them. Demultiplexing keeps stderr
	// distinguishable, which is usually where the reason for a failure is.
	//
	// The limit is applied to the connection rather than to each buffer, so a
	// command producing endless output cannot be read forever. Reading one
	// byte past it is what lets truncation be reported instead of a partial
	// answer being passed off as the whole thing.
	_, copyErr := stdcopy.StdCopy(&stdout, &stderr, io.LimitReader(attached.Reader, limit+1))

	result := ExecResult{
		Stdout:    capString(stdout.String(), limit),
		Stderr:    capString(stderr.String(), limit),
		Truncated: int64(stdout.Len()+stderr.Len()) > limit,
	}

	if copyErr != nil && !errors.Is(copyErr, io.EOF) {
		// A timeout closes the connection under the copy, so report what was
		// captured along with why it stopped.
		if ctx.Err() != nil {
			return result, errors.WrapIf(ctx.Err(), "environment/docker: exec did not finish in time")
		}
		return result, errors.WrapIf(copyErr, "environment/docker: could not read exec output")
	}

	inspected, err := e.client.ContainerExecInspect(ctx, created.ID)
	if err != nil {
		return result, errors.WrapIf(err, "environment/docker: could not read the exec's exit code")
	}
	result.ExitCode = inspected.ExitCode

	return result, nil
}

// containerUser returns the user the container runs as, in Docker's
// "uid:gid" form, matching how the container itself was created.
func (e *Environment) containerUser() string {
	cfg := config.Get()

	if cfg.System.User.Rootless.Enabled {
		return fmt.Sprintf("%d:%d", cfg.System.User.Rootless.ContainerUID, cfg.System.User.Rootless.ContainerGID)
	}
	return strconv.Itoa(cfg.System.User.Uid) + ":" + strconv.Itoa(cfg.System.User.Gid)
}

func capString(s string, limit int64) string {
	if int64(len(s)) <= limit {
		return s
	}
	return s[:limit]
}
