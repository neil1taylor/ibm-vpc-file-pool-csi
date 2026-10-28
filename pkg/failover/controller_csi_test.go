package failover

import (
	"context"
	"strings"
	"testing"

	v1alpha1 "github.com/IBM/ibm-vpc-file-pool-csi/api/v1alpha1"
	"github.com/IBM/ibm-vpc-file-pool-csi/pkg/driver"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// csiReceiverClaim is a receiver claim that the driver itself provisioned: a
// CSI volume whose folder is a subdirectory of a pool share.
func csiReceiverClaim(driverName string, attrs map[string]string) []client.Object {
	return []client.Object{
		&corev1.PersistentVolumeClaim{
			ObjectMeta: metav1.ObjectMeta{Name: "recv", Namespace: testNS},
			Spec:       corev1.PersistentVolumeClaimSpec{VolumeName: "recv-pv"},
		},
		&corev1.PersistentVolume{
			ObjectMeta: metav1.ObjectMeta{Name: "recv-pv"},
			Spec: corev1.PersistentVolumeSpec{PersistentVolumeSource: corev1.PersistentVolumeSource{
				CSI: &corev1.CSIPersistentVolumeSource{Driver: driverName, VolumeHandle: "pool/share-1/pvc-recv", VolumeAttributes: attrs},
			}},
		},
	}
}

func goodCSIAttrs() map[string]string {
	return map[string]string{"server": "10.1.2.3", "share": "/share_abc/", "subDir": "/pvcs/pvc-11111111-2222-3333-4444-555555555555", "pool": "dr"}
}

func TestDriverNameMatchesTheDriver(t *testing.T) {
	if csiDriverName != driver.DriverName {
		t.Fatalf("csiDriverName = %q, driver.DriverName = %q", csiDriverName, driver.DriverName)
	}
}

// runExecute runs a Plan-confirmed Execute to Done and returns the PV it made.
func runExecute(t *testing.T, f *v1alpha1.Failover, plan *v1alpha1.Failover, claim []client.Object) (*corev1.PersistentVolume, *v1alpha1.SubVolume, *v1alpha1.Failover) {
	t.Helper()
	ctl, c := newTestController(t, append(claim, f, plan)...)
	ctx := context.Background()
	if err := ctl.processOnce(ctx); err != nil {
		t.Fatal(err)
	}
	got := getFailover(t, c, f.Name)
	if got.Status.Phase != v1alpha1.FailoverPhaseExecuting {
		t.Fatalf("status = %+v", got.Status)
	}
	finishJob(t, c, jobFor(t, c, f.Name), true, planMessage(t))
	if err := ctl.processOnce(ctx); err != nil {
		t.Fatal(err)
	}
	got = getFailover(t, c, f.Name)
	if got.Status.Phase != v1alpha1.FailoverPhaseDone {
		t.Fatalf("status = %+v", got.Status)
	}
	pv := &corev1.PersistentVolume{}
	if err := c.Get(ctx, types.NamespacedName{Name: "dr-pvc-a"}, pv); err != nil {
		t.Fatal(err)
	}
	sv := &v1alpha1.SubVolume{}
	if err := c.Get(ctx, types.NamespacedName{Name: "pvc-a"}, sv); err != nil {
		t.Fatal(err)
	}
	return pv, sv, got
}

func TestController_ExecuteOnAClaimTheDriverProvisionedUsesTheVolumeAttributes(t *testing.T) {
	e := newFailover("go", v1alpha1.FailoverActionExecute)
	e.Spec.DRPool = "dr"
	pv, sv, _ := runExecute(t, e, freshPlan(), csiReceiverClaim(csiDriverName, goodCSIAttrs()))
	// The claim's folder is share + subDir; the copy sits under it at the source's sub path.
	wantExport := "/share_abc/pvcs/pvc-11111111-2222-3333-4444-555555555555"
	if pv.Spec.NFS == nil || pv.Spec.NFS.Server != "10.1.2.3" || pv.Spec.NFS.Path != wantExport+"/pvcs/pvc-a" {
		t.Errorf("pv nfs = %+v, want 10.1.2.3:%s/pvcs/pvc-a", pv.Spec.NFS, wantExport)
	}
	if sv.Spec.ShareMountTargetIP != "10.1.2.3" || sv.Spec.ShareExportPath != wantExport {
		t.Errorf("sub volume spec = %+v", sv.Spec)
	}
}

func TestController_AnExplicitShareAddressBeatsTheCSIAttributes(t *testing.T) {
	e := newFailover("go", v1alpha1.FailoverActionExecute)
	e.Spec.DRPool = "dr"
	e.Spec.DRShareIP, e.Spec.DRExportPath = "10.9.9.9", "/other"
	pv, _, _ := runExecute(t, e, freshPlan(), csiReceiverClaim(csiDriverName, goodCSIAttrs()))
	if pv.Spec.NFS.Server != "10.9.9.9" || pv.Spec.NFS.Path != "/other/pvcs/pvc-a" {
		t.Errorf("pv nfs = %+v", pv.Spec.NFS)
	}
}

func TestController_ExecuteRefusesBadCSIVolumes(t *testing.T) {
	cases := map[string]struct {
		driver string
		attrs  map[string]string
		want   string
	}{
		"another driver":  {"other.csi.example", goodCSIAttrs(), "other.csi.example"},
		"no server":       {csiDriverName, map[string]string{"share": "/s", "subDir": "/pvcs/pvc-11111111-2222-3333-4444-555555555555"}, "server"},
		"no share":        {csiDriverName, map[string]string{"server": "10.1.2.3", "subDir": "/pvcs/pvc-11111111-2222-3333-4444-555555555555"}, "share"},
		"traversal":       {csiDriverName, map[string]string{"server": "10.1.2.3", "share": "/s", "subDir": "/pvcs/../../etc"}, "subDir"},
		"relative export": {csiDriverName, map[string]string{"server": "10.1.2.3", "share": "s", "subDir": "/pvcs/pvc-11111111-2222-3333-4444-555555555555"}, "share"},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			e := newFailover("go", v1alpha1.FailoverActionExecute)
			e.Spec.DRPool = "dr"
			ctl, c := newTestController(t, append(csiReceiverClaim(tc.driver, tc.attrs), e, freshPlan())...)
			if err := ctl.processOnce(context.Background()); err != nil {
				t.Fatal(err)
			}
			f := getFailover(t, c, "go")
			if f.Status.Phase != v1alpha1.FailoverPhaseFailed || !strings.Contains(f.Status.Message, tc.want) || !strings.Contains(f.Status.Message, "spec.drShareIP") {
				t.Errorf("status = %+v, want a failure that names %q and the override", f.Status, tc.want)
			}
			if jobFor(t, c, "go") != nil {
				t.Error("no Job should be made")
			}
		})
	}
}

func TestController_BasePathMovesTheJobMountAndThePV(t *testing.T) {
	plan := freshPlan()
	plan.Spec.BasePath = "/repl"
	e := newFailover("go", v1alpha1.FailoverActionExecute)
	e.Spec.DRPool, e.Spec.BasePath = "dr", "/repl/"
	ctl, c := newTestController(t, append(csiReceiverClaim(csiDriverName, goodCSIAttrs()), e, plan)...)
	ctx := context.Background()
	if err := ctl.processOnce(ctx); err != nil {
		t.Fatal(err)
	}
	found := false
	for _, a := range jobFor(t, c, "go").Spec.Template.Spec.Containers[0].Args {
		if a == "--nfs-mount-path=/data/repl" {
			found = true
		}
	}
	if !found {
		t.Errorf("args = %v, want --nfs-mount-path=/data/repl", jobFor(t, c, "go").Spec.Template.Spec.Containers[0].Args)
	}
	finishJob(t, c, jobFor(t, c, "go"), true, planMessage(t))
	if err := ctl.processOnce(ctx); err != nil {
		t.Fatal(err)
	}
	pv := &corev1.PersistentVolume{}
	if err := c.Get(ctx, types.NamespacedName{Name: "dr-pvc-a"}, pv); err != nil {
		t.Fatal(err)
	}
	want := "/share_abc/pvcs/pvc-11111111-2222-3333-4444-555555555555/repl/pvcs/pvc-a"
	if pv.Spec.NFS.Path != want {
		t.Errorf("pv path = %q, want %q", pv.Spec.NFS.Path, want)
	}
}

func TestController_ABadBasePathFailsTheFailover(t *testing.T) {
	for _, bp := range []string{"repl", "/a/../b", "/a b"} {
		p := newFailover("p", v1alpha1.FailoverActionPlan)
		p.Spec.BasePath = bp
		ctl, c := newTestController(t, append(receiverClaim(), p)...)
		if err := ctl.processOnce(context.Background()); err != nil {
			t.Fatal(err)
		}
		f := getFailover(t, c, "p")
		if f.Status.Phase != v1alpha1.FailoverPhaseFailed || !strings.Contains(f.Status.Message, "basePath") {
			t.Errorf("basePath %q: status = %+v", bp, f.Status)
		}
		if jobFor(t, c, "p") != nil {
			t.Errorf("basePath %q: no Job should be made", bp)
		}
	}
}
