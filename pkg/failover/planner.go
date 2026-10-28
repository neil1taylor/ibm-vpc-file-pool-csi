package failover

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
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

	// LocalDir is the volume's folder on the mounted destination share.
	LocalDir string

	// The fields below are set for a running VM's disk by ResolveVMs.

	// VMGroup is "namespace/name" of the VM that owns the disk.
	VMGroup string
	// VMMoment is the moment the failed-over disk will show.
	VMMoment time.Time
	// Rewind is true if the disk must be rewound to VMMoment. MaterializeVMs
	// then makes a new disk and points SubPath at it.
	Rewind bool
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
// It fails, with a message that says what it looked for and where, if there is
// no pvcs folder or no replicated volume in it: an empty plan would look like
// a copy with nothing to fail over.
func (p *Planner) Plan(nfsMountPath string) (*FailoverPlan, error) {
	pvcsDir := filepath.Join(nfsMountPath, "pvcs")

	entries, err := os.ReadDir(pvcsDir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, noCopiesError(nfsMountPath, fmt.Sprintf("there is no folder %s", pvcsDir))
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
			LocalDir:      subDir,
		}

		plan.SubVolumes = append(plan.SubVolumes, svPlan)

		if oldestSync.IsZero() || meta.LastSyncTime.Before(oldestSync) {
			oldestSync = meta.LastSyncTime
		}
	}

	if len(plan.SubVolumes) == 0 {
		return nil, noCopiesError(nfsMountPath, fmt.Sprintf("the folder %s holds no replicated volume (no subfolder with a readable %s)", pvcsDir, ".subvolume-metadata.json"))
	}

	if !oldestSync.IsZero() {
		plan.RPO = now.Sub(oldestSync)
	}

	return plan, nil
}

// noCopiesError explains an empty plan. A replication writes its copies to
// <claim>/<destinationBasePath>/pvcs/<volume>, and the plan reads only
// <claim>/<spec.basePath>/pvcs, so a base path that differs from the policy's
// hides every copy. If copies are found below the mount, it names the base
// path to use.
func noCopiesError(mountPath, what string) error {
	msg := fmt.Sprintf("no replicated volumes to fail over: %s. A failover looks for <claim>/<basePath>/pvcs/<volume>/.subvolume-metadata.json on the claim mounted at %s; the replication wrote the copies under the ReplicationPolicy's destinationBasePath, so spec.basePath must equal it", what, mountPath)
	if found := findCopyBases(mountPath, 3); len(found) > 0 {
		msg += fmt.Sprintf(". Copies were found under: %s; set basePath: %s", strings.Join(found, ", "), found[0])
	}
	return errors.New(msg)
}

// findCopyBases returns the folders (relative to root, with a leading slash)
// at most depth levels down that hold a pvcs folder with a replicated volume.
func findCopyBases(root string, depth int) []string {
	var found []string
	var walk func(rel string, level int)
	walk = func(rel string, level int) {
		entries, err := os.ReadDir(filepath.Join(root, rel))
		if err != nil {
			return
		}
		for _, e := range entries {
			if !e.IsDir() || strings.HasPrefix(e.Name(), ".") {
				continue
			}
			child := rel + "/" + e.Name()
			if e.Name() == "pvcs" {
				if rel != "" && holdsCopy(filepath.Join(root, child)) {
					found = append(found, rel)
				}
				if rel != "" {
					continue
				}
				// A base path of /pvcs puts the copies in <claim>/pvcs/pvcs.
			}
			if level < depth {
				walk(child, level+1)
			}
		}
	}
	walk("", 1)
	return found
}

func holdsCopy(pvcsDir string) bool {
	entries, err := os.ReadDir(pvcsDir)
	if err != nil {
		return false
	}
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		if _, err := os.Stat(filepath.Join(pvcsDir, e.Name(), ".subvolume-metadata.json")); err == nil {
			return true
		}
	}
	return false
}
