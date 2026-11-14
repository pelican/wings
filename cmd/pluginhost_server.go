package cmd

import (
	"context"
	"encoding/json"
	"net"
	"strconv"

	"github.com/pelican/wings/server"
)

// allowedPowerActions is the accepted set for host.server.power.
var allowedPowerActions = map[string]bool{"start": true, "stop": true, "restart": true, "kill": true}

func init() {
	// --- read / projected ---

	register("resolveAddress", true, func(_ pluginHost, _ context.Context, s *server.Server, _ json.RawMessage) (any, error) {
		a := defaultAddress(s)
		if a.IP == "" {
			return nil, rpcErr(-32000, "server has no default allocation")
		}
		return map[string]string{"address": net.JoinHostPort(a.IP, strconv.Itoa(a.Port))}, nil
	})

	register("server.list", false, func(h pluginHost, _ context.Context, _ *server.Server, _ json.RawMessage) (any, error) {
		return h.list(), nil
	})

	register("server.get", true, func(h pluginHost, _ context.Context, s *server.Server, _ json.RawMessage) (any, error) {
		return h.info(s), nil
	})

	register("server.status", true, func(_ pluginHost, _ context.Context, s *server.Server, _ json.RawMessage) (any, error) {
		return serverStatus{
			Running:      s.IsRunning(),
			Suspended:    s.IsSuspended(),
			Installing:   s.IsInstalling(),
			Transferring: s.IsTransferring(),
			Restoring:    s.IsRestoring(),
			Protected:    s.IsInProtectedState(),
		}, nil
	})

	register("server.state", true, func(_ pluginHost, _ context.Context, s *server.Server, _ json.RawMessage) (any, error) {
		return map[string]string{"state": s.Environment.State()}, nil
	})

	register("server.stats", true, func(_ pluginHost, _ context.Context, s *server.Server, _ json.RawMessage) (any, error) {
		// Same shape clients already receive on the stats event bus.
		return s.Proc(), nil
	})

	register("server.allocations", true, func(_ pluginHost, _ context.Context, s *server.Server, _ json.RawMessage) (any, error) {
		a := s.Config().Allocations
		return allocationsView{
			Default:  defaultAddress(s),
			Mappings: a.Mappings,
		}, nil
	})

	register("server.logs", true, func(_ pluginHost, _ context.Context, s *server.Server, params json.RawMessage) (any, error) {
		args, err := decode[logsArgs](params)
		if err != nil {
			return nil, err
		}
		n := args.Lines
		if n <= 0 || n > 500 {
			n = 100
		}
		lines, err := s.ReadLogfile(n)
		if err != nil {
			return nil, rpcErr(-32000, err.Error())
		}
		return map[string]any{"lines": lines}, nil
	})

	// --- mutating (grant-gated in plugin.HostHandler) ---

	register("server.power", true, func(_ pluginHost, _ context.Context, s *server.Server, params json.RawMessage) (any, error) {
		args, err := decode[powerArgs](params)
		if err != nil {
			return nil, err
		}
		if !allowedPowerActions[args.Action] {
			return nil, rpcErr(-32602, "invalid power action")
		}
		if err := s.HandlePowerAction(server.PowerAction(args.Action)); err != nil {
			return nil, rpcErr(-32000, err.Error())
		}
		return okResult, nil
	})

	register("server.command", true, func(_ pluginHost, _ context.Context, s *server.Server, params json.RawMessage) (any, error) {
		args, err := decode[commandArgs](params)
		if err != nil {
			return nil, err
		}
		if err := s.Environment.SendCommand(args.Command); err != nil {
			return nil, rpcErr(-32000, err.Error())
		}
		return okResult, nil
	})

	// events.publish force-prefixes the topic with "plugin:" so a plugin can
	// never spoof a reserved Wings topic (status, stats, console output, ...).
	register("events.publish", true, func(_ pluginHost, _ context.Context, s *server.Server, params json.RawMessage) (any, error) {
		args, err := decode[publishArgs](params)
		if err != nil {
			return nil, err
		}
		if args.Topic == "" {
			return nil, rpcErr(-32602, "topic is required")
		}
		s.Events().Publish("plugin:"+args.Topic, args.Data)
		return okResult, nil
	})
}

type logsArgs struct {
	Lines int `json:"lines"`
}

type powerArgs struct {
	Action string `json:"action"`
}

type commandArgs struct {
	Command string `json:"command"`
}

type publishArgs struct {
	Topic string `json:"topic"`
	Data  any    `json:"data"`
}
