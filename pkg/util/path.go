package util

import (
	"fmt"
	"path/filepath"
	"regexp"
	"strings"
)

var validSubDirPattern = regexp.MustCompile(`^/pvcs/pvc-[a-f0-9-]{36}$`)
var validSnapshotPathPattern = regexp.MustCompile(`^/pvcs/\.snapshots/snap-[a-zA-Z0-9-]+$`)

// ValidateSubDir ensures the subdirectory path is safe.
// Returns an error if the path could be used for directory traversal.
func ValidateSubDir(subDir string) error {
	if !validSubDirPattern.MatchString(subDir) {
		return fmt.Errorf("subDir %q does not match expected pattern", subDir)
	}

	cleaned := filepath.Clean(subDir)
	if cleaned != subDir {
		return fmt.Errorf("subDir %q is not clean (cleaned: %q)", subDir, cleaned)
	}

	if strings.Contains(subDir, "..") {
		return fmt.Errorf("subDir %q contains path traversal", subDir)
	}

	return nil
}

// SafeJoin joins a base path and a subdirectory, ensuring the result is under base.
func SafeJoin(base, sub string) (string, error) {
	if err := ValidateSubDir(sub); err != nil {
		return "", err
	}

	joined := filepath.Join(base, sub)
	if !strings.HasPrefix(filepath.Clean(joined), filepath.Clean(base)) {
		return "", fmt.Errorf("path %q escapes base %q", joined, base)
	}

	return joined, nil
}

// ValidateSnapshotPath ensures the snapshot path is safe.
// Returns an error if the path could be used for directory traversal.
func ValidateSnapshotPath(snapshotPath string) error {
	if !validSnapshotPathPattern.MatchString(snapshotPath) {
		return fmt.Errorf("snapshotPath %q does not match expected pattern", snapshotPath)
	}

	cleaned := filepath.Clean(snapshotPath)
	if cleaned != snapshotPath {
		return fmt.Errorf("snapshotPath %q is not clean (cleaned: %q)", snapshotPath, cleaned)
	}

	if strings.Contains(snapshotPath, "..") {
		return fmt.Errorf("snapshotPath %q contains path traversal", snapshotPath)
	}

	return nil
}

// SafeJoinSnapshot joins a base path and a snapshot path, ensuring the result is under base.
func SafeJoinSnapshot(base, snapshotPath string) (string, error) {
	if err := ValidateSnapshotPath(snapshotPath); err != nil {
		return "", err
	}

	joined := filepath.Join(base, snapshotPath)
	if !strings.HasPrefix(filepath.Clean(joined), filepath.Clean(base)) {
		return "", fmt.Errorf("path %q escapes base %q", joined, base)
	}

	return joined, nil
}
