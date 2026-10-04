import { describe, expect, it } from 'vitest';
import { pageTotalLabel } from './pageTotal';

const plural = ((_key: string, count: number, params?: Record<string, string | number>) =>
  `${params?.count ?? count} ${count === 1 ? 'alert' : 'alerts'}`) as Parameters<typeof pageTotalLabel>[0];

describe('pageTotalLabel', () => {
  it('shows an exact count as is', () => {
    expect(pageTotalLabel(plural, 'alerts.total', { total: 128 })).toBe('128 alerts');
  });
  it('marks an estimate', () => {
    expect(pageTotalLabel(plural, 'alerts.total', { total: 1163000, total_estimated: true })).toBe('≈ 1163000 alerts');
  });
  it('shows a lower bound as 10000+, not ≈ 10001', () => {
    expect(
      pageTotalLabel(plural, 'alerts.total', { total: 10001, total_estimated: true, total_lower_bound: true }),
    ).toBe('10000+ alerts');
  });
  it('is empty-safe while loading', () => {
    expect(pageTotalLabel(plural, 'alerts.total', undefined)).toBe('0 alerts');
  });
});
