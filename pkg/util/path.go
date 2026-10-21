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

// A backup policy's folder name is its Kubernetes name, at most 63 characters
// (the length of a label value) of lower-case letters, digits, '-' and '.',
// starting and ending with a letter or digit.
const backupPolicyDir = `[a-z0-9]([-a-z0-9.]{0,61}[a-z0-9])?`

var (
	validBackupPathPattern    = regexp.MustCompile(`^/backup/` + backupPolicyDir + `/pvcs/pvc-[a-f0-9-]{36}$`)
	validBackupMomentsPattern = regexp.MustCompile(`^/backup/` + backupPolicyDir + `/\.moments/pvcs/pvc-[a-f0-9-]{36}$`)
)

// ValidateBackupPath ensures a path to the backup of one volume, on the
// snapshot pool's share, is safe: /backup/<policy>/pvcs/<volume>.
func ValidateBackupPath(p string) error {
	return validateBackup(p, validBackupPathPattern, "backup path")
}

// ValidateBackupMomentsPath ensures a path to the earlier moments of one
// volume's backup is safe: /backup/<policy>/.moments/pvcs/<volume>.
func ValidateBackupMomentsPath(p string) error {
	return validateBackup(p, validBackupMomentsPattern, "backup moments path")
}

func validateBackup(p string, re *regexp.Regexp, what string) error {
	if !re.MatchString(p) {
		return fmt.Errorf("%s %q does not match expected pattern", what, p)
	}
	if filepath.Clean(p) != p {
		return fmt.Errorf("%s %q is not clean", what, p)
	}
	if strings.Contains(p, "..") {
		return fmt.Errorf("%s %q contains path traversal", what, p)
	}
	return nil
}
