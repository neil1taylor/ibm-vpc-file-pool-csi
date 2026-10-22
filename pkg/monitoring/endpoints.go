// Package monitoring keeps the pieces that let OpenShift's user workload
// monitoring scrape the driver, which lives in kube-system where it cannot.
package monitoring

import (
	"context"
	"fmt"
	"reflect"
	"sort"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/client-go/kubernetes"
	"k8s.io/klog/v2"
)

// Target is one metrics Service in the monitoring namespace. It has no selector;
// its Endpoints list the driver pods of one component.
type Target struct {
	Service   string
	Component string // value of the app.kubernetes.io/component label
	Port      int32
}

// Config says which pods to list and which Endpoints to write.
type Config struct {
	DriverNamespace     string
	MonitoringNamespace string
	Selector            map[string]string // labels shared by all driver pods
	Targets             []Target
}

// Syncer writes the Endpoints of the metrics Services in the monitoring
// namespace. Kubernetes mirrors them to EndpointSlices.
type Syncer struct {
	cs  kubernetes.Interface
	cfg Config
}

// NewSyncer creates a syncer.
func NewSyncer(cs kubernetes.Interface, cfg Config) *Syncer {
	return &Syncer{cs: cs, cfg: cfg}
}

// Sync lists the driver pods and brings each Endpoints object in line.
func (s *Syncer) Sync(ctx context.Context) error {
	pods, err := s.cs.CoreV1().Pods(s.cfg.DriverNamespace).List(ctx, metav1.ListOptions{
		LabelSelector: labels.SelectorFromSet(s.cfg.Selector).String(),
	})
	if err != nil {
		return fmt.Errorf("list driver pods: %w", err)
	}
	var firstErr error
	for _, t := range s.cfg.Targets {
		if err := s.syncTarget(ctx, t, pods.Items); err != nil && firstErr == nil {
			firstErr = err
		}
	}
	return firstErr
}

func (s *Syncer) syncTarget(ctx context.Context, t Target, pods []corev1.Pod) error {
	var addrs []corev1.EndpointAddress
	for _, p := range pods {
		if p.Labels["app.kubernetes.io/component"] != t.Component || p.Status.PodIP == "" || !podReady(p) {
			continue
		}
		addrs = append(addrs, corev1.EndpointAddress{
			IP:        p.Status.PodIP,
			TargetRef: &corev1.ObjectReference{Kind: "Pod", Name: p.Name, Namespace: p.Namespace},
		})
	}
	sort.Slice(addrs, func(i, j int) bool { return addrs[i].IP < addrs[j].IP })

	var subsets []corev1.EndpointSubset
	if len(addrs) > 0 {
		subsets = []corev1.EndpointSubset{{
			Addresses: addrs,
			Ports:     []corev1.EndpointPort{{Name: "metrics", Port: t.Port, Protocol: corev1.ProtocolTCP}},
		}}
	}

	eps := s.cs.CoreV1().Endpoints(s.cfg.MonitoringNamespace)
	cur, err := eps.Get(ctx, t.Service, metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		_, err = eps.Create(ctx, &corev1.Endpoints{
			ObjectMeta: metav1.ObjectMeta{Name: t.Service, Namespace: s.cfg.MonitoringNamespace,
				Labels: map[string]string{"app.kubernetes.io/managed-by": "ibm-vpc-file-pool-csi-controller"}},
			Subsets: subsets,
		}, metav1.CreateOptions{})
		if err != nil {
			return fmt.Errorf("create endpoints %s: %w", t.Service, err)
		}
		return nil
	}
	if err != nil {
		return fmt.Errorf("get endpoints %s: %w", t.Service, err)
	}
	if reflect.DeepEqual(cur.Subsets, subsets) || (len(cur.Subsets) == 0 && len(subsets) == 0) {
		return nil
	}
	cur.Subsets = subsets
	if _, err := eps.Update(ctx, cur, metav1.UpdateOptions{}); err != nil {
		return fmt.Errorf("update endpoints %s: %w", t.Service, err)
	}
	return nil
}

func podReady(p corev1.Pod) bool {
	if p.Status.Phase != corev1.PodRunning {
		return false
	}
	for _, c := range p.Status.Conditions {
		if c.Type == corev1.PodReady {
			return c.Status == corev1.ConditionTrue
		}
	}
	return false
}

// Run syncs now and then every interval until ctx ends.
func (s *Syncer) Run(ctx context.Context, interval time.Duration) {
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		if err := s.Sync(ctx); err != nil {
			klog.ErrorS(err, "Cannot update the metrics Endpoints in the monitoring namespace")
		}
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}
