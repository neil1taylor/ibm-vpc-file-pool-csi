package failover

import (
	"fmt"
	"strings"
	"time"

	v1alpha1 "github.com/IBM/ibm-vpc-file-pool-csi/api/v1alpha1"
	"github.com/IBM/ibm-vpc-file-pool-csi/pkg/util"
)

// PlanMaxAge is how old a finished Plan may be when an Execute relies on it.
const PlanMaxAge = 15 * time.Minute

// EffectiveSource is the claim a Failover reads: the one the status names, or
// the spec's, or the default.
func EffectiveSource(f *v1alpha1.Failover, defaultSource string) string {
	switch {
	case f.Status.Source != "":
		return f.Status.Source
	case f.Spec.Source != "":
		return f.Spec.Source
	default:
		return defaultSource
	}
}

// BasePathOf returns spec.basePath as a folder under the claim: "" for the
// claim's root (unset or "/"), otherwise an absolute path without a trailing
// slash. It refuses a path that is not safe to append to a mount.
func BasePathOf(f *v1alpha1.Failover) (string, error) {
	bp := f.Spec.BasePath
	if bp == "" || bp == "/" {
		return "", nil
	}
	if !util.ValidBasePath(bp) {
		return "", fmt.Errorf("spec.basePath must be an absolute path of letters, digits, '.', '_', '-' and '/', without '..': %q", bp)
	}
	return strings.TrimRight(bp, "/"), nil
}

func sameRewind(a, b *time.Time) bool {
	if a == nil || b == nil {
		return a == nil && b == nil
	}
	return sameSecond(*a, *b)
}

func rewindOf(f *v1alpha1.Failover) *time.Time {
	if f.Spec.RewindTo == nil {
		return nil
	}
	t := f.Spec.RewindTo.Time
	return &t
}

func rewindText(t *time.Time) string {
	if t == nil {
		return "unset"
	}
	return t.UTC().Format(time.RFC3339)
}

func baseText(b string) string {
	if b == "" {
		return "unset: /"
	}
	return b
}

func vmsText(vms []string) string {
	n := NormalizeVMs(vms)
	if n == nil {
		return "unset: all VMs"
	}
	return strings.Join(n, ", ")
}

// FindPlan returns the newest Plan that an Execute may rely on: a Plan for the
// same source, rewindTo and VM filter that finished within PlanMaxAge. Otherwise
// it says what is missing.
func FindPlan(all []v1alpha1.Failover, exec *v1alpha1.Failover, defaultSource string, now time.Time) (*v1alpha1.Failover, error) {
	source := EffectiveSource(exec, defaultSource)
	if source == "" {
		return nil, fmt.Errorf("spec.source is required: no default source is set")
	}
	want := rewindOf(exec)
	var best *v1alpha1.Failover
	wantBase, _ := BasePathOf(exec)
	var staleSource, wrongRewind, wrongVMs, wrongBase bool
	for i := range all {
		p := &all[i]
		if p.Spec.Action != v1alpha1.FailoverActionPlan || p.Status.Phase != v1alpha1.FailoverPhasePlanned || p.Status.FinishedAt == nil {
			continue
		}
		if EffectiveSource(p, defaultSource) != source {
			continue
		}
		if !sameRewind(rewindOf(p), want) {
			wrongRewind = true
			continue
		}
		if !SameVMs(p.Spec.VMs, exec.Spec.VMs) {
			wrongVMs = true
			continue
		}
		if planBase, _ := BasePathOf(p); planBase != wantBase {
			wrongBase = true
			continue
		}
		if now.Sub(p.Status.FinishedAt.Time) > PlanMaxAge {
			staleSource = true
			continue
		}
		if best == nil || p.Status.FinishedAt.After(best.Status.FinishedAt.Time) {
			best = p
		}
	}
	if best != nil {
		return best, nil
	}
	switch {
	case staleSource:
		return nil, fmt.Errorf("the Plan for source %q is older than %d minutes; create a new Failover with action Plan, check it, and then Execute", source, int(PlanMaxAge.Minutes()))
	case wrongRewind:
		return nil, fmt.Errorf("no Plan for source %q has spec.rewindTo %s; plan with the same rewindTo that you execute", source, rewindText(want))
	case wrongBase:
		return nil, fmt.Errorf("no Plan for source %q has the same spec.basePath (%s); plan with the same basePath that you execute", source, baseText(wantBase))
	case wrongVMs:
		return nil, fmt.Errorf("no Plan for source %q has the same spec.vms (%s); plan with the same VMs that you execute", source, vmsText(exec.Spec.VMs))
	default:
		return nil, fmt.Errorf("no finished Plan for source %q within the last %d minutes; create a Failover with action Plan first", source, int(PlanMaxAge.Minutes()))
	}
}

// ExpectationFromPlan is what an Execute must find on the share: the moment of
// each VM and the volumes that the Plan showed.
func ExpectationFromPlan(p *v1alpha1.Failover) *Expectation {
	e := &Expectation{VMs: map[string]time.Time{}}
	for _, vm := range p.Status.VMs {
		e.VMs[vm.Name] = vm.Moment.Time
	}
	e.Targets = map[string]VolumeTarget{}
	for _, v := range p.Status.Volumes {
		e.Volumes = append(e.Volumes, v.SubVolume)
		e.Targets[v.SubVolume] = VolumeTarget{
			PVCName: v.PVCName, PVCNamespace: v.PVCNamespace, SubPath: v.SubPath,
			UID: v.UID, GID: v.GID, Permissions: v.Permissions,
		}
	}
	return e
}
