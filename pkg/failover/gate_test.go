package failover

import (
	"strings"
	"testing"
	"time"

	v1alpha1 "github.com/IBM/ibm-vpc-file-pool-csi/api/v1alpha1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func planned(name, source string, finished time.Time, rewind *time.Time) v1alpha1.Failover {
	f := v1alpha1.Failover{
		ObjectMeta: metav1.ObjectMeta{Name: name},
		Spec:       v1alpha1.FailoverSpec{Source: source, Action: v1alpha1.FailoverActionPlan},
		Status: v1alpha1.FailoverStatus{
			Phase:      v1alpha1.FailoverPhasePlanned,
			Source:     source,
			FinishedAt: &metav1.Time{Time: finished},
			VMs: []v1alpha1.FailoverVM{{
				Name: "ns/vm", Moment: metav1.NewTime(m2),
				Disks: []v1alpha1.FailoverDisk{{SubVolume: "pvc-a"}},
			}},
			Volumes: []v1alpha1.FailoverVolume{{SubVolume: "pvc-a"}, {SubVolume: "pvc-plain"}},
		},
	}
	if rewind != nil {
		f.Spec.RewindTo = &metav1.Time{Time: *rewind}
	}
	return f
}

func execFor(source string, rewind *time.Time) *v1alpha1.Failover {
	f := &v1alpha1.Failover{
		ObjectMeta: metav1.ObjectMeta{Name: "go"},
		Spec:       v1alpha1.FailoverSpec{Source: source, Action: v1alpha1.FailoverActionExecute},
	}
	if rewind != nil {
		f.Spec.RewindTo = &metav1.Time{Time: *rewind}
	}
	return f
}

func TestFindPlan_UsesTheNewestFreshPlanForTheSameSource(t *testing.T) {
	now := m3
	plans := []v1alpha1.Failover{
		planned("old", "recv", now.Add(-10*time.Minute), nil),
		planned("new", "recv", now.Add(-2*time.Minute), nil),
		planned("other", "other-claim", now.Add(-1*time.Minute), nil),
	}
	got, err := FindPlan(plans, execFor("recv", nil), "", now)
	if err != nil {
		t.Fatal(err)
	}
	if got.Name != "new" {
		t.Errorf("plan = %s, want new", got.Name)
	}
}

func TestFindPlan_RefusesAPlanOlderThan15Minutes(t *testing.T) {
	plans := []v1alpha1.Failover{planned("old", "recv", m3.Add(-16*time.Minute), nil)}
	_, err := FindPlan(plans, execFor("recv", nil), "", m3)
	if err == nil || !strings.Contains(err.Error(), "15 minutes") {
		t.Fatalf("err = %v, want a refusal that names the 15 minute limit", err)
	}
}

func TestFindPlan_RefusesAPlanThatIsNotFinished(t *testing.T) {
	p := planned("p", "recv", m3.Add(-time.Minute), nil)
	p.Status.Phase = v1alpha1.FailoverPhasePlanning
	p.Status.FinishedAt = nil
	if _, err := FindPlan([]v1alpha1.Failover{p}, execFor("recv", nil), "", m3); err == nil {
		t.Fatal("expected an error")
	}
}

func TestFindPlan_RefusesAPlanForAnotherRewindTime(t *testing.T) {
	at := m1
	plans := []v1alpha1.Failover{planned("p", "recv", m3.Add(-time.Minute), &at)}
	_, err := FindPlan(plans, execFor("recv", nil), "", m3)
	if err == nil || !strings.Contains(err.Error(), "rewindTo") {
		t.Fatalf("err = %v, want a refusal that names rewindTo", err)
	}
	if _, err = FindPlan(plans, execFor("recv", &at), "", m3); err != nil {
		t.Errorf("same rewindTo should match: %v", err)
	}
}

func TestFindPlan_AnEmptySourceMeansTheDefault(t *testing.T) {
	plans := []v1alpha1.Failover{planned("p", "recv", m3.Add(-time.Minute), nil)}
	if _, err := FindPlan(plans, execFor("", nil), "recv", m3); err != nil {
		t.Errorf("default source should match: %v", err)
	}
	if _, err := FindPlan(plans, execFor("", nil), "", m3); err == nil {
		t.Error("no source and no default should be refused")
	}
}

func TestExpectationFromPlan(t *testing.T) {
	p := planned("p", "recv", m3, nil)
	e := ExpectationFromPlan(&p)
	if !e.VMs["ns/vm"].Equal(m2) || len(e.Volumes) != 2 {
		t.Errorf("expectation = %+v", e)
	}
	if len(e.Targets) != 2 {
		t.Errorf("targets = %+v, want one for each planned volume", e.Targets)
	}
}

func TestFindPlan_NeedsTheSameBasePath(t *testing.T) {
	p := planned("p", "recv", m3.Add(-time.Minute), nil)
	p.Spec.BasePath = "/repl"
	exec := &v1alpha1.Failover{Spec: v1alpha1.FailoverSpec{Source: "recv", Action: v1alpha1.FailoverActionExecute}}
	if _, err := FindPlan([]v1alpha1.Failover{p}, exec, "", m3); err == nil || !strings.Contains(err.Error(), "basePath") {
		t.Fatalf("err = %v, want a refusal that names basePath", err)
	}
	exec.Spec.BasePath = "/repl/"
	if _, err := FindPlan([]v1alpha1.Failover{p}, exec, "", m3); err != nil {
		t.Fatalf("same base path (trailing slash aside) should match: %v", err)
	}
	exec.Spec.BasePath = "/"
	p.Spec.BasePath = ""
	if _, err := FindPlan([]v1alpha1.Failover{p}, exec, "", m3); err != nil {
		t.Fatalf("unset and / are the same: %v", err)
	}
}
