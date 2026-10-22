package replication

import (
	"archive/tar"
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
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

	// dataDirMode is the mode for directories that hold replicated volume data.
	// Workloads run as non-root UIDs (KubeVirt uses 107) and must be able to
	// traverse them after failover, so they are world-readable.
	dataDirMode os.FileMode = 0o755

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

	// destLocks holds one *sync.Mutex per destination directory, so uploads
	// into the same subvolume run one at a time instead of interleaving.
	destLocks sync.Map

	// vmLocks holds one *sync.Mutex per VM disk: one call at a time.
	vmLocks sync.Map

	// maxVMDiskBytes is the largest VM disk accepted; 0 means the default.
	maxVMDiskBytes int64
	// cacheDropEvery is how many bytes of a file are stored or read before the
	// receiver drops its cached pages; 0 means the default. onCacheDrop runs
	// after each drop (tests only).
	cacheDropEvery int64
	onCacheDrop    func()
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
	mux.HandleFunc("/api/v1/vmcopy/", r.handleVMCopy)
	mux.HandleFunc("/api/v1/health", r.handleHealth)
	mux.HandleFunc("/api/v1/auth-check", r.handleAuthCheck)

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

// handleAuthCheck tells a client whether its token is accepted and the receiver's
// storage is reachable. It changes nothing. The console's receiver setup uses it
// (through the sync client's Check) before a policy is made.
func (r *Receiver) handleAuthCheck(w http.ResponseWriter, req *http.Request) {
	if req.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if !r.authenticate(req) {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	if _, err := os.Stat(r.basePath); err != nil {
		klog.ErrorS(err, "Auth check failed: base path not accessible", "basePath", r.basePath)
		http.Error(w, "NFS mount not accessible", http.StatusServiceUnavailable)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	writeResponse(w, `{"status":"ok","basePath":%q}`, r.basePath)
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
	writeResponse(w, `{"status":"ok","basePath":%q}`, r.basePath)
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
	if err == nil && filepath.Base(destDir) != svName {
		err = fmt.Errorf("the destination %q is not the folder of volume %s", destDir, svName)
	}
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	klog.V(2).InfoS("Receiving sync upload",
		"subVolume", svName,
		"destDir", destDir,
	)

	// Create destination directory.
	if err := os.MkdirAll(destDir, dataDirMode); err != nil {
		klog.ErrorS(err, "Failed to create destination directory", "destDir", destDir)
		http.Error(w, "failed to create destination directory", http.StatusInternalServerError)
		return
	}

	// Extract tar stream, one upload per destination at a time.
	lock := &sync.Mutex{}
	if existing, loaded := r.destLocks.LoadOrStore(destDir, lock); loaded {
		// destLocks only ever stores *sync.Mutex values.
		if m, ok := existing.(*sync.Mutex); ok {
			lock = m
		}
	}
	lock.Lock()
	filesExtracted, bytesWritten, extractErr := r.extractTar(req.Body, destDir)
	lock.Unlock()
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
	writeResponse(w, `{"status":"ok","filesExtracted":%d,"bytesWritten":%d}`, filesExtracted, bytesWritten)
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
	if err == nil && filepath.Base(destDir) != svName {
		err = fmt.Errorf("the destination %q is not the folder of volume %s", destDir, svName)
	}
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
	if err := os.MkdirAll(destDir, dataDirMode); err != nil {
		klog.ErrorS(err, "Failed to create destination directory for metadata", "destDir", destDir)
		http.Error(w, "failed to create destination directory", http.StatusInternalServerError)
		return
	}

	// Write metadata file.
	metaPath := filepath.Join(destDir, metadataFileName)
	// Volume names and sizes, read by the failover tool: same mode as writeMetadata.
	if err := os.WriteFile(metaPath, data, 0644); err != nil { //nolint:gosec // G306: see comment above
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
	writeResponse(w, `{"status":"ok","path":%q}`, metaPath)
}

// writeResponse writes a formatted response body. A write error means the
// client went away, so it is logged and not returned.
func writeResponse(w http.ResponseWriter, format string, args ...any) {
	if _, err := fmt.Fprintf(w, format, args...); err != nil {
		klog.V(2).InfoS("Failed to write response", "err", err)
	}
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
	// An empty configured token must never match an empty header token.
	if r.authToken == "" {
		return false
	}
	// Compare digests in constant time, so the time taken does not show how
	// much of a guessed token was right.
	got := sha256.Sum256([]byte(strings.TrimSpace(auth[len(prefix):])))
	want := sha256.Sum256([]byte(r.authToken))
	return subtle.ConstantTimeCompare(got[:], want[:]) == 1
}

// MinTokenLength is the shortest bearer token the receiver accepts. A token
// from `openssl rand -hex 32` has 64 characters.
const MinTokenLength = 32

// ValidateToken checks that a bearer token is long enough and has no space or
// control character.
func ValidateToken(token string) error {
	if len(token) < MinTokenLength {
		return fmt.Errorf("the token has %d characters; use at least %d, for example `openssl rand -hex 32`", len(token), MinTokenLength)
	}
	for _, c := range token {
		if c <= ' ' || c == 0x7f {
			return fmt.Errorf("the token contains a space or control character")
		}
	}
	return nil
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

	// The receiver writes volume folders only. A token holder must not be able
	// to write anywhere else under the base path, for example .snapshots.
	if !subVolumeNamePattern.MatchString(filepath.Base(cleanDest)) {
		return "", fmt.Errorf("the destination %q is not a pvc-<uuid> folder", cleanDest)
	}
	// No folder on the way may start with a dot (.snapshots and the like).
	if err := checkNoDotFolders(cleanBase, cleanDest); err != nil {
		return "", err
	}
	// No folder on the way may be a symlink to somewhere else. This runs on the
	// real path of the deepest folder that exists.
	if err := checkRealDest(cleanBase, cleanDest); err != nil {
		return "", err
	}

	return cleanDest, nil
}

// checkRealDest resolves the symlinks in dest (as far as dest exists) and
// checks that the real path is under the real base path and that no folder
// below the base starts with a dot.
func checkRealDest(base, dest string) error {
	realBase, err := filepath.EvalSymlinks(base)
	if errors.Is(err, fs.ErrNotExist) {
		return nil // nothing below a missing base can be a symlink
	}
	if err != nil {
		return fmt.Errorf("the receiver's base path is not usable: %w", err)
	}
	// Find the deepest part of dest that exists, and resolve it.
	existing, tail := dest, ""
	for {
		if _, statErr := os.Lstat(existing); statErr == nil {
			break
		}
		parent := filepath.Dir(existing)
		if parent == existing {
			return fmt.Errorf("no part of %q exists", dest)
		}
		tail = filepath.Join(filepath.Base(existing), tail)
		existing = parent
	}
	realExisting, err := filepath.EvalSymlinks(existing)
	if err != nil {
		return fmt.Errorf("resolving %q: %w", existing, err)
	}
	realDest := filepath.Join(realExisting, tail)
	if realDest != realBase && !strings.HasPrefix(realDest, realBase+"/") {
		return fmt.Errorf("the destination %q leads outside the base path through a symlink", dest)
	}
	return checkNoDotFolders(realBase, realDest)
}

// checkNoDotFolders refuses a destination that has a folder starting with a dot
// below the base path.
func checkNoDotFolders(base, dest string) error {
	rel := strings.TrimPrefix(strings.TrimPrefix(dest, base), "/")
	for _, part := range strings.Split(rel, "/") {
		if strings.HasPrefix(part, ".") {
			return fmt.Errorf("the destination %q has a folder that starts with a dot", dest)
		}
	}
	return nil
}

// extractTar reads a tar stream and extracts files to destDir.
// Returns the number of files extracted, total bytes written, and any error.
func (r *Receiver) extractTar(reader io.Reader, destDir string) (_ int, _ int64, retErr error) {
	// All writes go through an os.Root, which refuses any path that leaves
	// destDir, including a path that passes through a symlink.
	root, err := os.OpenRoot(destDir)
	if err != nil {
		return 0, 0, fmt.Errorf("opening dest dir: %w", err)
	}
	defer func() {
		if err := root.Close(); err != nil && retErr == nil {
			retErr = fmt.Errorf("closing dest dir: %w", err)
		}
	}()

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
		if !filepath.IsLocal(cleanName) {
			return fileCount, totalBytes, fmt.Errorf("path traversal in tar: %s", header.Name)
		}

		// Check file count limit.
		fileCount++
		if fileCount > r.maxFileCount {
			return fileCount, totalBytes, fmt.Errorf("too many files in tar (limit %d)", r.maxFileCount)
		}

		switch header.Typeflag {
		case tar.TypeDir:
			if err := root.MkdirAll(cleanName, tarMode(header.Mode)|0700); err != nil {
				return fileCount, totalBytes, fmt.Errorf("creating dir %s: %w", cleanName, err)
			}

		case tar.TypeReg:
			// Check total bytes limit before writing.
			if totalBytes+header.Size > r.maxTotalBytes {
				return fileCount, totalBytes, fmt.Errorf("total size exceeds limit (%d bytes)", r.maxTotalBytes)
			}

			// Ensure parent directory exists.
			if err := root.MkdirAll(filepath.Dir(cleanName), dataDirMode); err != nil {
				return fileCount, totalBytes, fmt.Errorf("creating parent dir for %s: %w", cleanName, err)
			}

			written, writeErr := writeFile(root, cleanName, tr, tarMode(header.Mode), r.cacheDropEvery, r.onCacheDrop)
			totalBytes += written
			if writeErr != nil {
				return fileCount, totalBytes, fmt.Errorf("writing file %s: %w", cleanName, writeErr)
			}

		case tar.TypeSymlink:
			linkTarget := header.Linkname
			if filepath.IsAbs(linkTarget) {
				return fileCount, totalBytes, fmt.Errorf("absolute symlink target in tar: %s -> %s", header.Name, linkTarget)
			}

			// Ensure parent directory exists.
			parent := filepath.Dir(cleanName)
			if err := root.MkdirAll(parent, dataDirMode); err != nil {
				return fileCount, totalBytes, fmt.Errorf("creating parent dir for symlink %s: %w", cleanName, err)
			}

			// Validate the link target against the parent's real location, so a
			// symlinked parent cannot make an escaping target look local.
			escapes, err := symlinkEscapes(destDir, parent, linkTarget)
			if err != nil {
				return fileCount, totalBytes, fmt.Errorf("checking symlink %s: %w", cleanName, err)
			}
			if escapes {
				return fileCount, totalBytes, fmt.Errorf("symlink %q -> %q escapes dest dir", header.Name, linkTarget)
			}

			// Replace an existing entry, if any.
			if err := root.Remove(cleanName); err != nil && !errors.Is(err, fs.ErrNotExist) {
				return fileCount, totalBytes, fmt.Errorf("removing existing %s: %w", cleanName, err)
			}
			if err := root.Symlink(linkTarget, cleanName); err != nil {
				return fileCount, totalBytes, fmt.Errorf("creating symlink %s: %w", cleanName, err)
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

// symlinkEscapes reports whether a symlink at parent/<name> with the given
// relative target would point outside destDir. parent is relative to destDir
// and must already exist; symlinks in destDir and parent are resolved first.
func symlinkEscapes(destDir, parent, linkTarget string) (bool, error) {
	realDest, err := filepath.EvalSymlinks(destDir)
	if err != nil {
		return false, err
	}
	realParent, err := filepath.EvalSymlinks(filepath.Join(destDir, parent))
	if err != nil {
		return false, err
	}
	rel, err := filepath.Rel(realDest, filepath.Join(realParent, linkTarget))
	if err != nil {
		return true, nil
	}
	return !filepath.IsLocal(rel) && rel != ".", nil
}

// tarMode returns the permission bits of a tar header mode. Setuid, setgid and
// sticky bits are dropped.
func tarMode(mode int64) os.FileMode {
	return os.FileMode(mode & 0o777) //nolint:gosec // G115: masked to 9 bits, cannot overflow
}

// writeFile writes data from reader to the file name inside root with the given mode.
func writeFile(root *os.Root, name string, reader io.Reader, mode os.FileMode, syncEvery int64, onSync func()) (_ int64, retErr error) {
	f, err := root.OpenFile(name, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, mode|0600)
	if err != nil {
		return 0, err
	}
	defer func() {
		if err := f.Close(); err != nil && retErr == nil {
			retErr = err
		}
	}()

	// Sync while writing, so dirty pages of a large file do not fill the
	// container's memory (issue #225).
	bound := newDirtyBound(f, syncEvery)
	bound.onSync = onSync
	n, err := io.Copy(&boundedWriter{w: f, bound: bound}, reader)
	if err == nil {
		err = bound.flush()
	}
	return n, err
}
