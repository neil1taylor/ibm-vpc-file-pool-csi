package replication

import (
	"archive/tar"
	"bytes"
	"os"
	"path/filepath"
	"testing"
)

// A large plain file written to NFS must not fill the container's memory with
// dirty pages (#225, as #212 for VM disks): the receiver syncs while it writes.
func TestExtractTar_SyncsWhileWritingALargeFile(t *testing.T) {
	rcv, _ := createTestReceiver(t)
	syncs := 0
	rcv.cacheDropEvery = 2 * chunk
	rcv.onCacheDrop = func() { syncs++ }

	data := bytes.Repeat([]byte{7}, 8*chunk)
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	if err := tw.WriteHeader(&tar.Header{Name: "big.bin", Mode: 0o644, Size: int64(len(data)), Typeflag: tar.TypeReg}); err != nil {
		t.Fatal(err)
	}
	if _, err := tw.Write(data); err != nil {
		t.Fatal(err)
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	dest := t.TempDir()
	if _, _, err := rcv.extractTar(&buf, dest); err != nil {
		t.Fatal(err)
	}
	if syncs < 3 {
		t.Fatalf("syncs = %d, want at least 3 for 8 chunks at a limit of 2", syncs)
	}
	got, err := os.ReadFile(filepath.Join(dest, "big.bin"))
	if err != nil || !bytes.Equal(got, data) {
		t.Fatalf("file content wrong (%v)", err)
	}
}
