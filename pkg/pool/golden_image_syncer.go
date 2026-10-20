package pool

import (
	"context"
	"fmt"
	"strings"
	"time"

	v1alpha1 "github.com/IBM/ibm-vpc-file-pool-csi/api/v1alpha1"
	"github.com/IBM/ibm-vpc-file-pool-csi/pkg/k8s"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
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

	defaultPVCSizeGB = int64(15)

	goldenImageLabel = "pool.storage.ibmcloud.io/golden-image"
	goldenPoolLabel  = "pool.storage.ibmcloud.io/pool"
	goldenNameLabel  = "pool.storage.ibmcloud.io/image"
)

var (
	dataVolumeGVK = schema.GroupVersionKind{Group: "cdi.kubevirt.io", Version: "v1beta1", Kind: "DataVolume"}
	templateGVK   = schema.GroupVersionKind{Group: "template.openshift.io", Version: "v1", Kind: "Template"}
	dataSourceGVK = schema.GroupVersionKind{Group: "cdi.kubevirt.io", Version: "v1beta1", Kind: "DataSource"}
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
		if pool.Spec.GoldenImages == nil || !pool.Spec.GoldenImages.Enabled || pool.DeletionTimestamp != nil {
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

	pvcSizeGB := cfg.PVCSizeGB
	if pvcSizeGB <= 0 {
		pvcSizeGB = defaultPVCSizeGB
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
			nsStatus := s.syncImageInNamespace(ctx, pool.Name, scName, ns, img, pvcSizeGB)
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
// and returns the external registry address the ImageStream imports from.
// RHEL DataImportCrons use imageStream references instead of direct URLs.
// The node can pull the external address with the cluster pull secret.
// Returns empty string if not applicable.
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

	tags, _, _ := unstructured.NestedSlice(is.Object, "spec", "tags")
	for _, t := range tags {
		tag, ok := t.(map[string]interface{})
		if !ok || tag["name"] != "latest" {
			continue
		}
		kind, _, _ := unstructured.NestedString(tag, "from", "kind")
		from, _, _ := unstructured.NestedString(tag, "from", "name")
		if kind == "DockerImage" && from != "" {
			url := "docker://" + from
			klog.V(4).InfoS("Resolved ImageStream to its source registry",
				"dataimportcron", name, "imagestream", isName, "url", url)
			return url
		}
	}
	klog.V(4).InfoS("ImageStream has no external latest tag",
		"dataimportcron", name, "imagestream", isName)
	return ""
}

// syncImageInNamespace ensures the golden PVC, converter Job, and Template
// exist for a single image in a single namespace.
func (s *GoldenImageSyncer) syncImageInNamespace(
	ctx context.Context,
	poolName, scName, ns string,
	img goldenImageInfo,
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

	// 1. Ensure the golden DataVolume. CDI downloads the image and converts it
	// to a raw disk on the pool.
	phase, err := s.ensureGoldenDataVolume(ctx, ns, pvcName, img, poolName, scName, pvcSizeGB)
	if err != nil {
		status.Phase = "Failed"
		status.Message = fmt.Sprintf("DataVolume: %v", err)
		return status
	}
	switch phase {
	case "Succeeded":
	case "Failed":
		status.Phase = "Failed"
		status.Message = "CDI import failed; see the DataVolume's events"
		return status
	default:
		status.Phase = "Syncing"
		status.Message = "CDI import in progress"
		return status
	}

	// 3. Ensure CDI DataSource (for InstanceTypes catalog tab).
	// Non-fatal — DataSource is for UI catalog only; template cloning still works via PVC dataSource.
	dsName, dsErr := s.ensureDataSource(ctx, img.Name, poolName, pvcName, ns)
	if dsErr != nil {
		klog.ErrorS(dsErr, "Failed to create DataSource (non-fatal)", "image", img.Name, "pool", poolName)
	}
	status.DataSourceName = dsName

	// 4. Ensure OpenShift Template (for Template catalog tab).
	if err := s.ensureTemplate(ctx, ns, img.Name, templateName, poolName, scName, pvcName, pvcSizeGB); err != nil {
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

// ensureGoldenDataVolume creates the golden image DataVolume if it does not
// exist, and returns its phase. The DataVolume's claim has the same name.
func (s *GoldenImageSyncer) ensureGoldenDataVolume(
	ctx context.Context,
	ns, name string,
	img goldenImageInfo,
	poolName, scName string,
	sizeGB int64,
) (string, error) {
	dv := &unstructured.Unstructured{}
	dv.SetGroupVersionKind(dataVolumeGVK)
	err := s.directClient.Get(ctx, types.NamespacedName{Namespace: ns, Name: name}, dv)
	if err == nil {
		// Anyone who can write in the namespace could have made this
		// DataVolume. Publish it only if it is the one this pool would make.
		url, _, _ := unstructured.NestedString(dv.Object, "spec", "source", "registry", "url")
		sc, _, _ := unstructured.NestedString(dv.Object, "spec", "pvc", "storageClassName")
		labels := dv.GetLabels()
		if labels[goldenImageLabel] != "true" || labels[goldenPoolLabel] != poolName || url != img.RegistryURL || sc != scName {
			return "", fmt.Errorf("DataVolume %s/%s was not made by pool %s for %s; delete it to let the pool make it", ns, name, poolName, img.RegistryURL)
		}
		phase, _, _ := unstructured.NestedString(dv.Object, "status", "phase")
		return phase, nil
	}
	if !errors.IsNotFound(err) {
		return "", fmt.Errorf("getting DataVolume %s/%s: %w", ns, name, err)
	}

	dv = &unstructured.Unstructured{Object: map[string]interface{}{
		"apiVersion": "cdi.kubevirt.io/v1beta1",
		"kind":       "DataVolume",
		"metadata": map[string]interface{}{
			"name":      name,
			"namespace": ns,
			"labels": map[string]interface{}{
				goldenImageLabel: "true",
				goldenPoolLabel:  poolName,
				goldenNameLabel:  img.Name,
			},
		},
		"spec": map[string]interface{}{
			"source": map[string]interface{}{
				"registry": map[string]interface{}{
					"url":        img.RegistryURL,
					"pullMethod": "node",
				},
			},
			// spec.pvc, not spec.storage: the claim gets exactly sizeGB, so
			// the template's boot disk of the same size can be copied from it.
			"pvc": map[string]interface{}{
				"accessModes":      []interface{}{"ReadWriteMany"},
				"volumeMode":       "Filesystem",
				"storageClassName": scName,
				"resources": map[string]interface{}{
					"requests": map[string]interface{}{
						"storage": fmt.Sprintf("%dGi", sizeGB),
					},
				},
			},
		},
	}}
	if err := s.directClient.Create(ctx, dv); err != nil && !errors.IsAlreadyExists(err) {
		return "", fmt.Errorf("creating DataVolume %s/%s: %w", ns, name, err)
	}
	klog.V(2).InfoS("Created golden image DataVolume", "namespace", ns, "dataVolume", name,
		"pool", poolName, "url", img.RegistryURL)
	return "", nil
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
	sizeGB int64,
) error {
	return s.createTemplateInNamespace(ctx, ns, imageName, templateName, poolName, scName, goldenPVCName, sizeGB)
}

func (s *GoldenImageSyncer) createTemplateInNamespace(
	ctx context.Context,
	ns, imageName, templateName, poolName, scName, goldenPVCName string,
	sizeGB int64,
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

	tmpl := buildVMTemplate(imageName, templateName, ns, poolName, scName, goldenPVCName, sizeGB)
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
// The boot disk defaults to the golden image's size, because a copy cannot be
// smaller than its source.
func buildVMTemplate(imageName, templateName, ns, poolName, scName, goldenPVCName string, sizeGB int64) *unstructured.Unstructured {
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
					"template.kubevirt.io/type":             "base",
					"template.kubevirt.io/version":          "v1alpha1",
					"template.kubevirt.io/architecture":     "amd64",
					"os.template.kubevirt.io/" + osLabelKey: "true",
					"flavor.template.kubevirt.io/medium":    "true",
					"workload.template.kubevirt.io/server":  "true",
					goldenImageLabel:                        "true",
					goldenPoolLabel:                         poolName,
					goldenNameLabel:                         imageName,
				},
				"annotations": map[string]interface{}{
					"description":                                 fmt.Sprintf("%s VM with boot disk on IBM VPC NFS pool storage (%s). Clone from golden image for fast provisioning.", displayName, poolName),
					"openshift.io/display-name":                   fmt.Sprintf("%s Server (NFS Pool: %s)", displayName, poolName),
					"openshift.io/provider-display-name":          "IBM VPC File Pool CSI",
					"iconClass":                                   iconClass,
					"tags":                                        "kubevirt,virtualmachine,linux," + osLabelKey,
					"template.kubevirt.io/provider":               "IBM VPC File Pool CSI",
					"template.kubevirt.io/provider-url":           "https://github.com/IBM/ibm-vpc-file-pool-csi",
					"template.kubevirt.io/provider-support-level": "Community",
					"template.kubevirt.io/version":                "v1alpha1",
					"name.os.template.kubevirt.io/" + osLabelKey:  displayName,
					"defaults.template.kubevirt.io/disk":          "boot-disk",
					"template.openshift.io/bindable":              "false",
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
					"value":       fmt.Sprintf("%dGi", sizeGB),
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
							"app":                 "${NAME}",
							"vm.kubevirt.io/name": "${NAME}",
							goldenPoolLabel:       poolName,
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
										"threads": int64(1),
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
											"userData": "#cloud-config\npassword: ${CLOUD_USER_PASSWORD}\nchpasswd:\n  expire: false\nssh_pwauth: true\n",
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
	// One DataSource per golden claim, so every target namespace gets its own
	// catalog entry.
	cleanName := cleanImageName(imageName)
	dsName := fmt.Sprintf("%s-%s-nfs-pool", cleanName, targetNamespace)
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

// RemovePoolImages deletes the golden DataVolumes, claims and Templates made
// for pool in its target namespaces, and its catalog DataSources. It reports
// done=true once none are left, or the ones left are already being deleted.
func (s *GoldenImageSyncer) RemovePoolImages(ctx context.Context, pool *v1alpha1.FileSharePool) (bool, error) {
	poolName := pool.Name
	selector := client.MatchingLabels{goldenImageLabel: "true", goldenPoolLabel: poolName}
	newList := func(gvk schema.GroupVersionKind) client.ObjectList {
		list := &unstructured.UnstructuredList{}
		list.SetGroupVersionKind(gvk.GroupVersion().WithKind(gvk.Kind + "List"))
		return list
	}
	type search struct {
		list client.ObjectList
		ns   string
	}
	// Labels can be set by anyone who can write in a namespace, so only the
	// pool's target namespaces are searched. Only admins write in the shared
	// images namespace.
	searches := []search{{newList(dataSourceGVK), "openshift-virtualization-os-images"}}
	if pool.Spec.GoldenImages != nil {
		for _, ns := range pool.Spec.GoldenImages.TargetNamespaces {
			searches = append(searches,
				search{newList(dataVolumeGVK), ns},
				search{&corev1.PersistentVolumeClaimList{}, ns},
				search{newList(templateGVK), ns})
		}
	}

	done := true
	for _, sr := range searches {
		list := sr.list
		if err := s.directClient.List(ctx, list, selector, client.InNamespace(sr.ns)); err != nil {
			if isNoMatchError(err) {
				continue
			}
			return false, fmt.Errorf("listing golden image objects of pool %s: %w", poolName, err)
		}
		objs, err := meta.ExtractList(list)
		if err != nil {
			return false, err
		}
		for _, o := range objs {
			obj, ok := o.(client.Object)
			if !ok || obj.GetDeletionTimestamp() != nil {
				continue
			}
			done = false
			if err := s.directClient.Delete(ctx, obj); err != nil && !errors.IsNotFound(err) {
				return false, fmt.Errorf("deleting %s/%s of pool %s: %w", obj.GetNamespace(), obj.GetName(), poolName, err)
			}
			klog.V(2).InfoS("Deleted golden image object of deleted pool", "pool", poolName,
				"namespace", obj.GetNamespace(), "name", obj.GetName())
		}
	}
	return done, nil
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

func int64Ptr(v int64) *int64 { return &v }

// isNoMatchError returns true if the error indicates the API resource type is not registered.
func isNoMatchError(err error) bool {
	if err == nil {
		return false
	}
	return strings.Contains(err.Error(), "no match") ||
		strings.Contains(err.Error(), "the server could not find the requested resource")
}
