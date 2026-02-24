package failover

import (
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/IBM/ibm-vpc-file-pool-csi/pkg/pool"
)

// FailoverPlan describes the SubVolumes discovered on the destination NFS
// and the computed RPO (recovery point objective).
type FailoverPlan struct {
	// SubVolumes lists each discovered SubVolume with its metadata.
	SubVolumes []FailoverSubVolumePlan

	// RPO is the age of the oldest LastSyncTime across all discovered SubVolumes.
	// A larger RPO means more potential data loss.
	RPO time.Duration

	// NFSMountPath is the NFS mount path that was scanned.
	NFSMountPath string
}

// FailoverSubVolumePlan describes a single SubVolume to be failed over.
type FailoverSubVolumePlan struct {
	SubVolumeName string
	PVCName       string
	PVCNamespace  string
	RequestedGB   int64
	SubPath       string
	UID           *int64
	GID           *int64
	Permissions   string
	LastSyncTime  time.Time
	PoolName      string
	ShareID       string
	PolicyName    string
}

// Planner scans the destination NFS mount for .subvolume-metadata.json files
// and builds a FailoverPlan.
type Planner struct {
	nfsOps pool.NFSOperations
}

// NewPlanner creates a planner that reads metadata from the filesystem.
func NewPlanner(nfsOps pool.NFSOperations) *Planner {
	return &Planner{nfsOps: nfsOps}
}

// Plan scans nfsMountPath/pvcs/ for SubVolume metadata files and returns a FailoverPlan.
func (p *Planner) Plan(nfsMountPath string) (*FailoverPlan, error) {
	pvcsDir := filepath.Join(nfsMountPath, "pvcs")

	entries, err := os.ReadDir(pvcsDir)
	if err != nil {
		if os.IsNotExist(err) {
			return &FailoverPlan{NFSMountPath: nfsMountPath}, nil
		}
		return nil, fmt.Errorf("read pvcs directory %s: %w", pvcsDir, err)
	}

	plan := &FailoverPlan{
		NFSMountPath: nfsMountPath,
	}

	var oldestSync time.Time
	now := time.Now()

	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}

		subDir := filepath.Join(pvcsDir, entry.Name())
		meta, err := pool.ReadMetadataFromPath(p.nfsOps, subDir)
		if err != nil {
			// Skip directories without valid metadata (corrupt JSON, missing file).
			fmt.Fprintf(os.Stderr, "Warning: skipping %s: %v\n", subDir, err)
			continue
		}

		svPlan := FailoverSubVolumePlan{
			SubVolumeName: meta.SubVolumeName,
			PVCName:       meta.Spec.PVCName,
			PVCNamespace:  meta.Spec.PVCNamespace,
			RequestedGB:   meta.Spec.RequestedGB,
			SubPath:       meta.Spec.SubPath,
			UID:           meta.Spec.UID,
			GID:           meta.Spec.GID,
			Permissions:   meta.Spec.Permissions,
			LastSyncTime:  meta.LastSyncTime,
			PoolName:      meta.Spec.PoolName,
			ShareID:       meta.Spec.ShareID,
			PolicyName:    meta.ReplicationPolicy,
		}

		plan.SubVolumes = append(plan.SubVolumes, svPlan)

		if oldestSync.IsZero() || meta.LastSyncTime.Before(oldestSync) {
			oldestSync = meta.LastSyncTime
		}
	}

	if !oldestSync.IsZero() {
		plan.RPO = now.Sub(oldestSync)
	}

	return plan, nil
}
