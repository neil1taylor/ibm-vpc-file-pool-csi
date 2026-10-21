package replication

import (
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

const (
	// LabelReceiverCheck marks the Jobs the console's receiver setup makes.
	LabelReceiverCheck = "vpc-file-pool-csi.ibm.io/receiver-check"

	checkJobDeadlineSeconds = 60
	checkJobTTLSeconds      = 600
)

// CheckJobSpec holds the parameters for a receiver check Job.
type CheckJobSpec struct {
	// Name is the Job name.
	Name string
	// Namespace is the namespace of the Job and of the two Secrets (kube-system).
	Namespace string
	// Image is the driver image, which holds the check-receiver mode.
	Image string
	// Endpoint is the receiver's HTTPS URL.
	Endpoint string
	// AuthSecret is the Secret with the bearer token under the key "token".
	AuthSecret string
	// CASecret is the Secret with the CA certificate under the key "ca.crt". Empty means the system trust store.
	CASecret string
}

// BuildCheckJob makes the one-shot Job that runs `--mode=check-receiver` the way a replication Job
// reaches the receiver (same image, same Secret mounts). The result line is in the Pod's termination message.
// The console makes the same Job; a golden file ties the two builders together.
func BuildCheckJob(spec CheckJobSpec) *batchv1.Job {
	args := []string{
		"--mode=check-receiver",
		"--receiver-endpoint=" + spec.Endpoint,
		"--auth-token-file=/etc/replication/token",
	}
	mounts := []corev1.VolumeMount{{Name: "auth-token", MountPath: "/etc/replication", ReadOnly: true}}
	volumes := []corev1.Volume{{
		Name:         "auth-token",
		VolumeSource: corev1.VolumeSource{Secret: &corev1.SecretVolumeSource{SecretName: spec.AuthSecret}},
	}}
	if spec.CASecret != "" {
		args = append(args, "--ca-cert-file=/etc/replication-ca/ca.crt")
		mounts = append(mounts, corev1.VolumeMount{Name: "ca-cert", MountPath: "/etc/replication-ca", ReadOnly: true})
		volumes = append(volumes, corev1.Volume{
			Name:         "ca-cert",
			VolumeSource: corev1.VolumeSource{Secret: &corev1.SecretVolumeSource{SecretName: spec.CASecret}},
		})
	}

	backoffLimit := int32(0)
	deadline := int64(checkJobDeadlineSeconds)
	ttl := int32(checkJobTTLSeconds)
	runAsRoot := int64(0)
	labels := map[string]string{LabelReceiverCheck: "true"}

	return &batchv1.Job{
		TypeMeta:   metav1.TypeMeta{APIVersion: "batch/v1", Kind: "Job"},
		ObjectMeta: metav1.ObjectMeta{Name: spec.Name, Namespace: spec.Namespace, Labels: labels},
		Spec: batchv1.JobSpec{
			BackoffLimit:            &backoffLimit,
			ActiveDeadlineSeconds:   &deadline,
			TTLSecondsAfterFinished: &ttl,
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{Labels: labels},
				Spec: corev1.PodSpec{
					RestartPolicy:   corev1.RestartPolicyNever,
					SecurityContext: &corev1.PodSecurityContext{RunAsUser: &runAsRoot},
					Containers: []corev1.Container{{
						Name:         "check",
						Image:        spec.Image,
						Args:         args,
						VolumeMounts: mounts,
						Resources: corev1.ResourceRequirements{
							Requests: corev1.ResourceList{
								corev1.ResourceCPU:    resource.MustParse("50m"),
								corev1.ResourceMemory: resource.MustParse("64Mi"),
							},
							Limits: corev1.ResourceList{
								corev1.ResourceCPU:    resource.MustParse("500m"),
								corev1.ResourceMemory: resource.MustParse("128Mi"),
							},
						},
					}},
					Volumes: volumes,
				},
			},
		},
	}
}
