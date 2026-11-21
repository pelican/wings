package router

import (
	"bufio"
	"bytes"
	"context"
	"crypto/tls"
	"encoding/json"
	stderrors "errors"
	"io"
	"mime"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"os"
	"strings"
	"time"

	"emperror.dev/errors"
	"github.com/apex/log"
	"github.com/gin-gonic/gin"
	"github.com/google/uuid"

	"github.com/pelican/wings/config"
	wnet "github.com/pelican/wings/internal/network"
	"github.com/pelican/wings/router/middleware"
	"github.com/pelican/wings/server"
	"github.com/pelican/wings/server/backup"
	"github.com/pelican/wings/server/filesystem"
)

// sharedAddressSpace is the carrier-grade NAT range, which nodes on some private
// networks (such as Tailscale) are reached over. Like every other internal
// range it is blocked by wnet.IsInternal, unless private addresses are allowed.
var sharedAddressSpace = netip.MustParsePrefix("100.64.0.0/10")

type backupDownloadError string

func (e backupDownloadError) Error() string {
	return string(e)
}

// postServerBackup performs a backup against a given server instance using the
// provided backup adapter.
func postServerBackup(c *gin.Context) {
	s := middleware.ExtractServer(c)
	client := middleware.ExtractApiClient(c)
	logger := middleware.ExtractLogger(c)
	var data struct {
		Adapter backup.AdapterType `json:"adapter"`
		Uuid    string             `json:"uuid"`
		Ignore  string             `json:"ignore"`
		// The server's configuration, stored as the first entry of the archive so
		// a new server can be created from the backup. Null when it is not stored.
		PelicanConfig json.RawMessage `json:"pelican_config"`
	}
	if err := c.BindJSON(&data); err != nil {
		return
	}

	pelicanConfig, err := parsePelicanConfig(data.PelicanConfig)
	if err != nil {
		c.AbortWithStatusJSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}

	backupUuid, ok := parseBackupUuid(c, data.Uuid)
	if !ok {
		return
	}

	if err := filesystem.ValidateIgnore(data.Ignore); err != nil {
		c.AbortWithStatusJSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}

	var adapter backup.BackupInterface
	switch data.Adapter {
	case backup.LocalBackupAdapter:
		b := backup.NewLocal(client, backupUuid, s.ID(), data.Ignore)
		b.SetPelicanConfig(pelicanConfig)
		adapter = b
	case backup.S3BackupAdapter:
		b := backup.NewS3(client, backupUuid, s.ID(), data.Ignore)
		b.SetPelicanConfig(pelicanConfig)
		adapter = b
	default:
		middleware.CaptureAndAbort(c, errors.New("router/backups: provided adapter is not valid: "+string(data.Adapter)))
		return
	}

	// Attach the server ID and the request ID to the adapter log context for easier
	// parsing in the logs.
	adapter.WithLogContext(map[string]interface{}{
		"server":     s.ID(),
		"request_id": c.GetString("request_id"),
	})

	go func(b backup.BackupInterface, s *server.Server, logger *log.Entry) {
		if err := s.Backup(b); err != nil {
			logger.WithField("error", errors.WithStackIf(err)).Error("router: failed to generate server backup")
		}
	}(adapter, s, logger)

	c.Status(http.StatusAccepted)
}

// postServerRestoreBackup handles restoring a backup for a server by downloading
// or finding the given backup on the system and then unpacking the archive into
// the server's data directory. If the TruncateDirectory field is provided and
// is true all of the files will be deleted for the server.
//
// The backup does not have to belong to the server. When a server is created
// from another server's backup, SourceServer names the server the backup was
// made of, which may since have been deleted. A local backup that is stored on
// another node is downloaded from that node using DownloadUrl.
//
// TODO: stop the server if it is running
func postServerRestoreBackup(c *gin.Context) {
	s := middleware.ExtractServer(c)
	client := middleware.ExtractApiClient(c)
	logger := middleware.ExtractLogger(c)

	var data struct {
		Adapter           backup.AdapterType `binding:"required,oneof=wings s3" json:"adapter"`
		TruncateDirectory bool               `json:"truncate_directory"`
		// A UUID is always required for this endpoint, however the download URL
		// is only present when the given adapter type is s3, or when a local
		// backup is stored on another node.
		DownloadUrl string `json:"download_url"`
		// The server the backup was made of, when it is not this server.
		SourceServer string `json:"source_server"`
	}
	if err := c.BindJSON(&data); err != nil {
		return
	}
	backupUuid, ok := parseBackupUuid(c, c.Param("backup"))
	if !ok {
		return
	}
	sourceServer := s.ID()
	if data.SourceServer != "" {
		if !isCanonicalUuid(data.SourceServer) {
			c.AbortWithStatusJSON(http.StatusBadRequest, gin.H{"error": "The source_server field must be a valid UUID."})
			return
		}
		sourceServer = data.SourceServer
	}
	if data.Adapter == backup.S3BackupAdapter && data.DownloadUrl == "" {
		c.AbortWithStatusJSON(http.StatusBadRequest, gin.H{"error": "The download_url field is required when the backup adapter is set to S3."})
		return
	}

	// A local backup is restored from the disk when this node has it. Otherwise
	// it is on another node, and is downloaded from there when a link is given.
	var local *backup.LocalBackup
	if data.Adapter == backup.LocalBackupAdapter {
		b, _, err := backup.LocateLocal(client, backupUuid, sourceServer)
		switch {
		case err == nil:
			local = b
		case errors.Is(err, os.ErrNotExist) && data.DownloadUrl != "":
			// Downloaded from the node that has it below.
		case errors.Is(err, os.ErrNotExist):
			c.AbortWithStatusJSON(http.StatusNotFound, gin.H{"error": "The requested backup was not found on this node."})
			return
		default:
			middleware.CaptureAndAbort(c, err)
			return
		}
	}

	// Links to a backup on another node are generated by the Panel, and nodes are
	// commonly reached over a private network, so those addresses are allowed.
	allowPrivate := data.Adapter == backup.LocalBackupAdapter
	if local == nil {
		if err := validateBackupDownloadUrl(data.DownloadUrl, allowPrivate); err != nil {
			c.AbortWithStatusJSON(http.StatusBadRequest, gin.H{"error": err.Error()})
			return
		}
	}

	// Mark the server as restoring. This fails if a restore is already in
	// progress, or if the server is being installed or transferred, which must
	// never have their files replaced from under them. RestoreBackup clears it.
	if !s.StartRestoring() {
		c.AbortWithStatusJSON(http.StatusConflict, gin.H{
			"error": "This server is already restoring a backup, or is being installed or transferred.",
		})
		return
	}
	hasError := true
	defer func() {
		if !hasError {
			return
		}

		s.SetRestoring(false)
	}()

	logger.Info("processing server backup restore request")
	if data.TruncateDirectory {
		// The files are only deleted by RestoreBackup, once the server has stopped
		// and the whole backup was read without error, so that a restore that
		// cannot happen, or a corrupt backup, leaves them alone.
		logger.Info("received \"truncate_directory\" flag in request: server files will be deleted before restoring")
	}

	if local != nil {
		go func(s *server.Server, b backup.BackupInterface, logger *log.Entry) {
			logger.Info("starting restoration process for server backup using local driver")
			if err := s.RestoreBackup(b, nil, data.TruncateDirectory); err != nil {
				logger.WithField("error", err).Error("failed to restore local backup to server")
			}
			s.Events().Publish(server.DaemonMessageEvent, "Completed server restoration from local backup.")
			s.Events().Publish(server.BackupRestoreCompletedEvent, "")
			logger.Info("completed server restoration from local backup")
		}(s, local, logger)
		hasError = false
		c.Status(http.StatusAccepted)
		return
	}

	// Since this is not a local backup we need to stream the archive and then
	// parse over the contents as we go in order to restore it to the server.
	httpClient := backupRestoreHttpClient(allowPrivate, restoreTLSConfig(c))
	logger.Info("downloading backup from remote location...")
	// TODO: this will hang if there is an issue. We can't use c.Request.Context() (or really any)
	//  since it will be canceled when the request is closed which happens quickly since we push
	//  this into the background.
	//
	// For now I'm just using the server context so at least the request is canceled if
	// the server gets deleted.
	req, err := http.NewRequestWithContext(s.Context(), http.MethodGet, data.DownloadUrl, nil)
	if err != nil {
		middleware.CaptureAndAbort(c, err)
		return
	}
	res, err := httpClient.Do(req)
	if err != nil {
		var downloadErr backupDownloadError
		if stderrors.As(err, &downloadErr) {
			c.AbortWithStatusJSON(http.StatusBadRequest, gin.H{"error": downloadErr.Error()})
			return
		}
		middleware.CaptureAndAbort(c, err)
		return
	}
	if res.StatusCode != http.StatusOK {
		_ = res.Body.Close()
		c.AbortWithStatusJSON(http.StatusBadRequest, gin.H{"error": "The provided backup link returned an invalid response status: " + res.Status})
		return
	}
	// Don't allow content types that we know are going to give us problems.
	archive, err := openRestoreArchive(res)
	if err != nil {
		_ = res.Body.Close()
		c.AbortWithStatusJSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}

	go func(s *server.Server, uuid string, logger *log.Entry) {
		logger.Info("starting restoration process for server backup from remote location")
		// Any link is streamed the same way, the S3 adapter only reads the archive.
		// The Panel is notified with the identifier it gave.
		if err := s.RestoreBackup(backup.NewS3(client, uuid, s.ID(), ""), archive, data.TruncateDirectory); err != nil {
			logger.WithField("error", errors.WithStack(err)).Error("failed to restore remote backup to server")
		}
		s.Events().Publish(server.DaemonMessageEvent, "Completed server restoration from remote backup.")
		s.Events().Publish(server.BackupRestoreCompletedEvent, "")
		logger.Info("completed server restoration from remote backup")
	}(s, backupUuid, logger)

	hasError = false
	c.Status(http.StatusAccepted)
}

// deleteKeptBackup deletes a local backup of a server that no longer exists on
// this node, because the backup was kept when the server was deleted. A 404 is
// returned if the backup is not on this node.
func deleteKeptBackup(c *gin.Context) {
	serverUuid := c.Param("server")
	if !isCanonicalUuid(serverUuid) {
		c.AbortWithStatusJSON(http.StatusBadRequest, gin.H{"error": "The server identifier must be a valid UUID."})
		return
	}
	backupUuid, ok := parseBackupUuid(c, c.Param("backup"))
	if !ok {
		return
	}
	b, _, err := backup.LocateLocal(middleware.ExtractApiClient(c), backupUuid, serverUuid)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			c.AbortWithStatusJSON(http.StatusNotFound, gin.H{
				"error": "The requested backup was not found on this node.",
			})
			return
		}
		middleware.CaptureAndAbort(c, err)
		return
	}
	if err := b.Remove(); err != nil && !errors.Is(err, os.ErrNotExist) {
		middleware.CaptureAndAbort(c, err)
		return
	}
	c.Status(http.StatusNoContent)
}

// parsePelicanConfig checks the server configuration sent with a backup request
// and returns it compacted, or nil when none was sent.
func parsePelicanConfig(raw json.RawMessage) ([]byte, error) {
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 || bytes.Equal(trimmed, []byte("null")) {
		return nil, nil
	}
	var compact bytes.Buffer
	if trimmed[0] != '{' || json.Compact(&compact, trimmed) != nil {
		return nil, errors.New("The pelican_config field must be a JSON object.")
	}
	if compact.Len() > backup.MaxPelicanConfigSize {
		return nil, errors.New("The pelican_config field is too large.")
	}
	return compact.Bytes(), nil
}

// isCanonicalUuid reports whether value is a UUID in its canonical lowercase
// form, so it is safe to use in a path.
func isCanonicalUuid(value string) bool {
	parsed, err := uuid.Parse(value)
	return err == nil && value == parsed.String()
}

// deleteServerBackup deletes a local backup of a server. If the backup is not
// found on the machine just return a 404 error. The service calling this
// endpoint can make its own decisions as to how it wants to handle that
// response.
func deleteServerBackup(c *gin.Context) {
	backupUuid, ok := parseBackupUuid(c, c.Param("backup"))
	if !ok {
		return
	}
	b, _, err := backup.LocateLocal(middleware.ExtractApiClient(c), backupUuid, middleware.ExtractServer(c).ID())
	if err != nil {
		// Just return from the function at this point if the backup was not located.
		if errors.Is(err, os.ErrNotExist) {
			c.AbortWithStatusJSON(http.StatusNotFound, gin.H{
				"error": "The requested backup was not found on this server.",
			})
			return
		}
		middleware.CaptureAndAbort(c, err)
		return
	}
	// I'm not entirely sure how likely this is to happen, however if we did manage to
	// locate the backup previously and it is now missing when we go to delete, just
	// treat it as having been successful, rather than returning a 404.
	if err := b.Remove(); err != nil && !errors.Is(err, os.ErrNotExist) {
		middleware.CaptureAndAbort(c, err)
		return
	}
	c.Status(http.StatusNoContent)
}

func parseBackupUuid(c *gin.Context, value string) (string, bool) {
	parsed, err := uuid.Parse(value)
	if err == nil && len(value) == len(parsed.String()) && parsed.String() == strings.ToLower(value) {
		return parsed.String(), true
	}
	c.AbortWithStatusJSON(http.StatusBadRequest, gin.H{"error": "The backup identifier must be a valid UUID."})
	return "", false
}

func validateBackupDownloadUrl(raw string, allowPrivate bool) error {
	parsed, err := url.Parse(raw)
	if err != nil || parsed.Host == "" {
		return backupDownloadError("The provided backup link is not a valid URL.")
	}
	if parsed.Scheme != "http" && parsed.Scheme != "https" {
		return backupDownloadError("The provided backup link must use HTTP or HTTPS.")
	}
	if ip := net.ParseIP(parsed.Hostname()); ip != nil && isBlockedRestoreAddress(parsed.Hostname(), ip, allowPrivate) {
		return backupDownloadError("The provided backup link resolves to a blocked address.")
	}
	return nil
}

// restoreTLSConfig returns the manager's TLS configuration for a restore
// download (server.WithRestoreTLSConfig), or nil for the default, also when
// the request carries no manager.
func restoreTLSConfig(c *gin.Context) *tls.Config {
	if v, ok := c.Get("manager"); ok {
		if m, ok := v.(*server.Manager); ok {
			return m.RestoreTLSConfig()
		}
	}
	return nil
}

// backupRestoreHttpClient returns the client a restore downloads its archive
// with. A non-nil tlsConfig, from the manager's WithRestoreTLSConfig, replaces
// the TLS configuration of http.DefaultTransport.
func backupRestoreHttpClient(allowPrivate bool, tlsConfig *tls.Config) http.Client {
	transport := http.DefaultTransport.(*http.Transport).Clone()
	if tlsConfig != nil {
		transport.TLSClientConfig = tlsConfig
	}
	transport.Proxy = nil
	transport.ResponseHeaderTimeout = 30 * time.Second
	dialer := &net.Dialer{
		Timeout:   30 * time.Second,
		KeepAlive: 30 * time.Second,
	}
	transport.DialContext = func(ctx context.Context, network string, address string) (net.Conn, error) {
		host, port, err := net.SplitHostPort(address)
		if err != nil {
			return nil, err
		}
		ips, err := net.DefaultResolver.LookupIPAddr(ctx, host)
		if err != nil {
			return nil, err
		}
		if len(ips) == 0 {
			return nil, errors.New("router/backups: backup download host did not resolve to any addresses")
		}
		for _, resolved := range ips {
			if isBlockedRestoreAddress(host, resolved.IP, allowPrivate) {
				return nil, backupDownloadError("The provided backup link resolves to a blocked address.")
			}
		}
		var lastErr error
		for _, resolved := range ips {
			conn, err := dialer.DialContext(ctx, network, net.JoinHostPort(resolved.IP.String(), port))
			if err == nil {
				return conn, nil
			}
			lastErr = err
		}
		return nil, lastErr
	}
	return http.Client{
		Transport: transport,
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			if len(via) >= 10 {
				return backupDownloadError("The provided backup link redirects too many times.")
			}
			return validateBackupDownloadUrl(req.URL.String(), allowPrivate)
		},
	}
}

// isBlockedRestoreAddress reports whether a backup may not be downloaded from the
// given address. With allowPrivate, private network addresses (RFC 1918, unique
// local and shared address space) are allowed, loopback and link-local are not.
func isBlockedRestoreAddress(host string, ip net.IP, allowPrivate bool) bool {
	if allowPrivate {
		if addr, ok := netip.AddrFromSlice(ip); ok {
			addr = addr.Unmap()
			if addr.IsPrivate() || sharedAddressSpace.Contains(addr) {
				return false
			}
		}
	}
	return isBlockedBackupRestoreIP(host, ip)
}

func isBlockedBackupRestoreIP(host string, ip net.IP) bool {
	addr, ok := netip.AddrFromSlice(ip)
	if !ok {
		return true
	}
	// Anything other than a globally reachable unicast address is blocked, unless
	// the operator allowed it. This includes 0.0.0.0/8, which reaches the node
	// itself on Linux, and NAT64 addresses embedding an internal IPv4 address.
	return wnet.IsInternal(addr) && !wnet.IsAllowed(config.Get().System.Backups.RestoreHostAllowlist, host, addr)
}

// openRestoreArchive returns the body of a backup download, if it is a gzip
// archive. Many hosts (GitHub releases, CDNs, object stores) serve archives as
// generic binary data, so for those the body has to start with the gzip magic
// bytes instead. Anything else, like an HTML error or login page, is refused.
// restoreProbeTimeout is how long the start of a download without a gzip content
// type is waited for.
var restoreProbeTimeout = 30 * time.Second

func openRestoreArchive(res *http.Response) (io.ReadCloser, error) {
	contentType := res.Header.Get("Content-Type")
	if isSupportedBackupRestoreContentType(contentType) {
		return res.Body, nil
	}
	if !isGenericBinaryContentType(contentType) {
		return nil, stderrors.New("The provided backup link is not a supported content type. \"" + contentType + "\" is not application/x-gzip.")
	}

	// The response headers have arrived, but the host can still hold back the body
	// while the request is handled. Closing the body ends the wait.
	buffered := bufio.NewReader(res.Body)
	timer := time.AfterFunc(restoreProbeTimeout, func() { _ = res.Body.Close() })
	magic, err := buffered.Peek(2)
	if !timer.Stop() {
		return nil, stderrors.New("The provided backup link did not send any data.")
	}
	if err != nil || magic[0] != 0x1f || magic[1] != 0x8b {
		return nil, stderrors.New("The provided backup link did not return a gzip archive.")
	}
	return struct {
		io.Reader
		io.Closer
	}{buffered, res.Body}, nil
}

// isGenericBinaryContentType reports whether a content type says nothing more
// than that the response is binary data, or is missing.
func isGenericBinaryContentType(value string) bool {
	mediaType, _, err := mime.ParseMediaType(value)
	if err != nil {
		mediaType = strings.TrimSpace(value)
	}
	switch strings.ToLower(mediaType) {
	case "", "application/octet-stream", "binary/octet-stream":
		return true
	default:
		return false
	}
}

func isSupportedBackupRestoreContentType(value string) bool {
	mediaType, _, err := mime.ParseMediaType(value)
	if err != nil {
		mediaType = strings.TrimSpace(value)
	}
	switch strings.ToLower(mediaType) {
	case "application/x-gzip", "application/gzip":
		return true
	default:
		return false
	}
}
