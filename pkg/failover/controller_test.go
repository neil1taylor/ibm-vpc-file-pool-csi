package failover

import (
	"context"
	"strings"
	"testing"
	"time"

	v1alpha1 "github.com/IBM/ibm-vpc-file-pool-csi/api/v1alpha1"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

const testNS = "kube-system"

func testScheme(t *testing.T) *runtime.Scheme {
	t.Helper()
	s := runtime.NewScheme()
	for _, add := range []func(*runtime.Scheme) error{v1alpha1.AddToScheme, corev1.AddToScheme, batchv1.AddToScheme} {
		if err := add(s); err != nil {
			t.Fatal(err)
		}
	}
	return s
}

func receiverClaim() []client.Object {
	return []client.Object{
		&corev1.PersistentVolumeClaim{
			ObjectMeta: metav1.ObjectMeta{Name: "recv", Namespace: testNS},
			Spec:       corev1.PersistentVolumeClaimSpec{VolumeName: "recv-pv"},
		},
		&corev1.PersistentVolume{
			ObjectMeta: metav1.ObjectMeta{Name: "recv-pv"},
			Spec: corev1.PersistentVolumeSpec{PersistentVolumeSource: corev1.PersistentVolumeSource{
				NFS: &corev1.NFSVolumeSource{Server: "10.0.0.9", Path: "/export_1"},
			}},
		},
	}
}

func newTestController(t *testing.T, objs ...client.Object) (*Controller, client.Client) {
	t.Helper()
	c := fake.NewClientBuilder().WithScheme(testScheme(t)).
		WithStatusSubresource(&v1alpha1.Failover{}).
		WithObjects(objs...).Build()
	ctl := NewController(c, testNS, "driver:test", "recv")
	ctl.now = func() time.Time { return m3 }
	return ctl, c
}

func getFailover(t *testing.T, c client.Client, name string) *v1alpha1.Failover {
	t.Helper()
	f := &v1alpha1.Failover{}
	if err := c.Get(context.Background(), types.NamespacedName{Name: name}, f); err != nil {
		t.Fatal(err)
	}
	return f
}

func newFailover(name, action string) *v1alpha1.Failover {
	return &v1alpha1.Failover{
		ObjectMeta: metav1.ObjectMeta{Name: name},
		Spec:       v1alpha1.FailoverSpec{Action: action},
	}
}

func jobFor(t *testing.T, c client.Client, name string) *batchv1.Job {
	t.Helper()
	jobs := &batchv1.JobList{}
	if err := c.List(context.Background(), jobs, client.InNamespace(testNS)); err != nil {
		t.Fatal(err)
	}
	for i := range jobs.Items {
		if jobs.Items[i].Labels[failoverJobLabel] == failoverJobName(name) {
			return &jobs.Items[i]
		}
	}
	return nil
}

// finishJob marks the Job as ended, with a pod that left message behind.
func finishJob(t *testing.T, c client.Client, job *batchv1.Job, succeeded bool, message string) {
	t.Helper()
	if succeeded {
		job.Status.Succeeded = 1
	} else {
		job.Status.Failed = 1
	}
	if err := c.Status().Update(context.Background(), job); err != nil {
		t.Fatal(err)
	}
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: job.Name + "-x", Namespace: testNS, Labels: job.Spec.Template.Labels},
		Status: corev1.PodStatus{ContainerStatuses: []corev1.ContainerStatus{{
			Name:  "failover",
			State: corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{Message: message}},
		}}},
	}
	if err := c.Create(context.Background(), pod); err != nil {
		t.Fatal(err)
	}
}

func planMessage(t *testing.T) string {
	t.Helper()
	msg, err := EncodeOutcome(Outcome{Result: &Result{
		RPOSeconds: 600,
		VMs: []v1alpha1.FailoverVM{{
			Name: "ns/vm", Moment: metav1.NewTime(m2),
			Moments: []metav1.Time{metav1.NewTime(m2), metav1.NewTime(m1)},
			Disks:   []v1alpha1.FailoverDisk{{SubVolume: "pvc-a"}},
		}},
		Volumes: []v1alpha1.FailoverVolume{
			{SubVolume: "pvc-a", PVCName: "disk-a", PVCNamespace: "ns", RequestedGB: 10, SubPath: "/pvcs/pvc-a", Policy: "p", LastSync: metav1.NewTime(m2)},
			{SubVolume: "pvc-plain", PVCName: "data", PVCNamespace: "ns", RequestedGB: 5, SubPath: "/pvcs/pvc-plain", Policy: "p", LastSync: metav1.NewTime(m2)},
		},
	}})
	if err != nil {
		t.Fatal(err)
	}
	return msg
}

func TestController_PlanStartsAJobThatMountsTheSourceClaim(t *testing.T) {
	ctl, c := newTestController(t, append(receiverClaim(), newFailover("p1", v1alpha1.FailoverActionPlan))...)
	if err := ctl.processOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	f := getFailover(t, c, "p1")
	if f.Status.Phase != v1alpha1.FailoverPhasePlanning || f.Status.Source != "recv" || f.Status.StartedAt == nil {
		t.Errorf("status = %+v", f.Status)
	}
	job := jobFor(t, c, "p1")
	if job == nil {
		t.Fatal("no Job was made")
	}
	spec := job.Spec.Template.Spec
	if spec.Volumes[0].PersistentVolumeClaim == nil || spec.Volumes[0].PersistentVolumeClaim.ClaimName != "recv" {
		t.Errorf("volumes = %+v, want the claim recv", spec.Volumes)
	}
	ct := spec.Containers[0]
	if ct.Image != "driver:test" {
		t.Errorf("image = %s", ct.Image)
	}
	args := strings.Join(ct.Args, " ")
	for _, want := range []string{"--mode=failover", "--nfs-mount-path=/data", "--failover-action=Plan"} {
		if !strings.Contains(args, want) {
			t.Errorf("args %q lack %s", args, want)
		}
	}
	if spec.SecurityContext == nil || *spec.SecurityContext.RunAsUser != 107 {
		t.Errorf("the Job must run as the QEMU user, got %+v", spec.SecurityContext)
	}
	if *job.Spec.BackoffLimit != 0 || spec.RestartPolicy != corev1.RestartPolicyNever {
		t.Error("a failover Job must not be retried")
	}
}

func TestController_PlanFlagsAreCarriedToTheJob(t *testing.T) {
	f := newFailover("p1", v1alpha1.FailoverActionPlan)
	f.Spec.Force = true
	f.Spec.RewindTo = &metav1.Time{Time: m1}
	ctl, c := newTestController(t, append(receiverClaim(), f)...)
	if err := ctl.processOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	args := strings.Join(jobFor(t, c, "p1").Spec.Template.Spec.Containers[0].Args, " ")
	for _, want := range []string{"--force", "--rewind-to=" + m1.Format(time.RFC3339)} {
		if !strings.Contains(args, want) {
			t.Errorf("args %q lack %s", args, want)
		}
	}
}

func TestController_AMissingSourceClaimFailsTheFailover(t *testing.T) {
	ctl, c := newTestController(t, newFailover("p1", v1alpha1.FailoverActionPlan))
	if err := ctl.processOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	f := getFailover(t, c, "p1")
	if f.Status.Phase != v1alpha1.FailoverPhaseFailed || !strings.Contains(f.Status.Message, "recv") {
		t.Errorf("status = %+v, want Failed with a message that names the claim", f.Status)
	}
	if jobFor(t, c, "p1") != nil {
		t.Error("no Job should be made")
	}
}

func TestController_NoSourceAndNoDefaultFails(t *testing.T) {
	c := fake.NewClientBuilder().WithScheme(testScheme(t)).WithStatusSubresource(&v1alpha1.Failover{}).
		WithObjects(newFailover("p1", v1alpha1.FailoverActionPlan)).Build()
	ctl := NewController(c, testNS, "driver:test", "")
	if err := ctl.processOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	f := getFailover(t, c, "p1")
	if f.Status.Phase != v1alpha1.FailoverPhaseFailed || !strings.Contains(f.Status.Message, "source") {
		t.Errorf("status = %+v", f.Status)
	}
}

func TestController_AFinishedPlanFillsTheStatusAndRemovesTheJob(t *testing.T) {
	ctl, c := newTestController(t, append(receiverClaim(), newFailover("p1", v1alpha1.FailoverActionPlan))...)
	ctx := context.Background()
	if err := ctl.processOnce(ctx); err != nil {
		t.Fatal(err)
	}
	finishJob(t, c, jobFor(t, c, "p1"), true, planMessage(t))
	if err := ctl.processOnce(ctx); err != nil {
		t.Fatal(err)
	}
	f := getFailover(t, c, "p1")
	if f.Status.Phase != v1alpha1.FailoverPhasePlanned || f.Status.FinishedAt == nil {
		t.Fatalf("status = %+v", f.Status)
	}
	if len(f.Status.VMs) != 1 || len(f.Status.Volumes) != 2 || f.Status.RPO == nil || f.Status.RPO.Duration != 10*time.Minute {
		t.Errorf("status = %+v", f.Status)
	}
	if jobFor(t, c, "p1") != nil {
		t.Error("the Job should be removed")
	}
}

func TestController_AFailedJobFailsTheFailoverWithItsReason(t *testing.T) {
	ctl, c := newTestController(t, append(receiverClaim(), newFailover("p1", v1alpha1.FailoverActionPlan))...)
	ctx := context.Background()
	if err := ctl.processOnce(ctx); err != nil {
		t.Fatal(err)
	}
	msg, _ := EncodeOutcome(Outcome{Error: "a copy is still being applied to pvc-a"})
	finishJob(t, c, jobFor(t, c, "p1"), false, msg)
	if err := ctl.processOnce(ctx); err != nil {
		t.Fatal(err)
	}
	f := getFailover(t, c, "p1")
	if f.Status.Phase != v1alpha1.FailoverPhaseFailed || !strings.Contains(f.Status.Message, "still being applied") {
		t.Errorf("status = %+v", f.Status)
	}
}

func TestController_AFailedJobWithoutAMessageStillFails(t *testing.T) {
	ctl, c := newTestController(t, append(receiverClaim(), newFailover("p1", v1alpha1.FailoverActionPlan))...)
	ctx := context.Background()
	if err := ctl.processOnce(ctx); err != nil {
		t.Fatal(err)
	}
	finishJob(t, c, jobFor(t, c, "p1"), false, "")
	if err := ctl.processOnce(ctx); err != nil {
		t.Fatal(err)
	}
	if f := getFailover(t, c, "p1"); f.Status.Phase != v1alpha1.FailoverPhaseFailed || f.Status.Message == "" {
		t.Errorf("status = %+v", f.Status)
	}
}

func freshPlan() *v1alpha1.Failover {
	p := planned("plan1", "recv", m3.Add(-2*time.Minute), nil)
	return &p
}

func TestController_ExecuteWithoutAFreshPlanFails(t *testing.T) {
	e := newFailover("go", v1alpha1.FailoverActionExecute)
	e.Spec.DRPool = "dr"
	ctl, c := newTestController(t, append(receiverClaim(), e)...)
	if err := ctl.processOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	f := getFailover(t, c, "go")
	if f.Status.Phase != v1alpha1.FailoverPhaseFailed || !strings.Contains(f.Status.Message, "Plan") {
		t.Errorf("status = %+v", f.Status)
	}
	if jobFor(t, c, "go") != nil {
		t.Error("no Job should be made")
	}
}

func TestController_ExecuteNeedsADRPool(t *testing.T) {
	e := newFailover("go", v1alpha1.FailoverActionExecute)
	ctl, c := newTestController(t, append(receiverClaim(), e, freshPlan())...)
	if err := ctl.processOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	if f := getFailover(t, c, "go"); f.Status.Phase != v1alpha1.FailoverPhaseFailed || !strings.Contains(f.Status.Message, "drPool") {
		t.Errorf("status = %+v", f.Status)
	}
}

func TestController_ExecuteCarriesThePlanToTheJobAndMakesTheObjects(t *testing.T) {
	e := newFailover("go", v1alpha1.FailoverActionExecute)
	e.Spec.DRPool = "dr"
	ctl, c := newTestController(t, append(receiverClaim(), e, freshPlan())...)
	ctx := context.Background()
	if err := ctl.processOnce(ctx); err != nil {
		t.Fatal(err)
	}
	if f := getFailover(t, c, "go"); f.Status.Phase != v1alpha1.FailoverPhaseExecuting {
		t.Fatalf("status = %+v", f.Status)
	}
	job := jobFor(t, c, "go")
	args := strings.Join(job.Spec.Template.Spec.Containers[0].Args, " ")
	if !strings.Contains(args, "--failover-action=Execute") || !strings.Contains(args, "--failover-expect=") || !strings.Contains(args, "ns/vm") {
		t.Errorf("args = %q, want the action and the confirmed plan", args)
	}

	finishJob(t, c, job, true, planMessage(t))
	if err := ctl.processOnce(ctx); err != nil {
		t.Fatal(err)
	}
	f := getFailover(t, c, "go")
	if f.Status.Phase != v1alpha1.FailoverPhaseDone || len(f.Status.CreatedPVCs) != 2 {
		t.Fatalf("status = %+v", f.Status)
	}
	sv := &v1alpha1.SubVolume{}
	if err := c.Get(ctx, types.NamespacedName{Name: "pvc-a"}, sv); err != nil {
		t.Fatalf("SubVolume not made: %v", err)
	}
	if sv.Spec.PoolName != "dr" || sv.Spec.ShareMountTargetIP != "10.0.0.9" || sv.Spec.ShareExportPath != "/export_1" {
		t.Errorf("sub volume spec = %+v, want the pool and the share of the claim", sv.Spec)
	}
	pv := &corev1.PersistentVolume{}
	if err := c.Get(ctx, types.NamespacedName{Name: "dr-pvc-a"}, pv); err != nil {
		t.Fatalf("PV not made: %v", err)
	}
	if pv.Spec.NFS.Server != "10.0.0.9" || pv.Spec.NFS.Path != "/export_1/pvcs/pvc-a" {
		t.Errorf("pv nfs = %+v", pv.Spec.NFS)
	}
	pvc := &corev1.PersistentVolumeClaim{}
	if err := c.Get(ctx, types.NamespacedName{Namespace: "ns", Name: "disk-a"}, pvc); err != nil {
		t.Fatalf("PVC not made: %v", err)
	}
}

func TestController_ExecuteRefusesAVMThatWasFailedOverAlready(t *testing.T) {
	e := newFailover("go", v1alpha1.FailoverActionExecute)
	e.Spec.DRPool = "dr"
	done := &v1alpha1.SubVolume{ObjectMeta: metav1.ObjectMeta{Name: "pvc-a", Labels: map[string]string{FailoverSourceLabel: "p"}}}
	ctl, c := newTestController(t, append(receiverClaim(), e, freshPlan(), done)...)
	if err := ctl.processOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	f := getFailover(t, c, "go")
	if f.Status.Phase != v1alpha1.FailoverPhaseFailed || !strings.Contains(f.Status.Message, "ns/vm") {
		t.Errorf("status = %+v, want a refusal that names the VM", f.Status)
	}
}

func TestController_FinishedFailoversAreLeftAlone(t *testing.T) {
	f := newFailover("old", v1alpha1.FailoverActionPlan)
	f.Status.Phase = v1alpha1.FailoverPhasePlanned
	ctl, c := newTestController(t, append(receiverClaim(), f)...)
	if err := ctl.processOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	if jobFor(t, c, "old") != nil {
		t.Error("a finished Failover must not start a Job")
	}
}

func TestController_ALostJobIsStartedAgain(t *testing.T) {
	ctl, c := newTestController(t, append(receiverClaim(), newFailover("p1", v1alpha1.FailoverActionPlan))...)
	ctx := context.Background()
	if err := ctl.processOnce(ctx); err != nil {
		t.Fatal(err)
	}
	job := jobFor(t, c, "p1")
	if err := c.Delete(ctx, job); err != nil && !apierrors.IsNotFound(err) {
		t.Fatal(err)
	}
	if err := ctl.processOnce(ctx); err != nil {
		t.Fatal(err)
	}
	if jobFor(t, c, "p1") == nil {
		t.Error("the Job should be started again")
	}
}
