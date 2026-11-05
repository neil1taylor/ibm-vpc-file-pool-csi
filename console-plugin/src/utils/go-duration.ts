// A leaf module: it imports nothing, so any helper can use it without an import cycle.

export const MIN_MS = 60 * 1000;
export const HOUR_MS = 60 * MIN_MS;

const UNITS_MS: Record<string, number> = { h: HOUR_MS, m: MIN_MS, s: 1000, ms: 1 };

/** A Go duration such as "6h", "1h30m" or "1.5h", in milliseconds. Undefined if it is not one. */
export function parseGoDuration(value: string): number | undefined {
  if (!/^(\d+(\.\d+)?(h|ms|m|s))+$/.test(value)) return undefined;
  let total = 0;
  for (const [, amount, unit] of value.matchAll(/(\d+(?:\.\d+)?)(h|ms|m|s)/g)) {
    total += parseFloat(amount) * UNITS_MS[unit];
  }
  return Math.round(total);
}
