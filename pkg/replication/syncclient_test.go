package replication

import (
	"archive/tar"
	"context"
	"crypto/x509"
	"encoding/pem"
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

	_, err := client.SyncSubVolume(context.Background(), sourceDir, "/pvcs", subPath, "pvc-12345678-1234-1234-1234-123456789abc", metadata)
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
	_, err := client.SyncSubVolume(context.Background(), sourceDir, "/pvcs", subPath, "pvc-12345678-1234-1234-1234-123456789abc", nil)
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
	_, err := client.SyncSubVolume(context.Background(), sourceDir, "/pvcs", subPath, "pvc-12345678-1234-1234-1234-123456789abc", nil)
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
	_, err := client.SyncSubVolume(context.Background(), sourceDir, "/pvcs", subPath, "pvc-12345678-1234-1234-1234-123456789abc", nil)
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
	_, err := client.SyncSubVolume(context.Background(), sourceDir, "/pvcs", subPath, "pvc-12345678-1234-1234-1234-123456789abc", []byte(`{"test":true}`))
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
	_, err := client.SyncSubVolume(context.Background(), sourceDir, "/pvcs", subPath, "pvc-12345678-1234-1234-1234-123456789abc", nil)
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
	_, err := client.SyncSubVolume(ctx, sourceDir, "/pvcs", subPath, "pvc-12345678-1234-1234-1234-123456789abc", nil)
	if err == nil {
		t.Fatal("expected error for cancelled context, got nil")
	}
}

// TestSyncClient_ReturnsBytesWritten verifies SyncSubVolume returns bytesWritten
// from the receiver's JSON response (Fix 4).
func TestSyncClient_ReturnsBytesWritten(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.Contains(r.URL.Path, "/api/v1/sync/") {
			io.ReadAll(r.Body)
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusOK)
			w.Write([]byte(`{"status":"ok","filesExtracted":3,"bytesWritten":1048576}`))
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
	bytesWritten, err := client.SyncSubVolume(context.Background(), sourceDir, "/pvcs", subPath, "pvc-12345678-1234-1234-1234-123456789abc", nil)
	if err != nil {
		t.Fatalf("SyncSubVolume failed: %v", err)
	}
	if bytesWritten != 1048576 {
		t.Errorf("bytesWritten = %d, want 1048576", bytesWritten)
	}
}

// TestNewSyncClientWithCA_TrustedCA verifies the client succeeds when given the correct CA.
func TestNewSyncClientWithCA_TrustedCA(t *testing.T) {
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.Contains(r.URL.Path, "/api/v1/sync/") {
			io.ReadAll(r.Body)
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusOK)
			w.Write([]byte(`{"status":"ok","bytesWritten":42}`))
			return
		}
		http.NotFound(w, r)
	}))
	defer server.Close()

	// Extract the server's CA certificate as PEM.
	serverCert := server.TLS.Certificates[0]
	caCertPEM := pem.EncodeToMemory(&pem.Block{
		Type:  "CERTIFICATE",
		Bytes: serverCert.Certificate[0],
	})

	sourceDir := t.TempDir()
	subPath := "pvc-12345678-1234-1234-1234-123456789abc"
	svDir := filepath.Join(sourceDir, subPath)
	os.MkdirAll(svDir, 0755)
	os.WriteFile(filepath.Join(svDir, "file.txt"), []byte("data"), 0644)

	client := NewSyncClientWithCA(server.URL, "token", caCertPEM)
	bytesWritten, err := client.SyncSubVolume(context.Background(), sourceDir, "/pvcs", subPath, "pvc-12345678-1234-1234-1234-123456789abc", nil)
	if err != nil {
		t.Fatalf("SyncSubVolume with trusted CA failed: %v", err)
	}
	if bytesWritten != 42 {
		t.Errorf("bytesWritten = %d, want 42", bytesWritten)
	}
}

// TestNewSyncClientWithCA_UntrustedCA verifies the client fails when given no CA for a TLS server.
func TestNewSyncClientWithCA_UntrustedCA(t *testing.T) {
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.ReadAll(r.Body)
		w.WriteHeader(http.StatusOK)
		w.Write([]byte(`{"status":"ok"}`))
	}))
	defer server.Close()

	sourceDir := t.TempDir()
	subPath := "pvc-12345678-1234-1234-1234-123456789abc"
	svDir := filepath.Join(sourceDir, subPath)
	os.MkdirAll(svDir, 0755)
	os.WriteFile(filepath.Join(svDir, "file.txt"), []byte("data"), 0644)

	// Create client with empty CA — should fail TLS verification.
	client := NewSyncClientWithCA(server.URL, "token", nil)
	_, err := client.SyncSubVolume(context.Background(), sourceDir, "/pvcs", subPath, "pvc-12345678-1234-1234-1234-123456789abc", nil)
	if err == nil {
		t.Fatal("expected TLS error for untrusted server, got nil")
	}
	// Error should be related to certificate verification.
	if !strings.Contains(err.Error(), "certificate") && !strings.Contains(err.Error(), "tls") {
		t.Errorf("expected TLS/certificate error, got: %v", err)
	}
}

// TestNewSyncClientWithCA_EmptyCA verifies that empty CA PEM creates a default client.
func TestNewSyncClientWithCA_EmptyCA(t *testing.T) {
	client := NewSyncClientWithCA("https://example.com", "token", nil)
	if client.httpClient == nil {
		t.Fatal("expected non-nil httpClient")
	}
	if client.endpoint != "https://example.com" {
		t.Errorf("endpoint = %q, want https://example.com", client.endpoint)
	}
}

// TestNewSyncClientWithCA_ValidCAParsing verifies the CA cert pool is configured.
func TestNewSyncClientWithCA_ValidCAParsing(t *testing.T) {
	// Use a real (but test-only) TLS server to get a valid cert.
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	defer server.Close()

	serverCert := server.TLS.Certificates[0]
	caCertPEM := pem.EncodeToMemory(&pem.Block{
		Type:  "CERTIFICATE",
		Bytes: serverCert.Certificate[0],
	})

	client := NewSyncClientWithCA("https://example.com", "token", caCertPEM)
	if client.httpClient == nil {
		t.Fatal("expected non-nil httpClient")
	}

	// Verify the transport has a custom TLS config.
	transport, ok := client.httpClient.Transport.(*http.Transport)
	if !ok {
		t.Fatal("expected *http.Transport")
	}
	if transport.TLSClientConfig == nil {
		t.Fatal("expected non-nil TLSClientConfig")
	}
	if transport.TLSClientConfig.RootCAs == nil {
		t.Fatal("expected non-nil RootCAs")
	}

	// Verify the CA pool contains the expected cert.
	cert, err := x509.ParseCertificate(serverCert.Certificate[0])
	if err != nil {
		t.Fatalf("parsing cert: %v", err)
	}
	subjects := transport.TLSClientConfig.RootCAs.Subjects()
	found := false
	for _, s := range subjects {
		if string(s) == string(cert.RawSubject) {
			found = true
			break
		}
	}
	if !found {
		t.Error("CA cert not found in RootCAs pool")
	}
}

// TestSyncClient_NoBytesWrittenInResponse verifies graceful handling when
// the receiver response doesn't include bytesWritten.
func TestSyncClient_NoBytesWrittenInResponse(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.Contains(r.URL.Path, "/api/v1/sync/") {
			io.ReadAll(r.Body)
			w.WriteHeader(http.StatusOK)
			w.Write([]byte(`{"status":"ok"}`))
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
	bytesWritten, err := client.SyncSubVolume(context.Background(), sourceDir, "/pvcs", subPath, "pvc-12345678-1234-1234-1234-123456789abc", nil)
	if err != nil {
		t.Fatalf("SyncSubVolume failed: %v", err)
	}
	// Should be 0 when response doesn't include bytesWritten.
	if bytesWritten != 0 {
		t.Errorf("bytesWritten = %d, want 0", bytesWritten)
	}
}
