package metrics

import "github.com/prometheus/client_golang/prometheus"

var (
	// AllocationsTotal counts subvolume allocations and deallocations by pool and status.
	AllocationsTotal = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Name: "vpc_file_pool_allocations_total",
			Help: "Total number of subvolume allocations",
		},
		[]string{"pool", "status"},
	)

	// AllocationDuration tracks the time to allocate a subvolume.
	AllocationDuration = prometheus.NewHistogramVec(
		prometheus.HistogramOpts{
			Name:    "vpc_file_pool_allocation_duration_seconds",
			Help:    "Time to allocate a subvolume",
			Buckets: prometheus.ExponentialBuckets(0.01, 2, 15), // 10ms to ~163s
		},
		[]string{"pool"},
	)

	// PoolCapacityGB reports total pool capacity in GB.
	PoolCapacityGB = prometheus.NewGaugeVec(
		prometheus.GaugeOpts{
			Name: "vpc_file_pool_capacity_gb",
			Help: "Total pool capacity in GB",
		},
		[]string{"pool"},
	)

	// PoolAllocatedGB reports total allocated capacity in GB.
	PoolAllocatedGB = prometheus.NewGaugeVec(
		prometheus.GaugeOpts{
			Name: "vpc_file_pool_allocated_gb",
			Help: "Total allocated capacity in GB",
		},
		[]string{"pool"},
	)

	// PoolShareCount reports the number of VPC file shares in the pool.
	PoolShareCount = prometheus.NewGaugeVec(
		prometheus.GaugeOpts{
			Name: "vpc_file_pool_share_count",
			Help: "Number of VPC file shares in the pool",
		},
		[]string{"pool"},
	)

	// PoolPVCCount reports the number of PVCs in the pool.
	PoolPVCCount = prometheus.NewGaugeVec(
		prometheus.GaugeOpts{
			Name: "vpc_file_pool_pvc_count",
			Help: "Number of PVCs in the pool",
		},
		[]string{"pool"},
	)

	// VPCAPICallsTotal counts VPC API calls by operation and status.
	VPCAPICallsTotal = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Name: "vpc_file_pool_api_calls_total",
			Help: "Total VPC API calls",
		},
		[]string{"operation", "status"},
	)

	// VPCAPICallDuration tracks VPC API call duration by operation.
	VPCAPICallDuration = prometheus.NewHistogramVec(
		prometheus.HistogramOpts{
			Name:    "vpc_file_pool_api_call_duration_seconds",
			Help:    "VPC API call duration",
			Buckets: prometheus.ExponentialBuckets(0.1, 2, 15), // 100ms to ~1638s
		},
		[]string{"operation"},
	)

	// SnapshotsTotal counts snapshot operations by pool, operation (create/delete/restore), and status.
	SnapshotsTotal = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Name: "vpc_file_pool_snapshots_total",
			Help: "Total number of snapshot operations",
		},
		[]string{"pool", "operation", "status"},
	)

	// SnapshotDuration tracks the time for snapshot operations.
	SnapshotDuration = prometheus.NewHistogramVec(
		prometheus.HistogramOpts{
			Name:    "vpc_file_pool_snapshot_duration_seconds",
			Help:    "Time for snapshot operations",
			Buckets: prometheus.ExponentialBuckets(0.01, 2, 15), // 10ms to ~163s
		},
		[]string{"pool", "operation"},
	)
)

func init() {
	prometheus.MustRegister(
		AllocationsTotal,
		AllocationDuration,
		PoolCapacityGB,
		PoolAllocatedGB,
		PoolShareCount,
		PoolPVCCount,
		VPCAPICallsTotal,
		VPCAPICallDuration,
		SnapshotsTotal,
		SnapshotDuration,
	)
}
