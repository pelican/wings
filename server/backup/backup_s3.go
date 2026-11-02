package backup

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"time"

	"emperror.dev/errors"
	"github.com/cenkalti/backoff/v4"
	"github.com/juju/ratelimit"
	"github.com/mholt/archives"
	"golang.org/x/sync/errgroup"

	"github.com/pelican/wings/config"
	"github.com/pelican/wings/remote"
	"github.com/pelican/wings/server/filesystem"
)

type S3Backup struct {
	Backup
}

var _ BackupInterface = (*S3Backup)(nil)

func NewS3(client remote.Client, uuid string, suuid string, ignore string) *S3Backup {
	return &S3Backup{
		Backup{
			client:     client,
			Uuid:       uuid,
			ServerUuid: suuid,
			Ignore:     ignore,
			adapter:    S3BackupAdapter,
		},
	}
}

// Remove removes a backup from the system.
func (s *S3Backup) Remove() error {
	if err := s.validateIdentifier(); err != nil {
		return err
	}
	return os.Remove(s.Path())
}

// WithLogContext attaches additional context to the log output for this backup.
func (s *S3Backup) WithLogContext(c map[string]interface{}) {
	s.logContext = c
}

// Generate creates a new backup on the disk, moves it into the S3 bucket via
// the provided presigned URL, and then deletes the backup from the disk.
func (s *S3Backup) Generate(ctx context.Context, fsys *filesystem.Filesystem, ignore string) (*ArchiveDetails, error) {
	if err := s.validateIdentifier(); err != nil {
		return nil, err
	}
	defer s.Remove()

	a := &filesystem.Archive{
		Filesystem: fsys,
		Ignore:     ignore,
	}

	s.log().WithField("path", s.Path()).Info("creating backup for server")
	if _, err := os.Stat(filepath.Dir(s.Path())); os.IsNotExist(err) {
		err := os.Mkdir(filepath.Dir(s.Path()), 0o700)
		if err != nil {
			return nil, err
		}
	}
	if err := a.Create(ctx, s.Path()); err != nil {
		return nil, err
	}
	s.log().Info("created backup successfully")

	rc, err := os.Open(s.Path())
	if err != nil {
		return nil, errors.Wrap(err, "backup: could not read archive from disk")
	}
	defer rc.Close()

	parts, err := s.generateRemoteRequest(ctx, rc)
	if err != nil {
		return nil, err
	}
	ad, err := s.Details(ctx, parts)
	if err != nil {
		return nil, errors.WrapIf(err, "backup: failed to get archive details after upload")
	}
	return ad, nil
}

// Restore will read from the provided reader assuming that it is a gzipped
// tar reader. When a file is encountered in the archive the callback function
// will be triggered. If the callback returns an error the entire process is
// stopped, otherwise this function will run until all files have been written.
//
// This restoration uses a workerpool to use up to the number of CPUs available
// on the machine when writing files to the disk.
func (s *S3Backup) Restore(ctx context.Context, r io.Reader, callback RestoreCallback) error {
	reader := r
	// Steal the logic we use for making backups which will be applied when restoring
	// this specific backup. This allows us to prevent overloading the disk unintentionally.
	if writeLimit := int64(config.Get().System.Backups.WriteLimit * 1024 * 1024); writeLimit > 0 {
		reader = ratelimit.Reader(r, ratelimit.NewBucketWithRate(float64(writeLimit), writeLimit))
	}
	if err := format.Extract(ctx, reader, func(ctx context.Context, f archives.FileInfo) error {
		r, err := f.Open()
		if err != nil {
			return err
		}
		defer r.Close()

		return callback(f.NameInArchive, f.FileInfo, r)
	}); err != nil {
		return err
	}
	return nil
}

// Generates the remote S3 request and begins the upload.
func (s *S3Backup) generateRemoteRequest(ctx context.Context, rc *os.File) ([]remote.BackupPart, error) {
	s.log().Debug("attempting to get size of backup...")
	st, err := rc.Stat()
	if err != nil {
		return nil, err
	}
	size := st.Size()
	s.log().WithField("size", size).Debug("got size of backup")

	s.log().Debug("attempting to get S3 upload urls from Panel...")
	urls, err := s.client.GetBackupRemoteUploadURLs(ctx, s.Backup.Uuid, size)
	if err != nil {
		return nil, err
	}
	s.log().Debug("got S3 upload urls from the Panel")
	s.log().WithField("parts", len(urls.Parts)).Info("attempting to upload backup to s3 endpoint...")

	concurrency := urls.MaxConcurrentUploads

	// Always allow at least 1 upload at time
	if concurrency <= 0 {
		concurrency = 1
	}
	
	uploader := newS3FileUploader(concurrency)
	// Close all connections when the function completes
	defer uploader.client.CloseIdleConnections()

	parts := make([]remote.BackupPart, len(urls.Parts))

	g, ctx := errgroup.WithContext(ctx)
	g.SetLimit(concurrency)

	for i, part := range urls.Parts {
		// Get the size for the current part.
		partSize := urls.PartSize
		if i+1 == len(urls.Parts) {
			// This is the remaining size for the last part, there is not a
			// minimum size limit for the last part.
			partSize = size - (int64(i) * urls.PartSize)
		}
		offset := int64(i) * urls.PartSize

		g.Go(func() error {
			// Each part gets its own independent view of the file, backed by
			// ReadAt, which is safe for concurrent use.
			section := io.NewSectionReader(rc, offset, partSize)

			etag, err := uploader.uploadPart(ctx, part, section, partSize)
			if err != nil {
				s.log().WithField("part_id", i+1).WithError(err).Warn("failed to upload part")
				return errors.WrapIff(err, "backup: failed to upload part %d", i+1)
			}

			parts[i] = remote.BackupPart{
				ETag:       etag,
				PartNumber: i + 1,
			}

			s.log().WithField("part_id", i+1).Info("successfully uploaded backup part")
			return nil
		})
	}

	if err := g.Wait(); err != nil {
		return nil, err
	}

	s.log().WithField("parts", len(urls.Parts)).Info("backup has been successfully uploaded")
	return parts, nil
}

type s3FileUploader struct {
	client *http.Client
}

// newS3FileUploader returns a new file uploader instance.
func newS3FileUploader(concurrency int) *s3FileUploader {
    t := http.DefaultTransport.(*http.Transport).Clone()
	// DefaultTransport keeps only 2 idle connections per host, so most of the
	// concurrent parts would pay for a fresh TCP + TLS handshake.
	t.MaxIdleConnsPerHost = concurrency
	
	return &s3FileUploader{
		// We purposefully use a super high timeout on this request since we need to upload
		// a 5GB file. This assumes at worst a 10Mbps connection for uploading. While technically
		// you could go slower we're targeting mostly hosted servers that should have 100Mbps
		// connections anyways.
		client: &http.Client{
			Timeout: time.Hour * 2,
			Transport: t,
		},
	}
}

// backoff returns a new exponential backoff implementation using a context that
// will also stop the backoff if it is canceled.
//
// The elapsed time tracked by the backoff includes the time spent inside the
// operation itself, and a single part upload easily runs for several minutes.
// Bounding on elapsed time would therefore stop the retries before the second
// attempt ever happened, so the number of attempts is bounded instead and the
// context carries the actual deadline.
func (fu *s3FileUploader) backoff(ctx context.Context) backoff.BackOffContext {
	b := backoff.NewExponentialBackOff()
	b.Multiplier = 2
	b.MaxElapsedTime = 0

	return backoff.WithContext(backoff.WithMaxRetries(b, 5), ctx)
}

// uploadPart attempts to upload a given S3 file part to the S3 system. If a
// 5xx error is returned from the endpoint this will continue with an exponential
// backoff to try and successfully upload the part.
//
// The section is rewound before every attempt, so a retry always sends the full
// part from its correct offset.
//
// Once uploaded the ETag is returned to the caller.
func (fu *s3FileUploader) uploadPart(ctx context.Context, part string, section *io.SectionReader, size int64) (string, error) {
	var etag string
	err := backoff.Retry(func() error {
		// Rewind the section so that a retry re-sends the whole part rather than
		// whatever is left over from the previous attempt.
		if _, err := section.Seek(0, io.SeekStart); err != nil {
			return backoff.Permanent(errors.Wrap(err, "backup: could not rewind part reader"))
		}

		// Build the request inside the retry: a request body can only be
		// consumed once, so it cannot be reused across attempts.
		r, err := http.NewRequestWithContext(ctx, http.MethodPut, part, section)
		if err != nil {
			return backoff.Permanent(errors.Wrap(err, "backup: could not create request for S3"))
		}
		r.ContentLength = size
		r.Header.Set("Content-Type", "application/x-gzip")

		res, err := fu.client.Do(r)
		if err != nil {
			if errors.Is(err, context.DeadlineExceeded) || errors.Is(err, context.Canceled) {
				return backoff.Permanent(err)
			}
			// Don't use a permanent error here, if there is a temporary resolution error with
			// the URL due to DNS issues we want to keep re-trying.
			return errors.Wrap(err, "backup: S3 HTTP request failed")
		}
		// Drain the body so the connection can go back to the idle pool. On an
        // error S3 returns XML that would otherwise leave the connection unusable.
        _, _ = io.Copy(io.Discard, res.Body)
		_ = res.Body.Close()

		if res.StatusCode != http.StatusOK {
			err := fmt.Errorf("backup: failed to put S3 object: [HTTP/%d] %s", res.StatusCode, res.Status)
			// Only attempt a backoff retry if this error is because of a 5xx error from
			// the S3 endpoint. Any 4xx error should be treated as an error that a retry
			// would not fix.
			//
			// 429 is the exception: it signals rate limiting rather than a client
			// error, and S3-compatible endpoints emit it when parts are uploaded
			// concurrently. Backing off and retrying is the correct response.
			if res.StatusCode >= http.StatusInternalServerError || res.StatusCode == http.StatusTooManyRequests {
				return err
			}
			return backoff.Permanent(err)
		}

		// Get the ETag from the uploaded part, this should be sent with the
		// CompleteMultipartUpload request.
		etag = res.Header.Get("ETag")

		return nil
	}, fu.backoff(ctx))

	if err != nil {
		var permanent *backoff.PermanentError
		if errors.As(err, &permanent) {
			return "", permanent.Unwrap()
		}
		return "", err
	}
	return etag, nil
}
