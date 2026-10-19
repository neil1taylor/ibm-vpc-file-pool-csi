package main

import (
	"context"
	"slices"
	"testing"

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
