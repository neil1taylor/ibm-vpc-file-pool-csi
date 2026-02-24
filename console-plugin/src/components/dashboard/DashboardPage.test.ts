import { formatLatencyMs, latencyColor } from './DashboardPage';

describe('formatLatencyMs', () => {
  it('returns "-" for 0', () => {
    expect(formatLatencyMs(0)).toBe('-');
  });

  it('returns "<1 ms" for very small values', () => {
    expect(formatLatencyMs(0.0005)).toBe('<1 ms');
  });

  it('formats normal values as milliseconds', () => {
    expect(formatLatencyMs(0.1)).toBe('100 ms');
    expect(formatLatencyMs(0.25)).toBe('250 ms');
    expect(formatLatencyMs(1)).toBe('1000 ms');
  });

  it('rounds to nearest integer', () => {
    expect(formatLatencyMs(0.1234)).toBe('123 ms');
  });
});

describe('latencyColor', () => {
  it('returns grey for 0', () => {
    expect(latencyColor(0)).toBe('grey');
  });

  it('returns green for <100ms', () => {
    expect(latencyColor(0.05)).toBe('green'); // 50ms
    expect(latencyColor(0.099)).toBe('green'); // 99ms
  });

  it('returns gold for 100ms-499ms', () => {
    expect(latencyColor(0.1)).toBe('gold'); // 100ms
    expect(latencyColor(0.3)).toBe('gold'); // 300ms
    expect(latencyColor(0.499)).toBe('gold'); // 499ms
  });

  it('returns red for >=500ms', () => {
    expect(latencyColor(0.5)).toBe('red'); // 500ms
    expect(latencyColor(1)).toBe('red'); // 1000ms
    expect(latencyColor(5)).toBe('red'); // 5000ms
  });
});
