import { act, renderHook } from '@testing-library/react';
import { describe, expect, it } from 'vitest';
import { LIST_PAGE_SIZE, usePagedList } from './PagedList';

const people = Array.from({ length: 120 }, (_, i) => ({ id: `p${i}`, name: `Person ${i}`, email: i === 117 ? 'night@example.com' : '' }));

describe('usePagedList', () => {
  it('draws one page of a long list', () => {
    const { result } = renderHook(() => usePagedList(people, ['name', 'email']));
    expect(result.current.items).toHaveLength(LIST_PAGE_SIZE);
    expect(result.current.pages).toBe(3);
    act(() => result.current.setPage(3));
    expect(result.current.items.map((p) => p.id)).toEqual(people.slice(100).map((p) => p.id));
  });

  it('searches every item, not just the page on screen, and starts over at page one', () => {
    const { result } = renderHook(() => usePagedList(people, ['name', 'email']));
    act(() => result.current.setPage(2));
    act(() => result.current.setQuery('NIGHT@'));
    expect(result.current.page).toBe(1);
    expect(result.current.items.map((p) => p.id)).toEqual(['p117']);
    expect(result.current.matching).toBe(1);
    expect(result.current.total).toBe(120);
  });

  it('keeps the page in range when the list shrinks', () => {
    const { result, rerender } = renderHook(({ items }) => usePagedList(items, ['name']), { initialProps: { items: people } });
    act(() => result.current.setPage(3));
    rerender({ items: people.slice(0, 10) });
    expect(result.current.page).toBe(1);
    expect(result.current.items).toHaveLength(10);
  });
});
