package pool

import (
	"context"
	"strings"
	"testing"
	"time"

	v1alpha1 "github.com/IBM/ibm-vpc-file-pool-csi/api/v1alpha1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	fakeclient "sigs.k8s.io/controller-runtime/pkg/client/fake"
)

var vmGVK = schema.GroupVersionKind{Group: "kubevirt.io", Version: "v1", Kind: "VirtualMachine"}

type backupEnv struct {
	t   *testing.T
	c   client.Client
	bc  *BackupController
	ctx context.Context
}

func newBackupEnv(t *testing.T) *backupEnv {
	t.Helper()
	s := runtime.NewScheme()
	_ = corev1.AddToScheme(s)
	_ = v1alpha1.AddToScheme(s)
	s.AddKnownTypeWithName(vmGVK, &unstructured.Unstructured{})
	s.AddKnownTypeWithName(vmGVK.GroupVersion().WithKind("VirtualMachineList"), &unstructured.UnstructuredList{})
	c := fakeclient.NewClientBuilder().WithScheme(s).
		WithStatusSubresource(&v1alpha1.BackupPolicy{}, &v1alpha1.ReplicationPolicy{}).Build()
	e := &backupEnv{t: t, c: c, bc: NewBackupController(c), ctx: context.Background()}
	e.addPool("backups", "10.240.9.9", "/export_backups")
	e.addPool("data-a", "10.240.1.1", "/export_a")
	return e
}

func (e *backupEnv) create(obj client.Object) {
	e.t.Helper()
	if err := e.c.Create(e.ctx, obj); err != nil {
		e.t.Fatalf("create %T %s: %v", obj, obj.GetName(), err)
	}
}

func (e *backupEnv) addPool(name, ip, export string) {
	e.t.Helper()
	p := &v1alpha1.FileSharePool{ObjectMeta: metav1.ObjectMeta{Name: name}}
	p.Status.Phase = "Ready"
	p.Status.Shares = []v1alpha1.PoolShareStatus{{ShareID: name + "-share", MountTargetIP: ip, ExportPath: export, State: "Stable", TotalGB: 100}}
	e.create(p)
}

// addClaim makes a PVC with labels and the SubVolume that backs it.
func (e *backupEnv) addClaim(ns, name, pool string, labels map[string]string) *v1alpha1.SubVolume {
	e.t.Helper()
	e.create(&corev1.PersistentVolumeClaim{ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: name, Labels: labels},
		Status: corev1.PersistentVolumeClaimStatus{Phase: corev1.ClaimBound}})
	sv := &v1alpha1.SubVolume{
		ObjectMeta: metav1.ObjectMeta{Name: "pvc-" + ns + "-" + name, Labels: map[string]string{"storage.ibmcloud.io/pool": pool}},
		Spec:       v1alpha1.SubVolumeSpec{PoolName: pool, PVCNamespace: ns, PVCName: name, SubPath: "/pvcs/pvc-" + ns + "-" + name},
	}
	e.create(sv)
	return sv
}

// addVM makes a VirtualMachine that uses the given claims as disks.
func (e *backupEnv) addVM(ns, name string, claims ...string) {
	e.t.Helper()
	var vols []any
	for i, c := range claims {
		key := "persistentVolumeClaim"
		if i%2 == 1 {
			key = "dataVolume" // a DataVolume has the name of its claim
		}
		field := "claimName"
		if key == "dataVolume" {
			field = "name"
		}
		vols = append(vols, map[string]any{"name": "disk" + c, key: map[string]any{field: c}})
	}
	vm := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "kubevirt.io/v1", "kind": "VirtualMachine",
		"metadata": map[string]any{"name": name, "namespace": ns},
		"spec":     map[string]any{"template": map[string]any{"spec": map[string]any{"volumes": vols}}},
	}}
	e.create(vm)
}

func (e *backupEnv) policy(name string, mutate func(*v1alpha1.BackupPolicy)) *v1alpha1.BackupPolicy {
	e.t.Helper()
	bp := &v1alpha1.BackupPolicy{
		ObjectMeta: metav1.ObjectMeta{Name: name, UID: types.UID("uid-" + name)},
		Spec: v1alpha1.BackupPolicySpec{
			SnapshotPool: "backups", Schedule: "6h", KeepFor: "7d",
			Selector: &metav1.LabelSelector{MatchLabels: map[string]string{"backup": "yes"}},
		},
	}
	if mutate != nil {
		mutate(bp)
	}
	e.create(bp)
	return bp
}

func (e *backupEnv) reconcile() { e.bc.ReconcileAll(e.ctx) }

func (e *backupEnv) bp(name string) *v1alpha1.BackupPolicy {
	e.t.Helper()
	bp := &v1alpha1.BackupPolicy{}
	if err := e.c.Get(e.ctx, types.NamespacedName{Name: name}, bp); err != nil {
		e.t.Fatal(err)
	}
	return bp
}

func (e *backupEnv) sv(name string) *v1alpha1.SubVolume {
	e.t.Helper()
	sv := &v1alpha1.SubVolume{}
	if err := e.c.Get(e.ctx, types.NamespacedName{Name: name}, sv); err != nil {
		e.t.Fatal(err)
	}
	return sv
}

func (e *backupEnv) repls() []v1alpha1.ReplicationPolicy {
	e.t.Helper()
	l := &v1alpha1.ReplicationPolicyList{}
	if err := e.c.List(e.ctx, l); err != nil {
		e.t.Fatal(err)
	}
	return l.Items
}

var yes = map[string]string{"backup": "yes"}

func TestBackupController_MakesAnOwnedReplicationPolicy(t *testing.T) {
	e := newBackupEnv(t)
	sv := e.addClaim("apps", "db", "data-a", yes)
	e.addClaim("apps", "cache", "data-a", map[string]string{"backup": "no"})
	e.policy("nightly", func(p *v1alpha1.BackupPolicy) {
		p.Spec.KeepFor = "30d"
		s := int32(50)
		p.Spec.VMCopySpeedLimitMBps = &s
	})
	e.reconcile()

	rps := e.repls()
	if len(rps) != 1 {
		t.Fatalf("replication policies = %d, want 1", len(rps))
	}
	rp := rps[0]
	if rp.Name != "backup-nightly-data-a" {
		t.Errorf("name = %q", rp.Name)
	}
	sp := rp.Spec
	if sp.SourcePoolName != "data-a" || sp.DestinationNFSServer != "10.240.9.9" || sp.DestinationExportPath != "/export_backups" ||
		sp.DestinationBasePath != "/backup/nightly" || sp.Schedule != "6h" {
		t.Errorf("spec = %+v", sp)
	}
	if sp.RestorePointKeepHours == nil || *sp.RestorePointKeepHours != 30*24 {
		t.Errorf("restorePointKeepHours = %v, want 720", sp.RestorePointKeepHours)
	}
	if sp.VMCopySpeedLimitMBps == nil || *sp.VMCopySpeedLimitMBps != 50 {
		t.Errorf("speed = %v, want 50", sp.VMCopySpeedLimitMBps)
	}
	if sp.SubVolumeSelector == nil || sp.SubVolumeSelector.MatchLabels[backupPolicyLabel] != "nightly" {
		t.Errorf("selector = %+v, want the backup-policy label", sp.SubVolumeSelector)
	}
	if len(rp.OwnerReferences) != 1 || rp.OwnerReferences[0].Name != "nightly" || rp.OwnerReferences[0].Kind != "BackupPolicy" {
		t.Errorf("owner = %+v", rp.OwnerReferences)
	}
	if rp.Labels[backupPolicyLabel] != "nightly" {
		t.Errorf("labels = %v", rp.Labels)
	}
	if got := e.sv(sv.Name).Labels[backupPolicyLabel]; got != "nightly" {
		t.Errorf("selected SubVolume label = %q", got)
	}
	if got := e.sv("pvc-apps-cache").Labels[backupPolicyLabel]; got != "" {
		t.Errorf("an unselected SubVolume got the label %q", got)
	}
	st := e.bp("nightly").Status
	if st.Phase != "Active" || st.VolumeCount != 1 || len(st.ReplicationPolicies) != 1 {
		t.Errorf("status = %+v", st)
	}
}

func TestBackupController_Namespaces(t *testing.T) {
	e := newBackupEnv(t)
	e.addClaim("apps", "db", "data-a", yes)
	e.addClaim("other", "db", "data-a", yes)
	e.policy("nightly", func(p *v1alpha1.BackupPolicy) { p.Spec.Namespaces = []string{"apps"} })
	e.reconcile()
	if got := e.bp("nightly").Status.VolumeCount; got != 1 {
		t.Errorf("volumes = %d, want 1: only namespace apps", got)
	}
	if e.sv("pvc-other-db").Labels[backupPolicyLabel] != "" {
		t.Error("a claim outside the namespaces was selected")
	}
}

func TestBackupController_ABackedUpVMTakesAllItsDisks(t *testing.T) {
	e := newBackupEnv(t)
	e.addClaim("vms", "web-root", "data-a", yes)
	e.addClaim("vms", "web-data", "data-a", nil) // no label, but a disk of the same VM
	e.addClaim("vms", "other", "data-a", nil)
	e.addVM("vms", "web", "web-root", "web-data")
	e.policy("nightly", nil)
	e.reconcile()
	if e.sv("pvc-vms-web-data").Labels[backupPolicyLabel] != "nightly" {
		t.Error("the second disk of the VM was not selected")
	}
	if e.sv("pvc-vms-other").Labels[backupPolicyLabel] != "" {
		t.Error("a claim that no selected VM uses was selected")
	}
	vols := e.bp("nightly").Status.Volumes
	if len(vols) != 2 {
		t.Fatalf("volumes = %+v", vols)
	}
	for _, v := range vols {
		if v.VMName != "web" {
			t.Errorf("volume %s: vmName = %q, want web", v.PVCName, v.VMName)
		}
	}
}

func TestBackupController_OnePolicyPerSourcePool(t *testing.T) {
	e := newBackupEnv(t)
	e.addPool("data-b", "10.240.2.2", "/export_b")
	e.addClaim("apps", "a", "data-a", yes)
	e.addClaim("apps", "b", "data-b", yes)
	e.policy("nightly", nil)
	e.reconcile()
	if got := len(e.repls()); got != 2 {
		t.Fatalf("replication policies = %d, want 2", got)
	}
}

func TestBackupController_DropsAVolumeThatIsNoLongerSelected(t *testing.T) {
	e := newBackupEnv(t)
	e.addClaim("apps", "db", "data-a", yes)
	e.policy("nightly", nil)
	e.reconcile()

	pvc := &corev1.PersistentVolumeClaim{}
	if err := e.c.Get(e.ctx, types.NamespacedName{Namespace: "apps", Name: "db"}, pvc); err != nil {
		t.Fatal(err)
	}
	pvc.Labels = map[string]string{}
	if err := e.c.Update(e.ctx, pvc); err != nil {
		t.Fatal(err)
	}
	e.reconcile()
	if e.sv("pvc-apps-db").Labels[backupPolicyLabel] != "" {
		t.Error("the label stayed on a volume that is no longer selected")
	}
	if got := len(e.repls()); got != 0 {
		t.Errorf("replication policies = %d, want 0: no volume left in the pool", got)
	}
	if got := e.bp("nightly").Status.VolumeCount; got != 0 {
		t.Errorf("volumes = %d", got)
	}
}

func TestBackupController_AVolumeBelongsToOnePolicy(t *testing.T) {
	e := newBackupEnv(t)
	e.addClaim("apps", "db", "data-a", yes)
	e.policy("first", nil)
	e.reconcile()
	e.policy("second", nil)
	e.reconcile()
	if got := e.sv("pvc-apps-db").Labels[backupPolicyLabel]; got != "first" {
		t.Errorf("label = %q, want first: the second policy must not take the volume", got)
	}
	st := e.bp("second").Status
	if st.VolumeCount != 0 || !strings.Contains(st.Message, "first") {
		t.Errorf("second status = %+v, want it to name the policy that has the volume", st)
	}
}

func TestBackupController_SnapshotPoolProblemsAreReported(t *testing.T) {
	cases := []struct {
		name  string
		setup func(*backupEnv)
		want  string
	}{
		{"missing pool", func(e *backupEnv) {
			e.policy("nightly", func(p *v1alpha1.BackupPolicy) { p.Spec.SnapshotPool = "nope" })
		}, "nope"},
		{"pool not ready", func(e *backupEnv) {
			p := &v1alpha1.FileSharePool{}
			_ = e.c.Get(e.ctx, types.NamespacedName{Name: "backups"}, p)
			p.Status.Shares[0].State = "Creating"
			_ = e.c.Update(e.ctx, p)
			e.policy("nightly", nil)
		}, "share"},
		{"pool with workload claims", func(e *backupEnv) {
			e.addClaim("apps", "intruder", "backups", nil)
			e.policy("nightly", nil)
		}, "claim"},
		{"pool with two shares", func(e *backupEnv) {
			p := &v1alpha1.FileSharePool{}
			_ = e.c.Get(e.ctx, types.NamespacedName{Name: "backups"}, p)
			p.Status.Shares = append(p.Status.Shares, v1alpha1.PoolShareStatus{ShareID: "second", MountTargetIP: "10.0.0.5", State: "Stable"})
			_ = e.c.Update(e.ctx, p)
			e.policy("nightly", nil)
		}, "one share"},
	}
	for _, tc := range cases {
		e := newBackupEnv(t)
		e.addClaim("apps", "db", "data-a", yes)
		tc.setup(e)
		e.reconcile()
		st := e.bp("nightly").Status
		if st.Phase != "Error" || !strings.Contains(st.Message, tc.want) {
			t.Errorf("%s: status = %+v, want Error naming %q", tc.name, st, tc.want)
		}
		if len(e.repls()) != 0 {
			t.Errorf("%s: no replication policy may run against a bad snapshot pool", tc.name)
		}
	}
}

func TestBackupController_SuspendPausesTheOwnedPolicies(t *testing.T) {
	e := newBackupEnv(t)
	e.addClaim("apps", "db", "data-a", yes)
	e.policy("nightly", nil)
	e.reconcile()

	bp := e.bp("nightly")
	bp.Spec.Suspend = true
	if err := e.c.Update(e.ctx, bp); err != nil {
		t.Fatal(err)
	}
	e.reconcile()
	if got := e.repls()[0].Annotations[pausedAnnotation]; got != "true" {
		t.Errorf("paused annotation = %q, want true", got)
	}
	if st := e.bp("nightly").Status; st.Phase != "Suspended" {
		t.Errorf("phase = %q, want Suspended", st.Phase)
	}

	bp = e.bp("nightly")
	bp.Spec.Suspend = false
	if err := e.c.Update(e.ctx, bp); err != nil {
		t.Fatal(err)
	}
	e.reconcile()
	if got := e.repls()[0].Annotations[pausedAnnotation]; got != "" {
		t.Errorf("paused annotation = %q after resume, want none", got)
	}
}

func TestBackupController_FollowsChangesToThePolicy(t *testing.T) {
	e := newBackupEnv(t)
	e.addClaim("apps", "db", "data-a", yes)
	e.policy("nightly", nil)
	e.reconcile()
	bp := e.bp("nightly")
	bp.Spec.Schedule = "2h"
	bp.Spec.KeepFor = "48h"
	if err := e.c.Update(e.ctx, bp); err != nil {
		t.Fatal(err)
	}
	e.reconcile()
	rp := e.repls()[0]
	if rp.Spec.Schedule != "2h" || *rp.Spec.RestorePointKeepHours != 48 {
		t.Errorf("owned policy not updated: %+v", rp.Spec)
	}
}

func TestBackupController_ClearsLabelsOfADeletedPolicy(t *testing.T) {
	e := newBackupEnv(t)
	e.addClaim("apps", "db", "data-a", yes)
	e.policy("nightly", nil)
	e.reconcile()
	if err := e.c.Delete(e.ctx, e.bp("nightly")); err != nil {
		t.Fatal(err)
	}
	e.reconcile()
	if got := e.sv("pvc-apps-db").Labels[backupPolicyLabel]; got != "" {
		t.Errorf("label = %q: a deleted policy must not keep volumes", got)
	}
	if got := len(e.repls()); got != 0 {
		t.Errorf("replication policies = %d, want 0 (the owner is gone)", got)
	}
}

func TestBackupController_ReportsEachVolumeFromTheReplicationStatus(t *testing.T) {
	e := newBackupEnv(t)
	sv := e.addClaim("apps", "db", "data-a", yes)
	e.policy("nightly", nil)
	e.reconcile()

	rp := e.repls()[0]
	moment := metav1.NewTime(time.Date(2026, 10, 21, 9, 0, 0, 0, time.UTC))
	oldest := metav1.NewTime(time.Date(2026, 10, 20, 9, 0, 0, 0, time.UTC))
	n := int64(1234)
	rp.Status.SubVolumeStatuses = []v1alpha1.SubVolumeReplicationStatus{{
		SubVolumeName: sv.Name, LastSyncTime: &moment, CopyMoment: &moment, BytesSynced: &n,
		RestorePoints: 3, OldestRestorePoint: &oldest, Consistency: "FileSystemConsistent", LastError: "",
	}}
	if err := e.c.Status().Update(e.ctx, &rp); err != nil {
		t.Fatal(err)
	}
	e.reconcile()
	vols := e.bp("nightly").Status.Volumes
	if len(vols) != 1 {
		t.Fatalf("volumes = %+v", vols)
	}
	v := vols[0]
	if v.PVCNamespace != "apps" || v.PVCName != "db" || v.SubVolumeName != sv.Name ||
		v.LastBackupTime == nil || !v.LastBackupTime.Equal(&moment) ||
		v.OldestBackupTime == nil || !v.OldestBackupTime.Equal(&oldest) ||
		v.RestorePoints != 3 || v.BytesWritten != 1234 || v.Consistency != "FileSystemConsistent" {
		t.Errorf("volume status = %+v", v)
	}
}

func TestBackupController_NonVMVolumesAreReportedAsFileCopies(t *testing.T) {
	e := newBackupEnv(t)
	sv := e.addClaim("apps", "db", "data-a", yes)
	e.policy("nightly", nil)
	e.reconcile()
	rp := e.repls()[0]
	moment := metav1.NewTime(time.Date(2026, 10, 21, 9, 0, 0, 0, time.UTC))
	rp.Status.SubVolumeStatuses = []v1alpha1.SubVolumeReplicationStatus{{SubVolumeName: sv.Name, LastSyncTime: &moment}}
	if err := e.c.Status().Update(e.ctx, &rp); err != nil {
		t.Fatal(err)
	}
	e.reconcile()
	if got := e.bp("nightly").Status.Volumes[0].Consistency; got != "FileCopy" {
		t.Errorf("consistency = %q, want FileCopy: only VM disks get the freeze", got)
	}
}

func TestBackupController_LeavesOtherReplicationPoliciesAlone(t *testing.T) {
	e := newBackupEnv(t)
	e.create(&v1alpha1.ReplicationPolicy{
		ObjectMeta: metav1.ObjectMeta{Name: "dr-policy"},
		Spec:       v1alpha1.ReplicationPolicySpec{SourcePoolName: "data-a", DestinationNFSServer: "1.1.1.1", DestinationBasePath: "/repl", Schedule: "1h"},
	})
	e.addClaim("apps", "db", "data-a", yes)
	e.policy("nightly", nil)
	e.reconcile()
	e.reconcile()
	rp := &v1alpha1.ReplicationPolicy{}
	if err := e.c.Get(e.ctx, types.NamespacedName{Name: "dr-policy"}, rp); apierrors.IsNotFound(err) {
		t.Fatal("the controller deleted a policy it does not own")
	}
}
