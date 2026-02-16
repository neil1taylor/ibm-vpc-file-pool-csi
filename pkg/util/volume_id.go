package util

import (
	"fmt"
	"strings"
)

// ParseVolumeID splits a volume or snapshot ID into its components.
// Format: {pool-name}/{share-vpc-id}/{name}
func ParseVolumeID(id string) (poolName, shareID, name string, err error) {
	parts := strings.SplitN(id, "/", 3)
	if len(parts) != 3 {
		return "", "", "", fmt.Errorf("ID must have 3 parts separated by '/', got: %s", id)
	}
	return parts[0], parts[1], parts[2], nil
}
