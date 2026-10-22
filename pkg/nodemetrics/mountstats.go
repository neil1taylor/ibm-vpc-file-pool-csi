// Package nodemetrics reads what each NFS share mounted on this node is doing
// (operations, latency, bytes, space used) and serves it as Prometheus metrics.
package nodemetrics

import (
	"bufio"
	"io"
	"strconv"
	"strings"
)

// OpStats is the cumulative count of one NFS operation type on one mount.
type OpStats struct {
	Ops        uint64
	BytesSent  uint64
	BytesRecv  uint64
	RTTMillis  uint64 // time from sending the request to getting the reply
	ExecMillis uint64 // time from queueing the request to finishing it
}

// NFSMount is one NFS mount listed in /proc/self/mountstats.
type NFSMount struct {
	Server     string
	Export     string
	MountPoint string
	Ops        map[string]OpStats
}

// ParseMountStats reads the /proc/self/mountstats format and returns the NFS
// mounts in it. Lines it cannot read are skipped.
func ParseMountStats(r io.Reader) ([]NFSMount, error) {
	var mounts []NFSMount
	var cur *NFSMount
	inOps := false

	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	for sc.Scan() {
		line := sc.Text()
		if strings.HasPrefix(line, "device ") {
			cur, inOps = nil, false
			if m, ok := parseDeviceLine(line); ok {
				mounts = append(mounts, m)
				cur = &mounts[len(mounts)-1]
			}
			continue
		}
		if cur == nil {
			continue
		}
		trimmed := strings.TrimSpace(line)
		if trimmed == "per-op statistics" {
			inOps = true
			continue
		}
		if !inOps {
			continue
		}
		name, stats, ok := parseOpLine(trimmed)
		if ok {
			cur.Ops[name] = stats
		}
	}
	return mounts, sc.Err()
}

// parseDeviceLine reads "device SRV:/EXPORT mounted on PATH with fstype nfs4 ...".
func parseDeviceLine(line string) (NFSMount, bool) {
	f := strings.Fields(line)
	// device <dev> mounted on <path> with fstype <type> ...
	if len(f) < 8 || f[2] != "mounted" || f[3] != "on" || f[5] != "with" || f[6] != "fstype" {
		return NFSMount{}, false
	}
	if !strings.HasPrefix(f[7], "nfs") {
		return NFSMount{}, false
	}
	i := strings.Index(f[1], ":/")
	if i < 0 {
		return NFSMount{}, false
	}
	return NFSMount{
		Server:     f[1][:i],
		Export:     f[1][i+1:],
		MountPoint: f[4],
		Ops:        map[string]OpStats{},
	}, true
}

// parseOpLine reads "READ: ops trans timeouts sent recv queue rtt exec [errors]".
func parseOpLine(line string) (string, OpStats, bool) {
	name, rest, found := strings.Cut(line, ":")
	if !found {
		return "", OpStats{}, false
	}
	f := strings.Fields(rest)
	if len(f) < 8 {
		return "", OpStats{}, false
	}
	n := make([]uint64, 8)
	for i := range n {
		v, err := strconv.ParseUint(f[i], 10, 64)
		if err != nil {
			return "", OpStats{}, false
		}
		n[i] = v
	}
	return strings.TrimSpace(name), OpStats{
		Ops: n[0], BytesSent: n[3], BytesRecv: n[4], RTTMillis: n[6], ExecMillis: n[7],
	}, true
}
