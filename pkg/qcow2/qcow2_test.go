package qcow2

import (
	"bytes"
	"encoding/binary"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func openTestImage(t *testing.T, name string) *Image {
	t.Helper()
	f, err := os.Open(filepath.Join("testdata", name))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = f.Close() })
	img, err := Open(f)
	if err != nil {
		t.Fatalf("Open(%s): %v", name, err)
	}
	return img
}

func TestOpen_Extents(t *testing.T) {
	tests := []struct {
		file        string
		virtualSize int64
		clusterSize int64
		want        []Extent
	}{
		{
			file:        "v3.qcow2",
			virtualSize: 1049088,
			clusterSize: 4096,
			want: []Extent{
				{Offset: 0, Length: 4096, Kind: Data, HostOffset: 24576},
				{Offset: 8192, Length: 8192, Kind: Data, HostOffset: 28672},
				{Offset: 65536, Length: 8192, Kind: Zero},
				{Offset: 262144, Length: 4096, Kind: Data, HostOffset: 20480},
				{Offset: 1048576, Length: 512, Kind: Data, HostOffset: 36864},
			},
		},
		{
			file:        "v2.qcow2",
			virtualSize: 1048576,
			clusterSize: 4096,
			want: []Extent{
				{Offset: 0, Length: 4096, Kind: Data, HostOffset: 20480},
				{Offset: 12288, Length: 4096, Kind: Data, HostOffset: 24576},
			},
		},
		{
			file:        "default.qcow2",
			virtualSize: 4194304,
			clusterSize: 65536,
			want: []Extent{
				{Offset: 131072, Length: 65536, Kind: Data, HostOffset: 327680},
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.file, func(t *testing.T) {
			img := openTestImage(t, tt.file)
			if img.VirtualSize != tt.virtualSize {
				t.Errorf("VirtualSize = %d, want %d", img.VirtualSize, tt.virtualSize)
			}
			if img.ClusterSize != tt.clusterSize {
				t.Errorf("ClusterSize = %d, want %d", img.ClusterSize, tt.clusterSize)
			}
			got, err := img.Extents()
			if err != nil {
				t.Fatalf("Extents: %v", err)
			}
			if !reflect.DeepEqual(got, tt.want) {
				t.Errorf("Extents =\n%+v\nwant\n%+v", got, tt.want)
			}
		})
	}
}

func TestImage_DataReader(t *testing.T) {
	img := openTestImage(t, "v3.qcow2")
	exts, err := img.Extents()
	if err != nil {
		t.Fatal(err)
	}
	want := map[int64]byte{0: 0xaa, 8192: 0xbb, 262144: 0xdd, 1048576: 0xcc}
	for _, e := range exts {
		if e.Kind != Data {
			continue
		}
		b, err := io.ReadAll(img.DataReader(e))
		if err != nil {
			t.Fatalf("read extent at %d: %v", e.Offset, err)
		}
		if int64(len(b)) != e.Length {
			t.Fatalf("extent at %d: read %d bytes, want %d", e.Offset, len(b), e.Length)
		}
		if !bytes.Equal(b, bytes.Repeat([]byte{want[e.Offset]}, len(b))) {
			t.Errorf("extent at %d does not hold pattern %#x", e.Offset, want[e.Offset])
		}
	}
}

// mutated returns v3.qcow2 with one change made by edit.
func mutated(t *testing.T, edit func(b []byte)) *bytes.Reader {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("testdata", "v3.qcow2"))
	if err != nil {
		t.Fatal(err)
	}
	edit(b)
	return bytes.NewReader(b)
}

func TestOpen_Refuses(t *testing.T) {
	fromFile := func(name string) func(t *testing.T) io.ReaderAt {
		return func(t *testing.T) io.ReaderAt {
			b, err := os.ReadFile(filepath.Join("testdata", name))
			if err != nil {
				t.Fatal(err)
			}
			return bytes.NewReader(b)
		}
	}
	edit := func(f func(b []byte)) func(t *testing.T) io.ReaderAt {
		return func(t *testing.T) io.ReaderAt { return mutated(t, f) }
	}
	tests := []struct {
		name    string
		image   func(t *testing.T) io.ReaderAt
		wantErr string
	}{
		{"backing file", fromFile("backing.qcow2"), "backing file"},
		{"not qcow2", edit(func(b []byte) { copy(b, "RAW!") }), "not a qcow2 image"},
		{"version 4", edit(func(b []byte) { binary.BigEndian.PutUint32(b[4:], 4) }), "version 4"},
		{"encrypted", edit(func(b []byte) { binary.BigEndian.PutUint32(b[32:], 1) }), "encrypted"},
		{"dirty", edit(func(b []byte) { b[79] |= 1 }), "incompatible features"},
		{"corrupt", edit(func(b []byte) { b[79] |= 2 }), "incompatible features"},
		{"external data file", edit(func(b []byte) { b[79] |= 4 }), "incompatible features"},
		{"extended L2", edit(func(b []byte) { b[79] |= 16 }), "incompatible features"},
		{"cluster size too small", edit(func(b []byte) { binary.BigEndian.PutUint32(b[20:], 8) }), "cluster size"},
		{"cluster size too large", edit(func(b []byte) { binary.BigEndian.PutUint32(b[20:], 22) }), "cluster size"},
		{"truncated header", func(t *testing.T) io.ReaderAt { return bytes.NewReader([]byte("QFI\xfb")) }, "header"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := Open(tt.image(t))
			if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
				t.Fatalf("Open error = %v, want one containing %q", err, tt.wantErr)
			}
		})
	}
}

func TestExtents_RefusesCompressedClusters(t *testing.T) {
	img := openTestImage(t, "compressed.qcow2")
	if _, err := img.Extents(); err == nil || !strings.Contains(err.Error(), "compressed") {
		t.Fatalf("Extents error = %v, want one about compressed clusters", err)
	}
}

func TestExtents_RefusesUnalignedTables(t *testing.T) {
	// Point the L1 table at an offset that is not cluster aligned.
	img, err := Open(mutated(t, func(b []byte) {
		l1 := binary.BigEndian.Uint64(b[40:])
		binary.BigEndian.PutUint64(b[40:], l1+8)
	}))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	if _, err := img.Extents(); err == nil || !strings.Contains(err.Error(), "aligned") {
		t.Fatalf("Extents error = %v, want one about alignment", err)
	}
}

func TestExtents_RefusesL1TooSmall(t *testing.T) {
	// An L1 table that cannot cover the virtual size means a damaged header.
	_, err := Open(mutated(t, func(b []byte) { binary.BigEndian.PutUint32(b[36:], 0) }))
	if err == nil || !strings.Contains(err.Error(), "L1") {
		t.Fatalf("Open error = %v, want one about the L1 table", err)
	}
}

// memWriterAt is an io.WriterAt over a byte slice.
type memWriterAt struct {
	buf []byte
}

func (m *memWriterAt) WriteAt(p []byte, off int64) (int, error) {
	copy(m.buf[off:], p)
	return len(p), nil
}

func TestImage_WriteRaw(t *testing.T) {
	img := openTestImage(t, "v3.qcow2")
	want := make([]byte, img.VirtualSize)
	fill := func(off, n int, v byte) { copy(want[off:], bytes.Repeat([]byte{v}, n)) }
	fill(0, 4096, 0xaa)
	fill(8192, 8192, 0xbb)
	fill(262144, 4096, 0xdd)
	fill(1048576, 512, 0xcc)

	for _, zeros := range []bool{false, true} {
		// Old data sits under the zero extent, so a skipped zero write shows.
		dst := &memWriterAt{buf: make([]byte, img.VirtualSize)}
		copy(dst.buf[65536:], bytes.Repeat([]byte{0x11}, 8192))
		n, err := img.WriteRaw(dst, zeros)
		if err != nil {
			t.Fatalf("WriteRaw(zeros=%v): %v", zeros, err)
		}
		if n != 4096+8192+4096+512 {
			t.Errorf("WriteRaw(zeros=%v) reported %d data bytes", zeros, n)
		}
		zeroRegion := dst.buf[65536:73728]
		if zeros && !bytes.Equal(dst.buf, want) {
			t.Errorf("WriteRaw(zeros=true) output differs from the disk contents")
		}
		if !zeros && !bytes.Equal(zeroRegion, bytes.Repeat([]byte{0x11}, len(zeroRegion))) {
			t.Errorf("WriteRaw(zeros=false) wrote the zero extent")
		}
	}
}
