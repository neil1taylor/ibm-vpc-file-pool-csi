package failover

import (
	"fmt"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/IBM/ibm-vpc-file-pool-csi/pkg/replication"
)

// qemuUID is the user KubeVirt runs QEMU as.
const qemuUID = 107

// VMOptions say how to choose the moment of a running VM's disks.
type VMOptions struct {
	// RewindTo, if set, limits the choice to moments at or before it.
	RewindTo *time.Time
	// Force goes ahead although a commit may still be changing a disk.
	// Recovery makes that safe: an unfinished copy is rolled back first.
	Force bool
	// Now is the current time.
	Now time.Time
}

// diskInGroup is one disk of a running VM at the destination.
type diskInGroup struct {
	idx   int
	name  string
	state replication.DiskState
}

// vmGroupAnalysis is the disks of one VM and the moments they share.
type vmGroupAnalysis struct {
	disks  []diskInGroup
	common []time.Time // newest first
}

const diskFile = "disk.img"

// analyzeVMs reads the state of every VM disk in the plan and finds, for each
// VM, the moments all its disks can show. It changes nothing.
func (p *FailoverPlan) analyzeVMs(opts VMOptions) (map[string]*vmGroupAnalysis, error) {
	groups := map[string]*vmGroupAnalysis{}
	var busy []string
	for i, sv := range p.SubVolumes {
		if sv.LocalDir == "" {
			continue
		}
		disk := filepath.Join(sv.LocalDir, diskFile)
		if fi, err := os.Lstat(disk); err != nil || !fi.Mode().IsRegular() {
			continue
		}
		st, err := replication.ReadDiskState(disk)
		if err != nil {
			return nil, fmt.Errorf("reading the state of %s: %w", sv.SubVolumeName, err)
		}
		if !st.MomentKnown && st.VMGroup == "" {
			continue // not a disk the receiver wrote
		}
		if on, lerr := replication.LeaseActive(sv.LocalDir, opts.Now); lerr != nil {
			return nil, fmt.Errorf("checking for a running commit on %s: %w", sv.SubVolumeName, lerr)
		} else if on && !opts.Force {
			busy = append(busy, sv.SubVolumeName)
		}
		key := st.VMGroup
		if key == "" {
			key = "volume/" + sv.SubVolumeName
		}
		g := groups[key]
		if g == nil {
			g = &vmGroupAnalysis{}
			groups[key] = g
		}
		g.disks = append(g.disks, diskInGroup{idx: i, name: sv.SubVolumeName, state: st})
	}
	if len(busy) > 0 {
		return nil, fmt.Errorf("a copy is still being applied to %s; wait for it to end, or use --force (an unfinished copy is rolled back first)", strings.Join(busy, ", "))
	}

	for key, g := range groups {
		for _, d := range g.disks {
			if !d.state.MomentKnown && (len(g.disks) > 1 || opts.RewindTo != nil) {
				return nil, fmt.Errorf("the moment of %s (VM %s) is not known, so it cannot be matched with the VM's other disks or an earlier moment", d.name, key)
			}
		}
		if len(g.disks) == 1 && !g.disks[0].state.MomentKnown {
			continue
		}
		var common map[time.Time]bool
		for _, d := range g.disks {
			set := map[time.Time]bool{d.state.Moment.UTC(): true}
			for _, rp := range d.state.RestorePoints {
				set[rp.UTC()] = true
			}
			if common == nil {
				common = set
				continue
			}
			for m := range common {
				if !set[m] {
					delete(common, m)
				}
			}
		}
		for m := range common {
			if opts.RewindTo == nil || !m.After(*opts.RewindTo) {
				g.common = append(g.common, m)
			}
		}
		sort.Slice(g.common, func(a, b int) bool { return g.common[a].After(g.common[b]) })
		if len(g.common) == 0 {
			return nil, noCommonMoment(key, g, opts)
		}
	}
	return groups, nil
}

func noCommonMoment(key string, g *vmGroupAnalysis, opts VMOptions) error {
	var parts []string
	for _, d := range g.disks {
		ms := []string{d.state.Moment.UTC().Format(time.RFC3339)}
		for i := len(d.state.RestorePoints) - 1; i >= 0; i-- {
			ms = append(ms, d.state.RestorePoints[i].UTC().Format(time.RFC3339))
		}
		parts = append(parts, fmt.Sprintf("%s: %s", d.name, strings.Join(ms, ", ")))
	}
	limit := ""
	if opts.RewindTo != nil {
		limit = " at or before " + opts.RewindTo.UTC().Format(time.RFC3339)
	}
	return fmt.Errorf("the disks of VM %s share no moment%s; moments by disk: %s", key, limit, strings.Join(parts, "; "))
}

// CommonMoments returns, for each VM, the moments all its disks can show,
// newest first.
func (p *FailoverPlan) CommonMoments(opts VMOptions) (map[string][]time.Time, error) {
	groups, err := p.analyzeVMs(opts)
	if err != nil {
		return nil, err
	}
	out := map[string][]time.Time{}
	for key, g := range groups {
		out[key] = g.common
	}
	return out, nil
}

// ResolveVMs chooses, for each running VM, the newest moment all its disks
// can show (at or before opts.RewindTo, if set), and marks the disks that must
// be rewound to reach it. It changes nothing on the destination. A disk that
// is not a VM disk is left as it is.
func (p *FailoverPlan) ResolveVMs(opts VMOptions) error {
	groups, err := p.analyzeVMs(opts)
	if err != nil {
		return err
	}
	for key, g := range groups {
		if len(g.common) == 0 {
			continue // a lone disk whose moment is not known: used as it is
		}
		chosen := g.common[0]
		for _, d := range g.disks {
			sv := &p.SubVolumes[d.idx]
			sv.VMGroup = key
			sv.VMMoment = chosen
			sv.Rewind = !chosen.Equal(d.state.Moment)
			sv.LastSyncTime = chosen
		}
	}
	p.recomputeRPO(opts.Now)
	return nil
}

func (p *FailoverPlan) recomputeRPO(now time.Time) {
	var oldest time.Time
	for _, sv := range p.SubVolumes {
		if oldest.IsZero() || sv.LastSyncTime.Before(oldest) {
			oldest = sv.LastSyncTime
		}
	}
	if !oldest.IsZero() {
		p.RPO = now.Sub(oldest)
	}
}

// MaterializeVMs prepares the disks chosen by ResolveVMs. A disk used in place
// is first rolled back from any copy that stopped half way. A disk to rewind
// is rewritten as it was at the chosen moment into a new folder next to the
// old one, and the plan entry's SubPath points at it. The latest disk is not
// changed, so another moment can be chosen later. A rewound folder that exists
// already is reused.
func (p *FailoverPlan) MaterializeVMs() error {
	for i := range p.SubVolumes {
		sv := &p.SubVolumes[i]
		if sv.VMGroup == "" {
			continue
		}
		disk := filepath.Join(sv.LocalDir, diskFile)
		if !sv.Rewind {
			if _, err := replication.Recover(disk); err != nil {
				return fmt.Errorf("rolling back an unfinished copy of %s: %w", sv.SubVolumeName, err)
			}
			continue
		}
		suffix := ".rewind-" + strconv.FormatInt(sv.VMMoment.Unix(), 10)
		out := sv.LocalDir + suffix
		if _, err := os.Stat(filepath.Join(out, diskFile)); err != nil {
			if err = writeRewound(disk, sv.VMMoment, out); err != nil {
				return fmt.Errorf("rewinding %s to %s: %w", sv.SubVolumeName, sv.VMMoment.UTC().Format(time.RFC3339), err)
			}
		}
		sv.SubPath = path.Join(path.Dir(sv.SubPath), path.Base(sv.SubPath)+suffix)
	}
	return nil
}

// writeRewound writes the disk as it was at moment into folder out. It writes
// to a part folder first, so an interrupted run leaves nothing that looks done.
func writeRewound(disk string, moment time.Time, out string) error {
	part := out + ".part"
	if err := os.RemoveAll(part); err != nil {
		return err
	}
	if err := os.Mkdir(part, 0o777); err != nil { //nolint:gosec // G301: the VM runs as another user
		return err
	}
	file := filepath.Join(part, diskFile)
	if err := replication.Rewind(disk, moment, file); err != nil {
		_ = os.RemoveAll(part)
		return err
	}
	// QEMU runs as 107. Root may not change the owner on NFS (root_squash), so
	// if that fails the disk is made usable by anyone who can reach the share.
	if err := os.Chown(file, qemuUID, qemuUID); err != nil {
		if err = os.Chmod(file, 0o666); err != nil { //nolint:gosec // G302: see above
			return err
		}
	}
	if err := os.Chmod(part, 0o777); err != nil { //nolint:gosec // G302: the VM runs as another user
		return err
	}
	return os.Rename(part, out)
}
