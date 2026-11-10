package server

import (
	"os"
	"runtime"

	"fmt"
	"strings"

	"emperror.dev/errors"
	"github.com/gammazero/workerpool"

	"github.com/pelican/wings/server/filesystem"
)

// Helper function to replace variables in the file path of the configuration parser
func replaceParserConfigPathVariables(filename string, envvars map[string]interface{}) string {
	// Check if filename contains at least one '{' and one '}'
	// This is here for performance as 99% of the eggs configuration parsers do not have variables in its path
	if !strings.Contains(filename, "{") || !strings.Contains(filename, "}") {
		return filename
	}

	// Replace "{{" with "${" and "}}" with "}"
	filename = strings.ReplaceAll(filename, "{{", "${")
	filename = strings.ReplaceAll(filename, "}}", "}")

	// replaces ${varname} with varval
	for varname, varval := range envvars {
		filename = strings.ReplaceAll(filename, fmt.Sprintf("${%s}", varname), fmt.Sprint(varval))
	}

	return filename
}

// UpdateConfigurationFiles updates all the defined configuration files for
// a server automatically to ensure that they always use the specified values.
func (s *Server) UpdateConfigurationFiles() {
	pool := workerpool.New(runtime.NumCPU())

	files := s.ProcessConfiguration().ConfigurationFiles

	for _, cf := range files {
		f := cf

		pool.Submit(func() {
			filename := replaceParserConfigPathVariables(f.FileName, s.Config().EnvVars)
			file, err := func() (filesystem.File, error) {
				if f.AllowCreateFile {
					return s.Filesystem().Touch(filename, os.O_RDWR|os.O_CREATE, 0o644)
				}
				return s.Filesystem().Touch(filename, os.O_RDWR, 0o644)
			}()
			if err != nil {
				log := s.Log().WithField("file_name", filename)
				if errors.Is(err, os.ErrNotExist) && !f.AllowCreateFile {
					log.Debug("file not created")
				} else {
					s.Log().WithField("file_name", f.FileName).WithField("error", err).Error("failed to open configuration file")
				}
				return
			}
			defer file.Close()

			if err := f.Parse(file); err != nil {
				s.Log().WithField("error", err).WithField("file_name", f.FileName).Error("failed to parse and update server configuration file")
			}

			s.Log().WithField("file_name", f.FileName).Debug("finished processing server configuration file")
		})
	}

	pool.StopWait()
}
