package pool

import (
	"context"
	"strings"
	"testing"

	batchv1 "k8s.io/api/batch/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/types"
)

// A stopped VM's disk with instant snapshots is a chain of files. A file copy
// of its folder would give the far site an old disk.img, so it is not copied.
func TestReplicationController_SkipsAStoppedDiskWithInstantSnapshots(t *testing.T) {
	k := newReplFakeK8sClient()
	dc := newReplFakeDirectClient()
	name := "pvc-11111111-1111-1111-1111-111111111111"
	sv := newTestSubVolume(name, "prod-pool", "share-1", 10)
	sv.Status.DiskChain = []string{"snap-0a1b2c3d4e5f6a7b.qcow2", "disk.img"}
	k.addSubVolume(sv)
	policy := newTestReplicationPolicy("prod-to-dr", "prod-pool", "1h")
	k.addPolicy(policy)
	rc := newReplController(k, dc, newFakeNFSOperations())

	rc.processPolicy(context.Background(), k.getPolicy("prod-to-dr"))

	err := dc.Get(context.Background(), types.NamespacedName{Namespace: replJobNamespace, Name: replJobName("prod-to-dr", name)}, &batchv1.Job{})
	if !apierrors.IsNotFound(err) {
		t.Fatalf("no copy Job may start for a chained disk, got err=%v", err)
	}
	var msg string
	for _, st := range k.getPolicy("prod-to-dr").Status.SubVolumeStatuses {
		if st.SubVolumeName == name {
			msg = st.LastError
		}
	}
	if !strings.Contains(msg, "instant snapshots") {
		t.Errorf("status must say why the disk was not copied, got %q", msg)
	}
}
