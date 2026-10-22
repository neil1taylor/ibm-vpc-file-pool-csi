package pool

import (
	"context"
	"testing"

	"github.com/IBM/ibm-vpc-file-pool-csi/pkg/vmcopy"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

func replTempCount(t *testing.T, dc client.Client) int {
	t.Helper()
	ctx := context.Background()
	sel := client.MatchingLabels{replTempLabel: "true"}
	jobs, pvcs, pvs := &batchv1.JobList{}, &corev1.PersistentVolumeClaimList{}, &corev1.PersistentVolumeList{}
	for _, l := range []client.ObjectList{jobs, pvcs, pvs} {
		if err := dc.List(ctx, l, sel); err != nil {
			t.Fatal(err)
		}
	}
	return len(jobs.Items) + len(pvcs.Items) + len(pvs.Items)
}

// Deleting a policy must remove every Job, PVC and PV its plain-volume copies
// made, in direct NFS mode and in receiver mode (#214).
func TestReplicationController_PolicyDeletedRemovesPlainVolumeResources(t *testing.T) {
	for _, mode := range []string{"direct", "receiver"} {
		t.Run(mode, func(t *testing.T) {
			k := newReplFakeK8sClient()
			dc := newReplFakeDirectClient()
			sv := newTestSubVolume("pvc-11111111-1111-1111-1111-111111111111", "prod-pool", "share-1", 10)
			sv.Spec.ShareMountTargetIP = "10.240.0.1"
			sv.Spec.ShareExportPath = "/share_abc123"
			k.addSubVolume(sv)

			policy := newTestReplicationPolicy("gone", "prod-pool", "1h")
			if mode == "receiver" {
				policy = newTestReceiverModePolicy("gone", "prod-pool", "1h")
			}
			k.addPolicy(policy)
			rc := newReplController(k, dc, newFakeNFSOperations())
			rc.SetReplicationImage("driver:latest")
			ctx := context.Background()

			rc.processOnce(ctx)
			if n := replTempCount(t, dc); n == 0 {
				t.Fatal("expected temporary resources after the first cycle")
			}

			if err := k.DeleteReplicationPolicy(ctx, "gone"); err != nil {
				t.Fatal(err)
			}
			rc.processOnce(ctx)
			if n := replTempCount(t, dc); n != 0 {
				t.Errorf("%d temporary resources left after the policy was deleted", n)
			}
		})
	}
}

// A plain volume beside a VM disk that is being copied must show its result
// on the first pass after its Job finishes, not at the next due cycle (#215).
func TestVMCopy_PlainVolumeStatusFollowsItsJobBesideAVMCopy(t *testing.T) {
	e := newVMCopyEnv(t)
	plain := newTestSubVolume("pvc-plain-0000-0000-0000-000000000003", "prod-pool", "share-1", 5)
	plain.Spec.PVCName = "plain-pvc"
	e.k.addSubVolume(plain)

	e.cop.state = vmcopy.Running
	e.rc.processOnce(e.ctx) // the VM copy starts, the plain Job is made
	job := e.job(t, plain.Name)
	if job == nil {
		t.Fatal("no Job for the plain volume")
	}
	job.Status.Succeeded = 1
	if err := e.dc.Status().Update(e.ctx, job); err != nil {
		t.Fatal(err)
	}

	e.tick() // 30 seconds later, far from the next due cycle
	if s := e.status(t, plain.Name); s.LastSyncTime == nil || s.LastError != "" {
		t.Errorf("plain volume status = %+v, want synced after its Job finished", s)
	}
	if e.job(t, plain.Name) != nil {
		t.Error("the finished plain Job must be cleaned up")
	}
}
