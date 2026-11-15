package router

import (
	"bytes"
	"crypto/rand"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
	"unicode/utf8"

	"github.com/apex/log"
	"github.com/gin-gonic/gin"

	"github.com/pelican/wings/config"
	"github.com/pelican/wings/plugin"
	"github.com/pelican/wings/plugin/install"
)

// The Panel's plugin API. The node operator sets how much of it the Panel may
// use (plugins.panel_access, see plugin/access.go); every handler checks that
// first. Changes go through the same code and locks as `wings plugins`: the
// config file lock for enable/disable, the install lock for installs, and the
// trust store's lock for approvals. plugin/API.md documents it.

// registerPluginAPI mounts the plugin API on routes that already require the
// node token.
func registerPluginAPI(r gin.IRoutes) {
	r.GET("/api/plugins", getPlugins)
	r.GET("/api/plugins/available", getPluginsAvailable)
	r.POST("/api/plugins/import", postPluginImport)
	r.POST("/api/plugins/order", postPluginOrder)
	r.GET("/api/plugins/:id", getPlugin)
	r.GET("/api/plugins/:id/source", getPluginSource)
	r.GET("/api/plugins/:id/update-check", getPluginUpdateCheck)
	r.POST("/api/plugins/:id/enable", postPluginToggle(true))
	r.POST("/api/plugins/:id/disable", postPluginToggle(false))
	r.PUT("/api/plugins/:id/settings", putPluginSettings)
	r.POST("/api/plugins/:id/install", postPluginInstall)
	r.POST("/api/plugins/:id/update", postPluginUpdate)
	r.POST("/api/plugins/:id/uninstall", postPluginUninstall)
}

const (
	maxPluginJSONBody  = 256 << 10 // settings and install/import requests
	maxSourceFileBytes = 1 << 20   // one file shown by /source
	maxSourceEntries   = 5000      // files listed by /source
	maxPendingInstalls = 16
)

// pendingInstallTTL is how long a staged install waits for its approval. A
// variable so tests can shorten it.
var pendingInstallTTL = 10 * time.Minute

// --- access ---------------------------------------------------------------

// requiredLevel is the lowest panel_access level that allows act.
func requiredLevel(act plugin.PanelAction) plugin.PanelAccess {
	switch act {
	case plugin.ActionInstallSigned:
		return plugin.PanelAccessSigned
	case plugin.ActionInstallAny:
		return plugin.PanelAccessFull
	default:
		return plugin.PanelAccessView
	}
}

// allowPlugins answers 403 and returns false unless this node's panel_access
// level allows act.
func allowPlugins(c *gin.Context, act plugin.PanelAction) bool {
	level := plugin.CurrentPanelAccess()
	if level.Allows(act) {
		return true
	}
	need := requiredLevel(act)
	c.AbortWithStatusJSON(http.StatusForbidden, gin.H{
		"error":        fmt.Sprintf("plugins.panel_access is %q on this node; this request needs %q or higher. Only the node operator can change it, in config.yml.", level, need),
		"panel_access": level,
		"required":     need,
	})
	return false
}

// pluginID returns the :id path parameter, or answers 400 if it can't name a
// plugin directory. An empty id (/api/plugins//uninstall) is refused here, so
// it can never be joined onto the plugins directory and mean the directory
// itself.
func pluginID(c *gin.Context) (string, bool) {
	id := c.Param("id")
	if plugin.InvalidName(id) {
		c.AbortWithStatusJSON(http.StatusBadRequest, gin.H{"error": fmt.Sprintf("invalid plugin id %q", id)})
		return "", false
	}
	return id, true
}

// requireTrusted answers 409 (reason "untrusted") unless the node trusts m as
// it is on disk. Anything that fetches a manifest's update URL goes through
// it: an unapproved or edited manifest could point the node at any URL,
// including internal services (contract v3.1).
func requireTrusted(c *gin.Context, id string, m plugin.Manifest) bool {
	if ts, err := plugin.LoadTrustStore(); err == nil && ts.IsTrusted(m) {
		return true
	}
	c.AbortWithStatusJSON(http.StatusConflict, gin.H{
		"error":  fmt.Sprintf("plugin %q is not trusted on this node; the node operator must run `wings plugins trust %s` first", id, id),
		"reason": "untrusted",
	})
	return false
}

// approvePath is where the approval for a staged install or update of name
// goes. The name is path-escaped: names may hold characters such as "?".
func approvePath(action, name string) string {
	return "/api/plugins/" + url.PathEscape(name) + "/" + action
}

func logPluginChange(c *gin.Context, action, name string) *log.Entry {
	return log.WithFields(log.Fields{
		"action":      action,
		"plugin":      name,
		"remote_addr": c.ClientIP(),
		"request_id":  c.GetString("request_id"),
	})
}

// pluginsChanged lets the daemon pick up a plugin that just became loadable
// (installed, trusted or updated), like enable and disable do through
// plugin.PolicyChanged.
func pluginsChanged() {
	if plugin.AfterPolicyChange != nil {
		plugin.AfterPolicyChange()
	}
}

// --- listing --------------------------------------------------------------

// pluginJSON is one plugin as the Panel sees it. The field names follow what
// the Panel's WingsPlugin model reads (pelican/panel PR 2701).
type pluginJSON struct {
	ID           string            `json:"id"`
	Name         string            `json:"name"`
	Kind         plugin.Kind       `json:"kind"`
	Author       string            `json:"author,omitempty"`
	Version      string            `json:"version,omitempty"`
	Description  string            `json:"description,omitempty"`
	Category     string            `json:"category"`
	Capability   plugin.Capability `json:"capability,omitempty"`
	URL          string            `json:"url,omitempty"`
	UpdateURL    string            `json:"update_url,omitempty"`
	WingsVersion string            `json:"wings_version,omitempty"`
	APIVersion   int               `json:"api_version"`
	Transport    string            `json:"transport"`
	Events       []string          `json:"events"`
	Entrypoint   []string          `json:"entrypoint,omitempty"`
	HostAccess   []string          `json:"host_access"`
	Settings     []plugin.Setting  `json:"settings"`
	Meta         pluginMetaJSON    `json:"meta"`
}

type pluginMetaJSON struct {
	// Status is one of the Panel's PluginStatus values: enabled, disabled,
	// errored.
	Status        string         `json:"status"`
	StatusMessage string         `json:"status_message,omitempty"`
	LoadOrder     int            `json:"load_order"`
	Registered    registeredJSON `json:"registered"`
	// Values are the stored settings, secrets masked.
	Values       map[string]any `json:"values"`
	Enabled      bool           `json:"enabled"`
	EnableReason string         `json:"enable_reason,omitempty"`
	// Trust is native, trusted, untrusted, or changed (approved once, but
	// its files or manifest changed since).
	Trust        string `json:"trust"`
	IsTrusted    bool   `json:"is_trusted"`
	CanEnable    bool   `json:"can_enable"`
	CanDisable   bool   `json:"can_disable"`
	CanConfigure bool   `json:"can_configure"`
	CanUpdate    bool   `json:"can_update"`
	CanUninstall bool   `json:"can_uninstall"`
	Error        string `json:"error,omitempty"`
}

type registeredJSON struct {
	Hooks []string `json:"hooks"`
}

func describePlugin(info plugin.ProviderInfo, ts *plugin.TrustStore, level plugin.PanelAccess) pluginJSON {
	p := pluginJSON{
		ID:         info.Name,
		Name:       info.Name,
		Kind:       info.Kind,
		Author:     info.Author,
		Version:    info.Version,
		Category:   string(info.Capability),
		Capability: info.Capability,
		URL:        info.Source,
		UpdateURL:  info.Update,
		Transport:  info.Transport,
		Events:     nonNil(info.Events),
		HostAccess: []string{},
		Settings:   []plugin.Setting{},
	}
	if p.Category == "" {
		p.Category = "plugin"
	}
	meta := pluginMetaJSON{
		Registered:   registeredJSON{Hooks: p.Events},
		Values:       map[string]any{},
		Enabled:      info.Enabled,
		EnableReason: info.Reason,
		IsTrusted:    info.Trusted,
		Error:        info.Err,
	}

	var manifest *plugin.Manifest
	if info.Kind == plugin.KindExternal && info.Err == "" && info.Manifest != "" {
		if m, err := plugin.LoadManifest(info.Manifest); err != nil {
			meta.Error = err.Error()
		} else {
			manifest = &m
		}
	}
	if manifest != nil {
		p.Description = manifest.Description
		p.WingsVersion = manifest.MinWings
		p.APIVersion = apiVersionNumber(manifest.APIVersion)
		p.Entrypoint = manifest.Entrypoint
		p.HostAccess = nonNil(manifest.Requires.HostAPI)
		p.Settings = nonNil(manifest.Settings)
		if values, err := plugin.LoadSettings(*manifest); err != nil {
			meta.Error = "settings: " + err.Error()
		} else {
			meta.Values = plugin.MaskSecrets(*manifest, values)
		}
	}

	switch {
	case info.Kind == plugin.KindNative:
		meta.Trust = "native"
	case info.Trusted:
		meta.Trust = "trusted"
	case ts != nil && ts.Approved[info.Name] != "":
		meta.Trust = "changed"
	default:
		meta.Trust = "untrusted"
	}

	runnable := info.Kind == plugin.KindNative || info.Trusted
	switch {
	case meta.Error != "":
		meta.Status, meta.StatusMessage = "errored", meta.Error
	case !runnable && meta.Trust == "changed":
		meta.Status, meta.StatusMessage = "disabled", "its files changed since it was approved; the node operator must run `wings plugins trust "+info.Name+"`"
	case !runnable:
		meta.Status, meta.StatusMessage = "disabled", "not trusted on this node; the node operator must run `wings plugins trust "+info.Name+"`"
	case info.Enabled:
		meta.Status = "enabled"
	default:
		meta.Status, meta.StatusMessage = "disabled", info.Reason
	}

	external := info.Kind == plugin.KindExternal
	meta.CanEnable = level.Allows(plugin.ActionToggle) && !info.Enabled && runnable && meta.Error == ""
	meta.CanDisable = level.Allows(plugin.ActionToggle) && info.Enabled
	meta.CanConfigure = level.Allows(plugin.ActionSettings) && manifest != nil && info.Trusted && len(manifest.Settings) > 0
	meta.CanUpdate = level.Allows(plugin.ActionInstallSigned) && manifest != nil && manifest.Update != ""
	meta.CanUninstall = level.Allows(plugin.ActionInstallSigned) && external
	p.Meta = meta
	return p
}

// apiVersionNumber turns a manifest apiVersion ("v1") into the Panel's
// integer api_version.
func apiVersionNumber(s string) int {
	n, _ := strconv.Atoi(strings.TrimPrefix(strings.ToLower(s), "v"))
	return n
}

func nonNil[T any](s []T) []T {
	if s == nil {
		return []T{}
	}
	return s
}

func getPlugins(c *gin.Context) {
	if !allowPlugins(c, plugin.ActionList) {
		return
	}
	level := plugin.CurrentPanelAccess()
	inv, invErr := plugin.Inventory()
	ts, _ := plugin.LoadTrustStore()
	data := make([]pluginJSON, 0, len(inv))
	for _, info := range inv {
		data = append(data, describePlugin(info, ts, level))
	}
	resp := gin.H{
		"enabled":      true,
		"directory":    config.Get().Plugins.Directory,
		"panel_access": level,
		"data":         data,
	}
	if invErr != nil {
		resp["error"] = invErr.Error()
	}
	c.JSON(http.StatusOK, resp)
}

// findPlugin answers 404 unless the inventory has a plugin named id.
func findPlugin(c *gin.Context, id string) (plugin.ProviderInfo, bool) {
	info, ok, _ := plugin.Find(id)
	if !ok {
		c.AbortWithStatusJSON(http.StatusNotFound, gin.H{"error": fmt.Sprintf("no plugin named %q on this node", id)})
		return info, false
	}
	return info, true
}

func respondPlugin(c *gin.Context, status int, id string, extra gin.H) {
	info, ok, _ := plugin.Find(id)
	if !ok {
		c.JSON(status, extra)
		return
	}
	ts, _ := plugin.LoadTrustStore()
	out := gin.H{"plugin": describePlugin(info, ts, plugin.CurrentPanelAccess())}
	for k, v := range extra {
		out[k] = v
	}
	c.JSON(status, out)
}

func getPlugin(c *gin.Context) {
	if !allowPlugins(c, plugin.ActionList) {
		return
	}
	id, ok := pluginID(c)
	if !ok {
		return
	}
	info, ok := findPlugin(c, id)
	if !ok {
		return
	}
	ts, _ := plugin.LoadTrustStore()
	c.JSON(http.StatusOK, describePlugin(info, ts, plugin.CurrentPanelAccess()))
}

// availableJSON is one installable registry entry (contract A3.2).
type availableJSON struct {
	Name         string            `json:"name"`
	Version      string            `json:"version"`
	Registry     string            `json:"registry"`
	Author       string            `json:"author,omitempty"`
	Description  string            `json:"description,omitempty"`
	Capability   plugin.Capability `json:"capability,omitempty"`
	HasSignature bool              `json:"has_signature"`
	// Installed is the installed version, or null when it isn't installed.
	Installed *string `json:"installed"`
}

// getPluginsAvailable lists what the node's configured registries offer
// Wings, at the level that may install it. A registry that can't be read is
// reported under its namespace in "errors"; the call itself succeeds.
func getPluginsAvailable(c *gin.Context) {
	if !allowPlugins(c, plugin.ActionInstallSigned) {
		return
	}
	installed := map[string]string{}
	inv, _ := plugin.Inventory()
	for _, info := range inv {
		if info.Kind == plugin.KindExternal {
			installed[info.Name] = info.Version
		}
	}
	data := []availableJSON{}
	errs := map[string]string{}
	for _, l := range install.ListRegistries(c.Request.Context()) {
		if l.Err != nil {
			errs[l.Namespace] = l.Err.Error()
			continue
		}
		for _, e := range l.Entries {
			a := availableJSON{
				Name: e.Name, Version: e.Version, Registry: l.Namespace,
				Author: e.Author, Description: e.Description, Capability: e.Capability,
				HasSignature: e.Signature != "",
			}
			if v, ok := installed[e.Name]; ok {
				a.Installed = &v
			}
			data = append(data, a)
		}
	}
	c.JSON(http.StatusOK, gin.H{"data": data, "errors": errs})
}

// getPluginUpdateCheck reports whether the plugin's update URL offers a newer
// version, without downloading or staging anything. A plugin without an
// update URL has latest null; a fetch that fails is a 200 with "error".
func getPluginUpdateCheck(c *gin.Context) {
	if !allowPlugins(c, plugin.ActionList) {
		return
	}
	id, ok := pluginID(c)
	if !ok {
		return
	}
	info, ok := findPlugin(c, id)
	if !ok {
		return
	}
	resp := gin.H{"installed": info.Version, "latest": nil, "available": false}
	if info.Kind != plugin.KindExternal || info.Manifest == "" {
		c.JSON(http.StatusOK, resp)
		return
	}
	if info.Err != "" {
		resp["error"] = info.Err
		c.JSON(http.StatusOK, resp)
		return
	}
	m, err := plugin.LoadManifest(info.Manifest)
	if err != nil {
		resp["error"] = err.Error()
		c.JSON(http.StatusOK, resp)
		return
	}
	resp["installed"] = m.Version
	if !requireTrusted(c, id, m) {
		return
	}
	if m.Update == "" {
		c.JSON(http.StatusOK, resp)
		return
	}
	st, err := install.CheckUpdate(c.Request.Context(), m)
	if err != nil {
		resp["error"] = err.Error()
		c.JSON(http.StatusOK, resp)
		return
	}
	resp["latest"], resp["available"] = st.Latest, st.Available
	c.JSON(http.StatusOK, resp)
}

// --- source ---------------------------------------------------------------

// getPluginSource lists the files in a plugin's own directory, or returns one
// of them with ?file=. Everything is read through an os.Root opened on that
// directory, so no path or symlink reaches outside it.
func getPluginSource(c *gin.Context) {
	if !allowPlugins(c, plugin.ActionList) {
		return
	}
	id, ok := pluginID(c)
	if !ok {
		return
	}
	info, ok := findPlugin(c, id)
	if !ok {
		return
	}
	if info.Kind != plugin.KindExternal || info.Manifest == "" {
		c.AbortWithStatusJSON(http.StatusBadRequest, gin.H{"error": "native plugins are compiled into Wings and have no files to show"})
		return
	}
	root, err := os.OpenRoot(filepath.Dir(info.Manifest))
	if err != nil {
		abortInternal(c, err)
		return
	}
	defer root.Close()

	file := c.Query("file")
	if file == "" {
		var files []string
		truncated := false
		err := fs.WalkDir(root.FS(), ".", func(name string, d fs.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if d.IsDir() {
				return nil
			}
			if len(files) >= maxSourceEntries {
				truncated = true
				return fs.SkipAll
			}
			files = append(files, name)
			return nil
		})
		if err != nil {
			abortInternal(c, err)
			return
		}
		c.JSON(http.StatusOK, gin.H{"files": nonNil(files), "truncated": truncated})
		return
	}

	// Non-blocking, so a FIFO (or another special file) can't hold the request
	// open: the regular-file check below then refuses it. Reads of a regular
	// file are unaffected.
	f, err := root.OpenFile(filepath.FromSlash(file), os.O_RDONLY|syscall.O_NONBLOCK, 0)
	if err != nil {
		c.AbortWithStatusJSON(http.StatusNotFound, gin.H{"error": fmt.Sprintf("no file %q in plugin %q", file, id)})
		return
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil {
		abortInternal(c, err)
		return
	}
	if !st.Mode().IsRegular() {
		c.AbortWithStatusJSON(http.StatusBadRequest, gin.H{"error": fmt.Sprintf("%q is not a regular file", file)})
		return
	}
	if st.Size() > maxSourceFileBytes {
		c.AbortWithStatusJSON(http.StatusRequestEntityTooLarge, gin.H{"error": fmt.Sprintf("%q is larger than %d bytes", file, maxSourceFileBytes), "size": st.Size()})
		return
	}
	b, err := io.ReadAll(io.LimitReader(f, maxSourceFileBytes+1))
	if err != nil {
		abortInternal(c, err)
		return
	}
	if len(b) > maxSourceFileBytes {
		c.AbortWithStatusJSON(http.StatusRequestEntityTooLarge, gin.H{"error": fmt.Sprintf("%q is larger than %d bytes", file, maxSourceFileBytes)})
		return
	}
	if !utf8.Valid(b) {
		c.JSON(http.StatusOK, gin.H{"file": file, "size": len(b), "binary": true})
		return
	}
	c.JSON(http.StatusOK, gin.H{"file": file, "size": len(b), "binary": false, "content": string(b)})
}

func abortInternal(c *gin.Context, err error) {
	c.AbortWithStatusJSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
}

// --- enable, disable, settings -------------------------------------------

func postPluginToggle(enable bool) gin.HandlerFunc {
	verb := "disable"
	if enable {
		verb = "enable"
	}
	return func(c *gin.Context) {
		if !allowPlugins(c, plugin.ActionToggle) {
			return
		}
		id, ok := pluginID(c)
		if !ok {
			return
		}
		info, ok := findPlugin(c, id)
		if !ok {
			return
		}
		// Enabling must not reach code the operator has not approved.
		// Disabling is always safe, so any listed plugin can be disabled.
		if enable {
			if info.Err != "" {
				c.AbortWithStatusJSON(http.StatusConflict, gin.H{"error": fmt.Sprintf("plugin %q has an error and can't be enabled: %s", id, info.Err)})
				return
			}
			if info.Kind != plugin.KindNative && !info.Trusted {
				c.AbortWithStatusJSON(http.StatusConflict, gin.H{"error": fmt.Sprintf("plugin %q is not trusted on this node; the node operator must run `wings plugins trust %s` first", id, id)})
				return
			}
		}
		// UpdatePluginsOnDisk also sets the running block, under the file lock.
		if _, err := config.UpdatePluginsOnDisk(config.Get(), func(p *config.PluginConfiguration) {
			install.Toggle(p, id, enable)
		}); err != nil {
			abortInternal(c, fmt.Errorf("writing the config file: %w", err))
			return
		}
		plugin.PolicyChanged()
		logPluginChange(c, verb, id).Info("plugin changed through the Panel API")
		respondPlugin(c, http.StatusOK, id, gin.H{"status": verb + "d"})
	}
}

// readJSONObject reads a request body of at most maxPluginJSONBody bytes as a
// JSON object. An empty body, null or [] (what the Panel's HTTP client sends
// for "no data") gives an empty object.
func readJSONObject(c *gin.Context) (map[string]json.RawMessage, bool) {
	b, err := io.ReadAll(http.MaxBytesReader(c.Writer, c.Request.Body, maxPluginJSONBody))
	if err != nil {
		c.AbortWithStatusJSON(http.StatusRequestEntityTooLarge, gin.H{"error": fmt.Sprintf("request body is larger than %d bytes", maxPluginJSONBody)})
		return nil, false
	}
	b = bytes.TrimSpace(b)
	switch string(b) {
	case "", "null", "[]", "{}":
		return map[string]json.RawMessage{}, true
	}
	var obj map[string]json.RawMessage
	if err := json.Unmarshal(b, &obj); err != nil {
		c.AbortWithStatusJSON(http.StatusBadRequest, gin.H{"error": "request body must be a JSON object: " + err.Error()})
		return nil, false
	}
	return obj, true
}

func stringField(obj map[string]json.RawMessage, key string) (string, error) {
	raw, ok := obj[key]
	if !ok || string(raw) == "null" {
		return "", nil
	}
	var s string
	if err := json.Unmarshal(raw, &s); err != nil {
		return "", fmt.Errorf("%s must be a string", key)
	}
	return s, nil
}

// putPluginSettings replaces a plugin's setting values. The body is the
// values object itself, as the Panel sends it. See plugin.UpdateSettings for
// nulls and masked secrets.
func putPluginSettings(c *gin.Context) {
	if !allowPlugins(c, plugin.ActionSettings) {
		return
	}
	id, ok := pluginID(c)
	if !ok {
		return
	}
	info, ok := findPlugin(c, id)
	if !ok {
		return
	}
	if info.Kind != plugin.KindExternal || info.Manifest == "" || info.Err != "" {
		c.AbortWithStatusJSON(http.StatusConflict, gin.H{"error": fmt.Sprintf("plugin %q has no settings", id)})
		return
	}
	if !info.Trusted {
		c.AbortWithStatusJSON(http.StatusConflict, gin.H{"error": fmt.Sprintf("plugin %q is not trusted on this node; the node operator must run `wings plugins trust %s` first", id, id)})
		return
	}
	m, err := plugin.LoadManifest(info.Manifest)
	if err != nil {
		c.AbortWithStatusJSON(http.StatusConflict, gin.H{"error": err.Error()})
		return
	}
	raw, ok := readJSONObject(c)
	if !ok {
		return
	}
	values := make(map[string]any, len(raw))
	for k, v := range raw {
		var val any
		if err := json.Unmarshal(v, &val); err != nil {
			c.AbortWithStatusJSON(http.StatusBadRequest, gin.H{"error": err.Error()})
			return
		}
		values[k] = val
	}
	saved, err := plugin.UpdateSettings(m, values)
	var bad plugin.SettingsError
	switch {
	case errors.As(err, &bad):
		c.AbortWithStatusJSON(http.StatusUnprocessableEntity, gin.H{"error": bad.Error(), "fields": bad})
		return
	case errors.Is(err, plugin.ErrSettingsTooLarge):
		c.AbortWithStatusJSON(http.StatusRequestEntityTooLarge, gin.H{"error": err.Error()})
		return
	case err != nil:
		abortInternal(c, err)
		return
	}
	logPluginChange(c, "settings", id).Info("plugin changed through the Panel API")
	c.JSON(http.StatusOK, gin.H{"status": "saved", "values": plugin.MaskSecrets(m, saved)})
}

// --- staged installs ------------------------------------------------------

// pendingInstall is a staged install waiting for the Panel's approval.
type pendingInstall struct {
	token  string
	action string // install, update or import
	name   string // the staged plugin's name
	stage  *install.StagedInstall
	digest string
	check  func(plugin.Manifest) error
	// signed records that the archive's signature verified against a
	// trusted key when it was staged, at any level. Such a stage needs only
	// the signed level to be approved; any other needs full.
	signed bool
	// destination is what the plugin's directory held at staging
	// (destinationState); the approval is refused if it changed.
	destination string
	expires     time.Time
	timer       *time.Timer
}

type pendingInstalls struct {
	mu      sync.Mutex
	entries map[string]*pendingInstall // by token
}

var pending = &pendingInstalls{entries: map[string]*pendingInstall{}}

var errPendingFull = errors.New("too many installs are waiting for approval; approve them or let them expire first")

// add keeps p until it is taken or expires. A newer stage of the same plugin
// replaces an older one.
func (s *pendingInstalls) add(p *pendingInstall) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	for tok, old := range s.entries {
		if old.name == p.name {
			s.dropLocked(tok)
		}
	}
	if len(s.entries) >= maxPendingInstalls {
		return errPendingFull
	}
	p.expires = time.Now().Add(pendingInstallTTL)
	s.entries[p.token] = p
	token := p.token
	p.timer = time.AfterFunc(pendingInstallTTL, func() {
		s.mu.Lock()
		defer s.mu.Unlock()
		if s.entries[token] == p {
			s.dropLocked(token)
		}
	})
	return nil
}

// dropLocked forgets an entry and removes its staging directory.
func (s *pendingInstalls) dropLocked(token string) {
	p := s.entries[token]
	if p == nil {
		return
	}
	delete(s.entries, token)
	if p.timer != nil {
		p.timer.Stop()
	}
	if err := p.stage.Cleanup(); err != nil {
		log.WithField("plugin", p.name).WithField("error", err).Warn("could not remove a staged plugin install")
	}
}

var errBadApproval = errors.New("unknown, used or expired approval token; stage the install again")

// take returns and forgets the entry for token, if it is for action (and, when
// name is set, that plugin) and has not expired. A token for another action
// or plugin is refused and left in place.
func (s *pendingInstalls) take(token, action, name string) (*pendingInstall, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var found *pendingInstall
	for tok, p := range s.entries {
		if subtle.ConstantTimeCompare([]byte(tok), []byte(token)) == 1 {
			found = p
		}
	}
	if found == nil || found.action != action || (name != "" && found.name != name) {
		return nil, errBadApproval
	}
	if time.Now().After(found.expires) {
		s.dropLocked(found.token)
		return nil, errBadApproval
	}
	delete(s.entries, found.token)
	found.timer.Stop()
	return found, nil
}

func newApprovalToken() (string, error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return hex.EncodeToString(b), nil
}

// stagedPluginJSON is the staged manifest shown for approval.
type stagedPluginJSON struct {
	Name        string            `json:"name"`
	Version     string            `json:"version,omitempty"`
	Author      string            `json:"author,omitempty"`
	Description string            `json:"description,omitempty"`
	Capability  plugin.Capability `json:"capability"`
	Source      string            `json:"source,omitempty"`
	Entrypoint  []string          `json:"entrypoint"`
	Events      []string          `json:"events"`
	Settings    []plugin.Setting  `json:"settings"`
}

// respondStaged vets a fresh stage, keeps it for approval and answers 202
// with what is to be approved. The stage is cleaned up on every failure.
func respondStaged(c *gin.Context, action, path string, stage *install.StagedInstall, check func(plugin.Manifest) error) {
	m, err := stage.Manifest()
	if err == nil && check != nil {
		err = check(m)
	}
	var digest, token, destination string
	if err == nil {
		digest, err = stage.Digest()
	}
	if err == nil {
		destination, err = destinationState(stage, m.Name)
	}
	if err == nil {
		token, err = newApprovalToken()
	}
	if err != nil {
		_ = stage.Cleanup()
		c.AbortWithStatusJSON(http.StatusUnprocessableEntity, gin.H{"error": err.Error()})
		return
	}
	var replaces any
	if old, err := plugin.LoadManifest(filepath.Join(stage.DestDir, plugin.ManifestFile)); err == nil {
		replaces = old.Version
	} else if _, statErr := os.Lstat(stage.DestDir); statErr == nil {
		replaces = ""
	}
	p := &pendingInstall{token: token, action: action, name: m.Name, stage: stage, digest: digest, check: check, signed: stage.Signed(), destination: destination}
	if err := pending.add(p); err != nil {
		_ = stage.Cleanup()
		c.AbortWithStatusJSON(http.StatusTooManyRequests, gin.H{"error": err.Error()})
		return
	}
	logPluginChange(c, action+"-staged", m.Name).Info("plugin staged through the Panel API; waiting for approval")
	if path == "" {
		path = approvePath(action, m.Name)
	}
	c.JSON(http.StatusAccepted, gin.H{
		"status":         "approval_required",
		"action":         action,
		"plugin":         stagedPluginJSON{Name: m.Name, Version: m.Version, Author: m.Author, Description: m.Description, Capability: m.Capability, Source: m.Source, Entrypoint: nonNil(m.Entrypoint), Events: nonNil(plugin.EffectiveEvents(m)), Settings: nonNil(m.Settings)},
		"host_access":    nonNil(m.Requires.HostAPI),
		"replaces":       replaces,
		"signed":         p.signed,
		"approval_token": token,
		"expires_at":     p.expires.UTC().Format(time.RFC3339),
		"approve_with":   gin.H{"method": http.MethodPost, "path": path, "body": gin.H{"approve": token}},
	})
}

// destinationState describes what the plugin named name has on disk now: the
// contents of the directory the stage would replace, and every other
// directory a plugin of that name was discovered in. The approval compares it
// with the value at staging, so an install, removal or update made in between
// (by the CLI, or another Panel request) is not silently overwritten.
func destinationState(stage *install.StagedInstall, name string) (string, error) {
	dest, err := stage.DestinationState()
	if err != nil {
		return "", err
	}
	state := "dest:" + dest
	destDir, err := filepath.Abs(stage.DestDir)
	if err != nil {
		return "", err
	}
	manifests, issues, err := plugin.DiscoverDetailed(filepath.Dir(destDir))
	if err != nil {
		return "", err
	}
	for _, m := range manifests {
		if m.Name == name && m.Dir() != destDir {
			state += "\x00elsewhere:" + m.Dir()
		}
	}
	for _, issue := range issues {
		if issue.Name == name {
			state += "\x00elsewhere:" + issue.Dir
		}
	}
	return state, nil
}

// Errors from the checks made at approval, under the install lock.
var errDestinationChanged = errors.New("the plugin's directory changed since this install was staged (it was installed, removed or updated meanwhile); stage it again")

type levelRefused struct{ level, need plugin.PanelAccess }

func (e levelRefused) Error() string {
	return fmt.Sprintf("plugins.panel_access is %q on this node; approving this stage needs %q", e.level, e.need)
}

type signatureRefused struct{ err error }

func (e signatureRefused) Error() string {
	return "the staged archive no longer passes this node's signature policy: " + e.err.Error()
}

// commitApproved installs a pending stage whose token the Panel sent back,
// with the same install code the CLI uses after its prompt. Right before the
// plugin is moved into place, under the install lock, it checks the node as
// it is now: its access level still allows the stage (403), the archive still
// passes its signature policy (422), and the destination is as it was when
// staged (409).
func commitApproved(c *gin.Context, action, name, token string) {
	p, err := pending.take(token, action, name)
	if err != nil {
		c.AbortWithStatusJSON(http.StatusConflict, gin.H{"error": err.Error(), "reason": "token"})
		return
	}
	m, err := install.InstallIf(p.stage, nil, func(plugin.Manifest) (bool, error) { return true, nil }, func(m plugin.Manifest) error {
		level := plugin.CurrentPanelAccess()
		need := plugin.ActionInstallSigned
		if action == "import" || !p.signed {
			need = plugin.ActionInstallAny
		}
		if !level.Allows(need) {
			return levelRefused{level: level, need: requiredLevel(need)}
		}
		opts := stageOptions()
		if err := p.stage.CheckSignature(opts...); err != nil {
			return signatureRefused{err}
		}
		d, err := p.stage.Digest()
		if err != nil {
			return err
		}
		if d != p.digest {
			return errors.New("the staged files changed after the approval token was issued; stage the install again")
		}
		if m.Name != p.name {
			return fmt.Errorf("staged manifest name %q does not match %q", m.Name, p.name)
		}
		if p.check != nil {
			if err := p.check(m); err != nil {
				return err
			}
		}
		dest, err := destinationState(p.stage, p.name)
		if err != nil {
			return err
		}
		if dest != p.destination {
			return errDestinationChanged
		}
		return nil
	})
	var lr levelRefused
	var sr signatureRefused
	switch {
	case errors.As(err, &lr):
		c.AbortWithStatusJSON(http.StatusForbidden, gin.H{"error": lr.Error(), "panel_access": lr.level, "required": lr.need})
		return
	case errors.As(err, &sr):
		c.AbortWithStatusJSON(http.StatusUnprocessableEntity, gin.H{"error": sr.Error()})
		return
	case errors.Is(err, errDestinationChanged):
		c.AbortWithStatusJSON(http.StatusConflict, gin.H{"error": err.Error(), "reason": "destination_changed"})
		return
	case err != nil:
		c.AbortWithStatusJSON(http.StatusUnprocessableEntity, gin.H{"error": err.Error()})
		return
	}
	logPluginChange(c, action, m.Name).WithField("version", m.Version).Info("plugin changed through the Panel API")
	pluginsChanged()
	status := "installed"
	if action == "update" {
		status = "updated"
	}
	respondPlugin(c, http.StatusOK, m.Name, gin.H{"status": status})
}

// stageOptions is what the level requires of an archive: below full, a
// signature by a key in plugins.trusted_keys.
func stageOptions() []install.StageOption {
	if plugin.CurrentPanelAccess().Allows(plugin.ActionInstallAny) {
		return nil
	}
	return []install.StageOption{install.RequireTrustedSignature()}
}

// isInstalled reports whether id has a directory in dir, or a plugin of that
// name was discovered in a directory of another name.
func isInstalled(dir, id string) bool {
	if _, err := os.Lstat(filepath.Join(dir, id)); err == nil {
		return true
	}
	info, ok, _ := plugin.Find(id)
	return ok && info.Kind == plugin.KindExternal
}

// postPluginInstall installs a plugin from the node's configured registries.
// Without "approve" it stages and answers 202 with an approval token; with
// {"approve": token} it installs what was staged.
func postPluginInstall(c *gin.Context) {
	if !allowPlugins(c, plugin.ActionInstallSigned) {
		return
	}
	id, ok := pluginID(c)
	if !ok {
		return
	}
	body, ok := readJSONObject(c)
	if !ok {
		return
	}
	approve, err1 := stringField(body, "approve")
	registry, err2 := stringField(body, "registry")
	if err := errors.Join(err1, err2); err != nil {
		c.AbortWithStatusJSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	if approve != "" {
		commitApproved(c, "install", id, approve)
		return
	}
	// Only bare names and configured namespaces: Resolve also takes
	// "url#name", which would fetch a registry the operator never set up.
	if strings.ContainsAny(id, "#@") || strings.ContainsAny(registry, "#@/") {
		c.AbortWithStatusJSON(http.StatusBadRequest, gin.H{"error": "plugin ids and registry names can't contain #, @ or /"})
		return
	}
	dir := config.Get().Plugins.Directory
	if isInstalled(dir, id) {
		c.AbortWithStatusJSON(http.StatusConflict, gin.H{"error": fmt.Sprintf("plugin %q is already installed; use update", id)})
		return
	}
	ref := id
	if registry != "" {
		ref = "@" + registry + "/" + id
	}
	resolved, err := install.Resolve(c.Request.Context(), ref)
	if err != nil {
		c.AbortWithStatusJSON(http.StatusUnprocessableEntity, gin.H{"error": err.Error()})
		return
	}
	opts := stageOptions()
	stage, err := install.StageAdd(c.Request.Context(), resolved, dir, opts...)
	if err != nil {
		c.AbortWithStatusJSON(http.StatusUnprocessableEntity, gin.H{"error": err.Error()})
		return
	}
	respondStaged(c, "install", "", stage, nil)
}

// postPluginUpdate updates an installed plugin from its update manifest, in
// the same two steps as install.
func postPluginUpdate(c *gin.Context) {
	if !allowPlugins(c, plugin.ActionInstallSigned) {
		return
	}
	id, ok := pluginID(c)
	if !ok {
		return
	}
	body, ok := readJSONObject(c)
	if !ok {
		return
	}
	approve, err := stringField(body, "approve")
	if err != nil {
		c.AbortWithStatusJSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	if approve != "" {
		commitApproved(c, "update", id, approve)
		return
	}
	info, ok := findPlugin(c, id)
	if !ok {
		return
	}
	if info.Kind != plugin.KindExternal || info.Manifest == "" {
		c.AbortWithStatusJSON(http.StatusBadRequest, gin.H{"error": "only external plugins can be updated; native ones ship with Wings"})
		return
	}
	if info.Err != "" {
		c.AbortWithStatusJSON(http.StatusConflict, gin.H{"error": fmt.Sprintf("plugin %q has an error: %s", id, info.Err)})
		return
	}
	m, err := plugin.LoadManifest(info.Manifest)
	if err != nil {
		c.AbortWithStatusJSON(http.StatusConflict, gin.H{"error": err.Error()})
		return
	}
	if !requireTrusted(c, id, m) {
		return
	}
	st, err := install.CheckUpdate(c.Request.Context(), m)
	if err != nil {
		c.AbortWithStatusJSON(http.StatusUnprocessableEntity, gin.H{"error": err.Error()})
		return
	}
	if !st.Available {
		c.JSON(http.StatusOK, gin.H{"status": "up_to_date", "installed": st.Installed, "latest": st.Latest})
		return
	}
	opts := stageOptions()
	stage, err := install.StageUpdate(c.Request.Context(), st, opts...)
	if err != nil {
		c.AbortWithStatusJSON(http.StatusUnprocessableEntity, gin.H{"error": err.Error()})
		return
	}
	respondStaged(c, "update", "", stage, func(m plugin.Manifest) error {
		if m.Name != st.Name {
			return fmt.Errorf("staged manifest name %q does not match plugin %q", m.Name, st.Name)
		}
		if m.Version != st.Latest {
			return fmt.Errorf("staged manifest version %q does not match update version %q", m.Version, st.Latest)
		}
		return nil
	})
}

// postPluginUninstall disables a plugin, revokes its approval and, with
// ?delete=true, removes the directory its manifest is in. Its data directory
// is kept.
func postPluginUninstall(c *gin.Context) {
	if !allowPlugins(c, plugin.ActionInstallSigned) {
		return
	}
	id, ok := pluginID(c)
	if !ok {
		return
	}
	var deleteFiles bool
	switch c.Query("delete") {
	case "", "false", "0":
	case "true", "1":
		deleteFiles = true
	default:
		c.AbortWithStatusJSON(http.StatusBadRequest, gin.H{"error": "delete must be true or false"})
		return
	}
	info, ok := findPlugin(c, id)
	if !ok {
		return
	}
	if info.Kind != plugin.KindExternal {
		c.AbortWithStatusJSON(http.StatusBadRequest, gin.H{"error": "native plugins ship with Wings and can't be uninstalled; disable it instead"})
		return
	}
	var dirName string
	if deleteFiles {
		var err error
		if dirName, err = pluginDirName(info); err != nil {
			c.AbortWithStatusJSON(http.StatusConflict, gin.H{"error": err.Error()})
			return
		}
	}
	if _, err := config.UpdatePluginsOnDisk(config.Get(), func(p *config.PluginConfiguration) {
		install.Toggle(p, id, false)
	}); err != nil {
		abortInternal(c, fmt.Errorf("writing the config file: %w", err))
		return
	}
	plugin.PolicyChanged()
	ts, err := plugin.LoadTrustStore()
	if err == nil {
		err = ts.Revoke(id)
	}
	if err != nil {
		abortInternal(c, fmt.Errorf("revoking trust (the plugin is disabled): %w", err))
		return
	}
	if deleteFiles {
		if err := install.Remove(config.Get().Plugins.Directory, dirName); err != nil {
			abortInternal(c, fmt.Errorf("deleting files (the plugin is disabled and untrusted): %w", err))
			return
		}
	}
	logPluginChange(c, "uninstall", id).WithField("deleted", deleteFiles).Info("plugin changed through the Panel API")
	c.JSON(http.StatusOK, gin.H{"status": "uninstalled", "deleted": deleteFiles})
}

// pluginDirName returns the name, inside the plugins directory, of the
// directory holding info's manifest. Discovery accepts a plugin in a directory
// of any name ("notify" in notify-main), so it is not always the plugin's
// name, and removing <plugins>/<name> could miss the plugin or hit something
// else. A manifest that isn't directly in a sub directory of the plugins
// directory is refused.
func pluginDirName(info plugin.ProviderInfo) (string, error) {
	if info.Manifest == "" {
		return "", fmt.Errorf("plugin %q has no directory to delete", info.Name)
	}
	pluginsDir, err := filepath.Abs(config.Get().Plugins.Directory)
	if err != nil {
		return "", err
	}
	dir, err := filepath.Abs(filepath.Dir(info.Manifest))
	if err != nil {
		return "", err
	}
	name := filepath.Base(dir)
	if filepath.Dir(dir) != pluginsDir || plugin.InvalidName(name) {
		return "", fmt.Errorf("plugin %q is not in its own directory under %s; not deleting %s", info.Name, pluginsDir, dir)
	}
	return name, nil
}

// postPluginImport installs from any https URL ({"url": ...}) or an uploaded
// archive (multipart field "plugin"), in the same two steps as install. The
// plugin's name comes from the archive's manifest; an existing plugin of that
// name is replaced.
func postPluginImport(c *gin.Context) {
	if !allowPlugins(c, plugin.ActionInstallAny) {
		return
	}
	dir := config.Get().Plugins.Directory
	if strings.HasPrefix(c.ContentType(), "multipart/") {
		c.Request.Body = http.MaxBytesReader(c.Writer, c.Request.Body, install.MaxArchiveBytes+(1<<20))
		fh, err := c.FormFile("plugin")
		if err != nil {
			c.AbortWithStatusJSON(http.StatusBadRequest, gin.H{"error": "upload the archive in the multipart field \"plugin\": " + err.Error()})
			return
		}
		tmpDir, err := os.MkdirTemp("", "wings-plugin-upload-")
		if err != nil {
			abortInternal(c, err)
			return
		}
		defer os.RemoveAll(tmpDir)
		archive := filepath.Join(tmpDir, "upload")
		if err := c.SaveUploadedFile(fh, archive); err != nil {
			abortInternal(c, err)
			return
		}
		stage, err := install.StageFile(c.Request.Context(), archive, c.PostForm("signature"), dir)
		if err != nil {
			c.AbortWithStatusJSON(http.StatusUnprocessableEntity, gin.H{"error": err.Error()})
			return
		}
		respondStaged(c, "import", "/api/plugins/import", stage, nil)
		return
	}

	body, ok := readJSONObject(c)
	if !ok {
		return
	}
	var fields [4]string
	var errs []error
	for i, k := range []string{"approve", "url", "sha256", "signature"} {
		v, err := stringField(body, k)
		fields[i] = v
		errs = append(errs, err)
	}
	if err := errors.Join(errs...); err != nil {
		c.AbortWithStatusJSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	approve, url, sha, sig := fields[0], fields[1], fields[2], fields[3]
	if approve != "" {
		commitApproved(c, "import", "", approve)
		return
	}
	if url == "" {
		c.AbortWithStatusJSON(http.StatusBadRequest, gin.H{"error": "send {\"url\": \"https://...\"} or a multipart upload in the field \"plugin\""})
		return
	}
	stage, err := install.StageURL(c.Request.Context(), url, sha, sig, dir)
	if err != nil {
		c.AbortWithStatusJSON(http.StatusUnprocessableEntity, gin.H{"error": err.Error()})
		return
	}
	respondStaged(c, "import", "/api/plugins/import", stage, nil)
}

// postPluginOrder exists because the Panel calls it. Wings has no load order.
func postPluginOrder(c *gin.Context) {
	if !allowPlugins(c, plugin.ActionToggle) {
		return
	}
	c.AbortWithStatusJSON(http.StatusNotImplemented, gin.H{"error": "Wings plugins have no load order: each external plugin is its own process, and every event goes to every plugin subscribed to it. There is nothing to set."})
}
