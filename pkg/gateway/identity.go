package gateway

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
)

// ErrUnauthenticated is returned for a token that is not valid.
var ErrUnauthenticated = errors.New("unauthenticated")

// userLabel is on a BackupBrowse. Its value is a hash of the user's name, so
// the name itself is not in a label.
const userLabel = "storage.ibmcloud.io/browse-user"

func userHash(name string) string {
	sum := sha256.Sum256([]byte(name))
	return hex.EncodeToString(sum[:])[:32]
}
