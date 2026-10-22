package driver

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/IBM/ibm-vpc-file-pool-csi/pkg/pool"
	"github.com/container-storage-interface/spec/lib/go/csi"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// Every way of creating a volume (new, from a snapshot, from a clone) must map
// the same pool errors to the same gRPC codes. A pool that is still growing, or
// whose shares have no mount target yet, is a retriable condition.
func TestCreateVolume_PoolErrorsMapTheSameForEverySource(t *testing.T) {
	noUsable := fmt.Errorf("%w: share-x", pool.ErrNoUsableShare)
	errs := []struct {
		err  error
		code codes.Code
	}{
		{pool.ErrPoolExhausted, codes.ResourceExhausted},
		{pool.ErrPoolExpanding, codes.Unavailable},
		{pool.ErrShareCreationPending, codes.Unavailable},
		{noUsable, codes.Unavailable},
	}
	sources := map[string]func(err error) (*mockPoolManager, *csi.VolumeContentSource){
		"new": func(err error) (*mockPoolManager, *csi.VolumeContentSource) {
			return &mockPoolManager{allocateErr: err}, nil
		},
		"snapshot": func(err error) (*mockPoolManager, *csi.VolumeContentSource) {
			return &mockPoolManager{restoreSnapshotErr: err}, &csi.VolumeContentSource{
				Type: &csi.VolumeContentSource_Snapshot{Snapshot: &csi.VolumeContentSource_SnapshotSource{SnapshotId: "p/s/snap"}},
			}
		},
		"clone": func(err error) (*mockPoolManager, *csi.VolumeContentSource) {
			return &mockPoolManager{cloneVolumeErr: err}, &csi.VolumeContentSource{
				Type: &csi.VolumeContentSource_Volume{Volume: &csi.VolumeContentSource_VolumeSource{VolumeId: "p/s/pvc-src"}},
			}
		},
	}
	for name, source := range sources {
		for _, e := range errs {
			t.Run(name+"/"+e.err.Error(), func(t *testing.T) {
				pm, content := source(e.err)
				_, err := newTestDriver(pm, nil).CreateVolume(context.Background(), &csi.CreateVolumeRequest{
					Name:                "pvc-test",
					Parameters:          map[string]string{"pool": "p"},
					CapacityRange:       &csi.CapacityRange{RequiredBytes: gbToBytes(10)},
					VolumeContentSource: content,
				})
				assertGRPCCode(t, err, e.code)
			})
		}
	}
}

func TestCreateVolume_NoUsableShareNamesTheShares(t *testing.T) {
	pm := &mockPoolManager{allocateErr: fmt.Errorf("%w: share-x", pool.ErrNoUsableShare)}
	_, err := newTestDriver(pm, nil).CreateVolume(context.Background(), &csi.CreateVolumeRequest{
		Name:          "pvc-test",
		Parameters:    map[string]string{"pool": "p"},
		CapacityRange: &csi.CapacityRange{RequiredBytes: gbToBytes(10)},
	})
	if msg := status.Convert(err).Message(); !strings.Contains(msg, "share-x") {
		t.Errorf("message %q does not name the share without a mount target", msg)
	}
}
