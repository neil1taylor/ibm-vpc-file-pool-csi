package replication

import (
	"archive/tar"
	"context"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"

	"k8s.io/klog/v2"
)

// SyncClient uploads SubVolume data to a replication receiver.
type SyncClient struct {
	endpoint   string
	authToken  string
	httpClient *http.Client
}

// NewSyncClient creates a new sync client for uploading to a receiver.
func NewSyncClient(endpoint, authToken string) *SyncClient {
	return &SyncClient{
		endpoint:   strings.TrimRight(endpoint, "/"),
		authToken:  authToken,
		httpClient: &http.Client{},
	}
}

// SetHTTPClient overrides the default HTTP client. Useful for tests.
func (c *SyncClient) SetHTTPClient(client *http.Client) {
	c.httpClient = client
}

// SyncSubVolume uploads the SubVolume directory as a tar stream and then
// uploads the metadata JSON to the receiver.
func (c *SyncClient) SyncSubVolume(ctx context.Context, sourcePath, destBasePath, subPath, svName string, metadata []byte) error {
	// 1. Upload tar stream.
	if err := c.uploadTar(ctx, sourcePath, subPath, destBasePath, svName); err != nil {
		return fmt.Errorf("upload tar: %w", err)
	}

	// 2. Upload metadata.
	if len(metadata) > 0 {
		if err := c.uploadMetadata(ctx, destBasePath, subPath, svName, metadata); err != nil {
			return fmt.Errorf("upload metadata: %w", err)
		}
	}

	return nil
}

// uploadTar creates a tar stream of the source directory and uploads it to the receiver.
func (c *SyncClient) uploadTar(ctx context.Context, sourcePath, subPath, destBasePath, svName string) error {
	sourceDir := filepath.Join(sourcePath, subPath)

	// Use a pipe so we stream the tar directly without buffering in memory.
	pr, pw := io.Pipe()

	// Create tar in a goroutine.
	go func() {
		tw := tar.NewWriter(pw)
		walkErr := filepath.Walk(sourceDir, func(path string, info os.FileInfo, err error) error {
			if err != nil {
				return err
			}

			// Make path relative to sourceDir.
			relPath, err := filepath.Rel(sourceDir, path)
			if err != nil {
				return fmt.Errorf("computing relative path: %w", err)
			}

			// Skip the root directory entry itself.
			if relPath == "." {
				return nil
			}

			header, err := tar.FileInfoHeader(info, "")
			if err != nil {
				return fmt.Errorf("creating tar header for %s: %w", relPath, err)
			}
			header.Name = relPath

			// Handle symlinks.
			if info.Mode()&os.ModeSymlink != 0 {
				linkTarget, err := os.Readlink(path)
				if err != nil {
					return fmt.Errorf("reading symlink %s: %w", path, err)
				}
				header.Linkname = linkTarget
				header.Typeflag = tar.TypeSymlink
			}

			if err := tw.WriteHeader(header); err != nil {
				return fmt.Errorf("writing tar header for %s: %w", relPath, err)
			}

			// Write file content for regular files.
			if info.Mode().IsRegular() {
				f, err := os.Open(path)
				if err != nil {
					return fmt.Errorf("opening file %s: %w", path, err)
				}
				defer f.Close()
				if _, err := io.Copy(tw, f); err != nil {
					return fmt.Errorf("copying file %s to tar: %w", relPath, err)
				}
			}

			return nil
		})

		closeErr := tw.Close()
		if walkErr != nil {
			pw.CloseWithError(walkErr)
		} else if closeErr != nil {
			pw.CloseWithError(closeErr)
		} else {
			pw.Close()
		}
	}()

	// Build URL.
	syncURL := fmt.Sprintf("%s/api/v1/sync/%s?basePath=%s&subPath=%s",
		c.endpoint,
		url.PathEscape(svName),
		url.QueryEscape(destBasePath),
		url.QueryEscape(subPath),
	)

	req, err := http.NewRequestWithContext(ctx, http.MethodPut, syncURL, pr)
	if err != nil {
		pr.Close()
		return fmt.Errorf("creating sync request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+c.authToken)
	req.Header.Set("Content-Type", "application/x-tar")

	klog.V(2).InfoS("Uploading tar stream to receiver",
		"subVolume", svName,
		"url", syncURL,
	)

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("sync request failed: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 1024))
		return fmt.Errorf("sync request returned %d: %s", resp.StatusCode, string(body))
	}

	klog.V(2).InfoS("Tar upload complete", "subVolume", svName)
	return nil
}

// uploadMetadata uploads metadata JSON to the receiver.
func (c *SyncClient) uploadMetadata(ctx context.Context, destBasePath, subPath, svName string, metadata []byte) error {
	metaURL := fmt.Sprintf("%s/api/v1/metadata/%s?basePath=%s&subPath=%s",
		c.endpoint,
		url.PathEscape(svName),
		url.QueryEscape(destBasePath),
		url.QueryEscape(subPath),
	)

	req, err := http.NewRequestWithContext(ctx, http.MethodPut, metaURL, strings.NewReader(string(metadata)))
	if err != nil {
		return fmt.Errorf("creating metadata request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+c.authToken)
	req.Header.Set("Content-Type", "application/json")

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("metadata request failed: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 1024))
		return fmt.Errorf("metadata request returned %d: %s", resp.StatusCode, string(body))
	}

	klog.V(2).InfoS("Metadata upload complete", "subVolume", svName)
	return nil
}
