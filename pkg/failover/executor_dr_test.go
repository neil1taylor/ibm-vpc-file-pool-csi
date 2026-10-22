package failover

import (
	"bytes"
	"context"
	"slices"
	"strings"
	"testing"
	"time"

	v1alpha1 "github.com/IBM/ibm-vpc-file-pool-csi/api/v1alpha1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

const drUUID = "pvc-0a1b2c3d-0000-4000-8000-000000000001"

func rewindPlan() *FailoverPlan {
	p := testPlan()
	p.SubVolumes[0].SubVolumeName = drUUID
	p.SubVolumes[0].SubPath = "/pvcs/" + drUUID
	p.SubVolumes[0].VMGroup = "vms/my-vm"
	p.SubVolumes[0].VMMoment = time.Unix(1761129900, 0).UTC()
	p.SubVolumes[0].Rewind = true
	return p
}

// A dry run of a rewind shows, and checks, the folder the real run will use (#209).
func TestExecutor_DryRunShowsTheRewoundFolder(t *testing.T) {
	var out bytes.Buffer
	exec := NewExecutor(nil, &out)
	opts := ExecuteOptions{DRPoolName: "dr-pool", DRShareIP: "10.0.0.5", DRExportPath: "/export", DryRun: true}
	if err := exec.Execute(context.Background(), rewindPlan(), opts); err != nil {
		t.Fatal(err)
	}
	want := "nfs://10.0.0.5/export/pvcs/" + drUUID + ".rewind-1761129900"
	if !strings.Contains(out.String(), want) {
		t.Fatalf("dry run output lacks %q:\n%s", want, out.String())
	}
}

// After the disks were rewound, the plan already holds the new folder. It must
// not get the suffix a second time.
func TestExecutor_RewoundFolderGetsTheSuffixOnce(t *testing.T) {
	var out bytes.Buffer
	exec := NewExecutor(nil, &out)
	plan := rewindPlan()
	plan.SubVolumes[0].SubPath += ".rewind-1761129900"
	opts := ExecuteOptions{DRPoolName: "dr-pool", DRShareIP: "10.0.0.5", DryRun: true}
	if err := exec.Execute(context.Background(), plan, opts); err != nil {
		t.Fatal(err)
	}
	if strings.Count(out.String(), ".rewind-") != 1 {
		t.Fatalf("expected the suffix once:\n%s", out.String())
	}
}

// Both a dry run and a real run refuse a folder the SubVolume would not accept,
// before anything is made.
func TestExecutor_RefusesAnUnsafeFolder(t *testing.T) {
	for _, bad := range []string{
		"/pvcs/../etc",
		"/pvcs/" + drUUID + "/../..",
		"/pvcs/" + drUUID + ".rewind-1/../x",
		"/etc/passwd",
		"/pvcs/" + drUUID + ".rewind-",
	} {
		for _, dry := range []bool{true, false} {
			fc := fake.NewClientBuilder().WithScheme(newScheme()).Build()
			var out bytes.Buffer
			exec := NewExecutor(fc, &out)
			plan := testPlan()
			plan.SubVolumes[0].SubPath = bad
			err := exec.Execute(context.Background(), plan, ExecuteOptions{DRPoolName: "dr-pool", DRShareIP: "10.0.0.5", DryRun: dry})
			if err == nil {
				t.Fatalf("%q (dry run %v): expected an error", bad, dry)
			}
			var svs v1alpha1.SubVolumeList
			if lerr := fc.List(context.Background(), &svs); lerr != nil || len(svs.Items) != 0 {
				t.Fatalf("%q: nothing may be created, found %d (%v)", bad, len(svs.Items), lerr)
			}
		}
	}
}

// A failover PV carries the mount options of the pool's PVs (#210).
func TestExecutor_PVHasTheNodeMountOptions(t *testing.T) {
	fc := fake.NewClientBuilder().WithScheme(newScheme()).Build()
	exec := NewExecutor(fc, &bytes.Buffer{})
	if err := exec.Execute(context.Background(), testPlan(), ExecuteOptions{DRPoolName: "dr-pool", DRShareIP: "10.0.0.5"}); err != nil {
		t.Fatal(err)
	}
	var pv corev1.PersistentVolume
	if err := fc.Get(context.Background(), client.ObjectKey{Name: "dr-pvc-aaa"}, &pv); err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"sec=sys", "soft", "nfsvers=4.1", "timeo=600", "retrans=3"} {
		if !slices.Contains(pv.Spec.MountOptions, want) {
			t.Errorf("mount options %v lack %q", pv.Spec.MountOptions, want)
		}
	}
}

// The pool's own mount options win over the defaults, as they do on the node.
func TestExecutor_PVUsesThePoolMountOptions(t *testing.T) {
	pool := &v1alpha1.FileSharePool{
		ObjectMeta: metav1.ObjectMeta{Name: "dr-pool"},
		Spec:       v1alpha1.FileSharePoolSpec{MountOptions: []string{"timeo=300", "rsize=1048576"}},
	}
	fc := fake.NewClientBuilder().WithScheme(newScheme()).WithObjects(pool).Build()
	exec := NewExecutor(fc, &bytes.Buffer{})
	if err := exec.Execute(context.Background(), testPlan(), ExecuteOptions{DRPoolName: "dr-pool", DRShareIP: "10.0.0.5"}); err != nil {
		t.Fatal(err)
	}
	var pv corev1.PersistentVolume
	if err := fc.Get(context.Background(), client.ObjectKey{Name: "dr-pvc-aaa"}, &pv); err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"sec=sys", "timeo=300", "rsize=1048576"} {
		if !slices.Contains(pv.Spec.MountOptions, want) {
			t.Errorf("mount options %v lack %q", pv.Spec.MountOptions, want)
		}
	}
	if slices.Contains(pv.Spec.MountOptions, "timeo=600") {
		t.Errorf("the default timeo=600 must be replaced: %v", pv.Spec.MountOptions)
	}
}

// The dry run shows the mount options too.
func TestExecutor_DryRunShowsTheMountOptions(t *testing.T) {
	var out bytes.Buffer
	exec := NewExecutor(nil, &out)
	if err := exec.Execute(context.Background(), testPlan(), ExecuteOptions{DRPoolName: "dr-pool", DRShareIP: "10.0.0.5", DryRun: true}); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "sec=sys") {
		t.Fatalf("dry run output lacks the mount options:\n%s", out.String())
	}
}

// The PVC names no cluster default class: it asks for no class, and the PV has
// none either, so they bind to each other and to nothing else (#211).
func TestExecutor_PVCNamesNoDefaultClass(t *testing.T) {
	fc := fake.NewClientBuilder().WithScheme(newScheme()).Build()
	exec := NewExecutor(fc, &bytes.Buffer{})
	if err := exec.Execute(context.Background(), testPlan(), ExecuteOptions{DRPoolName: "dr-pool", DRShareIP: "10.0.0.5"}); err != nil {
		t.Fatal(err)
	}
	var pvc corev1.PersistentVolumeClaim
	if err := fc.Get(context.Background(), client.ObjectKey{Namespace: "production", Name: "my-data"}, &pvc); err != nil {
		t.Fatal(err)
	}
	if pvc.Spec.StorageClassName == nil || *pvc.Spec.StorageClassName != "" {
		t.Fatalf("PVC storageClassName = %v, want an empty string", pvc.Spec.StorageClassName)
	}
	var pv corev1.PersistentVolume
	if err := fc.Get(context.Background(), client.ObjectKey{Name: "dr-pvc-aaa"}, &pv); err != nil {
		t.Fatal(err)
	}
	if pv.Spec.StorageClassName != "" {
		t.Fatalf("PV storageClassName = %q, want none", pv.Spec.StorageClassName)
	}
}
