package main

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	v1alpha1 "github.com/IBM/ibm-vpc-file-pool-csi/api/v1alpha1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	fakeclient "sigs.k8s.io/controller-runtime/pkg/client/fake"
)

var (
	last   = metav1.NewTime(time.Date(2026, 10, 21, 9, 0, 0, 0, time.UTC))
	oldest = metav1.NewTime(time.Date(2026, 10, 20, 9, 0, 0, 0, time.UTC))
)

func testPolicy() *v1alpha1.BackupPolicy {
	return &v1alpha1.BackupPolicy{
		ObjectMeta: metav1.ObjectMeta{Name: "nightly"},
		Status: v1alpha1.BackupPolicyStatus{
			Phase: "Active",
			Volumes: []v1alpha1.BackupVolumeStatus{
				{PVCNamespace: "vms", PVCName: "web-root", SubVolumeName: "pvc-a", VMName: "web", LastBackupTime: &last,
					OldestBackupTime: &oldest, RestorePoints: 7, Consistency: "FileSystemConsistent", SubPath: "/pvcs/pvc-a"},
				{PVCNamespace: "apps", PVCName: "db", SubVolumeName: "pvc-b", LastBackupTime: &last, Consistency: "FileCopy",
					Retained: true, LastError: "disk full", SubPath: "/pvcs/pvc-b"},
			},
		},
	}
}

func newClient(objs ...client.Object) client.Client {
	s := runtime.NewScheme()
	_ = v1alpha1.AddToScheme(s)
	return fakeclient.NewClientBuilder().WithScheme(s).WithObjects(objs...).
		WithStatusSubresource(&v1alpha1.BackupRestore{}, &v1alpha1.BackupPolicy{}).Build()
}

func TestList_ShowsEachVolumeOfEachPolicy(t *testing.T) {
	var out bytes.Buffer
	if err := run(context.Background(), newClient(testPolicy()), &out, []string{"list"}, time.Now); err != nil {
		t.Fatal(err)
	}
	got := out.String()
	for _, want := range []string{"nightly", "vms/web-root", "web", "FileSystemConsistent", "2026-10-21T09:00:00Z", "2026-10-20T09:00:00Z",
		"apps/db", "FileCopy", "retained", "disk full"} {
		if !strings.Contains(got, want) {
			t.Errorf("list output lacks %q:\n%s", want, got)
		}
	}
}

func TestList_FilterByPolicyAndShowsAPolicyInError(t *testing.T) {
	broken := &v1alpha1.BackupPolicy{ObjectMeta: metav1.ObjectMeta{Name: "broken"},
		Status: v1alpha1.BackupPolicyStatus{Phase: "Error", Message: "the snapshot pool \"x\" does not exist"}}
	c := newClient(testPolicy(), broken)
	var out bytes.Buffer
	if err := run(context.Background(), c, &out, []string{"list", "--policy", "broken"}, time.Now); err != nil {
		t.Fatal(err)
	}
	got := out.String()
	if strings.Contains(got, "web-root") {
		t.Errorf("another policy's volumes are listed:\n%s", got)
	}
	if !strings.Contains(got, "does not exist") {
		t.Errorf("the policy's error is not shown:\n%s", got)
	}
	if err := run(context.Background(), c, &out, []string{"list", "--policy", "nope"}, time.Now); err == nil {
		t.Error("want an error for a policy that does not exist")
	}
}

func TestRestore_MakesTheRequest(t *testing.T) {
	c := newClient(testPolicy())
	var out bytes.Buffer
	now := func() time.Time { return time.Date(2026, 10, 21, 10, 0, 0, 0, time.UTC) }
	err := run(context.Background(), c, &out, []string{"restore", "--policy", "nightly", "--pvc", "vms/web-root",
		"--to", "vms/web-root-restored", "--moment", "2026-10-20T12:00:00Z", "--storage-class", "fast"}, now)
	if err != nil {
		t.Fatal(err)
	}
	l := &v1alpha1.BackupRestoreList{}
	if err := c.List(context.Background(), l); err != nil || len(l.Items) != 1 {
		t.Fatalf("restores = %v, %v; want one", l.Items, err)
	}
	sp := l.Items[0].Spec
	if sp.Policy != "nightly" || sp.SourceNamespace != "vms" || sp.SourceName != "web-root" ||
		sp.TargetNamespace != "vms" || sp.TargetName != "web-root-restored" || sp.StorageClassName != "fast" ||
		sp.Moment == nil || !sp.Moment.Equal(&metav1.Time{Time: time.Date(2026, 10, 20, 12, 0, 0, 0, time.UTC)}) {
		t.Errorf("spec = %+v", sp)
	}
	if !strings.HasPrefix(l.Items[0].Name, "restore-web-root-") {
		t.Errorf("name = %q", l.Items[0].Name)
	}
	if !strings.Contains(out.String(), l.Items[0].Name) {
		t.Errorf("output does not name the request:\n%s", out.String())
	}
}

func TestRestore_RefusesBadRequestsBeforeMakingAnything(t *testing.T) {
	cases := map[string][]string{
		"no policy":         {"restore", "--pvc", "vms/web-root", "--to", "vms/x"},
		"no pvc":            {"restore", "--policy", "nightly", "--to", "vms/x"},
		"no target":         {"restore", "--policy", "nightly", "--pvc", "vms/web-root"},
		"pvc without ns":    {"restore", "--policy", "nightly", "--pvc", "web-root", "--to", "vms/x"},
		"target without ns": {"restore", "--policy", "nightly", "--pvc", "vms/web-root", "--to", "x"},
		"bad moment":        {"restore", "--policy", "nightly", "--pvc", "vms/web-root", "--to", "vms/x", "--moment", "yesterday"},
		"unknown policy":    {"restore", "--policy", "other", "--pvc", "vms/web-root", "--to", "vms/x"},
		"no backup of pvc":  {"restore", "--policy", "nightly", "--pvc", "vms/none", "--to", "vms/x"},
	}
	for name, args := range cases {
		c := newClient(testPolicy())
		if err := run(context.Background(), c, &bytes.Buffer{}, args, time.Now); err == nil {
			t.Errorf("%s: want an error", name)
		}
		l := &v1alpha1.BackupRestoreList{}
		_ = c.List(context.Background(), l)
		if len(l.Items) != 0 {
			t.Errorf("%s: a request was made for a bad command", name)
		}
	}
}

func TestRestore_WaitReportsTheResult(t *testing.T) {
	c := newClient(testPolicy())
	var out bytes.Buffer
	// A controller answers a moment after the request.
	go func() {
		for i := 0; i < 200; i++ {
			l := &v1alpha1.BackupRestoreList{}
			if err := c.List(context.Background(), l); err == nil && len(l.Items) == 1 {
				br := l.Items[0].DeepCopy()
				m := metav1.NewTime(time.Date(2026, 10, 20, 9, 0, 0, 0, time.UTC))
				br.Status = v1alpha1.BackupRestoreStatus{Phase: "Done", RestoredMoment: &m}
				_ = c.Status().Update(context.Background(), br)
				return
			}
			time.Sleep(10 * time.Millisecond)
		}
	}()
	args := []string{"restore", "--policy", "nightly", "--pvc", "vms/web-root", "--to", "vms/x", "--wait", "--poll", "10ms", "--timeout", "10s"}
	if err := run(context.Background(), c, &out, args, time.Now); err != nil {
		t.Fatalf("err = %v\n%s", err, out.String())
	}
	if !strings.Contains(out.String(), "2026-10-20T09:00:00Z") || !strings.Contains(out.String(), "Done") {
		t.Errorf("output lacks the result:\n%s", out.String())
	}
}

func TestRestore_WaitFailsWhenTheRestoreFails(t *testing.T) {
	c := newClient(testPolicy())
	go func() {
		for i := 0; i < 200; i++ {
			l := &v1alpha1.BackupRestoreList{}
			if err := c.List(context.Background(), l); err == nil && len(l.Items) == 1 {
				br := l.Items[0].DeepCopy()
				br.Status = v1alpha1.BackupRestoreStatus{Phase: "Failed", Message: "vms/x exists"}
				_ = c.Status().Update(context.Background(), br)
				return
			}
			time.Sleep(10 * time.Millisecond)
		}
	}()
	args := []string{"restore", "--policy", "nightly", "--pvc", "vms/web-root", "--to", "vms/x", "--wait", "--poll", "10ms", "--timeout", "10s"}
	err := run(context.Background(), c, &bytes.Buffer{}, args, time.Now)
	if err == nil || !strings.Contains(err.Error(), "vms/x exists") {
		t.Errorf("err = %v, want the failure reason", err)
	}
}

func TestRestore_WaitTimesOut(t *testing.T) {
	c := newClient(testPolicy())
	args := []string{"restore", "--policy", "nightly", "--pvc", "vms/web-root", "--to", "vms/x", "--wait", "--poll", "10ms", "--timeout", "50ms"}
	err := run(context.Background(), c, &bytes.Buffer{}, args, time.Now)
	if err == nil || !strings.Contains(err.Error(), "timed out") {
		t.Errorf("err = %v, want a timeout", err)
	}
	// The request stays: the controller may still finish it.
	l := &v1alpha1.BackupRestoreList{}
	if err := c.List(context.Background(), l); err != nil || len(l.Items) != 1 {
		t.Errorf("restores = %v, %v; want the request to stay", l.Items, err)
	}
}

// kubectl plugins must use the KUBECONFIG variable when --kubeconfig is not given.
func TestBuildClient_UsesKUBECONFIGVariable(t *testing.T) {
	cfg := filepath.Join(t.TempDir(), "kubeconfig")
	yaml := "apiVersion: v1\nkind: Config\nclusters:\n- name: c\n  cluster:\n    server: https://127.0.0.1:1\ncontexts:\n- name: x\n  context:\n    cluster: c\n    user: u\ncurrent-context: x\nusers:\n- name: u\n  user:\n    token: t\n"
	if err := os.WriteFile(cfg, []byte(yaml), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("KUBECONFIG", cfg)
	t.Setenv("HOME", t.TempDir())
	if _, err := buildClient(""); err != nil {
		t.Fatalf("buildClient did not use KUBECONFIG: %v", err)
	}
}
