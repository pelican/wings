package docker

import (
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"sync"

	"emperror.dev/errors"
	cerrdefs "github.com/containerd/errdefs"
	"github.com/containerd/errdefs/pkg/errhttp"
	"github.com/goccy/go-json"
	"github.com/moby/moby/api/types/common"
	"github.com/moby/moby/api/types/container"
	"github.com/moby/moby/client"

	"github.com/pelican/wings/config"
)

var (
	o           sync.Once
	cli         cliSettings
	fastEnabled bool
)

type cliSettings struct {
	// http is a client that talks to the same daemon as the Docker client,
	// dialing through the Docker client's own dialer so unix sockets, named
	// pipes and TLS connections all behave the same way.
	http *http.Client
	// host is the value sent as the request host.
	host string
}

func configure(c *client.Client) {
	o.Do(func() {
		fastEnabled = config.Get().Docker.UsePerformantInspect
		if !fastEnabled {
			return
		}

		u, err := client.ParseHostURL(c.DaemonHost())
		if err != nil {
			// Fall back to the standard client if the host cannot be parsed.
			fastEnabled = false
			return
		}

		cli.host = u.Host
		if u.Scheme == "unix" || u.Scheme == "npipe" {
			cli.host = "docker"
		}

		dial := c.Dialer()
		cli.http = &http.Client{
			Transport: &http.Transport{
				DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
					return dial(ctx)
				},
			},
		}
	})
}

// ContainerInspect is a rough equivalent of Docker's client.ContainerInspect()
// but re-written to use a more performant JSON decoder. This is important since
// a large number of requests to this endpoint are spawned by Wings, and the
// standard "encoding/json" shows its performance woes badly even with single
// containers running.
func (e *Environment) ContainerInspect(ctx context.Context) (container.InspectResponse, error) {
	configure(e.client)

	// Support feature flagging of this functionality so that if something goes
	// wrong for now it is easy enough for people to switch back to the older method
	// of fetching stats.
	if !fastEnabled {
		res, err := e.client.ContainerInspect(ctx, e.Id, client.ContainerInspectOptions{})
		return res.Container, err
	}

	var st container.InspectResponse
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "http://"+cli.host+"/containers/"+e.Id+"/json", nil)
	if err != nil {
		return st, errors.WithStack(err)
	}

	res, err := cli.http.Do(req)
	if err != nil {
		return st, fmt.Errorf("%w: %w", cerrdefs.ErrUnknown, err)
	}
	defer res.Body.Close()

	body, err := io.ReadAll(res.Body)
	if err != nil {
		return st, errors.Wrap(err, "failed to read response body from Docker")
	}
	if err := parseErrorFromResponse(res, body); err != nil {
		return st, fmt.Errorf("%w: %w", errhttp.ToNative(res.StatusCode), err)
	}
	if err := json.Unmarshal(body, &st); err != nil {
		return st, errors.WithStack(err)
	}
	return st, nil
}

// parseErrorFromResponse is a re-implementation of Docker's
// client.checkResponseErr() function.
func parseErrorFromResponse(res *http.Response, body []byte) error {
	if res.StatusCode >= 200 && res.StatusCode < 400 {
		return nil
	}

	var ct string
	if res.Header != nil {
		ct = res.Header.Get("Content-Type")
	}

	var emsg string
	if ct == "application/json" {
		var errResp common.ErrorResponse
		if err := json.Unmarshal(body, &errResp); err != nil {
			return errors.WithStack(err)
		}
		emsg = strings.TrimSpace(errResp.Message)
	} else {
		emsg = strings.TrimSpace(string(body))
	}

	return errors.Wrap(errors.New(emsg), "Error response from daemon")
}
