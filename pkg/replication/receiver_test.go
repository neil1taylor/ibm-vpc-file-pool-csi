package replication

import (
	"archive/tar"
	"bytes"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/json"
	"encoding/pem"
	"io"
	"math/big"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

func createTestReceiver(t *testing.T) (*Receiver, string) {
	t.Helper()
	basePath := t.TempDir()
	r := NewReceiver(basePath, "test-token-123", ":0")
	return r, basePath
}

func createTarStream(t *testing.T, files map[string]string) *bytes.Buffer {
	t.Helper()
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)

	for name, content := range files {
		hdr := &tar.Header{
			Name: name,
			Mode: 0644,
			Size: int64(len(content)),
		}
		if err := tw.WriteHeader(hdr); err != nil {
			t.Fatalf("writing tar header for %s: %v", name, err)
		}
		if _, err := tw.Write([]byte(content)); err != nil {
			t.Fatalf("writing tar content for %s: %v", name, err)
		}
	}

	if err := tw.Close(); err != nil {
		t.Fatalf("closing tar writer: %v", err)
	}
	return &buf
}

func createTarStreamWithDir(t *testing.T, dirs []string, files map[string]string) *bytes.Buffer {
	t.Helper()
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)

	for _, dir := range dirs {
		hdr := &tar.Header{
			Name:     dir + "/",
			Mode:     0755,
			Typeflag: tar.TypeDir,
		}
		if err := tw.WriteHeader(hdr); err != nil {
			t.Fatalf("writing tar dir header for %s: %v", dir, err)
		}
	}

	for name, content := range files {
		hdr := &tar.Header{
			Name: name,
			Mode: 0644,
			Size: int64(len(content)),
		}
		if err := tw.WriteHeader(hdr); err != nil {
			t.Fatalf("writing tar header for %s: %v", name, err)
		}
		if _, err := tw.Write([]byte(content)); err != nil {
			t.Fatalf("writing tar content for %s: %v", name, err)
		}
	}

	if err := tw.Close(); err != nil {
		t.Fatalf("closing tar writer: %v", err)
	}
	return &buf
}

func TestReceiver_Sync_ValidTar(t *testing.T) {
	r, basePath := createTestReceiver(t)

	tarData := createTarStream(t, map[string]string{
		"file1.txt":        "hello world",
		"subdir/file2.txt": "nested content",
	})

	req := httptest.NewRequest(http.MethodPut,
		"/api/v1/sync/pvc-12345678-1234-1234-1234-123456789abc?basePath=/pvcs&subPath=pvc-12345678-1234-1234-1234-123456789abc",
		tarData)
	req.Header.Set("Authorization", "Bearer test-token-123")
	w := httptest.NewRecorder()

	r.handleSync(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}

	// Verify files were extracted.
	destDir := filepath.Join(basePath, "pvcs", "pvc-12345678-1234-1234-1234-123456789abc")
	content, err := os.ReadFile(filepath.Join(destDir, "file1.txt"))
	if err != nil {
		t.Fatalf("reading extracted file1.txt: %v", err)
	}
	if string(content) != "hello world" {
		t.Errorf("file1.txt content = %q, want %q", string(content), "hello world")
	}

	content, err = os.ReadFile(filepath.Join(destDir, "subdir", "file2.txt"))
	if err != nil {
		t.Fatalf("reading extracted subdir/file2.txt: %v", err)
	}
	if string(content) != "nested content" {
		t.Errorf("subdir/file2.txt content = %q, want %q", string(content), "nested content")
	}
}

func TestReceiver_Sync_InvalidAuth(t *testing.T) {
	r, _ := createTestReceiver(t)

	tarData := createTarStream(t, map[string]string{"file.txt": "data"})

	req := httptest.NewRequest(http.MethodPut,
		"/api/v1/sync/pvc-12345678-1234-1234-1234-123456789abc?basePath=/pvcs&subPath=pvc-12345678-1234-1234-1234-123456789abc",
		tarData)
	req.Header.Set("Authorization", "Bearer wrong-token")
	w := httptest.NewRecorder()

	r.handleSync(w, req)

	if w.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401, got %d: %s", w.Code, w.Body.String())
	}
}

func TestReceiver_Sync_MissingAuth(t *testing.T) {
	r, _ := createTestReceiver(t)

	tarData := createTarStream(t, map[string]string{"file.txt": "data"})

	req := httptest.NewRequest(http.MethodPut,
		"/api/v1/sync/pvc-12345678-1234-1234-1234-123456789abc?basePath=/pvcs&subPath=pvc-12345678-1234-1234-1234-123456789abc",
		tarData)
	w := httptest.NewRecorder()

	r.handleSync(w, req)

	if w.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401, got %d: %s", w.Code, w.Body.String())
	}
}

func TestReceiver_Sync_MissingSubVolumeName(t *testing.T) {
	r, _ := createTestReceiver(t)

	req := httptest.NewRequest(http.MethodPut,
		"/api/v1/sync/?basePath=/pvcs&subPath=sub1",
		nil)
	req.Header.Set("Authorization", "Bearer test-token-123")
	w := httptest.NewRecorder()

	r.handleSync(w, req)

	if w.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d: %s", w.Code, w.Body.String())
	}
}

func TestReceiver_Sync_InvalidSubVolumeName(t *testing.T) {
	r, _ := createTestReceiver(t)

	req := httptest.NewRequest(http.MethodPut,
		"/api/v1/sync/invalid-name?basePath=/pvcs&subPath=sub1",
		nil)
	req.Header.Set("Authorization", "Bearer test-token-123")
	w := httptest.NewRecorder()

	r.handleSync(w, req)

	if w.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d: %s", w.Code, w.Body.String())
	}
}

func TestReceiver_Sync_PathTraversalInTar(t *testing.T) {
	r, _ := createTestReceiver(t)

	// Create a tar with a path traversal entry.
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	hdr := &tar.Header{
		Name: "../../../etc/passwd",
		Mode: 0644,
		Size: 4,
	}
	tw.WriteHeader(hdr)
	tw.Write([]byte("evil"))
	tw.Close()

	req := httptest.NewRequest(http.MethodPut,
		"/api/v1/sync/pvc-12345678-1234-1234-1234-123456789abc?basePath=/pvcs&subPath=pvc-12345678-1234-1234-1234-123456789abc",
		&buf)
	req.Header.Set("Authorization", "Bearer test-token-123")
	w := httptest.NewRecorder()

	r.handleSync(w, req)

	if w.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 for path traversal, got %d: %s", w.Code, w.Body.String())
	}
}

func TestReceiver_Sync_AbsolutePathInTar(t *testing.T) {
	r, _ := createTestReceiver(t)

	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	hdr := &tar.Header{
		Name: "/etc/passwd",
		Mode: 0644,
		Size: 4,
	}
	tw.WriteHeader(hdr)
	tw.Write([]byte("evil"))
	tw.Close()

	req := httptest.NewRequest(http.MethodPut,
		"/api/v1/sync/pvc-12345678-1234-1234-1234-123456789abc?basePath=/pvcs&subPath=pvc-12345678-1234-1234-1234-123456789abc",
		&buf)
	req.Header.Set("Authorization", "Bearer test-token-123")
	w := httptest.NewRecorder()

	r.handleSync(w, req)

	if w.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 for absolute path, got %d: %s", w.Code, w.Body.String())
	}
}

func TestReceiver_Sync_MissingQueryParams(t *testing.T) {
	r, _ := createTestReceiver(t)

	req := httptest.NewRequest(http.MethodPut,
		"/api/v1/sync/pvc-12345678-1234-1234-1234-123456789abc",
		nil)
	req.Header.Set("Authorization", "Bearer test-token-123")
	w := httptest.NewRecorder()

	r.handleSync(w, req)

	if w.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d: %s", w.Code, w.Body.String())
	}
}

func TestReceiver_Sync_ConcurrentUploads(t *testing.T) {
	r, basePath := createTestReceiver(t)

	var wg sync.WaitGroup
	for i := 0; i < 5; i++ {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			fileName := "file.txt"
			content := "content from goroutine"
			tarData := createTarStream(t, map[string]string{fileName: content})

			// Use different subPaths for each goroutine.
			svName := "pvc-12345678-1234-1234-1234-123456789abc"
			subPath := svName

			req := httptest.NewRequest(http.MethodPut,
				"/api/v1/sync/"+svName+"?basePath=/pvcs&subPath="+subPath,
				tarData)
			req.Header.Set("Authorization", "Bearer test-token-123")
			w := httptest.NewRecorder()

			r.handleSync(w, req)

			if w.Code != http.StatusOK {
				t.Errorf("goroutine %d: expected 200, got %d: %s", idx, w.Code, w.Body.String())
			}
		}(i)
	}
	wg.Wait()

	// Verify the destination directory exists.
	destDir := filepath.Join(basePath, "pvcs", "pvc-12345678-1234-1234-1234-123456789abc")
	if _, err := os.Stat(destDir); err != nil {
		t.Fatalf("destination directory doesn't exist after concurrent uploads: %v", err)
	}
}

func TestReceiver_Sync_WrongMethod(t *testing.T) {
	r, _ := createTestReceiver(t)

	req := httptest.NewRequest(http.MethodGet,
		"/api/v1/sync/pvc-12345678-1234-1234-1234-123456789abc?basePath=/pvcs&subPath=sub1",
		nil)
	req.Header.Set("Authorization", "Bearer test-token-123")
	w := httptest.NewRecorder()

	r.handleSync(w, req)

	if w.Code != http.StatusMethodNotAllowed {
		t.Fatalf("expected 405, got %d", w.Code)
	}
}

func TestReceiver_Metadata_Write(t *testing.T) {
	r, basePath := createTestReceiver(t)

	meta := map[string]interface{}{
		"subVolumeName":     "pvc-12345678-1234-1234-1234-123456789abc",
		"replicationPolicy": "test-policy",
	}
	metaJSON, _ := json.Marshal(meta)

	req := httptest.NewRequest(http.MethodPut,
		"/api/v1/metadata/pvc-12345678-1234-1234-1234-123456789abc?basePath=/pvcs&subPath=pvc-12345678-1234-1234-1234-123456789abc",
		bytes.NewReader(metaJSON))
	req.Header.Set("Authorization", "Bearer test-token-123")
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()

	r.handleMetadata(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}

	// Verify metadata file was written.
	metaPath := filepath.Join(basePath, "pvcs", "pvc-12345678-1234-1234-1234-123456789abc", metadataFileName)
	data, err := os.ReadFile(metaPath)
	if err != nil {
		t.Fatalf("reading metadata file: %v", err)
	}

	var readBack map[string]interface{}
	if err := json.Unmarshal(data, &readBack); err != nil {
		t.Fatalf("unmarshaling metadata: %v", err)
	}
	if readBack["subVolumeName"] != "pvc-12345678-1234-1234-1234-123456789abc" {
		t.Errorf("metadata subVolumeName = %v, want pvc-12345678-1234-1234-1234-123456789abc", readBack["subVolumeName"])
	}
}

func TestReceiver_Metadata_InvalidAuth(t *testing.T) {
	r, _ := createTestReceiver(t)

	req := httptest.NewRequest(http.MethodPut,
		"/api/v1/metadata/pvc-12345678-1234-1234-1234-123456789abc?basePath=/pvcs&subPath=sub1",
		strings.NewReader(`{"test":true}`))
	req.Header.Set("Authorization", "Bearer wrong")
	w := httptest.NewRecorder()

	r.handleMetadata(w, req)

	if w.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401, got %d", w.Code)
	}
}

func TestReceiver_Metadata_InvalidJSON(t *testing.T) {
	r, _ := createTestReceiver(t)

	req := httptest.NewRequest(http.MethodPut,
		"/api/v1/metadata/pvc-12345678-1234-1234-1234-123456789abc?basePath=/pvcs&subPath=pvc-12345678-1234-1234-1234-123456789abc",
		strings.NewReader("not json"))
	req.Header.Set("Authorization", "Bearer test-token-123")
	w := httptest.NewRecorder()

	r.handleMetadata(w, req)

	if w.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 for invalid JSON, got %d: %s", w.Code, w.Body.String())
	}
}

func TestReceiver_Health_OK(t *testing.T) {
	r, _ := createTestReceiver(t)

	req := httptest.NewRequest(http.MethodGet, "/api/v1/health", nil)
	w := httptest.NewRecorder()

	r.handleHealth(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}

	if !strings.Contains(w.Body.String(), `"status":"ok"`) {
		t.Errorf("health response doesn't contain ok status: %s", w.Body.String())
	}
}

func TestReceiver_Health_BadMount(t *testing.T) {
	r := NewReceiver("/nonexistent/path/that/doesnt/exist", "token", ":0")

	req := httptest.NewRequest(http.MethodGet, "/api/v1/health", nil)
	w := httptest.NewRecorder()

	r.handleHealth(w, req)

	if w.Code != http.StatusServiceUnavailable {
		t.Fatalf("expected 503 for bad mount, got %d: %s", w.Code, w.Body.String())
	}
}

func TestReceiver_Sync_TooManyFiles(t *testing.T) {
	r, _ := createTestReceiver(t)
	r.SetMaxFileCount(2)

	tarData := createTarStream(t, map[string]string{
		"file1.txt": "a",
		"file2.txt": "b",
		"file3.txt": "c",
	})

	req := httptest.NewRequest(http.MethodPut,
		"/api/v1/sync/pvc-12345678-1234-1234-1234-123456789abc?basePath=/pvcs&subPath=pvc-12345678-1234-1234-1234-123456789abc",
		tarData)
	req.Header.Set("Authorization", "Bearer test-token-123")
	w := httptest.NewRecorder()

	r.handleSync(w, req)

	if w.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 for too many files, got %d: %s", w.Code, w.Body.String())
	}
}

func TestReceiver_Sync_PathTraversalInQuery(t *testing.T) {
	r, _ := createTestReceiver(t)

	req := httptest.NewRequest(http.MethodPut,
		"/api/v1/sync/pvc-12345678-1234-1234-1234-123456789abc?basePath=../../etc&subPath=passwd",
		nil)
	req.Header.Set("Authorization", "Bearer test-token-123")
	w := httptest.NewRecorder()

	r.handleSync(w, req)

	if w.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 for path traversal in query, got %d: %s", w.Code, w.Body.String())
	}
}

func TestReceiver_Sync_WithDirectories(t *testing.T) {
	r, basePath := createTestReceiver(t)

	tarData := createTarStreamWithDir(t,
		[]string{"subdir"},
		map[string]string{
			"subdir/file.txt": "content in subdir",
		})

	req := httptest.NewRequest(http.MethodPut,
		"/api/v1/sync/pvc-12345678-1234-1234-1234-123456789abc?basePath=/pvcs&subPath=pvc-12345678-1234-1234-1234-123456789abc",
		tarData)
	req.Header.Set("Authorization", "Bearer test-token-123")
	w := httptest.NewRecorder()

	r.handleSync(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}

	destFile := filepath.Join(basePath, "pvcs", "pvc-12345678-1234-1234-1234-123456789abc", "subdir", "file.txt")
	content, err := os.ReadFile(destFile)
	if err != nil {
		t.Fatalf("reading extracted file: %v", err)
	}
	if string(content) != "content in subdir" {
		t.Errorf("content = %q, want %q", string(content), "content in subdir")
	}
}

func TestExtractSubVolumeName(t *testing.T) {
	tests := []struct {
		path    string
		prefix  string
		want    string
		wantErr bool
	}{
		{"/api/v1/sync/pvc-12345678-1234-1234-1234-123456789abc", "/api/v1/sync/", "pvc-12345678-1234-1234-1234-123456789abc", false},
		{"/api/v1/sync/", "/api/v1/sync/", "", true},
		{"/api/v1/sync/invalid", "/api/v1/sync/", "", true},
		{"/api/v1/sync/pvc-12345678-1234-1234-1234-123456789abc/", "/api/v1/sync/", "pvc-12345678-1234-1234-1234-123456789abc", false},
	}

	for _, tt := range tests {
		got, err := extractSubVolumeName(tt.path, tt.prefix)
		if (err != nil) != tt.wantErr {
			t.Errorf("extractSubVolumeName(%q, %q) error = %v, wantErr %v", tt.path, tt.prefix, err, tt.wantErr)
			continue
		}
		if got != tt.want {
			t.Errorf("extractSubVolumeName(%q, %q) = %q, want %q", tt.path, tt.prefix, got, tt.want)
		}
	}
}

func TestResolveDestPath(t *testing.T) {
	basePath := "/data"
	r := &Receiver{basePath: basePath}

	tests := []struct {
		base    string
		sub     string
		wantErr bool
	}{
		{"/pvcs", "pvc-abc", false},
		{"../../etc", "passwd", true},
		{"/pvcs", "../../../etc/passwd", true},
		{"/pvcs", "valid-path", false},
	}

	for _, tt := range tests {
		_, err := r.resolveDestPath(tt.base, tt.sub)
		if (err != nil) != tt.wantErr {
			t.Errorf("resolveDestPath(%q, %q) error = %v, wantErr %v", tt.base, tt.sub, err, tt.wantErr)
		}
	}
}

// TestResolveDestPath_PathDoubling verifies that subPath with basePath prefix
// doesn't result in doubled path segments (Fix 1: /data/pvcs/pvcs/pvc-xxx).
func TestResolveDestPath_PathDoubling(t *testing.T) {
	r := &Receiver{basePath: "/data"}

	tests := []struct {
		name     string
		base     string
		sub      string
		wantPath string
	}{
		{
			name:     "subPath already includes basePath prefix",
			base:     "/pvcs",
			sub:      "/pvcs/pvc-12345678-1234-1234-1234-123456789abc",
			wantPath: "/data/pvcs/pvc-12345678-1234-1234-1234-123456789abc",
		},
		{
			name:     "subPath without basePath prefix (no doubling)",
			base:     "/pvcs",
			sub:      "pvc-12345678-1234-1234-1234-123456789abc",
			wantPath: "/data/pvcs/pvc-12345678-1234-1234-1234-123456789abc",
		},
		{
			name:     "basePath with trailing slash",
			base:     "/pvcs/",
			sub:      "/pvcs/pvc-12345678-1234-1234-1234-123456789abc",
			wantPath: "/data/pvcs/pvc-12345678-1234-1234-1234-123456789abc",
		},
		{
			name:     "different basePath and subPath",
			base:     "/volumes",
			sub:      "/pvcs/pvc-12345678-1234-1234-1234-123456789abc",
			wantPath: "/data/volumes/pvcs/pvc-12345678-1234-1234-1234-123456789abc",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := r.resolveDestPath(tt.base, tt.sub)
			if err != nil {
				t.Fatalf("resolveDestPath(%q, %q) unexpected error: %v", tt.base, tt.sub, err)
			}
			if got != tt.wantPath {
				t.Errorf("resolveDestPath(%q, %q) = %q, want %q", tt.base, tt.sub, got, tt.wantPath)
			}
		})
	}
}

// TestReceiver_Sync_PathDoubling_E2E verifies data lands at /pvcs/pvc-xxx
// (not /pvcs/pvcs/pvc-xxx) when subPath already includes the basePath prefix.
func TestReceiver_Sync_PathDoubling_E2E(t *testing.T) {
	r, basePath := createTestReceiver(t)

	tarData := createTarStream(t, map[string]string{
		"disk.img": "fake disk image content",
	})

	// Simulate what the sync-client sends: subPath = sv.Spec.SubPath = "/pvcs/pvc-xxx"
	svName := "pvc-12345678-1234-1234-1234-123456789abc"
	req := httptest.NewRequest(http.MethodPut,
		"/api/v1/sync/"+svName+"?basePath=/pvcs&subPath=/pvcs/"+svName,
		tarData)
	req.Header.Set("Authorization", "Bearer test-token-123")
	w := httptest.NewRecorder()

	r.handleSync(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}

	// Verify data is at /pvcs/pvc-xxx (NOT /pvcs/pvcs/pvc-xxx).
	correctPath := filepath.Join(basePath, "pvcs", svName, "disk.img")
	if _, err := os.Stat(correctPath); err != nil {
		t.Errorf("expected file at %s: %v", correctPath, err)
	}

	// Verify the doubled path does NOT exist.
	doubledPath := filepath.Join(basePath, "pvcs", "pvcs", svName, "disk.img")
	if _, err := os.Stat(doubledPath); err == nil {
		t.Errorf("path doubling detected: file exists at %s (should not)", doubledPath)
	}
}

// TestReceiver_ResponseJSON verifies the sync response contains valid JSON.
func TestReceiver_ResponseJSON(t *testing.T) {
	r, _ := createTestReceiver(t)

	tarData := createTarStream(t, map[string]string{"file.txt": "hello"})

	req := httptest.NewRequest(http.MethodPut,
		"/api/v1/sync/pvc-12345678-1234-1234-1234-123456789abc?basePath=/pvcs&subPath=pvc-12345678-1234-1234-1234-123456789abc",
		tarData)
	req.Header.Set("Authorization", "Bearer test-token-123")
	w := httptest.NewRecorder()

	r.handleSync(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}

	var resp map[string]interface{}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("response is not valid JSON: %v", err)
	}
	if resp["status"] != "ok" {
		t.Errorf("status = %v, want ok", resp["status"])
	}

	files, ok := resp["filesExtracted"].(float64)
	if !ok || files != 1 {
		t.Errorf("filesExtracted = %v, want 1", resp["filesExtracted"])
	}
}

// TestReceiver_Metadata_TooLarge verifies metadata size limit enforcement.
func TestReceiver_Metadata_TooLarge(t *testing.T) {
	r, _ := createTestReceiver(t)

	// Create metadata larger than 1 MB.
	largeData := strings.Repeat("x", DefaultMaxMetadataBytes+100)

	req := httptest.NewRequest(http.MethodPut,
		"/api/v1/metadata/pvc-12345678-1234-1234-1234-123456789abc?basePath=/pvcs&subPath=pvc-12345678-1234-1234-1234-123456789abc",
		strings.NewReader(largeData))
	req.Header.Set("Authorization", "Bearer test-token-123")
	w := httptest.NewRecorder()

	r.handleMetadata(w, req)

	if w.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("expected 413 for too-large metadata, got %d: %s", w.Code, w.Body.String())
	}
}

// TestReceiver_TLS_Integration verifies the receiver serves HTTPS with TLS.
func TestReceiver_TLS_Integration(t *testing.T) {
	basePath := t.TempDir()
	r := NewReceiver(basePath, "tls-token", ":0")

	// Use httptest.NewUnstartedServer with StartTLS for a proper TLS test.
	tlsServer := httptest.NewUnstartedServer(r.httpServer.Handler)
	tlsServer.StartTLS()
	defer tlsServer.Close()

	// Create client trusting the test server's cert.
	tlsClient := tlsServer.Client()

	// Health check over HTTPS.
	resp, err := tlsClient.Get(tlsServer.URL + "/api/v1/health")
	if err != nil {
		t.Fatalf("HTTPS health check failed: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200, got %d", resp.StatusCode)
	}

	// Upload tar over HTTPS.
	tarData := createTarStream(t, map[string]string{"tls-file.txt": "encrypted content"})
	svName := "pvc-aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee"
	syncReq, _ := http.NewRequest(http.MethodPut,
		tlsServer.URL+"/api/v1/sync/"+svName+"?basePath=/pvcs&subPath="+svName,
		tarData)
	syncReq.Header.Set("Authorization", "Bearer tls-token")
	syncResp, err := tlsClient.Do(syncReq)
	if err != nil {
		t.Fatalf("HTTPS sync request failed: %v", err)
	}
	syncResp.Body.Close()
	if syncResp.StatusCode != http.StatusOK {
		t.Fatalf("sync expected 200, got %d", syncResp.StatusCode)
	}

	// Verify file was extracted.
	content, err := os.ReadFile(filepath.Join(basePath, "pvcs", svName, "tls-file.txt"))
	if err != nil {
		t.Fatalf("reading extracted file: %v", err)
	}
	if string(content) != "encrypted content" {
		t.Errorf("content = %q, want %q", string(content), "encrypted content")
	}
}

// TestReceiver_SetTLS_Fields verifies SetTLS sets the struct fields correctly.
func TestReceiver_SetTLS_Fields(t *testing.T) {
	r := NewReceiver("/data", "token", ":8443")
	if r.tlsCertFile != "" || r.tlsKeyFile != "" {
		t.Error("expected empty TLS fields initially")
	}

	r.SetTLS("/certs/tls.crt", "/certs/tls.key")
	if r.tlsCertFile != "/certs/tls.crt" {
		t.Errorf("tlsCertFile = %q, want /certs/tls.crt", r.tlsCertFile)
	}
	if r.tlsKeyFile != "/certs/tls.key" {
		t.Errorf("tlsKeyFile = %q, want /certs/tls.key", r.tlsKeyFile)
	}
}

// generateSelfSignedCert creates a self-signed certificate PEM for testing.
func generateSelfSignedCert(t *testing.T) (certPEM, keyPEM []byte) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("generating key: %v", err)
	}
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(1),
		Subject:      pkix.Name{CommonName: "test"},
		NotBefore:    time.Now(),
		NotAfter:     time.Now().Add(time.Hour),
		KeyUsage:     x509.KeyUsageCertSign | x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		DNSNames:     []string{"localhost"},
		IsCA:         true,
		BasicConstraintsValid: true,
	}
	certDER, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatalf("creating cert: %v", err)
	}
	certPEM = pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: certDER})
	keyDER, _ := x509.MarshalECPrivateKey(key)
	keyPEM = pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER})
	return certPEM, keyPEM
}

// Receiver integration test with httptest.Server.
func TestReceiver_Integration(t *testing.T) {
	basePath := t.TempDir()
	r := NewReceiver(basePath, "secret-token", ":0")

	ts := httptest.NewServer(r.httpServer.Handler)
	defer ts.Close()

	// 1. Health check.
	resp, err := http.Get(ts.URL + "/api/v1/health")
	if err != nil {
		t.Fatalf("health check request failed: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("health check expected 200, got %d", resp.StatusCode)
	}

	// 2. Upload tar.
	tarData := createTarStream(t, map[string]string{
		"data.bin": "binary content here",
	})

	svName := "pvc-aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee"
	syncReq, _ := http.NewRequest(http.MethodPut,
		ts.URL+"/api/v1/sync/"+svName+"?basePath=/pvcs&subPath="+svName,
		tarData)
	syncReq.Header.Set("Authorization", "Bearer secret-token")
	syncResp, err := http.DefaultClient.Do(syncReq)
	if err != nil {
		t.Fatalf("sync request failed: %v", err)
	}
	syncBody, _ := io.ReadAll(syncResp.Body)
	syncResp.Body.Close()
	if syncResp.StatusCode != http.StatusOK {
		t.Fatalf("sync expected 200, got %d: %s", syncResp.StatusCode, string(syncBody))
	}

	// 3. Upload metadata.
	metaJSON := `{"subVolumeName":"` + svName + `","replicationPolicy":"test"}`
	metaReq, _ := http.NewRequest(http.MethodPut,
		ts.URL+"/api/v1/metadata/"+svName+"?basePath=/pvcs&subPath="+svName,
		strings.NewReader(metaJSON))
	metaReq.Header.Set("Authorization", "Bearer secret-token")
	metaResp, err := http.DefaultClient.Do(metaReq)
	if err != nil {
		t.Fatalf("metadata request failed: %v", err)
	}
	metaResp.Body.Close()
	if metaResp.StatusCode != http.StatusOK {
		t.Fatalf("metadata expected 200, got %d", metaResp.StatusCode)
	}

	// Verify files on disk.
	destDir := filepath.Join(basePath, "pvcs", svName)
	content, err := os.ReadFile(filepath.Join(destDir, "data.bin"))
	if err != nil {
		t.Fatalf("reading data.bin: %v", err)
	}
	if string(content) != "binary content here" {
		t.Errorf("data.bin = %q, want %q", string(content), "binary content here")
	}

	metaContent, err := os.ReadFile(filepath.Join(destDir, metadataFileName))
	if err != nil {
		t.Fatalf("reading metadata: %v", err)
	}
	if !strings.Contains(string(metaContent), svName) {
		t.Errorf("metadata doesn't contain SubVolume name")
	}
}
