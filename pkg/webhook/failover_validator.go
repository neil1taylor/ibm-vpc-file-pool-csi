package webhook

import (
	"context"
	"fmt"
	"reflect"
	"time"

	v1alpha1 "github.com/IBM/ibm-vpc-file-pool-csi/api/v1alpha1"
	"github.com/IBM/ibm-vpc-file-pool-csi/pkg/failover"
	"k8s.io/apimachinery/pkg/util/validation"
	"k8s.io/klog/v2"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/webhook/admission"
)

// FailoverValidator validates Failover create and update requests. An Execute
// moves production VMs, so it is refused unless a Plan for the same source,
// rewindTo and VM filter finished in the last 15 minutes, and refused for a VM
// that was failed over on this cluster already. The Job that runs the Execute checks the plan
// against the share again, because only the Job can read the share.
type FailoverValidator struct {
	// Reader reads Failovers and SubVolumes without a cache, so a Plan that
	// has just finished is seen.
	Reader client.Reader
	// DefaultSource is the claim a Failover reads when it names none.
	DefaultSource string
	// Now is the current time; nil means time.Now.
	Now func() time.Time
}

var _ admission.Validator[*v1alpha1.Failover] = &FailoverValidator{}

func (v *FailoverValidator) now() time.Time {
	if v.Now != nil {
		return v.Now()
	}
	return time.Now()
}

func (v *FailoverValidator) ValidateCreate(ctx context.Context, f *v1alpha1.Failover) (admission.Warnings, error) {
	klog.V(4).InfoS("Validating Failover create", "name", f.Name)
	if err := v.validateSpec(&f.Spec); err != nil {
		return nil, err
	}
	if f.Spec.Action != v1alpha1.FailoverActionExecute {
		return nil, nil
	}
	if f.Spec.DRPool == "" {
		return nil, fmt.Errorf("spec.drPool is required for Execute")
	}
	plans := &v1alpha1.FailoverList{}
	if err := v.Reader.List(ctx, plans); err != nil {
		return nil, fmt.Errorf("list Failovers to find the Plan: %w", err)
	}
	plan, err := failover.FindPlan(plans.Items, f, v.DefaultSource, v.now())
	if err != nil {
		return nil, err
	}
	if err = failover.AlreadyFailedOver(ctx, v.Reader, plan); err != nil {
		return nil, err
	}
	return nil, nil
}

// ValidateUpdate refuses a change to the spec: a Job may be running for the
// first request already. Status changes pass.
func (v *FailoverValidator) ValidateUpdate(_ context.Context, old, f *v1alpha1.Failover) (admission.Warnings, error) {
	if !reflect.DeepEqual(old.Spec, f.Spec) {
		return nil, fmt.Errorf("the spec of a Failover cannot change; make a new one")
	}
	return nil, nil
}

func (v *FailoverValidator) ValidateDelete(_ context.Context, _ *v1alpha1.Failover) (admission.Warnings, error) {
	return nil, nil
}

func (v *FailoverValidator) validateSpec(spec *v1alpha1.FailoverSpec) error {
	if spec.Action != v1alpha1.FailoverActionPlan && spec.Action != v1alpha1.FailoverActionExecute {
		return fmt.Errorf("spec.action must be %s or %s, got %q", v1alpha1.FailoverActionPlan, v1alpha1.FailoverActionExecute, spec.Action)
	}
	if spec.Source == "" && v.DefaultSource == "" {
		return fmt.Errorf("spec.source is required: no default source is set (replicationReceiver.pvcName)")
	}
	if spec.Source != "" {
		if errs := validation.IsDNS1123Subdomain(spec.Source); len(errs) > 0 {
			return fmt.Errorf("spec.source is not a claim name: %v", errs)
		}
	}
	for _, vm := range spec.VMs {
		if err := failover.ValidateVMName(vm); err != nil {
			return fmt.Errorf("spec.vms: %w", err)
		}
	}
	if spec.Namespace != "" {
		if errs := validation.IsDNS1123Label(spec.Namespace); len(errs) > 0 {
			return fmt.Errorf("spec.namespace is not a namespace name: %v", errs)
		}
	}
	return nil
}
