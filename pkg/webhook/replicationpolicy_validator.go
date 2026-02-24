package webhook

import (
	"context"
	"fmt"
	"strings"
	"time"

	v1alpha1 "github.com/IBM/ibm-vpc-file-pool-csi/api/v1alpha1"
	"k8s.io/klog/v2"
	"sigs.k8s.io/controller-runtime/pkg/webhook/admission"
)

// ReplicationPolicyValidator validates ReplicationPolicy create and update requests.
type ReplicationPolicyValidator struct{}

var _ admission.Validator[*v1alpha1.ReplicationPolicy] = &ReplicationPolicyValidator{}

func (v *ReplicationPolicyValidator) ValidateCreate(_ context.Context, rp *v1alpha1.ReplicationPolicy) (admission.Warnings, error) {
	klog.V(4).InfoS("Validating ReplicationPolicy create", "name", rp.Name)
	return nil, validateReplicationPolicySpec(&rp.Spec)
}

func (v *ReplicationPolicyValidator) ValidateUpdate(_ context.Context, _, rp *v1alpha1.ReplicationPolicy) (admission.Warnings, error) {
	klog.V(4).InfoS("Validating ReplicationPolicy update", "name", rp.Name)
	return nil, validateReplicationPolicySpec(&rp.Spec)
}

func (v *ReplicationPolicyValidator) ValidateDelete(_ context.Context, _ *v1alpha1.ReplicationPolicy) (admission.Warnings, error) {
	return nil, nil
}

// rsyncBlocklist contains rsync flags that are rejected for security reasons.
var rsyncBlocklist = []string{"--daemon", "--server", "--rsh", "--rsync-path"}

func validateReplicationPolicySpec(spec *v1alpha1.ReplicationPolicySpec) error {
	if spec.SourcePoolName == "" {
		return fmt.Errorf("spec.sourcePoolName is required")
	}
	if spec.DestinationNFSServer == "" {
		return fmt.Errorf("spec.destinationNFSServer is required")
	}
	if spec.DestinationBasePath == "" {
		return fmt.Errorf("spec.destinationBasePath is required")
	}
	if spec.Schedule == "" {
		return fmt.Errorf("spec.schedule is required")
	}
	if _, err := time.ParseDuration(spec.Schedule); err != nil {
		return fmt.Errorf("spec.schedule is not a valid duration: %v", err)
	}
	if spec.MaxRetries < 0 {
		return fmt.Errorf("spec.maxRetries must be >= 0, got %d", spec.MaxRetries)
	}
	if spec.BandwidthLimitMbps < 0 {
		return fmt.Errorf("spec.bandwidthLimitMbps must be >= 0, got %d", spec.BandwidthLimitMbps)
	}
	if spec.MaxParallelSyncs < 0 {
		return fmt.Errorf("spec.maxParallelSyncs must be >= 0, got %d", spec.MaxParallelSyncs)
	}
	for _, opt := range spec.RsyncOptions {
		for _, blocked := range rsyncBlocklist {
			if opt == blocked || strings.HasPrefix(opt, blocked+"=") {
				return fmt.Errorf("spec.rsyncOptions contains disallowed flag %q", opt)
			}
		}
	}
	return nil
}
