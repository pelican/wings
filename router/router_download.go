package router

import (
	"errors"
	"io"
	"net/http"
	"os"
	"strconv"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"

	"github.com/pelican/wings/router/middleware"
	"github.com/pelican/wings/router/tokens"
	"github.com/pelican/wings/server/backup"
)

// Download tokens are not single-use. Browsers (e.g. Chromium's download
// protection), antivirus software and download managers commonly request the
// same signed URL more than once, and a resumed or parallel download is a new
// request as well. Treating the token as single-use made those requests fail
// with a 404 partway through large downloads. A download token stays valid until
// the expiry set by the Panel, and is still subject to the user denylist.

// Handle a download request for a server backup.
func getDownloadBackup(c *gin.Context) {
	client := middleware.ExtractApiClient(c)

	// Get the payload from the token.
	token := tokens.BackupPayload{}
	if err := tokens.ParseToken([]byte(c.Query("token")), &token); err != nil {
		middleware.CaptureAndAbort(c, err)
		return
	}

	// The server does not have to exist on this node: backups kept after their
	// server was deleted can still be downloaded, to create a new server from them.
	// The token is signed by the Panel and the backup is looked up below.
	if token.Denylisted() || !token.HasScope(tokens.BackupDownload) {
		c.AbortWithStatusJSON(http.StatusNotFound, gin.H{
			"error": "The requested resource was not found on this server.",
		})
		return
	}
	middleware.ClearDeadlines(c)

	// Validate that the BackupUuid and ServerUuid fields are actually UUIDs and not some
	// random characters or a file path, the server UUID is part of the backup's path.
	if _, err := uuid.Parse(token.BackupUuid); err != nil {
		middleware.CaptureAndAbort(c, err)
		return
	}
	if !isCanonicalUuid(token.ServerUuid) {
		c.AbortWithStatusJSON(http.StatusNotFound, gin.H{
			"error": "The requested resource was not found on this server.",
		})
		return
	}

	// Locate the backup on the local disk.
	b, st, err := backup.LocateLocal(client, token.BackupUuid, token.ServerUuid)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			c.AbortWithStatusJSON(http.StatusNotFound, gin.H{
				"error": "The requested backup was not found on this server.",
			})
			return
		}

		middleware.CaptureAndAbort(c, err)
		return
	}

	// The use of `os` here is safe as backups are not stored within server directories.
	// this path is program-controlled, not user input.
	f, err := os.Open(b.Path())
	if err != nil {
		middleware.CaptureAndAbort(c, err)
		return
	}
	defer f.Close()

	// Backups are gzipped tarballs, which is also what a restore from this link
	// accepts.
	serveDownloadAs(c, f, st.Name(), st.ModTime(), "application/gzip")
}

// Handles downloading a specific file for a server.
func getDownloadFile(c *gin.Context) {
	manager := middleware.ExtractManager(c)
	token := tokens.FilePayload{}
	if err := tokens.ParseToken([]byte(c.Query("token")), &token); err != nil {
		middleware.CaptureAndAbort(c, err)
		return
	}

	s, ok := manager.Get(token.ServerUuid)
	if !ok || token.Denylisted() || !token.HasScope(tokens.FileDownload) {
		c.AbortWithStatusJSON(http.StatusNotFound, gin.H{
			"error": "The requested resource was not found on this server.",
		})
		return
	}
	middleware.ClearDeadlines(c)

	if err := s.Filesystem().IsIgnored(token.FilePath); err != nil {
		middleware.CaptureAndAbort(c, err)
		return
	}

	f, st, err := s.Filesystem().File(token.FilePath)
	if err != nil {
		middleware.CaptureAndAbort(c, err)
		return
	}
	defer f.Close()
	if st.IsDir() {
		c.AbortWithStatusJSON(http.StatusNotFound, gin.H{
			"error": "The requested resource was not found on this server.",
		})
		return
	}

	serveDownload(c, f, st.Name(), st.ModTime())
}

// serveDownload writes the content of f as a file attachment. Range, If-Range
// and HEAD requests are handled by http.ServeContent, which allows clients to
// resume an interrupted download or fetch it over multiple connections.
func serveDownload(c *gin.Context, f io.ReadSeeker, name string, modTime time.Time) {
	serveDownloadAs(c, f, name, modTime, "application/octet-stream")
}

// serveDownloadAs is serveDownload with the given content type.
func serveDownloadAs(c *gin.Context, f io.ReadSeeker, name string, modTime time.Time, contentType string) {
	c.Header("Content-Disposition", "attachment; filename="+strconv.Quote(name))
	// Set explicitly so ServeContent does not sniff the content type.
	c.Header("Content-Type", contentType)

	http.ServeContent(c.Writer, c.Request, name, modTime, f)
}
