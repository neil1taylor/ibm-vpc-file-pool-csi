package main

import (
	"context"
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
