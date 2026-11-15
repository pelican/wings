package server

import (
	"bytes"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/apex/log"

	"github.com/pelican/wings/events"
	"github.com/pelican/wings/plugins"
	"github.com/pelican/wings/plugins/api"
	"github.com/pelican/wings/system"

	"github.com/pelican/wings/environment"
	"github.com/pelican/wings/remote"
)

var dockerEvents = []string{
	environment.DockerImagePullStatus,
	environment.DockerImagePullStarted,
	environment.DockerImagePullCompleted,
}

type diskSpaceLimiter struct {
	o      sync.Once
	mu     sync.Mutex
	server *Server
}

type FeatureMatchPayload struct {
	Key     string `json:"key"`
	Pattern string `json:"pattern"`
	Line    string `json:"line"`
}

func newDiskLimiter(s *Server) *diskSpaceLimiter {
	return &diskSpaceLimiter{server: s}
}

// Reset the disk space limiter status.
func (dsl *diskSpaceLimiter) Reset() {
	dsl.mu.Lock()
	dsl.o = sync.Once{}
	dsl.mu.Unlock()
}

// Trigger the disk space limiter which will attempt to stop a running server instance within
// 15 seconds, and terminate it forcefully if it does not stop.
//
// This function is only executed one time, so whenever a server is marked as booting the limiter
// should be reset, so it can properly be triggered as needed.
func (dsl *diskSpaceLimiter) Trigger() {
	dsl.o.Do(func() {
		dsl.server.PublishConsoleOutputFromDaemon("Server is exceeding the assigned disk space limit, stopping process now.")
		if err := dsl.server.Environment.WaitForStop(dsl.server.Context(), time.Minute, true); err != nil {
			dsl.server.Log().WithField("error", err).Error("failed to stop server after exceeding space limit!")
		}
	})
}

// processConsoleOutputEvent handles output from a server's Docker container
// and runs through different limiting logic to ensure that spam console output
// does not cause negative effects to the system. This will also monitor the
// output lines to determine if the server is started yet, and if the output is
// not being throttled, will send the data over to the websocket.
func (s *Server) processConsoleOutputEvent(v []byte) {
	// Always process the console output, but do this in a seperate thread since we
	// don't really care about side-effects from this call, and don't want it to block
	// the console sending logic.
	go s.onConsoleOutput(v)

	// If the console is being throttled, do nothing else with it, we don't want
	// to waste time. This code previously terminated server instances after violating
	// different throttle limits. That code was clunky and difficult to reason about,
	// in addition to being a consistent pain point for users.
	//
	// In the interest of building highly efficient software, that code has been removed
	// here, and we'll rely on the host to detect bad actors through their own means.
	if !s.Throttler().Allow() {
		return
	}

	// Offer the line to plugins, which may rewrite it or drop it. This runs
	// after throttling so a plugin is never handed output that nobody was
	// going to see anyway, and the check is guarded because building the
	// server snapshot is far more expensive than the check itself. On a node
	// with no console plugins this is a single atomic load.
	if plugins.HasConsoleHooks() {
		line, keep := plugins.ConsoleLine(s.PluginSnapshot(), string(v))
		if !keep {
			return
		}
		v = []byte(line)
	}

	s.Sink(system.LogSink).Push(v)
}

// StartEventListeners adds all the internal event listeners we want to use for
// a server. These listeners can only be removed by deleting the server as they
// should last for the duration of the process' lifetime.
func (s *Server) StartEventListeners() {
	c := make(chan []byte, 8)
	limit := newDiskLimiter(s)

	s.Log().Debug("registering event listeners: console, state, resources...")
	s.Environment.Events().On(c)
	s.Environment.SetLogCallback(s.processConsoleOutputEvent)

	go func() {
		for {
			select {
			case v := <-c:
				go func(v []byte, limit *diskSpaceLimiter) {
					var e events.Event
					if err := events.DecodeTo(v, &e); err != nil {
						return
					}
					switch e.Topic {
					case environment.ResourceEvent:
						{
							var stats struct {
								Topic string
								Data  environment.Stats
							}
							if err := events.DecodeTo(v, &stats); err != nil {
								s.Log().WithField("error", err).Warn("failed to decode server resource event")
								return
							}
							s.resources.UpdateStats(stats.Data)

							// Offer the sample to plugins. Guarded because
							// this fires for every running server on a fixed
							// interval, and building the snapshot costs more
							// than the check.
							if plugins.HasStatsHooks() {
								plugins.Stats(s.PluginSnapshot(), api.Stats{
									MemoryBytes:      stats.Data.Memory,
									MemoryLimitBytes: stats.Data.MemoryLimit,
									CPUPercent:       stats.Data.CpuAbsolute,
									DiskBytes:        s.Proc().Disk,
									NetworkRxBytes:   stats.Data.Network.RxBytes,
									NetworkTxBytes:   stats.Data.Network.TxBytes,
									UptimeMillis:     stats.Data.Uptime,
								})
							}
							// If there is no disk space available at this point, trigger the server
							// disk limiter logic which will start to stop the running instance.
							if !s.Filesystem().HasSpaceAvailable(true) {
								limit.Trigger()
							}
							s.Events().Publish(StatsEvent, s.Proc())
						}
					case environment.StateChangeEvent:
						{
							// Reset the throttler when the process is started.
							if e.Data == environment.ProcessStartingState {
								limit.Reset()
								s.Throttler().Reset()
							}
							s.OnStateChange()
						}
					case environment.DockerImagePullStatus:
						s.Events().Publish(InstallOutputEvent, e.Data)
					case environment.DockerImagePullStarted:
						s.PublishConsoleOutputFromDaemon("Pulling Docker container image, this could take a few minutes to complete...")
					case environment.DockerImagePullCompleted:
						s.PublishConsoleOutputFromDaemon("Finished pulling Docker container image")
					default:
					}
				}(v, limit)
			case <-s.Context().Done():
				return
			}
		}
	}()
}

var stripAnsiRegex = regexp.MustCompile("[\u001B\u009B][[\\]()#;?]*(?:(?:(?:[a-zA-Z\\d]*(?:;[a-zA-Z\\d]*)*)?\u0007)|(?:(?:\\d{1,4}(?:;\\d{0,4})*)?[\\dA-PRZcf-ntqry=><~]))")

// Custom listener for console output events that will check if the given line
// of output matches one that should mark the server as started or not.
func (s *Server) onConsoleOutput(data []byte) {
	if s.Environment.State() != environment.ProcessStartingState && !s.IsRunning() {
		return
	}

	processConfiguration := s.ProcessConfiguration()
	EggConfiguration := s.cfg.Egg.Features

	// Make a copy of the data provided since it is by reference, otherwise you'll
	// potentially introduce a race condition by modifying the value.
	v := make([]byte, len(data))
	copy(v, data)

	// Check if the server is currently starting.
	if s.Environment.State() == environment.ProcessStartingState {
		// Check if we should strip ansi color codes.
		if processConfiguration.Startup.StripAnsi {
			v = stripAnsiRegex.ReplaceAll(v, []byte(""))
		}

		// Iterate over all the done lines.
		for _, l := range processConfiguration.Startup.Done {
			if !l.Matches(v) {
				continue
			}

			s.Log().WithFields(log.Fields{
				"match":   l.String(),
				"against": strconv.QuoteToASCII(string(v)),
			}).Debug("detected server in running state based on console line output")

			// If the specific line of output is one that would mark the server as started,
			// set the server to that state. Only do this if the server is not currently stopped
			// or stopping.
			s.Environment.SetState(environment.ProcessRunningState)
			break
		}
	}

	// Check if this Egg has Features configured that we need to listen for.
	if EggConfiguration != nil {
		// Check if we should strip ansi color codes.
		if processConfiguration.Startup.StripAnsi {
			v = stripAnsiRegex.ReplaceAll(v, []byte(""))
		}

		// Convert the console output to a string for easier pattern matching.
		output := string(v)

		outputLower := strings.ToLower(output)

	foundMatch:
		for key, patterns := range EggConfiguration {
			for _, pattern := range patterns {
				patternLower := strings.ToLower(pattern)
				// Check if the current line contains a defined feature match string.
				if strings.Contains(outputLower, patternLower) {
					// Send a WebSocket event to notify the frontend of the match.
					// This can be used for triggering UI behaviors or user prompts.
					s.Events().Publish(
						FeatureMatchEvent,
						FeatureMatchPayload{
							Key:     key,
							Pattern: pattern,
							Line:    output,
						},
					)

					// Log the match internally for debugging or tracing purposes.
					s.Log().WithFields(log.Fields{
						"match":   pattern,
						"key":     key,
						"against": strconv.QuoteToASCII(output),
					}).Debug("Feature: detected based on console line output")

					// Break out of both loops — we only want one match trigger per console line.
					break foundMatch
				}
			}
		}
	}

	// If the command sent to the server is one that should stop the server we will need to
	// set the server to be in a stopping state, otherwise crash detection will kick in and
	// cause the server to unexpectedly restart on the user.
	if s.IsRunning() {
		stop := processConfiguration.Stop

		if stop.Type == remote.ProcessStopCommand && bytes.Equal(v, []byte(stop.Value)) {
			s.Environment.SetState(environment.ProcessOfflineState)
		}
	}
}
