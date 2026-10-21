package main

import (
	"context"
	"errors"
	"slices"
	"testing"
	"time"

	"github.com/IBM/ibm-vpc-file-pool-csi/pkg/replication"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"
)

func configMap(name string, data map[string]string) *corev1.ConfigMap {
	return &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "kube-system"}, Data: data}
}

func TestDiscoverNetworkConfig_ReadsAllSubnetsAndClusterID(t *testing.T) {
	clientset := fake.NewClientset(
		configMap("ibm-cloud-provider-data", map[string]string{
			"vpc_id":         "vpc-1",
			"vpc_subnet_ids": "subnet-a, subnet-b,subnet-c",
		}),
		configMap("cluster-info", map[string]string{"cluster_id": "dakmjdqf0rpvr8ketsmg"}),
	)

	cfg := discoverNetworkConfig(context.Background(), clientset, "", "")

	if cfg.vpcID != "vpc-1" {
		t.Errorf("vpcID = %q, want vpc-1", cfg.vpcID)
	}
	if !slices.Equal(cfg.subnetIDs, []string{"subnet-a", "subnet-b", "subnet-c"}) {
		t.Errorf("subnetIDs = %v, want all three subnets", cfg.subnetIDs)
	}
	if cfg.clusterID != "dakmjdqf0rpvr8ketsmg" {
		t.Errorf("clusterID = %q, want dakmjdqf0rpvr8ketsmg", cfg.clusterID)
	}
}

func TestDiscoverNetworkConfig_FlagsOverrideConfigMap(t *testing.T) {
	clientset := fake.NewClientset(
		configMap("ibm-cloud-provider-data", map[string]string{"vpc_id": "vpc-cm", "vpc_subnet_ids": "subnet-cm"}),
	)

	cfg := discoverNetworkConfig(context.Background(), clientset, "vpc-flag", "subnet-x,subnet-y")

	if cfg.vpcID != "vpc-flag" || !slices.Equal(cfg.subnetIDs, []string{"subnet-x", "subnet-y"}) {
		t.Errorf("config = %+v, want the flag values", cfg)
	}
	if cfg.clusterID != "" {
		t.Errorf("clusterID = %q, want empty without cluster-info", cfg.clusterID)
	}
}

func TestVMCopyFlags_ApplyOptions(t *testing.T) {
	now := time.Date(2026, 10, 20, 15, 0, 0, 0, time.UTC)
	opts, err := vmCopyFlags{from: "2026-10-20T13:00:00.5Z", to: "2026-10-20T14:00:00.25Z", keep: 6 * time.Hour}.applyOptions(now)
	if err != nil {
		t.Fatal(err)
	}
	if !opts.From.Equal(time.Date(2026, 10, 20, 13, 0, 0, 5e8, time.UTC)) || !opts.To.Equal(time.Date(2026, 10, 20, 14, 0, 0, 25e7, time.UTC)) ||
		opts.Full || opts.Keep != 6*time.Hour || !opts.Now.Equal(now) {
		t.Errorf("opts = %+v", opts)
	}
	if opts, err = (vmCopyFlags{to: "2026-10-20T14:00:00Z", full: true}).applyOptions(now); err != nil || !opts.From.IsZero() || !opts.Full {
		t.Errorf("first copy: opts = %+v, err = %v", opts, err)
	}
	for _, f := range []vmCopyFlags{{}, {to: "yesterday"}, {to: "2026-10-20T14:00:00Z", from: "noon"}} {
		if _, err := f.applyOptions(now); err == nil {
			t.Errorf("flags %+v: expected an error", f)
		}
	}
}

func TestApplyVMDiskMessage(t *testing.T) {
	oldest := time.Date(2026, 10, 20, 13, 0, 0, 0, time.UTC)
	res := replication.VMDiskResult{RestorePoints: 3, OldestRestorePoint: &oldest}
	res.ChangedBytes = 4096
	want := `{"bytesWritten":4096,"restorePoints":3,"oldestRestorePoint":"2026-10-20T13:00:00Z"}`
	if got := applyVMDiskMessage(res); got != want {
		t.Errorf("message = %s, want %s", got, want)
	}
}

func TestSendVMDiskMessage(t *testing.T) {
	oldest := time.Date(2026, 10, 20, 13, 0, 0, 0, time.UTC)
	res := replication.VMSendResult{SentBytes: 2048, CommitResponse: replication.CommitResponse{RestorePoints: 2, OldestRestorePoint: &oldest}}
	want := `{"bytesWritten":2048,"restorePoints":2,"oldestRestorePoint":"2026-10-20T13:00:00Z"}`
	if got := sendVMDiskMessage(res, nil); got != want {
		t.Errorf("message = %s, want %s", got, want)
	}
	wb := &replication.WrongBaseError{DiskMoment: "2026-10-20T12:00:00Z", Message: "x"}
	want = `{"bytesWritten":0,"restorePoints":0,"wrongBase":true,"diskMoment":"2026-10-20T12:00:00Z"}`
	if got := sendVMDiskMessage(replication.VMSendResult{}, wb); got != want {
		t.Errorf("wrong base message = %s, want %s", got, want)
	}
}

func TestSendVMDiskArgs(t *testing.T) {
	f := vmCopyFlags{from: "2026-10-20T13:00:00Z", to: "2026-10-20T14:00:00Z", keep: time.Hour}
	a, err := sendVMDiskArgs(f, "/", "/pvcs/pvc-1", "pvc-1", "copy-7", "ns/vm")
	if err != nil {
		t.Fatal(err)
	}
	if a.From == nil || a.To.IsZero() || a.CopyID != "copy-7" || a.VMGroup != "ns/vm" || a.Keep != time.Hour || a.Full {
		t.Errorf("args = %+v", a)
	}
	if a, err = sendVMDiskArgs(vmCopyFlags{to: "2026-10-20T14:00:00Z", full: true}, "/", "/p", "pvc-1", "c", "g"); err != nil || a.From != nil || !a.Full {
		t.Errorf("first full copy args = %+v %v", a, err)
	}
	if _, err = sendVMDiskArgs(vmCopyFlags{}, "/", "/p", "pvc-1", "c", "g"); err == nil {
		t.Error("expected an error without --to")
	}
}

func TestCheckReceiverResult(t *testing.T) {
	tests := []struct {
		name     string
		err      error
		wantLine string
		wantCode int
	}{
		{"ok", nil, "ok", 0},
		{"a check error shows its short reason only", &replication.CheckError{Reason: replication.ReasonTokenRefused, Err: errors.New("HTTP 401")}, "token refused", 1},
		{"another error shows its text", errors.New("boom"), "boom", 1},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			line, code := checkReceiverResult(tt.err)
			if line != tt.wantLine || code != tt.wantCode {
				t.Errorf("got (%q, %d), want (%q, %d)", line, code, tt.wantLine, tt.wantCode)
			}
		})
	}
}
