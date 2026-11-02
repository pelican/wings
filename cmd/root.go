package cmd

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	log2 "log"
	"net/http"
	_ "net/http/pprof"
	"os"
	"path"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"time"

	"github.com/NYTimes/logrotate"
	"github.com/apex/log"
	"github.com/apex/log/handlers/multi"
	"github.com/docker/docker/client"
	"github.com/gammazero/workerpool"
	"github.com/mitchellh/colorstring"
	"github.com/pelican/wings/server/filesystem/quotas"
	"github.com/spf13/cobra"
	"golang.org/x/crypto/acme"
	"golang.org/x/crypto/acme/autocert"
	"golang.org/x/sys/unix"

	"github.com/pelican/wings/config"
	"github.com/pelican/wings/environment"
	"github.com/pelican/wings/internal/cron"
	"github.com/pelican/wings/internal/database"
	"github.com/pelican/wings/internal/pluginbridge"
	"github.com/pelican/wings/loggers/cli"
	"github.com/pelican/wings/plugins"
	"github.com/pelican/wings/remote"
	"github.com/pelican/wings/router"
	"github.com/pelican/wings/server"
	"github.com/pelican/wings/sftp"
	"github.com/pelican/wings/system"
)

var (
	configPath = config.DefaultLocation
	debug      = false
)

var rootCommand = &cobra.Command{
	Use:   "wings",
	Short: "Runs the API server allowing programmatic control of game servers for Pelican Panel.",
	PreRun: func(cmd *cobra.Command, args []string) {
		initConfig()
		initLogging()
		if tls, _ := cmd.Flags().GetBool("auto-tls"); tls {
			if host, _ := cmd.Flags().GetString("tls-hostname"); host == "" {
				fmt.Println("A TLS hostname must be provided when running wings with automatic TLS, e.g.:\n\n    ./wings --auto-tls --tls-hostname my.example.com")
				os.Exit(1)
			}
		}
	},
	Run: rootCmdRun,
}

var versionCommand = &cobra.Command{
	Use:   "version",
	Short: "Prints the current executable version and exits.",
	Run: func(cmd *cobra.Command, _ []string) {
		fmt.Printf("wings v%s\nCopyright © 2018 - %d Dane Everitt & Contributors\n", system.Version, time.Now().Year())
	},
}

func Execute() {
	if err := rootCommand.Execute(); err != nil {
		log2.Fatalf("failed to execute command: %s", err)
	}
}

func init() {
	rootCommand.PersistentFlags().StringVar(&configPath, "config", config.DefaultLocation, "set the location for the configuration file")
	rootCommand.PersistentFlags().BoolVar(&debug, "debug", false, "pass in order to run wings in debug mode")

	// Flags specifically used when running the API.
	rootCommand.Flags().Bool("pprof", false, "if the pprof profiler should be enabled. The profiler will bind to localhost:6060 by default")
	rootCommand.Flags().Int("pprof-block-rate", 0, "enables block profile support, may have performance impacts")
	rootCommand.Flags().Int("pprof-port", 6060, "If provided with --pprof, the port it will run on")
	rootCommand.Flags().Bool("auto-tls", false, "pass in order to have wings generate and manage its own SSL certificates using Let's Encrypt")
	rootCommand.Flags().String("tls-hostname", "", "required with --auto-tls, the FQDN for the generated SSL certificate")
	rootCommand.Flags().Bool("ignore-certificate-errors", false, "ignore certificate verification errors when executing API calls")

	rootCommand.AddCommand(versionCommand)
	rootCommand.AddCommand(configureCmd)
	rootCommand.AddCommand(newDiagnosticsCommand())
	rootCommand.AddCommand(newSelfupdateCommand())
}

func isDockerSnap() bool {
	cli, err := client.NewClientWithOpts(client.FromEnv, client.WithAPIVersionNegotiation())
	if err != nil {
		log.Fatalf("Unable to initialize Docker client: %s", err)
	}

	defer cli.Close() // Close the client when the function returns (should not be needed, but just to be safe)

	info, err := cli.Info(context.Background())
	if err != nil {
		log.Fatalf("Unable to get Docker info: %s", err)
	}

	// Check if Docker root directory contains '/var/snap/docker'
	return strings.Contains(info.DockerRootDir, "/var/snap/docker")
}

func rootCmdRun(cmd *cobra.Command, _ []string) {
	if config.Get().Debug || !config.Get().Quiet {
		printLogo()
	}
	log.Debug("running in debug mode")
	log.WithField("path", filepath.Join(config.Get().System.LogDirectory, "wings.log")).Info("writing log files to disk")
	log.WithField("config_file", configPath).Info("loading configuration from file")

	if isDockerSnap() {
		log.Error("Docker Snap installation detected. Exiting...")
		os.Exit(1)
	}

	if ok, _ := cmd.Flags().GetBool("ignore-certificate-errors"); ok {
		log.Warn("running with --ignore-certificate-errors: TLS certificate host chains and name will not be verified")
		http.DefaultTransport.(*http.Transport).TLSClientConfig = &tls.Config{
			InsecureSkipVerify: true,
		}
	}

	if err := config.ConfigureTimezone(); err != nil {
		log.WithField("error", err).Fatal("failed to detect system timezone or use supplied configuration value")
		return
	}
	log.WithField("timezone", config.Get().System.Timezone).Info("configured wings with system timezone")
	if err := config.ConfigureDirectories(); err != nil {
		log.WithField("error", err).Fatal("failed to configure system directories for pelican")
		return
	}
	if err := config.EnsurePelicanUser(); err != nil {
		log.WithField("error", err).Fatal("failed to create pelican system user")
		return
	}
	if err := config.ConfigurePasswd(); err != nil {
		log.WithField("error", err).Fatal("failed to create passwd files for pelican")
	}
	log.WithFields(log.Fields{
		"username": config.Get().System.Username,
		"uid":      config.Get().System.User.Uid,
		"gid":      config.Get().System.User.Gid,
	}).Info("configured system user successfully")
	if err := config.EnableLogRotation(); err != nil {
		log.WithField("error", err).Fatal("failed to configure log rotation on the system")
		return
	}

	t := config.Get().Token
	pclient := remote.New(
		config.Get().PanelLocation,
		remote.WithCredentials(t.ID, t.Token),
		remote.WithCustomHeaders(config.Get().RemoteQuery.CustomHeaders),
		remote.WithHttpClient(&http.Client{
			Timeout: time.Second * time.Duration(config.Get().RemoteQuery.Timeout),
		}),
	)

	if err := database.Initialize(); err != nil {
		log.WithField("error", err).Fatal("failed to initialize database")
		return
	}

	// if quotas are enabled ensure they are added and enabled.
	if runtime.GOOS == "linux" && config.Get().System.Quotas.Enabled {
		log.Info("validating system is configured for quotas")
		// check if the fs is supported
		if !quotas.IsSupportedFS() {
			log.Fatal("failed to validate quota configuration")
		}

		log.Info("quotas are supported and enabled")
	}

	manager, err := server.NewManager(cmd.Context(), pclient)
	if err != nil {
		log.WithField("error", err).Fatal("failed to load server configurations")
		return
	}

	// Bring the plugin subsystem up. Discovery happens here rather than with
	// loading so the management endpoints can report a plugin that fails to
	// load, along with the reason. Nothing is loaded yet: a plugin may
	// register recurring jobs, and the scheduler does not exist until later.
	pluginManager := plugins.NewManager(config.Get().Plugins, pluginbridge.New(manager), database.Instance())
	plugins.SetManager(pluginManager)
	defer pluginManager.Shutdown()

	if err := pluginManager.Discover(); err != nil {
		log.WithField("error", err).Error("failed to scan the plugin directory")
	}

	if err := environment.ConfigureDocker(cmd.Context()); err != nil {
		log.WithField("error", err).Fatal("failed to configure docker environment")
		return
	}

	if err := config.WriteToDisk(config.Get()); err != nil {
		if !errors.Is(err, unix.EROFS) {
			log.WithField("error", err).Error("failed to write configuration to disk")
		} else {
			log.WithField("error", err).Debug("failed to write configuration to disk")
		}
	}

	// Just for some nice log output.
	for _, s := range manager.All() {
		log.WithField("server", s.ID()).Info("finished loading configuration")
	}

	states, err := manager.ReadStates()
	if err != nil {
		log.WithField("error", err).Error("failed to retrieve locally cached server states from disk, assuming all servers in offline state")
	}

	ticker := time.NewTicker(time.Minute)
	// Every minute, write the current server states to the disk to allow for a more
	// seamless hard-reboot process in which wings will re-sync server states based
	// on its last tracked state.
	go func() {
		for {
			select {
			case <-ticker.C:
				if err := manager.PersistStates(); err != nil {
					log.WithField("error", err).Warn("failed to persist server states to disk")
				}
			case <-cmd.Context().Done():
				ticker.Stop()
				return
			}
		}
	}()

	// Create a new workerpool that limits us to 4 servers being bootstrapped at a time
	// on Wings. This allows us to ensure the environment exists, write configurations,
	// and reboot processes without causing a slow-down due to sequential booting.
	pool := workerpool.New(4)
	for _, serv := range manager.All() {
		s := serv

		// For each server we encounter make sure the root data directory exists.
		if err := s.EnsureDataDirectoryExists(); err != nil {
			s.Log().Error("could not create root data directory for server: not loading server...")
			continue
		}

		pool.Submit(func() {
			s.Log().Info("configuring server environment and restoring to previous state")
			var st string
			if state, exists := states[s.ID()]; exists {
				st = state
			}

			// Use a timed context here to avoid booting issues where Docker hangs for a
			// specific container that would cause Wings to be un-bootable until the entire
			// machine is rebooted. It is much better for us to just have a single failed
			// server instance than an entire offline node.
			//
			// @see https://github.com/pterodactyl/panel/issues/2475
			// @see https://github.com/pterodactyl/panel/issues/3358
			ctx, cancel := context.WithTimeout(cmd.Context(), time.Second*30)
			defer cancel()

			r, err := s.Environment.IsRunning(ctx)
			// We ignore missing containers because we don't want to actually block booting of wings at this
			// point. If we didn't do this, and you pruned all the images and then started wings you could
			// end up waiting a long period of time for all the images to be re-pulled on Wings boot rather
			// than when the server itself is started.
			if err != nil && !client.IsErrNotFound(err) {
				s.Log().WithField("error", err).Error("error checking server environment status")
			}

			// Check if the server was previously running. If so, attempt to start the server now so that Wings
			// can pick up where it left off. If the environment does not exist at all, just create it and then allow
			// the normal flow to execute.
			//
			// This does mean that booting wings after a catastrophic machine crash and wiping out the Docker images
			// as a result will result in a slow boot.
			if !r && (st == environment.ProcessRunningState || st == environment.ProcessStartingState) {
				if err := s.HandlePowerAction(server.PowerActionStart); err != nil {
					s.Log().WithField("error", err).Warn("failed to return server to running state")
				}
			} else if r || (!r && s.IsRunning()) {
				// If the server is currently running on Docker, mark the process as being in that state.
				// We never want to stop an instance that is currently running external from Wings since
				// that is a good way of keeping things running even if Wings gets in a very corrupted state.
				//
				// This will also validate that a server process is running if the last tracked state we have
				// is that it was running, but we see that the container process is not currently running.
				s.Log().Info("detected server is running, re-attaching to process...")

				s.Environment.SetState(environment.ProcessRunningState)
				if err := s.Environment.Attach(ctx); err != nil {
					s.Log().WithField("error", err).Warn("failed to attach to running server environment")
				}
			} else {
				// At this point we've determined that the server should indeed be in an offline state, so we'll
				// make a call to set that state just to ensure we don't ever accidentally end up with some invalid
				// state being tracked.
				s.Environment.SetState(environment.ProcessOfflineState)
			}

			if state := s.Environment.State(); state == environment.ProcessStartingState || state == environment.ProcessRunningState {
				s.Log().Debug("re-syncing server configuration for already running server")
				if err := s.Sync(); err != nil {
					s.Log().WithError(err).Error("failed to re-sync server configuration")
				}
			}
		})
	}

	// Wait until all the servers are ready to go before we fire up the SFTP and HTTP servers.
	pool.StopWait()
	defer func() {
		// Cancel the context on all the running servers at this point, even though the
		// program is just shutting down.
		for _, s := range manager.All() {
			s.CtxCancel()
		}
	}()

	if s, err := cron.Scheduler(cmd.Context(), manager); err != nil {
		log.WithField("error", err).Fatal("failed to initialize cron system")
	} else {
		log.WithField("subsystem", "cron").Info("starting cron processes")
		s.Start()

		// Plugins are loaded last, with the scheduler available so a plugin
		// registering a job gets it scheduled, and with every server already
		// booted so a plugin's Init can see them.
		pluginManager.SetJobScheduler(cron.PluginScheduler(s))
		if err := pluginManager.LoadEnabled(); err != nil {
			log.WithField("error", err).Error("failed to load plugins")
		}
	}

	go func() {
		// Run the SFTP server.
		if err := sftp.New(manager).Run(); err != nil {
			log.WithError(err).Fatal("failed to initialize the sftp server")
			return
		}
	}()

	go func() {
		log.Info("updating server states on Panel: marking installing/restoring servers as normal")
		// Update all the servers on the Panel to be in a valid state if they're
		// currently marked as installing/restoring now that Wings is restarted.
		if err := pclient.ResetServersState(cmd.Context()); err != nil {
			log.WithField("error", err).Error("failed to reset server states on Panel: some instances may be stuck in an installing/restoring state unexpectedly")
		}
	}()

	sys := config.Get().System
	// Ensure the archive directory exists.
	if err := os.MkdirAll(sys.ArchiveDirectory, 0o755); err != nil {
		log.WithField("error", err).Error("failed to create archive directory")
	}

	// Ensure the backup directory exists.
	if err := os.MkdirAll(sys.BackupDirectory, 0o755); err != nil {
		log.WithField("error", err).Error("failed to create backup directory")
	}

	autotls, _ := cmd.Flags().GetBool("auto-tls")
	tlshostname, _ := cmd.Flags().GetString("tls-hostname")
	if autotls && tlshostname == "" {
		autotls = false
	}

	api := config.Get().Api
	log.WithFields(log.Fields{
		"use_ssl":      api.Ssl.Enabled,
		"use_auto_tls": autotls,
		"host_address": api.Host,
		"host_port":    api.Port,
	}).Info("configuring internal webserver")

	// Create a new HTTP server instance to handle inbound requests from the Panel
	// and external clients.
	s := &http.Server{
		Addr:      api.Host + ":" + strconv.Itoa(api.Port),
		Handler:   router.Configure(manager, pclient),
		TLSConfig: config.DefaultTLSConfig,
	}

	profile, _ := cmd.Flags().GetBool("pprof")
	if profile {
		if r, _ := cmd.Flags().GetInt("pprof-block-rate"); r > 0 {
			runtime.SetBlockProfileRate(r)
		}
		// Catch at least 1% of mutex contention issues.
		runtime.SetMutexProfileFraction(100)

		profilePort, _ := cmd.Flags().GetInt("pprof-port")
		go func() {
			http.ListenAndServe(fmt.Sprintf("localhost:%d", profilePort), nil)
		}()
	}

	// Check if the server should run with TLS but using autocert.
	if autotls {
		m := autocert.Manager{
			Prompt:     autocert.AcceptTOS,
			Cache:      autocert.DirCache(path.Join(sys.RootDirectory, "/.tls-cache")),
			HostPolicy: autocert.HostWhitelist(tlshostname),
		}

		log.WithField("hostname", tlshostname).Info("webserver is now listening with auto-TLS enabled; certificates will be automatically generated by Let's Encrypt")

		// Hook autocert into the main http server.
		s.TLSConfig.GetCertificate = m.GetCertificate
		s.TLSConfig.NextProtos = append(s.TLSConfig.NextProtos, acme.ALPNProto) // enable tls-alpn ACME challenges

		// Start the autocert server.
		go func() {
			if err := http.ListenAndServe(":http", m.HTTPHandler(nil)); err != nil {
				log.WithError(err).Error("failed to serve autocert http server")
			}
		}()
		// Start the main http server with TLS using autocert.
		if err := s.ListenAndServeTLS("", ""); err != nil {
			log.WithFields(log.Fields{"auto_tls": true, "tls_hostname": tlshostname, "error": err}).Fatal("failed to configure HTTP server using auto-tls")
		}
		return
	}

	// Check if main http server should run with TLS. Otherwise, reset the TLS
	// config on the server and then serve it over normal HTTP.
	if api.Ssl.Enabled {
		if err := s.ListenAndServeTLS(api.Ssl.CertificateFile, api.Ssl.KeyFile); err != nil {
			log.WithFields(log.Fields{"auto_tls": false, "error": err}).Fatal("failed to configure HTTPS server")
		}
		return
	}
	s.TLSConfig = nil
	if err := s.ListenAndServe(); err != nil {
		log.WithField("error", err).Fatal("failed to configure HTTP server")
	}
}

// Reads the configuration from the disk and then sets up the global singleton
// with all the configuration values.
func initConfig() {
	if !filepath.IsAbs(configPath) {
		d, err := filepath.Abs(configPath)
		if err != nil {
			log2.Fatalf("cmd/root: failed to get path to config file: %s", err)
		}
		configPath = d
	}
	err := config.FromFile(configPath)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			exitWithConfigurationNotice()
		}
		log2.Fatalf("cmd/root: error while reading configuration file: %s", err)
	}
	if debug && !config.Get().Debug {
		config.SetDebugViaFlag(debug)
	}
}

// Configures the global logger for Zap so that we can call it from any location
// in the code without having to pass around a logger instance.
func initLogging() {
	dir := config.Get().System.LogDirectory
	if err := os.MkdirAll(path.Join(dir, "/install"), 0o700); err != nil {
		log2.Fatalf("cmd/root: failed to create install directory path: %s", err)
	}
	p := filepath.Join(dir, "/wings.log")
	w, err := logrotate.NewFile(p)
	if err != nil {
		log2.Fatalf("cmd/root: failed to create wings log: %s", err)
	}
	log.SetLevel(log.InfoLevel)
	if config.Get().Debug {
		log.SetLevel(log.DebugLevel)
	} else if config.Get().Quiet {
		log.SetLevel(log.WarnLevel)
	}
	log.SetHandler(multi.New(cli.Default, cli.New(w, false)))
	log.WithField("path", p).Info("writing log files to disk")
}

// bannerColor is how much color stdout can render for the startup banner.
type bannerColor int

const (
	bannerPlain bannerColor = iota // not a TTY: emit no escape codes at all
	banner256                      // TTY without truecolor: 256-color codes
	bannerTrue                     // TTY with truecolor: 24-bit color
)

// detectBannerColor picks the richest color mode stdout supports: plain when
// output is not a terminal, truecolor when COLORTERM advertises it, otherwise
// 256-color.
func detectBannerColor() bannerColor {
	fi, err := os.Stdout.Stat()
	if err != nil || fi.Mode()&os.ModeCharDevice == 0 {
		return bannerPlain
	}
	switch os.Getenv("COLORTERM") {
	case "truecolor", "24bit":
		return bannerTrue
	}
	return banner256
}

// hue is a single semantic banner color: a 24-bit value plus a 256-color
// fallback, resolved to an SGR foreground escape for the active color mode.
type hue struct {
	r, g, b, c256 int
}

func (h hue) fg(m bannerColor) string {
	switch m {
	case bannerTrue:
		return fmt.Sprintf("\x1b[38;2;%d;%d;%dm", h.r, h.g, h.b)
	case banner256:
		return fmt.Sprintf("\x1b[38;5;%dm", h.c256)
	default:
		return ""
	}
}

// ANSI Shadow "PELICAN" wordmark, six rows painted top→bottom cyan→blue.
var pelicanRows = [6]string{
	`██████╗ ███████╗██╗     ██╗ ██████╗ █████╗ ███╗   ██╗`,
	`██╔══██╗██╔════╝██║     ██║██╔════╝██╔══██╗████╗  ██║`,
	`██████╔╝█████╗  ██║     ██║██║     ███████║██╔██╗ ██║`,
	`██╔═══╝ ██╔══╝  ██║     ██║██║     ██╔══██║██║╚██╗██║`,
	`██║     ███████╗███████╗██║╚██████╗██║  ██║██║ ╚████║`,
	`╚═╝     ╚══════╝╚══════╝╚═╝ ╚═════╝╚═╝  ╚═╝╚═╝  ╚═══╝`,
}

var pelicanRowHues = [6]hue{
	{103, 232, 249, 123}, // #67E8F9
	{56, 189, 248, 117},  // #38BDF8
	{14, 165, 233, 39},   // #0EA5E9
	{59, 130, 246, 33},   // #3B82F6
	{37, 99, 235, 27},    // #2563EB
	{30, 64, 175, 25},    // #1E40AF
}

var (
	hueLightCyan = hue{103, 232, 249, 123} // #67E8F9 — W I N G S letters
	hueTeal      = hue{22, 78, 99, 23}     // #164E63 — rule end caps
	hueCyan      = hue{34, 211, 238, 51}   // #22D3EE — ▸ accents / URLs
	hueDim       = hue{107, 114, 128, 243} // #6B7280 — version / descriptions
	hueGray      = hue{156, 163, 175, 247} // #9CA3AF — tagline
	hueWhite     = hue{244, 244, 245, 255} // #F4F4F5 — link labels
	hueYellow    = hue{250, 204, 21, 226}  // #FACC15 — star CTA
)

// printLogo renders the wings startup banner once, before log output begins:
// an ANSI Shadow "PELICAN" wordmark with a vertical cyan→blue gradient, the
// WINGS rule + version, a tagline, link rows, and a star call-to-action. Color
// depth adapts to the terminal (truecolor / 256-color / none).
func printLogo() {
	m := detectBannerColor()
	bold, underline, reset := "", "", ""
	if m != bannerPlain {
		bold, underline, reset = "\x1b[1m", "\x1b[4m", "\x1b[0m"
	}

	var b strings.Builder
	line := func(parts ...string) {
		for _, p := range parts {
			b.WriteString(p)
		}
		b.WriteString(reset + "\n")
	}

	b.WriteByte('\n')

	// 1. Wordmark — one color per row.
	for i, row := range pelicanRows {
		line(pelicanRowHues[i].fg(m), row)
	}

	// 2. WINGS rule (indent 6) with the version centered beneath "W I N G S",
	//    which begins at column 14 and is 9 columns wide.
	b.WriteByte('\n')
	line(
		"      ",
		hueTeal.fg(m), "▰▰▰▰▰▰", reset,
		"  ", hueLightCyan.fg(m), bold, "W I N G S", reset,
		"  ", hueTeal.fg(m), "▰▰▰▰▰▰",
	)
	vIndent := 14 + (9-len(system.Version))/2
	if vIndent < 0 {
		vIndent = 0
	}
	line(strings.Repeat(" ", vIndent), hueDim.fg(m), system.Version)

	// 3. Tagline.
	b.WriteByte('\n')
	line(hueGray.fg(m), "Where your servers take flight.")

	// 4. Link rows — "▸ <label:8> ─ <desc:19> →  <url>".
	b.WriteByte('\n')
	linkRow := func(label, desc, url string) {
		line(
			hueCyan.fg(m), "▸ ", reset,
			hueWhite.fg(m), bold, fmt.Sprintf("%-8s", label), reset,
			hueDim.fg(m), fmt.Sprintf("─ %-19s→  ", desc), reset,
			hueCyan.fg(m), url,
		)
	}
	linkRow("Source", "Star us on GitHub", "github.com/pelican/wings")
	linkRow("Docs", "Get started", "pelican.dev/docs")

	// 5. Star call-to-action.
	b.WriteByte('\n')
	line(hueYellow.fg(m), bold, "★ Help Pelican soar: Star the project on GitHub")
	line(hueCyan.fg(m), underline, "https://github.com/pelican/panel")

	b.WriteByte('\n')
	fmt.Print(b.String())
}

func exitWithConfigurationNotice() {
	fmt.Print(colorstring.Color(`
[_red_][white][bold]Error: Configuration File Not Found[reset]

Wings was not able to locate your configuration file, and therefore is not
able to complete its boot process. Please ensure you have copied your instance
configuration file into the default location below.

Default Location: /etc/pelican/config.yml

[yellow]This is not a bug with this software. Please do not make a bug report
for this issue, it will be closed.[reset]

`))
	os.Exit(1)
}
