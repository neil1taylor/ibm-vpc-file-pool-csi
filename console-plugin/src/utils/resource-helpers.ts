const UNITS = ['B', 'KiB', 'MiB', 'GiB', 'TiB', 'PiB'];

export function humanizeBytes(bytes: number): string {
  if (bytes === 0) return '0 B';
  const i = Math.floor(Math.log(bytes) / Math.log(1024));
  const value = bytes / Math.pow(1024, i);
  return `${value.toFixed(i === 0 ? 0 : 1)} ${UNITS[i]}`;
}

export function gbToBytes(gb: number): number {
  return gb * 1024 * 1024 * 1024;
}

export function formatDuration(durationStr: string): string {
  // Parse Go duration strings like "1h30m45s", "15m", "2.5s"
  const re = /(?:(\d+)h)?(?:(\d+)m)?(?:([\d.]+)s)?/;
  const match = durationStr.match(re);
  if (!match) return durationStr;

  const hours = parseInt(match[1] || '0', 10);
  const minutes = parseInt(match[2] || '0', 10);
  const seconds = parseFloat(match[3] || '0');

  const parts: string[] = [];
  if (hours > 0) parts.push(`${hours}h`);
  if (minutes > 0) parts.push(`${minutes}m`);
  if (seconds > 0) parts.push(`${Math.round(seconds)}s`);

  return parts.join(' ') || '0s';
}

export function formatAge(timestamp: string | undefined): string {
  if (!timestamp) return '-';
  const diff = Date.now() - new Date(timestamp).getTime();
  const seconds = Math.floor(diff / 1000);
  if (seconds < 60) return `${seconds}s`;
  const minutes = Math.floor(seconds / 60);
  if (minutes < 60) return `${minutes}m`;
  const hours = Math.floor(minutes / 60);
  if (hours < 24) return `${hours}h`;
  const days = Math.floor(hours / 24);
  return `${days}d`;
}

export function utilizationPercent(allocated: number, total: number): number {
  if (total === 0) return 0;
  return Math.round((allocated / total) * 100);
}

type PhaseColorMapping = {
  green: string[];
  blue: string[];
  orange: string[];
  red: string[];
  grey: string[];
};

const PHASE_COLORS: PhaseColorMapping = {
  green: ['Ready', 'Bound', 'Complete', 'Active', 'stable'],
  blue: ['Initializing', 'Creating', 'Cloning', 'Expanding', 'InProgress', 'Pending', 'Syncing', 'creating'],
  orange: ['Degraded', 'PartialFailure', 'Paused', 'Retained', 'Archived', 'draining', 'degraded'],
  red: ['Failed', 'Full', 'deleting'],
  grey: ['Deleting'],
};

export type BadgeColor = 'green' | 'blue' | 'orange' | 'red' | 'grey';

export function phaseBadgeColor(phase: string | undefined): BadgeColor {
  if (!phase) return 'grey';
  for (const [color, phases] of Object.entries(PHASE_COLORS)) {
    if (phases.includes(phase)) return color as BadgeColor;
  }
  return 'grey';
}

export function phaseToPatternFlyStatus(
  phase: string | undefined,
): 'success' | 'info' | 'warning' | 'danger' | 'custom' {
  const color = phaseBadgeColor(phase);
  switch (color) {
    case 'green':
      return 'success';
    case 'blue':
      return 'info';
    case 'orange':
      return 'warning';
    case 'red':
      return 'danger';
    default:
      return 'custom';
  }
}
