package monitoring

import (
	"context"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/klog/v2"
	"sigs.k8s.io/yaml"
)

// UserWorkloadMonitoringEnabled reports whether OpenShift's user workload
// monitoring is on, as set in openshift-monitoring/cluster-monitoring-config.
// Anything it cannot read counts as off.
func UserWorkloadMonitoringEnabled(ctx context.Context, cs kubernetes.Interface) bool {
	cm, err := cs.CoreV1().ConfigMaps("openshift-monitoring").Get(ctx, "cluster-monitoring-config", metav1.GetOptions{})
	if err != nil {
		return false
	}
	var cfg struct {
		EnableUserWorkload bool `json:"enableUserWorkload"`
	}
	if err := yaml.Unmarshal([]byte(cm.Data["config.yaml"]), &cfg); err != nil {
		return false
	}
	return cfg.EnableUserWorkload
}

// WarnIfAlertsNotEvaluated logs a warning and records an event on the
// monitoring namespace when user workload monitoring is off: the driver's
// alerts live in that namespace and nothing evaluates them without it.
func WarnIfAlertsNotEvaluated(ctx context.Context, cs kubernetes.Interface, monitoringNS string) {
	if UserWorkloadMonitoringEnabled(ctx, cs) {
		return
	}
	const msg = "User workload monitoring is off, so the driver's alerts are not evaluated. " +
		"An admin turns it on with enableUserWorkload: true in openshift-monitoring/cluster-monitoring-config."
	klog.InfoS("Alerts are not evaluated: user workload monitoring is off", "namespace", monitoringNS)
	klog.Warning(msg)
	_, err := cs.CoreV1().Events(monitoringNS).Create(ctx, &corev1.Event{
		ObjectMeta:     metav1.ObjectMeta{GenerateName: "user-workload-monitoring-off-", Namespace: monitoringNS},
		InvolvedObject: corev1.ObjectReference{Kind: "Namespace", Name: monitoringNS, APIVersion: "v1"},
		Reason:         "UserWorkloadMonitoringOff",
		Message:        msg,
		Type:           corev1.EventTypeWarning,
		Source:         corev1.EventSource{Component: "ibm-vpc-file-pool-csi-controller"},
	}, metav1.CreateOptions{})
	if err != nil {
		klog.ErrorS(err, "Cannot record the user workload monitoring event")
	}
}
