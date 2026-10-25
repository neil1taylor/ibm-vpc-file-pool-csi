package webhook

import (
	"context"
	"fmt"
	"math"
	"reflect"

	v1alpha1 "github.com/IBM/ibm-vpc-file-pool-csi/api/v1alpha1"
	"github.com/IBM/ibm-vpc-file-pool-csi/pkg/ibmcloud"
	"k8s.io/klog/v2"
	"sigs.k8s.io/controller-runtime/pkg/webhook/admission"
)

// maxPoolBenchName keeps the run's namespace, "poolbench-<name>", within the
// 63 characters of a label value.
const maxPoolBenchName = 40

// maxPoolBenchVMs is the most VMs one run may make.
const maxPoolBenchVMs = 500

// minBurstSeconds is the shortest burst fio can measure with any meaning.
const minBurstSeconds = 10

// defaultRootDiskGiB is the root disk of a test VM when the spec sets none. A
// copy of a catalog disk must be at least as large as the catalog disk.
const defaultRootDiskGiB = 30

// PoolBenchValidator validates PoolBench create and update requests. Checks
// that need the cluster (free room, images) are made by the PoolBench service,
// which reports them in the run's status.
type PoolBenchValidator struct{}

var _ admission.Validator[*v1alpha1.PoolBench] = &PoolBenchValidator{}

func (v *PoolBenchValidator) ValidateCreate(_ context.Context, pb *v1alpha1.PoolBench) (admission.Warnings, error) {
	klog.V(4).InfoS("Validating PoolBench create", "name", pb.Name)
	if len(pb.Name) > maxPoolBenchName {
		return nil, fmt.Errorf("the name must be at most %d characters, got %d", maxPoolBenchName, len(pb.Name))
	}
	return nil, validatePoolBenchSpec(&pb.Spec)
}

func (v *PoolBenchValidator) ValidateUpdate(_ context.Context, oldPB, pb *v1alpha1.PoolBench) (admission.Warnings, error) {
	klog.V(4).InfoS("Validating PoolBench update", "name", pb.Name)
	if err := validatePoolBenchSpec(&pb.Spec); err != nil {
		return nil, err
	}
	if oldPB.Status.Phase == "" || oldPB.Status.Phase == v1alpha1.PoolBenchPending {
		return nil, nil
	}
	// Once started, only Control may change, and events may only be added.
	if len(pb.Spec.Events) < len(oldPB.Spec.Events) ||
		!reflect.DeepEqual(pb.Spec.Events[:len(oldPB.Spec.Events)], oldPB.Spec.Events) {
		return nil, fmt.Errorf("spec.events: the run has started, so events may only be added")
	}
	oldSpec, newSpec := oldPB.Spec, pb.Spec
	oldSpec.Control, newSpec.Control = v1alpha1.PoolBenchControl{}, v1alpha1.PoolBenchControl{}
	oldSpec.Events, newSpec.Events = nil, nil
	if !reflect.DeepEqual(oldSpec, newSpec) {
		return nil, fmt.Errorf("the run has started, so only spec.control and spec.events may change")
	}
	return nil, nil
}

func (v *PoolBenchValidator) ValidateDelete(_ context.Context, _ *v1alpha1.PoolBench) (admission.Warnings, error) {
	return nil, nil
}

func validatePoolBenchSpec(spec *v1alpha1.PoolBenchSpec) error {
	if spec.Profile != "" && spec.Profile != "dp2" {
		return fmt.Errorf("spec.profile must be dp2, got %q", spec.Profile)
	}
	if _, _, ok := ibmcloud.ShareIOPSRange("dp2", spec.ShareSizeGB); !ok {
		return fmt.Errorf("spec.shareSizeGB must be from 10 to 32000 GB for a dp2 share, got %d", spec.ShareSizeGB)
	}
	if err := ibmcloud.ValidateShareIOPS("dp2", spec.ShareSizeGB, spec.IOPS); err != nil {
		return fmt.Errorf("spec.iops: %v", err)
	}
	if spec.StepVMs < 1 {
		return fmt.Errorf("spec.stepVMs must be at least 1, got %d", spec.StepVMs)
	}
	if spec.MaxVMs < spec.StepVMs || spec.MaxVMs%spec.StepVMs != 0 {
		return fmt.Errorf("spec.maxVMs must be a whole number of steps of %d VMs, got %d", spec.StepVMs, spec.MaxVMs)
	}
	if spec.MaxVMs > maxPoolBenchVMs {
		return fmt.Errorf("spec.maxVMs must be at most %d, got %d", maxPoolBenchVMs, spec.MaxVMs)
	}
	if spec.WindowsEvery != nil && *spec.WindowsEvery < 0 {
		return fmt.Errorf("spec.windowsEvery must be 0 (Linux only) or more, got %d", *spec.WindowsEvery)
	}
	if spec.SettleMinutes != nil && *spec.SettleMinutes < 0 {
		return fmt.Errorf("spec.settleMinutes must be 0 or more, got %d", *spec.SettleMinutes)
	}
	if err := validatePoolBenchLoad(&spec.Load); err != nil {
		return err
	}
	if int64(spec.MeasureMinutes)*60 < int64(spec.Load.PeriodSeconds) {
		return fmt.Errorf("spec.measureMinutes must cover at least one load period of %d seconds, got %d minutes",
			spec.Load.PeriodSeconds, spec.MeasureMinutes)
	}
	if err := validatePoolBenchKnee(&spec.Knee); err != nil {
		return err
	}
	for i, e := range spec.Events {
		if err := validatePoolBenchEvent(spec, e); err != nil {
			return fmt.Errorf("spec.events[%d]: %v", i, err)
		}
	}
	if spec.Control.ExtendMinutes < 0 {
		return fmt.Errorf("spec.control.extendMinutes must be 0 or more, got %d", spec.Control.ExtendMinutes)
	}
	return validatePoolBenchDisks(spec)
}

func validatePoolBenchLoad(l *v1alpha1.PoolBenchLoad) error {
	if l.SteadyIOPS < 1 {
		return fmt.Errorf("spec.load.steadyIOPS must be at least 1, got %d", l.SteadyIOPS)
	}
	if l.BurstIOPS < l.SteadyIOPS {
		return fmt.Errorf("spec.load.burstIOPS must be at least the steady rate of %d, got %d", l.SteadyIOPS, l.BurstIOPS)
	}
	if l.BurstSeconds < minBurstSeconds || l.BurstSeconds >= l.PeriodSeconds {
		return fmt.Errorf("spec.load.burstSeconds must be from %d seconds to less than the period of %d, got %d",
			minBurstSeconds, l.PeriodSeconds, l.BurstSeconds)
	}
	if l.ReadPercent != nil && (*l.ReadPercent < 0 || *l.ReadPercent > 100) {
		return fmt.Errorf("spec.load.readPercent must be from 0 to 100, got %d", *l.ReadPercent)
	}
	if _, err := v1alpha1.ParseBlockMix(l.BlockMix); err != nil {
		return fmt.Errorf("spec.load.blockMix: %v", err)
	}
	if l.AreaGiB < 1 {
		return fmt.Errorf("spec.load.areaGiB must be at least 1, got %d", l.AreaGiB)
	}
	return nil
}

func validatePoolBenchKnee(k *v1alpha1.PoolBenchKnee) error {
	if k.LatencyLimitMs < 1 {
		return fmt.Errorf("spec.knee.latencyLimitMs must be at least 1, got %d", k.LatencyLimitMs)
	}
	if k.SlowVMPercent < 1 || k.SlowVMPercent > 100 {
		return fmt.Errorf("spec.knee.slowVMPercent must be from 1 to 100, got %d", k.SlowVMPercent)
	}
	if k.StallSeconds < 1 {
		return fmt.Errorf("spec.knee.stallSeconds must be at least 1, got %d", k.StallSeconds)
	}
	return nil
}

func validatePoolBenchEvent(spec *v1alpha1.PoolBenchSpec, e v1alpha1.PoolBenchEvent) error {
	switch e.Type {
	case v1alpha1.PoolBenchEventBootStorm, v1alpha1.PoolBenchEventPatchWindow, v1alpha1.PoolBenchEventMigration,
		v1alpha1.PoolBenchEventSnapshot, v1alpha1.PoolBenchEventBackup:
	default:
		return fmt.Errorf("type %q is not BootStorm, PatchWindow, Migration, Snapshot or Backup", e.Type)
	}
	if e.AtVMs < spec.StepVMs || e.AtVMs > spec.MaxVMs || e.AtVMs%spec.StepVMs != 0 {
		return fmt.Errorf("atVMs must be a step of the run (a multiple of %d up to %d), got %d", spec.StepVMs, spec.MaxVMs, e.AtVMs)
	}
	return nil
}

// validatePoolBenchDisks refuses a share that cannot hold the disks of the
// largest step: each VM has a root disk and a load disk one GiB larger than
// its test area.
func validatePoolBenchDisks(spec *v1alpha1.PoolBenchSpec) error {
	windows := int64(0)
	if spec.WindowsEvery != nil && *spec.WindowsEvery > 0 {
		windows = int64(spec.MaxVMs / *spec.WindowsEvery)
	}
	linux := int64(spec.MaxVMs) - windows
	load := int64(spec.Load.AreaGiB) + 1
	gib := linux*(rootDiskGiB(spec.Linux)+load) + windows*(rootDiskGiB(spec.Windows)+load)
	needGB := int64(math.Ceil(float64(gib) * (1 << 30) / 1e9))
	if needGB > spec.ShareSizeGB {
		return fmt.Errorf("the disks of %d VMs need %d GB and do not fit a %d GB share", spec.MaxVMs, needGB, spec.ShareSizeGB)
	}
	return nil
}

func rootDiskGiB(vm v1alpha1.PoolBenchVM) int64 {
	if vm.RootDiskGiB > 0 {
		return int64(vm.RootDiskGiB)
	}
	return defaultRootDiskGiB
}
