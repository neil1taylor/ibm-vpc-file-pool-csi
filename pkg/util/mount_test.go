package util

import (
	"sync"
	"testing"
)

func TestNewMountCache(t *testing.T) {
	c := NewMountCache()
	if c == nil {
		t.Fatal("NewMountCache() returned nil")
	}
	if c.IsMounted("/some/path") {
		t.Error("new cache should have no mounts")
	}
}

func TestMountCache_AddAndIsMounted(t *testing.T) {
	c := NewMountCache()

	c.Add("/mnt/share1", "10.0.0.1", "/")
	if !c.IsMounted("/mnt/share1") {
		t.Error("IsMounted should return true after Add")
	}
	if c.IsMounted("/mnt/share2") {
		t.Error("IsMounted should return false for unadded path")
	}
}

func TestMountCache_Remove(t *testing.T) {
	c := NewMountCache()

	c.Add("/mnt/share1", "10.0.0.1", "/")
	c.Remove("/mnt/share1")
	if c.IsMounted("/mnt/share1") {
		t.Error("IsMounted should return false after Remove")
	}
}

func TestMountCache_RemoveNonExistent(t *testing.T) {
	c := NewMountCache()
	// Should not panic.
	c.Remove("/mnt/does-not-exist")
}

func TestMountCache_OverwriteEntry(t *testing.T) {
	c := NewMountCache()

	c.Add("/mnt/share1", "10.0.0.1", "/old")
	c.Add("/mnt/share1", "10.0.0.2", "/new")

	if !c.IsMounted("/mnt/share1") {
		t.Error("IsMounted should return true after overwrite")
	}

	// Verify the entry was overwritten by checking internals.
	c.mu.RLock()
	entry := c.mounts["/mnt/share1"]
	c.mu.RUnlock()
	if entry.Server != "10.0.0.2" || entry.SharePath != "/new" {
		t.Errorf("entry = %+v, want Server=10.0.0.2 SharePath=/new", entry)
	}
}

func TestMountCache_MultipleEntries(t *testing.T) {
	c := NewMountCache()

	c.Add("/mnt/share1", "10.0.0.1", "/")
	c.Add("/mnt/share2", "10.0.0.2", "/")
	c.Add("/mnt/share3", "10.0.0.3", "/")

	for _, p := range []string{"/mnt/share1", "/mnt/share2", "/mnt/share3"} {
		if !c.IsMounted(p) {
			t.Errorf("IsMounted(%q) = false, want true", p)
		}
	}

	c.Remove("/mnt/share2")
	if c.IsMounted("/mnt/share2") {
		t.Error("share2 should be removed")
	}
	if !c.IsMounted("/mnt/share1") || !c.IsMounted("/mnt/share3") {
		t.Error("share1 and share3 should still be mounted")
	}
}

func TestMountCache_ConcurrentAccess(t *testing.T) {
	c := NewMountCache()
	var wg sync.WaitGroup

	// Concurrent writers.
	for i := 0; i < 50; i++ {
		wg.Add(1)
		go func(n int) {
			defer wg.Done()
			path := "/mnt/share"
			if n%2 == 0 {
				c.Add(path, "10.0.0.1", "/")
			} else {
				c.Remove(path)
			}
		}(i)
	}

	// Concurrent readers.
	for i := 0; i < 50; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			c.IsMounted("/mnt/share")
		}()
	}

	wg.Wait()
}
