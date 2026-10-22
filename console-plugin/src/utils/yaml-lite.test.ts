import { toYAML } from './yaml-lite';

describe('toYAML', () => {
  it('writes nested objects, lists and scalars', () => {
    const text = toYAML({
      apiVersion: 'v1',
      kind: 'Thing',
      metadata: { name: 'a' },
      spec: { list: ['x', 'z'], count: 3, on: true, items: [{ k: 'v', w: 1 }] },
    });
    expect(text).toBe(
      [
        'apiVersion: v1',
        'kind: Thing',
        'metadata:',
        '  name: a',
        'spec:',
        '  list:',
        '    - x',
        '    - z',
        '  count: 3',
        '  "on": true',
        '  items:',
        '    - k: v',
        '      w: 1',
        '',
      ].join('\n'),
    );
  });

  it('quotes strings that would be read as something else', () => {
    const text = toYAML({ a: 'true', b: '12', c: 'a: b', d: '', e: '2026-10-20T09:00:00Z', f: '#x' });
    expect(text).toContain('a: "true"');
    expect(text).toContain('b: "12"');
    expect(text).toContain('c: "a: b"');
    expect(text).toContain('d: ""');
    expect(text).toContain('e: "2026-10-20T09:00:00Z"');
    expect(text).toContain('f: "#x"');
  });

  it('writes empty lists and objects inline and skips undefined values', () => {
    expect(toYAML({ a: [], b: {}, c: undefined })).toBe('a: []\nb: {}\n');
  });
});
