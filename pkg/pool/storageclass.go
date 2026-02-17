package pool

import (
	"fmt"

	v1alpha1 "github.com/IBM/ibm-vpc-file-pool-csi/api/v1alpha1"
	corev1 "k8s.io/api/core/v1"
	storagev1 "k8s.io/api/storage/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

const (
	// ProvisionerName is the CSI provisioner name used in StorageClasses.
	// Duplicated from pkg/driver to avoid a circular import.
	ProvisionerName = "vpc-file-pool.csi.ibm.io"

	// SkipStorageClassAnnotation opts out of automatic StorageClass creation.
	SkipStorageClassAnnotation = "storage.ibmcloud.io/skip-storageclass"

	// managedByLabel marks StorageClasses created by the controller.
	managedByLabel = "storage.ibmcloud.io/managed-by"
	managedByValue = "ibm-vpc-file-pool-csi"

	// poolLabel identifies which pool owns a StorageClass.
	poolLabel = "storage.ibmcloud.io/pool"
)

// defaultMountOptions are used when pool.Spec.MountOptions is empty.
var defaultMountOptions = []string{"nfsvers=4.1", "soft", "timeo=600", "retrans=3"}

// shouldSkipStorageClass returns true if the pool opts out of auto-SC creation.
func shouldSkipStorageClass(pool *v1alpha1.FileSharePool) bool {
	if pool.Annotations == nil {
		return false
	}
	return pool.Annotations[SkipStorageClassAnnotation] == "true"
}

// storageClassesForPool returns the desired StorageClass(es) for the pool.
// Non-tiered pools get one SC; tiered pools get one SC per tier.
func storageClassesForPool(pool *v1alpha1.FileSharePool) []*storagev1.StorageClass {
	if len(pool.Spec.Tiers) > 0 {
		var classes []*storagev1.StorageClass
		for _, tier := range pool.Spec.Tiers {
			classes = append(classes, buildStorageClass(pool, tier.Name))
		}
		return classes
	}
	return []*storagev1.StorageClass{buildStorageClass(pool, "")}
}

// buildStorageClass constructs a StorageClass from the pool spec.
func buildStorageClass(pool *v1alpha1.FileSharePool, tierName string) *storagev1.StorageClass {
	name := pool.Name
	if tierName != "" {
		name = fmt.Sprintf("%s-%s", pool.Name, tierName)
	}

	params := map[string]string{
		"pool": pool.Name,
	}
	if tierName != "" {
		params["tier"] = tierName
	}
	if pool.Spec.DefaultUID != nil {
		params["uid"] = fmt.Sprintf("%d", *pool.Spec.DefaultUID)
	}
	if pool.Spec.DefaultGID != nil {
		params["gid"] = fmt.Sprintf("%d", *pool.Spec.DefaultGID)
	}
	if pool.Spec.DefaultPermissions != "" {
		params["permissions"] = pool.Spec.DefaultPermissions
	}

	mountOptions := pool.Spec.MountOptions
	if len(mountOptions) == 0 {
		mountOptions = defaultMountOptions
	}

	bindingMode := storagev1.VolumeBindingImmediate
	if len(pool.Spec.AccessorZones) > 0 {
		bindingMode = storagev1.VolumeBindingWaitForFirstConsumer
	}

	reclaimPolicy := corev1.PersistentVolumeReclaimDelete
	allowExpansion := true

	return &storagev1.StorageClass{
		ObjectMeta: metav1.ObjectMeta{
			Name: name,
			Labels: map[string]string{
				managedByLabel: managedByValue,
				poolLabel:      pool.Name,
			},
			OwnerReferences: []metav1.OwnerReference{
				{
					APIVersion: pool.APIVersion,
					Kind:       pool.Kind,
					Name:       pool.Name,
					UID:        pool.UID,
				},
			},
		},
		Provisioner:          ProvisionerName,
		Parameters:           params,
		MountOptions:         mountOptions,
		VolumeBindingMode:    &bindingMode,
		ReclaimPolicy:        &reclaimPolicy,
		AllowVolumeExpansion: &allowExpansion,
	}
}
