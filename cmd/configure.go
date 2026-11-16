package cmd

import (
	"crypto/tls"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path"
	"regexp"
	"strings"
	"time"

	"github.com/charmbracelet/huh"
	"github.com/creasty/defaults"
	"github.com/goccy/go-json"
	"github.com/spf13/cobra"

	"github.com/pelican/wings/config"
	"github.com/pelican/wings/plugin"
)

var configureArgs struct {
	PanelURL      string
	Token         string
	ConfigPath    string
	Node          string
	Override      bool
	AllowInsecure bool
	PluginAccess  string
}

var configureCmd = &cobra.Command{
	Use:   "configure",
	Short: "Use a token to configure wings automatically",
	Run:   configureCmdRun,
}

func init() {
	configureCmd.PersistentFlags().StringVarP(&configureArgs.PanelURL, "panel-url", "p", "", "The base URL for this daemon's panel")
	configureCmd.PersistentFlags().StringVarP(&configureArgs.Token, "token", "t", "", "The API key to use for fetching node information")
	configureCmd.PersistentFlags().StringVarP(&configureArgs.Node, "node", "n", "", "The ID of the node which will be connected to this daemon")
	configureCmd.PersistentFlags().StringVarP(&configureArgs.ConfigPath, "config-path", "c", config.DefaultLocation, "The path where the configuration file should be made")
	configureCmd.PersistentFlags().BoolVar(&configureArgs.Override, "override", false, "Set to true to override an existing configuration for this node")
	configureCmd.PersistentFlags().BoolVar(&configureArgs.AllowInsecure, "allow-insecure", false, "Set to true to disable certificate checking")
	configureCmd.PersistentFlags().StringVar(&configureArgs.PluginAccess, "plugin-access", "", "Set how much the Panel may do with plugins on this node: off, view, signed or full (plugins.panel_access). Without it the value in the file is kept")
}

// pluginAccessFlag returns the level --plugin-access asks for, or "" if the
// flag was not given.
func pluginAccessFlag() (plugin.PanelAccess, error) {
	if configureArgs.PluginAccess == "" {
		return "", nil
	}
	return plugin.ParsePanelAccess(strings.ToLower(strings.TrimSpace(configureArgs.PluginAccess)))
}

func configureCmdRun(cmd *cobra.Command, args []string) {
	// Check the flags that need no Panel before asking it anything.
	access, err := pluginAccessFlag()
	if err != nil {
		fmt.Println("Invalid --plugin-access:", err)
		os.Exit(1)
	}

	if configureArgs.AllowInsecure {
		http.DefaultTransport.(*http.Transport).TLSClientConfig = &tls.Config{
			InsecureSkipVerify: true,
		}
	}

	if _, err := os.Stat(configureArgs.ConfigPath); err == nil && !configureArgs.Override {
		err := huh.NewConfirm().
			Title("Override existing configuration file?").
			Value(&configureArgs.Override).
			Run()
		if err != nil {
			if err == huh.ErrUserAborted {
				return
			}
			panic(err)
		}
		if !configureArgs.Override {
			fmt.Println("Aborting process; a configuration file already exists for this node.")
			os.Exit(1)
		}
	} else if err != nil && !os.IsNotExist(err) {
		panic(err)
	}
	var fields []huh.Field

	if err := validateField("url", configureArgs.PanelURL); err != nil {
		fields = append(fields, huh.NewInput().
			Title("Panel URL: ").
			Validate(func(str string) error {
				return validateField("url", str)
			}).
			Value(&configureArgs.PanelURL),
		)
	}

	if err := validateField("token", configureArgs.Token); err != nil {
		fields = append(fields, huh.NewInput().
			Title("API Token: ").
			Validate(func(str string) error {
				return validateField("token", str)
			}).
			Value(&configureArgs.Token),
		)
	}

	if err := validateField("node", configureArgs.Node); err != nil {
		fields = append(fields, huh.NewInput().
			Title("Node ID: ").
			Validate(func(str string) error {
				return validateField("node", str)
			}).
			Value(&configureArgs.Node),
		)
	}
	if len(fields) > 0 {
		if err := huh.NewForm(huh.NewGroup(fields...)).Run(); err != nil {
			if err == huh.ErrUserAborted {
				return
			}
			panic(err)
		}
	}

	c := &http.Client{
		Timeout: time.Second * 30,
	}

	req, err := getRequest()
	if err != nil {
		panic(err)
	}

	fmt.Printf("%+v %s\n", req.Header, req.URL.String())

	res, err := c.Do(req)
	if err != nil {
		fmt.Println("Failed to fetch configuration from the panel.\n", err.Error())
		os.Exit(1)
	}
	defer res.Body.Close()

	if res.StatusCode == http.StatusForbidden || res.StatusCode == http.StatusUnauthorized {
		fmt.Println("The authentication credentials provided were not valid.")
		os.Exit(1)
	} else if res.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(res.Body)

		fmt.Println("An error occurred while processing this request.\n", string(b))
		os.Exit(1)
	}

	b, err := io.ReadAll(res.Body)

	cfg, err := config.NewAtPath(configPath)
	if err != nil {
		panic(err)
	}

	if err := json.Unmarshal(b, cfg); err != nil {
		panic(err)
	}

	// Manually specify the Panel URL as it won't be decoded from JSON.
	cfg.PanelLocation = configureArgs.PanelURL

	// Plugin policy is the node operator's, not the Panel's: ignore any
	// plugins block in the answer, and keep the one already in the file.
	var plugins config.PluginConfiguration
	if err := defaults.Set(&plugins); err != nil {
		panic(err)
	}
	cfg.Plugins = plugins
	if err = config.WriteToDiskKeepingPlugins(cfg); err != nil {
		panic(err)
	}
	// Plugin policy is still the operator's: the flag is how one sets it
	// when deploying many nodes, under the same lock `wings plugins` uses.
	if access != "" {
		if _, err := config.UpdatePluginsOnDisk(cfg, func(p *config.PluginConfiguration) {
			p.PanelAccess = string(access)
		}); err != nil {
			panic(err)
		}
		fmt.Printf("Set plugins.panel_access to %s.\n", access)
	}

	fmt.Println("Successfully configured wings.")
}

func getRequest() (*http.Request, error) {
	u, err := url.Parse(configureArgs.PanelURL)
	if err != nil {
		panic(err)
	}

	u.Path = path.Join(u.Path, fmt.Sprintf("api/application/nodes/%s/configuration", configureArgs.Node))

	r, err := http.NewRequest(http.MethodGet, u.String(), nil)
	if err != nil {
		return nil, err
	}

	r.Header.Set("Accept", "application/json")
	r.Header.Set("Content-Type", "application/json")
	r.Header.Set("Authorization", fmt.Sprintf("Bearer %s", configureArgs.Token))

	return r, nil
}

func validateField(name string, str string) error {
	switch name {
	case "url":
		u, err := url.Parse(str)
		if err != nil || u.Host == "" || (u.Scheme != "http" && u.Scheme != "https") || u.Path != "" {
			return fmt.Errorf("please provide a valid panel URL")
		}
	case "token":
		if !regexp.MustCompile(`^(peli|papp)_(\w{43})$`).Match([]byte(str)) {
			return fmt.Errorf("please provide a valid authentication token")
		}
	case "node":
		if !regexp.MustCompile(`^(\d+)$`).Match([]byte(str)) {
			return fmt.Errorf("please provide a valid numeric node ID")
		}
	}
	return nil
}
