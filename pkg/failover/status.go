package failover

import (
	"context"
	"fmt"

	v1alpha1 "github.com/IBM/ibm-vpc-file-pool-csi/api/v1alpha1"
	corev1 "k8s.io/api/core/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// FailoverStatus describes the state of a single failed-over SubVolume.
type FailoverStatus struct {
	SubVolumeName string
	PVCName       string
	PVCNamespace  string
	PVCPhase      string
	PolicyName    string
}

// StatusReporter queries the DR cluster for SubVolumes created by failover.
type StatusReporter struct {
	client client.Client
}

// NewStatusReporter creates a new status reporter.
func NewStatusReporter(c client.Client) *StatusReporter {
	return &StatusReporter{client: c}
}

// Report lists all SubVolumes with the failover-source label and checks
// the phase of associated PVCs.
func (s *StatusReporter) Report(ctx context.Context) ([]FailoverStatus, error) {
	// List SubVolumes with failover label.
	var svList v1alpha1.SubVolumeList
	if err := s.client.List(ctx, &svList, client.HasLabels{FailoverSourceLabel}); err != nil {
		return nil, fmt.Errorf("list failover SubVolumes: %w", err)
	}

	var statuses []FailoverStatus
	for _, sv := range svList.Items {
		fs := FailoverStatus{
			SubVolumeName: sv.Name,
			PVCName:       sv.Spec.PVCName,
			PVCNamespace:  sv.Spec.PVCNamespace,
			PolicyName:    sv.Labels[FailoverSourceLabel],
		}

		// Check PVC phase.
		var pvc corev1.PersistentVolumeClaim
		key := client.ObjectKey{Namespace: sv.Spec.PVCNamespace, Name: sv.Spec.PVCName}
		if err := s.client.Get(ctx, key, &pvc); err != nil {
			fs.PVCPhase = "NotFound"
		} else {
			fs.PVCPhase = string(pvc.Status.Phase)
		}

		statuses = append(statuses, fs)
	}

	return statuses, nil
}
