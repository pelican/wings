package backup

import (
	"archive/tar"
	"context"
	"crypto/sha1"
	"encoding/hex"
	"io"
	"io/fs"
	"os"
	"path"
	"strings"

	"emperror.dev/errors"
	"github.com/apex/log"
	"github.com/google/uuid"
	"github.com/mholt/archives"
	"golang.org/x/sync/errgroup"

	"github.com/pelican/wings/config"
	"github.com/pelican/wings/remote"
	"github.com/pelican/wings/server/filesystem"
)

var format = archives.CompressedArchive{
	Compression: archives.Gz{},
	Archival:    archives.Tar{},
	Extraction:  archives.Tar{},
}

type AdapterType string

const (
	LocalBackupAdapter AdapterType = "wings"
	S3BackupAdapter    AdapterType = "s3"
)

const (
	// PelicanConfigFile is the name of the server configuration stored as the
	// first entry of a backup archive.
	PelicanConfigFile = ".pelicanconfig"
	// MaxPelicanConfigSize is the largest server configuration that is stored, the
	// Panel does not read a larger one.
	MaxPelicanConfigSize = 64 * 1024
)

// IsPelicanConfigEntry reports whether an archive entry is the server
// configuration written by Wings, rather than a file from the server's directory.
func IsPelicanConfigEntry(info fs.FileInfo) bool {
	fi, ok := info.(archives.FileInfo)
	if !ok {
		return false
	}
	header, ok := fi.Header.(*tar.Header)
	return ok && filesystem.IsPreludeHeader(header, PelicanConfigFile)
}

// RestoreCallback is a generic restoration callback that exists for both local
// and remote backups allowing the files to be restored.
type RestoreCallback func(file string, info fs.FileInfo, r io.ReadCloser) error

// noinspection GoNameStartsWithPackageName
type BackupInterface interface {
	// SetClient sets the API request client on the backup interface.
	SetClient(remote.Client)
	// Identifier returns the UUID of this backup as tracked by the panel
	// instance.
	Identifier() string
	// ServerId returns the UUID of the server the backup is associated with
	ServerId() string
	// WithLogContext attaches additional context to the log output for this
	// backup.
	WithLogContext(map[string]interface{})
	// Generate creates a backup in whatever the configured source for the
	// specific implementation is.
	Generate(context.Context, *filesystem.Filesystem, string) (*ArchiveDetails, error)
	// Ignored returns the ignored files for this backup instance.
	Ignored() string
	// Checksum returns a SHA1 checksum for the generated backup.
	Checksum() ([]byte, error)
	// Size returns the size of the generated backup.
	Size() (int64, error)
	// Path returns the path to the backup on the machine. This is not always
	// the final storage location of the backup, simply the location we're using
	// to store it until it is moved to the final spot.
	Path() string
	// Details returns details about the archive.
	Details(context.Context, []remote.BackupPart) (*ArchiveDetails, error)
	// Remove removes a backup file.
	Remove() error
	// Restore is called when a backup is ready to be restored to the disk from
	// the given source. Not every backup implementation will support this nor
	// will every implementation require a reader be provided.
	Restore(context.Context, io.Reader, RestoreCallback) error
}

type Backup struct {
	// The UUID of this backup object. This must line up with a backup from
	// the panel instance.
	Uuid string `json:"uuid"`

	// The UUID of the server the backup is associated with.
	ServerUuid string `json:"server_uuid"`

	// An array of files to ignore when generating this backup. This should be
	// compatible with a standard .gitignore structure.
	Ignore string `json:"ignore"`

	// The server configuration sent by the Panel, stored as the first entry of
	// the archive so a new server can be created from the backup.
	pelicanConfig []byte

	client     remote.Client
	adapter    AdapterType
	logContext map[string]interface{}
}

func (b *Backup) SetClient(c remote.Client) {
	b.client = c
}

// SetPelicanConfig sets the server configuration stored as the first entry of the
// archive. Nothing is stored when it is empty.
func (b *Backup) SetPelicanConfig(config []byte) {
	b.pelicanConfig = config
}

// archiveOptions returns the options used to create this backup's archive.
func (b *Backup) archiveOptions(ignore string) []filesystem.ArchiveOption {
	opts := []filesystem.ArchiveOption{filesystem.WithIgnored(ignore)}
	if len(b.pelicanConfig) > 0 {
		opts = append(opts, filesystem.WithPrelude(PelicanConfigFile, b.pelicanConfig))
	}
	return opts
}

func (b *Backup) Identifier() string {
	return b.Uuid
}

func (b *Backup) normalizedIdentifier() (string, error) {
	parsed, err := uuid.Parse(b.Identifier())
	if err != nil || len(b.Identifier()) != len(parsed.String()) || parsed.String() != strings.ToLower(b.Identifier()) {
		return "", errors.New("backup: identifier must be a valid UUID")
	}
	return parsed.String(), nil
}

func (b *Backup) validateIdentifier() error {
	identifier, err := b.normalizedIdentifier()
	if err != nil {
		return err
	}
	// Only write when normalizing changes the identifier: Details validates it
	// from two goroutines at once.
	if b.Uuid != identifier {
		b.Uuid = identifier
	}
	return nil
}

func (b *Backup) ServerId() string { return b.ServerUuid }

// Path returns the path for this specific backup.
func (b *Backup) Path() string {
	identifier, err := b.normalizedIdentifier()
	if err != nil {
		identifier = path.Base(b.Identifier())
	}
	return path.Join(config.Get().System.BackupDirectory, b.ServerId(), identifier+".tar.gz")
}

// Size returns the size of the generated backup.
func (b *Backup) Size() (int64, error) {
	if err := b.validateIdentifier(); err != nil {
		return 0, err
	}
	st, err := os.Stat(b.Path())
	if err != nil {
		return 0, err
	}

	return st.Size(), nil
}

// Checksum returns the SHA256 checksum of a backup.
func (b *Backup) Checksum() ([]byte, error) {
	if err := b.validateIdentifier(); err != nil {
		return nil, err
	}
	h := sha1.New()

	f, err := os.Open(b.Path())
	if err != nil {
		return nil, err
	}
	defer f.Close()

	buf := make([]byte, 1024*4)
	if _, err := io.CopyBuffer(h, f, buf); err != nil {
		return nil, err
	}

	return h.Sum(nil), nil
}

// Details returns both the checksum and size of the archive currently stored on
// the disk to the caller.
func (b *Backup) Details(ctx context.Context, parts []remote.BackupPart) (*ArchiveDetails, error) {
	// Normalize the identifier before Checksum and Size read it concurrently.
	if err := b.validateIdentifier(); err != nil {
		return nil, err
	}
	ad := ArchiveDetails{ChecksumType: "sha1", Parts: parts}
	g, ctx := errgroup.WithContext(ctx)

	g.Go(func() error {
		resp, err := b.Checksum()
		if err != nil {
			return err
		}
		ad.Checksum = hex.EncodeToString(resp)
		return nil
	})

	g.Go(func() error {
		s, err := b.Size()
		if err != nil {
			return err
		}
		ad.Size = s
		return nil
	})

	if err := g.Wait(); err != nil {
		return nil, errors.WithStackDepth(err, 1)
	}
	return &ad, nil
}

func (b *Backup) Ignored() string {
	return b.Ignore
}

// Returns a logger instance for this backup with the additional context fields
// assigned to the output.
func (b *Backup) log() *log.Entry {
	l := log.WithField("backup", b.Identifier()).WithField("adapter", b.adapter)
	for k, v := range b.logContext {
		l = l.WithField(k, v)
	}
	return l
}

type ArchiveDetails struct {
	Checksum     string              `json:"checksum"`
	ChecksumType string              `json:"checksum_type"`
	Size         int64               `json:"size"`
	Parts        []remote.BackupPart `json:"parts"`
}

// ToRequest returns a request object.
func (ad *ArchiveDetails) ToRequest(successful bool) remote.BackupRequest {
	return remote.BackupRequest{
		Checksum:     ad.Checksum,
		ChecksumType: ad.ChecksumType,
		Size:         ad.Size,
		Successful:   successful,
		Parts:        ad.Parts,
	}
}
