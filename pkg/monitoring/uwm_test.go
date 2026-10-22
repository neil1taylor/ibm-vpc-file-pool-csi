package monitoring

import (
	"context"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"
)

func monitoringConfig(config string) *corev1.ConfigMap {
	return &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{Name: "cluster-monitoring-config", Namespace: "openshift-monitoring"},
		Data:       map[string]string{"config.yaml": config},
	}
}

func TestUserWorkloadMonitoringEnabled(t *testing.T) {
	cases := []struct {
		name string
		cs   *fake.Clientset
		want bool
	}{
		{"on", fake.NewClientset(monitoringConfig("enableUserWorkload: true\nprometheusK8s:\n  retentionSize: 10GB\n")), true},
		{"off", fake.NewClientset(monitoringConfig("enableUserWorkload: false\n")), false},
		{"not set", fake.NewClientset(monitoringConfig("prometheusK8s:\n  retentionSize: 10GB\n")), false},
		{"no config map", fake.NewClientset(), false},
		{"no config.yaml key", fake.NewClientset(&corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: "cluster-monitoring-config", Namespace: "openshift-monitoring"}}), false},
		{"unreadable yaml", fake.NewClientset(monitoringConfig("enableUserWorkload: [")), false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := UserWorkloadMonitoringEnabled(context.Background(), c.cs); got != c.want {
				t.Errorf("got %v want %v", got, c.want)
			}
		})
	}
}

func TestWarnIfAlertsNotEvaluated(t *testing.T) {
	ctx := context.Background()

	off := fake.NewClientset(monitoringConfig("enableUserWorkload: false\n"))
	WarnIfAlertsNotEvaluated(ctx, off, "mon")
	evs, _ := off.CoreV1().Events("mon").List(ctx, metav1.ListOptions{})
	if len(evs.Items) != 1 || evs.Items[0].Reason != "UserWorkloadMonitoringOff" || evs.Items[0].Type != corev1.EventTypeWarning ||
		evs.Items[0].InvolvedObject.Kind != "Namespace" || evs.Items[0].InvolvedObject.Name != "mon" ||
		evs.Items[0].InvolvedObject.Namespace != "mon" { // the API server requires it to match the event namespace
		t.Fatalf("want one warning event on the monitoring namespace, got %+v", evs.Items)
	}

	on := fake.NewClientset(monitoringConfig("enableUserWorkload: true\n"))
	WarnIfAlertsNotEvaluated(ctx, on, "mon")
	evs, _ = on.CoreV1().Events("mon").List(ctx, metav1.ListOptions{})
	if len(evs.Items) != 0 {
		t.Errorf("no event when user workload monitoring is on: %+v", evs.Items)
	}
}
