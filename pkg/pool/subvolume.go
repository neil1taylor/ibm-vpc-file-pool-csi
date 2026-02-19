package pool

import (
	"fmt"
	"os"
	"strconv"

	"github.com/IBM/ibm-vpc-file-pool-csi/pkg/util"
	"k8s.io/klog/v2"
)

// createSubDirectory creates a PVC subdirectory on an NFS share.
// It validates the subPath, creates the directory, and optionally sets ownership/permissions.
func createSubDirectory(nfsOps NFSOperations, basePath, subPath string, uid, gid *int64, permissions string) error {
	fullPath, err := util.SafeJoin(basePath, subPath)
	if err != nil {
		return fmt.Errorf("invalid subpath: %w", err)
	}

	perm := os.FileMode(0755)
	if permissions != "" {
		parsed, err := strconv.ParseUint(permissions, 8, 32)
		if err != nil {
			return fmt.Errorf("invalid permissions %q: %w", permissions, err)
		}
		perm = os.FileMode(parsed)
	}

	if err := nfsOps.MkdirAll(fullPath, perm); err != nil {
		return fmt.Errorf("mkdir %s: %w", fullPath, err)
	}

	// Chown is best-effort: VPC NFS uses sec=null (anonymous auth) which
	// rejects all ownership changes. Log a warning and continue — the
	// directory permissions (e.g. 0777) are sufficient for access.
	if uid != nil || gid != nil {
		uidVal := -1
		gidVal := -1
		if uid != nil {
			uidVal = int(*uid)
		}
		if gid != nil {
			gidVal = int(*gid)
		}
		if err := nfsOps.Chown(fullPath, uidVal, gidVal); err != nil {
			klog.V(4).InfoS("Chown failed (expected on VPC NFS with sec=null)",
				"path", fullPath, "uid", uidVal, "gid", gidVal, "error", err)
		}
	}

	if permissions != "" {
		if err := nfsOps.Chmod(fullPath, perm); err != nil {
			return fmt.Errorf("chmod %s: %w", fullPath, err)
		}
	}

	return nil
}

// removeSubDirectory removes a PVC subdirectory from an NFS share.
// It validates the subPath before performing any filesystem operation.
func removeSubDirectory(nfsOps NFSOperations, basePath, subPath string) error {
	fullPath, err := util.SafeJoin(basePath, subPath)
	if err != nil {
		return fmt.Errorf("invalid subpath: %w", err)
	}

	if err := nfsOps.RemoveAll(fullPath); err != nil {
		return fmt.Errorf("remove %s: %w", fullPath, err)
	}

	return nil
}
