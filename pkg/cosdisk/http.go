package cosdisk

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
)

// HTTPSource reads a file from the WebDAV server of the Kopia program
// (`kopia mount SNAPSHOT '*' --webdav`), which runs in the same pod. It only
// ever talks to 127.0.0.1: the address is checked, so a crafted name can never
// make the pod fetch from anywhere else.
type HTTPSource struct {
	client *http.Client
	url    string
	size   int64
}

// NewHTTPSource finds the size of the file with a HEAD request.
func NewHTTPSource(ctx context.Context, client *http.Client, rawURL string) (*HTTPSource, error) {
	u, err := url.Parse(rawURL)
	if err != nil {
		return nil, fmt.Errorf("cosdisk: bad URL: %w", err)
	}
	if u.Scheme != "http" || u.Hostname() != "127.0.0.1" || u.User != nil {
		return nil, fmt.Errorf("cosdisk: the disk must be on http://127.0.0.1, got %q", u.Redacted())
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodHead, rawURL, nil)
	if err != nil {
		return nil, err
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("cosdisk: asking for the size: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("cosdisk: asking for the size: %s", resp.Status)
	}
	if resp.ContentLength <= 0 {
		return nil, errors.New("cosdisk: the server did not give a size")
	}
	return &HTTPSource{client: client, url: rawURL, size: resp.ContentLength}, nil
}

// Size is the size of the file.
func (h *HTTPSource) Size() int64 { return h.size }

// ReadRange reads len(p) bytes at off. A reply that is not 206 is an error: a
// server that ignores the range would send the whole file for each read.
func (h *HTTPSource) ReadRange(ctx context.Context, p []byte, off int64) (int, error) {
	if len(p) == 0 {
		return 0, nil
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, h.url, nil)
	if err != nil {
		return 0, err
	}
	req.Header.Set("Range", "bytes="+strconv.FormatInt(off, 10)+"-"+strconv.FormatInt(off+int64(len(p))-1, 10))
	resp, err := h.client.Do(req)
	if err != nil {
		return 0, err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusPartialContent {
		return 0, fmt.Errorf("cosdisk: range read at %d: %s (want 206)", off, resp.Status)
	}
	return io.ReadFull(resp.Body, p)
}
