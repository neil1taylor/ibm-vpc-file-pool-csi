package pool

import (
	"context"
	"fmt"
	"strings"
	"time"

	v1alpha1 "github.com/IBM/ibm-vpc-file-pool-csi/api/v1alpha1"
	"github.com/IBM/ibm-vpc-file-pool-csi/pkg/k8s"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/klog/v2"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

const (
	// DefaultGoldenImageInterval is how often the syncer checks for golden images.
	DefaultGoldenImageInterval = 1 * time.Hour

	defaultConverterImage = "quay.io/centos/centos:stream9"
	defaultPVCSizeGB      = int64(15)

	goldenImageLabel = "pool.storage.ibmcloud.io/golden-image"
	goldenPoolLabel  = "pool.storage.ibmcloud.io/pool"
	goldenNameLabel  = "pool.storage.ibmcloud.io/image"
)

// goldenImageInfo holds discovered image metadata from CDI DataImportCrons.
type goldenImageInfo struct {
	Name        string
	RegistryURL string
}

// GoldenImageSyncer discovers CDI golden images and creates ready-to-clone
// PVCs and OpenShift Templates in target namespaces. It detects whether the
// pool StorageClass is the cluster default (native CDI mode) and skips
// syncing if so.
type GoldenImageSyncer struct {
	k8sClient    k8s.Client
	directClient client.Client
	interval     time.Duration
}

// NewGoldenImageSyncer creates a new golden image syncer.
func NewGoldenImageSyncer(k8sClient k8s.Client, directClient client.Client) *GoldenImageSyncer {
	return &GoldenImageSyncer{
		k8sClient:    k8sClient,
		directClient: directClient,
		interval:     DefaultGoldenImageInterval,
	}
}

// SetInterval overrides the default poll interval. Useful for tests.
func (s *GoldenImageSyncer) SetInterval(d time.Duration) {
	s.interval = d
}

// Run starts the golden image syncer loop. It blocks until the context is cancelled.
// It runs one cycle after a short startup delay, then every interval thereafter.
func (s *GoldenImageSyncer) Run(ctx context.Context) {
	klog.V(2).InfoS("Golden image syncer started", "interval", s.interval)

	// Wait briefly for the controller-runtime cache to be populated before the first cycle.
	select {
	case <-ctx.Done():
		return
	case <-time.After(30 * time.Second):
	}

	// Run immediately after startup delay so golden images are created as soon as the pool is ready,
	// rather than waiting for the full interval to elapse.
	s.processOnce(ctx)

	ticker := time.NewTicker(s.interval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			klog.V(2).InfoS("Golden image syncer stopped")
			return
		case <-ticker.C:
			s.processOnce(ctx)
		}
	}
}

// processOnce runs a single sync cycle across all pools.
func (s *GoldenImageSyncer) processOnce(ctx context.Context) {
	pools, err := s.k8sClient.ListFileSharePools(ctx)
	if err != nil {
		klog.ErrorS(err, "Golden image syncer failed to list pools")
		return
	}

	// Check if any pool-managed StorageClass is the cluster default.
	if s.isPoolSCDefault(ctx) {
		klog.V(4).InfoS("Native CDI golden image mode active — pool StorageClass is default, skipping syncer")
		return
	}

	for i := range pools {
		pool := &pools[i]
		if pool.Spec.GoldenImages == nil || !pool.Spec.GoldenImages.Enabled {
			continue
		}
		s.syncPool(ctx, pool)
	}
}

// isPoolSCDefault checks if any pool-managed StorageClass is the cluster default.
func (s *GoldenImageSyncer) isPoolSCDefault(ctx context.Context) bool {
	scList := &unstructured.UnstructuredList{}
	scList.SetGroupVersionKind(schema.GroupVersionKind{
		Group:   "storage.k8s.io",
		Version: "v1",
		Kind:    "StorageClassList",
	})
	if err := s.directClient.List(ctx, scList, client.MatchingLabels{
		managedByLabel: managedByValue,
	}); err != nil {
		klog.V(4).ErrorS(err, "Failed to list pool StorageClasses for default check")
		return false
	}

	for _, sc := range scList.Items {
		annotations := sc.GetAnnotations()
		if annotations["storageclass.kubernetes.io/is-default-class"] == "true" {
			return true
		}
	}
	return false
}

// syncPool processes golden images for a single pool.
func (s *GoldenImageSyncer) syncPool(ctx context.Context, pool *v1alpha1.FileSharePool) {
	cfg := pool.Spec.GoldenImages
	klog.V(2).InfoS("Syncing golden images for pool", "pool", pool.Name,
		"targetNamespaces", cfg.TargetNamespaces)

	images, err := s.discoverImages(ctx, cfg.ImageFilter)
	if err != nil {
		klog.ErrorS(err, "Failed to discover golden images", "pool", pool.Name)
		return
	}
	if len(images) == 0 {
		klog.V(4).InfoS("No golden images discovered", "pool", pool.Name)
		return
	}

	converterImage := cfg.ConverterImage
	if converterImage == "" {
		converterImage = defaultConverterImage
	}
	pvcSizeGB := cfg.PVCSizeGB
	if pvcSizeGB <= 0 {
		pvcSizeGB = defaultPVCSizeGB
	}

	// If any image uses the internal registry (ImageStream source), ensure the
	// image-puller RoleBinding exists so converter Jobs can pull images.
	hasInternalRegistry := false
	for _, img := range images {
		if isInternalRegistryURL(img.RegistryURL) {
			hasInternalRegistry = true
			break
		}
	}
	if hasInternalRegistry {
		for _, ns := range cfg.TargetNamespaces {
			if err := s.ensureImagePullerRoleBinding(ctx, ns); err != nil {
				klog.ErrorS(err, "Failed to ensure image-puller RoleBinding",
					"pool", pool.Name, "namespace", ns)
			}
		}
	}

	// Determine the StorageClass name for this pool.
	scName := pool.Name

	var statusList []v1alpha1.GoldenImageStatus
	for _, img := range images {
		imgStatus := v1alpha1.GoldenImageStatus{
			Name:      img.Name,
			SourceURL: img.RegistryURL,
		}

		for _, ns := range cfg.TargetNamespaces {
			nsStatus := s.syncImageInNamespace(ctx, pool.Name, scName, ns, img, converterImage, pvcSizeGB)
			imgStatus.Namespaces = append(imgStatus.Namespaces, nsStatus)
		}
		statusList = append(statusList, imgStatus)
	}

	// Re-read the pool to get the latest resourceVersion before updating status.
	// The reconciler may have updated the pool concurrently, causing a conflict
	// if we use the stale version we read at the start of the sync cycle.
	freshPool, err := s.k8sClient.GetFileSharePool(ctx, pool.Name)
	if err != nil {
		klog.ErrorS(err, "Failed to re-read pool for status update", "pool", pool.Name)
		return
	}
	freshPool.Status.GoldenImages = statusList
	if err := s.k8sClient.UpdateFileSharePoolStatus(ctx, freshPool); err != nil {
		klog.ErrorS(err, "Failed to update pool golden image status", "pool", pool.Name)
	}
}

// discoverImages lists CDI DataImportCrons and extracts registry URLs.
func (s *GoldenImageSyncer) discoverImages(ctx context.Context, imageFilter []string) ([]goldenImageInfo, error) {
	dicList := &unstructured.UnstructuredList{}
	dicList.SetGroupVersionKind(schema.GroupVersionKind{
		Group:   "cdi.kubevirt.io",
		Version: "v1beta1",
		Kind:    "DataImportCronList",
	})
	if err := s.directClient.List(ctx, dicList, client.InNamespace("openshift-virtualization-os-images")); err != nil {
		if errors.IsNotFound(err) || isNoMatchError(err) {
			klog.V(4).InfoS("CDI not installed, skipping golden image discovery")
			return nil, nil
		}
		return nil, fmt.Errorf("listing DataImportCrons: %w", err)
	}

	var images []goldenImageInfo
	for _, dic := range dicList.Items {
		name := dic.GetName()
		url, found, err := unstructured.NestedString(dic.Object,
			"spec", "template", "spec", "source", "registry", "url")
		if err != nil || !found || url == "" {
			// Check for ImageStream-based source (RHEL images use this instead of direct URLs)
			url = s.resolveImageStreamURL(ctx, dic, name)
			if url == "" {
				klog.V(6).InfoS("Skipping DataImportCron without registry URL or imageStream",
					"name", name)
				continue
			}
		}

		if len(imageFilter) > 0 && !matchesFilter(name, imageFilter) {
			continue
		}

		images = append(images, goldenImageInfo{
			Name:        name,
			RegistryURL: url,
		})
	}

	names := make([]string, len(images))
	for i, img := range images {
		names[i] = img.Name
	}
	klog.V(2).InfoS("Discovered golden images", "count", len(images), "images", names)
	return images, nil
}

// resolveImageStreamURL looks up an ImageStream reference from a DataImportCron
// and returns the internal registry URL. RHEL DataImportCrons use imageStream
// references instead of direct URLs. Returns empty string if not applicable.
func (s *GoldenImageSyncer) resolveImageStreamURL(ctx context.Context, dic unstructured.Unstructured, name string) string {
	isName, isFound, _ := unstructured.NestedString(dic.Object,
		"spec", "template", "spec", "source", "registry", "imageStream")
	if !isFound || isName == "" {
		return ""
	}

	is := &unstructured.Unstructured{}
	is.SetGroupVersionKind(schema.GroupVersionKind{
		Group: "image.openshift.io", Version: "v1", Kind: "ImageStream",
	})
	if err := s.directClient.Get(ctx, types.NamespacedName{
		Namespace: "openshift-virtualization-os-images", Name: isName,
	}, is); err != nil {
		klog.V(2).InfoS("Could not look up ImageStream for DataImportCron",
			"dataimportcron", name, "imagestream", isName, "error", err)
		return ""
	}

	repo, _, _ := unstructured.NestedString(is.Object, "status", "dockerImageRepository")
	if repo == "" {
		klog.V(4).InfoS("ImageStream has no dockerImageRepository",
			"dataimportcron", name, "imagestream", isName)
		return ""
	}

	url := "docker://" + repo + ":latest"
	klog.V(4).InfoS("Resolved ImageStream to internal registry URL",
		"dataimportcron", name, "imagestream", isName, "url", url)
	return url
}

// ensureImagePullerRoleBinding creates a RoleBinding in openshift-virtualization-os-images
// that grants system:image-puller to all service accounts in the target namespace.
// This allows converter Jobs to pull images from the internal registry.
func (s *GoldenImageSyncer) ensureImagePullerRoleBinding(ctx context.Context, targetNS string) error {
	rbName := fmt.Sprintf("pool-csi-image-puller-%s", targetNS)
	rbNamespace := "openshift-virtualization-os-images"

	rb := &unstructured.Unstructured{}
	rb.SetGroupVersionKind(schema.GroupVersionKind{
		Group: "rbac.authorization.k8s.io", Version: "v1", Kind: "RoleBinding",
	})

	// Check if it already exists.
	err := s.directClient.Get(ctx, types.NamespacedName{Namespace: rbNamespace, Name: rbName}, rb)
	if err == nil {
		return nil // already exists
	}
	if !errors.IsNotFound(err) {
		return fmt.Errorf("checking RoleBinding %s/%s: %w", rbNamespace, rbName, err)
	}

	newRB := &unstructured.Unstructured{
		Object: map[string]interface{}{
			"apiVersion": "rbac.authorization.k8s.io/v1",
			"kind":       "RoleBinding",
			"metadata": map[string]interface{}{
				"name":      rbName,
				"namespace": rbNamespace,
				"labels": map[string]interface{}{
					goldenImageLabel: "true",
				},
			},
			"roleRef": map[string]interface{}{
				"apiGroup": "rbac.authorization.k8s.io",
				"kind":     "ClusterRole",
				"name":     "system:image-puller",
			},
			"subjects": []interface{}{
				map[string]interface{}{
					"kind":      "Group",
					"apiGroup":  "rbac.authorization.k8s.io",
					"name":      fmt.Sprintf("system:serviceaccounts:%s", targetNS),
				},
			},
		},
	}

	if err := s.directClient.Create(ctx, newRB); err != nil {
		if errors.IsAlreadyExists(err) {
			return nil
		}
		return fmt.Errorf("creating RoleBinding %s/%s: %w", rbNamespace, rbName, err)
	}

	klog.V(2).InfoS("Created image-puller RoleBinding for converter Jobs",
		"rolebinding", rbName, "namespace", rbNamespace, "targetNamespace", targetNS)
	return nil
}

// syncImageInNamespace ensures the golden PVC, converter Job, and Template
// exist for a single image in a single namespace.
func (s *GoldenImageSyncer) syncImageInNamespace(
	ctx context.Context,
	poolName, scName, ns string,
	img goldenImageInfo,
	converterImage string,
	pvcSizeGB int64,
) v1alpha1.GoldenImageNamespaceStatus {
	cleanName := cleanImageName(img.Name)
	pvcName := fmt.Sprintf("golden-%s", img.Name)
	templateName := fmt.Sprintf("%s-nfs-pool", cleanName)

	status := v1alpha1.GoldenImageNamespaceStatus{
		Namespace:    ns,
		Phase:        "Pending",
		PVCName:      pvcName,
		TemplateName: templateName,
	}

	// 1. Ensure PVC exists.
	pvcReady, err := s.ensureGoldenPVC(ctx, ns, pvcName, img.Name, poolName, scName, pvcSizeGB)
	if err != nil {
		status.Phase = "Failed"
		status.Message = fmt.Sprintf("PVC: %v", err)
		return status
	}

	// 2. Ensure converter Job (if PVC is not yet populated).
	if !pvcReady {
		jobDone, err := s.ensureConverterJob(ctx, ns, img, poolName, pvcName, converterImage, pvcSizeGB)
		if err != nil {
			status.Phase = "Failed"
			status.Message = fmt.Sprintf("Job: %v", err)
			return status
		}
		if !jobDone {
			status.Phase = "Syncing"
			status.Message = "Converter job running"
			return status
		}
	}

	// 3. Ensure CDI DataSource (for InstanceTypes catalog tab).
	// Non-fatal — DataSource is for UI catalog only; template cloning still works via PVC dataSource.
	dsName, dsErr := s.ensureDataSource(ctx, img.Name, poolName, pvcName, ns)
	if dsErr != nil {
		klog.ErrorS(dsErr, "Failed to create DataSource (non-fatal)", "image", img.Name, "pool", poolName)
	}
	status.DataSourceName = dsName

	// 4. Ensure OpenShift Template (for Template catalog tab).
	if err := s.ensureTemplate(ctx, ns, img.Name, templateName, poolName, scName, pvcName); err != nil {
		status.Phase = "Failed"
		status.Message = fmt.Sprintf("Template: %v", err)
		return status
	}

	now := metav1.Now()
	status.Phase = "Ready"
	status.LastSyncTime = &now
	status.Message = "Golden image ready"
	return status
}

// ensureGoldenPVC creates the golden image PVC if it doesn't exist.
// Returns true if the PVC already has the "ready" annotation (disk.img populated).
func (s *GoldenImageSyncer) ensureGoldenPVC(
	ctx context.Context,
	ns, pvcName, imageName, poolName, scName string,
	sizeGB int64,
) (bool, error) {
	existing := &corev1.PersistentVolumeClaim{}
	err := s.directClient.Get(ctx, types.NamespacedName{Namespace: ns, Name: pvcName}, existing)
	if err == nil {
		// PVC exists — check if marked ready.
		return existing.Annotations["pool.storage.ibmcloud.io/golden-ready"] == "true", nil
	}
	if !errors.IsNotFound(err) {
		return false, fmt.Errorf("getting PVC %s/%s: %w", ns, pvcName, err)
	}

	// Create the PVC.
	accessMode := corev1.ReadWriteMany
	pvc := &corev1.PersistentVolumeClaim{
		ObjectMeta: metav1.ObjectMeta{
			Name:      pvcName,
			Namespace: ns,
			Labels: map[string]string{
				goldenImageLabel: "true",
				goldenPoolLabel:  poolName,
			},
		},
		Spec: corev1.PersistentVolumeClaimSpec{
			AccessModes:      []corev1.PersistentVolumeAccessMode{accessMode},
			StorageClassName: &scName,
			Resources: corev1.VolumeResourceRequirements{
				Requests: corev1.ResourceList{
					corev1.ResourceStorage: resource.MustParse(fmt.Sprintf("%dGi", sizeGB)),
				},
			},
		},
	}
	if err := s.directClient.Create(ctx, pvc); err != nil {
		if errors.IsAlreadyExists(err) {
			return false, nil
		}
		return false, fmt.Errorf("creating PVC %s/%s: %w", ns, pvcName, err)
	}

	klog.V(2).InfoS("Created golden image PVC", "namespace", ns, "pvc", pvcName, "pool", poolName)
	return false, nil
}

// ensureConverterJob creates or checks the converter Job for a golden image.
// Returns true if the Job has completed successfully.
func (s *GoldenImageSyncer) ensureConverterJob(
	ctx context.Context,
	ns string,
	img goldenImageInfo,
	poolName, pvcName, converterImage string,
	pvcSizeGB int64,
) (bool, error) {
	jobName := fmt.Sprintf("golden-convert-%s", img.Name)

	// Check if Job already exists.
	existing := &batchv1.Job{}
	err := s.directClient.Get(ctx, types.NamespacedName{Namespace: ns, Name: jobName}, existing)
	if err == nil {
		// Job exists — check status.
		if existing.Status.Succeeded > 0 {
			// Mark PVC as ready.
			s.markPVCReady(ctx, ns, pvcName)
			return true, nil
		}
		if existing.Status.Failed > 0 && existing.Status.Active == 0 {
			// Delete the failed job so the next sync cycle recreates it.
			klog.V(2).InfoS("Deleting failed converter job for retry",
				"namespace", ns, "job", jobName)
			propagation := metav1.DeletePropagationBackground
			if delErr := s.directClient.Delete(ctx, existing, &client.DeleteOptions{
				PropagationPolicy: &propagation,
			}); delErr != nil && !errors.IsNotFound(delErr) {
				klog.ErrorS(delErr, "Failed to delete failed converter job",
					"namespace", ns, "job", jobName)
			}
			return false, fmt.Errorf("converter job %s/%s failed (deleted for retry)", ns, jobName)
		}
		return false, nil // still running
	}
	if !errors.IsNotFound(err) {
		return false, fmt.Errorf("getting job %s/%s: %w", ns, jobName, err)
	}

	// Create the converter Job.
	backoffLimit := int32(3)
	ttl := int32(3600)
	stagingSizeGB := pvcSizeGB
	if stagingSizeGB < 12 {
		stagingSizeGB = 12
	}

	script := fmt.Sprintf(`set -euo pipefail
echo "Installing tools..."
dnf install -y skopeo qemu-img jq file tar

REGISTRY_URL="%s"
echo "Downloading OCI image from $REGISTRY_URL..."
mkdir -p /staging/oci

# If pulling from the internal OpenShift registry, configure auth using the SA token.
SKOPEO_AUTH=""
if echo "$REGISTRY_URL" | grep -q "image-registry.openshift-image-registry.svc"; then
  echo "Detected internal registry, configuring auth..."
  TOKEN=$(cat /var/run/secrets/kubernetes.io/serviceaccount/token)
  AUTH_B64=$(printf 'serviceaccount:%%s' "$TOKEN" | base64 -w0)
  REGISTRY_HOST=$(echo "$REGISTRY_URL" | sed 's|docker://||' | cut -d/ -f1)
  printf '{"auths":{"%%s":{"auth":"%%s"}}}' "$REGISTRY_HOST" "$AUTH_B64" > /tmp/auth.json
  SKOPEO_AUTH="--authfile=/tmp/auth.json --src-tls-verify=false"
fi

skopeo copy $SKOPEO_AUTH $REGISTRY_URL dir:/staging/oci

echo "Extracting disk layer..."
MANIFEST=/staging/oci/manifest.json
LAYER=$(jq -r '.layers[-1].digest' "$MANIFEST" | cut -d: -f2)
LAYER_FILE="/staging/oci/$LAYER"

# OCI layers are gzip-compressed tar archives containing the container filesystem.
# For containerdisks, the disk image is at disk/disk.img inside the tar.
# Decompress directly to NFS PVC (/data) to avoid OOM on emptyDir (tmpfs).
echo "Processing OCI layer..."
LAYER_EXTRACTED="/data/disk-layer.raw"
if file "$LAYER_FILE" | grep -q gzip; then
  echo "Decompressing gzip layer directly to PVC..."
  gunzip -c "$LAYER_FILE" > "$LAYER_EXTRACTED"
  rm -f "$LAYER_FILE"
else
  echo "Layer is not compressed, moving..."
  mv "$LAYER_FILE" "$LAYER_EXTRACTED"
fi

# OCI layers are tar archives. Extract the disk image from inside.
DISK_RAW="/data/disk-extracted.raw"
if file "$LAYER_EXTRACTED" | grep -q "tar archive"; then
  echo "OCI layer is a tar archive, extracting disk image..."
  # containerdisks store the image at disk/disk.img inside the tar
  tar xf "$LAYER_EXTRACTED" -C /data/ --strip-components=0 2>/dev/null || true
  if [ -f /data/disk/disk.img ]; then
    mv /data/disk/disk.img "$DISK_RAW"
    rm -rf /data/disk
  else
    # Fallback: find the largest file in the tar
    echo "disk/disk.img not found in tar, searching for disk image..."
    LARGEST=$(tar tf "$LAYER_EXTRACTED" | while read f; do
      tar xf "$LAYER_EXTRACTED" -O "$f" 2>/dev/null | wc -c | tr -d ' '
    done | sort -rn | head -1)
    # Just extract everything and find the largest regular file
    mkdir -p /data/tar-tmp
    tar xf "$LAYER_EXTRACTED" -C /data/tar-tmp 2>/dev/null || true
    DISK_FILE=$(find /data/tar-tmp -type f -name "*.img" -o -name "*.raw" -o -name "*.qcow2" | head -1)
    if [ -z "$DISK_FILE" ]; then
      DISK_FILE=$(find /data/tar-tmp -type f -printf '%%s %%p\n' 2>/dev/null | sort -rn | head -1 | awk '{print $2}')
    fi
    if [ -n "$DISK_FILE" ]; then
      mv "$DISK_FILE" "$DISK_RAW"
    else
      echo "ERROR: Could not find disk image in tar archive"
      tar tf "$LAYER_EXTRACTED" | head -20
      exit 1
    fi
    rm -rf /data/tar-tmp
  fi
  rm -f "$LAYER_EXTRACTED"
else
  echo "Layer is not a tar archive, treating as disk image directly..."
  mv "$LAYER_EXTRACTED" "$DISK_RAW"
fi

echo "Detecting image format..."
FORMAT=$(qemu-img info --output=json "$DISK_RAW" | jq -r '.format')
echo "Detected format: $FORMAT"

if [ "$FORMAT" = "qcow2" ]; then
  echo "Converting qcow2 to raw..."
  qemu-img convert -f qcow2 -O raw "$DISK_RAW" /data/disk.img
  rm -f "$DISK_RAW"
elif [ "$FORMAT" = "raw" ]; then
  echo "Image is already raw..."
  mv "$DISK_RAW" /data/disk.img
else
  echo "Unknown format $FORMAT, attempting qcow2 conversion..."
  qemu-img convert -f qcow2 -O raw "$DISK_RAW" /data/disk.img
  rm -f "$DISK_RAW"
fi
chmod 0666 /data/disk.img

# Ensure the MBR has the bootable/active flag set on partition 1.
# Cloud images with GPT often have a "protective MBR" where no partition
# is marked active (boot indicator = 0x00). SeaBIOS requires at least one
# active partition to boot. Setting byte at offset 446 (first partition
# entry's status byte) to 0x80 makes the disk bootable by both BIOS and
# UEFI firmware. This is safe for MBR disks too.
echo "Setting MBR active/bootable flag..."
printf '\x80' | dd of=/data/disk.img bs=1 seek=446 count=1 conv=notrunc 2>&1

echo "Cleaning up staging..."
rm -rf /staging/oci

echo "Golden image ready."
ls -la /data/disk.img
file /data/disk.img
`, img.RegistryURL)

	job := &batchv1.Job{
		ObjectMeta: metav1.ObjectMeta{
			Name:      jobName,
			Namespace: ns,
			Labels: map[string]string{
				goldenImageLabel: "true",
				goldenPoolLabel:  poolName,
				goldenNameLabel:  img.Name,
			},
		},
		Spec: batchv1.JobSpec{
			BackoffLimit:            &backoffLimit,
			TTLSecondsAfterFinished: &ttl,
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{
					Labels: map[string]string{
						goldenImageLabel: "true",
						goldenPoolLabel:  poolName,
						goldenNameLabel:  img.Name,
					},
				},
				Spec: corev1.PodSpec{
					RestartPolicy: corev1.RestartPolicyOnFailure,
					SecurityContext: &corev1.PodSecurityContext{
						RunAsUser: int64Ptr(0),
					},
					Containers: []corev1.Container{
						{
							Name:    "converter",
							Image:   converterImage,
							Command: []string{"/bin/bash", "-c", script},
							Resources: corev1.ResourceRequirements{
								Requests: corev1.ResourceList{
									corev1.ResourceCPU:    resource.MustParse("500m"),
									corev1.ResourceMemory: resource.MustParse("512Mi"),
								},
								Limits: corev1.ResourceList{
									corev1.ResourceCPU:    resource.MustParse("2"),
									corev1.ResourceMemory: resource.MustParse("2Gi"),
								},
							},
							VolumeMounts: []corev1.VolumeMount{
								{Name: "data", MountPath: "/data"},
								{Name: "staging", MountPath: "/staging"},
							},
						},
					},
					Volumes: []corev1.Volume{
						{
							Name: "data",
							VolumeSource: corev1.VolumeSource{
								PersistentVolumeClaim: &corev1.PersistentVolumeClaimVolumeSource{
									ClaimName: pvcName,
								},
							},
						},
						{
							Name: "staging",
							VolumeSource: corev1.VolumeSource{
								EmptyDir: &corev1.EmptyDirVolumeSource{
									SizeLimit: resource.NewQuantity(stagingSizeGB*(1<<30), resource.BinarySI),
								},
							},
						},
					},
				},
			},
		},
	}

	if err := s.directClient.Create(ctx, job); err != nil {
		if errors.IsAlreadyExists(err) {
			return false, nil
		}
		return false, fmt.Errorf("creating converter job %s/%s: %w", ns, jobName, err)
	}

	klog.V(2).InfoS("Created golden image converter job",
		"namespace", ns, "job", jobName, "image", img.Name, "registryURL", img.RegistryURL)
	return false, nil
}

// markPVCReady annotates the golden PVC to indicate disk.img is populated.
func (s *GoldenImageSyncer) markPVCReady(ctx context.Context, ns, pvcName string) {
	pvc := &corev1.PersistentVolumeClaim{}
	if err := s.directClient.Get(ctx, types.NamespacedName{Namespace: ns, Name: pvcName}, pvc); err != nil {
		klog.ErrorS(err, "Failed to get PVC for ready annotation", "namespace", ns, "pvc", pvcName)
		return
	}
	if pvc.Annotations == nil {
		pvc.Annotations = make(map[string]string)
	}
	if pvc.Annotations["pool.storage.ibmcloud.io/golden-ready"] == "true" {
		return
	}
	pvc.Annotations["pool.storage.ibmcloud.io/golden-ready"] = "true"
	if err := s.directClient.Update(ctx, pvc); err != nil {
		klog.ErrorS(err, "Failed to mark PVC ready", "namespace", ns, "pvc", pvcName)
	}
}

// ensureTemplate creates an OpenShift Template for VM creation from a golden image.
// Templates are created in the target namespace only. The "openshift" namespace is
// avoided because the SSP operator manages common templates there and marks
// unrecognized templates as deprecated, hiding them from the catalog.
// Users should select the target namespace/project in the console to see templates
// in the Virtualization Catalog.
func (s *GoldenImageSyncer) ensureTemplate(
	ctx context.Context,
	ns, imageName, templateName, poolName, scName, goldenPVCName string,
) error {
	return s.createTemplateInNamespace(ctx, ns, imageName, templateName, poolName, scName, goldenPVCName)
}

func (s *GoldenImageSyncer) createTemplateInNamespace(
	ctx context.Context,
	ns, imageName, templateName, poolName, scName, goldenPVCName string,
) error {
	template := &unstructured.Unstructured{}
	template.SetGroupVersionKind(schema.GroupVersionKind{
		Group:   "template.openshift.io",
		Version: "v1",
		Kind:    "Template",
	})

	err := s.directClient.Get(ctx, types.NamespacedName{Namespace: ns, Name: templateName}, template)
	if err == nil {
		return nil // Template already exists
	}
	if !errors.IsNotFound(err) && !isNoMatchError(err) {
		return fmt.Errorf("getting template %s/%s: %w", ns, templateName, err)
	}

	tmpl := buildVMTemplate(imageName, templateName, ns, poolName, scName, goldenPVCName)
	if err := s.directClient.Create(ctx, tmpl); err != nil {
		if errors.IsAlreadyExists(err) {
			return nil
		}
		if isNoMatchError(err) {
			klog.V(4).InfoS("Template API not available, skipping template creation",
				"namespace", ns, "template", templateName)
			return nil
		}
		return fmt.Errorf("creating template %s/%s: %w", ns, templateName, err)
	}

	klog.V(2).InfoS("Created VM template", "namespace", ns, "template", templateName, "image", imageName)
	return nil
}

// osIconClass returns the OpenShift console icon class for a given image name.
func osIconClass(imageName string) string {
	switch {
	case strings.Contains(imageName, "centos"):
		return "icon-centos"
	case strings.Contains(imageName, "fedora"):
		return "icon-fedora"
	case strings.Contains(imageName, "rhel"):
		return "icon-rhel"
	case strings.Contains(imageName, "ubuntu"):
		return "icon-ubuntu"
	case strings.Contains(imageName, "windows"):
		return "icon-windows"
	default:
		return "icon-other-linux"
	}
}

// cleanImageName strips common CDI suffixes from a DataImportCron name
// to produce a clean OS identifier (e.g. "centos-stream9-image-cron" → "centos-stream9").
func cleanImageName(imageName string) string {
	name := strings.TrimSuffix(imageName, "-image-cron")
	name = strings.TrimSuffix(name, "-cron")
	return name
}

// osDisplayName returns a human-readable OS name from the image cron name.
func osDisplayName(imageName string) string {
	name := cleanImageName(imageName)

	// Known OS brand capitalizations.
	replacer := strings.NewReplacer(
		"centos", "CentOS",
		"rhel", "RHEL",
	)
	name = replacer.Replace(name)

	// Title-case the dash-separated parts (preserving already-cased brands).
	parts := strings.Split(name, "-")
	for i, p := range parts {
		if len(p) > 0 && p == strings.ToLower(p) {
			parts[i] = strings.ToUpper(p[:1]) + p[1:]
		}
	}
	return strings.Join(parts, " ")
}

// buildVMTemplate constructs an OpenShift Template for a golden image VM.
func buildVMTemplate(imageName, templateName, ns, poolName, scName, goldenPVCName string) *unstructured.Unstructured {
	osLabelKey := cleanImageName(imageName)
	displayName := osDisplayName(imageName)
	iconClass := osIconClass(imageName)

	tmpl := &unstructured.Unstructured{
		Object: map[string]interface{}{
			"apiVersion": "template.openshift.io/v1",
			"kind":       "Template",
			"metadata": map[string]interface{}{
				"name":      templateName,
				"namespace": ns,
				"labels": map[string]interface{}{
					"template.kubevirt.io/type":                         "base",
					"template.kubevirt.io/version":                      "v1alpha1",
					"template.kubevirt.io/architecture":                 "amd64",
					"os.template.kubevirt.io/" + osLabelKey:             "true",
					"flavor.template.kubevirt.io/medium":                "true",
					"workload.template.kubevirt.io/server":              "true",
					goldenImageLabel:                                    "true",
					goldenPoolLabel:                                     poolName,
					goldenNameLabel:                                     imageName,
				},
				"annotations": map[string]interface{}{
					"description":                                        fmt.Sprintf("%s VM with boot disk on IBM VPC NFS pool storage (%s). Clone from golden image for fast provisioning.", displayName, poolName),
					"openshift.io/display-name":                          fmt.Sprintf("%s Server (NFS Pool: %s)", displayName, poolName),
					"openshift.io/provider-display-name":                 "IBM VPC File Pool CSI",
					"iconClass":                                          iconClass,
					"tags":                                               "kubevirt,virtualmachine,linux," + osLabelKey,
					"template.kubevirt.io/provider":                      "IBM VPC File Pool CSI",
					"template.kubevirt.io/provider-url":                  "https://github.com/IBM/ibm-vpc-file-pool-csi",
					"template.kubevirt.io/provider-support-level":        "Community",
					"template.kubevirt.io/version":                       "v1alpha1",
					"name.os.template.kubevirt.io/" + osLabelKey:         displayName,
					"defaults.template.kubevirt.io/disk":                 "boot-disk",
					"template.openshift.io/bindable":                     "false",
					"openshift.kubevirt.io/pronounceable-suffix-for-name-expression": "true",
				},
			},
			"parameters": []interface{}{
				map[string]interface{}{
					"name":        "NAME",
					"description": "VM name",
					"generate":    "expression",
					"from":        fmt.Sprintf("%s-[a-z0-9]{6}", osLabelKey),
				},
				map[string]interface{}{
					"name":        "BOOT_DISK_SIZE",
					"description": "Boot disk size",
					"value":       "15Gi",
				},
				map[string]interface{}{
					"name":        "DATA_DISK_SIZE",
					"description": "Data disk size",
					"value":       "10Gi",
				},
				map[string]interface{}{
					"name":        "CLOUD_USER_PASSWORD",
					"description": "cloud-user password for console access",
					"generate":    "expression",
					"from":        "[a-zA-Z0-9]{16}",
				},
			},
			"objects": []interface{}{
				// Boot PVC (clone from golden image)
				map[string]interface{}{
					"apiVersion": "v1",
					"kind":       "PersistentVolumeClaim",
					"metadata": map[string]interface{}{
						"name": "${NAME}-boot",
					},
					"spec": map[string]interface{}{
						"accessModes":      []interface{}{"ReadWriteMany"},
						"storageClassName": scName,
						"resources": map[string]interface{}{
							"requests": map[string]interface{}{
								"storage": "${BOOT_DISK_SIZE}",
							},
						},
						"dataSource": map[string]interface{}{
							"kind": "PersistentVolumeClaim",
							"name": goldenPVCName,
						},
					},
				},
				// Data PVC
				map[string]interface{}{
					"apiVersion": "v1",
					"kind":       "PersistentVolumeClaim",
					"metadata": map[string]interface{}{
						"name": "${NAME}-data",
					},
					"spec": map[string]interface{}{
						"accessModes":      []interface{}{"ReadWriteMany"},
						"storageClassName": scName,
						"resources": map[string]interface{}{
							"requests": map[string]interface{}{
								"storage": "${DATA_DISK_SIZE}",
							},
						},
					},
				},
				// VirtualMachine
				map[string]interface{}{
					"apiVersion": "kubevirt.io/v1",
					"kind":       "VirtualMachine",
					"metadata": map[string]interface{}{
						"name": "${NAME}",
						"labels": map[string]interface{}{
							"app":                      "${NAME}",
							"vm.kubevirt.io/name":      "${NAME}",
							goldenPoolLabel:             poolName,
						},
					},
					"spec": map[string]interface{}{
						"running": true,
						"template": map[string]interface{}{
							"metadata": map[string]interface{}{
								"labels": map[string]interface{}{
									"kubevirt.io/domain": "${NAME}",
								},
							},
							"spec": map[string]interface{}{
								"domain": map[string]interface{}{
									"cpu": map[string]interface{}{
										"cores":   int64(1),
										"sockets": int64(1),
										"threads":  int64(1),
									},
									"resources": map[string]interface{}{
										"requests": map[string]interface{}{
											"memory": "2Gi",
										},
									},
									"devices": map[string]interface{}{
										"disks": []interface{}{
											map[string]interface{}{
												"name": "boot-disk",
												"disk": map[string]interface{}{
													"bus": "virtio",
												},
											},
											map[string]interface{}{
												"name": "data-disk",
												"disk": map[string]interface{}{
													"bus": "virtio",
												},
											},
											map[string]interface{}{
												"name": "cloudinit",
												"disk": map[string]interface{}{
													"bus": "virtio",
												},
											},
										},
									},
								},
								"volumes": []interface{}{
									map[string]interface{}{
										"name": "boot-disk",
										"persistentVolumeClaim": map[string]interface{}{
											"claimName": "${NAME}-boot",
										},
									},
									map[string]interface{}{
										"name": "data-disk",
										"persistentVolumeClaim": map[string]interface{}{
											"claimName": "${NAME}-data",
										},
									},
									map[string]interface{}{
										"name": "cloudinit",
										"cloudInitNoCloud": map[string]interface{}{
											"userData": fmt.Sprintf("#cloud-config\npassword: ${CLOUD_USER_PASSWORD}\nchpasswd:\n  expire: false\nssh_pwauth: true\n"),
										},
									},
								},
							},
						},
					},
				},
			},
		},
	}
	return tmpl
}

// osPreference maps a clean image name to a VirtualMachineClusterPreference name.
// CDI DataSources use dot-separated names (e.g., "centos.stream9") as preference
// identifiers for the InstanceTypes catalog.
func osPreference(cleanName string) string {
	return strings.ReplaceAll(cleanName, "-", ".")
}

// ensureDataSource creates a CDI DataSource in openshift-virtualization-os-images
// so that pool-backed golden images appear in the InstanceTypes catalog tab.
// The DataSource points cross-namespace to the golden PVC in the target namespace.
func (s *GoldenImageSyncer) ensureDataSource(
	ctx context.Context,
	imageName, poolName, goldenPVCName, targetNamespace string,
) (string, error) {
	cleanName := cleanImageName(imageName)
	dsName := fmt.Sprintf("%s-nfs-pool", cleanName)
	dsNamespace := "openshift-virtualization-os-images"

	ds := &unstructured.Unstructured{}
	ds.SetGroupVersionKind(schema.GroupVersionKind{
		Group:   "cdi.kubevirt.io",
		Version: "v1beta1",
		Kind:    "DataSource",
	})

	err := s.directClient.Get(ctx, types.NamespacedName{Namespace: dsNamespace, Name: dsName}, ds)
	if err == nil {
		return dsName, nil // already exists
	}
	if !errors.IsNotFound(err) && !isNoMatchError(err) {
		return dsName, fmt.Errorf("getting DataSource %s/%s: %w", dsNamespace, dsName, err)
	}

	preference := osPreference(cleanName)

	newDS := &unstructured.Unstructured{
		Object: map[string]interface{}{
			"apiVersion": "cdi.kubevirt.io/v1beta1",
			"kind":       "DataSource",
			"metadata": map[string]interface{}{
				"name":      dsName,
				"namespace": dsNamespace,
				"labels": map[string]interface{}{
					"instancetype.kubevirt.io/default-instancetype": "u1.medium",
					"instancetype.kubevirt.io/default-preference":   preference,
					"app.kubernetes.io/part-of":                     "hyperconverged-cluster",
					goldenImageLabel:                                "true",
					goldenPoolLabel:                                 poolName,
					goldenNameLabel:                                 imageName,
				},
			},
			"spec": map[string]interface{}{
				"source": map[string]interface{}{
					"pvc": map[string]interface{}{
						"name":      goldenPVCName,
						"namespace": targetNamespace,
					},
				},
			},
		},
	}

	if err := s.directClient.Create(ctx, newDS); err != nil {
		if errors.IsAlreadyExists(err) {
			return dsName, nil
		}
		if isNoMatchError(err) {
			klog.V(4).InfoS("DataSource API not available, skipping DataSource creation",
				"datasource", dsName)
			return dsName, nil
		}
		return dsName, fmt.Errorf("creating DataSource %s/%s: %w", dsNamespace, dsName, err)
	}

	klog.V(2).InfoS("Created CDI DataSource for InstanceTypes catalog",
		"datasource", dsName, "namespace", dsNamespace, "pool", poolName,
		"pvc", goldenPVCName, "pvcNamespace", targetNamespace)
	return dsName, nil
}

// matchesFilter checks if a name matches any of the filter substrings.
func matchesFilter(name string, filters []string) bool {
	for _, f := range filters {
		if strings.Contains(name, f) {
			return true
		}
	}
	return false
}

// isInternalRegistryURL returns true if the URL points to the OpenShift internal registry.
func isInternalRegistryURL(url string) bool {
	return strings.Contains(url, "image-registry.openshift-image-registry.svc")
}

func int64Ptr(v int64) *int64 { return &v }

// isNoMatchError returns true if the error indicates the API resource type is not registered.
func isNoMatchError(err error) bool {
	if err == nil {
		return false
	}
	return strings.Contains(err.Error(), "no match") ||
		strings.Contains(err.Error(), "the server could not find the requested resource")
}
