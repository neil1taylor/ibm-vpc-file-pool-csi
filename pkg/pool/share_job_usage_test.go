package pool

import (
	"context"
	"errors"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func TestShareJobRunner_ShareUsage_FirstCallStartsAReadOnlyDfJob(t *testing.T) {
	r, c := newShareJobTestRunner()
	done, pct, err := r.ShareUsage(context.Background(), "usage-a", testShareA)
	if err != nil || done || pct != 0 {
		t.Fatalf("ShareUsage() = %v, %d, %v; want false, 0, nil", done, pct, err)
	}
	script := jobScript(getShareJob(t, c, "usage-a"))
	for _, want := range []string{"df -P /share0", "/dev/termination-log"} {
		if !strings.Contains(script, want) {
			t.Errorf("script lacks %q:\n%s", want, script)
		}
	}
	if strings.Contains(script, "rm ") || strings.Contains(script, "cp ") {
		t.Errorf("a usage Job must only read:\n%s", script)
	}
}

func TestShareJobRunner_ShareUsage_SucceededReturnsThePercentage(t *testing.T) {
	r, c := newShareJobTestRunner()
	ctx := context.Background()
	if _, _, err := r.ShareUsage(ctx, "usage-a", testShareA); err != nil {
		t.Fatal(err)
	}
	setJobStatus(t, c, "usage-a", 1, 0)
	job := getShareJob(t, c, "usage-a")
	if err := c.Create(ctx, &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: "usage-a-pod", Namespace: testDriverNS, Labels: job.Spec.Template.Labels},
		Status: corev1.PodStatus{ContainerStatuses: []corev1.ContainerStatus{{
			Name: "share-data",
			State: corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{
				Message: "10.240.0.1:/share_a 104857600 90000000 14857600 86% /share0\n",
			}},
		}}},
	}); err != nil {
		t.Fatal(err)
	}
	done, pct, err := r.ShareUsage(ctx, "usage-a", testShareA)
	if err != nil || !done || pct != 86 {
		t.Fatalf("ShareUsage() = %v, %d, %v; want true, 86, nil", done, pct, err)
	}
	if countPVs(t, c) != 0 {
		t.Errorf("the share PV was not removed")
	}
}

func TestShareJobRunner_ShareUsage_UnreadableOutputIsAnError(t *testing.T) {
	r, c := newShareJobTestRunner()
	ctx := context.Background()
	if _, _, err := r.ShareUsage(ctx, "usage-a", testShareA); err != nil {
		t.Fatal(err)
	}
	setJobStatus(t, c, "usage-a", 1, 0)
	if _, _, err := r.ShareUsage(ctx, "usage-a", testShareA); err == nil {
		t.Errorf("a Job that left no output must give an error, not 0%%")
	}
}

func TestShareJobRunner_ShareUsage_FailedJobReportsError(t *testing.T) {
	r, c := newShareJobTestRunner()
	ctx := context.Background()
	if _, _, err := r.ShareUsage(ctx, "usage-a", testShareA); err != nil {
		t.Fatal(err)
	}
	setJobStatus(t, c, "usage-a", 0, shareJobBackoffLimit+1)
	if _, _, err := r.ShareUsage(ctx, "usage-a", testShareA); !errors.Is(err, ErrShareJobFailed) {
		t.Errorf("err = %v, want ErrShareJobFailed", err)
	}
}

func TestParseDfPercent(t *testing.T) {
	for in, want := range map[string]int{
		"10.0.0.1:/x 100 50 50 50% /share0\n":                  50,
		"Filesystem 1K-blocks Used Available Capacity Mounted": -1,
		"":                -1,
		"a 1 2 3 101% /m": 101,
		"a 1 2 3 x% /m":   -1,
	} {
		if got := parseDfPercent(in); got != want {
			t.Errorf("parseDfPercent(%q) = %d, want %d", in, got, want)
		}
	}
}
