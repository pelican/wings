package cmd

import (
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/spf13/cobra"

	"github.com/pelican/wings/config"
	"github.com/pelican/wings/plugin"
)

// panelAccessMeaning says what each level lets the Panel do, for the CLI.
var panelAccessMeaning = map[plugin.PanelAccess]string{
	plugin.PanelAccessOff:    "the Panel can't see or change plugins on this node",
	plugin.PanelAccessView:   "the Panel can see plugins and enable, disable and configure the ones this node trusts; it can't install anything",
	plugin.PanelAccessSigned: "as view, and the Panel can install, update and uninstall plugins from the configured registries when they are signed by a key in plugins.trusted_keys",
	plugin.PanelAccessFull:   "as signed, and the Panel can install anything, from any URL or an upload, signed or not: Panel admins can run code on this node",
}

func newPluginsPanelAccessCommand() *cobra.Command {
	return &cobra.Command{
		Use:   "panel-access [off|view|signed|full]",
		Short: "Show or set how much the Panel may do with plugins on this node.",
		Long: `Show or set plugins.panel_access, how much the Panel may do with plugins on
this node. External plugins run as the Wings user, so this is the node
operator's decision; a configuration push from the Panel never changes it.

  off     the Panel can't see or change plugins
  view    see plugins; enable, disable and configure trusted ones (default)
  signed  also install, update and uninstall signed plugins from configured registries
  full    also install anything, signed or not, from any URL or an upload

WINGS_PLUGIN_PANEL_ACCESS in Wings' environment overrides the file. A running
Wings applies a change within a few seconds.`,
		Args: cobra.MaximumNArgs(1),
		Run: func(cmd *cobra.Command, args []string) {
			if err := runPanelAccess(cmd.OutOrStdout(), cmd.ErrOrStderr(), configPath, args); err != nil {
				fmt.Fprintln(cmd.ErrOrStderr(), err)
				os.Exit(1)
			}
		},
	}
}

// runPanelAccess prints the panel access level in effect, or, given a level,
// writes it to the configuration file at path.
func runPanelAccess(out, errOut io.Writer, path string, args []string) error {
	env, envSet := os.LookupEnv(plugin.PanelAccessEnv)
	if len(args) == 0 {
		level := plugin.CurrentPanelAccess()
		raw, source := config.Get().Plugins.PanelAccess, "plugins.panel_access in "+path
		if envSet {
			raw, source = env, plugin.PanelAccessEnv+" in this environment"
		}
		if raw == "" {
			source += ", not set: the default"
		} else if _, err := plugin.ParsePanelAccess(raw); err != nil {
			source += fmt.Sprintf(", which has the invalid value %q: refusing every Panel plugin request", raw)
		}
		fmt.Fprintf(out, "%s (%s)\n%s.\n", level, source, panelAccessMeaning[level])
		if !envSet {
			fmt.Fprintf(out, "%s in the Wings service's environment would override this.\n", plugin.PanelAccessEnv)
		}
		return nil
	}

	level, err := plugin.ParsePanelAccess(strings.ToLower(strings.TrimSpace(args[0])))
	if err != nil {
		return err
	}
	c := config.Get()
	p, err := config.UpdatePluginsOnDisk(c, func(p *config.PluginConfiguration) {
		p.PanelAccess = string(level)
	})
	if err != nil {
		return fmt.Errorf("failed to write config: %w", err)
	}
	c.Plugins = p
	config.Set(c)
	fmt.Fprintf(out, "set plugins.panel_access to %s in %s.\n%s.\nA running Wings applies it within a few seconds.\n", level, path, panelAccessMeaning[level])
	if envSet {
		fmt.Fprintf(errOut, "warning: %s=%s is set in this environment and overrides plugins.panel_access while it is set.\n", plugin.PanelAccessEnv, env)
	}
	return nil
}
