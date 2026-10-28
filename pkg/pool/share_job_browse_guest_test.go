package pool

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
)

const testGuestImage = "de.icr.io/x/browse-guest:v1"

func guestRequest() BrowseJobRequest {
	req := browseRequest()
	req.Guest = true
	req.MomentsPath = ""
	req.Consistency = "CrashConsistent"
	return req
}

func TestStartBrowse_GuestNeedsItsImage(t *testing.T) {
	r, _ := newBrowseRunner()
	if r.GuestEnabled() {
		t.Fatal("no guest image, no guest sessions")
	}
	if err := r.StartBrowse(context.Background(), guestRequest()); !errors.Is(err, errNoBrowseGuest) {
		t.Fatalf("err = %v", err)
	}
}

func TestStartBrowse_GuestPod(t *testing.T) {
	r, c := newBrowseRunner()
	r.SetBrowseGuest(testGuestImage, BrowseGuestConfig{})
	if !r.GuestEnabled() {
		t.Fatal("not enabled")
	}
	req := guestRequest()
	req.At = time.Date(2026, 10, 22, 10, 30, 0, 0, time.UTC)
	if err := r.StartBrowse(context.Background(), req); err != nil {
		t.Fatal(err)
	}
	pod := browseJob(t, c, "bb-1").Spec.Template.Spec
	ctr := pod.Containers[0]
	if ctr.Image != testGuestImage {
		t.Errorf("image = %q", ctr.Image)
	}
	args := strings.Join(ctr.Command, " ")
	for _, want := range []string{"--mode=browse-server", "--guest", "--root=/latest", "--guest-socket=/tmp/disk.sock",
		"--guest-appliance=/opt/guestfs-appliance", "--guest-consistency=CrashConsistent", "--at=2026-10-22T10:30:00Z",
		"--token-file=/etc/browse/token", "--idle=15m0s", "--max-life=2h0m0s"} {
		if !strings.Contains(args, want) {
			t.Errorf("command lacks %q: %s", want, args)
		}
	}
	if strings.Contains(args, "--moments") {
		t.Errorf("a VM disk has no moments folder: %s", args)
	}
	mounts := map[string]corev1.VolumeMount{}
	for _, m := range ctr.VolumeMounts {
		mounts[m.MountPath] = m
	}
	for _, p := range []string{"/latest", "/etc/browse"} {
		if !mounts[p].ReadOnly {
			t.Errorf("%s must be read-only", p)
		}
	}
	if len(mounts) != 4 || mounts["/tmp"].ReadOnly || mounts["/var/tmp"].ReadOnly {
		t.Errorf("mounts = %+v", mounts)
	}
	empty := 0
	for _, v := range pod.Volumes {
		if v.HostPath != nil {
			t.Errorf("host path %s", v.Name)
		}
		if v.EmptyDir != nil {
			empty++
			if v.EmptyDir.SizeLimit == nil || v.EmptyDir.SizeLimit.IsZero() {
				t.Errorf("emptyDir %s has no size limit", v.Name)
			}
			if v.EmptyDir.Medium == corev1.StorageMediumMemory {
				t.Errorf("emptyDir %s is in memory: the appliance needs room", v.Name)
			}
		}
	}
	if empty != 2 {
		t.Errorf("%d emptyDir volumes, want 2", empty)
	}
	lim := ctr.Resources.Limits
	if lim.Cpu().Value() != 1 || lim.Memory().Value() != 1<<30 {
		t.Errorf("limits = %v", lim)
	}
	if _, ok := lim[kvmResource]; ok {
		t.Error("the KVM device was not asked for")
	}
	sc := ctr.SecurityContext
	if sc == nil || sc.Privileged != nil && *sc.Privileged || *sc.AllowPrivilegeEscalation || !*sc.ReadOnlyRootFilesystem ||
		len(sc.Capabilities.Drop) != 1 || len(sc.Capabilities.Add) != 0 {
		t.Errorf("security context = %+v", sc)
	}
	if *pod.AutomountServiceAccountToken || pod.HostNetwork {
		t.Error("no token and no host network")
	}
}

func TestStartBrowse_GuestAsksForTheKVMDeviceWhenTold(t *testing.T) {
	r, c := newBrowseRunner()
	r.SetBrowseGuest(testGuestImage, BrowseGuestConfig{KVM: true, CPU: "2", Memory: "2Gi"})
	if err := r.StartBrowse(context.Background(), guestRequest()); err != nil {
		t.Fatal(err)
	}
	ctr := browseJob(t, c, "bb-1").Spec.Template.Spec.Containers[0]
	for name, list := range map[string]corev1.ResourceList{"limits": ctr.Resources.Limits, "requests": ctr.Resources.Requests} {
		q, ok := list[kvmResource]
		if !ok || q.Value() != 1 {
			t.Errorf("%s lack the KVM device: %v", name, list)
		}
	}
	if ctr.Resources.Limits.Cpu().Value() != 2 || ctr.Resources.Limits.Memory().Value() != 2<<30 {
		t.Errorf("limits = %v", ctr.Resources.Limits)
	}
}

func TestStartBrowse_GuestOmitsConsistencyWhenUnknown(t *testing.T) {
	r, c := newBrowseRunner()
	r.SetBrowseGuest(testGuestImage, BrowseGuestConfig{})
	req := guestRequest()
	req.Consistency = ""
	if err := r.StartBrowse(context.Background(), req); err != nil {
		t.Fatal(err)
	}
	if args := strings.Join(browseJob(t, c, "bb-1").Spec.Template.Spec.Containers[0].Command, " "); strings.Contains(args, "consistency") {
		t.Errorf("args = %s", args)
	}
}

func TestStartBrowse_GuestRefusesAnUnknownConsistency(t *testing.T) {
	r, _ := newBrowseRunner()
	r.SetBrowseGuest(testGuestImage, BrowseGuestConfig{})
	req := guestRequest()
	req.Consistency = "Maybe; echo"
	if err := r.StartBrowse(context.Background(), req); err == nil {
		t.Fatal("a value that is not one of the two must be refused")
	}
}
