package pool

import (
	"context"
	"fmt"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/klog/v2"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/builder"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/predicate"

	"github.com/IBM/ibm-vpc-file-pool-csi/pkg/k8s"
	"github.com/IBM/ibm-vpc-file-pool-csi/pkg/util"
)

// ClaimRebindReconciler keeps each SubVolume's claim equal to the claim its
// volume is bound to. CDI provisions a clone through a temporary claim and
// then binds the volume to the real claim; the provisioner only saw the
// temporary one. Without this, the driver cannot find the VM that uses the
// disk, and snapshots, backups and replication treat a running disk as idle.
type ClaimRebindReconciler struct {
	Reader     client.Reader
	K8sClient  k8s.Client
	DriverName string
}

// Reconcile implements reconcile.Reconciler for one PersistentVolume.
func (r *ClaimRebindReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	pv := &corev1.PersistentVolume{}
	if err := r.Reader.Get(ctx, req.NamespacedName, pv); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}
	if pv.Spec.CSI == nil || pv.Spec.CSI.Driver != r.DriverName ||
		pv.Status.Phase != corev1.VolumeBound || pv.Spec.ClaimRef == nil {
		return ctrl.Result{}, nil
	}
	_, _, svName, err := util.ParseVolumeID(pv.Spec.CSI.VolumeHandle)
	if err != nil {
		return ctrl.Result{}, nil
	}
	sv, err := r.K8sClient.GetSubVolume(ctx, svName)
	if apierrors.IsNotFound(err) {
		return ctrl.Result{}, nil
	}
	if err != nil {
		return ctrl.Result{}, fmt.Errorf("read SubVolume %s: %w", svName, err)
	}
	claim := pv.Spec.ClaimRef
	if sv.Spec.PVCNamespace == claim.Namespace && sv.Spec.PVCName == claim.Name {
		return ctrl.Result{}, nil
	}
	klog.InfoS("SubVolume follows the claim its volume is bound to", "subVolume", svName,
		"from", klog.KRef(sv.Spec.PVCNamespace, sv.Spec.PVCName), "to", klog.KRef(claim.Namespace, claim.Name))
	sv.Spec.PVCNamespace, sv.Spec.PVCName = claim.Namespace, claim.Name
	if err := r.K8sClient.UpdateSubVolume(ctx, sv); err != nil {
		return ctrl.Result{}, fmt.Errorf("update SubVolume %s: %w", svName, err)
	}
	return ctrl.Result{}, nil
}

// SetupWithManager registers the reconciler for this driver's volumes.
func (r *ClaimRebindReconciler) SetupWithManager(mgr ctrl.Manager) error {
	ours := predicate.NewPredicateFuncs(func(o client.Object) bool {
		pv, ok := o.(*corev1.PersistentVolume)
		return ok && pv.Spec.CSI != nil && pv.Spec.CSI.Driver == r.DriverName
	})
	return ctrl.NewControllerManagedBy(mgr).
		For(&corev1.PersistentVolume{}, builder.WithPredicates(ours)).
		Named("claimrebind").
		Complete(r)
}
