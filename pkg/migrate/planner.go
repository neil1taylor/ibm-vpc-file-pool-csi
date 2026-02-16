// Package migrate provides PVC migration from the stock IBM VPC File CSI
// driver (1:1 PVC-to-share) to the pool CSI driver (many PVCs per share).
package migrate

import (
	"context"
	"fmt"
	"sort"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
)

// PVCInfo describes a PVC that is a candidate for migration.
type PVCInfo struct {
	// Name is the PVC name.
	Name string
	// Namespace is the PVC namespace.
	Namespace string
	// StorageClass is the StorageClass name.
	StorageClass string
	// SizeGB is the requested storage in GiB.
	SizeGB int64
	// VolumeName is the bound PV name.
	VolumeName string
	// ShareID is the VPC file share ID from the PV's volumeAttributes, if available.
	ShareID string
	// Pods lists the names of pods currently using this PVC.
	Pods []string
	// Phase is the PVC phase (Bound, Pending, etc.).
	Phase corev1.PersistentVolumeClaimPhase
}

// MigrationPlan is the output of planning a migration.
type MigrationPlan struct {
	// Namespace is the target namespace.
	Namespace string
	// SourceStorageClass is the old StorageClass.
	SourceStorageClass string
	// TargetPool is the pool name to migrate into.
	TargetPool string
	// PVCs lists the PVCs to migrate.
	PVCs []PVCInfo
	// TotalSizeGB is the aggregate size across all PVCs.
	TotalSizeGB int64
}

// Planner discovers PVCs and builds a migration plan.
type Planner struct {
	clientset kubernetes.Interface
}

// NewPlanner creates a Planner that uses the given kubernetes.Interface.
func NewPlanner(clientset kubernetes.Interface) *Planner {
	return &Planner{clientset: clientset}
}

// Plan discovers PVCs in namespace using the specified storageClass and
// returns a MigrationPlan targeting the given pool.
func (p *Planner) Plan(ctx context.Context, namespace, storageClass, targetPool string) (*MigrationPlan, error) {
	pvcList, err := p.clientset.CoreV1().PersistentVolumeClaims(namespace).List(ctx, metav1.ListOptions{})
	if err != nil {
		return nil, fmt.Errorf("failed to list PVCs in namespace %s: %w", namespace, err)
	}

	var candidates []PVCInfo
	var totalSizeGB int64

	for i := range pvcList.Items {
		pvc := &pvcList.Items[i]
		sc := ""
		if pvc.Spec.StorageClassName != nil {
			sc = *pvc.Spec.StorageClassName
		}
		if sc != storageClass {
			continue
		}

		sizeGB := int64(0)
		if q, ok := pvc.Spec.Resources.Requests[corev1.ResourceStorage]; ok {
			// Convert to GiB (integer ceiling).
			sizeGB = q.Value() / (1024 * 1024 * 1024)
			if q.Value()%(1024*1024*1024) != 0 {
				sizeGB++
			}
		}

		shareID := ""
		if pvc.Spec.VolumeName != "" {
			shareID = p.lookupShareID(ctx, pvc.Spec.VolumeName)
		}

		pods, err := p.findPodsUsingPVC(ctx, namespace, pvc.Name)
		if err != nil {
			// Non-fatal: we can still plan without pod information.
			pods = nil
		}

		candidates = append(candidates, PVCInfo{
			Name:         pvc.Name,
			Namespace:    pvc.Namespace,
			StorageClass: sc,
			SizeGB:       sizeGB,
			VolumeName:   pvc.Spec.VolumeName,
			ShareID:      shareID,
			Pods:         pods,
			Phase:        pvc.Status.Phase,
		})
		totalSizeGB += sizeGB
	}

	sort.Slice(candidates, func(i, j int) bool {
		return candidates[i].Name < candidates[j].Name
	})

	return &MigrationPlan{
		Namespace:          namespace,
		SourceStorageClass: storageClass,
		TargetPool:         targetPool,
		PVCs:               candidates,
		TotalSizeGB:        totalSizeGB,
	}, nil
}

// lookupShareID retrieves the VPC share ID from a PV's volumeAttributes.
func (p *Planner) lookupShareID(ctx context.Context, pvName string) string {
	pv, err := p.clientset.CoreV1().PersistentVolumes().Get(ctx, pvName, metav1.GetOptions{})
	if err != nil {
		return ""
	}
	if pv.Spec.CSI == nil {
		return ""
	}
	// The stock IBM VPC file CSI driver stores the share ID in volumeAttributes.
	if id, ok := pv.Spec.CSI.VolumeAttributes["shareID"]; ok {
		return id
	}
	if id, ok := pv.Spec.CSI.VolumeAttributes["volumeId"]; ok {
		return id
	}
	return ""
}

// findPodsUsingPVC returns the names of pods in namespace that mount the given PVC.
func (p *Planner) findPodsUsingPVC(ctx context.Context, namespace, pvcName string) ([]string, error) {
	podList, err := p.clientset.CoreV1().Pods(namespace).List(ctx, metav1.ListOptions{})
	if err != nil {
		return nil, err
	}

	var names []string
	for i := range podList.Items {
		pod := &podList.Items[i]
		for _, vol := range pod.Spec.Volumes {
			if vol.PersistentVolumeClaim != nil && vol.PersistentVolumeClaim.ClaimName == pvcName {
				names = append(names, pod.Name)
				break
			}
		}
	}
	return names, nil
}
