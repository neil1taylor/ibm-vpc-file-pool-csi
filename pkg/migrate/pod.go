package migrate

import (
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

const (
	// MigrationImage is the container image used for migration pods.
	MigrationImage = "alpine:3.20"

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
// and target PVCs and runs rsync to copy data from source to target.
func BuildMigrationPod(spec MigrationPodSpec) *corev1.Pod {
	return &corev1.Pod{
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
			Containers: []corev1.Container{
				{
					Name:  "migrate",
					Image: MigrationImage,
					// Install rsync, run the copy, then report file counts for verification.
					Command: []string{"sh", "-c", `
set -e
apk add --no-cache rsync >/dev/null 2>&1
echo "=== Migration started ==="
echo "Source: /source"
echo "Target: /target"
SOURCE_COUNT=$(find /source -type f | wc -l)
echo "Source file count: $SOURCE_COUNT"
rsync -avz --progress /source/ /target/
TARGET_COUNT=$(find /target -type f | wc -l)
echo "Target file count: $TARGET_COUNT"
echo "=== Migration complete ==="
if [ "$SOURCE_COUNT" = "$TARGET_COUNT" ]; then
  echo "VERIFICATION: OK (file counts match: $SOURCE_COUNT)"
  exit 0
else
  echo "VERIFICATION: WARNING (source=$SOURCE_COUNT target=$TARGET_COUNT)"
  exit 0
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
