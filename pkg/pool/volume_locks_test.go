package pool

import (
	"testing"
	"time"
)

func TestVolumeLocks_OneVolumeAtATimeOthersFree(t *testing.T) {
	var v volumeLocks
	unlockA := v.lock("pvc-a")

	other := make(chan struct{})
	go func() { v.lock("pvc-b")(); close(other) }()
	select {
	case <-other:
	case <-time.After(time.Second):
		t.Fatal("another volume waited")
	}

	same := make(chan struct{})
	go func() { v.lock("pvc-a")(); close(same) }()
	select {
	case <-same:
		t.Fatal("the same volume did not wait")
	case <-time.After(100 * time.Millisecond):
	}
	unlockA()
	select {
	case <-same:
	case <-time.After(time.Second):
		t.Fatal("the same volume never got the lock")
	}
	if len(v.locks) != 0 {
		t.Errorf("%d locks left after all were unlocked", len(v.locks))
	}
}
