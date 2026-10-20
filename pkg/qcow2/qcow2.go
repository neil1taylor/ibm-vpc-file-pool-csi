// Package qcow2 reads which parts of a qcow2 image hold data.
//
// QEMU writes VM disk copies as qcow2 staging files. The driver image has no
// qemu-img, so the copy Job reads the staging file with this package. It reads
// only what a QEMU backup target uses: no backing file, no encryption, no
// compressed clusters, no external data file and no extended L2 entries. It
// refuses anything else rather than guess.
//
// Format reference: docs/interop/qcow2.rst in the QEMU source.
package qcow2

import (
	"encoding/binary"
	"fmt"
	"io"
)

const (
	magic = 0x514649fb // "QFI\xfb"

	// Header field offsets. All fields are big-endian.
	offVersion          = 4
	offBackingOffset    = 8
	offClusterBits      = 20
	offSize             = 24
	offCryptMethod      = 32
	offL1Size           = 36
	offL1TableOffset    = 40
	offIncompatFeatures = 72

	headerV2Len = 72
	headerV3Len = 104

	minClusterBits = 9
	maxClusterBits = 21

	// maxL1Entries is QEMU's own limit: an L1 table of at most 32 MiB.
	maxL1Entries = 32 << 20 / tableEntrySize

	// Bits 9-55 of an L1 or L2 entry hold a host offset.
	offsetMask     = 0x00fffffffffffe00
	l2Compressed   = 1 << 62
	l2ZeroFlag     = 1 << 0
	tableEntrySize = 8
)

// Kind says what an extent holds.
type Kind int

const (
	// Data means the extent's bytes are stored in the image at HostOffset.
	Data Kind = iota
	// Zero means the image records the extent as all zeros.
	Zero
)

// Extent is a run of the virtual disk that the image records. Parts of the
// disk with no extent are not in the image: for a full copy they are zeros,
// and for a change copy they did not change.
type Extent struct {
	Offset     int64 // offset in the virtual disk
	Length     int64
	Kind       Kind
	HostOffset int64 // offset in the image file; only for Data
}

// Image is an open qcow2 image.
type Image struct {
	VirtualSize int64
	ClusterSize int64

	r         io.ReaderAt
	fileSize  int64
	version   uint32
	l1Size    int64
	l1Offset  int64
	l2Entries int64
}

// Open reads and checks the qcow2 header. fileSize is the size of the image
// file; no table or data cluster may lie past it.
func Open(r io.ReaderAt, fileSize int64) (*Image, error) {
	h := make([]byte, headerV3Len)
	n, err := r.ReadAt(h, 0)
	if n < headerV2Len {
		return nil, fmt.Errorf("reading qcow2 header: %d bytes, need %d: %v", n, headerV2Len, err)
	}
	be := binary.BigEndian
	if be.Uint32(h) != magic {
		return nil, fmt.Errorf("not a qcow2 image")
	}
	img := &Image{r: r, fileSize: fileSize, version: be.Uint32(h[offVersion:])}
	if img.version != 2 && img.version != 3 {
		return nil, fmt.Errorf("qcow2 version %d is not supported", img.version)
	}
	if img.version == 3 {
		if n < headerV3Len {
			return nil, fmt.Errorf("reading qcow2 v3 header: %d bytes, need %d: %v", n, headerV3Len, err)
		}
		if f := be.Uint64(h[offIncompatFeatures:]); f != 0 {
			return nil, fmt.Errorf("qcow2 image has incompatible features %#x", f)
		}
	}
	if be.Uint64(h[offBackingOffset:]) != 0 {
		return nil, fmt.Errorf("qcow2 image has a backing file")
	}
	if be.Uint32(h[offCryptMethod:]) != 0 {
		return nil, fmt.Errorf("qcow2 image is encrypted")
	}
	bits := be.Uint32(h[offClusterBits:])
	if bits < minClusterBits || bits > maxClusterBits {
		return nil, fmt.Errorf("qcow2 cluster size 2^%d is out of range", bits)
	}
	img.ClusterSize = int64(1) << bits
	img.l2Entries = img.ClusterSize / tableEntrySize

	size := be.Uint64(h[offSize:])
	if size > 1<<62 {
		return nil, fmt.Errorf("qcow2 virtual size %d is out of range", size)
	}
	img.VirtualSize = int64(size)
	img.l1Size = int64(be.Uint32(h[offL1Size:]))
	if img.l1Size > maxL1Entries {
		return nil, fmt.Errorf("qcow2 L1 table has %d entries, more than the limit of %d", img.l1Size, maxL1Entries)
	}
	l1Offset := be.Uint64(h[offL1TableOffset:])
	if l1Offset >= 1<<56 {
		return nil, fmt.Errorf("qcow2 L1 table offset %#x is out of range", l1Offset)
	}
	img.l1Offset = int64(l1Offset) // #nosec G115 -- checked to fit in 56 bits above
	if img.l1Offset+img.l1Size*tableEntrySize > fileSize {
		return nil, fmt.Errorf("qcow2 L1 table at %d ends past the end of the file (%d bytes)", img.l1Offset, fileSize)
	}

	bytesPerL2 := img.ClusterSize * img.l2Entries
	if need := (img.VirtualSize + bytesPerL2 - 1) / bytesPerL2; img.l1Size < need {
		return nil, fmt.Errorf("qcow2 L1 table has %d entries, need %d", img.l1Size, need)
	}
	return img, nil
}

// Extents returns every extent the image records, in virtual disk order.
func (img *Image) Extents() ([]Extent, error) {
	var exts []Extent
	err := img.Walk(func(e Extent) error {
		exts = append(exts, e)
		return nil
	})
	return exts, err
}

// Walk calls fn for every extent the image records, in virtual disk order.
// Neighbouring clusters of the same kind are merged; Data clusters are merged
// only if they are also next to each other in the image file. Walk refuses an
// image in which two L1 entries point at the same L2 table, or a table or data
// cluster lies past the end of the file.
func (img *Image) Walk(fn func(Extent) error) error {
	if img.l1Offset%img.ClusterSize != 0 {
		return fmt.Errorf("qcow2 L1 table offset %d is not cluster aligned", img.l1Offset)
	}
	l1, err := img.readTable(img.l1Offset, img.l1Size)
	if err != nil {
		return fmt.Errorf("reading L1 table: %w", err)
	}

	var pending *Extent
	add := func(e Extent) error {
		if e.Offset+e.Length > img.VirtualSize {
			e.Length = img.VirtualSize - e.Offset
		}
		if e.Kind == Data && e.HostOffset+e.Length > img.fileSize {
			return fmt.Errorf("qcow2 cluster at %d has host offset %d past the end of the file", e.Offset, e.HostOffset)
		}
		if pending != nil && pending.Kind == e.Kind && pending.Offset+pending.Length == e.Offset &&
			(e.Kind != Data || pending.HostOffset+pending.Length == e.HostOffset) {
			pending.Length += e.Length
			return nil
		}
		if pending != nil {
			if err := fn(*pending); err != nil {
				return err
			}
		}
		pending = &e
		return nil
	}

	seenL2 := make(map[int64]bool)
	bytesPerL2 := img.ClusterSize * img.l2Entries
	for i, l1e := range l1 {
		base := int64(i) * bytesPerL2
		if base >= img.VirtualSize {
			break
		}
		l2Offset := entryOffset(l1e)
		if l2Offset == 0 {
			continue
		}
		if l2Offset%img.ClusterSize != 0 {
			return fmt.Errorf("qcow2 L2 table offset %d is not cluster aligned", l2Offset)
		}
		if l2Offset+img.ClusterSize > img.fileSize {
			return fmt.Errorf("qcow2 L2 table at %d ends past the end of the file", l2Offset)
		}
		if seenL2[l2Offset] {
			return fmt.Errorf("qcow2 L2 table at %d is used more than once", l2Offset)
		}
		seenL2[l2Offset] = true
		l2, err := img.readTable(l2Offset, img.l2Entries)
		if err != nil {
			return fmt.Errorf("reading L2 table at %d: %w", l2Offset, err)
		}
		for j, l2e := range l2 {
			off := base + int64(j)*img.ClusterSize
			if off >= img.VirtualSize {
				break
			}
			if l2e&l2Compressed != 0 {
				return fmt.Errorf("qcow2 cluster at %d is compressed", off)
			}
			host := entryOffset(l2e)
			switch {
			case img.version == 3 && l2e&l2ZeroFlag != 0:
				err = add(Extent{Offset: off, Length: img.ClusterSize, Kind: Zero})
			case host == 0:
				// Not in this image.
			case host%img.ClusterSize != 0:
				return fmt.Errorf("qcow2 cluster at %d has unaligned host offset %d", off, host)
			default:
				err = add(Extent{Offset: off, Length: img.ClusterSize, Kind: Data, HostOffset: host})
			}
			if err != nil {
				return err
			}
		}
	}
	if pending != nil {
		return fn(*pending)
	}
	return nil
}

// entryOffset returns the host offset in bits 9-55 of an L1 or L2 entry.
func entryOffset(e uint64) int64 {
	return int64(e & offsetMask) // #nosec G115 -- the mask keeps 56 bits
}

// DataReader returns a reader for the bytes of a Data extent.
func (img *Image) DataReader(e Extent) io.Reader {
	return io.NewSectionReader(img.r, e.HostOffset, e.Length)
}

func (img *Image) readTable(offset, entries int64) ([]uint64, error) {
	buf := make([]byte, entries*tableEntrySize)
	if _, err := img.r.ReadAt(buf, offset); err != nil {
		return nil, err
	}
	t := make([]uint64, entries)
	for i := range t {
		t[i] = binary.BigEndian.Uint64(buf[i*tableEntrySize:])
	}
	return t, nil
}

// zeroChunk bounds the buffer used to write a Zero extent.
const zeroChunk = 1 << 20

// WriteRaw writes the image's extents into dst at their virtual offsets, as a
// raw disk. Parts with no extent are not written. If zeros is false, Zero
// extents are not written either, which keeps a new sparse file sparse. It
// returns the number of Data bytes written.
func (img *Image) WriteRaw(dst io.WriterAt, zeros bool) (int64, error) {
	var written int64
	var zbuf []byte
	err := img.Walk(func(e Extent) error {
		switch e.Kind {
		case Data:
			n, err := io.Copy(io.NewOffsetWriter(dst, e.Offset), img.DataReader(e))
			written += n
			if err != nil {
				return fmt.Errorf("copying extent at %d: %w", e.Offset, err)
			}
			if n != e.Length {
				return fmt.Errorf("copying extent at %d: got %d bytes, want %d", e.Offset, n, e.Length)
			}
		case Zero:
			if !zeros {
				return nil
			}
			if zbuf == nil {
				zbuf = make([]byte, zeroChunk)
			}
			for off := e.Offset; off < e.Offset+e.Length; {
				n := min(int64(len(zbuf)), e.Offset+e.Length-off)
				if _, err := dst.WriteAt(zbuf[:n], off); err != nil {
					return fmt.Errorf("zeroing extent at %d: %w", e.Offset, err)
				}
				off += n
			}
		}
		return nil
	})
	return written, err
}
