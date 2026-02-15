package util

import (
	"fmt"
	"path/filepath"
	"regexp"
	"strings"
)

var validSubDirPattern = regexp.MustCompile(`^/pvcs/pvc-[a-f0-9-]{36}$`)

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
