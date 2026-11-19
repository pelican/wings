package cmd

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"net/url"
	"time"

	"github.com/apex/log"
	"github.com/charmbracelet/huh"
	"github.com/goccy/go-json"
	"github.com/spf13/cobra"

	"github.com/pelican/wings/internal/diagnostics"
	"github.com/pelican/wings/loggers/cli"
)

const (
	DefaultHastebinUrl = "https://logs.pelican.dev"
	DefaultLogLines    = 200
)

var diagnosticsArgs struct {
	IncludeEndpoints   bool
	IncludeLogs        bool
	ReviewBeforeUpload bool
	HastebinURL        string
	LogLines           int
}

func newDiagnosticsCommand() *cobra.Command {
	command := &cobra.Command{
		Use:   "diagnostics",
		Short: "Collect and report information about this Wings instance to assist in debugging.",
		PreRun: func(cmd *cobra.Command, args []string) {
			initConfig()
			log.SetHandler(cli.Default)
		},
		Run: diagnosticsCmdRun,
	}

	command.Flags().StringVar(&diagnosticsArgs.HastebinURL, "hastebin-url", DefaultHastebinUrl, "the url of the hastebin instance to use")
	command.Flags().IntVar(&diagnosticsArgs.LogLines, "log-lines", DefaultLogLines, "the number of log lines to include in the report")

	return command
}

// diagnosticsCmdRun collects diagnostics about wings, its configuration and the node.
// We collect:
// - wings and docker versions
// - relevant parts of daemon configuration
// - the docker debug output
// - running docker containers
// - logs
func diagnosticsCmdRun(*cobra.Command, []string) {
	// To set default to true
	defaultTrueConfirmAccessor := func() huh.Accessor[bool] {
		accessor := huh.EmbeddedAccessor[bool]{}
		accessor.Set(true)
		return &accessor
	}
	form := huh.NewForm(
		huh.NewGroup(
			huh.NewConfirm().
				Title("Do you want to include endpoints (i.e. the FQDN/IP of your panel)?").
				Value(&diagnosticsArgs.IncludeEndpoints),
			huh.NewConfirm().
				Title("Do you want to include the latest logs?").
				Accessor(defaultTrueConfirmAccessor()).
				Value(&diagnosticsArgs.IncludeLogs),
			huh.NewConfirm().
				Title(fmt.Sprintf("Do you want to review the collected data before uploading to %s ?", diagnosticsArgs.HastebinURL)).
				Description("The data, especially the logs, might contain sensitive information, so you should review it. You will be asked again if you want to upload.").
				Accessor(defaultTrueConfirmAccessor()).
				Value(&diagnosticsArgs.ReviewBeforeUpload),
		),
	)
	if err := form.Run(); err != nil {
		if err == huh.ErrUserAborted {
			return
		}
		panic(err)
	}

	report, err := diagnostics.GenerateDiagnosticsReport(
		diagnosticsArgs.IncludeEndpoints,
		diagnosticsArgs.IncludeLogs,
		diagnosticsArgs.LogLines,
	)
	if err != nil {
		fmt.Println("Error generating report:", err)
		return
	}

	fmt.Println("\n---------------  generated report  ---------------")
	fmt.Println(report)
	fmt.Print("---------------   end of report    ---------------\n\n")

	// Always ask before uploading, defaulting to no. Answering no to reviewing the
	// report must not upload it without asking.
	upload := false
	if err := huh.NewConfirm().Title("Upload to " + diagnosticsArgs.HastebinURL + "?").Value(&upload).Run(); err != nil || !upload {
		return
	}

	u, err := uploadToHastebin(diagnosticsArgs.HastebinURL, report)
	if err != nil {
		fmt.Println("Failed to upload report:", err)
		return
	}
	fmt.Println("Your report is available here: ", u)
}

// maxHastebinResponseSize is the most that is read of the response from the
// hastebin instance.
const maxHastebinResponseSize = 1 << 20

func uploadToHastebin(hbUrl, content string) (string, error) {
	u, err := url.Parse(hbUrl)
	if err != nil {
		return "", err
	}

	formData := new(bytes.Buffer)
	formWriter := multipart.NewWriter(formData)
	formWriter.WriteField("c", content)
	formWriter.WriteField("e", "14d")
	formWriter.Close()

	c := &http.Client{Timeout: 30 * time.Second}
	res, err := c.Post(u.String(), formWriter.FormDataContentType(), formData)
	if err != nil {
		return "", err
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusOK {
		myb, _ := io.ReadAll(io.LimitReader(res.Body, maxHastebinResponseSize))
		return "", fmt.Errorf("unexpected response status %s from %s: %s", res.Status, u.String(), string(myb))
	}
	pres := make(map[string]interface{})
	body, err := io.ReadAll(io.LimitReader(res.Body, maxHastebinResponseSize))
	if err != nil {
		fmt.Println("Failed to parse response.", err)
		return "", err
	}
	json.Unmarshal(body, &pres)
	if pasteUrl, ok := pres["url"].(string); ok {
		return pasteUrl, nil
	}
	return "", errors.New("failed to find key in response")
}
