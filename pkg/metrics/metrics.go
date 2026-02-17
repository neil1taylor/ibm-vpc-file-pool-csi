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

	// ClonesTotal counts clone operations by pool and status (success/error).
	ClonesTotal = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Name: "vpc_file_pool_clones_total",
			Help: "Total number of clone operations",
		},
		[]string{"pool", "status"},
	)

	// CloneDuration tracks the time for synchronous clone operations.
	CloneDuration = prometheus.NewHistogramVec(
		prometheus.HistogramOpts{
			Name:    "vpc_file_pool_clone_duration_seconds",
			Help:    "Time for synchronous clone operations",
			Buckets: prometheus.ExponentialBuckets(0.01, 2, 15), // 10ms to ~163s
		},
		[]string{"pool"},
	)

	// GroupSnapshotsTotal counts group snapshot operations by pool, operation, and status.
	GroupSnapshotsTotal = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Name: "vpc_file_pool_group_snapshots_total",
			Help: "Total number of group snapshot operations",
		},
		[]string{"pool", "operation", "status"},
	)

	// GroupSnapshotDuration tracks the time for group snapshot operations.
	GroupSnapshotDuration = prometheus.NewHistogramVec(
		prometheus.HistogramOpts{
			Name:    "vpc_file_pool_group_snapshot_duration_seconds",
			Help:    "Time for group snapshot operations",
			Buckets: prometheus.ExponentialBuckets(0.01, 2, 15), // 10ms to ~163s
		},
		[]string{"pool", "operation"},
	)

	// GroupSnapshotMemberCount tracks the number of members per group snapshot.
	GroupSnapshotMemberCount = prometheus.NewHistogramVec(
		prometheus.HistogramOpts{
			Name:    "vpc_file_pool_group_snapshot_member_count",
			Help:    "Number of members per group snapshot",
			Buckets: prometheus.LinearBuckets(1, 1, 20), // 1 to 20
		},
		[]string{"pool"},
	)

	// GroupSnapshotInconsistencyWindow tracks the inconsistency window in milliseconds.
	GroupSnapshotInconsistencyWindow = prometheus.NewHistogramVec(
		prometheus.HistogramOpts{
			Name:    "vpc_file_pool_group_snapshot_inconsistency_window_ms",
			Help:    "Inconsistency window in milliseconds between first and last copy",
			Buckets: prometheus.ExponentialBuckets(1, 2, 20), // 1ms to ~524s
		},
		[]string{"pool"},
	)

	// ReplicationSyncsTotal counts replication sync attempts by policy, source pool, and result.
	ReplicationSyncsTotal = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Name: "pool_csi_replication_sync_total",
			Help: "Total replication sync attempts",
		},
		[]string{"policy", "source_pool", "result"},
	)

	// ReplicationSyncDuration tracks the duration of replication cycles.
	ReplicationSyncDuration = prometheus.NewHistogramVec(
		prometheus.HistogramOpts{
			Name:    "pool_csi_replication_sync_duration_seconds",
			Help:    "Duration of replication sync cycles",
			Buckets: prometheus.ExponentialBuckets(1, 2, 15), // 1s to ~16384s
		},
		[]string{"policy", "source_pool"},
	)

	// ReplicationLagSeconds tracks the current replication lag (time since last successful sync).
	ReplicationLagSeconds = prometheus.NewGaugeVec(
		prometheus.GaugeOpts{
			Name: "pool_csi_replication_lag_seconds",
			Help: "Current replication lag in seconds (time since last successful sync)",
		},
		[]string{"policy", "source_pool"},
	)

	// ReplicationConsecutiveFailures tracks the current consecutive failure count.
	ReplicationConsecutiveFailures = prometheus.NewGaugeVec(
		prometheus.GaugeOpts{
			Name: "pool_csi_replication_consecutive_failures",
			Help: "Current consecutive failure count for replication policy",
		},
		[]string{"policy", "source_pool"},
	)

	// ReplicationSubVolumeCount tracks the number of SubVolumes being replicated.
	ReplicationSubVolumeCount = prometheus.NewGaugeVec(
		prometheus.GaugeOpts{
			Name: "pool_csi_replication_subvolume_count",
			Help: "Number of SubVolumes being replicated per policy",
		},
		[]string{"policy", "source_pool"},
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
		ClonesTotal,
		CloneDuration,
		GroupSnapshotsTotal,
		GroupSnapshotDuration,
		GroupSnapshotMemberCount,
		GroupSnapshotInconsistencyWindow,
		ReplicationSyncsTotal,
		ReplicationSyncDuration,
		ReplicationLagSeconds,
		ReplicationConsecutiveFailures,
		ReplicationSubVolumeCount,
	)
}
