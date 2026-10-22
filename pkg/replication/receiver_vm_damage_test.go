package replication

import (
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"testing"
)

// A receiver that is killed while it writes the upload record leaves a record
// that is cut off, or damaged. Every retry then failed with "the upload record
// file is damaged" (#213). The receiver repairs the record: it keeps the
// entries it can trust, and the client sends the rest again.

const damageCopyID = "dddddddd-1"

func (c *vmTestClient) manifestPath(copyID string) string {
	return filepath.Join(filepath.Dir(c.diskPath()), incomingDirName, copyID, manifestName)
}

func (c *vmTestClient) storedOffsets(copyID string) []int64 {
	c.t.Helper()
	code, body := c.do(http.MethodGet, c.url(copyID, ""), nil, "test-token-123")
	if code != http.StatusOK {
		c.t.Fatalf("status = %d: %s", code, body)
	}
	var st StatusResponse
	if err := json.Unmarshal(body, &st); err != nil {
		c.t.Fatal(err)
	}
	return st.Offsets
}

func appendBytes(t *testing.T, path string, b []byte) {
	t.Helper()
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_APPEND, 0)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = f.Write(b); err != nil {
		t.Fatal(err)
	}
	if err = f.Close(); err != nil {
		t.Fatal(err)
	}
}

// fullCopy is a first copy of 4 data chunks.
func fullCopy() (CopySource, []UploadRecord) {
	src := change(4*chunk, 7, []int64{0, chunk, 2 * chunk, 3 * chunk}, nil)
	var recs []UploadRecord
	for _, o := range []int64{0, chunk, 2 * chunk, 3 * chunk} {
		recs = append(recs, UploadRecord{Off: o, Data: src.data[o]})
	}
	return src, recs
}

func (c *vmTestClient) finishCopy(copyID string, src CopySource, recs []UploadRecord) {
	c.t.Helper()
	code, _ := c.commit(copyID, CommitRequest{Size: src.Size(), To: t1, Full: true, Digest: digestOf(recs), VMGroup: "ns/vm1"})
	if code != http.StatusOK {
		c.t.Fatalf("commit = %d: %s", code, c.lastBody)
	}
	if got := readDisk(c.t, c.diskPath()); len(got) != int(src.Size()) || got[0] != 7 || got[3*chunk] != 7 {
		c.t.Fatal("the disk does not hold the copy")
	}
}

// The receiver stopped in the middle of an entry. The next records must not be
// written after the cut-off bytes, or every later entry is read out of step.
func TestReceiverVM_CutOffRecordDoesNotSpoilTheNextTry(t *testing.T) {
	c := newVMTestClient(t)
	src, recs := fullCopy()
	if code := c.upload(damageCopyID, recs[:2], "test-token-123"); code != http.StatusOK {
		t.Fatal("first part failed")
	}
	appendBytes(t, c.manifestPath(damageCopyID), []byte{0, 1, 2, 3, 4, 5, 6, 7, 8, 9})
	if got := c.storedOffsets(damageCopyID); len(got) != 2 {
		t.Fatalf("offsets after the cut = %v, want 2", got)
	}
	if code := c.upload(damageCopyID, recs[2:], "test-token-123"); code != http.StatusOK {
		t.Fatal("second part failed")
	}
	if got := c.storedOffsets(damageCopyID); len(got) != 4 {
		t.Fatalf("offsets after the second part = %v, want 4", got)
	}
	c.finishCopy(damageCopyID, src, recs)
}

// A damaged entry: the receiver keeps the entries before it and drops the rest.
func TestReceiverVM_DamagedRecordKeepsTheGoodEntriesBeforeIt(t *testing.T) {
	c := newVMTestClient(t)
	src, recs := fullCopy()
	if code := c.upload(damageCopyID, recs[:3], "test-token-123"); code != http.StatusOK {
		t.Fatal("upload failed")
	}
	raw, err := os.ReadFile(c.manifestPath(damageCopyID))
	if err != nil {
		t.Fatal(err)
	}
	raw[manifestEntrySize] = 9 // the kind of the second entry
	if err = os.WriteFile(c.manifestPath(damageCopyID), raw, 0o660); err != nil {
		t.Fatal(err)
	}
	got := c.storedOffsets(damageCopyID)
	if len(got) != 1 || got[0] != 0 {
		t.Fatalf("offsets = %v, want only the entry before the damage", got)
	}
	if code := c.upload(damageCopyID, recs[1:], "test-token-123"); code != http.StatusOK {
		t.Fatal("upload of the rest failed")
	}
	c.finishCopy(damageCopyID, src, recs)
}

// A cut-off that left zeros (a file that grew but was never written) is damage too.
func TestReceiverVM_ZeroFilledTailIsDamage(t *testing.T) {
	c := newVMTestClient(t)
	src, recs := fullCopy()
	if code := c.upload(damageCopyID, recs[:2], "test-token-123"); code != http.StatusOK {
		t.Fatal("upload failed")
	}
	appendBytes(t, c.manifestPath(damageCopyID), make([]byte, 3*manifestEntrySize))
	if got := c.storedOffsets(damageCopyID); len(got) != 2 {
		t.Fatalf("offsets = %v, want the 2 good entries", got)
	}
	if code := c.upload(damageCopyID, recs[2:], "test-token-123"); code != http.StatusOK {
		t.Fatal("upload of the rest failed")
	}
	c.finishCopy(damageCopyID, src, recs)
}

// Damage that the entry check cannot see (an entry that looks right but names
// the wrong data) is caught by the digest. The upload is then discarded, so the
// next try starts clean and does not fail the same way for ever.
func TestReceiverVM_WrongDigestDiscardsTheUpload(t *testing.T) {
	c := newVMTestClient(t)
	src, recs := fullCopy()
	if code := c.upload(damageCopyID, recs, "test-token-123"); code != http.StatusOK {
		t.Fatal("upload failed")
	}
	code, _ := c.commit(damageCopyID, CommitRequest{Size: src.Size(), To: t1, Full: true, Digest: "00", VMGroup: "ns/vm1"})
	if code != http.StatusBadRequest {
		t.Fatalf("commit with a wrong digest = %d, want 400", code)
	}
	if got := c.storedOffsets(damageCopyID); len(got) != 0 {
		t.Fatalf("the upload was not discarded: %v", got)
	}
	if code = c.upload(damageCopyID, recs, "test-token-123"); code != http.StatusOK {
		t.Fatal("second upload failed")
	}
	c.finishCopy(damageCopyID, src, recs)
}
