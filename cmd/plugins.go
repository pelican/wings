package cmd

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"text/tabwriter"

	"github.com/spf13/cobra"

	"github.com/pelican/wings/config"
	"github.com/pelican/wings/plugin"

	// Link the native plugins so they register and appear in the inventory.
	_ "github.com/pelican/wings/plugin/builtin"
	"github.com/pelican/wings/plugin/install"
)

func newPluginsCommand() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "plugins",
		Short: "Inspect and manage Wings capability plugins.",
		PersistentPreRun: func(cmd *cobra.Command, args []string) {
			if needsNodeConfig(cmd) {
				initConfig()
			}
		},
	}
	cmd.AddCommand(
		newPluginsListCommand(),
		newPluginsGetCommand(),
		newPluginsToggleCommand("enable", true),
		newPluginsToggleCommand("disable", false),
		newPluginsDoctorCommand(),
		newPluginsUpdateCommand(),
		newPluginsAddCommand(),
		newPluginsTrustCommand(),
		newPluginsUntrustCommand(),
		newPluginsKeygenCommand(),
		newPluginsSignCommand(),
		newPluginsPanelAccessCommand(),
	)
	return cmd
}

// needsNodeConfig reports whether a plugins subcommand reads this node's Wings
// configuration. keygen and sign are run by plugin authors, usually on a
// machine that is not a Wings node at all.
func needsNodeConfig(cmd *cobra.Command) bool {
	switch cmd.Name() {
	case "keygen", "sign":
		return false
	}
	return true
}

func newPluginsKeygenCommand() *cobra.Command {
	var out string
	cmd := &cobra.Command{
		Use:   "keygen",
		Short: "Generate an Ed25519 signing keypair for publishing plugins.",
		Run: func(cmd *cobra.Command, _ []string) {
			pub, priv, err := install.GenerateKeypair()
			if err != nil {
				fmt.Fprintf(os.Stderr, "%v\n", err)
				os.Exit(1)
			}
			if err := writePrivateKey(out, priv); err != nil {
				fmt.Fprintf(os.Stderr, "writing private key: %v\n", err)
				os.Exit(1)
			}
			fmt.Printf("private key written to %s (keep it secret)\n\n", out)
			fmt.Printf("public key — add to plugins.trusted_keys in config.yml:\n%s\n", pub)
		},
	}
	cmd.Flags().StringVar(&out, "out", "wings-plugin.key", "path to write the private key")
	return cmd
}

// writePrivateKey creates path with mode 0600 and refuses to touch an existing
// file. os.WriteFile would truncate an existing file and keep its permissions,
// so a key written over a world-readable file would stay world-readable.
func writePrivateKey(path, priv string) error {
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return err
	}
	if _, err := f.WriteString(priv + "\n"); err != nil {
		f.Close()
		os.Remove(path)
		return err
	}
	return f.Close()
}

func newPluginsSignCommand() *cobra.Command {
	var keyfile string
	cmd := &cobra.Command{
		Use:   "sign <archive>",
		Short: "Sign a plugin archive; prints the base64 signature for the registry entry.",
		Args:  cobra.ExactArgs(1),
		Run: func(cmd *cobra.Command, args []string) {
			key, err := os.ReadFile(keyfile)
			if err != nil {
				fmt.Fprintf(os.Stderr, "reading key: %v\n", err)
				os.Exit(1)
			}
			f, err := os.Open(args[0])
			if err != nil {
				fmt.Fprintf(os.Stderr, "opening archive: %v\n", err)
				os.Exit(1)
			}
			defer f.Close()
			sig, err := install.SignArchive(strings.TrimSpace(string(key)), f)
			if err != nil {
				fmt.Fprintf(os.Stderr, "signing: %v\n", err)
				os.Exit(1)
			}
			fmt.Println(sig)
		},
	}
	cmd.Flags().StringVar(&keyfile, "key", "wings-plugin.key", "path to the Ed25519 private key")
	return cmd
}

func newPluginsAddCommand() *cobra.Command {
	var assumeYes bool
	cmd := &cobra.Command{
		Use:   "add <@namespace/name | name | url#name>",
		Short: "Install and trust a plugin from a configured registry.",
		Args:  cobra.ExactArgs(1),
		Run: func(cmd *cobra.Command, args []string) {
			ctx := context.Background()
			resolved, err := install.Resolve(ctx, args[0])
			if err != nil {
				fmt.Fprintf(os.Stderr, "%v\n", err)
				os.Exit(1)
			}
			dir := config.Get().Plugins.Directory
			stage, err := install.StageAdd(ctx, resolved, dir)
			if err != nil {
				fmt.Fprintf(os.Stderr, "install failed: %v\n", err)
				os.Exit(1)
			}
			m, err := installStaged(stage, nil, assumeYes)
			switch {
			case errors.Is(err, errNotApproved):
				fmt.Printf("did not install %s; it was not trusted.\n", m.Name)
			case err != nil:
				fmt.Fprintf(os.Stderr, "install failed: %v\n", err)
				os.Exit(1)
			default:
				fmt.Printf("installed and trusted %s %s. A running Wings loads it within a few seconds.\n", m.Name, m.Version)
			}
		},
	}
	cmd.Flags().BoolVarP(&assumeYes, "yes", "y", false, "approve the plugin without prompting")
	return cmd
}

func newPluginsTrustCommand() *cobra.Command {
	return &cobra.Command{
		Use:   "trust <name>",
		Short: "Review and approve an installed external plugin so it may run.",
		Args:  cobra.ExactArgs(1),
		Run: func(cmd *cobra.Command, args []string) {
			info, ok, _ := plugin.Find(args[0])
			if !ok || info.Kind != plugin.KindExternal || info.Manifest == "" {
				fmt.Fprintf(os.Stderr, "no external plugin named %q\n", args[0])
				os.Exit(1)
			}
			if info.Err != "" {
				fmt.Fprintf(os.Stderr, "cannot trust %q: %s\n", args[0], info.Err)
				os.Exit(1)
			}
			m, err := plugin.LoadManifest(info.Manifest)
			if err != nil {
				fmt.Fprintf(os.Stderr, "%v\n", err)
				os.Exit(1)
			}
			ok, err = approvePlugin(m, false)
			if err != nil {
				fmt.Fprintf(os.Stderr, "%v\n", err)
				os.Exit(1)
			}
			if !ok {
				fmt.Println("not trusted.")
				return
			}
			fmt.Printf("trusted %s. A running Wings loads it within a few seconds.\n", m.Name)
		},
	}
}

func newPluginsUntrustCommand() *cobra.Command {
	return &cobra.Command{
		Use:   "untrust <name>",
		Short: "Revoke trust for a plugin so it stops loading.",
		Args:  cobra.ExactArgs(1),
		Run: func(cmd *cobra.Command, args []string) {
			ts, err := plugin.LoadTrustStore()
			if err != nil {
				fmt.Fprintf(os.Stderr, "%v\n", err)
				os.Exit(1)
			}
			if err := ts.Revoke(args[0]); err != nil {
				fmt.Fprintf(os.Stderr, "%v\n", err)
				os.Exit(1)
			}
			fmt.Printf("untrusted %s. A running Wings stops it within a few seconds.\n", args[0])
		},
	}
}

// errNotApproved means the operator declined the plugin at the prompt.
var errNotApproved = install.ErrNotApproved

// installStaged takes a staged add or update the rest of the way with
// install.Install, asking the operator to approve the staged manifest (or not,
// with assumeYes). check, if set, vets the staged manifest first. The Panel API
// uses the same install.Install with an approval token instead of the prompt.
func installStaged(stage *install.StagedInstall, check func(plugin.Manifest) error, assumeYes bool) (plugin.Manifest, error) {
	signature := "not signed"
	if stage.Signed() {
		signature = "verified against a key in plugins.trusted_keys"
	}
	return install.Install(stage, check, func(m plugin.Manifest) (bool, error) {
		return reviewPlugin(m, signature, assumeYes)
	})
}

// reviewPlugin prints what the operator is approving (printTrustPrompt) and
// asks for approval, skipping the question, not the details, if assumeYes.
// signature is the archive's signature status.
func reviewPlugin(m plugin.Manifest, signature string, assumeYes bool) (bool, error) {
	printTrustPrompt(m, signature)
	if !assumeYes && !confirm("Approve and trust this plugin?") {
		return false, nil
	}
	return true, nil
}

// approvePlugin reviews an installed plugin's manifest and records approval
// when accepted.
func approvePlugin(m plugin.Manifest, assumeYes bool) (bool, error) {
	ok, err := reviewPlugin(m, "not checked: signatures are checked when Wings downloads a plugin, and this one is already on the node", assumeYes)
	if err != nil || !ok {
		return ok, err
	}
	return true, install.RecordTrust(m)
}

// promptOut is where the approval prompt is printed.
var promptOut io.Writer = os.Stdout

// printTrustPrompt shows what the operator approves (contract S, "Disclosure
// at approval"): the plugin, whether its code is source or binary, the
// signature status, the host access it requests, the OS user it will run as,
// and what that user's access means.
func printTrustPrompt(m plugin.Manifest, signature string) {
	w := promptOut
	fmt.Fprintf(w, "\nPlugin %q\n", m.Name)
	fmt.Fprintf(w, "  version:     %s\n", m.Version)
	if m.Author != "" {
		fmt.Fprintf(w, "  author:      %s\n", m.Author)
	}
	fmt.Fprintf(w, "  capability:  %s\n", m.Capability)
	if m.Source != "" {
		fmt.Fprintf(w, "  source:      %s\n", m.Source)
	}
	fmt.Fprintf(w, "  entrypoint:  %s\n", strings.Join(m.Entrypoint, " "))
	if events := plugin.EffectiveEvents(m); len(events) > 0 {
		fmt.Fprintf(w, "  events:      %s\n", strings.Join(events, ","))
	}
	printForm(w, m)
	fmt.Fprintf(w, "  signature:   %s\n", signature)
	runsAs := printRunsAs(w)
	if len(m.Requires.HostAPI) > 0 {
		fmt.Fprintln(w, "  HOST ACCESS REQUESTED:")
		for _, r := range m.Requires.HostAPI {
			fmt.Fprintf(w, "    - %s\n", r)
		}
	} else {
		fmt.Fprintln(w, "  host access requested: none")
	}
	fmt.Fprintln(w)
	fmt.Fprintf(w, "%s\n", plugin.ProcessDisclosure(runsAs))
	fmt.Fprintln(w, "Wings does not review plugin code, and binary files can't be read as source; approve only code you trust.")
	fmt.Fprintln(w)
}

// printForm prints whether the plugin is source or binary, and its binary
// files.
func printForm(w io.Writer, m plugin.Manifest) {
	f, err := plugin.DetectForm(m.Dir())
	switch {
	case err != nil:
		fmt.Fprintf(w, "  form:        unknown (%v)\n", err)
	case f.Form == plugin.FormSource:
		fmt.Fprintln(w, "  form:        source (every file is text)")
	default:
		fmt.Fprintf(w, "  form:        binary (%d %s not text):\n", f.BinaryCount, plural(f.BinaryCount, "file is", "files are"))
		for _, name := range f.BinaryFiles {
			fmt.Fprintf(w, "    - %s\n", name)
		}
		if more := f.BinaryCount - len(f.BinaryFiles); more > 0 {
			fmt.Fprintf(w, "    - … and %d more\n", more)
		}
	}
}

// printRunsAs prints the OS user the plugin will run as and returns its name
// for the disclosure. If plugins can't run (system.user is root without
// plugins.run_as_root), it says so and returns the configured user name.
func printRunsAs(w io.Writer) string {
	u, err := plugin.ExternalProcessUser()
	if err != nil {
		fmt.Fprintf(w, "  runs as:     nobody yet; it won't start: %v\n", err)
		return config.Get().System.Username
	}
	fmt.Fprintf(w, "  runs as:     %s\n", u.Name)
	return u.Name
}

func plural(n int, one, many string) string {
	if n == 1 {
		return one
	}
	return many
}

func confirm(prompt string) bool {
	fmt.Printf("%s [y/N]: ", prompt)
	line, _ := bufio.NewReader(os.Stdin).ReadString('\n')
	line = strings.TrimSpace(strings.ToLower(line))
	return line == "y" || line == "yes"
}

func newPluginsUpdateCommand() *cobra.Command {
	var checkOnly bool
	var assumeYes bool
	cmd := &cobra.Command{
		Use:   "update <name>",
		Short: "Check for and install a newer version of an external plugin.",
		Args:  cobra.ExactArgs(1),
		Run: func(cmd *cobra.Command, args []string) {
			info, ok, _ := plugin.Find(args[0])
			if !ok {
				fmt.Fprintf(os.Stderr, "no plugin named %q\n", args[0])
				os.Exit(1)
			}
			if info.Kind != plugin.KindExternal || info.Manifest == "" {
				fmt.Fprintln(os.Stderr, "only external plugins can be updated; native providers ship with Wings")
				os.Exit(1)
			}
			if info.Err != "" {
				fmt.Fprintf(os.Stderr, "cannot update %q: %s\n", args[0], info.Err)
				os.Exit(1)
			}

			m, err := plugin.LoadManifest(info.Manifest)
			if err != nil {
				fmt.Fprintf(os.Stderr, "%v\n", err)
				os.Exit(1)
			}

			ctx := context.Background()
			st, err := install.CheckUpdate(ctx, m)
			if err != nil {
				fmt.Fprintf(os.Stderr, "%v\n", err)
				os.Exit(1)
			}
			if !st.Available {
				fmt.Printf("%s is up to date (%s)\n", st.Name, st.Installed)
				return
			}
			fmt.Printf("%s: update available %s -> %s\n", st.Name, st.Installed, st.Latest)
			if checkOnly {
				return
			}
			stage, err := install.StageUpdate(ctx, st)
			if err != nil {
				fmt.Fprintf(os.Stderr, "update failed: %v\n", err)
				os.Exit(1)
			}
			_, err = installStaged(stage, func(m plugin.Manifest) error {
				if m.Name != st.Name {
					return fmt.Errorf("staged manifest name %q does not match plugin %q", m.Name, st.Name)
				}
				if m.Version != st.Latest {
					return fmt.Errorf("staged manifest version %q does not match update version %q", m.Version, st.Latest)
				}
				return nil
			}, assumeYes)
			switch {
			case errors.Is(err, errNotApproved):
				fmt.Printf("did not update %s to %s; it was not trusted.\n", st.Name, st.Latest)
			case err != nil:
				fmt.Fprintf(os.Stderr, "update failed: %v\n", err)
				os.Exit(1)
			default:
				fmt.Printf("updated and trusted %s to %s. A running Wings switches to it within a few seconds.\n", st.Name, st.Latest)
			}
		},
	}
	cmd.Flags().BoolVar(&checkOnly, "check", false, "only check for an update; do not install")
	cmd.Flags().BoolVarP(&assumeYes, "yes", "y", false, "approve the updated plugin without prompting")
	return cmd
}

func newPluginsListCommand() *cobra.Command {
	return &cobra.Command{
		Use:   "list",
		Short: "List native and discovered plugins with their resolved state.",
		Run: func(cmd *cobra.Command, _ []string) {
			inv, err := plugin.Inventory()
			if err != nil {
				fmt.Fprintf(os.Stderr, "warning: discovery incomplete: %v\n", err)
			}
			if len(inv) == 0 {
				fmt.Println("no plugins registered or discovered")
				return
			}
			w := tabwriter.NewWriter(os.Stdout, 0, 2, 2, ' ', 0)
			fmt.Fprintln(w, "NAME\tKIND\tTRANSPORT\tSCOPE\tSTATE\tTRUST\tREASON")
			for _, p := range inv {
				fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\t%s\t%s\n",
					p.Name, p.Kind, p.Transport, scopeColumn(p), stateColumn(p), trustColumn(p), reasonColumn(p))
			}
			_ = w.Flush()
		},
	}
}

func newPluginsGetCommand() *cobra.Command {
	return &cobra.Command{
		Use:   "get <name>",
		Short: "Show details for a single plugin.",
		Args:  cobra.ExactArgs(1),
		Run: func(cmd *cobra.Command, args []string) {
			info, ok, err := plugin.Find(args[0])
			if err != nil {
				fmt.Fprintf(os.Stderr, "warning: discovery incomplete: %v\n", err)
			}
			if !ok {
				fmt.Fprintf(os.Stderr, "no plugin named %q\n", args[0])
				os.Exit(1)
			}
			w := tabwriter.NewWriter(os.Stdout, 0, 2, 2, ' ', 0)
			fmt.Fprintf(w, "name:\t%s\n", info.Name)
			fmt.Fprintf(w, "kind:\t%s\n", info.Kind)
			fmt.Fprintf(w, "capability:\t%s\n", info.Capability)
			if info.Version != "" {
				fmt.Fprintf(w, "version:\t%s\n", info.Version)
			}
			if info.Author != "" {
				fmt.Fprintf(w, "author:\t%s\n", info.Author)
			}
			if info.Source != "" {
				fmt.Fprintf(w, "source:\t%s\n", info.Source)
			}
			if info.Update != "" {
				fmt.Fprintf(w, "update:\t%s\n", info.Update)
			}
			fmt.Fprintf(w, "transport:\t%s\n", info.Transport)
			if len(info.Events) > 0 {
				fmt.Fprintf(w, "events:\t%s\n", listColumn(info.Events))
			}
			if info.Kind == plugin.KindExternal && info.Manifest != "" {
				if f, err := plugin.CachedForm(filepath.Dir(info.Manifest)); err == nil {
					fmt.Fprintf(w, "form:\t%s\n", f.Form)
					if len(f.BinaryFiles) > 0 {
						fmt.Fprintf(w, "binary files:\t%s\n", strings.Join(f.BinaryFiles, ", "))
					}
				}
				if u, err := plugin.ExternalProcessUser(); err == nil {
					fmt.Fprintf(w, "runs as:\t%s\n", u.Name)
				} else {
					fmt.Fprintf(w, "runs as:\tit won't start: %v\n", err)
				}
			} else if info.Kind != plugin.KindExternal {
				fmt.Fprintf(w, "form:\t%s\n", plugin.FormNative)
				fmt.Fprintf(w, "runs as:\t%s (inside Wings)\n", plugin.WingsUser().Name)
			}
			fmt.Fprintf(w, "enabled:\t%t (%s)\n", info.Enabled, info.Reason)
			fmt.Fprintf(w, "trusted:\t%t\n", info.Trusted)
			if info.Manifest != "" {
				fmt.Fprintf(w, "manifest:\t%s\n", info.Manifest)
			}
			if info.Err != "" {
				fmt.Fprintf(w, "error:\t%s\n", info.Err)
			}
			_ = w.Flush()
		},
	}
}

// togglePlugin updates plugin policy so name ends up enabled or disabled; see
// install.Toggle, which the Panel API uses too.
func togglePlugin(p *config.PluginConfiguration, name string, enable bool) {
	install.Toggle(p, name, enable)
}

// newPluginsToggleCommand builds the shared enable/disable command.
func newPluginsToggleCommand(verb string, enable bool) *cobra.Command {
	return &cobra.Command{
		Use:   verb + " <name>",
		Short: fmt.Sprintf("%s%s a plugin and persist the change to the config file.", strings.ToUpper(verb[:1]), verb[1:]),
		Args:  cobra.ExactArgs(1),
		Run: func(cmd *cobra.Command, args []string) {
			name := args[0]
			// Change the block as it is in the file now, under the config
			// file lock, so another command or the daemon writing at the same
			// time can't undo it (or this undo theirs).
			c := config.Get()
			p, err := config.UpdatePluginsOnDisk(c, func(p *config.PluginConfiguration) {
				togglePlugin(p, name, enable)
			})
			if err != nil {
				fmt.Fprintf(os.Stderr, "failed to write config: %v\n", err)
				os.Exit(1)
			}
			c.Plugins = p
			config.Set(c)
			fmt.Printf("%sd %q. A running Wings applies this within a few seconds.\n", verb, name)
		},
	}
}

func newPluginsDoctorCommand() *cobra.Command {
	return &cobra.Command{
		Use:   "doctor",
		Short: "Diagnose plugin discovery: directory, manifests, and enable decisions.",
		Run: func(cmd *cobra.Command, _ []string) {
			dir := config.Get().Plugins.Directory
			fmt.Printf("plugins directory: %s\n", dir)
			fmt.Printf("autoload: %t\n", config.Get().Plugins.Autoload)

			inv, err := plugin.Inventory()
			if err != nil {
				fmt.Printf("discovery error: %v\n", err)
			}
			fmt.Printf("providers: %d\n\n", len(inv))
			w := tabwriter.NewWriter(os.Stdout, 0, 2, 2, ' ', 0)
			fmt.Fprintln(w, "NAME\tKIND\tENABLED\tTRUST\tREASON\tSOURCE")
			for _, p := range inv {
				source := "native init()"
				if p.Manifest != "" {
					source = p.Manifest
				}
				fmt.Fprintf(w, "%s\t%s\t%t\t%s\t%s\t%s\n", p.Name, p.Kind, p.Enabled, trustColumn(p), reasonColumn(p), source)
			}
			_ = w.Flush()
		},
	}
}

func stateColumn(p plugin.ProviderInfo) string {
	if p.Err != "" {
		return "error"
	}
	if p.Enabled {
		return "enabled"
	}
	return "disabled"
}

func trustColumn(p plugin.ProviderInfo) string {
	if p.Trusted {
		return "trusted"
	}
	return "untrusted"
}

func reasonColumn(p plugin.ProviderInfo) string {
	if p.Err != "" {
		return p.Err
	}
	return p.Reason
}

func scopeColumn(p plugin.ProviderInfo) string {
	switch p.Capability {
	case plugin.CapabilityEvents:
		return "events:" + listColumn(p.Events)
	default:
		if len(p.Events) > 0 {
			return "events:" + listColumn(p.Events)
		}
		return "-"
	}
}

func listColumn(values []string) string {
	if len(values) == 0 {
		return "—"
	}
	return strings.Join(values, ",")
}
