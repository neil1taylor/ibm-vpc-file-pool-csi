package webhook

import (
	"context"
	"strings"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func rpTime(h int) *metav1.Time {
	t := metav1.NewTime(time.Date(2026, 11, 5, h, 0, 0, 0, time.UTC))
	return &t
}

func rpInt(v int32) *int32 { return &v }

func TestRestorePointFields_BackupRestore(t *testing.T) {
	v := &BackupRestoreValidator{}
	br := validBackupRestore()
	br.Spec.RestorePoint, br.Spec.Moment = rpTime(1), rpTime(2)
	_, err := v.ValidateCreate(context.Background(), br)
	if err == nil || !strings.Contains(err.Error(), "restorePoint") || !strings.Contains(err.Error(), "moment") {
		t.Fatalf("want conflict naming both, got %v", err)
	}
	br = validBackupRestore()
	br.Spec.Moment = rpTime(2)
	w, err := v.ValidateCreate(context.Background(), br)
	if err != nil || len(w) != 1 || !strings.Contains(w[0], "spec.moment is deprecated") {
		t.Fatalf("old only: %v %v", w, err)
	}
	br = validBackupRestore()
	br.Spec.RestorePoint = rpTime(2)
	if w, err = v.ValidateCreate(context.Background(), br); err != nil || len(w) != 0 {
		t.Fatalf("new only: %v %v", w, err)
	}
}

func TestRestorePointFields_BackupFileRestore(t *testing.T) {
	v := &BackupFileRestoreValidator{}
	fr := validFileRestore()
	fr.Spec.RestorePoint, fr.Spec.Moment = rpTime(1), rpTime(2)
	if _, err := v.ValidateCreate(context.Background(), fr); err == nil || !strings.Contains(err.Error(), "restorePoint") || !strings.Contains(err.Error(), "moment") {
		t.Fatalf("want conflict naming both, got %v", err)
	}
	fr = validFileRestore()
	fr.Spec.Moment = rpTime(2)
	w, err := v.ValidateCreate(context.Background(), fr)
	if err != nil || len(w) != 1 || !strings.Contains(w[0], "spec.moment is deprecated") {
		t.Fatalf("old only: %v %v", w, err)
	}
}

func TestRestorePointFields_ReplicationPolicy(t *testing.T) {
	v := &ReplicationPolicyValidator{}
	rp := validReplicationPolicy()
	rp.Spec.VolumeRestorePointKeepHours, rp.Spec.MomentKeepHours = rpInt(1), rpInt(2)
	if _, err := v.ValidateCreate(context.Background(), rp); err == nil || !strings.Contains(err.Error(), "volumeRestorePointKeepHours") || !strings.Contains(err.Error(), "momentKeepHours") {
		t.Fatalf("want conflict naming both, got %v", err)
	}
	rp = validReplicationPolicy()
	rp.Spec.MomentKeepHours = rpInt(2)
	w, err := v.ValidateCreate(context.Background(), rp)
	if err != nil || len(w) != 1 || !strings.Contains(w[0], "momentKeepHours is deprecated") {
		t.Fatalf("old only: %v %v", w, err)
	}
	w, err = v.ValidateUpdate(context.Background(), rp, rp)
	if err != nil || len(w) != 1 {
		t.Fatalf("update warns too: %v %v", w, err)
	}
	rp = validReplicationPolicy()
	rp.Spec.VolumeRestorePointKeepHours = rpInt(-1)
	if _, err := v.ValidateCreate(context.Background(), rp); err == nil || !strings.Contains(err.Error(), "volumeRestorePointKeepHours must be >= 0") {
		t.Fatalf("negative: %v", err)
	}
	rp = validReplicationPolicy()
	rp.Spec.VolumeRestorePointKeepHours = rpInt(0)
	if w, err := v.ValidateCreate(context.Background(), rp); err != nil || len(w) != 0 {
		t.Fatalf("zero: %v %v", w, err)
	}
	rp = validReplicationPolicy()
	rp.Spec.VolumeRestorePointKeepHours = rpInt(5)
	rp.Spec.RsyncOptions = []string{"--inplace"}
	if _, err := v.ValidateCreate(context.Background(), rp); err == nil {
		t.Fatal("in-place flag must be refused with the new field too")
	}
}
