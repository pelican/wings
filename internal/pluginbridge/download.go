package pluginbridge

import (
	"net/url"
	"path"
	"strings"
	"sync"

	"emperror.dev/errors"

	"github.com/pelican/wings/config"
	"github.com/pelican/wings/plugins/api"
	"github.com/pelican/wings/router/downloader"
	"github.com/pelican/wings/server"
)

// maxDownloadConcurrency bounds how many files one batch fetches at once.
//
// The point of a batch is to stop a caller issuing hundreds of sequential
// requests, not to let it open hundreds of sockets. Beyond a handful the
// bottleneck is the source and the disk anyway, and a modpack install should
// not be able to saturate a node's uplink against every other server on it.
const maxDownloadConcurrency = 8

// defaultDownloadConcurrency is used when a caller does not say.
const defaultDownloadConcurrency = 4

// FileDownload fetches a URL onto a server's disk.
//
// This goes through the same downloader the Panel's own remote-pull uses, so a
// plugin gets its redirect limit, its loop detection and its refusal to fetch
// from the local network, rather than a second implementation with its own
// mistakes.
func (b *Bridge) FileDownload(uuid string, req api.DownloadRequest) (api.DownloadResult, error) {
	s, err := b.find(uuid)
	if err != nil {
		return api.DownloadResult{}, err
	}

	result, err := b.download(s, req)
	return result, err
}

// FileDownloadMany fetches several URLs concurrently.
//
// A failure is reported against the request it belongs to rather than aborting
// the batch, because the obvious use is installing a long list of files where
// one dead link should not lose the rest.
func (b *Bridge) FileDownloadMany(uuid string, reqs []api.DownloadRequest, concurrency int) ([]api.DownloadResult, error) {
	s, err := b.find(uuid)
	if err != nil {
		return nil, err
	}

	if len(reqs) == 0 {
		return nil, nil
	}

	if concurrency <= 0 {
		concurrency = defaultDownloadConcurrency
	}
	if concurrency > maxDownloadConcurrency {
		concurrency = maxDownloadConcurrency
	}

	results := make([]api.DownloadResult, len(reqs))

	var wg sync.WaitGroup
	sem := make(chan struct{}, concurrency)

	for i, req := range reqs {
		wg.Add(1)

		go func(i int, req api.DownloadRequest) {
			defer wg.Done()

			sem <- struct{}{}
			defer func() { <-sem }()

			result, err := b.download(s, req)
			if err != nil {
				result.URL = req.URL
				result.Error = err.Error()
			}
			results[i] = result
		}(i, req)
	}

	wg.Wait()

	return results, nil
}

// download performs one download onto the server.
func (b *Bridge) download(s *server.Server, req api.DownloadRequest) (api.DownloadResult, error) {
	out := api.DownloadResult{URL: req.URL}

	// A node that has turned remote downloads off means it, whoever is asking.
	if config.Get().Api.DisableRemoteDownload {
		return out, errors.New("pluginbridge: remote downloads are disabled on this node")
	}

	parsed, err := url.Parse(req.URL)
	if err != nil {
		return out, errors.Wrapf(err, "pluginbridge: %q is not a usable url", req.URL)
	}
	if parsed.Scheme != "http" && parsed.Scheme != "https" {
		return out, errors.Errorf("pluginbridge: downloads must be http or https, not %q", parsed.Scheme)
	}

	// Refuse before fetching rather than after filling the disk.
	if err := s.Filesystem().HasSpaceErr(true); err != nil {
		return out, err
	}

	directory := req.Directory
	if directory == "" {
		directory = "/"
	}

	dl := downloader.New(s, downloader.DownloadRequest{
		Directory: directory,
		URL:       parsed,
		FileName:  req.FileName,
		UseHeader: req.UseHeader,
	})

	if err := dl.Execute(); err != nil {
		return out, errors.Wrapf(err, "pluginbridge: could not download %s", req.URL)
	}

	// Report where it landed and how big it is, which a caller otherwise has
	// to go and stat for itself.
	//
	// Path is already relative to the server root, so it is usable with the
	// rest of the file API as-is, and the size has to be read through the
	// server's filesystem rather than with os.Stat for the same reason.
	out.Path = dl.Path()

	if st, err := s.Filesystem().Stat(out.Path); err == nil {
		out.Bytes = st.Size()
	}

	return out, nil
}

// FileDecompress extracts an archive that is already on a server's disk.
//
// Wings extracts into the directory holding the archive, so the path is split
// into that directory and the file name rather than taking a destination the
// daemon has no way to honour.
func (b *Bridge) FileDecompress(uuid, archive string) error {
	s, err := b.find(uuid)
	if err != nil {
		return err
	}

	dir, file := path.Split(strings.TrimPrefix(archive, "/"))
	if file == "" {
		return errors.Errorf("pluginbridge: %q does not name an archive", archive)
	}
	if dir == "" {
		dir = "/"
	} else {
		dir = "/" + strings.TrimSuffix(dir, "/")
	}

	// Check the expanded size against the server's limit before writing
	// anything, so an archive cannot fill the disk on the way out.
	if err := s.Filesystem().SpaceAvailableForDecompression(s.Context(), dir, file); err != nil {
		return err
	}

	return s.Filesystem().DecompressFile(s.Context(), dir, file)
}
