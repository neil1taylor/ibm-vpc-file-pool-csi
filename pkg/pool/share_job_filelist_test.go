package pool

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func newListRunner() (*ShareJobRunner, string) {
	r, _ := newShareJobTestRunner()
	r.SetBrowseGuest(testGuestImage, BrowseGuestConfig{})
	return r, "fl-1"
}

func TestBuildFileList_NeedsTheGuestImage(t *testing.T) {
	r, _ := newShareJobTestRunner()
	if _, _, err := r.BuildFileList(context.Background(), "fl-1", testShareA, testBackupPath, "CrashConsistent"); !errors.Is(err, errNoBrowseGuest) {
		t.Fatalf("err = %v", err)
	}
}

func TestBuildFileList_RefusesAnUnsafePath(t *testing.T) {
	r, _ := newListRunner()
	for _, p := range []string{"", "/backup/nightly/pvcs/../x", "/etc", "/backup/nightly/pvcs/pvc-1"} {
		if _, _, err := r.BuildFileList(context.Background(), "fl-1", testShareA, p, ""); err == nil {
			t.Errorf("path %q was accepted", p)
		}
	}
	if _, _, err := r.BuildFileList(context.Background(), "fl-1", testShareA, testBackupPath, "Maybe"); err == nil {
		t.Error("an unknown consistency was accepted")
	}
}

func TestBuildFileList_ReaderIsReadOnlyAndHasNoSecrets(t *testing.T) {
	r, key := newListRunner()
	r.SetBrowseGuest(testGuestImage, BrowseGuestConfig{})
	c := r.client
	if done, _, err := r.BuildFileList(context.Background(), key, testShareA, testBackupPath, "CrashConsistent"); err != nil || done {
		t.Fatalf("BuildFileList() = %v, %v", done, err)
	}
	pod := getShareJob(t, c, key).Spec.Template.Spec
	if pod.AutomountServiceAccountToken == nil || *pod.AutomountServiceAccountToken {
		t.Error("the pod must not get an API token")
	}
	main := pod.Containers[0]
	args := strings.Join(main.Command, " ")
	for _, want := range []string{"--mode=build-list", "--root=/backup", "--guest-socket=/tmp/disk.sock",
		"--guest-appliance=/opt/guestfs-appliance", "--guest-consistency=CrashConsistent", "--list-out=/out/filelist.json.gz"} {
		if !strings.Contains(args, want) {
			t.Errorf("command lacks %q: %s", want, args)
		}
	}
	if main.Image != testGuestImage {
		t.Errorf("image = %q", main.Image)
	}
	for _, m := range main.VolumeMounts {
		if m.MountPath == "/backup" && !m.ReadOnly {
			t.Error("the reader must mount the backup read-only")
		}
	}
	for _, v := range pod.Volumes {
		if v.Secret != nil {
			t.Errorf("volume %s is a Secret; the reader holds none", v.Name)
		}
	}
	if len(main.Env) != 0 {
		t.Errorf("env = %v", main.Env)
	}
}

func TestBuildFileList_OnlyThePublisherWritesToTheShare(t *testing.T) {
	r, key := newListRunner()
	_, _, _ = r.BuildFileList(context.Background(), key, testShareA, testBackupPath, "")
	pod := getShareJob(t, r.client, key).Spec.Template.Spec
	if len(pod.Containers) != 2 {
		t.Fatalf("containers = %d; want the reader and the publisher", len(pod.Containers))
	}
	pub := pod.Containers[1]
	var dest *corev1.VolumeMount
	for i, m := range pub.VolumeMounts {
		if m.MountPath == "/dest" {
			dest = &pub.VolumeMounts[i]
		}
	}
	if dest == nil || dest.ReadOnly || dest.SubPath != strings.TrimPrefix(testBackupPath, "/") {
		t.Fatalf("publisher mounts = %+v", pub.VolumeMounts)
	}
	script := strings.Join(pub.Command, " ")
	for _, want := range []string{"/out/filelist.json.gz", "/dest/.filelist.json.gz", "mv ", `[ ! -L "$f" ]`, "head -c 268435456", "stat -c %s", `"$f.failed"`} {
		if !strings.Contains(script, want) {
			t.Errorf("publisher script lacks %q: %s", want, script)
		}
	}
	for _, m := range pub.VolumeMounts {
		if m.Name == "guest-tmp" {
			t.Error("the publisher must not share the reader's scratch folders")
		}
	}
	if pub.SecurityContext == nil || pub.SecurityContext.Capabilities == nil || len(pub.SecurityContext.Capabilities.Add) != 0 {
		t.Errorf("the publisher needs no capabilities: %+v", pub.SecurityContext)
	}
}

func TestBuildFileList_ReadsTheOutcome(t *testing.T) {
	r, key := newListRunner()
	ctx := context.Background()
	_, _, _ = r.BuildFileList(ctx, key, testShareA, testBackupPath, "")
	setJobStatus(t, r.client, key, 1, 0)
	job := getShareJob(t, r.client, key)
	msg := `{"moment":"2026-10-21T09:30:00Z","entries":28758,"bytes":737000,"notes":["sdb1 could not be listed"]}`
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: key + "-pod", Namespace: testDriverNS, Labels: job.Spec.Template.Labels},
		Status: corev1.PodStatus{ContainerStatuses: []corev1.ContainerStatus{
			{Name: mainContainerName, State: corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{Message: msg}}},
			{Name: "publish", State: corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{Message: `{"entries":-1}`}}},
		}},
	}
	if err := r.client.Create(ctx, pod); err != nil {
		t.Fatal(err)
	}
	done, out, err := r.BuildFileList(ctx, key, testShareA, testBackupPath, "")
	if err != nil || !done {
		t.Fatalf("BuildFileList() = %v, %v", done, err)
	}
	want := time.Date(2026, 10, 21, 9, 30, 0, 0, time.UTC)
	if out.Entries != 28758 || out.Bytes != 737000 || !out.Moment.Equal(want) || len(out.Notes) != 1 {
		t.Errorf("outcome = %+v", out)
	}
}

func TestParseFileListOutcome_RefusesWhatCannotBeTrue(t *testing.T) {
	for _, msg := range []string{"", "garbage", `{"entries":-1}`, `{"entries":1,"bytes":-5}`, `{"entries":1,"moment":"yesterday"}`} {
		if _, ok := parseFileListOutcome(msg); ok {
			t.Errorf("%q was accepted", msg)
		}
	}
	long := strings.Repeat("x", 5000)
	out, ok := parseFileListOutcome(`{"entries":1,"bytes":1,"moment":"2026-10-21T09:30:00Z","notes":["` + long + `","a","b","c","d","e","f","g","h","i","j"]}`)
	if !ok || len(out.Notes) > 8 {
		t.Fatalf("ok=%v notes=%d", ok, len(out.Notes))
	}
	for _, n := range out.Notes {
		if len(n) > 300 {
			t.Errorf("a note of %d bytes was kept", len(n))
		}
	}
}
