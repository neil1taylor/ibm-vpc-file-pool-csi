package metrics

import (
	"testing"

	"github.com/prometheus/client_golang/prometheus"
	dto "github.com/prometheus/client_model/go"
)

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
	if err := AllocationDuration.WithLabelValues("test-pool").(prometheus.Metric).Write(&m); err != nil {
		t.Fatalf("failed to write metric: %v", err)
	}
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
	if err := VPCAPICallDuration.WithLabelValues("create_share").(prometheus.Metric).Write(&m); err != nil {
		t.Fatalf("failed to write metric: %v", err)
	}
	if m.Histogram == nil || *m.Histogram.SampleCount < 1 {
		t.Error("expected histogram sample count to be at least 1")
	}
}

func TestAllCollectorsRegistered(t *testing.T) {
	// Gather all registered metrics and check our 8 are present.
	families, err := prometheus.DefaultGatherer.Gather()
	if err != nil {
		t.Fatalf("failed to gather metrics: %v", err)
	}

	expected := map[string]bool{
		"vpc_file_pool_allocations_total":           false,
		"vpc_file_pool_allocation_duration_seconds": false,
		"vpc_file_pool_capacity_gb":                 false,
		"vpc_file_pool_allocated_gb":                false,
		"vpc_file_pool_share_count":                 false,
		"vpc_file_pool_pvc_count":                   false,
		"vpc_file_pool_api_calls_total":             false,
		"vpc_file_pool_api_call_duration_seconds":   false,
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
