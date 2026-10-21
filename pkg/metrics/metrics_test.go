package metrics

import (
	"testing"

	"github.com/prometheus/client_golang/prometheus"
	dto "github.com/prometheus/client_model/go"
	ctrlmetrics "sigs.k8s.io/controller-runtime/pkg/metrics"
)

// writeObserver writes the current state of a histogram observer into m.
func writeObserver(t *testing.T, o prometheus.Observer, m *dto.Metric) {
	t.Helper()
	pm, ok := o.(prometheus.Metric)
	if !ok {
		t.Fatalf("observer %T is not a prometheus.Metric", o)
	}
	if err := pm.Write(m); err != nil {
		t.Fatalf("failed to write metric: %v", err)
	}
}

func TestAllocationsTotal_Increment(t *testing.T) {
	AllocationsTotal.WithLabelValues("test-pool", "success").Inc()

	var m dto.Metric
	if err := AllocationsTotal.WithLabelValues("test-pool", "success").Write(&m); err != nil {
		t.Fatalf("failed to write metric: %v", err)
	}
	if m.Counter == nil || *m.Counter.Value < 1 {
		t.Error("expected counter to be at least 1")
	}
}

func TestAllocationDuration_Observe(t *testing.T) {
	AllocationDuration.WithLabelValues("test-pool").Observe(0.5)

	var m dto.Metric
	writeObserver(t, AllocationDuration.WithLabelValues("test-pool"), &m)
	if m.Histogram == nil || *m.Histogram.SampleCount < 1 {
		t.Error("expected histogram sample count to be at least 1")
	}
}

func TestPoolGauges_Set(t *testing.T) {
	tests := []struct {
		name  string
		gauge *prometheus.GaugeVec
		value float64
	}{
		{"PoolCapacityGB", PoolCapacityGB, 1000},
		{"PoolAllocatedGB", PoolAllocatedGB, 500},
		{"PoolShareCount", PoolShareCount, 3},
		{"PoolPVCCount", PoolPVCCount, 25},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			tt.gauge.WithLabelValues("test-pool").Set(tt.value)

			var m dto.Metric
			if err := tt.gauge.WithLabelValues("test-pool").Write(&m); err != nil {
				t.Fatalf("failed to write metric: %v", err)
			}
			if m.Gauge == nil || *m.Gauge.Value != tt.value {
				t.Errorf("expected gauge value %f, got %f", tt.value, *m.Gauge.Value)
			}
		})
	}
}

func TestVPCAPICallsTotal_Increment(t *testing.T) {
	VPCAPICallsTotal.WithLabelValues("create_share", "success").Inc()

	var m dto.Metric
	if err := VPCAPICallsTotal.WithLabelValues("create_share", "success").Write(&m); err != nil {
		t.Fatalf("failed to write metric: %v", err)
	}
	if m.Counter == nil || *m.Counter.Value < 1 {
		t.Error("expected counter to be at least 1")
	}
}

func TestVPCAPICallDuration_Observe(t *testing.T) {
	VPCAPICallDuration.WithLabelValues("create_share").Observe(1.5)

	var m dto.Metric
	writeObserver(t, VPCAPICallDuration.WithLabelValues("create_share"), &m)
	if m.Histogram == nil || *m.Histogram.SampleCount < 1 {
		t.Error("expected histogram sample count to be at least 1")
	}
}

func TestSnapshotsTotal_Increment(t *testing.T) {
	SnapshotsTotal.WithLabelValues("test-pool", "create", "success").Inc()

	var m dto.Metric
	if err := SnapshotsTotal.WithLabelValues("test-pool", "create", "success").Write(&m); err != nil {
		t.Fatalf("failed to write metric: %v", err)
	}
	if m.Counter == nil || *m.Counter.Value < 1 {
		t.Error("expected counter to be at least 1")
	}
}

func TestSnapshotDuration_Observe(t *testing.T) {
	SnapshotDuration.WithLabelValues("test-pool", "create").Observe(0.5)

	var m dto.Metric
	writeObserver(t, SnapshotDuration.WithLabelValues("test-pool", "create"), &m)
	if m.Histogram == nil || *m.Histogram.SampleCount < 1 {
		t.Error("expected histogram sample count to be at least 1")
	}
}

func TestAllCollectorsRegistered(t *testing.T) {
	// A vector with no series is not gathered; make one for each backup metric.
	BackupVolumes.WithLabelValues("test-policy").Set(1)
	BackupStalestAgeSeconds.WithLabelValues("test-policy").Set(1)
	BackupOldestAgeSeconds.WithLabelValues("test-policy").Set(1)
	BackupSnapshotPoolUsedPercent.WithLabelValues("test-pool", "test-share").Set(1)

	// Gather from controller-runtime's registry (where init() registers them).
	gatherer, ok := ctrlmetrics.Registry.(prometheus.Gatherer)
	if !ok {
		t.Fatal("controller-runtime metrics.Registry does not implement prometheus.Gatherer")
	}
	families, err := gatherer.Gather()
	if err != nil {
		t.Fatalf("failed to gather metrics: %v", err)
	}

	expected := map[string]bool{
		"vpc_file_pool_allocations_total":                 false,
		"vpc_file_pool_allocation_duration_seconds":       false,
		"vpc_file_pool_capacity_gb":                       false,
		"vpc_file_pool_allocated_gb":                      false,
		"vpc_file_pool_share_count":                       false,
		"vpc_file_pool_pvc_count":                         false,
		"vpc_file_pool_api_calls_total":                   false,
		"vpc_file_pool_api_call_duration_seconds":         false,
		"vpc_file_pool_snapshots_total":                   false,
		"vpc_file_pool_snapshot_duration_seconds":         false,
		"vpc_file_pool_cdi_copy_limit_raised_total":       false,
		"vpc_file_pool_backup_volumes":                    false,
		"vpc_file_pool_backup_stalest_age_seconds":        false,
		"vpc_file_pool_backup_oldest_age_seconds":         false,
		"vpc_file_pool_backup_snapshot_pool_used_percent": false,
	}

	for _, fam := range families {
		if _, ok := expected[fam.GetName()]; ok {
			expected[fam.GetName()] = true
		}
	}

	for name, found := range expected {
		if !found {
			t.Errorf("metric %q not found in default registry", name)
		}
	}
}
