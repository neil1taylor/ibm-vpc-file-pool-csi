package main

import (
	"context"
	"fmt"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
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
// Deployment of OpenShift Virtualization or upstream KubeVirt, or returns "".
func discoverLauncherImage(ctx context.Context, cs kubernetes.Interface) string {
	for _, ns := range []string{"openshift-cnv", "kubevirt"} {
		d, err := cs.AppsV1().Deployments(ns).Get(ctx, "virt-operator", metav1.GetOptions{})
		if err != nil {
			continue
		}
		for _, c := range d.Spec.Template.Spec.Containers {
			for _, e := range c.Env {
				if e.Name == "VIRT_LAUNCHER_IMAGE" && e.Value != "" {
					return e.Value
				}
			}
		}
	}
	return ""
}
