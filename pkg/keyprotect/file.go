package keyprotect

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
)

// FileOptions names the files that the unwrap step of a Job reads and writes.
// The Secrets are mounted as files, so no key is in the Job's environment.
type FileOptions struct {
	// WrappedKeyFile holds the key wrapped by Key Protect.
	WrappedKeyFile string
	// EmergencyKeyFile holds the readable key, for when Key Protect is down.
	// Empty means there is none.
	EmergencyKeyFile string
	// OutFile is where the readable key is written. It must not exist. It is
	// meant to be on a memory-backed volume.
	OutFile string
}

// UnwrapToFile resolves the backup key and writes it, and nothing else, to
// OutFile with mode 0400. On any failure it writes no file. The log gets
// messages about what happened, never a key.
func UnwrapToFile(ctx context.Context, u Unwrapper, o FileOptions, log io.Writer) error {
	wrapped, err := readSecretFile(o.WrappedKeyFile, true)
	if err != nil {
		return err
	}
	emergency, err := readSecretFile(o.EmergencyKeyFile, false)
	if err != nil {
		return err
	}
	key, err := Resolve(ctx, u, wrapped, emergency, log)
	if err != nil {
		return err
	}
	f, err := os.OpenFile(o.OutFile, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o400)
	if err != nil {
		return fmt.Errorf("creating the key file: %w", err)
	}
	if _, err = io.WriteString(f, key); err != nil {
		_ = f.Close()
		_ = os.Remove(o.OutFile)
		return fmt.Errorf("writing the key file: %w", err)
	}
	if err = f.Close(); err != nil {
		_ = os.Remove(o.OutFile)
		return fmt.Errorf("writing the key file: %w", err)
	}
	return nil
}

// readSecretFile returns the trimmed content of a mounted Secret file. An empty
// name gives an empty value when the file is optional.
func readSecretFile(name string, required bool) (string, error) {
	if name == "" {
		if required {
			return "", errors.New("a file name is required")
		}
		return "", nil
	}
	b, err := os.ReadFile(name) //nolint:gosec // the path is a flag of the Job, set by the driver
	if err != nil {
		// The error names the file, never its content.
		return "", fmt.Errorf("reading %s: %w", name, err)
	}
	return strings.TrimSpace(string(b)), nil
}
