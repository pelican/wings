package router

import (
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"

	"github.com/gin-gonic/gin"

	"github.com/pelican/wings/plugins"
	"github.com/pelican/wings/plugins/api"
	"github.com/pelican/wings/router/middleware"
)

// pluginResponse is one plugin as the Panel sees it.
//
// The field names deliberately mirror the Panel's own plugin resource, so the
// admin UI can render a node's plugins with the components it already has
// rather than a parallel set for the Wings side. The fields that differ are the
// ones that have to: a Wings plugin names a Go package where a Panel plugin
// names a PHP class, and its settings are declared in the manifest because the
// Panel has to draw a form for a plugin running on another machine.
type pluginResponse struct {
	ID          string `json:"id"`
	Name        string `json:"name"`
	Author      string `json:"author"`
	Version     string `json:"version"`
	Description string `json:"description"`
	Category    string `json:"category"`
	URL         string `json:"url"`
	UpdateURL   string `json:"update_url"`

	APIVersion   int    `json:"api_version"`
	WingsVersion string `json:"wings_version"`

	Package    string `json:"package"`
	Entrypoint string `json:"entrypoint"`

	Settings []plugins.Setting `json:"settings"`

	Meta pluginMeta `json:"meta"`
}

// pluginMeta carries the state an operator acts on, matching the shape the
// Panel's own plugin resource puts under "meta".
type pluginMeta struct {
	Status        string `json:"status"`
	StatusMessage string `json:"status_message"`
	LoadOrder     int    `json:"load_order"`

	// Values are the operator's saved settings, keyed the same way the
	// manifest declares them.
	Values map[string]any `json:"values"`

	Loaded       bool `json:"loaded"`
	Compatible   bool `json:"is_compatible"`
	APISupported bool `json:"is_api_supported"`
	Trusted      bool `json:"is_trusted"`

	CanEnable    bool `json:"can_enable"`
	CanDisable   bool `json:"can_disable"`
	CanUninstall bool `json:"can_uninstall"`

	// Registered reports what the plugin actually published once loaded, which
	// is how an operator tells a plugin that is enabled from one that is
	// enabled and doing something.
	Registered pluginRegistered `json:"registered"`
}

type pluginRegistered struct {
	Hooks         []string `json:"hooks"`
	Routes        int      `json:"routes"`
	Jobs          int      `json:"jobs"`
	ConfigParsers []string `json:"config_parsers"`
	BackupAdapter string   `json:"backup_adapter"`
}

// toPluginResponse renders one plugin for the API.
func toPluginResponse(m *plugins.Manager, inst *plugins.Instance) pluginResponse {
	manifest := inst.Manifest()
	status := manifest.Meta.Status

	res := pluginResponse{
		ID:           manifest.ID,
		Name:         manifest.Name,
		Author:       manifest.Author,
		Version:      manifest.Version,
		Description:  manifest.Description,
		Category:     string(manifest.Category),
		URL:          manifest.URL,
		UpdateURL:    manifest.UpdateURL,
		APIVersion:   manifest.EffectiveAPIVersion(),
		WingsVersion: manifest.WingsVersion,
		Package:      manifest.Package,
		Entrypoint:   manifest.EffectiveEntrypoint(),
		Settings:     manifest.Settings,
		Meta: pluginMeta{
			Status:        string(status),
			StatusMessage: manifest.Meta.StatusMessage,
			LoadOrder:     manifest.Meta.LoadOrder,
			Values:        manifest.ResolvedSettings(),
			Loaded:        inst.Loaded(),
			Compatible:    manifest.Compatible(),
			APISupported:  manifest.APIVersionSupported(),
			Trusted:       m.IsTrusted(manifest.ID),

			// These mirror the Panel's own canEnable and canDisable, so the
			// admin UI can decide which buttons to show without restating the
			// rules.
			CanEnable:    status == plugins.StatusDisabled && manifest.Compatible() && manifest.APIVersionSupported(),
			CanDisable:   status == plugins.StatusEnabled || status == plugins.StatusErrored || status == plugins.StatusIncompatible,
			CanUninstall: status != plugins.StatusNotInstalled,
		},
	}

	if inst.Loaded() {
		reg := inst.Registration()
		res.Meta.Registered = pluginRegistered{
			Hooks:  registeredHooks(reg),
			Routes: len(reg.Routes),
			Jobs:   len(reg.Jobs),
		}
		for format := range reg.ConfigParsers {
			res.Meta.Registered.ConfigParsers = append(res.Meta.Registered.ConfigParsers, format)
		}
		if reg.Backup != nil {
			res.Meta.Registered.BackupAdapter = reg.Backup.Name
		}
	}

	return res
}

// registeredHooks names the hooks a loaded plugin actually set, so an operator
// can see at a glance what it is able to affect.
func registeredHooks(reg api.Registration) []string {
	var out []string

	for name, set := range map[string]bool{
		"before_power_action":    reg.BeforePowerAction != nil,
		"before_server_install":  reg.BeforeServerInstall != nil,
		"before_server_delete":   reg.BeforeServerDelete != nil,
		"before_server_transfer": reg.BeforeServerTransfer != nil,
		"server_lifecycle":       reg.OnServerLifecycle != nil,
		"console_line":           reg.OnConsoleLine != nil,
		"command":                reg.OnCommand != nil,
		"before_file_action":     reg.BeforeFileAction != nil,
		"file_action":            reg.OnFileAction != nil,
		"container":              reg.MutateContainer != nil,
		"websocket":              reg.OnWebsocketMessage != nil,
		"settings_changed":       reg.SettingsChanged != nil,
	} {
		if set {
			out = append(out, name)
		}
	}

	sortStrings(out)
	return out
}

// sortStrings keeps the hook list stable between requests, so a Panel diffing
// two responses does not see a change that is only map iteration order.
func sortStrings(in []string) {
	for i := 1; i < len(in); i++ {
		for j := i; j > 0 && in[j] < in[j-1]; j-- {
			in[j], in[j-1] = in[j-1], in[j]
		}
	}
}

// requirePluginManager resolves the node's plugin manager, or answers the
// request itself when the subsystem is switched off.
//
// A node with plugins disabled answers the management endpoints rather than
// 404ing them, because the Panel needs to tell "this node has no plugins" apart
// from "this node is too old to know what a plugin is".
func requirePluginManager(c *gin.Context) (*plugins.Manager, bool) {
	m := plugins.Active()
	if m == nil || !m.Enabled() {
		c.JSON(http.StatusOK, gin.H{
			"enabled": false,
			"data":    []pluginResponse{},
		})
		return nil, false
	}
	return m, true
}

// getPlugins lists every plugin on the node.
func getPlugins(c *gin.Context) {
	m, ok := requirePluginManager(c)
	if !ok {
		return
	}

	// Rescan first so a plugin copied onto the node by hand appears without
	// the daemon having to be restarted.
	if err := m.Discover(); err != nil {
		middleware.CaptureAndAbort(c, err)
		return
	}

	all := m.All()
	data := make([]pluginResponse, 0, len(all))
	for _, inst := range all {
		data = append(data, toPluginResponse(m, inst))
	}

	c.JSON(http.StatusOK, gin.H{
		"enabled":   true,
		"directory": m.Directory(),
		"data":      data,
	})
}

// getPlugin returns one plugin.
func getPlugin(c *gin.Context) {
	m, ok := requirePluginManager(c)
	if !ok {
		return
	}

	inst, found := m.Get(c.Param("plugin"))
	if !found {
		c.AbortWithStatusJSON(http.StatusNotFound, gin.H{"error": "No plugin with that id exists on this node."})
		return
	}

	c.JSON(http.StatusOK, toPluginResponse(m, inst))
}

// pluginAction applies one of the lifecycle operations and answers with the
// plugin's new state, so the Panel does not have to follow up with a read.
func pluginAction(c *gin.Context, apply func(m *plugins.Manager, id string) error) {
	m, ok := requirePluginManager(c)
	if !ok {
		return
	}

	id := c.Param("plugin")
	if _, found := m.Get(id); !found {
		c.AbortWithStatusJSON(http.StatusNotFound, gin.H{"error": "No plugin with that id exists on this node."})
		return
	}

	if err := apply(m, id); err != nil {
		// A refusal here is almost always the operator asking for something
		// that does not apply to the plugin's current state, such as enabling
		// one that is already on. That is a conflict rather than a fault, and
		// the message is written to be shown to them.
		c.AbortWithStatusJSON(http.StatusConflict, gin.H{"error": err.Error()})
		return
	}

	inst, found := m.Get(id)
	if !found {
		// Uninstalling with delete removes the plugin outright, so there is
		// nothing left to return.
		c.Status(http.StatusNoContent)
		return
	}

	c.JSON(http.StatusOK, toPluginResponse(m, inst))
}

func postPluginInstall(c *gin.Context) {
	enable := c.Query("enable") != "false"

	pluginAction(c, func(m *plugins.Manager, id string) error {
		return m.Install(id, enable)
	})
}

func postPluginEnable(c *gin.Context) {
	pluginAction(c, func(m *plugins.Manager, id string) error { return m.Enable(id) })
}

func postPluginDisable(c *gin.Context) {
	pluginAction(c, func(m *plugins.Manager, id string) error { return m.Disable(id) })
}

func postPluginUninstall(c *gin.Context) {
	// Deleting the files is opt-in, matching the Panel's own uninstall, so the
	// default leaves the plugin on disk to be re-enabled later.
	deleteFiles := c.Query("delete") == "true"

	pluginAction(c, func(m *plugins.Manager, id string) error {
		return m.Uninstall(id, deleteFiles)
	})
}

func postPluginUpdate(c *gin.Context) {
	pluginAction(c, func(m *plugins.Manager, id string) error { return m.Update(id) })
}

// putPluginSettings saves an operator's settings for a plugin.
func putPluginSettings(c *gin.Context) {
	var body map[string]any
	if err := c.BindJSON(&body); err != nil {
		return
	}

	pluginAction(c, func(m *plugins.Manager, id string) error {
		return m.SaveSettings(id, body)
	})
}

// postPluginOrder sets the order plugins load, and therefore the order their
// hooks run in.
func postPluginOrder(c *gin.Context) {
	m, ok := requirePluginManager(c)
	if !ok {
		return
	}

	var body struct {
		Order []string `json:"order"`
	}
	if err := c.BindJSON(&body); err != nil {
		return
	}

	if err := m.SetLoadOrder(body.Order); err != nil {
		c.AbortWithStatusJSON(http.StatusConflict, gin.H{"error": err.Error()})
		return
	}

	getPlugins(c)
}

// maxPluginUploadBytes bounds an uploaded archive before it is written to disk.
// The manager applies the configured limit to the archive's contents as well;
// this only stops an obviously oversized upload from being spooled first.
const maxPluginUploadBytes = 64 << 20

// postPluginImport installs a plugin from an uploaded archive or a URL.
func postPluginImport(c *gin.Context) {
	m, ok := requirePluginManager(c)
	if !ok {
		return
	}

	// A URL body and a multipart upload are both accepted, matching the two
	// ways the Panel imports its own plugins.
	if strings.HasPrefix(c.ContentType(), "application/json") {
		var body struct {
			URL string `json:"url"`
		}
		if err := c.BindJSON(&body); err != nil {
			return
		}
		if body.URL == "" {
			c.AbortWithStatusJSON(http.StatusUnprocessableEntity, gin.H{"error": "A url is required to import a plugin."})
			return
		}

		id, err := m.InstallFromURL(body.URL, "")
		if err != nil {
			c.AbortWithStatusJSON(http.StatusConflict, gin.H{"error": err.Error()})
			return
		}

		respondWithImported(c, m, id)
		return
	}

	file, err := c.FormFile("plugin")
	if err != nil {
		c.AbortWithStatusJSON(http.StatusUnprocessableEntity, gin.H{"error": "No plugin archive was uploaded."})
		return
	}
	if file.Size > maxPluginUploadBytes {
		c.AbortWithStatusJSON(http.StatusRequestEntityTooLarge, gin.H{"error": "The uploaded archive is too large."})
		return
	}

	src, err := file.Open()
	if err != nil {
		middleware.CaptureAndAbort(c, err)
		return
	}
	defer src.Close()

	tmp, err := os.CreateTemp("", "wings-plugin-upload-*.zip")
	if err != nil {
		middleware.CaptureAndAbort(c, err)
		return
	}
	defer os.Remove(tmp.Name())

	if _, err := io.Copy(tmp, io.LimitReader(src, maxPluginUploadBytes)); err != nil {
		tmp.Close()
		middleware.CaptureAndAbort(c, err)
		return
	}
	tmp.Close()

	id, err := m.InstallFromArchive(tmp.Name(), "")
	if err != nil {
		c.AbortWithStatusJSON(http.StatusConflict, gin.H{"error": err.Error()})
		return
	}

	respondWithImported(c, m, id)
}

func respondWithImported(c *gin.Context, m *plugins.Manager, id string) {
	inst, found := m.Get(id)
	if !found {
		c.Status(http.StatusCreated)
		return
	}
	c.JSON(http.StatusCreated, toPluginResponse(m, inst))
}

// getPluginSource returns one of a plugin's source files.
//
// A Wings plugin is distributed as readable source precisely so an operator can
// see what it does, and asking them to SSH into the node to exercise that is a
// good way to make sure nobody ever does. This lets the Panel show the source
// next to the enable button.
func getPluginSource(c *gin.Context) {
	m, ok := requirePluginManager(c)
	if !ok {
		return
	}

	inst, found := m.Get(c.Param("plugin"))
	if !found {
		c.AbortWithStatusJSON(http.StatusNotFound, gin.H{"error": "No plugin with that id exists on this node."})
		return
	}

	dir := inst.Manifest().Directory()

	// With no file named, list what there is to read.
	name := c.Query("file")
	if name == "" {
		entries, err := os.ReadDir(dir)
		if err != nil {
			middleware.CaptureAndAbort(c, err)
			return
		}

		files := make([]string, 0, len(entries))
		for _, e := range entries {
			if e.IsDir() {
				continue
			}
			if strings.HasSuffix(e.Name(), ".go") || e.Name() == plugins.ManifestName || strings.EqualFold(e.Name(), "README.md") {
				files = append(files, e.Name())
			}
		}
		sortStrings(files)

		c.JSON(http.StatusOK, gin.H{"files": files})
		return
	}

	// Only a plain file name is accepted, so this cannot be used to read
	// arbitrary paths on the node by way of a plugin id.
	if name != filepath.Base(name) || strings.HasPrefix(name, ".") {
		c.AbortWithStatusJSON(http.StatusBadRequest, gin.H{"error": "That is not a file name this endpoint will read."})
		return
	}
	if !strings.HasSuffix(name, ".go") && name != plugins.ManifestName && !strings.EqualFold(name, "README.md") {
		c.AbortWithStatusJSON(http.StatusBadRequest, gin.H{"error": "Only a plugin's source, manifest and readme can be read."})
		return
	}

	content, err := os.ReadFile(filepath.Join(dir, name))
	if err != nil {
		if os.IsNotExist(err) {
			c.AbortWithStatusJSON(http.StatusNotFound, gin.H{"error": "That file is not part of this plugin."})
			return
		}
		middleware.CaptureAndAbort(c, err)
		return
	}

	c.JSON(http.StatusOK, gin.H{"file": name, "content": string(content)})
}
