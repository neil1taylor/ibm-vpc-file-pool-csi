/** Texts of the PoolBench pages. */

export const POOLBENCH_INTRO =
  'PoolBench measures how many VMs one share carries before users notice. Each run makes its own pool and test VMs, ' +
  'adds VMs in steps while it measures, finds the step where the VMs slow down (the knee), and deletes everything at the end.';

export const POOLBENCH_SHARED_NODES =
  'The test VMs use their own pool, so the storage of your VMs is never tested. They do use the nodes\' CPU, memory and network.';

export const POOLBENCH_KEPT =
  'The test VMs and the pool are kept after the end, and they cost money until you clean up.';
