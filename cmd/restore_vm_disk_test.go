package main

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/IBM/ibm-vpc-file-pool-csi/pkg/replication"
)

func TestRestoreVMDiskMessage(t *testing.T) {
	m := time.Date(2026, 10, 21, 9, 0, 0, 0, time.UTC)
	var got struct {
		Moment string `json:"moment"`
		Latest bool   `json:"latest"`
	}
	if err := json.Unmarshal([]byte(restoreVMDiskMessage(replication.RestoreResult{Moment: m, Latest: true})), &got); err != nil {
		t.Fatal(err)
	}
	if got.Moment != "2026-10-21T09:00:00Z" || !got.Latest {
		t.Errorf("message = %+v", got)
	}
	// A moment that is not known is left out, not written as the year 1.
	if err := json.Unmarshal([]byte(restoreVMDiskMessage(replication.RestoreResult{Latest: true})), &got); err != nil {
		t.Fatal(err)
	}
	if got.Moment != "" {
		t.Errorf("an unknown moment was written as %q", got.Moment)
	}
}

func TestParseRestoreMoment(t *testing.T) {
	if got, err := parseRestoreMoment(""); err != nil || !got.IsZero() {
		t.Errorf("empty = %v, %v; want the zero time", got, err)
	}
	if got, err := parseRestoreMoment("2026-10-21T09:00:00Z"); err != nil || !got.Equal(time.Date(2026, 10, 21, 9, 0, 0, 0, time.UTC)) {
		t.Errorf("RFC 3339 = %v, %v", got, err)
	}
	if _, err := parseRestoreMoment("yesterday"); err == nil {
		t.Error("want an error for a time that is not RFC 3339")
	}
}
