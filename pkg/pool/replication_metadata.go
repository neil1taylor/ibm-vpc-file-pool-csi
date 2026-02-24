package pool

import (
	"encoding/json"
	"fmt"
	"os"
	"time"

	v1alpha1 "github.com/IBM/ibm-vpc-file-pool-csi/api/v1alpha1"
)

const metadataFileName = ".subvolume-metadata.json"

// SubVolumeMetadata is written alongside each replicated SubVolume directory.
// The failover CLI reads these files from the destination NFS to reconstruct
// SubVolume CRs, PVs, and PVCs on the DR cluster.
type SubVolumeMetadata struct {
	SubVolumeName     string                 `json:"subVolumeName"`
	Spec              v1alpha1.SubVolumeSpec `json:"spec"`
	Labels            map[string]string      `json:"labels,omitempty"`
	LastSyncTime      time.Time              `json:"lastSyncTime"`
	ReplicationPolicy string                 `json:"replicationPolicy"`
}

// writeMetadata marshals the metadata to JSON and writes it to <path>/<metadataFileName>.
func writeMetadata(nfsOps NFSOperations, path string, meta *SubVolumeMetadata) error {
	data, err := json.MarshalIndent(meta, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal metadata: %w", err)
	}
	filePath := path + "/" + metadataFileName
	if err := nfsOps.WriteFile(filePath, data, 0644); err != nil {
		return fmt.Errorf("write metadata to %s: %w", filePath, err)
	}
	return nil
}

// ReadMetadataFromPath is the exported version of readMetadata for use by the failover package.
func ReadMetadataFromPath(nfsOps NFSOperations, path string) (*SubVolumeMetadata, error) {
	return readMetadata(nfsOps, path)
}

// readMetadata reads and unmarshals a SubVolumeMetadata from <path>/<metadataFileName>.
func readMetadata(nfsOps NFSOperations, path string) (*SubVolumeMetadata, error) {
	filePath := path + "/" + metadataFileName
	data, err := nfsOps.ReadFile(filePath)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, fmt.Errorf("metadata file not found: %s", filePath)
		}
		return nil, fmt.Errorf("read metadata from %s: %w", filePath, err)
	}
	var meta SubVolumeMetadata
	if err := json.Unmarshal(data, &meta); err != nil {
		return nil, fmt.Errorf("unmarshal metadata from %s: %w", filePath, err)
	}
	return &meta, nil
}
