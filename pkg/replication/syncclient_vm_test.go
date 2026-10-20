package replication

import (
	"bytes"
	"context"
	"errors"
	"testing"
	"time"

	"github.com/IBM/ibm-vpc-file-pool-csi/pkg/qcow2"
)

func fullSource(data []byte) *memSource {
	src := &memSource{size: int64(len(data)), data: map[int64][]byte{}}
	for off := int64(0); off < int64(len(data)); off += chunk {
		src.exts = append(src.exts, qcow2.Extent{Offset: off, Length: chunk, Kind: qcow2.Data})
		src.data[off] = data[off : off+chunk]
	}
	return src
}

func (c *vmTestClient) sync() *SyncClient { return NewSyncClient(c.srv.URL, "test-token-123") }

func (c *vmTestClient) args(copyID string, src CopySource, full bool, from *time.Time, to time.Time) VMSendArgs {
	return VMSendArgs{DestBasePath: "/", SubPath: "/pvcs/" + testSV, SVName: testSV, CopyID: copyID,
		Src: src, Full: full, From: from, To: to, VMGroup: "ns/vm1"}
}

func TestSendVMDisk_FirstCopyThenChangeCopy(t *testing.T) {
	c := newVMTestClient(t)
	disk := patterned(4 * chunk)
	res, err := c.sync().SendVMDisk(context.Background(), c.args("copy-one", fullSource(disk), true, nil, t1))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(readDisk(t, c.diskPath()), disk) {
		t.Fatal("far-site disk differs from the copy")
	}
	if res.SentBytes != 4*chunk {
		t.Fatalf("sent %d bytes, want %d", res.SentBytes, 4*chunk)
	}

	res, err = c.sync().SendVMDisk(context.Background(), c.args("copy-two", change(4*chunk, 0x42, []int64{chunk}, nil), false, ptr(t1), t2))
	if err != nil {
		t.Fatal(err)
	}
	if res.SentBytes != chunk || res.ChangedBytes != chunk || res.RestorePoints != 1 {
		t.Fatalf("result = %+v, want one chunk sent and changed and one restore point", res)
	}
	if got := readDisk(t, c.diskPath()); got[chunk] != 0x42 || got[0] != 1 {
		t.Fatal("change was not applied")
	}
}

func TestSendVMDisk_FullCopyOntoExistingDiskSendsOnlyDifferences(t *testing.T) {
	c := newVMTestClient(t)
	base := patterned(8 * chunk)
	if _, err := c.sync().SendVMDisk(context.Background(), c.args("copy-one", fullSource(base), true, nil, t1)); err != nil {
		t.Fatal(err)
	}
	// A restart lost the change list: the next copy is full, with two
	// chunks changed. The caller's from moment is not needed for a full copy.
	next := append([]byte(nil), base...)
	next[0], next[5*chunk] = 0xaa, 0xbb
	res, err := c.sync().SendVMDisk(context.Background(), c.args("copy-two", fullSource(next), true, nil, t2))
	if err != nil {
		t.Fatal(err)
	}
	if res.SentBytes != 2*chunk {
		t.Fatalf("sent %d bytes, want %d (the two chunks that differ)", res.SentBytes, 2*chunk)
	}
	if !bytes.Equal(readDisk(t, c.diskPath()), next) {
		t.Fatal("far-site disk differs from the copy")
	}
}

func TestSendVMDisk_WrongBaseNamesTheDiskMoment(t *testing.T) {
	c := newVMTestClient(t)
	if _, err := c.sync().SendVMDisk(context.Background(), c.args("copy-one", fullSource(patterned(2*chunk)), true, nil, t1)); err != nil {
		t.Fatal(err)
	}
	_, err := c.sync().SendVMDisk(context.Background(), c.args("copy-two", change(2*chunk, 9, []int64{0}, nil), false, ptr(t0), t2))
	var wb *WrongBaseError
	if !errors.As(err, &wb) {
		t.Fatalf("error = %v, want WrongBaseError", err)
	}
	if wb.DiskMoment != t1.UTC().Format(time.RFC3339Nano) {
		t.Fatalf("disk moment = %q", wb.DiskMoment)
	}
}

func TestSendVMDisk_RetryAfterTheCommitSucceeded(t *testing.T) {
	c := newVMTestClient(t)
	if _, err := c.sync().SendVMDisk(context.Background(), c.args("copy-one", fullSource(patterned(2*chunk)), true, nil, t1)); err != nil {
		t.Fatal(err)
	}
	args := c.args("copy-two", change(2*chunk, 9, []int64{0}, nil), false, ptr(t1), t2)
	if _, err := c.sync().SendVMDisk(context.Background(), args); err != nil {
		t.Fatal(err)
	}
	// The Job's pod restarted after the commit: the same call again.
	res, err := c.sync().SendVMDisk(context.Background(), args)
	if err != nil {
		t.Fatalf("repeat of a committed copy: %v", err)
	}
	if !res.AlreadyApplied {
		t.Fatal("expected AlreadyApplied")
	}
	if pts, _ := RestorePoints(c.diskPath()); len(pts) != 1 {
		t.Fatalf("restore points = %d, want 1 (the repeat must not add one)", len(pts))
	}
	// Same for a full copy.
	full := c.args("copy-three", fullSource(patterned(2*chunk)), true, nil, t3)
	if _, err = c.sync().SendVMDisk(context.Background(), full); err != nil {
		t.Fatal(err)
	}
	if res, err = c.sync().SendVMDisk(context.Background(), full); err != nil || !res.AlreadyApplied {
		t.Fatalf("repeat of a committed full copy: %+v %v", res, err)
	}
}

func TestSendVMDisk_ResumesAnUpload(t *testing.T) {
	c := newVMTestClient(t)
	src := change(4*chunk, 3, []int64{0, chunk, 2 * chunk, 3 * chunk}, nil)
	if _, err := c.sync().SendVMDisk(context.Background(), c.args("copy-one", fullSource(patterned(4*chunk)), true, nil, t1)); err != nil {
		t.Fatal(err)
	}
	// An earlier try stored the first two chunks.
	recs := chunks(t, src, false)
	copyID := vmCopyID("copy-two")
	if code := c.upload(copyID, recs[:2], "test-token-123"); code != 200 {
		t.Fatal("first part failed")
	}
	res, err := c.sync().SendVMDisk(context.Background(), c.args("copy-two", src, false, ptr(t1), t2))
	if err != nil {
		t.Fatal(err)
	}
	if res.SentBytes != 2*chunk {
		t.Fatalf("sent %d bytes, want only the missing %d", res.SentBytes, 2*chunk)
	}
	got := readDisk(t, c.diskPath())
	for i := int64(0); i < 4; i++ {
		if got[i*chunk] != 3 {
			t.Fatalf("chunk %d not applied", i)
		}
	}
}

func TestSendVMDisk_ReceiverErrorIsReturned(t *testing.T) {
	c := newVMTestClient(t)
	bad := NewSyncClient(c.srv.URL, "wrong-token")
	_, err := bad.SendVMDisk(context.Background(), c.args("copy-one", fullSource(patterned(chunk)), true, nil, t1))
	if err == nil {
		t.Fatal("expected an error for a wrong token")
	}
	var wb *WrongBaseError
	if errors.As(err, &wb) {
		t.Fatal("a wrong token must not look like a wrong base")
	}
}
