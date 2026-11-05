package v1alpha1

import (
	"fmt"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// The old "moment" fields and their restore-point names live side by side for
// one release. Spec fields: read the new one, fall back to the old one.
// Status fields: write both, with the same value.

func effectiveTime(newV, old *metav1.Time) *metav1.Time {
	if newV != nil {
		return newV
	}
	return old
}

// CheckRestorePointFields says what to do about a new and an old spec field
// that mean the same thing. It returns an error if both are set and differ,
// and a deprecation warning if only the old one is set.
func CheckRestorePointFields(newV, old *metav1.Time, newName, oldName string) (string, error) {
	switch {
	case old == nil:
		return "", nil
	case newV != nil && !newV.Equal(old):
		return "", fmt.Errorf("spec.%s and spec.%s are both set and differ; set only spec.%s (spec.%s is deprecated)", newName, oldName, newName, oldName)
	default:
		return fmt.Sprintf("spec.%s is deprecated; use spec.%s", oldName, newName), nil
	}
}

// EffectiveRestorePoint is spec.restorePoint, or spec.moment if only that is set.
func (s *BackupRestoreSpec) EffectiveRestorePoint() *metav1.Time {
	return effectiveTime(s.RestorePoint, s.Moment)
}

// CheckRestorePointFields checks restorePoint against the deprecated moment.
func (s *BackupRestoreSpec) CheckRestorePointFields() (string, error) {
	return CheckRestorePointFields(s.RestorePoint, s.Moment, "restorePoint", "moment")
}

// EffectiveRestorePoint is spec.restorePoint, or spec.moment if only that is set.
func (s *BackupFileRestoreSpec) EffectiveRestorePoint() *metav1.Time {
	return effectiveTime(s.RestorePoint, s.Moment)
}

// CheckRestorePointFields checks restorePoint against the deprecated moment.
func (s *BackupFileRestoreSpec) CheckRestorePointFields() (string, error) {
	return CheckRestorePointFields(s.RestorePoint, s.Moment, "restorePoint", "moment")
}

// EffectiveRestorePoint is spec.restorePoint, or spec.moment if only that is set.
func (s *BackupBrowseSpec) EffectiveRestorePoint() *metav1.Time {
	return effectiveTime(s.RestorePoint, s.Moment)
}

// CheckRestorePointFields checks restorePoint against the deprecated moment.
func (s *BackupBrowseSpec) CheckRestorePointFields() (string, error) {
	return CheckRestorePointFields(s.RestorePoint, s.Moment, "restorePoint", "moment")
}

// EffectiveVolumeRestorePointKeepHours is spec.volumeRestorePointKeepHours, or
// spec.momentKeepHours if only that is set.
func (s *ReplicationPolicySpec) EffectiveVolumeRestorePointKeepHours() *int32 {
	if s.VolumeRestorePointKeepHours != nil {
		return s.VolumeRestorePointKeepHours
	}
	return s.MomentKeepHours
}

// CheckKeepHoursFields checks volumeRestorePointKeepHours against the
// deprecated momentKeepHours.
func (s *ReplicationPolicySpec) CheckKeepHoursFields() (string, error) {
	if s.MomentKeepHours == nil {
		return "", nil
	}
	if s.VolumeRestorePointKeepHours != nil && *s.VolumeRestorePointKeepHours != *s.MomentKeepHours {
		return "", fmt.Errorf("spec.volumeRestorePointKeepHours and spec.momentKeepHours are both set and differ; set only spec.volumeRestorePointKeepHours (spec.momentKeepHours is deprecated)")
	}
	return "spec.momentKeepHours is deprecated; use spec.volumeRestorePointKeepHours", nil
}

func copyTime(t *metav1.Time) *metav1.Time {
	if t == nil {
		return nil
	}
	c := *t
	return &c
}

func copyTimes(ts []metav1.Time) []metav1.Time {
	if ts == nil {
		return nil
	}
	return append([]metav1.Time(nil), ts...)
}

// SetRestoredRestorePoint writes restoredRestorePoint and the old restoredMoment.
func (s *BackupRestoreStatus) SetRestoredRestorePoint(t *metav1.Time) {
	s.RestoredRestorePoint, s.RestoredMoment = copyTime(t), copyTime(t)
}

// SetRestoredRestorePoint writes restoredRestorePoint and the old restoredMoment.
func (s *BackupFileRestoreStatus) SetRestoredRestorePoint(t *metav1.Time) {
	s.RestoredRestorePoint, s.RestoredMoment = copyTime(t), copyTime(t)
}

// SetCopyRestorePoint writes copyRestorePoint and the old copyMoment.
func (s *SnapshotStatus) SetCopyRestorePoint(t *metav1.Time) {
	s.CopyRestorePoint, s.CopyMoment = copyTime(t), copyTime(t)
}

// SetCopyRestorePoint writes copyRestorePoint and the old copyMoment.
func (s *SubVolumeReplicationStatus) SetCopyRestorePoint(t *metav1.Time) {
	s.CopyRestorePoint, s.CopyMoment = copyTime(t), copyTime(t)
}

// SetRestorePointTimes writes restorePointTimes and the old restorePointMoments.
func (s *SubVolumeReplicationStatus) SetRestorePointTimes(ts []metav1.Time) {
	s.RestorePointTimes, s.RestorePointMoments = copyTimes(ts), copyTimes(ts)
}

// SetCopyRestorePoint writes copyRestorePoint and the old copyMoment.
func (s *VMReplicationStatus) SetCopyRestorePoint(t *metav1.Time) {
	s.CopyRestorePoint, s.CopyMoment = copyTime(t), copyTime(t)
}

// SetCommonRestorePoints writes commonRestorePoints and the old commonMoments.
func (s *VMReplicationStatus) SetCommonRestorePoints(ts []metav1.Time) {
	s.CommonRestorePoints, s.CommonMoments = copyTimes(ts), copyTimes(ts)
}

// SetRestorePoint writes restorePoint and the old moment.
func (s *VMCopyState) SetRestorePoint(t metav1.Time) {
	s.RestorePoint, s.Moment = t, t
}

// SetRestorePoint writes restorePoint and the old moment.
func (s *VMCopySet) SetRestorePoint(t metav1.Time) {
	s.RestorePoint, s.Moment = t, t
}

// SetRestorePoint writes restorePoint and the old moment.
func (s *FailoverVM) SetRestorePoint(t metav1.Time) {
	s.RestorePoint, s.Moment = t, t
}

// SetRestorePoints writes restorePoints and the old moments.
func (s *FailoverVM) SetRestorePoints(ts []metav1.Time) {
	s.RestorePoints, s.Moments = copyTimes(ts), copyTimes(ts)
}

// SetRestorePointsTruncated writes restorePointsTruncated and the old momentsTruncated.
func (s *FailoverStatus) SetRestorePointsTruncated(v bool) {
	s.RestorePointsTruncated, s.MomentsTruncated = v, v
}
