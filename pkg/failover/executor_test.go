package failover

import (
	"bytes"
	"context"
	"strings"
	"testing"
	"time"

	v1alpha1 "github.com/IBM/ibm-vpc-file-pool-csi/api/v1alpha1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

func newScheme() *runtime.Scheme {
	s := runtime.NewScheme()
	_ = corev1.AddToScheme(s)
	_ = v1alpha1.AddToScheme(s)
	return s
}

func testPlan() *FailoverPlan {
	uid := int64(107)
	gid := int64(107)
	return &FailoverPlan{
		NFSMountPath: "/repl/dst/10.245.3.8",
		RPO:          5 * time.Minute,
		SubVolumes: []FailoverSubVolumePlan{
			{
				SubVolumeName: "pvc-aaa",
				PVCName:       "my-data",
				PVCNamespace:  "production",
				RequestedGB:   10,
				SubPath:       "/pvcs/pvc-aaa",
				UID:           &uid,
				GID:           &gid,
				Permissions:   "0777",
				LastSyncTime:  time.Now().Add(-5 * time.Minute),
				PoolName:      "prod-pool",
				ShareID:       "share-1",
				PolicyName:    "prod-to-dr",
			},
		},
	}
}

func TestExecutor_CreatesResources(t *testing.T) {
	scheme := newScheme()
	fc := fake.NewClientBuilder().WithScheme(scheme).Build()

	var out bytes.Buffer
	exec := NewExecutor(fc, &out)

	plan := testPlan()
	opts := ExecuteOptions{
		DRPoolName: "dr-pool",
		DRShareIP:  "10.245.3.8",
	}

	if err := exec.Execute(context.Background(), plan, opts); err != nil {
		t.Fatalf("Execute failed: %v", err)
	}

	// Verify SubVolume CR was created.
	var sv v1alpha1.SubVolume
	if err := fc.Get(context.Background(), client.ObjectKey{Name: "pvc-aaa"}, &sv); err != nil {
		t.Fatalf("SubVolume CR not found: %v", err)
	}
	if sv.Labels[FailoverSourceLabel] != "prod-to-dr" {
		t.Errorf("expected failover-source label 'prod-to-dr', got %q", sv.Labels[FailoverSourceLabel])
	}
	if sv.Spec.PoolName != "dr-pool" {
		t.Errorf("expected PoolName 'dr-pool', got %q", sv.Spec.PoolName)
	}
	if sv.Spec.ShareMountTargetIP != "10.245.3.8" {
		t.Errorf("expected ShareMountTargetIP '10.245.3.8', got %q", sv.Spec.ShareMountTargetIP)
	}

	// Verify PV was created.
	var pv corev1.PersistentVolume
	if err := fc.Get(context.Background(), client.ObjectKey{Name: "dr-pvc-aaa"}, &pv); err != nil {
		t.Fatalf("PV not found: %v", err)
	}
	if pv.Spec.NFS == nil {
		t.Fatal("PV should have NFS source")
	}
	if pv.Spec.NFS.Server != "10.245.3.8" {
		t.Errorf("expected NFS server 10.245.3.8, got %s", pv.Spec.NFS.Server)
	}
	if pv.Spec.ClaimRef.Name != "my-data" {
		t.Errorf("expected ClaimRef.Name 'my-data', got %q", pv.Spec.ClaimRef.Name)
	}

	// Verify PVC was created.
	var pvc corev1.PersistentVolumeClaim
	if err := fc.Get(context.Background(), client.ObjectKey{Namespace: "production", Name: "my-data"}, &pvc); err != nil {
		t.Fatalf("PVC not found: %v", err)
	}
	if pvc.Spec.VolumeName != "dr-pvc-aaa" {
		t.Errorf("expected VolumeName 'dr-pvc-aaa', got %q", pvc.Spec.VolumeName)
	}
}

func TestExecutor_DryRun(t *testing.T) {
	var out bytes.Buffer
	exec := NewExecutor(nil, &out)

	plan := testPlan()
	opts := ExecuteOptions{
		DRPoolName: "dr-pool",
		DRShareIP:  "10.245.3.8",
		DryRun:     true,
	}

	if err := exec.Execute(context.Background(), plan, opts); err != nil {
		t.Fatalf("Execute (dry-run) failed: %v", err)
	}

	output := out.String()
	if !strings.Contains(output, "[dry-run]") {
		t.Errorf("expected dry-run output, got: %s", output)
	}
	if !strings.Contains(output, "pvc-aaa") {
		t.Errorf("expected SubVolume name in dry-run output, got: %s", output)
	}
}

func TestExecutor_Idempotent(t *testing.T) {
	scheme := newScheme()

	// Pre-create the SubVolume CR.
	existingSV := &v1alpha1.SubVolume{
		ObjectMeta: metav1.ObjectMeta{
			Name: "pvc-aaa",
			Labels: map[string]string{
				FailoverSourceLabel: "prod-to-dr",
			},
		},
		Spec: v1alpha1.SubVolumeSpec{
			PoolName:           "dr-pool",
			ShareID:            "dr-share",
			ShareMountTargetIP: "10.245.3.8",
			SubPath:            "/pvcs/pvc-aaa",
			RequestedGB:        10,
			PVName:             "dr-pvc-aaa",
			PVCName:            "my-data",
			PVCNamespace:       "production",
			ReclaimPolicy:      "Retain",
		},
	}

	existingPV := &corev1.PersistentVolume{
		ObjectMeta: metav1.ObjectMeta{Name: "dr-pvc-aaa"},
		Spec: corev1.PersistentVolumeSpec{
			PersistentVolumeSource: corev1.PersistentVolumeSource{
				NFS: &corev1.NFSVolumeSource{Server: "10.245.3.8", Path: "/pvcs/pvc-aaa"},
			},
		},
	}

	existingPVC := &corev1.PersistentVolumeClaim{
		ObjectMeta: metav1.ObjectMeta{Name: "my-data", Namespace: "production"},
		Spec: corev1.PersistentVolumeClaimSpec{
			VolumeName: "dr-pvc-aaa",
		},
	}

	fc := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(existingSV, existingPV, existingPVC).
		Build()

	var out bytes.Buffer
	exec := NewExecutor(fc, &out)

	plan := testPlan()
	opts := ExecuteOptions{
		DRPoolName: "dr-pool",
		DRShareIP:  "10.245.3.8",
	}

	// Should succeed (skip existing resources).
	if err := exec.Execute(context.Background(), plan, opts); err != nil {
		t.Fatalf("Execute (idempotent) failed: %v", err)
	}

	output := out.String()
	if !strings.Contains(output, "already exists") {
		t.Errorf("expected 'already exists' messages, got: %s", output)
	}
}

func TestExecutor_NamespaceOverride(t *testing.T) {
	scheme := newScheme()
	fc := fake.NewClientBuilder().WithScheme(scheme).Build()

	var out bytes.Buffer
	exec := NewExecutor(fc, &out)

	plan := testPlan()
	opts := ExecuteOptions{
		DRPoolName:        "dr-pool",
		DRShareIP:         "10.245.3.8",
		NamespaceOverride: "dr-namespace",
	}

	if err := exec.Execute(context.Background(), plan, opts); err != nil {
		t.Fatalf("Execute failed: %v", err)
	}

	// Verify PVC was created in the overridden namespace.
	var pvc corev1.PersistentVolumeClaim
	if err := fc.Get(context.Background(), client.ObjectKey{Namespace: "dr-namespace", Name: "my-data"}, &pvc); err != nil {
		t.Fatalf("PVC not found in overridden namespace: %v", err)
	}
}

func TestExecutor_WithExportPath(t *testing.T) {
	scheme := newScheme()
	fc := fake.NewClientBuilder().WithScheme(scheme).Build()

	var out bytes.Buffer
	exec := NewExecutor(fc, &out)

	plan := testPlan()
	opts := ExecuteOptions{
		DRPoolName:   "dr-pool",
		DRShareIP:    "10.245.3.8",
		DRExportPath: "/share_abc123",
	}

	if err := exec.Execute(context.Background(), plan, opts); err != nil {
		t.Fatalf("Execute failed: %v", err)
	}

	// Verify PV has the export path prepended.
	var pv corev1.PersistentVolume
	if err := fc.Get(context.Background(), client.ObjectKey{Name: "dr-pvc-aaa"}, &pv); err != nil {
		t.Fatalf("PV not found: %v", err)
	}
	expectedPath := "/share_abc123/pvcs/pvc-aaa"
	if pv.Spec.NFS.Path != expectedPath {
		t.Errorf("expected NFS path %q, got %q", expectedPath, pv.Spec.NFS.Path)
	}
}
