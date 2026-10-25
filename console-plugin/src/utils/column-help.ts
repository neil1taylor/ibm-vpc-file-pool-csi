import { ThProps } from '@patternfly/react-table';

/**
 * Plain-language help for the status columns of the list pages: what each value means and what to do
 * when it is bad. One to three sentences each.
 */
export interface ColumnHelp {
  title: string;
  text: string;
}

export const COLUMN_HELP = {
  poolPhase: {
    title: 'Phase',
    text: 'Ready means new volumes can be placed; Initializing and Expanding mean a share is being made. Degraded means a share is draining or failing, or no share can be mounted: open the pool and read its shares and conditions. Full means every share is full and the pool is at its share limit: raise the share limit.',
  },
  subVolumePhase: {
    title: 'Phase',
    text: 'Bound means the volume is ready to use; Creating, Cloning and Expanding are steps that finish by themselves. Deleting, Retained and Archived follow the reclaim policy of the volume after its claim is deleted. Failed needs a look: open the volume and read its conditions.',
  },
  cloneStatus: {
    title: 'Clone Status',
    text: 'Only for a volume made as a clone or a restore. Pending and InProgress mean the copy Job is running, and the volume cannot be mounted until it is Complete. Failed means the copy Job failed: read its log in the driver namespace.',
  },
  snapshotPhase: {
    title: 'Phase',
    text: 'Creating means the snapshot is being taken, and Ready means it can be restored. Deleting finishes by itself; for an instant VM snapshot it includes merging its file into the disk. Failed needs a look: open the snapshot and read its message.',
  },
  snapshotReady: {
    title: 'Ready',
    text: 'True when the snapshot is complete and can be restored to a new volume.',
  },
  replicationPhase: {
    title: 'Phase',
    text: 'Active means the policy syncs on its schedule. Paused means someone paused it, or more syncs in a row failed than its retry limit allows: read the last error, fix the cause, then resume the policy on its page. Failed means the policy stopped: read its last error.',
  },
  replicationLastSync: {
    title: 'Last Sync',
    text: 'How long ago the last sync of the whole policy finished. Much older than the schedule means syncs are late or failing.',
  },
  replicationReceiver: {
    title: 'Receiver',
    text: 'Only for a policy that copies to a receiver on another cluster. Reachable means the receiver answers, its certificate is trusted and it accepts the token. Not reachable gives the reason (for example the token or the certificate is wrong): fix it on the policy page, because syncs will fail until then.',
  },
  replicationFailures: {
    title: 'Failures',
    text: 'Syncs in a row that failed; normal is 0. When it reaches the retry limit of the policy (3 by default), the policy pauses and stops syncing until it is resumed.',
  },
  syncCopyMoment: {
    title: 'Last Sync / Copy Moment',
    text: 'For a running VM disk, the moment its last good copy shows; all disks of the VM show the same moment. For other volumes, how long ago the last sync finished.',
  },
  syncConsistency: {
    title: 'Consistency',
    text: 'FileSystemConsistent means the guest file systems were frozen for the copy. CrashConsistent means they were not, as after a power loss; most applications recover from that, and the reason is shown below the value.',
  },
  syncRestorePoints: {
    title: 'Restore Points',
    text: 'How many earlier copies of a running VM disk the destination keeps, so a failover can go back to an earlier moment.',
  },
  syncNextCopy: {
    title: 'Next Copy',
    text: 'For a running VM disk: whether the next copy sends only the blocks changed since the last copy, or the full disk. A full copy is needed again, for example, after the VM moved to another node.',
  },
  syncStatus: {
    title: 'Status',
    text: 'Copying or Sending while a copy of a running VM disk is in progress, and the last message for the volume. An orange message only says the volume is waiting; a red message is an error to read.',
  },
  poolbenchPhase: {
    title: 'State',
    text: 'Checking and Running mean the run goes on; Paused means someone paused it. CleaningUp deletes the test VMs and the pool. Complete and Stopped have a result; Failed says why on its row and on its page.',
  },
  poolbenchKnee: {
    title: 'Knee',
    text: 'The VM count of the first step where users would notice: too many VMs slow, any stall, or the share at its IOPS limit. A dash means no knee was found up to the last step measured.',
  },
  poolbenchRecommended: {
    title: 'Recommended',
    text: 'The largest step at or below 80% of the knee: how many VMs to put on a share like this one. With no knee it is the largest step measured, so the real limit is higher.',
  },
  backupPhase: {
    title: 'Phase',
    text: 'Active means the policy makes backups on its schedule, and Suspended means someone stopped new backups (existing backups stay). Error means something is wrong, such as a snapshot pool that is more than 85% full: read the message on the page.',
  },
  backupStalest: {
    title: 'Stalest Backup',
    text: 'How long ago the oldest of the latest backups was made, over the volumes the policy still backs up. Much longer than the schedule means a volume is late: open the policy and read the error of that volume.',
  },
  backupListExport: {
    title: 'Export',
    text: 'On means the policy also sends its backups to an IBM Cloud Object Storage bucket, so they survive the loss of the snapshot pool. Off means backups stay in the snapshot pool only.',
  },
  backupLast: {
    title: 'Last Backup',
    text: 'How long ago the newest backup of this volume was made. For a running VM, all disks of the VM show the same moment.',
  },
  backupOldest: {
    title: 'Oldest Backup',
    text: 'How long ago the oldest backup that is still kept was made. A restore can go back as far as this.',
  },
  backupRestorePoints: {
    title: 'Restore Points',
    text: 'How many older moments, besides the newest, a restore can go back to. Backups older than the keep time of the policy are removed.',
  },
  backupConsistency: {
    title: 'Consistency',
    text: 'File-system consistent means the guest file systems were frozen for the copy. Crash consistent means they were not, as after a power loss; most applications recover from that. File copy is a plain volume that no VM uses.',
  },
  backupVolumeExport: {
    title: 'Export',
    text: 'Whether the newest backup of this volume reached the COS bucket, and how long ago. A failure is tried again by itself and never stops the backups in the snapshot pool.',
  },
  poolbenchShare: {
    title: 'Share',
    text: 'The one file share of the test pool, as size and speed: GB and the IOPS limit. All test VMs of the run use this share.',
  },
  poolbenchVMsNow: {
    title: 'VMs now',
    text: 'How many test VMs the run has now, and how many of them run Windows. A run that has ended shows the count of its last step.',
  },
  poolbenchStarted: {
    title: 'Started',
    text: 'When the run began.',
  },
  poolbenchRunTime: {
    title: 'Run time',
    text: 'How long the run has taken, or took in all. It includes the cleanup at the end.',
  },
  poolbenchStepVMs: {
    title: 'VMs',
    text: 'How many test VMs ran in this step, and how many of them run Windows. Each step adds more VMs to the same share.',
  },
  poolbenchShareIOPS: {
    title: 'Share IOPS',
    text: 'How many operations a second the share served, counted at the share by the driver. Compare with Guest IOPS: they should be close.',
  },
  poolbenchGuestIOPS: {
    title: 'Guest IOPS',
    text: 'How many I/O a second the VMs say they did, added up for all VMs.',
  },
  poolbenchIBMOps: {
    title: 'IBM operations',
    text: 'IBM counts one operation for each 16 KiB of an I/O, so a large I/O counts more than once. The percent is the part of the share IOPS limit used. At 95% the share is full and the step fails.',
  },
  poolbenchShareLatency: {
    title: 'Share latency',
    text: 'The average time the share took to answer one request, measured at the node. It does not include the VM.',
  },
  poolbenchP50: {
    title: 'p50',
    text: 'The typical latency: half of the I/O finished in this time or faster. The value is the median over all VMs.',
  },
  poolbenchP95: {
    title: 'p95',
    text: 'Out of 100 I/O, 95 finished in this time or faster. The knee rule uses it. The value is the median over all VMs, each taken at its worst.',
  },
  poolbenchP99: {
    title: 'p99',
    text: 'Out of 100 I/O, 99 finished in this time or faster. It shows the rare slow I/O that users still feel.',
  },
  poolbenchSlowVMs: {
    title: 'Slow VMs',
    text: 'The share of VMs whose p95 is over the latency limit. When it is over the limit set for the knee, the step fails.',
  },
  poolbenchStalls: {
    title: 'Stalls',
    text: 'How many I/O took longer than the stall time. One stall is enough to fail the step.',
  },
  poolbenchReadyAfter: {
    title: 'Ready after',
    text: 'How long the new VMs of this step took until all were up, had copied their disks and sent their first result.',
  },
  poolbenchResult: {
    title: 'Result',
    text: 'Pass means users would not notice; Fail means too many slow VMs, a stall, or a full share. Knee is the first failed step, and Recommended is the VM count to plan for.',
  },
  poolbenchVMOS: {
    title: 'OS',
    text: 'The operating system of the test VM: Linux (CentOS Stream 9) or Windows Server 2022.',
  },
  poolbenchVMNode: {
    title: 'Node',
    text: 'The worker node that runs the VM. VMs on the same node share its CPU, memory and network.',
  },
  poolbenchVMState: {
    title: 'State',
    text: 'The state of the VM in OpenShift Virtualization. Running is normal. Scheduling and Pending mean it is not up yet.',
  },
  poolbenchLastIOPS: {
    title: 'Last IOPS',
    text: 'The I/O a second this VM reported in its latest result, outside its bursts.',
  },
  poolbenchLastP95: {
    title: 'Last p95',
    text: 'Out of 100 I/O of this VM, 95 finished in this time or faster, in its latest result. Compare with the latency limit.',
  },
  poolbenchWindowsCount: {
    title: 'Windows',
    text: 'How many of the VMs in this step run Windows Server 2022. The others run Linux.',
  },
  poolbenchSteadyGuest: {
    title: 'Steady guest IOPS',
    text: 'What all VMs of this step will ask for between bursts, added up. This is the planned load; the run page shows what the VMs really did.',
  },
  poolbenchOfLimit: {
    title: 'Of the limit',
    text: 'The IBM operations as a part of the IOPS limit of the share. At 95% the step fails.',
  },
  poolbenchAllBurst: {
    title: 'If all burst at once',
    text: 'The IBM operations if every VM burst at the same moment. It is a worst case, and it may be over the limit.',
  },
} satisfies Record<string, ColumnHelp>;

export type ColumnHelpKey = keyof typeof COLUMN_HELP;

/** The info prop of a table header: a help button that opens the column text. */
export function columnInfo(key: ColumnHelpKey): ThProps['info'] {
  const { title, text } = COLUMN_HELP[key];
  return {
    popover: text,
    ariaLabel: `About the ${title} column`,
    popoverProps: { headerContent: title },
  };
}
