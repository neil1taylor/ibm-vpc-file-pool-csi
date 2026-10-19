import { FileSharePool, PoolShareStatus } from '../types';
import { phaseToPatternFlyStatus } from './resource-helpers';

export interface ShareHealth {
  label: string;
  status: 'success' | 'info' | 'warning' | 'danger' | 'custom';
}

/** A share can serve volumes only when it is stable and has a mount target IP. */
export function shareIsUsable(share: PoolShareStatus): boolean {
  return share.state === 'stable' && !!share.mountTargetIP;
}

export function hasUsableShare(pool: FileSharePool): boolean {
  return (pool.status?.shares ?? []).some(shareIsUsable);
}

export function shareHealth(share: PoolShareStatus): ShareHealth {
  if (share.state === 'stable' && !share.mountTargetIP) {
    return { label: 'No mount target', status: 'warning' };
  }
  return { label: share.state, status: phaseToPatternFlyStatus(share.state) };
}

/** Why a pool is not Ready, taken from the condition that explains its phase. */
export function poolPhaseReason(pool: FileSharePool): string | undefined {
  const phase = pool.status?.phase;
  if (!phase || phase === 'Ready') return undefined;
  const type = phase === 'Full' ? 'CapacityAvailable' : 'SharesReady';
  const condition = pool.status?.conditions?.find((c) => c.type === type);
  if (!condition || condition.status !== 'False' || !condition.message) return undefined;
  return condition.message;
}

/** One ID per line or comma-separated; blank entries are dropped. */
export function parseSecurityGroupIDs(input: string): string[] {
  return input
    .split(/[\n,]/)
    .map((id) => id.trim())
    .filter((id) => id !== '');
}
