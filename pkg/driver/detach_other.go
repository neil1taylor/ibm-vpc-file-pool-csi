//go:build !linux

package driver

import "errors"

// detachMount is only possible on Linux.
func detachMount(string) error {
	return errors.New("detaching a mount needs Linux")
}
