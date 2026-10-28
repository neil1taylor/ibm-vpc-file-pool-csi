/** Texts of the PoolBench pages. */

export const POOLBENCH_INTRO =
  'PoolBench measures how many VMs one share carries before users notice. Each run makes its own pool and test VMs, ' +
  'adds VMs in steps while it measures, finds the step where the VMs slow down (the knee), and deletes everything at the end.';

export const POOLBENCH_SHARED_NODES =
  'The test VMs use their own pool, so the storage of your VMs is never tested. They do use the nodes\' CPU, memory and network.';

export const POOLBENCH_KEPT =
  'The test VMs and the pool are kept after the end, and they cost money until you clean up.';

/** Help for the terms of the Settings card of a run. */
export const POOLBENCH_TERM_HELP: Record<string, string> = {
  Share: 'The size and the IOPS limit of the one share of the test pool, and the name of the pool.',
  VMs: 'How many VMs each step adds, the most VMs the run goes up to, and how many of them run Windows.',
  'Load per VM':
    'What each VM does: a steady rate, and a short burst every period. Reads is the share of reads. Blocks lists the block sizes with their share of the I/O.',
  'Each step':
    'Settle is the wait after new VMs are ready, because making them slows the others. Measure is the time the numbers are taken.',
  'Knee rule':
    'A step fails, and is the knee, when too many VMs are slow, any I/O stalls, or the share is full. p95 means 95 of 100 I/O were this fast or faster.',
  Namespace: 'The namespace that holds the test VMs. The run deletes it at the end.',
};

export const POOLBENCH_PHASE_HELP_TITLE = 'State of a run';
