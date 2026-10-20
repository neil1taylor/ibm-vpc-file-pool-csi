package driver

import (
	"context"
	"fmt"
	"testing"

	"github.com/IBM/ibm-vpc-file-pool-csi/pkg/pool"
	"github.com/container-storage-interface/spec/lib/go/csi"
	"google.golang.org/grpc/codes"
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
