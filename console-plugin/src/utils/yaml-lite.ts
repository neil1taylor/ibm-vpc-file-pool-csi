/**
 * A small YAML writer for the objects the console shows to an administrator (plain objects, lists, strings,
 * numbers and booleans). It adds no dependency. A string that YAML could read as another type is quoted.
 */
const RESERVED = /^(true|false|yes|no|on|off|y|n|null|~)$/i;
const NEEDS_QUOTES = /^$|^[-?:,[\]{}#&*!|>'"%@`\s]|[:#]\s|[:#]$|\s$|^\d|^[+-]?\.?\d/;

function scalar(value: unknown): string {
  if (typeof value === 'string') {
    return RESERVED.test(value) || NEEDS_QUOTES.test(value) || /: /.test(value) ? JSON.stringify(value) : value;
  }
  return String(value);
}

function key(k: string): string {
  return RESERVED.test(k) ? JSON.stringify(k) : k;
}

function isEmpty(v: unknown): boolean {
  return Array.isArray(v) ? v.length === 0 : typeof v === 'object' && v !== null && Object.keys(v).length === 0;
}

function lines(value: unknown, indent: number): string[] {
  const pad = ' '.repeat(indent);
  const out: string[] = [];
  if (Array.isArray(value)) {
    for (const item of value) {
      if (typeof item === 'object' && item !== null && !isEmpty(item)) {
        const inner = lines(item, indent + 2);
        out.push(`${pad}- ${inner[0].slice(indent + 2)}`, ...inner.slice(1));
      } else {
        out.push(`${pad}- ${isEmpty(item) ? (Array.isArray(item) ? '[]' : '{}') : scalar(item)}`);
      }
    }
    return out;
  }
  for (const [k, v] of Object.entries(value as Record<string, unknown>)) {
    if (v === undefined) continue;
    if (typeof v === 'object' && v !== null) {
      if (isEmpty(v)) {
        out.push(`${pad}${key(k)}: ${Array.isArray(v) ? '[]' : '{}'}`);
      } else if (Array.isArray(v)) {
        out.push(`${pad}${key(k)}:`, ...lines(v, indent + 2));
      } else {
        out.push(`${pad}${key(k)}:`, ...lines(v, indent + 2));
      }
    } else {
      out.push(`${pad}${key(k)}: ${scalar(v)}`);
    }
  }
  return out;
}

export function toYAML(value: unknown): string {
  return `${lines(value, 0).join('\n')}\n`;
}
