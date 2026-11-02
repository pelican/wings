package server

import (
	"bytes"
	"io"
	"strings"

	"emperror.dev/errors"

	"github.com/pelican/wings/internal/ufs"
	"github.com/pelican/wings/parser"
	"github.com/pelican/wings/plugins"
	"github.com/pelican/wings/plugins/api"
)

// PluginSnapshot renders the server as the value plugin hooks receive.
//
// It is a copy rather than a handle: a plugin cannot reach back into daemon
// state through what it was given, and a hook that takes a while cannot be
// surprised by the server changing underneath it. The cost is that a hook sees
// the server as it was when the hook fired, which is the right trade for
// something arbitrary code is handed.
func (s *Server) PluginSnapshot() api.Server {
	cfg := s.Config()

	out := api.Server{
		UUID:         s.ID(),
		ID:           cfg.Pid,
		Name:         cfg.Meta.Name,
		Description:  cfg.Meta.Description,
		State:        s.Environment.State(),
		Suspended:    s.IsSuspended(),
		Invocation:   cfg.Invocation,
		Image:        cfg.Container.Image,
		EggID:        cfg.Egg.ID,
		Installing:   s.IsInstalling(),
		Restoring:    s.IsRestoring(),
		Transferring: s.IsTransferring(),
		FileDenylist: append([]string(nil), cfg.Egg.FileDenylist...),
		Limits: api.Limits{
			MemoryMiB:  cfg.Build.MemoryLimit,
			SwapMiB:    cfg.Build.Swap,
			DiskMiB:    cfg.Build.DiskSpace,
			IOWeight:   cfg.Build.IoWeight,
			CPUPercent: cfg.Build.CpuLimit,
			Threads:    cfg.Build.Threads,
			OOMKiller:  cfg.Build.OOMKiller,
		},
	}

	if len(cfg.Labels) > 0 {
		out.Labels = make(map[string]string, len(cfg.Labels))
		for k, v := range cfg.Labels {
			out.Labels[k] = v
		}
	}

	if m := cfg.Allocations.DefaultMapping; m != nil {
		out.Allocation = api.Allocation{IP: m.Ip, Port: m.Port}
	}

	for ip, ports := range cfg.Allocations.Mappings {
		for _, port := range ports {
			out.Allocations = append(out.Allocations, api.Allocation{IP: ip, Port: port})
		}
	}

	// Environment values arrive from the Panel as whatever JSON type they were
	// written as, but reach a container as strings, so they are rendered here
	// the way the container will see them.
	if len(cfg.EnvVars) > 0 {
		out.Environment = make(map[string]string, len(cfg.EnvVars))
		for k := range cfg.EnvVars {
			out.Environment[k] = cfg.EnvVars.Get(k)
		}
	}

	for _, m := range cfg.Mounts {
		out.Mounts = append(out.Mounts, api.Mount{
			Source:   m.Source,
			Target:   m.Target,
			ReadOnly: m.ReadOnly,
			Default:  m.Default,
		})
	}

	return out
}

// parseWithPlugin gives a plugin-provided parser the chance to rewrite an egg
// configuration file.
//
// It reports whether a plugin handled the file. When none did, the caller falls
// back to Wings' own parser, so registering a parser for a format Wings already
// knows overrides it and registering one for a new format adds it, with no
// difference in how they are wired.
//
// A plugin that fails is reported rather than silently falling back. Falling
// back would boot the server with a configuration file the operator believes
// their plugin rewrote, which is a worse outcome than a visible error.
func (s *Server) parseWithPlugin(f parser.ConfigurationFile, filename string, file ufs.File) (bool, error) {
	m := plugins.Active()
	if m == nil || !m.Enabled() {
		return false, nil
	}

	inst, parse, ok := m.ConfigParser(string(f.Parser))
	if !ok {
		return false, nil
	}

	st, err := file.Stat()
	if err != nil {
		return false, err
	}
	// The same ceiling the built-in parsers apply. A plugin parser buffers the
	// file just as they do, and the contents are server-owned input.
	if st.Size() > maxPluginConfigFileSize {
		return false, errors.Errorf("server: refusing to hand %s to a plugin parser: it is larger than %d bytes", filename, maxPluginConfigFileSize)
	}

	content, err := io.ReadAll(io.LimitReader(file, maxPluginConfigFileSize))
	if err != nil {
		return false, err
	}

	// Replacements are flattened to the dotted key and the value the egg wants,
	// which is the shape a parser actually needs; the full match syntax is
	// Wings' own concern.
	replacements := make(map[string]string, len(f.Replace))
	for _, r := range f.Replace {
		replacements[r.Match] = r.ReplaceWith.String()
	}

	var (
		out      []byte
		parseErr error
	)

	if !inst.InvokeRoute(func() {
		out, parseErr = parse(api.ConfigFile{
			Server:       s.PluginSnapshot(),
			Path:         filename,
			Content:      content,
			Replacements: replacements,
		})
	}) {
		return false, errors.New("server: the plugin parser panicked")
	}

	if parseErr != nil {
		return false, parseErr
	}

	// A parser returning nothing is taken as "leave the file alone", which is
	// how a plugin declines a file it does not recognise without failing.
	if out == nil {
		return true, nil
	}

	if err := s.Filesystem().Write(filename, bytes.NewReader(out), int64(len(out)), 0o644); err != nil {
		return false, err
	}

	return true, nil
}

// maxPluginConfigFileSize matches the parser package's own ceiling.
const maxPluginConfigFileSize = 64 * 1024 * 1024

// applyPluginStartup lets plugins adjust the command and environment a server
// is about to boot with.
//
// This runs on every boot, unlike the container hook, which only fires when a
// container is created and then reused. That makes it the place for anything
// that has to track a server's current configuration rather than whatever it
// was when its container was first built.
//
// The variables Wings sets itself are patchable, because overriding STARTUP is
// the entire point for a plugin adding flags to a startup command, and a plugin
// that breaks SERVER_PORT breaks only its own node's servers in a way its
// operator can undo by disabling it.
func (s *Server) applyPluginStartup(env []string) []string {
	if !plugins.HasStartupHooks() {
		return env
	}

	// Split into a map for the hook, keeping the order so the result is stable
	// between boots rather than reshuffling on every start.
	order := make([]string, 0, len(env))
	current := make(map[string]string, len(env))

	for _, entry := range env {
		k, v, found := strings.Cut(entry, "=")
		if !found {
			continue
		}
		if _, seen := current[k]; !seen {
			order = append(order, k)
		}
		current[k] = v
	}

	patch := plugins.MutateStartup(s.PluginSnapshot(), api.Startup{
		Command:     current["STARTUP"],
		Environment: current,
	})

	if patch.Command != "" {
		current["STARTUP"] = patch.Command
	}

	for k, v := range patch.Environment {
		k = strings.ToUpper(k)
		if _, seen := current[k]; !seen {
			order = append(order, k)
		}
		current[k] = v
	}

	out := make([]string, 0, len(order))
	for _, k := range order {
		out = append(out, k+"="+current[k])
	}
	return out
}
