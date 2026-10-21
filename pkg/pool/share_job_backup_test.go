package pool

import (
	"context"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

const (
	testBackupPath  = "/backup/nightly/pvcs/pvc-11111111-2222-3333-4444-555555555555"
	testMomentsPath = "/backup/nightly/.moments/pvcs/pvc-11111111-2222-3333-4444-555555555555"
)

func TestRemoveBackup_JobRemovesBothFoldersAsRoot(t *testing.T) {
	r, c := newShareJobTestRunner()
	done, err := r.RemoveBackup(context.Background(), "bk-rm", testShareA, testBackupPath, testMomentsPath)
	if err != nil || done {
		t.Fatalf("RemoveBackup() = %v, %v; want false, nil", done, err)
	}
	job := getShareJob(t, c, "bk-rm")
	script := jobScript(job)
	for _, p := range []string{testBackupPath, testMomentsPath} {
		if !strings.Contains(script, `rm -r -f -- "/share0`+p+`"`) {
			t.Errorf("script does not remove %s:\n%s", p, script)
		}
	}
	if strings.Count(script, "rm ") != 2 {
		t.Errorf("script must remove exactly the two folders:\n%s", script)
	}
}

func TestRemoveBackup_RefusesUnsafePathsBeforeAnyJob(t *testing.T) {
	cases := map[string][2]string{
		"traversal":         {"/backup/nightly/pvcs/../../etc", testMomentsPath},
		"a data volume":     {"/pvcs/pvc-11111111-2222-3333-4444-555555555555", testMomentsPath},
		"the policy folder": {"/backup/nightly", testMomentsPath},
		"moments as backup": {testMomentsPath, testMomentsPath},
		"backup as moments": {testBackupPath, testBackupPath},
		"empty":             {"", ""},
		"shell quote":       {`/backup/nightly/pvcs/pvc-11111111-2222-3333-4444-55555555555"; rm -rf /`, testMomentsPath},
	}
	for name, p := range cases {
		t.Run(name, func(t *testing.T) {
			r, c := newShareJobTestRunner()
			if _, err := r.RemoveBackup(context.Background(), "bk-rm", testShareA, p[0], p[1]); err == nil {
				t.Fatal("want an error")
			}
			if n := countPVs(t, c); n != 0 {
				t.Errorf("%d share volume(s) made for an unsafe path", n)
			}
		})
	}
}

func TestRemoveBackup_FoldersOfTwoPoliciesAreRefused(t *testing.T) {
	r, _ := newShareJobTestRunner()
	other := "/backup/weekly/.moments/pvcs/pvc-11111111-2222-3333-4444-555555555555"
	if _, err := r.RemoveBackup(context.Background(), "bk-rm", testShareA, testBackupPath, other); err == nil {
		t.Fatal("folders of two different policies must be refused")
	}
}

func TestRemoveBackup_FoldersOfTwoVolumesAreRefused(t *testing.T) {
	r, _ := newShareJobTestRunner()
	other := "/backup/nightly/.moments/pvcs/pvc-66666666-7777-8888-9999-000000000000"
	if _, err := r.RemoveBackup(context.Background(), "bk-rm", testShareA, testBackupPath, other); err == nil {
		t.Fatal("folders of two different volumes must be refused")
	}
}

func TestRemoveBackup_SucceededJobIsDone(t *testing.T) {
	r, c := newShareJobTestRunner()
	ctx := context.Background()
	_, _ = r.RemoveBackup(ctx, "bk-rm", testShareA, testBackupPath, testMomentsPath)
	setJobStatus(t, c, "bk-rm", 1, 0)
	done, err := r.RemoveBackup(ctx, "bk-rm", testShareA, testBackupPath, testMomentsPath)
	if err != nil || !done {
		t.Fatalf("RemoveBackup() = %v, %v; want true, nil", done, err)
	}
}

func TestMeasureBackup_ReadsTheSizeFromTheTerminationMessage(t *testing.T) {
	r, c := newShareJobTestRunner()
	ctx := context.Background()
	done, _, err := r.MeasureBackup(ctx, "bk-du", testShareA, testBackupPath, testMomentsPath)
	if err != nil || done {
		t.Fatalf("MeasureBackup() = %v, %v; want false, nil", done, err)
	}
	script := jobScript(getShareJob(t, c, "bk-du"))
	if strings.Contains(script, "rm ") {
		t.Errorf("a measuring Job must not remove anything:\n%s", script)
	}
	for _, p := range []string{testBackupPath, testMomentsPath} {
		if !strings.Contains(script, "/share0"+p) {
			t.Errorf("script does not measure %s:\n%s", p, script)
		}
	}
	setJobStatus(t, c, "bk-du", 1, 0)
	job := getShareJob(t, c, "bk-du")
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: "bk-du-pod", Namespace: testDriverNS, Labels: job.Spec.Template.Labels},
		Status: corev1.PodStatus{ContainerStatuses: []corev1.ContainerStatus{{
			State: corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{Message: "2048\n"}},
		}}},
	}
	if err = c.Create(ctx, pod); err != nil {
		t.Fatal(err)
	}
	done, bytes, err := r.MeasureBackup(ctx, "bk-du", testShareA, testBackupPath, testMomentsPath)
	if err != nil || !done || bytes != 2048*1024 {
		t.Fatalf("MeasureBackup() = %v, %d, %v; want true, 2097152, nil", done, bytes, err)
	}
}

func TestMeasureBackup_AnUnreadableSizeIsAnError(t *testing.T) {
	r, c := newShareJobTestRunner()
	ctx := context.Background()
	_, _, _ = r.MeasureBackup(ctx, "bk-du", testShareA, testBackupPath, testMomentsPath)
	setJobStatus(t, c, "bk-du", 1, 0)
	if _, _, err := r.MeasureBackup(ctx, "bk-du", testShareA, testBackupPath, testMomentsPath); err == nil {
		t.Fatal("a Job with no size message must be an error, not 0 bytes")
	}
}
