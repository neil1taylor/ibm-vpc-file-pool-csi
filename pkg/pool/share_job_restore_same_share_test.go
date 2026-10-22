package pool

import (
	"context"
	"strings"
	"testing"
	"time"
)

// A restore from COS reads its scratch copy from the share it writes to. The
// Job must mount that share once, not twice.
func TestRestoreBackup_SameShareIsMountedOnce(t *testing.T) {
	r, c := newShareJobTestRunner()
	r.SetDriverImage("registry.example/driver:test")
	dstPath := "/pvcs/" + testTargetVolume
	if _, _, err := r.RestoreBackup(context.Background(), "rs-1", testShareA, testBackupPath, testMomentsPath, testShareA, dstPath, time.Time{}, nil); err != nil {
		t.Fatal(err)
	}
	job := getShareJob(t, c, "rs-1")
	claims := 0
	for _, v := range job.Spec.Template.Spec.Volumes {
		if v.PersistentVolumeClaim != nil {
			claims++
		}
	}
	if claims != 1 {
		t.Errorf("the Job mounts %d shares, want 1", claims)
	}
	script := jobScript(job)
	if !strings.Contains(script, `dst="/share0`+dstPath+`"`) || strings.Contains(script, "/share1") {
		t.Errorf("the script must write to /share0 when both are one share:\n%s", script)
	}
}

func TestRestoreBackup_TwoSharesStillUseTwoMounts(t *testing.T) {
	r, c := newShareJobTestRunner()
	r.SetDriverImage("registry.example/driver:test")
	dstPath := "/pvcs/" + testTargetVolume
	if _, _, err := r.RestoreBackup(context.Background(), "rs-2", testShareA, testBackupPath, testMomentsPath, testShareB, dstPath, time.Time{}, nil); err != nil {
		t.Fatal(err)
	}
	if script := jobScript(getShareJob(t, c, "rs-2")); !strings.Contains(script, `dst="/share1`+dstPath+`"`) {
		t.Errorf("the script must write to /share1:\n%s", script)
	}
}
