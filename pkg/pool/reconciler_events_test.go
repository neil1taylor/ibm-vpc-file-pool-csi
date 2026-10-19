package pool

import (
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/event"

	"github.com/IBM/ibm-vpc-file-pool-csi/api/v1alpha1"
)

func TestPoolEventFilter_Update(t *testing.T) {
	base := func() *v1alpha1.FileSharePool {
		return &v1alpha1.FileSharePool{
			ObjectMeta: metav1.ObjectMeta{Name: "p", Generation: 1},
			Spec:       v1alpha1.FileSharePoolSpec{Zone: "eu-de-3"},
		}
	}

	statusOnly := base()
	now := metav1.Now()
	statusOnly.Status.LastReconcileTime = &now
	statusOnly.Status.Phase = "Ready"

	specChanged := base()
	specChanged.Generation = 2

	annotated := base()
	annotated.Annotations = map[string]string{"pool.storage.ibmcloud.io/example": "x"}

	labelled := base()
	labelled.Labels = map[string]string{"team": "a"}

	tests := []struct {
		name string
		new  *v1alpha1.FileSharePool
		want bool
	}{
		{"status-only change is ignored", statusOnly, false},
		{"spec change reconciles", specChanged, true},
		{"annotation change reconciles", annotated, true},
		{"label change reconciles", labelled, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := poolEventFilter().Update(event.UpdateEvent{ObjectOld: base(), ObjectNew: tt.new})
			if got != tt.want {
				t.Errorf("Update() = %v, want %v", got, tt.want)
			}
		})
	}
}
