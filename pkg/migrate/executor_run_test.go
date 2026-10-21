package migrate

import (
	"bytes"
	"context"
	"strings"
	"sync/atomic"
	"testing"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/watch"
	"k8s.io/client-go/kubernetes/fake"
	ktesting "k8s.io/client-go/testing"
)

func boundSourcePVC() *corev1.PersistentVolumeClaim {
	return &corev1.PersistentVolumeClaim{
		ObjectMeta: metav1.ObjectMeta{Name: "data", Namespace: "apps"},
		Spec: corev1.PersistentVolumeClaimSpec{
			StorageClassName: strPtr("ibmc-vpc-file-3000-iops"),
			Resources: corev1.VolumeResourceRequirements{
				Requests: corev1.ResourceList{corev1.ResourceStorage: resource.MustParse("200Gi")},
			},
		},
		Status: corev1.PersistentVolumeClaimStatus{Phase: corev1.ClaimBound},
	}
}

// runFixture is a fake cluster in which the driver binds a new claim at once, and the migration pod
// is Running when the first watch starts and ends in finalPhase when the second watch starts.
type runFixture struct {
	clientset *fake.Clientset
	output    *bytes.Buffer
	executor  *Executor
}

func newRunFixture(t *testing.T, finalPhase corev1.PodPhase, objects ...runtime.Object) *runFixture {
	t.Helper()
	cs := fake.NewSimpleClientset(append([]runtime.Object{boundSourcePVC()}, objects...)...) //nolint:staticcheck // NewClientset requires generated apply configs

	// The driver binds a new claim as soon as it is made.
	cs.PrependReactor("create", "persistentvolumeclaims", func(action ktesting.Action) (bool, runtime.Object, error) {
		if create, ok := action.(ktesting.CreateAction); ok {
			if pvc, isPVC := create.GetObject().(*corev1.PersistentVolumeClaim); isPVC {
				pvc.Status.Phase = corev1.ClaimBound
			}
		}
		return false, nil, nil
	})
	// A real watch without a resource version starts with the current object. The fake one does not, so send it.
	cs.PrependWatchReactor("persistentvolumeclaims", func(action ktesting.Action) (bool, watch.Interface, error) {
		w := watch.NewFakeWithChanSize(1, false)
		watchAction, ok := action.(ktesting.WatchAction)
		if !ok {
			return false, nil, nil
		}
		name := strings.TrimPrefix(watchAction.GetWatchRestrictions().Fields.String(), "metadata.name=")
		pvc, err := cs.Tracker().Get(corev1.SchemeGroupVersion.WithResource("persistentvolumeclaims"), "apps", name)
		if err != nil {
			return true, nil, err
		}
		w.Add(pvc)
		return true, w, nil
	})
	var podWatches atomic.Int32
	cs.PrependWatchReactor("pods", func(action ktesting.Action) (bool, watch.Interface, error) {
		w := watch.NewFakeWithChanSize(1, false)
		pod := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "migrate-data", Namespace: "apps"}}
		pod.Status.Phase = corev1.PodRunning
		if podWatches.Add(1) > 1 {
			pod.Status.Phase = finalPhase
		}
		w.Add(pod)
		return true, w, nil
	})

	out := &bytes.Buffer{}
	e := NewExecutor(cs, out)
	e.deleteWait = 0
	return &runFixture{clientset: cs, output: out, executor: e}
}

func TestExecutor_Execute_Success(t *testing.T) {
	f := newRunFixture(t, corev1.PodSucceeded)

	result, err := f.executor.Execute(context.Background(), "apps", "data", "pool-a", "pool-a", false)
	if err != nil {
		t.Fatalf("Execute() error: %v", err)
	}
	if !result.Success || result.TargetPVC != "data-pooled" || result.PodName != "migrate-data" {
		t.Fatalf("unexpected result: %+v", result)
	}
	if !strings.Contains(result.Message, "data-pooled") {
		t.Errorf("message does not name the new claim: %q", result.Message)
	}

	target, err := f.clientset.CoreV1().PersistentVolumeClaims("apps").Get(context.Background(), "data-pooled", metav1.GetOptions{})
	if err != nil {
		t.Fatalf("target claim not made: %v", err)
	}
	if got := target.Spec.Resources.Requests[corev1.ResourceStorage]; got.Cmp(resource.MustParse("200Gi")) != 0 {
		t.Errorf("target size = %s, want 200Gi", got.String())
	}
	if target.Spec.StorageClassName == nil || *target.Spec.StorageClassName != "pool-a" {
		t.Errorf("target class = %v, want pool-a", target.Spec.StorageClassName)
	}
	if target.Annotations[AnnotationSourcePVC] != "data" {
		t.Errorf("target does not record its source: %v", target.Annotations)
	}
	if _, err = f.clientset.CoreV1().Pods("apps").Get(context.Background(), "migrate-data", metav1.GetOptions{}); err != nil {
		t.Errorf("migration pod not made: %v", err)
	}
	if _, err = f.clientset.CoreV1().PersistentVolumeClaims("apps").Get(context.Background(), "data", metav1.GetOptions{}); err != nil {
		t.Errorf("the old claim must stay: %v", err)
	}
	for _, want := range []string{"Target PVC is Bound", "Migration pod is running", "fake logs"} {
		if !strings.Contains(f.output.String(), want) {
			t.Errorf("output lacks %q:\n%s", want, f.output.String())
		}
	}
}

func TestExecutor_Execute_CopyFails(t *testing.T) {
	f := newRunFixture(t, corev1.PodFailed)

	result, err := f.executor.Execute(context.Background(), "apps", "data", "pool-a", "pool-a", false)
	if err != nil {
		t.Fatalf("a failed copy is a result, not an error: %v", err)
	}
	if result.Success {
		t.Fatal("Success must be false when the pod failed")
	}
	if !strings.Contains(result.Message, "failed") || !strings.Contains(result.Message, "kubectl logs") {
		t.Errorf("message should say it failed and where the log is: %q", result.Message)
	}
	if _, err = f.clientset.CoreV1().PersistentVolumeClaims("apps").Get(context.Background(), "data", metav1.GetOptions{}); err != nil {
		t.Errorf("the old claim must stay after a failed copy: %v", err)
	}
}

func TestExecutor_Execute_ReusesAnExistingTargetClaim(t *testing.T) {
	existing := &corev1.PersistentVolumeClaim{
		ObjectMeta: metav1.ObjectMeta{Name: "data-pooled", Namespace: "apps"},
		Status:     corev1.PersistentVolumeClaimStatus{Phase: corev1.ClaimBound},
	}
	f := newRunFixture(t, corev1.PodSucceeded, existing)

	result, err := f.executor.Execute(context.Background(), "apps", "data", "pool-a", "pool-a", false)
	if err != nil {
		t.Fatalf("Execute() error: %v", err)
	}
	if !result.Success {
		t.Fatalf("expected success, got %+v", result)
	}
	if !strings.Contains(f.output.String(), "already exists, reusing it") {
		t.Errorf("output should say the claim is reused:\n%s", f.output.String())
	}
}

func TestExecutor_Execute_PodThatNeverStartsIsRemoved(t *testing.T) {
	f := newRunFixture(t, corev1.PodSucceeded)
	// The pod is Failed as soon as the first watch starts.
	f.clientset.PrependWatchReactor("pods", func(ktesting.Action) (bool, watch.Interface, error) {
		w := watch.NewFakeWithChanSize(1, false)
		w.Add(&corev1.Pod{
			ObjectMeta: metav1.ObjectMeta{Name: "migrate-data", Namespace: "apps"},
			Status:     corev1.PodStatus{Phase: corev1.PodFailed},
		})
		return true, w, nil
	})

	_, err := f.executor.Execute(context.Background(), "apps", "data", "pool-a", "pool-a", false)
	if err == nil || !strings.Contains(err.Error(), "did not start") {
		t.Fatalf("want a 'did not start' error, got %v", err)
	}
	_, getErr := f.clientset.CoreV1().Pods("apps").Get(context.Background(), "migrate-data", metav1.GetOptions{})
	if !apierrors.IsNotFound(getErr) {
		t.Errorf("the pod that never started must be removed, got %v", getErr)
	}
}

func TestExecutor_Execute_TargetClaimThatNeverBinds(t *testing.T) {
	f := newRunFixture(t, corev1.PodSucceeded)
	f.clientset.PrependWatchReactor("persistentvolumeclaims", func(ktesting.Action) (bool, watch.Interface, error) {
		w := watch.NewFakeWithChanSize(1, false)
		w.Error(&metav1.Status{Status: metav1.StatusFailure, Message: "boom"})
		return true, w, nil
	})

	_, err := f.executor.Execute(context.Background(), "apps", "data", "pool-a", "pool-a", false)
	if err == nil || !strings.Contains(err.Error(), "did not bind") {
		t.Fatalf("want a 'did not bind' error, got %v", err)
	}
	if _, getErr := f.clientset.CoreV1().Pods("apps").Get(context.Background(), "migrate-data", metav1.GetOptions{}); !apierrors.IsNotFound(getErr) {
		t.Errorf("no pod must be made when the claim did not bind, got %v", getErr)
	}
}
