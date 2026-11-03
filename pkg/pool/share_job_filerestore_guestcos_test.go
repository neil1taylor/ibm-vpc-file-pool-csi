package pool

import (
	"context"
	"strings"
	"testing"

	"github.com/IBM/ibm-vpc-file-pool-csi/pkg/cosbrowse"
	"github.com/IBM/ibm-vpc-file-pool-csi/pkg/filerestore"
	corev1 "k8s.io/api/core/v1"
)

// A restore of files from a VM disk in COS: the disk server holds the keys, the
// restore Job reads the disk from it and holds none.
func guestCOSFileRestoreRequest(mode string) FileRestoreRequest {
	req := cosFileRestoreRequest(mode)
	req.Guest = &FileRestoreGuest{
		Consistency: "CrashConsistent",
		Disk:        &BrowseDisk{IP: "10.1.2.3", SecretName: "restoredisk-uid-1"},
	}
	return req
}

func TestRunFileRestoreGuestCOS_ReaderHoldsNoKeyAndReadsTheDiskServer(t *testing.T) {
	r, c := newApplyRunner()
	r.SetBrowseGuest(testGuestImage, BrowseGuestConfig{})
	if _, _, err := r.RunFileRestore(context.Background(), guestCOSFileRestoreRequest(filerestore.ModeFolder)); err != nil {
		t.Fatal(err)
	}
	job := fileRestoreJob(t, c)
	pod := job.Spec.Template.Spec
	ctr := pod.Containers[0]
	if ctr.Image != testGuestImage {
		t.Errorf("image = %q, want the guest image", ctr.Image)
	}
	spec := jobSpecOf(t, job)
	if spec.COS != nil || spec.Latest != "" || spec.Moments != "" {
		t.Errorf("the Job names the COS block or a folder: %+v", spec)
	}
	if spec.Guest == nil || spec.Guest.Disk == nil || spec.Guest.Disk.URL != "https://10.1.2.3:8443/disk" ||
		spec.Guest.Disk.CertFile != "/etc/disk/tls.crt" || spec.Guest.Disk.TokenFile != "/etc/disk/token" {
		t.Fatalf("guest = %+v", spec.Guest)
	}
	mounts := map[string]corev1.VolumeMount{}
	for _, m := range ctr.VolumeMounts {
		mounts[m.MountPath] = m
	}
	for _, p := range []string{cosbrowse.CredentialsDir, cosbrowse.WrappedKeyDir, cosbrowse.APIKeyDir, cosbrowse.DefaultStateDir, "/src", "/moments", "/dst"} {
		if _, ok := mounts[p]; ok {
			t.Errorf("the reader mounts %s: it must hold no key, no backup and no target", p)
		}
	}
	if m, ok := mounts["/etc/disk"]; !ok || !m.ReadOnly {
		t.Errorf("/etc/disk = %+v, want the disk server's token and certificate, read-only", m)
	}
	var disk *corev1.Volume
	for i := range pod.Volumes {
		if pod.Volumes[i].Name == mounts["/etc/disk"].Name {
			disk = &pod.Volumes[i]
		}
	}
	if disk == nil || disk.Secret == nil || disk.Secret.SecretName != "restoredisk-uid-1" || len(disk.Secret.Items) != 2 {
		t.Fatalf("disk volume = %+v", disk)
	}
	for _, it := range disk.Secret.Items {
		if it.Key == "tls.key" {
			t.Error("the private key of the disk server must not reach the reader")
		}
	}
	if pod.AutomountServiceAccountToken == nil || *pod.AutomountServiceAccountToken {
		t.Error("no service account token")
	}
	if job.Spec.Template.Labels[restoreGuestLabel] != "true" {
		t.Errorf("pod labels = %v: the disk server lets only labelled restore pods in", job.Spec.Template.Labels)
	}
	if len(pod.Containers) != 2 || pod.Containers[1].Command[1] != "--mode=restore-writer" {
		t.Fatalf("containers = %d, want the reader and the writer", len(pod.Containers))
	}
	for _, a := range ctr.Command {
		if strings.Contains(a, "alice") || strings.Contains(a, "restoredisk") {
			t.Errorf("a path or a Secret name reached the command line: %q", a)
		}
	}
}

func TestRunFileRestoreGuestCOS_NeedsADiskServer(t *testing.T) {
	r, _ := newApplyRunner()
	r.SetBrowseGuest(testGuestImage, BrowseGuestConfig{})
	req := guestCOSFileRestoreRequest(filerestore.ModeFolder)
	req.Guest.Disk = nil
	if _, _, err := r.RunFileRestore(context.Background(), req); err == nil {
		t.Error("a VM disk in COS was restored without a disk server")
	}
	for _, ip := range []string{"", "not-an-ip", "::1", "10.1.2.3:80"} {
		req = guestCOSFileRestoreRequest(filerestore.ModeFolder)
		req.Guest.Disk.IP = ip
		if _, _, err := r.RunFileRestore(context.Background(), req); err == nil {
			t.Errorf("address %q accepted", ip)
		}
	}
	req = guestCOSFileRestoreRequest(filerestore.ModeFolder)
	req.Guest.Disk.SecretName = ""
	if _, _, err := r.RunFileRestore(context.Background(), req); err == nil {
		t.Error("a disk server without a Secret accepted")
	}
}
