package server

import (
	"fmt"
	"strconv"
	"sync"
	"time"

	"emperror.dev/errors"
	"github.com/apex/log"

	"github.com/pelican/wings/config"
	"github.com/pelican/wings/environment"
	"github.com/pelican/wings/internal/models"
	"github.com/pelican/wings/plugins"
	"github.com/pelican/wings/plugins/api"
)

type CrashHandler struct {
	mu sync.RWMutex

	// Tracks the time of the last server crash event.
	lastCrash time.Time
}

// Returns the time of the last crash for this server instance.
func (cd *CrashHandler) LastCrashTime() time.Time {
	cd.mu.RLock()
	defer cd.mu.RUnlock()

	return cd.lastCrash
}

// Sets the last crash time for a server.
func (cd *CrashHandler) SetLastCrash(t time.Time) {
	cd.mu.Lock()
	cd.lastCrash = t
	cd.mu.Unlock()
}

// Looks at the environment exit state to determine if the process exited cleanly or
// if it was the result of an event that we should try to recover from.
//
// This function assumes it is called under circumstances where a crash is suspected
// of occurring. It will not do anything to determine if it was actually a crash, just
// look at the exit state and check if it meets the criteria of being called a crash
// by Wings.
//
// If the server is determined to have crashed, the process will be restarted and the
// counter for the server will be incremented.
func (s *Server) handleServerCrash() error {
	// No point in doing anything here if the server isn't currently offline, there
	// is no reason to do a crash detection event. If the server crash detection is
	// disabled we want to skip anything after this as well.
	if s.Environment.State() != environment.ProcessOfflineState || !s.Config().CrashDetectionEnabled {
		if !s.Config().CrashDetectionEnabled {
			s.Log().Debug("server triggered crash detection but handler is disabled for server process")
			s.PublishConsoleOutputFromDaemon("Aborting automatic restart, crash detection is disabled for this instance.")
		}

		return nil
	}

	exitCode, oomKilled, err := s.Environment.ExitState()
	if err != nil {
		return errors.Wrap(err, "failed to get exit state for server process")
	}

	// If the system is not configured to detect a clean exit code as a crash, and the
	// crash is not the result of the program running out of memory, do nothing.
	if exitCode == 0 && !oomKilled && !config.Get().System.CrashDetection.DetectCleanExitAsCrash {
		s.Log().Debug("server exited with successful exit code; system is configured to not detect this as a crash")
		return nil
	}

	// Get the last lines from the output before the crash so we can log it
	logs, err := s.Environment.Readlog(config.Get().System.CrashActivityLogLines)
	if err != nil {
		log.WithField("server_id", s.ID()).Warn("Faild to get the last lines out of the console for the activity logs")
	}

	s.PublishConsoleOutputFromDaemon("---------- Detected server process in a crashed state! ----------")
	s.PublishConsoleOutputFromDaemon(fmt.Sprintf("Exit code: %d", exitCode))
	s.PublishConsoleOutputFromDaemon(fmt.Sprintf("Out of memory: %t", oomKilled))

	c := s.crasher.LastCrashTime()

	// Read before SetLastCrash below replaces it, so the hook sees the
	// previous crash rather than this one.
	var lastCrashUnix int64
	if !c.IsZero() {
		lastCrashUnix = c.Unix()
	}

	timeout := config.Get().System.CrashDetection.Timeout

	// If the last crash time was within the last `timeout` seconds we do not want to perform
	// an automatic reboot of the process. Return an error that can be handled.
	//
	// If timeout is set to 0, always reboot the server (this is probably a terrible idea, but some people want it)
	if timeout != 0 && !c.IsZero() && c.Add(time.Second*time.Duration(config.Get().System.CrashDetection.Timeout)).After(time.Now()) {
		s.PublishConsoleOutputFromDaemon("Aborting automatic restart, last crash occurred less than " + strconv.Itoa(timeout) + " seconds ago.")
		return &crashTooFrequent{}
	}

	// Log that the server has crashed
	s.SaveActivity(s.NewRequestActivity("", "127.0.0.1"), ActivityServerCrashed, models.ActivityMeta{
		"exit_code": exitCode,
		"oomkilled": oomKilled,
		"logs":      logs,
	})

	s.crasher.SetLastCrash(time.Now())

	snapshot := s.PluginSnapshot()

	plugins.Lifecycle(api.Lifecycle{
		Server:    snapshot,
		Event:     api.ServerCrashed,
		ExitCode:  int(exitCode),
		OOMKilled: oomKilled,
	})

	// Ask plugins whether this one should come back up. Wings has already
	// decided the crash is worth restarting from; this is for policy it cannot
	// know, such as a maintenance window or a crash the logs say a restart
	// will not fix.
	if allow, reason := plugins.GateCrashRestart(api.CrashInfo{
		Server:        snapshot,
		ExitCode:      int(exitCode),
		OOMKilled:     oomKilled,
		LastCrashUnix: lastCrashUnix,
		Logs:          logs,
	}); !allow {
		s.PublishConsoleOutputFromDaemon(reason)
		s.Log().WithField("reason", reason).Info("a plugin declined the automatic restart after a crash")

		return nil
	}

	return errors.Wrap(s.HandlePowerAction(PowerActionStart), "failed to start server after crash detection")
}
