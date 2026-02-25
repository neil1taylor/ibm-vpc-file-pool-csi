package replication

import (
	"archive/tar"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"k8s.io/klog/v2"
)

const (
	// DefaultMaxFileCount is the maximum number of files allowed in a single tar upload.
	DefaultMaxFileCount = 100000

	// DefaultMaxTotalBytes is the maximum total bytes allowed in a single tar upload (50 GB).
	DefaultMaxTotalBytes = 50 * 1024 * 1024 * 1024

	// DefaultMaxMetadataBytes is the maximum size of a metadata JSON upload (1 MB).
	DefaultMaxMetadataBytes = 1 * 1024 * 1024

	// metadataFileName is the sidecar metadata file written alongside replicated data.
	metadataFileName = ".subvolume-metadata.json"
)

// subVolumeNamePattern validates SubVolume names (pvc-<uuid> format).
var subVolumeNamePattern = regexp.MustCompile(`^pvc-[a-f0-9-]{36}$`)

// pathSegmentPattern validates basePath and subPath segments (no traversal).
var pathSegmentPattern = regexp.MustCompile(`^[a-zA-Z0-9._/-]+$`)

// Receiver is an HTTP server that accepts tar uploads and writes to destination NFS.
type Receiver struct {
	basePath      string
	authToken     string
	httpServer    *http.Server
	maxFileCount  int
	maxTotalBytes int64
	tlsCertFile   string
	tlsKeyFile    string
}

// NewReceiver creates a new replication receiver HTTP server.
func NewReceiver(basePath, authToken, listenAddr string) *Receiver {
	r := &Receiver{
		basePath:      basePath,
		authToken:     authToken,
		maxFileCount:  DefaultMaxFileCount,
		maxTotalBytes: DefaultMaxTotalBytes,
	}

	mux := http.NewServeMux()
	mux.HandleFunc("/api/v1/sync/", r.handleSync)
	mux.HandleFunc("/api/v1/metadata/", r.handleMetadata)
	mux.HandleFunc("/api/v1/health", r.handleHealth)

	r.httpServer = &http.Server{
		Addr:              listenAddr,
		Handler:           mux,
		ReadHeaderTimeout: 10 * time.Second,
		// No ReadTimeout — tar uploads can be very large.
		WriteTimeout: 0,
		IdleTimeout:  120 * time.Second,
	}

	return r
}

// SetMaxFileCount overrides the default max file count for tar uploads.
func (r *Receiver) SetMaxFileCount(n int) {
	r.maxFileCount = n
}

// SetMaxTotalBytes overrides the default max total bytes for tar uploads.
func (r *Receiver) SetMaxTotalBytes(n int64) {
	r.maxTotalBytes = n
}

// SetTLS configures the receiver to use TLS with the given certificate and key files.
func (r *Receiver) SetTLS(certFile, keyFile string) {
	r.tlsCertFile = certFile
	r.tlsKeyFile = keyFile
}

// Start starts the HTTP server. Blocks until the server stops.
func (r *Receiver) Start(ctx context.Context) error {
	tlsEnabled := r.tlsCertFile != "" && r.tlsKeyFile != ""
	klog.InfoS("Replication receiver starting", "addr", r.httpServer.Addr, "basePath", r.basePath, "tls", tlsEnabled)

	// Shut down gracefully when context is cancelled.
	go func() {
		<-ctx.Done()
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		if err := r.httpServer.Shutdown(shutdownCtx); err != nil {
			klog.ErrorS(err, "Receiver shutdown error")
		}
	}()

	var err error
	if tlsEnabled {
		err = r.httpServer.ListenAndServeTLS(r.tlsCertFile, r.tlsKeyFile)
	} else {
		err = r.httpServer.ListenAndServe()
	}
	if err != nil && err != http.ErrServerClosed {
		return fmt.Errorf("receiver listen: %w", err)
	}
	return nil
}

// Stop gracefully stops the HTTP server.
func (r *Receiver) Stop(ctx context.Context) error {
	return r.httpServer.Shutdown(ctx)
}

// handleHealth checks that the NFS base path is accessible.
func (r *Receiver) handleHealth(w http.ResponseWriter, req *http.Request) {
	if req.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	if _, err := os.Stat(r.basePath); err != nil {
		klog.ErrorS(err, "Health check failed: base path not accessible", "basePath", r.basePath)
		http.Error(w, "NFS mount not accessible", http.StatusServiceUnavailable)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	fmt.Fprintf(w, `{"status":"ok","basePath":%q}`, r.basePath)
}

// handleSync receives a tar stream and extracts it to the destination directory.
// PUT /api/v1/sync/{subvolumeName}?basePath=X&subPath=Y
func (r *Receiver) handleSync(w http.ResponseWriter, req *http.Request) {
	if req.Method != http.MethodPut {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	if !r.authenticate(req) {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}

	svName, err := extractSubVolumeName(req.URL.Path, "/api/v1/sync/")
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	destBasePath := req.URL.Query().Get("basePath")
	subPath := req.URL.Query().Get("subPath")

	if destBasePath == "" || subPath == "" {
		http.Error(w, "basePath and subPath query parameters are required", http.StatusBadRequest)
		return
	}

	destDir, err := r.resolveDestPath(destBasePath, subPath)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	klog.V(2).InfoS("Receiving sync upload",
		"subVolume", svName,
		"destDir", destDir,
	)

	// Create destination directory.
	if err := os.MkdirAll(destDir, 0755); err != nil {
		klog.ErrorS(err, "Failed to create destination directory", "destDir", destDir)
		http.Error(w, "failed to create destination directory", http.StatusInternalServerError)
		return
	}

	// Extract tar stream.
	filesExtracted, bytesWritten, extractErr := r.extractTar(req.Body, destDir)
	if extractErr != nil {
		klog.ErrorS(extractErr, "Tar extraction failed",
			"subVolume", svName,
			"destDir", destDir,
			"filesExtracted", filesExtracted,
			"bytesWritten", bytesWritten,
		)
		http.Error(w, fmt.Sprintf("tar extraction failed: %v", extractErr), http.StatusBadRequest)
		return
	}

	klog.V(2).InfoS("Sync upload complete",
		"subVolume", svName,
		"destDir", destDir,
		"filesExtracted", filesExtracted,
		"bytesWritten", bytesWritten,
	)

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	fmt.Fprintf(w, `{"status":"ok","filesExtracted":%d,"bytesWritten":%d}`, filesExtracted, bytesWritten)
}

// handleMetadata receives JSON metadata and writes it as a sidecar file.
// PUT /api/v1/metadata/{subvolumeName}?basePath=X&subPath=Y
func (r *Receiver) handleMetadata(w http.ResponseWriter, req *http.Request) {
	if req.Method != http.MethodPut {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	if !r.authenticate(req) {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}

	svName, err := extractSubVolumeName(req.URL.Path, "/api/v1/metadata/")
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	destBasePath := req.URL.Query().Get("basePath")
	subPath := req.URL.Query().Get("subPath")

	if destBasePath == "" || subPath == "" {
		http.Error(w, "basePath and subPath query parameters are required", http.StatusBadRequest)
		return
	}

	destDir, err := r.resolveDestPath(destBasePath, subPath)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	// Read metadata body (limited).
	data, err := io.ReadAll(io.LimitReader(req.Body, DefaultMaxMetadataBytes+1))
	if err != nil {
		http.Error(w, "failed to read metadata body", http.StatusBadRequest)
		return
	}
	if int64(len(data)) > DefaultMaxMetadataBytes {
		http.Error(w, "metadata body too large", http.StatusRequestEntityTooLarge)
		return
	}

	// Validate it's valid JSON.
	if !json.Valid(data) {
		http.Error(w, "metadata is not valid JSON", http.StatusBadRequest)
		return
	}

	// Ensure destination directory exists.
	if err := os.MkdirAll(destDir, 0755); err != nil {
		klog.ErrorS(err, "Failed to create destination directory for metadata", "destDir", destDir)
		http.Error(w, "failed to create destination directory", http.StatusInternalServerError)
		return
	}

	// Write metadata file.
	metaPath := filepath.Join(destDir, metadataFileName)
	if err := os.WriteFile(metaPath, data, 0644); err != nil {
		klog.ErrorS(err, "Failed to write metadata file", "path", metaPath)
		http.Error(w, "failed to write metadata", http.StatusInternalServerError)
		return
	}

	klog.V(2).InfoS("Metadata written",
		"subVolume", svName,
		"path", metaPath,
		"bytes", len(data),
	)

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	fmt.Fprintf(w, `{"status":"ok","path":%q}`, metaPath)
}

// authenticate checks the Bearer token in the Authorization header.
func (r *Receiver) authenticate(req *http.Request) bool {
	auth := req.Header.Get("Authorization")
	if auth == "" {
		return false
	}
	const prefix = "Bearer "
	if !strings.HasPrefix(auth, prefix) {
		return false
	}
	return strings.TrimSpace(auth[len(prefix):]) == r.authToken
}

// extractSubVolumeName extracts and validates the SubVolume name from the URL path.
func extractSubVolumeName(urlPath, prefix string) (string, error) {
	name := strings.TrimPrefix(urlPath, prefix)
	name = strings.TrimSuffix(name, "/")
	if name == "" {
		return "", fmt.Errorf("subvolume name is required in URL path")
	}
	if !subVolumeNamePattern.MatchString(name) {
		return "", fmt.Errorf("invalid subvolume name %q: must match pvc-<uuid>", name)
	}
	return name, nil
}

// resolveDestPath builds and validates the destination directory path.
func (r *Receiver) resolveDestPath(basePath, subPath string) (string, error) {
	// Validate path segments.
	if !pathSegmentPattern.MatchString(basePath) {
		return "", fmt.Errorf("invalid basePath %q", basePath)
	}
	if !pathSegmentPattern.MatchString(subPath) {
		return "", fmt.Errorf("invalid subPath %q", subPath)
	}

	// Check for traversal.
	if strings.Contains(basePath, "..") || strings.Contains(subPath, "..") {
		return "", fmt.Errorf("path traversal not allowed")
	}

	// Strip basePath prefix from subPath if present to prevent path doubling.
	// The sync-client sends subPath = sv.Spec.SubPath which already includes
	// the basePath prefix (e.g. "/pvcs/pvc-xxx"). Without this, we'd get
	// /data/pvcs/pvcs/pvc-xxx instead of /data/pvcs/pvc-xxx.
	trimmedBase := strings.TrimRight(basePath, "/")
	if strings.HasPrefix(subPath, trimmedBase+"/") {
		subPath = strings.TrimPrefix(subPath, trimmedBase)
	}

	destDir := filepath.Join(r.basePath, basePath, subPath)

	// Ensure it's under our base path.
	cleanDest := filepath.Clean(destDir)
	cleanBase := filepath.Clean(r.basePath)
	if !strings.HasPrefix(cleanDest, cleanBase+"/") && cleanDest != cleanBase {
		return "", fmt.Errorf("resolved path %q escapes base path %q", cleanDest, cleanBase)
	}

	return cleanDest, nil
}

// extractTar reads a tar stream and extracts files to destDir.
// Returns the number of files extracted, total bytes written, and any error.
func (r *Receiver) extractTar(reader io.Reader, destDir string) (int, int64, error) {
	tr := tar.NewReader(reader)
	fileCount := 0
	var totalBytes int64

	for {
		header, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return fileCount, totalBytes, fmt.Errorf("reading tar header: %w", err)
		}

		// Validate: no absolute paths.
		if filepath.IsAbs(header.Name) {
			return fileCount, totalBytes, fmt.Errorf("absolute path in tar: %s", header.Name)
		}

		// Validate: no traversal.
		cleanName := filepath.Clean(header.Name)
		if strings.HasPrefix(cleanName, "..") || strings.Contains(cleanName, "/..") {
			return fileCount, totalBytes, fmt.Errorf("path traversal in tar: %s", header.Name)
		}

		// Check file count limit.
		fileCount++
		if fileCount > r.maxFileCount {
			return fileCount, totalBytes, fmt.Errorf("too many files in tar (limit %d)", r.maxFileCount)
		}

		target := filepath.Join(destDir, cleanName)

		// Ensure target is under destDir.
		cleanTarget := filepath.Clean(target)
		if !strings.HasPrefix(cleanTarget, filepath.Clean(destDir)+string(filepath.Separator)) && cleanTarget != filepath.Clean(destDir) {
			return fileCount, totalBytes, fmt.Errorf("tar entry %q resolves outside dest dir", header.Name)
		}

		switch header.Typeflag {
		case tar.TypeDir:
			if err := os.MkdirAll(target, os.FileMode(header.Mode)|0700); err != nil {
				return fileCount, totalBytes, fmt.Errorf("creating dir %s: %w", target, err)
			}

		case tar.TypeReg:
			// Check total bytes limit before writing.
			if totalBytes+header.Size > r.maxTotalBytes {
				return fileCount, totalBytes, fmt.Errorf("total size exceeds limit (%d bytes)", r.maxTotalBytes)
			}

			// Ensure parent directory exists.
			if err := os.MkdirAll(filepath.Dir(target), 0755); err != nil {
				return fileCount, totalBytes, fmt.Errorf("creating parent dir for %s: %w", target, err)
			}

			written, writeErr := writeFile(target, tr, os.FileMode(header.Mode))
			totalBytes += written
			if writeErr != nil {
				return fileCount, totalBytes, fmt.Errorf("writing file %s: %w", target, writeErr)
			}

		case tar.TypeSymlink:
			// Validate symlink target doesn't escape destDir.
			linkTarget := header.Linkname
			if filepath.IsAbs(linkTarget) {
				return fileCount, totalBytes, fmt.Errorf("absolute symlink target in tar: %s -> %s", header.Name, linkTarget)
			}
			resolved := filepath.Clean(filepath.Join(filepath.Dir(target), linkTarget))
			if !strings.HasPrefix(resolved, filepath.Clean(destDir)) {
				return fileCount, totalBytes, fmt.Errorf("symlink %q -> %q escapes dest dir", header.Name, linkTarget)
			}

			// Ensure parent directory exists.
			if err := os.MkdirAll(filepath.Dir(target), 0755); err != nil {
				return fileCount, totalBytes, fmt.Errorf("creating parent dir for symlink %s: %w", target, err)
			}

			// Remove existing symlink if any.
			os.Remove(target)
			if err := os.Symlink(linkTarget, target); err != nil {
				return fileCount, totalBytes, fmt.Errorf("creating symlink %s: %w", target, err)
			}

		default:
			// Skip unsupported entry types (block devices, char devices, etc.).
			klog.V(4).InfoS("Skipping unsupported tar entry type",
				"name", header.Name,
				"typeflag", header.Typeflag,
			)
		}
	}

	return fileCount, totalBytes, nil
}

// writeFile writes data from reader to a file at path with the given mode.
func writeFile(path string, reader io.Reader, mode os.FileMode) (int64, error) {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, mode|0600)
	if err != nil {
		return 0, err
	}
	defer f.Close()

	written, err := io.Copy(f, reader)
	if err != nil {
		return written, err
	}
	return written, f.Close()
}
