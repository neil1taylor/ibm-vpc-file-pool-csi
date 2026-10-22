package monitoring

import (
	"context"
	"errors"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/kubernetes/fake"
	k8stesting "k8s.io/client-go/testing"
)

func pod(name, component, ip string, ready bool) *corev1.Pod {
	st := corev1.ConditionFalse
	if ready {
		st = corev1.ConditionTrue
	}
	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "kube-system", Labels: map[string]string{
			"app.kubernetes.io/name": "csi", "app.kubernetes.io/instance": "rel", "app.kubernetes.io/component": component}},
		Status: corev1.PodStatus{Phase: corev1.PodRunning, PodIP: ip,
			Conditions: []corev1.PodCondition{{Type: corev1.PodReady, Status: st}}},
	}
}

func testConfig() Config {
	return Config{
		DriverNamespace:     "kube-system",
		MonitoringNamespace: "mon",
		Selector:            map[string]string{"app.kubernetes.io/name": "csi", "app.kubernetes.io/instance": "rel"},
		Targets: []Target{
			{Service: "rel-metrics", Component: "controller", Port: 8080},
			{Service: "rel-node-metrics", Component: "node", Port: 9810},
		},
	}
}

func TestSync_CreatesEndpointsForReadyPodsOnly(t *testing.T) {
	cs := fake.NewClientset(
		pod("c1", "controller", "10.1.0.5", true),
		pod("n2", "node", "10.0.0.2", true),
		pod("n1", "node", "10.0.0.1", true),
		pod("n3", "node", "10.0.0.3", false), // not ready
		pod("n4", "node", "", true),          // no address yet
	)
	if err := NewSyncer(cs, testConfig()).Sync(context.Background()); err != nil {
		t.Fatal(err)
	}

	ep, err := cs.CoreV1().Endpoints("mon").Get(context.Background(), "rel-node-metrics", metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if len(ep.Subsets) != 1 || len(ep.Subsets[0].Addresses) != 2 {
		t.Fatalf("want 2 addresses, got %+v", ep.Subsets)
	}
	if ep.Subsets[0].Addresses[0].IP != "10.0.0.1" || ep.Subsets[0].Addresses[1].IP != "10.0.0.2" {
		t.Errorf("addresses not sorted or wrong: %+v", ep.Subsets[0].Addresses)
	}
	if p := ep.Subsets[0].Ports[0]; p.Name != "metrics" || p.Port != 9810 || p.Protocol != corev1.ProtocolTCP {
		t.Errorf("port = %+v", p)
	}
	if ref := ep.Subsets[0].Addresses[0].TargetRef; ref == nil || ref.Kind != "Pod" || ref.Name != "n1" || ref.Namespace != "kube-system" {
		t.Errorf("target ref = %+v", ref)
	}

	ctl, err := cs.CoreV1().Endpoints("mon").Get(context.Background(), "rel-metrics", metav1.GetOptions{})
	if err != nil || ctl.Subsets[0].Addresses[0].IP != "10.1.0.5" || ctl.Subsets[0].Ports[0].Port != 8080 {
		t.Fatalf("controller endpoints = %+v, %v", ctl, err)
	}
}

func TestSync_NoPodsGivesEmptyEndpoints(t *testing.T) {
	cs := fake.NewClientset()
	if err := NewSyncer(cs, testConfig()).Sync(context.Background()); err != nil {
		t.Fatal(err)
	}
	ep, err := cs.CoreV1().Endpoints("mon").Get(context.Background(), "rel-node-metrics", metav1.GetOptions{})
	if err != nil || len(ep.Subsets) != 0 {
		t.Fatalf("want empty endpoints, got %+v %v", ep, err)
	}
}

func TestSync_UpdatesOnlyWhenPodsChange(t *testing.T) {
	cs := fake.NewClientset(pod("n1", "node", "10.0.0.1", true))
	s := NewSyncer(cs, testConfig())
	ctx := context.Background()
	if err := s.Sync(ctx); err != nil {
		t.Fatal(err)
	}

	cs.ClearActions()
	if err := s.Sync(ctx); err != nil {
		t.Fatal(err)
	}
	for _, a := range cs.Actions() {
		if a.GetVerb() == "update" || a.GetVerb() == "create" {
			t.Errorf("unchanged pods must not write: %v", a)
		}
	}

	if _, err := cs.CoreV1().Pods("kube-system").Create(ctx, pod("n2", "node", "10.0.0.2", true), metav1.CreateOptions{}); err != nil {
		t.Fatal(err)
	}
	if err := s.Sync(ctx); err != nil {
		t.Fatal(err)
	}
	ep, _ := cs.CoreV1().Endpoints("mon").Get(ctx, "rel-node-metrics", metav1.GetOptions{})
	if len(ep.Subsets[0].Addresses) != 2 {
		t.Errorf("new node not added: %+v", ep.Subsets)
	}
}

func TestSync_ListErrorChangesNothing(t *testing.T) {
	cs := fake.NewClientset()
	cs.PrependReactor("list", "pods", func(k8stesting.Action) (bool, runtime.Object, error) {
		return true, nil, errors.New("api down")
	})
	if err := NewSyncer(cs, testConfig()).Sync(context.Background()); err == nil {
		t.Fatal("want an error")
	}
	if eps, _ := cs.CoreV1().Endpoints("mon").List(context.Background(), metav1.ListOptions{}); len(eps.Items) != 0 {
		t.Errorf("must not write when listing fails: %+v", eps.Items)
	}
}
