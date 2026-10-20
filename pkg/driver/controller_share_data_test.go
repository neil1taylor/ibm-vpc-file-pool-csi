package driver

import (
	"context"
	"fmt"
	"testing"

	v1alpha1 "github.com/IBM/ibm-vpc-file-pool-csi/api/v1alpha1"
	"github.com/IBM/ibm-vpc-file-pool-csi/pkg/pool"
	"github.com/container-storage-interface/spec/lib/go/csi"
	"google.golang.org/grpc/codes"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// The CSI sidecars retry Aborted and Unavailable without treating the
// operation as finished, so data work still running must use them.

func TestDeleteVolume_DataRemovalInProgress_ReturnsAborted(t *testing.T) {
	d := newTestDriver(&mockPoolManager{deallocateErr: pool.ErrOperationInProgress}, nil)

	_, err := d.DeleteVolume(context.Background(), &csi.DeleteVolumeRequest{
		VolumeId: "test-pool/share-1/pvc-test",
	})

	assertGRPCCode(t, err, codes.Aborted)
}

func TestDeleteSnapshot_DataRemovalInProgress_ReturnsAborted(t *testing.T) {
	d := newTestDriver(&mockPoolManager{deleteSnapshotErr: pool.ErrOperationInProgress}, nil)

	_, err := d.DeleteSnapshot(context.Background(), &csi.DeleteSnapshotRequest{
		SnapshotId: "pool/share/snap-1",
	})

	assertGRPCCode(t, err, codes.Aborted)
}

func restoreVolumeRequest() *csi.CreateVolumeRequest {
	return &csi.CreateVolumeRequest{
		Name:          "pvc-restored",
		Parameters:    map[string]string{"pool": "test-pool"},
		CapacityRange: &csi.CapacityRange{RequiredBytes: gbToBytes(10)},
		VolumeContentSource: &csi.VolumeContentSource{
			Type: &csi.VolumeContentSource_Snapshot{
				Snapshot: &csi.VolumeContentSource_SnapshotSource{SnapshotId: "pool/share/snap-1"},
			},
		},
	}
}

func TestCreateVolume_FromSnapshot_CopyInProgress_ReturnsAborted(t *testing.T) {
	d := newTestDriver(&mockPoolManager{restoreSnapshotErr: pool.ErrOperationInProgress}, nil)

	_, err := d.CreateVolume(context.Background(), restoreVolumeRequest())

	assertGRPCCode(t, err, codes.Aborted)
}

func TestCreateVolume_FromSnapshot_CopyFailed_ReturnsUnavailable(t *testing.T) {
	failed := fmt.Errorf("restore snapshot: %w", pool.ErrShareJobFailed)
	d := newTestDriver(&mockPoolManager{restoreSnapshotErr: failed}, nil)

	_, err := d.CreateVolume(context.Background(), restoreVolumeRequest())

	assertGRPCCode(t, err, codes.Unavailable)
}

// A retried restore or clone must go back to the pool manager, which checks
// the copy, and its answer must name the content source. Answering from the
// existing record alone reports an unfinished copy as done, and the
// provisioner rejects an answer without the content source.
func TestCreateVolume_FromSnapshot_ExistingRecord_AsksPoolManager(t *testing.T) {
	k := newMockK8sClient()
	k.subVolumes["pvc-restored"] = &v1alpha1.SubVolume{
		ObjectMeta: metav1.ObjectMeta{Name: "pvc-restored"},
		Spec:       v1alpha1.SubVolumeSpec{PoolName: "test-pool", ShareID: "share-1", SubPath: "/pvcs/pvc-restored"},
	}
	pm := &mockPoolManager{restoreSnapshotErr: pool.ErrOperationInProgress}
	d := newTestDriver(pm, k)

	_, err := d.CreateVolume(context.Background(), restoreVolumeRequest())

	assertGRPCCode(t, err, codes.Aborted)
	if pm.lastRestoreSnapshotName == "" {
		t.Error("the pool manager was not asked about the restore")
	}
}

func TestCreateVolume_FromSnapshot_ExistingRecord_NamesContentSource(t *testing.T) {
	k := newMockK8sClient()
	k.subVolumes["pvc-restored"] = &v1alpha1.SubVolume{
		ObjectMeta: metav1.ObjectMeta{Name: "pvc-restored"},
		Spec:       v1alpha1.SubVolumeSpec{PoolName: "test-pool", ShareID: "share-1", SubPath: "/pvcs/pvc-restored"},
	}
	pm := &mockPoolManager{restoreSnapshotResult: &pool.AllocationResult{
		ShareID: "share-1", MountTargetIP: "10.240.0.1", SubPath: "/pvcs/pvc-restored", SharePath: "/share_one",
	}}
	d := newTestDriver(pm, k)

	resp, err := d.CreateVolume(context.Background(), restoreVolumeRequest())
	if err != nil {
		t.Fatalf("CreateVolume: %v", err)
	}
	if resp.GetVolume().GetContentSource().GetSnapshot() == nil {
		t.Error("answer for a restored volume does not name its snapshot")
	}
}
