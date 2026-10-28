package failover

import (
	"bytes"
	"compress/gzip"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"sort"
	"strings"
	"time"

	v1alpha1 "github.com/IBM/ibm-vpc-file-pool-csi/api/v1alpha1"
	"github.com/IBM/ibm-vpc-file-pool-csi/pkg/pool"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// MaxMessageBytes is how much of the 4096-byte termination message a result
// may use. It leaves room for the text Kubernetes adds.
const MaxMessageBytes = 3800

// BuildPlan reads the destination share at mountPath and chooses the moment
// of each running VM. It is the one place that builds a plan: the CLI and the
// Job mode of the driver both call it, so they cannot differ. It changes
// nothing on the share.
func BuildPlan(nfsOps pool.NFSOperations, mountPath string, opts VMOptions) (*FailoverPlan, error) {
	plan, err := NewPlanner(nfsOps).Plan(mountPath)
	if err != nil {
		return nil, err
	}
	if err = plan.ResolveVMs(opts); err != nil {
		return nil, err
	}
	return plan, nil
}

// Expectation is what an operator confirmed in an earlier Plan: the moment
// of each VM, and the volumes. An Execute that finds something else refuses.
type Expectation struct {
	// VMs maps "namespace/name" to the moment the VM was planned to show.
	VMs map[string]time.Time `json:"vms"`
	// Volumes are the SubVolume names that were planned.
	Volumes []string `json:"volumes"`
	// Targets say, for each planned SubVolume, where the failed-over volume
	// goes: the claim, the folder and its owner. The names and moments alone
	// would let the share change these after the operator confirmed.
	Targets map[string]VolumeTarget `json:"targets,omitempty"`
}

// VolumeTarget is what the operator saw for one volume in the Plan.
type VolumeTarget struct {
	PVCName      string `json:"pvcName"`
	PVCNamespace string `json:"pvcNamespace"`
	SubPath      string `json:"subPath"`
	UID          *int64 `json:"uid,omitempty"`
	GID          *int64 `json:"gid,omitempty"`
	Permissions  string `json:"permissions,omitempty"`
}

func sameInt64(a, b *int64) bool {
	if a == nil || b == nil {
		return a == b
	}
	return *a == *b
}

func (t VolumeTarget) differsFrom(sv FailoverSubVolumePlan) bool {
	return t.PVCName != sv.PVCName || t.PVCNamespace != sv.PVCNamespace || t.SubPath != sv.SubPath ||
		t.Permissions != sv.Permissions || !sameInt64(t.UID, sv.UID) || !sameInt64(t.GID, sv.GID)
}

// Request is one run of the failover Job.
type Request struct {
	MountPath string
	// Action is Plan or Execute.
	Action   string
	RewindTo *time.Time
	Force    bool
	// Now is the current time; zero means time.Now().
	Now time.Time
	// Expect, if set, must match the plan found now, or an Execute refuses
	// before it changes anything.
	Expect *Expectation
}

// Result is what the Job reports: the plan, and for an Execute the folders
// that rewinding made.
type Result struct {
	RPOSeconds       int64                     `json:"rpoSeconds"`
	VMs              []v1alpha1.FailoverVM     `json:"vms,omitempty"`
	Volumes          []v1alpha1.FailoverVolume `json:"volumes,omitempty"`
	MomentsTruncated bool                      `json:"momentsTruncated,omitempty"`
}

// RunJob does what the failover Job does. A Plan reads the share and changes
// nothing. An Execute also rolls back unfinished copies and rewinds the disks
// that need it (MaterializeVMs), after it has checked the plan against Expect.
func RunJob(nfsOps pool.NFSOperations, req Request) (*Result, error) {
	if req.Action != v1alpha1.FailoverActionPlan && req.Action != v1alpha1.FailoverActionExecute {
		return nil, fmt.Errorf("unknown action %q; use %s or %s", req.Action, v1alpha1.FailoverActionPlan, v1alpha1.FailoverActionExecute)
	}
	now := req.Now
	if now.IsZero() {
		now = time.Now()
	}
	opts := VMOptions{RewindTo: req.RewindTo, Force: req.Force, Now: now}
	plan, err := BuildPlan(nfsOps, req.MountPath, opts)
	if err != nil {
		return nil, err
	}
	moments, err := plan.CommonMoments(opts)
	if err != nil {
		return nil, err
	}
	if req.Action == v1alpha1.FailoverActionExecute {
		if req.Expect != nil {
			if err = req.Expect.Check(plan); err != nil {
				return nil, err
			}
		}
		if err = plan.MaterializeVMs(); err != nil {
			return nil, err
		}
	}
	return NewResult(plan, moments), nil
}

// vmDisks groups the VM disks of a plan by VM, with the VM names sorted.
func vmDisks(plan *FailoverPlan) (names []string, disks map[string][]FailoverSubVolumePlan) {
	disks = map[string][]FailoverSubVolumePlan{}
	for _, sv := range plan.SubVolumes {
		if sv.VMGroup != "" {
			disks[sv.VMGroup] = append(disks[sv.VMGroup], sv)
		}
	}
	for g := range disks {
		names = append(names, g)
	}
	sort.Strings(names)
	return names, disks
}

// Check compares the expectation with the plan found now.
func (e *Expectation) Check(plan *FailoverPlan) error {
	var problems []string
	names, disks := vmDisks(plan)
	seen := map[string]bool{}
	for _, g := range names {
		seen[g] = true
		got := disks[g][0].VMMoment
		want, ok := e.VMs[g]
		switch {
		case !ok:
			problems = append(problems, fmt.Sprintf("VM %s was not in the plan", g))
		case !sameSecond(got, want):
			problems = append(problems, fmt.Sprintf("VM %s now shows %s, the plan showed %s", g, got.UTC().Format(time.RFC3339), want.UTC().Format(time.RFC3339)))
		}
	}
	var missing []string
	for g := range e.VMs {
		if !seen[g] {
			missing = append(missing, g)
		}
	}
	sort.Strings(missing)
	for _, g := range missing {
		problems = append(problems, fmt.Sprintf("VM %s was in the plan and is not on the share now", g))
	}
	have := map[string]bool{}
	for _, sv := range plan.SubVolumes {
		have[sv.SubVolumeName] = true
	}
	planned := map[string]bool{}
	for _, n := range e.Volumes {
		planned[n] = true
		if !have[n] {
			problems = append(problems, fmt.Sprintf("volume %s was in the plan and is not on the share now", n))
		}
	}
	for _, sv := range plan.SubVolumes {
		if !planned[sv.SubVolumeName] {
			problems = append(problems, fmt.Sprintf("volume %s was not in the plan", sv.SubVolumeName))
		} else if want, ok := e.Targets[sv.SubVolumeName]; e.Targets != nil && (!ok || want.differsFrom(sv)) {
			problems = append(problems, fmt.Sprintf("volume %s now goes to a different claim, folder or owner than the plan showed", sv.SubVolumeName))
		}
	}
	if len(problems) > 0 {
		return fmt.Errorf("the share no longer matches the plan that was confirmed (%s); run a new Plan and confirm it", strings.Join(problems, "; "))
	}
	return nil
}

func sameSecond(a, b time.Time) bool {
	return a.Truncate(time.Second).Equal(b.Truncate(time.Second))
}

// NewResult describes a plan. moments are the moments each VM's disks can all
// show, as CommonMoments gives them.
func NewResult(plan *FailoverPlan, moments map[string][]time.Time) *Result {
	res := &Result{RPOSeconds: int64(plan.RPO.Seconds())}
	for _, sv := range plan.SubVolumes {
		res.Volumes = append(res.Volumes, v1alpha1.FailoverVolume{
			SubVolume:    sv.SubVolumeName,
			PVCName:      sv.PVCName,
			PVCNamespace: sv.PVCNamespace,
			RequestedGB:  sv.RequestedGB,
			SubPath:      sv.SubPath,
			UID:          sv.UID,
			GID:          sv.GID,
			Permissions:  sv.Permissions,
			Policy:       sv.PolicyName,
			LastSync:     metav1.NewTime(sv.LastSyncTime),
		})
	}
	names, disks := vmDisks(plan)
	for _, g := range names {
		vm := v1alpha1.FailoverVM{Name: g, Moment: metav1.NewTime(disks[g][0].VMMoment)}
		for _, m := range moments[g] {
			vm.Moments = append(vm.Moments, metav1.NewTime(m))
		}
		for _, d := range disks[g] {
			vm.Disks = append(vm.Disks, v1alpha1.FailoverDisk{SubVolume: d.SubVolumeName, Rewind: d.Rewind})
		}
		res.VMs = append(res.VMs, vm)
	}
	return res
}

// PlanFromVolumes rebuilds the part of a plan that the Executor needs, from
// the volumes a Job reported.
func PlanFromVolumes(vols []v1alpha1.FailoverVolume) *FailoverPlan {
	plan := &FailoverPlan{}
	for _, v := range vols {
		plan.SubVolumes = append(plan.SubVolumes, FailoverSubVolumePlan{
			SubVolumeName: v.SubVolume,
			PVCName:       v.PVCName,
			PVCNamespace:  v.PVCNamespace,
			RequestedGB:   v.RequestedGB,
			SubPath:       v.SubPath,
			UID:           v.UID,
			GID:           v.GID,
			Permissions:   v.Permissions,
			PolicyName:    v.Policy,
			LastSyncTime:  v.LastSync.Time,
		})
	}
	return plan
}

// Outcome is the termination message of a failover Job: a result, or why
// there is none.
type Outcome struct {
	Error  string  `json:"error,omitempty"`
	Result *Result `json:"result,omitempty"`
}

const gzipPrefix = "gz:"

// EncodeOutcome makes the termination message. A small outcome is plain JSON.
// A larger one is gzip and base64. If it still does not fit, the lists of
// moments are cut (newest kept) and MomentsTruncated is set. The volumes are
// never cut: an Execute needs them all.
func EncodeOutcome(o Outcome) (string, error) {
	res := o.Result
	if res != nil {
		cp := *res
		cp.VMs = make([]v1alpha1.FailoverVM, len(res.VMs))
		copy(cp.VMs, res.VMs)
		res = &cp
		o.Result = res
	}
	for {
		msg, err := encodeOnce(o)
		if err != nil {
			return "", err
		}
		if len(msg) <= MaxMessageBytes {
			return msg, nil
		}
		if res == nil || !halveMoments(res) {
			return "", fmt.Errorf("the result is %d bytes and does not fit the %d bytes of a termination message", len(msg), MaxMessageBytes)
		}
	}
}

// halveMoments cuts every VM's list of moments to half, newest kept, down to
// one. It reports false if nothing could be cut.
func halveMoments(res *Result) bool {
	cut := false
	for i := range res.VMs {
		n := len(res.VMs[i].Moments)
		if n <= 1 {
			continue
		}
		keep := n / 2
		if keep < 1 {
			keep = 1
		}
		res.VMs[i].Moments = res.VMs[i].Moments[:keep:keep]
		cut = true
	}
	if cut {
		res.MomentsTruncated = true
	}
	return cut
}

func encodeOnce(o Outcome) (string, error) {
	plain, err := json.Marshal(o)
	if err != nil {
		return "", err
	}
	if len(plain) <= MaxMessageBytes {
		return string(plain), nil
	}
	var buf bytes.Buffer
	zw := gzip.NewWriter(&buf)
	if _, err = zw.Write(plain); err != nil {
		return "", err
	}
	if err = zw.Close(); err != nil {
		return "", err
	}
	return gzipPrefix + base64.StdEncoding.EncodeToString(buf.Bytes()), nil
}

// DecodeOutcome reads a termination message made by EncodeOutcome.
func DecodeOutcome(msg string) (Outcome, error) {
	var o Outcome
	data := []byte(msg)
	if strings.HasPrefix(msg, gzipPrefix) {
		raw, err := base64.StdEncoding.DecodeString(strings.TrimPrefix(msg, gzipPrefix))
		if err != nil {
			return o, fmt.Errorf("the failover result is not valid base64: %w", err)
		}
		zr, err := gzip.NewReader(bytes.NewReader(raw))
		if err != nil {
			return o, fmt.Errorf("the failover result is not valid gzip: %w", err)
		}
		// The result is small; cap what a bad message can expand to.
		data, err = io.ReadAll(io.LimitReader(zr, 8<<20))
		if err != nil {
			return o, fmt.Errorf("reading the failover result: %w", err)
		}
	}
	if err := json.Unmarshal(data, &o); err != nil {
		return o, fmt.Errorf("the failover result is not valid JSON: %w", err)
	}
	return o, nil
}
