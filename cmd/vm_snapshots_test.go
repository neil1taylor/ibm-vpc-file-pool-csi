package main

import (
	"context"
	"testing"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"
)

func virtOperator(ns, image string) *appsv1.Deployment {
	return &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: "virt-operator"},
		Spec: appsv1.DeploymentSpec{Template: corev1.PodTemplateSpec{Spec: corev1.PodSpec{Containers: []corev1.Container{{
			Name: "virt-operator",
			Env:  []corev1.EnvVar{{Name: "OTHER", Value: "x"}, {Name: "VIRT_LAUNCHER_IMAGE", Value: image}},
		}}}}},
	}
}

func TestDiscoverLauncherImage(t *testing.T) {
	ctx := context.Background()
	if got := discoverLauncherImage(ctx, fake.NewSimpleClientset(virtOperator("openshift-cnv", "registry.redhat.io/cnv/virt-launcher@sha256:1"))); got != "registry.redhat.io/cnv/virt-launcher@sha256:1" {
		t.Errorf("OpenShift Virtualization: got %q", got)
	}
	if got := discoverLauncherImage(ctx, fake.NewSimpleClientset(virtOperator("kubevirt", "quay.io/kubevirt/virt-launcher:v1.7.0"))); got != "quay.io/kubevirt/virt-launcher:v1.7.0" {
		t.Errorf("upstream KubeVirt: got %q", got)
	}
	if got := discoverLauncherImage(ctx, fake.NewSimpleClientset()); got != "" {
		t.Errorf("no KubeVirt: got %q", got)
	}
}

func TestVMSnapshotOptionsCheck(t *testing.T) {
	for _, m := range []string{"auto", "copy"} {
		if err := (vmSnapshotOptions{method: m}).check(); err != nil {
			t.Errorf("%s: %v", m, err)
		}
	}
	if err := (vmSnapshotOptions{method: "fast"}).check(); err == nil {
		t.Error("an unknown method must be refused")
	}
}
