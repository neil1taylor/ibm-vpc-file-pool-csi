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
