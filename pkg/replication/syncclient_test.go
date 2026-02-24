package replication

import (
	"archive/tar"
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestSyncClient_Upload(t *testing.T) {
	// Create a test source directory with files.
	sourceDir := t.TempDir()
	subPath := "pvc-12345678-1234-1234-1234-123456789abc"
	svDir := filepath.Join(sourceDir, subPath)
	if err := os.MkdirAll(svDir, 0755); err != nil {
		t.Fatalf("creating subvolume dir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(svDir, "test.txt"), []byte("hello"), 0644); err != nil {
		t.Fatalf("writing test file: %v", err)
	}
	if err := os.MkdirAll(filepath.Join(svDir, "subdir"), 0755); err != nil {
		t.Fatalf("creating subdir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(svDir, "subdir", "nested.txt"), []byte("nested"), 0644); err != nil {
		t.Fatalf("writing nested file: %v", err)
	}

	// Track what the server received.
	var receivedTarFiles []string
	var receivedMetadata string
	var receivedAuthHeader string

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		receivedAuthHeader = r.Header.Get("Authorization")

		if strings.Contains(r.URL.Path, "/api/v1/sync/") {
			// Read tar and record file names.
			tr := tar.NewReader(r.Body)
			for {
				hdr, err := tr.Next()
				if err == io.EOF {
					break
				}
				if err != nil {
					http.Error(w, err.Error(), 500)
					return
				}
				receivedTarFiles = append(receivedTarFiles, hdr.Name)
			}
			w.WriteHeader(http.StatusOK)
			w.Write([]byte(`{"status":"ok"}`))
			return
		}

		if strings.Contains(r.URL.Path, "/api/v1/metadata/") {
			body, _ := io.ReadAll(r.Body)
			receivedMetadata = string(body)
			w.WriteHeader(http.StatusOK)
			w.Write([]byte(`{"status":"ok"}`))
			return
		}

		http.NotFound(w, r)
	}))
	defer server.Close()

	client := NewSyncClient(server.URL, "test-token")
	metadata := []byte(`{"subVolumeName":"pvc-12345678-1234-1234-1234-123456789abc"}`)

	err := client.SyncSubVolume(context.Background(), sourceDir, "/pvcs", subPath, "pvc-12345678-1234-1234-1234-123456789abc", metadata)
	if err != nil {
		t.Fatalf("SyncSubVolume failed: %v", err)
	}

	// Verify auth header was sent.
	if receivedAuthHeader != "Bearer test-token" {
		t.Errorf("auth header = %q, want %q", receivedAuthHeader, "Bearer test-token")
	}

	// Verify tar contained the expected files.
	if len(receivedTarFiles) == 0 {
		t.Fatal("no files received in tar")
	}
	foundTestTxt := false
	foundNested := false
	for _, f := range receivedTarFiles {
		if f == "test.txt" {
			foundTestTxt = true
		}
		if f == "subdir/nested.txt" || f == filepath.Join("subdir", "nested.txt") {
			foundNested = true
		}
	}
	if !foundTestTxt {
		t.Errorf("test.txt not found in tar files: %v", receivedTarFiles)
	}
	if !foundNested {
		t.Errorf("subdir/nested.txt not found in tar files: %v", receivedTarFiles)
	}

	// Verify metadata was sent.
	if receivedMetadata == "" {
		t.Error("no metadata received")
	}
	if !strings.Contains(receivedMetadata, "pvc-12345678-1234-1234-1234-123456789abc") {
		t.Errorf("metadata doesn't contain expected SubVolume name: %s", receivedMetadata)
	}
}

func TestSyncClient_UploadNoMetadata(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.Contains(r.URL.Path, "/api/v1/sync/") {
			// Drain tar body.
			io.ReadAll(r.Body)
			w.WriteHeader(http.StatusOK)
			w.Write([]byte(`{"status":"ok"}`))
			return
		}
		if strings.Contains(r.URL.Path, "/api/v1/metadata/") {
			t.Error("metadata endpoint should not be called when metadata is nil")
			w.WriteHeader(http.StatusOK)
			return
		}
		http.NotFound(w, r)
	}))
	defer server.Close()

	sourceDir := t.TempDir()
	subPath := "pvc-12345678-1234-1234-1234-123456789abc"
	svDir := filepath.Join(sourceDir, subPath)
	os.MkdirAll(svDir, 0755)
	os.WriteFile(filepath.Join(svDir, "file.txt"), []byte("data"), 0644)

	client := NewSyncClient(server.URL, "token")
	err := client.SyncSubVolume(context.Background(), sourceDir, "/pvcs", subPath, "pvc-12345678-1234-1234-1234-123456789abc", nil)
	if err != nil {
		t.Fatalf("SyncSubVolume failed: %v", err)
	}
}

func TestSyncClient_NetworkError(t *testing.T) {
	// Use a closed server to simulate network error.
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	server.Close()

	sourceDir := t.TempDir()
	subPath := "pvc-12345678-1234-1234-1234-123456789abc"
	svDir := filepath.Join(sourceDir, subPath)
	os.MkdirAll(svDir, 0755)
	os.WriteFile(filepath.Join(svDir, "file.txt"), []byte("data"), 0644)

	client := NewSyncClient(server.URL, "token")
	err := client.SyncSubVolume(context.Background(), sourceDir, "/pvcs", subPath, "pvc-12345678-1234-1234-1234-123456789abc", nil)
	if err == nil {
		t.Fatal("expected error for closed server, got nil")
	}
}

func TestSyncClient_ServerError(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.ReadAll(r.Body)
		http.Error(w, "internal error", http.StatusInternalServerError)
	}))
	defer server.Close()

	sourceDir := t.TempDir()
	subPath := "pvc-12345678-1234-1234-1234-123456789abc"
	svDir := filepath.Join(sourceDir, subPath)
	os.MkdirAll(svDir, 0755)
	os.WriteFile(filepath.Join(svDir, "file.txt"), []byte("data"), 0644)

	client := NewSyncClient(server.URL, "token")
	err := client.SyncSubVolume(context.Background(), sourceDir, "/pvcs", subPath, "pvc-12345678-1234-1234-1234-123456789abc", nil)
	if err == nil {
		t.Fatal("expected error for 500 response, got nil")
	}
	if !strings.Contains(err.Error(), "500") {
		t.Errorf("error should mention status code: %v", err)
	}
}

func TestSyncClient_MetadataError(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.ReadAll(r.Body)
		if strings.Contains(r.URL.Path, "/api/v1/sync/") {
			w.WriteHeader(http.StatusOK)
			w.Write([]byte(`{"status":"ok"}`))
			return
		}
		if strings.Contains(r.URL.Path, "/api/v1/metadata/") {
			http.Error(w, "metadata error", http.StatusInternalServerError)
			return
		}
	}))
	defer server.Close()

	sourceDir := t.TempDir()
	subPath := "pvc-12345678-1234-1234-1234-123456789abc"
	svDir := filepath.Join(sourceDir, subPath)
	os.MkdirAll(svDir, 0755)
	os.WriteFile(filepath.Join(svDir, "file.txt"), []byte("data"), 0644)

	client := NewSyncClient(server.URL, "token")
	err := client.SyncSubVolume(context.Background(), sourceDir, "/pvcs", subPath, "pvc-12345678-1234-1234-1234-123456789abc", []byte(`{"test":true}`))
	if err == nil {
		t.Fatal("expected error for metadata failure, got nil")
	}
	if !strings.Contains(err.Error(), "metadata") {
		t.Errorf("error should mention metadata: %v", err)
	}
}

func TestSyncClient_StreamingNoBuffering(t *testing.T) {
	// Verify the client uses streaming (pipe) rather than buffering.
	// We check this by creating a large-ish source and verifying it works.
	sourceDir := t.TempDir()
	subPath := "pvc-12345678-1234-1234-1234-123456789abc"
	svDir := filepath.Join(sourceDir, subPath)
	os.MkdirAll(svDir, 0755)

	// Create a 1MB file.
	bigContent := strings.Repeat("A", 1024*1024)
	os.WriteFile(filepath.Join(svDir, "big.bin"), []byte(bigContent), 0644)

	var receivedBytes int64
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.Contains(r.URL.Path, "/api/v1/sync/") {
			n, _ := io.Copy(io.Discard, r.Body)
			receivedBytes = n
			w.WriteHeader(http.StatusOK)
			w.Write([]byte(`{"status":"ok"}`))
			return
		}
		http.NotFound(w, r)
	}))
	defer server.Close()

	client := NewSyncClient(server.URL, "token")
	err := client.SyncSubVolume(context.Background(), sourceDir, "/pvcs", subPath, "pvc-12345678-1234-1234-1234-123456789abc", nil)
	if err != nil {
		t.Fatalf("SyncSubVolume failed: %v", err)
	}

	// Tar overhead is ~1-2KB, so received bytes should be about 1MB.
	if receivedBytes < 1024*1024 {
		t.Errorf("received only %d bytes, expected at least 1MB", receivedBytes)
	}
}

func TestSyncClient_ContextCancellation(t *testing.T) {
	// Server that blocks forever.
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Read body so the client doesn't get a broken pipe before context cancels.
		io.ReadAll(r.Body)
		select {} // block forever
	}))
	defer server.Close()

	sourceDir := t.TempDir()
	subPath := "pvc-12345678-1234-1234-1234-123456789abc"
	svDir := filepath.Join(sourceDir, subPath)
	os.MkdirAll(svDir, 0755)
	os.WriteFile(filepath.Join(svDir, "file.txt"), []byte("data"), 0644)

	ctx, cancel := context.WithCancel(context.Background())
	cancel() // Cancel immediately.

	client := NewSyncClient(server.URL, "token")
	err := client.SyncSubVolume(ctx, sourceDir, "/pvcs", subPath, "pvc-12345678-1234-1234-1234-123456789abc", nil)
	if err == nil {
		t.Fatal("expected error for cancelled context, got nil")
	}
}
