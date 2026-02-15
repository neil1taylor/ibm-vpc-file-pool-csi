package util

import "sync"

// MountEntry tracks a cached NFS mount.
type MountEntry struct {
	Server    string
	SharePath string
}

// MountCache tracks NFS share mounts on the node to avoid redundant mounts.
// One NFS mount per share per node; PVCs bind-mount subdirectories.
type MountCache struct {
	mu     sync.RWMutex
	mounts map[string]MountEntry
}

// NewMountCache creates an empty mount cache.
func NewMountCache() *MountCache {
	return &MountCache{
		mounts: make(map[string]MountEntry),
	}
}

// IsMounted returns true if the given path is already mounted and cached.
func (c *MountCache) IsMounted(path string) bool {
	c.mu.RLock()
	defer c.mu.RUnlock()
	_, ok := c.mounts[path]
	return ok
}

// Add records a new mount in the cache.
func (c *MountCache) Add(path, server, sharePath string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.mounts[path] = MountEntry{Server: server, SharePath: sharePath}
}

// Remove deletes a mount from the cache.
func (c *MountCache) Remove(path string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	delete(c.mounts, path)
}
