package driver

import (
	"context"
	"fmt"
	"testing"

	"github.com/IBM/ibm-vpc-file-pool-csi/pkg/pool"
	"github.com/container-storage-interface/spec/lib/go/csi"
	"google.golang.org/grpc/codes"
)

func TestCreateSnapshot_InProgressIsAborted(t *testing.T) {
	pm := &mockPoolManager{createSnapshotErr: fmt.Errorf("%w: the VM started", pool.ErrOperationInProgress)}
	d := newTestDriver(pm, nil)
	_, err := d.CreateSnapshot(context.Background(), &csi.CreateSnapshotRequest{Name: "snap-test", SourceVolumeId: "pool/share/pvc-test"})
	assertGRPCCode(t, err, codes.Aborted)
}

func TestCreateVolume_FromSnapshot_TooSmallIsOutOfRange(t *testing.T) {
	pm := &mockPoolManager{restoreSnapshotErr: fmt.Errorf("%w: 10 GB snapshot, 5 GB asked", pool.ErrRestoreTooSmall)}
	d := newTestDriver(pm, nil)
	_, err := d.CreateVolume(context.Background(), &csi.CreateVolumeRequest{
		Name:          "pvc-restored",
		Parameters:    map[string]string{"pool": "test-pool"},
		CapacityRange: &csi.CapacityRange{RequiredBytes: gbToBytes(5)},
		VolumeContentSource: &csi.VolumeContentSource{Type: &csi.VolumeContentSource_Snapshot{
			Snapshot: &csi.VolumeContentSource_SnapshotSource{SnapshotId: "test-pool/share-1/snap-test"},
		}},
	})
	assertGRPCCode(t, err, codes.OutOfRange)
}

func TestCreateVolume_FromClone_UnsupportedIsFailedPrecondition(t *testing.T) {
	pm := &mockPoolManager{cloneVolumeErr: fmt.Errorf("%w: the disk has instant snapshots", pool.ErrSnapshotUnsupported)}
	d := newTestDriver(pm, nil)
	_, err := d.CreateVolume(context.Background(), &csi.CreateVolumeRequest{
		Name:          "pvc-clone",
		Parameters:    map[string]string{"pool": "test-pool"},
		CapacityRange: &csi.CapacityRange{RequiredBytes: gbToBytes(10)},
		VolumeContentSource: &csi.VolumeContentSource{Type: &csi.VolumeContentSource_Volume{
			Volume: &csi.VolumeContentSource_VolumeSource{VolumeId: "pool/share/pvc-source"},
		}},
	})
	assertGRPCCode(t, err, codes.FailedPrecondition)
}
