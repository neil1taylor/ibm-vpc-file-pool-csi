package keyprotect

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strings"
)

// Unwrapper turns a wrapped key into the readable key. *Client is one.
type Unwrapper interface {
	Unwrap(ctx context.Context, wrapped string) (string, error)
}

// Resolve returns the readable backup key. It asks Key Protect to unwrap the
// wrapped key. Only when Key Protect cannot be reached, and an emergency key is
// given, it returns the emergency key and says so on log. It never writes a key
// to log. A refusal from Key Protect (a wrong root key, a bad API key) is an
// error: the emergency key does not hide a setup mistake.
func Resolve(ctx context.Context, u Unwrapper, wrapped, emergency string, log io.Writer) (string, error) {
	emergency = strings.TrimSpace(emergency)
	if wrapped == "" {
		if emergency == "" {
			return "", errors.New("there is no wrapped backup key and no emergency key")
		}
		_, _ = fmt.Fprintln(log, "There is no wrapped backup key: using the emergency key.")
		return emergency, nil
	}
	key, err := u.Unwrap(ctx, wrapped)
	if err == nil {
		return key, nil
	}
	if !errors.Is(err, ErrUnreachable) {
		return "", fmt.Errorf("could not unwrap the backup key: %w", err)
	}
	if emergency == "" {
		return "", fmt.Errorf("cannot reach Key Protect and no emergency key is set (spec.export.emergencyKeySecret): %w", err)
	}
	_, _ = fmt.Fprintf(log, "Key Protect cannot be reached (%v): using the emergency key.\n", err)
	return emergency, nil
}
