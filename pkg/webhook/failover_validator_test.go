package webhook

import (
	"context"
	"strings"
	"testing"
	"time"

	v1alpha1 "github.com/IBM/ibm-vpc-file-pool-csi/api/v1alpha1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

var failoverNow = time.Date(2026, 10, 21, 12, 0, 0, 0, time.UTC)

func newFailoverValidator(t *testing.T, defaultSource string, objs ...client.Object) *FailoverValidator {
	t.Helper()
	s := runtime.NewScheme()
	if err := v1alpha1.AddToScheme(s); err != nil {
		t.Fatal(err)
	}
	c := fake.NewClientBuilder().WithScheme(s).WithObjects(objs...).Build()
	return &FailoverValidator{Reader: c, DefaultSource: defaultSource, Now: func() time.Time { return failoverNow }}
}

func planFailover(name, source string, finished time.Time) *v1alpha1.Failover {
	return &v1alpha1.Failover{
		ObjectMeta: metav1.ObjectMeta{Name: name},
		Spec:       v1alpha1.FailoverSpec{Source: source, Action: v1alpha1.FailoverActionPlan},
		Status: v1alpha1.FailoverStatus{
			Phase: v1alpha1.FailoverPhasePlanned, Source: source,
			FinishedAt: &metav1.Time{Time: finished},
			VMs: []v1alpha1.FailoverVM{{
				Name: "ns/vm", Moment: metav1.NewTime(finished),
				Disks: []v1alpha1.FailoverDisk{{SubVolume: "pvc-a"}},
			}},
			Volumes: []v1alpha1.FailoverVolume{{SubVolume: "pvc-a", PVCName: "disk-a", PVCNamespace: "ns"}},
		},
	}
}

func executeFailover(source string) *v1alpha1.Failover {
	return &v1alpha1.Failover{
		ObjectMeta: metav1.ObjectMeta{Name: "go"},
		Spec:       v1alpha1.FailoverSpec{Source: source, Action: v1alpha1.FailoverActionExecute, DRPool: "dr"},
	}
}

func TestFailoverValidator_APlanNeedsNothingElse(t *testing.T) {
	v := newFailoverValidator(t, "recv")
	f := &v1alpha1.Failover{Spec: v1alpha1.FailoverSpec{Action: v1alpha1.FailoverActionPlan}}
	if _, err := v.ValidateCreate(context.Background(), f); err != nil {
		t.Fatal(err)
	}
}

func TestFailoverValidator_SpecErrors(t *testing.T) {
	v := newFailoverValidator(t, "")
	for name, tc := range map[string]struct {
		spec v1alpha1.FailoverSpec
		want string
	}{
		"no action":          {v1alpha1.FailoverSpec{Source: "recv"}, "spec.action"},
		"unknown action":     {v1alpha1.FailoverSpec{Source: "recv", Action: "Destroy"}, "spec.action"},
		"no source, default": {v1alpha1.FailoverSpec{Action: v1alpha1.FailoverActionPlan}, "spec.source"},
		"bad source":         {v1alpha1.FailoverSpec{Source: "Not A Name", Action: v1alpha1.FailoverActionPlan}, "spec.source"},
		"bad namespace":      {v1alpha1.FailoverSpec{Source: "recv", Action: v1alpha1.FailoverActionPlan, Namespace: "No_Good"}, "spec.namespace"},
	} {
		t.Run(name, func(t *testing.T) {
			_, err := v.ValidateCreate(context.Background(), &v1alpha1.Failover{Spec: tc.spec})
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Errorf("err = %v, want one that names %s", err, tc.want)
			}
		})
	}
}

func TestFailoverValidator_ExecuteNeedsAFreshPlan(t *testing.T) {
	ctx := context.Background()
	v := newFailoverValidator(t, "recv")
	if _, err := v.ValidateCreate(ctx, executeFailover("recv")); err == nil || !strings.Contains(err.Error(), "Plan") {
		t.Errorf("err = %v, want a refusal that asks for a Plan", err)
	}

	v = newFailoverValidator(t, "recv", planFailover("old", "recv", failoverNow.Add(-16*time.Minute)))
	if _, err := v.ValidateCreate(ctx, executeFailover("recv")); err == nil || !strings.Contains(err.Error(), "15 minutes") {
		t.Errorf("err = %v, want a refusal that names the 15 minute limit", err)
	}

	v = newFailoverValidator(t, "recv", planFailover("fresh", "recv", failoverNow.Add(-5*time.Minute)))
	if _, err := v.ValidateCreate(ctx, executeFailover("recv")); err != nil {
		t.Errorf("a fresh Plan should allow the Execute: %v", err)
	}
	if _, err := v.ValidateCreate(ctx, executeFailover("")); err != nil {
		t.Errorf("the default source should match the Plan: %v", err)
	}
}

func TestFailoverValidator_ExecuteNeedsADRPool(t *testing.T) {
	v := newFailoverValidator(t, "recv", planFailover("fresh", "recv", failoverNow.Add(-time.Minute)))
	f := executeFailover("recv")
	f.Spec.DRPool = ""
	if _, err := v.ValidateCreate(context.Background(), f); err == nil || !strings.Contains(err.Error(), "spec.drPool") {
		t.Errorf("err = %v", err)
	}
}

func TestFailoverValidator_RefusesAVMThatWasFailedOverAlready(t *testing.T) {
	done := &v1alpha1.SubVolume{ObjectMeta: metav1.ObjectMeta{Name: "pvc-a", Labels: map[string]string{"storage.ibmcloud.io/failover-source": "p"}}}
	v := newFailoverValidator(t, "recv", planFailover("fresh", "recv", failoverNow.Add(-time.Minute)), done)
	_, err := v.ValidateCreate(context.Background(), executeFailover("recv"))
	if err == nil || !strings.Contains(err.Error(), "ns/vm") || !strings.Contains(err.Error(), "already") {
		t.Errorf("err = %v, want a refusal that names the VM", err)
	}
}

func TestFailoverValidator_APlanForAVMThatWasFailedOverIsAllowed(t *testing.T) {
	done := &v1alpha1.SubVolume{ObjectMeta: metav1.ObjectMeta{Name: "pvc-a", Labels: map[string]string{"storage.ibmcloud.io/failover-source": "p"}}}
	v := newFailoverValidator(t, "recv", done)
	f := &v1alpha1.Failover{Spec: v1alpha1.FailoverSpec{Source: "recv", Action: v1alpha1.FailoverActionPlan}}
	if _, err := v.ValidateCreate(context.Background(), f); err != nil {
		t.Errorf("a Plan changes nothing and must be allowed: %v", err)
	}
}

func TestFailoverValidator_TheSpecCannotChange(t *testing.T) {
	v := newFailoverValidator(t, "recv")
	old := &v1alpha1.Failover{Spec: v1alpha1.FailoverSpec{Source: "recv", Action: v1alpha1.FailoverActionPlan}}
	same := old.DeepCopy()
	same.Status.Phase = v1alpha1.FailoverPhasePlanned
	if _, err := v.ValidateUpdate(context.Background(), old, same); err != nil {
		t.Errorf("a status-only change should pass: %v", err)
	}
	changed := old.DeepCopy()
	changed.Spec.Action = v1alpha1.FailoverActionExecute
	if _, err := v.ValidateUpdate(context.Background(), old, changed); err == nil {
		t.Error("a change to the spec must be refused")
	}
}
