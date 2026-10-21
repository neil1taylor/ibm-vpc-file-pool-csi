package main

import (
	"context"
	"fmt"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/kubernetes"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// vmSnapshotOptions are the flags for snapshots of VM disks.
type vmSnapshotOptions struct {
	// method is "auto" (instant snapshots where the cluster allows them) or
	// "copy" (never instant).
	method string
	// launcherImage is KubeVirt's virt-launcher image for the Jobs that change
	// a stopped VM's snapshot chain; empty finds it from virt-operator.
	launcherImage string
}

func (o vmSnapshotOptions) check() error {
	if o.method != "auto" && o.method != "copy" {
		return fmt.Errorf("--vm-snapshot-method must be auto or copy, got %q", o.method)
	}
	return nil
}

// discoverLauncherImage reads the virt-launcher image from the virt-operator
// Deployment in the namespace where KubeVirt is installed, or returns "". The
// namespace comes from the KubeVirt resource, which only an admin can make: a
// Deployment found by a fixed namespace name alone could be anyone's, and the
// image runs in Jobs that mount the shares.
func discoverLauncherImage(ctx context.Context, cs kubernetes.Interface, c client.Reader) string {
	list := &unstructured.UnstructuredList{}
	list.SetGroupVersionKind(schema.GroupVersionKind{Group: "kubevirt.io", Version: "v1", Kind: "KubeVirtList"})
	if err := c.List(ctx, list); err != nil || len(list.Items) != 1 {
		return ""
	}
	d, err := cs.AppsV1().Deployments(list.Items[0].GetNamespace()).Get(ctx, "virt-operator", metav1.GetOptions{})
	if err != nil {
		return ""
	}
	for _, ct := range d.Spec.Template.Spec.Containers {
		for _, e := range ct.Env {
			if e.Name == "VIRT_LAUNCHER_IMAGE" && e.Value != "" {
				return e.Value
			}
		}
	}
	return ""
}
