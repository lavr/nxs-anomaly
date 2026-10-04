import type { I18nContextValue } from '../i18n/I18nProvider';
import type { Page } from './types';

/** Listings count exactly up to this many rows (the server's ListTotalCap). */
export const LIST_TOTAL_CAP = 10_000;

type Plural = I18nContextValue['plural'];
type PageTotal = Pick<Page<unknown>, 'total' | 'total_estimated' | 'total_lower_bound'>;

/**
 * "128 alerts", "≈ 1163000 alerts" or "10000+ alerts". Past the cap the server
 * estimates; when its estimate is no better than the cap it answers 10 001,
 * a lower bound, and "≈ 10 001" would claim a precision nobody has.
 */
export function pageTotalLabel(plural: Plural, key: Parameters<Plural>[0], page: PageTotal | undefined): string {
  if (page?.total_lower_bound) {
    // The grammatical form follows the number; the text shows the bound.
    return plural(key, LIST_TOTAL_CAP, { count: `${LIST_TOTAL_CAP}+` });
  }
  const label = plural(key, page?.total ?? 0);
  return page?.total_estimated ? `≈ ${label}` : label;
}
