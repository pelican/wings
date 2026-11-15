package diagnostics

import (
	"context"
	"fmt"
	"io"
	"os/exec"
	"path"
	"strconv"
	"strings"
	"time"

	dockerSystem "github.com/moby/moby/api/types/system" // Alias the correct system package
	"github.com/moby/moby/client"

	"github.com/pelican/wings/config"
	"github.com/pelican/wings/environment"
	"github.com/pelican/wings/system"
)

// GenerateDiagnosticsReport collects diagnostic data and returns it as a string.
func GenerateDiagnosticsReport(includeEndpoints bool, includeLogs bool, logLines int) (string, error) {
	dockerVersion, dockerInfo, dockerErr := getDockerInfo()
	output := &strings.Builder{}

	fmt.Fprintln(output, "Pelican Wings - Diagnostics Report")
	printHeader(output, "Versions")
	fmt.Fprintln(output, "               Wings:", system.Version)
	if dockerErr == nil {
		fmt.Fprintln(output, "              Docker:", dockerVersion.Version)
	}
	if v, err := system.KernelVersion(); err == nil {
		fmt.Fprintln(output, "              Kernel:", v)
	}
	if os, err := system.OperatingSystem(); err == nil {
		fmt.Fprintln(output, "                  OS:", os)
	}

	printHeader(output, "Wings Configuration")
	if err := config.FromFile(config.DefaultLocation); err != nil {
	}
	cfg := config.Get()
	fmt.Fprintln(output, "      Panel Location:", redactField(cfg.PanelLocation, includeEndpoints))
	fmt.Fprintln(output, "")
	fmt.Fprintln(output, "  Internal Webserver:", redactField(cfg.Api.Host, includeEndpoints), ":", cfg.Api.Port)
	fmt.Fprintln(output, "         SSL Enabled:", cfg.Api.Ssl.Enabled)
	fmt.Fprintln(output, "     SSL Certificate:", redactField(cfg.Api.Ssl.CertificateFile, includeEndpoints))
	fmt.Fprintln(output, "             SSL Key:", redactField(cfg.Api.Ssl.KeyFile, includeEndpoints))
	fmt.Fprintln(output, "")
	fmt.Fprintln(output, "         SFTP Server:", redactField(cfg.System.Sftp.Address, includeEndpoints), ":", cfg.System.Sftp.Port)
	fmt.Fprintln(output, "      SFTP Read-Only:", cfg.System.Sftp.ReadOnly)
	fmt.Fprintln(output, "")
	fmt.Fprintln(output, "      Root Directory:", cfg.System.RootDirectory)
	fmt.Fprintln(output, "      Logs Directory:", cfg.System.LogDirectory)
	fmt.Fprintln(output, "      Data Directory:", cfg.System.Data)
	fmt.Fprintln(output, "   Archive Directory:", cfg.System.ArchiveDirectory)
	fmt.Fprintln(output, "    Backup Directory:", cfg.System.BackupDirectory)
	fmt.Fprintln(output, "")
	fmt.Fprintln(output, "            Username:", cfg.System.Username)
	fmt.Fprintln(output, "         Server Time:", time.Now().Format(time.RFC1123Z))
	fmt.Fprintln(output, "          Debug Mode:", cfg.Debug)

	printHeader(output, "Docker: Info")
	if dockerErr == nil {
		fmt.Fprintln(output, "Server Version:", dockerInfo.ServerVersion)
		fmt.Fprintln(output, "Storage Driver:", dockerInfo.Driver)
		if dockerInfo.DriverStatus != nil {
			for _, pair := range dockerInfo.DriverStatus {
				fmt.Fprintf(output, "  %s: %s\n", pair[0], pair[1])
			}
		}
		if dockerInfo.SystemStatus != nil {
			for _, pair := range dockerInfo.SystemStatus {
				fmt.Fprintf(output, " %s: %s\n", pair[0], pair[1])
			}
		}
		fmt.Fprintln(output, "LoggingDriver:", dockerInfo.LoggingDriver)
		fmt.Fprintln(output, " CgroupDriver:", dockerInfo.CgroupDriver)
		if len(dockerInfo.Warnings) > 0 {
			for _, w := range dockerInfo.Warnings {
				fmt.Fprintln(output, w)
			}
		}
	} else {
		fmt.Fprintln(output, dockerErr.Error())
	}

	printHeader(output, "Docker: Running Containers")
	c := exec.Command("docker", "ps")
	if co, err := c.Output(); err == nil {
		output.Write(co)
	} else {
		fmt.Fprint(output, "Couldn't list containers: ", err)
	}

	printHeader(output, "Latest Wings Logs")
	if includeLogs {
		p := path.Join(cfg.System.LogDirectory, "wings.log")
		if c, err := exec.Command("tail", "-n", strconv.Itoa(logLines), p).Output(); err == nil {
			fmt.Fprintf(output, "%s\n", string(c))
		} else {
			fmt.Fprintln(output, "No logs found or an error occurred.")
		}
	} else {
		fmt.Fprintln(output, "Logs redacted.")
	}

	return output.String(), nil
}

func getDockerInfo() (client.ServerVersionResult, dockerSystem.Info, error) {
	cli, err := environment.Docker()
	if err != nil {
		return client.ServerVersionResult{}, dockerSystem.Info{}, err
	}
	dockerVersion, err := cli.ServerVersion(context.Background(), client.ServerVersionOptions{})
	if err != nil {
		return client.ServerVersionResult{}, dockerSystem.Info{}, err
	}
	dockerInfo, err := cli.Info(context.Background(), client.InfoOptions{})
	if err != nil {
		return client.ServerVersionResult{}, dockerSystem.Info{}, err
	}
	return dockerVersion, dockerInfo.Info, nil
}

func redactField(s string, include bool) string {
	if !include {
		return "{redacted}"
	}
	return s
}

func printHeader(w io.Writer, title string) {
	fmt.Fprintln(w, "\n|\n|", title)
	fmt.Fprintln(w, "| ------------------------------")
}
