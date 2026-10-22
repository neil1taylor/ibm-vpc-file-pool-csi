package metrics

import (
	"github.com/prometheus/client_golang/prometheus"
	ctrlmetrics "sigs.k8s.io/controller-runtime/pkg/metrics"
)

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

	// PoolMaxShares reports the most shares the pool may have (spec.maxShares).
	PoolMaxShares = prometheus.NewGaugeVec(
		prometheus.GaugeOpts{
			Name: "vpc_file_pool_max_shares",
			Help: "Most shares the pool may have (spec.maxShares)",
		},
		[]string{"pool"},
	)

	// ShareTotalGB reports the provisioned size of one share of a pool.
	ShareTotalGB = prometheus.NewGaugeVec(
		prometheus.GaugeOpts{
			Name: "vpc_file_pool_share_total_gb",
			Help: "Provisioned size of one share of the pool in GB",
		},
		[]string{"pool", "share"},
	)

	// ShareAllocatedGB reports the capacity promised to PVCs on one share.
	// It is what PVCs asked for, not what they wrote.
	ShareAllocatedGB = prometheus.NewGaugeVec(
		prometheus.GaugeOpts{
			Name: "vpc_file_pool_share_allocated_gb",
			Help: "Capacity allocated to PVCs on one share of the pool in GB",
		},
		[]string{"pool", "share"},
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

	// CloneDuration tracks the time from the clone worker picking up a clone to the end of its copy Job.
	CloneDuration = prometheus.NewHistogramVec(
		prometheus.HistogramOpts{
			Name:    "vpc_file_pool_clone_duration_seconds",
			Help:    "Time from the clone worker picking up a clone to the end of its copy Job",
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

	// BackupVolumes is the number of volumes a BackupPolicy backs up.
	BackupVolumes = prometheus.NewGaugeVec(
		prometheus.GaugeOpts{
			Name: "vpc_file_pool_backup_volumes",
			Help: "Number of volumes a backup policy backs up",
		},
		[]string{"policy"},
	)

	// BackupStalestAgeSeconds is the age of the newest backup of the volume
	// that was backed up longest ago. A volume that was never backed up counts
	// from the time the policy was made.
	BackupStalestAgeSeconds = prometheus.NewGaugeVec(
		prometheus.GaugeOpts{
			Name: "vpc_file_pool_backup_stalest_age_seconds",
			Help: "Age in seconds of the newest backup of the least recently backed-up volume of a policy",
		},
		[]string{"policy"},
	)

	// BackupOldestAgeSeconds is the age of the oldest backup that is kept.
	BackupOldestAgeSeconds = prometheus.NewGaugeVec(
		prometheus.GaugeOpts{
			Name: "vpc_file_pool_backup_oldest_age_seconds",
			Help: "Age in seconds of the oldest backup a policy keeps",
		},
		[]string{"policy"},
	)

	// BackupExportOldestUnexportedAgeSeconds is the age of the oldest moment
	// that is in the snapshot pool and not yet in COS. 0 when all is exported.
	BackupExportOldestUnexportedAgeSeconds = prometheus.NewGaugeVec(
		prometheus.GaugeOpts{
			Name: "vpc_file_pool_backup_export_oldest_unexported_age_seconds",
			Help: "Age in seconds of the oldest backup moment of a policy that is not yet exported to COS",
		},
		[]string{"policy"},
	)

	// BackupExportNewestAgeSeconds is the age of the newest export to COS. With
	// backups to export and no export yet, it counts from the time the policy
	// was made.
	BackupExportNewestAgeSeconds = prometheus.NewGaugeVec(
		prometheus.GaugeOpts{
			Name: "vpc_file_pool_backup_export_newest_age_seconds",
			Help: "Age in seconds of the newest export of a policy to COS",
		},
		[]string{"policy"},
	)

	// BackupSnapshotPoolUsedPercent is how full a share of a snapshot pool was
	// when a Job last read it.
	BackupSnapshotPoolUsedPercent = prometheus.NewGaugeVec(
		prometheus.GaugeOpts{
			Name: "vpc_file_pool_backup_snapshot_pool_used_percent",
			Help: "Percent of a share of a snapshot pool that is used, as last read by a Job",
		},
		[]string{"pool", "share"},
	)

	// CDICopyLimitRaisedTotal counts how often the controller raised CDI's
	// copy pod memory limit. More than one raise means something resets it.
	CDICopyLimitRaisedTotal = prometheus.NewCounter(
		prometheus.CounterOpts{
			Name: "vpc_file_pool_cdi_copy_limit_raised_total",
			Help: "Times the controller raised the CDI copy pod memory limit",
		},
	)
)

func init() {
	ctrlmetrics.Registry.MustRegister(
		AllocationsTotal,
		AllocationDuration,
		PoolCapacityGB,
		PoolAllocatedGB,
		PoolShareCount,
		PoolMaxShares,
		ShareTotalGB,
		ShareAllocatedGB,
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
		BackupVolumes,
		BackupStalestAgeSeconds,
		BackupOldestAgeSeconds,
		BackupSnapshotPoolUsedPercent,
		BackupExportOldestUnexportedAgeSeconds,
		BackupExportNewestAgeSeconds,
		CDICopyLimitRaisedTotal,
	)
}
