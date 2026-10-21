package main

import (
	"context"
	"testing"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/kubernetes/fake"
	"sigs.k8s.io/controller-runtime/pkg/client"
	fakeclient "sigs.k8s.io/controller-runtime/pkg/client/fake"
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

func kubevirtIn(ns string) client.Reader {
	s := runtime.NewScheme()
	gvk := schema.GroupVersionKind{Group: "kubevirt.io", Version: "v1", Kind: "KubeVirt"}
	s.AddKnownTypeWithName(gvk, &unstructured.Unstructured{})
	s.AddKnownTypeWithName(gvk.GroupVersion().WithKind("KubeVirtList"), &unstructured.UnstructuredList{})
	b := fakeclient.NewClientBuilder().WithScheme(s)
	if ns != "" {
		kv := &unstructured.Unstructured{}
		kv.SetGroupVersionKind(gvk)
		kv.SetNamespace(ns)
		kv.SetName("kubevirt")
		b = b.WithObjects(kv)
	}
	return b.Build()
}

func TestDiscoverLauncherImage(t *testing.T) {
	ctx := context.Background()
	ocp := fake.NewSimpleClientset(virtOperator("openshift-cnv", "registry.redhat.io/cnv/virt-launcher@sha256:1"))
	if got := discoverLauncherImage(ctx, ocp, kubevirtIn("openshift-cnv")); got != "registry.redhat.io/cnv/virt-launcher@sha256:1" {
		t.Errorf("OpenShift Virtualization: got %q", got)
	}
	up := fake.NewSimpleClientset(virtOperator("kubevirt", "quay.io/kubevirt/virt-launcher:v1.7.0"))
	if got := discoverLauncherImage(ctx, up, kubevirtIn("kubevirt")); got != "quay.io/kubevirt/virt-launcher:v1.7.0" {
		t.Errorf("upstream KubeVirt: got %q", got)
	}
	if got := discoverLauncherImage(ctx, fake.NewSimpleClientset(), kubevirtIn("")); got != "" {
		t.Errorf("no KubeVirt: got %q", got)
	}
	// A user who made a "kubevirt" project with a fake virt-operator must not
	// choose the image: KubeVirt is installed in openshift-cnv.
	planted := fake.NewSimpleClientset(virtOperator("kubevirt", "evil.example/launcher:1"))
	if got := discoverLauncherImage(ctx, planted, kubevirtIn("openshift-cnv")); got != "" {
		t.Errorf("a Deployment outside KubeVirt's namespace was trusted: %q", got)
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
