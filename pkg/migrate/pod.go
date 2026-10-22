package migrate

import (
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

const (
	// MigrationImage is the container image used for migration pods. It is the
	// OpenShift "tools" image: it already holds rsync and find, needs no pull
	// secret, and runs as any user id, so a user with only the edit role can
	// use it (issue #196).
	MigrationImage = "image-registry.openshift-image-registry.svc:5000/openshift/tools:latest"

	// LabelMigration is applied to all migration pods for easy discovery.
	LabelMigration = "vpc-file-pool-csi.ibm.io/migration"

	// AnnotationSourcePVC records the source PVC name on the migration pod.
	AnnotationSourcePVC = "vpc-file-pool-csi.ibm.io/source-pvc"

	// AnnotationTargetPVC records the target PVC name on the migration pod.
	AnnotationTargetPVC = "vpc-file-pool-csi.ibm.io/target-pvc"
)

// MigrationPodSpec holds the parameters for building a migration pod.
type MigrationPodSpec struct {
	// Name is the pod name.
	Name string
	// Namespace is the pod namespace.
	Namespace string
	// SourcePVC is the name of the old PVC to read from.
	SourcePVC string
	// TargetPVC is the name of the new PVC to write to.
	TargetPVC string
}

// BuildMigrationPod creates a Pod specification that mounts both the source
// and target PVCs and runs rsync to copy data from source to target. It fits
// the restricted rules of OpenShift: it sets no user id, so the cluster gives
// the user of the namespace, and it changes no owners.
func BuildMigrationPod(spec MigrationPodSpec) *corev1.Pod {
	noEscalation := false
	return &corev1.Pod{
		TypeMeta: metav1.TypeMeta{APIVersion: "v1", Kind: "Pod"},
		ObjectMeta: metav1.ObjectMeta{
			Name:      spec.Name,
			Namespace: spec.Namespace,
			Labels: map[string]string{
				LabelMigration: "true",
			},
			Annotations: map[string]string{
				AnnotationSourcePVC: spec.SourcePVC,
				AnnotationTargetPVC: spec.TargetPVC,
			},
		},
		Spec: corev1.PodSpec{
			RestartPolicy: corev1.RestartPolicyNever,
			SecurityContext: &corev1.PodSecurityContext{
				SeccompProfile: &corev1.SeccompProfile{Type: corev1.SeccompProfileTypeRuntimeDefault},
			},
			Containers: []corev1.Container{
				{
					Name:  "migrate",
					Image: MigrationImage,
					SecurityContext: &corev1.SecurityContext{
						AllowPrivilegeEscalation: &noEscalation,
						Capabilities:             &corev1.Capabilities{Drop: []corev1.Capability{"ALL"}},
					},
					// Run the copy, then report file counts for verification. A count mismatch fails the pod.
					// The copy runs as the user the cluster gives the pod, so it cannot set owners or modes:
					// the new files belong to that user, who is also the user of the namespace's other pods.
					// The top folder of the new claim belongs to the pool's user, so its times are left alone.
					Command: []string{"sh", "-c", `
set -e
echo "=== Migration started ==="
echo "Source: /source"
echo "Target: /target"
SOURCE_COUNT=$(find /source -type f | wc -l)
echo "Source file count: $SOURCE_COUNT"
rsync -rltSv --omit-dir-times --progress /source/ /target/
TARGET_COUNT=$(find /target -type f | wc -l)
echo "Target file count: $TARGET_COUNT"
echo "=== Migration complete ==="
if [ "$SOURCE_COUNT" = "$TARGET_COUNT" ]; then
  echo "VERIFICATION: OK (file counts match: $SOURCE_COUNT)"
  exit 0
else
  echo "VERIFICATION: FAILED (source=$SOURCE_COUNT target=$TARGET_COUNT)"
  exit 1
fi
`},
					VolumeMounts: []corev1.VolumeMount{
						{
							Name:      "source",
							MountPath: "/source",
							ReadOnly:  true,
						},
						{
							Name:      "target",
							MountPath: "/target",
						},
					},
					Resources: corev1.ResourceRequirements{
						Requests: corev1.ResourceList{
							corev1.ResourceCPU:    resource.MustParse("100m"),
							corev1.ResourceMemory: resource.MustParse("128Mi"),
						},
						Limits: corev1.ResourceList{
							corev1.ResourceCPU:    resource.MustParse("500m"),
							corev1.ResourceMemory: resource.MustParse("256Mi"),
						},
					},
				},
			},
			Volumes: []corev1.Volume{
				{
					Name: "source",
					VolumeSource: corev1.VolumeSource{
						PersistentVolumeClaim: &corev1.PersistentVolumeClaimVolumeSource{
							ClaimName: spec.SourcePVC,
							ReadOnly:  true,
						},
					},
				},
				{
					Name: "target",
					VolumeSource: corev1.VolumeSource{
						PersistentVolumeClaim: &corev1.PersistentVolumeClaimVolumeSource{
							ClaimName: spec.TargetPVC,
						},
					},
				},
			},
		},
	}
}

// TargetPVCSpec holds the parameters for building the new claim a volume moves into.
type TargetPVCSpec struct {
	// Name is the new claim's name.
	Name string
	// Namespace is the new claim's namespace (the same as the source claim's).
	Namespace string
	// SourcePVC is the name of the old claim, recorded on the new one.
	SourcePVC string
	// Pool is the pool the new claim lands in, recorded on the new claim.
	Pool string
	// StorageClass is the pool's storage class.
	StorageClass string
	// Size is the requested size (the size of the source claim).
	Size resource.Quantity
}

// BuildTargetPVC creates the claim on the pool driver that a migration pod copies into.
func BuildTargetPVC(spec TargetPVCSpec) *corev1.PersistentVolumeClaim {
	return &corev1.PersistentVolumeClaim{
		TypeMeta: metav1.TypeMeta{APIVersion: "v1", Kind: "PersistentVolumeClaim"},
		ObjectMeta: metav1.ObjectMeta{
			Name:      spec.Name,
			Namespace: spec.Namespace,
			Labels: map[string]string{
				LabelMigration: "true",
			},
			Annotations: map[string]string{
				AnnotationSourcePVC:             spec.SourcePVC,
				"vpc-file-pool-csi.ibm.io/pool": spec.Pool,
			},
		},
		Spec: corev1.PersistentVolumeClaimSpec{
			AccessModes:      []corev1.PersistentVolumeAccessMode{corev1.ReadWriteMany},
			StorageClassName: &spec.StorageClass,
			Resources: corev1.VolumeResourceRequirements{
				Requests: corev1.ResourceList{
					corev1.ResourceStorage: spec.Size,
				},
			},
		},
	}
}
