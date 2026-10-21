package pool

import (
	"context"
	"strings"
	"testing"

	v1alpha1 "github.com/IBM/ibm-vpc-file-pool-csi/api/v1alpha1"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
)

const momentsSV = "pvc-11111111-1111-1111-1111-111111111111"

func syncScript(t *testing.T, mutate func(*v1alpha1.ReplicationPolicy)) string {
	t.Helper()
	k := newReplFakeK8sClient()
	dc := newReplFakeDirectClient()
	k.addSubVolume(newTestSubVolume(momentsSV, "prod-pool", "share-1", 10))
	p := newTestReplicationPolicy("keep", "prod-pool", "1ms")
	if mutate != nil {
		mutate(p)
	}
	k.addPolicy(p)
	rc := newReplController(k, dc, newFakeNFSOperations())
	rc.processOnce(context.Background())
	job := &batchv1.Job{}
	if err := dc.Get(context.Background(), types.NamespacedName{Namespace: replJobNamespace, Name: replJobName("keep", momentsSV)}, job); err != nil {
		t.Fatal(err)
	}
	return strings.Join(job.Spec.Template.Spec.Containers[0].Command, " ")
}

func TestReplicationJob_KeepsMomentsAsHardLinks(t *testing.T) {
	script := syncScript(t, func(p *v1alpha1.ReplicationPolicy) { p.Spec.MomentKeepHours = int32Ptr(48) })
	for _, want := range []string{
		`cp -a -l '/dest/pvcs/pvcs/pvc-11111111-1111-1111-1111-111111111111'`,
		"/dest/pvcs/.moments/pvcs/pvc-11111111-1111-1111-1111-111111111111",
		`date -u -d "-48 hours"`,
		"/dev/termination-log",
	} {
		if !strings.Contains(script, want) {
			t.Errorf("script lacks %q:\n%s", want, script)
		}
	}
	// rsync writes a changed file as a new file, so an older moment keeps the
	// old one. The metadata file is written the same way.
	if strings.Contains(script, "--inplace") {
		t.Error("rsync must not write files in place: that would change older moments")
	}
	if !strings.Contains(script, ".subvolume-metadata.json.tmp") || !strings.Contains(script, "mv ") {
		t.Errorf("the metadata file must be replaced, not rewritten, or older moments change:\n%s", script)
	}
}

func TestReplicationJob_NoMomentsByDefault(t *testing.T) {
	script := syncScript(t, nil)
	if strings.Contains(script, ".moments") || strings.Contains(script, "cp -a -l") {
		t.Errorf("a policy without momentKeepHours must not keep moments:\n%s", script)
	}
}

func TestReplicationJob_MomentsNeedRsyncEvenIfIncrementalIsOff(t *testing.T) {
	off := false
	script := syncScript(t, func(p *v1alpha1.ReplicationPolicy) {
		p.Spec.MomentKeepHours = int32Ptr(24)
		p.Spec.IncrementalSync = &off
	})
	if !strings.Contains(script, "rsync -a --delete") {
		t.Errorf("cp writes into the same files and would change older moments; moments need rsync:\n%s", script)
	}
}

func TestReplicationController_FileMomentsAreReported(t *testing.T) {
	k := newReplFakeK8sClient()
	dc := newReplFakeDirectClient()
	k.addSubVolume(newTestSubVolume(momentsSV, "prod-pool", "share-1", 10))
	p := newTestReplicationPolicy("keep", "prod-pool", "1ms")
	p.Spec.MomentKeepHours = int32Ptr(24)
	k.addPolicy(p)
	rc := newReplController(k, dc, newFakeNFSOperations())
	ctx := context.Background()
	rc.processOnce(ctx)

	jobName := replJobName("keep", momentsSV)
	job := &batchv1.Job{}
	if err := dc.Get(ctx, types.NamespacedName{Namespace: replJobNamespace, Name: jobName}, job); err != nil {
		t.Fatal(err)
	}
	job.Status.Succeeded = 1
	if err := dc.Status().Update(ctx, job); err != nil {
		t.Fatal(err)
	}
	// Nothing was written (no bytes), but three moments are kept.
	if err := dc.Create(ctx, &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: jobName + "-pod", Namespace: replJobNamespace, Labels: job.Spec.Template.Labels},
		Status: corev1.PodStatus{ContainerStatuses: []corev1.ContainerStatus{{
			Name: "repl-sync",
			State: corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{
				Message: `{"bytesWritten":0,"restorePoints":3,"oldestRestorePoint":"2026-10-20T09:00:00Z"}`,
			}},
		}}},
	}); err != nil {
		t.Fatal(err)
	}
	rc.mu.Lock()
	delete(rc.lastSyncTimes, "keep")
	rc.mu.Unlock()
	rc.processOnce(ctx)

	var got *v1alpha1.SubVolumeReplicationStatus
	for _, s := range k.getPolicy("keep").Status.SubVolumeStatuses {
		if s.SubVolumeName == momentsSV {
			s := s
			got = &s
		}
	}
	if got == nil {
		t.Fatal("no status for the volume")
	}
	if got.RestorePoints != 3 || got.OldestRestorePoint == nil || got.OldestRestorePoint.UTC().Format("2006-01-02T15:04:05Z") != "2026-10-20T09:00:00Z" {
		t.Errorf("status = %+v, want 3 moments with the oldest at 2026-10-20T09:00:00Z", got)
	}
}
