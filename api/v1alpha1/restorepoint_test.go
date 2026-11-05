package v1alpha1

import (
	"strings"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func tp(h int) *metav1.Time {
	t := metav1.NewTime(time.Date(2026, 11, 5, h, 0, 0, 0, time.UTC))
	return &t
}

func i32(v int32) *int32 { return &v }

func TestEffectiveRestorePoint(t *testing.T) {
	tests := []struct {
		name      string
		newV, old *metav1.Time
		want      *metav1.Time
	}{
		{"none", nil, nil, nil},
		{"only old", nil, tp(1), tp(1)},
		{"only new", tp(2), nil, tp(2)},
		{"both", tp(2), tp(1), tp(2)},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			specs := map[string]*metav1.Time{
				"restore":     (&BackupRestoreSpec{RestorePoint: tt.newV, Moment: tt.old}).EffectiveRestorePoint(),
				"filerestore": (&BackupFileRestoreSpec{RestorePoint: tt.newV, Moment: tt.old}).EffectiveRestorePoint(),
				"browse":      (&BackupBrowseSpec{RestorePoint: tt.newV, Moment: tt.old}).EffectiveRestorePoint(),
			}
			for n, got := range specs {
				if (got == nil) != (tt.want == nil) || (got != nil && !got.Equal(tt.want)) {
					t.Errorf("%s: got %v want %v", n, got, tt.want)
				}
			}
		})
	}
}

func TestEffectiveVolumeRestorePointKeepHours(t *testing.T) {
	if (&ReplicationPolicySpec{}).EffectiveVolumeRestorePointKeepHours() != nil {
		t.Fatal("want nil")
	}
	if got := (&ReplicationPolicySpec{MomentKeepHours: i32(3)}).EffectiveVolumeRestorePointKeepHours(); got == nil || *got != 3 {
		t.Fatalf("old only: %v", got)
	}
	if got := (&ReplicationPolicySpec{VolumeRestorePointKeepHours: i32(5), MomentKeepHours: i32(3)}).EffectiveVolumeRestorePointKeepHours(); got == nil || *got != 5 {
		t.Fatalf("new wins: %v", got)
	}
}

func TestCheckRestorePointFields(t *testing.T) {
	_, err := CheckRestorePointFields(tp(1), tp(2), "restorePoint", "moment")
	if err == nil || !strings.Contains(err.Error(), "restorePoint") || !strings.Contains(err.Error(), "moment") {
		t.Fatalf("want conflict naming both, got %v", err)
	}
	if w, err := CheckRestorePointFields(tp(1), tp(1), "restorePoint", "moment"); err != nil || w == "" {
		t.Fatalf("equal: want warning and no error, got %q %v", w, err)
	}
	if w, err := CheckRestorePointFields(nil, tp(1), "restorePoint", "moment"); err != nil || !strings.Contains(w, "moment is deprecated") || !strings.Contains(w, "restorePoint") {
		t.Fatalf("old only: %q %v", w, err)
	}
	if w, err := CheckRestorePointFields(tp(1), nil, "restorePoint", "moment"); err != nil || w != "" {
		t.Fatalf("new only: %q %v", w, err)
	}
	if w, err := CheckRestorePointFields(nil, nil, "restorePoint", "moment"); err != nil || w != "" {
		t.Fatalf("none: %q %v", w, err)
	}
}

func TestCheckKeepHours(t *testing.T) {
	if _, err := (&ReplicationPolicySpec{VolumeRestorePointKeepHours: i32(1), MomentKeepHours: i32(2)}).CheckKeepHoursFields(); err == nil {
		t.Fatal("want conflict")
	}
	if w, _ := (&ReplicationPolicySpec{MomentKeepHours: i32(2)}).CheckKeepHoursFields(); !strings.Contains(w, "momentKeepHours is deprecated") {
		t.Fatalf("warning: %q", w)
	}
}

func TestStatusSettersWriteBoth(t *testing.T) {
	br := BackupRestoreStatus{}
	br.SetRestoredRestorePoint(tp(1))
	if br.RestoredRestorePoint == nil || br.RestoredMoment == nil || !br.RestoredMoment.Equal(br.RestoredRestorePoint) {
		t.Fatal("restore")
	}
	br.SetRestoredRestorePoint(nil)
	if br.RestoredRestorePoint != nil || br.RestoredMoment != nil {
		t.Fatal("restore clear")
	}
	fr := BackupFileRestoreStatus{}
	fr.SetRestoredRestorePoint(tp(1))
	if fr.RestoredRestorePoint == nil || fr.RestoredMoment == nil {
		t.Fatal("filerestore")
	}
	s := SnapshotStatus{}
	s.SetCopyRestorePoint(tp(1))
	if s.CopyRestorePoint == nil || s.CopyMoment == nil {
		t.Fatal("snapshot")
	}
	d := SubVolumeReplicationStatus{}
	d.SetCopyRestorePoint(tp(1))
	d.SetRestorePointTimes([]metav1.Time{*tp(1), *tp(2)})
	if d.CopyRestorePoint == nil || d.CopyMoment == nil || len(d.RestorePointTimes) != 2 || len(d.RestorePointMoments) != 2 {
		t.Fatal("disk status")
	}
	v := VMReplicationStatus{}
	v.SetCopyRestorePoint(tp(1))
	v.SetCommonRestorePoints([]metav1.Time{*tp(2)})
	if v.CopyRestorePoint == nil || v.CopyMoment == nil || len(v.CommonRestorePoints) != 1 || len(v.CommonMoments) != 1 {
		t.Fatal("vm status")
	}
	fv := FailoverVM{}
	fv.SetRestorePoint(*tp(1))
	fv.SetRestorePoints([]metav1.Time{*tp(2)})
	if fv.RestorePoint.IsZero() || fv.Moment.IsZero() || len(fv.RestorePoints) != 1 || len(fv.Moments) != 1 {
		t.Fatal("failover vm")
	}
	fs := FailoverStatus{}
	fs.SetRestorePointsTruncated(true)
	if !fs.RestorePointsTruncated || !fs.MomentsTruncated {
		t.Fatal("truncated")
	}
	c := VMCopyState{}
	c.SetRestorePoint(*tp(1))
	if c.RestorePoint.IsZero() || c.Moment.IsZero() {
		t.Fatal("vmcopystate")
	}
	cs := VMCopySet{}
	cs.SetRestorePoint(*tp(1))
	if cs.RestorePoint.IsZero() || cs.Moment.IsZero() {
		t.Fatal("vmcopyset")
	}
}
