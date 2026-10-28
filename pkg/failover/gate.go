package failover

import (
	"fmt"
	"time"

	v1alpha1 "github.com/IBM/ibm-vpc-file-pool-csi/api/v1alpha1"
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

// FindPlan returns the newest Plan that an Execute may rely on: a Plan for the
// same source and the same rewindTo that finished within PlanMaxAge. Otherwise
// it says what is missing.
func FindPlan(all []v1alpha1.Failover, exec *v1alpha1.Failover, defaultSource string, now time.Time) (*v1alpha1.Failover, error) {
	source := EffectiveSource(exec, defaultSource)
	if source == "" {
		return nil, fmt.Errorf("spec.source is required: no default source is set")
	}
	want := rewindOf(exec)
	var best *v1alpha1.Failover
	var staleSource, wrongRewind bool
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
	for _, v := range p.Status.Volumes {
		e.Volumes = append(e.Volumes, v.SubVolume)
	}
	return e
}
