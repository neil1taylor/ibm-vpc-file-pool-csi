package cosdisk

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"time"
)

var zeroTime time.Time

// RemoteSource reads a disk from a disk server over HTTPS. The client must pin
// the session certificate (browse.PinnedClient); the token goes in every
// request. The guest helper uses it to read the disk it cannot hold itself.
type RemoteSource struct {
	client *http.Client
	url    string
	token  string
	size   int64
	moment time.Time
}

// MomentHeader is the response header with the moment of the disk (RFC 3339),
// which the disk server adds to its answers.
const MomentHeader = "X-Disk-Moment"

// NewRemoteSource finds the size of the disk with a HEAD request, which also
// checks the token.
func NewRemoteSource(ctx context.Context, client *http.Client, rawURL, token string) (*RemoteSource, error) {
	u, err := url.Parse(rawURL)
	if err != nil {
		return nil, fmt.Errorf("cosdisk: bad URL: %w", err)
	}
	if u.Scheme != "https" || u.User != nil || u.Host == "" {
		return nil, errors.New("cosdisk: the disk server must be on https")
	}
	r := &RemoteSource{client: client, url: rawURL, token: token}
	req, err := r.request(ctx, http.MethodHead, "")
	if err != nil {
		return nil, err
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("cosdisk: asking the disk server for the size: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("cosdisk: asking the disk server for the size: %s", resp.Status)
	}
	if resp.ContentLength <= 0 {
		return nil, errors.New("cosdisk: the disk server did not give a size")
	}
	r.size = resp.ContentLength
	if t, perr := time.Parse(time.RFC3339, resp.Header.Get(MomentHeader)); perr == nil {
		r.moment = t
	}
	return r, nil
}

func (r *RemoteSource) request(ctx context.Context, method, rng string) (*http.Request, error) {
	req, err := http.NewRequestWithContext(ctx, method, r.url, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+r.token)
	if rng != "" {
		req.Header.Set("Range", rng)
	}
	return req, nil
}

// Moment is the moment of the disk, or the zero time if the server did not say.
func (r *RemoteSource) Moment() time.Time { return r.moment }

// Size is the size of the disk.
func (r *RemoteSource) Size() int64 { return r.size }

// ReadRange fills p with the bytes at off. A reply that is not 206 is an error.
func (r *RemoteSource) ReadRange(ctx context.Context, p []byte, off int64) (int, error) {
	if len(p) == 0 {
		return 0, nil
	}
	req, err := r.request(ctx, http.MethodGet, "bytes="+strconv.FormatInt(off, 10)+"-"+strconv.FormatInt(off+int64(len(p))-1, 10))
	if err != nil {
		return 0, err
	}
	resp, err := r.client.Do(req)
	if err != nil {
		return 0, err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusPartialContent {
		return 0, fmt.Errorf("cosdisk: range read at %d: %s (want 206)", off, resp.Status)
	}
	return io.ReadFull(resp.Body, p)
}

// FetchFileList fetches the file list from the disk server. It returns nil and
// no error when the export has none. A list that says it is larger than max is
// refused, and the reader stops at max. The caller closes the result.
func FetchFileList(ctx context.Context, client *http.Client, rawURL, token string, max int64) (io.ReadCloser, error) {
	u, err := url.Parse(rawURL)
	if err != nil || u.Scheme != "https" || u.User != nil || u.Host == "" {
		return nil, errors.New("cosdisk: the disk server must be on https")
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("cosdisk: fetching the file list: %w", err)
	}
	switch {
	case resp.StatusCode == http.StatusNotFound:
		_ = resp.Body.Close()
		return nil, nil
	case resp.StatusCode != http.StatusOK:
		_ = resp.Body.Close()
		return nil, fmt.Errorf("cosdisk: fetching the file list: %s", resp.Status)
	case resp.ContentLength < 0 || resp.ContentLength > max:
		_ = resp.Body.Close()
		return nil, errors.New("cosdisk: the file list is larger than the limit")
	}
	return &limitedBody{Reader: io.LimitReader(resp.Body, max), Closer: resp.Body}, nil
}

type limitedBody struct {
	io.Reader
	io.Closer
}
