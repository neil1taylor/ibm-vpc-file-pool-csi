package pool

import (
	"context"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func diskRequest() BrowseJobRequest {
	req := cosBrowseRequest()
	req.Guest = true
	req.DiskSecretName = "browsedisk-bb-9"
	req.Consistency = "CrashConsistent"
	return req
}

func TestStartBrowseDisk_PodHoldsTheKeysAndServesOnlyTheDisk(t *testing.T) {
	r, c := newBrowseRunner()
	if err := r.StartBrowseDisk(context.Background(), diskRequest()); err != nil {
		t.Fatal(err)
	}
	job := getShareJob(t, c, "browsedisk-bb-9")
	pod := job.Spec.Template.Spec
	ctr := pod.Containers[0]
	if ctr.Image != testBrowseImage {
		t.Errorf("image = %q; the disk server needs Kopia, which the plain browse image has", ctr.Image)
	}
	args := strings.Join(ctr.Command, " ")
	for _, want := range []string{"--mode=cos-disk-server", "--cos-source=", "--kopia=/usr/local/bin/kopia", "--listen=:8443",
		"--token-file=/etc/browse/token", "--tls-cert-file=/etc/browse/tls.crt", "--tls-key-file=/etc/browse/tls.key", "--idle=15m0s", "--max-life=2h0m0s"} {
		if !strings.Contains(args, want) {
			t.Errorf("command lacks %q: %s", want, args)
		}
	}
	if strings.Contains(args, "--root") || strings.Contains(args, "--guest") {
		t.Errorf("the disk server must not read a folder or parse a disk: %s", args)
	}
	var disk, creds bool
	for _, v := range pod.Volumes {
		if v.Secret != nil && v.Secret.SecretName == "browsedisk-bb-9" {
			disk = true
		}
		if v.Secret != nil && v.Secret.SecretName == "cos-credentials" {
			creds = true
		}
	}
	if !disk || !creds {
		t.Errorf("volumes = %+v; want the COS key and the disk session Secret", pod.Volumes)
	}
	if pod.AutomountServiceAccountToken == nil || *pod.AutomountServiceAccountToken {
		t.Error("the disk server must not get an API token")
	}
	if got := job.Spec.Template.Labels[browseDiskLabel]; got != "true" {
		t.Errorf("pod label %s = %q; the network policy selects on it", browseDiskLabel, got)
	}
	if ctr.SecurityContext == nil || ctr.SecurityContext.ReadOnlyRootFilesystem == nil || !*ctr.SecurityContext.ReadOnlyRootFilesystem {
		t.Error("the root file system must be read-only")
	}
}

func TestStartBrowseDisk_NeedsAPlainRequest(t *testing.T) {
	r, _ := newBrowseRunner()
	req := diskRequest()
	req.COS = nil
	if err := r.StartBrowseDisk(context.Background(), req); err == nil {
		t.Error("a request without COS was accepted")
	}
	req = diskRequest()
	req.DiskSecretName = ""
	if err := r.StartBrowseDisk(context.Background(), req); err == nil {
		t.Error("a request without the disk Secret was accepted")
	}
	req = diskRequest()
	req.At = time.Now()
	if err := r.StartBrowseDisk(context.Background(), req); err != nil {
		t.Errorf("a moment must pass to the disk server, which refuses an older one itself: %v", err)
	}
}

func TestStartBrowse_GuestReaderOfADiskServer(t *testing.T) {
	r, c := newBrowseRunner()
	r.SetBrowseGuest(testGuestImage, BrowseGuestConfig{})
	req := diskRequest()
	req.COS = nil
	req.Disk = &BrowseDisk{IP: "10.1.2.3", SecretName: "browsedisk-bb-9"}
	if err := r.StartBrowse(context.Background(), req); err != nil {
		t.Fatal(err)
	}
	job := browseJob(t, c, "bb-9")
	pod := job.Spec.Template.Spec
	ctr := pod.Containers[0]
	args := strings.Join(ctr.Command, " ")
	for _, want := range []string{"--mode=browse-server", "--guest", "--guest-disk-url=https://10.1.2.3:8443/disk",
		"--guest-disk-cert-file=/etc/disk/tls.crt", "--guest-disk-token-file=/etc/disk/token", "--guest-consistency=CrashConsistent"} {
		if !strings.Contains(args, want) {
			t.Errorf("command lacks %q: %s", want, args)
		}
	}
	if strings.Contains(args, "--root") {
		t.Errorf("the reader has no folder: %s", args)
	}
	for _, v := range pod.Volumes {
		if v.PersistentVolumeClaim != nil {
			t.Errorf("volume %s is a claim; the reader mounts no share", v.Name)
		}
		if v.Secret != nil && v.Secret.SecretName == "cos-credentials" {
			t.Error("the reader must not hold the COS key")
		}
	}
	var item *corev1.SecretVolumeSource
	for _, v := range pod.Volumes {
		if v.Secret != nil && v.Secret.SecretName == "browsedisk-bb-9" {
			item = v.Secret
		}
	}
	if item == nil {
		t.Fatal("the reader needs the disk Secret")
	}
	for _, it := range item.Items {
		if it.Key == "tls.key" {
			t.Error("the reader must not get the private key of the disk server")
		}
	}
	if got := job.Spec.Template.Labels[browseGuestLabel]; got != "true" {
		t.Errorf("pod label %s = %q; the network policy selects on it", browseGuestLabel, got)
	}
}

func TestStartBrowse_DiskReaderRefusesAnUnsafeAddress(t *testing.T) {
	r, _ := newBrowseRunner()
	r.SetBrowseGuest(testGuestImage, BrowseGuestConfig{})
	for _, ip := range []string{"", "not-an-ip", "10.0.0.1/evil", "10.0.0.1:80", "evil.example.com", "::1"} {
		req := diskRequest()
		req.COS = nil
		req.Disk = &BrowseDisk{IP: ip, SecretName: "browsedisk-bb-9"}
		if err := r.StartBrowse(context.Background(), req); err == nil {
			t.Errorf("address %q was accepted", ip)
		}
	}
}

func TestDiskState_FollowsTheDiskServerJob(t *testing.T) {
	r, c := newBrowseRunner()
	ctx := context.Background()
	if st, err := r.DiskState(ctx, "bb-9"); err != nil || st.Exists {
		t.Fatalf("before start: %+v, %v", st, err)
	}
	_ = r.StartBrowseDisk(ctx, diskRequest())
	st, err := r.DiskState(ctx, "bb-9")
	if err != nil || !st.Exists || st.Ready {
		t.Fatalf("after start: %+v, %v", st, err)
	}
	job := getShareJob(t, c, "browsedisk-bb-9")
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: "d-pod", Namespace: testDriverNS, Labels: job.Spec.Template.Labels},
		Status:     corev1.PodStatus{PodIP: "10.9.9.9", Conditions: []corev1.PodCondition{{Type: corev1.PodReady, Status: corev1.ConditionTrue}}},
	}
	if err := c.Create(ctx, pod); err != nil {
		t.Fatal(err)
	}
	if st, err = r.DiskState(ctx, "bb-9"); err != nil || !st.Ready || st.PodIP != "10.9.9.9" {
		t.Fatalf("ready: %+v, %v", st, err)
	}
}

func TestStopBrowse_StopsTheDiskServerToo(t *testing.T) {
	r, c := newBrowseRunner()
	ctx := context.Background()
	_ = r.StartBrowseDisk(ctx, diskRequest())
	for i := 0; i < 5; i++ {
		done, err := r.StopBrowse(ctx, "bb-9")
		if err != nil {
			t.Fatal(err)
		}
		if done {
			break
		}
	}
	if st, _ := r.DiskState(ctx, "bb-9"); st.Exists {
		t.Error("the disk server Job is still there after StopBrowse")
	}
	_ = c
	if names, _ := r.BrowseSessions(ctx); len(names) != 0 {
		t.Errorf("sessions = %v", names)
	}
}

func TestBrowseSessions_ListsADiskServerOnce(t *testing.T) {
	r, _ := newBrowseRunner()
	r.SetBrowseGuest(testGuestImage, BrowseGuestConfig{})
	ctx := context.Background()
	_ = r.StartBrowseDisk(ctx, diskRequest())
	req := diskRequest()
	req.COS = nil
	req.Disk = &BrowseDisk{IP: "10.1.2.3", SecretName: "browsedisk-bb-9"}
	if err := r.StartBrowse(ctx, req); err != nil {
		t.Fatal(err)
	}
	names, err := r.BrowseSessions(ctx)
	if err != nil || len(names) != 1 || names[0] != "bb-9" {
		t.Errorf("sessions = %v, %v; want bb-9 once", names, err)
	}
}
