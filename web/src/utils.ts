export function avatarColor(_name: string): string {
  return 'rgba(255, 255, 255, 0.08)'
}

export function avatarLetter(name: string): string {
  return (name || '?')[0].toUpperCase()
}

// providerDisplay maps a Provider.ID() to its short display label for
// account/history badges. Unknown IDs round-trip so the badge still tells
// the operator something meaningful.
export function providerDisplay(id: string): string {
  switch (id) {
    case 'microsoft':
      return 'Microsoft'
    case 'google':
      return 'Google'
    case 'github':
      return 'GitHub'
    default:
      return id
  }
}

export function fmt(n: number): string {
  return (n || 0).toLocaleString()
}

// formatCompactNumber renders large counters as terse strings tuned for the
// 5-column Summary StatCards (e.g. 1234 -> "1.23K", 1500000 -> "1.5M").
// Smaller values fall back to the locale-aware fmt() so we never lose
// precision in the common low-traffic case.
export function formatCompactNumber(n: number): string {
  const v = n || 0
  if (v < 1000) return fmt(v)
  if (v < 1_000_000) return trimTrailingZeros((v / 1000).toFixed(2)) + 'K'
  if (v < 1_000_000_000) return trimTrailingZeros((v / 1_000_000).toFixed(2)) + 'M'
  return trimTrailingZeros((v / 1_000_000_000).toFixed(2)) + 'B'
}

// formatTokens renders Token counters with adaptive K/M units so the
// Summary card stays scannable across the wide value range a long-lived
// dashboard sees (a few thousand tokens during the first hour, many
// millions after a week of traffic). Anything below 1 M uses K, anything
// at or above 1 M uses M.
//
// Precision is tuned per band:
//   - 0                      -> "0 K"
//   - 0 < n < 1000           -> three decimals, trimmed, e.g. "0.523 K"
//   - 1e3 <= n < 1e5         -> two decimals, trimmed, e.g. "12.34 K"
//   - 1e5 <= n < 1e6         -> integer with thousands separator, e.g. "523 K"
//   - 1e6 <= n < 1e8         -> two decimals, trimmed, e.g. "1.23 M" / "5 M"
//   - >= 1e8                 -> integer with thousands separator, e.g. "1,234 M"
export function formatTokens(n: number): string {
  const v = n || 0
  if (v === 0) return '0 K'
  if (v < 1_000_000) {
    const k = v / 1000
    if (k >= 100) return Math.round(k).toLocaleString() + ' K'
    if (k >= 1) return trimTrailingZeros(k.toFixed(2)) + ' K'
    return trimTrailingZeros(k.toFixed(3)) + ' K'
  }
  const m = v / 1_000_000
  if (m >= 100) return Math.round(m).toLocaleString() + ' M'
  return trimTrailingZeros(m.toFixed(2)) + ' M'
}

function trimTrailingZeros(s: string): string {
  if (!s.includes('.')) return s
  return s.replace(/\.?0+$/, '')
}

export type QuotaStatus = 'ok' | 'low' | 'exhausted'

export function getQuotaStatus(exhausted: boolean, permanent: boolean, usage?: number, limit?: number): QuotaStatus {
  if (permanent || exhausted) return 'exhausted'
  if (limit && usage && usage / limit >= 0.9) return 'low'
  return 'ok'
}

export function getQuotaStatusByUsage(usage?: number, limit?: number): QuotaStatus {
  if (limit && (usage || 0) >= limit) return 'exhausted'
  if (limit && usage && usage / limit >= 0.9) return 'low'
  return 'ok'
}

export function getQuotaPct(usage?: number, limit?: number): number {
  if (!limit) return 0
  return Math.min(((usage || 0) / limit) * 100, 100)
}

import i18n from './i18n'

const getLocale = () => i18n.language === 'zh' ? 'zh-CN' : 'en-US'

export function formatCheckedAt(iso?: string): string {
  if (!iso) return '—'
  try {
    return new Date(iso).toLocaleString(getLocale(), {
      month: 'numeric', day: 'numeric', hour: '2-digit', minute: '2-digit',
    })
  } catch {
    return '—'
  }
}

export function formatTimestampMs(ms?: number): string {
  if (!ms) return '—'
  try {
    return new Date(ms).toLocaleString(getLocale(), {
      month: 'numeric', day: 'numeric', hour: '2-digit', minute: '2-digit',
    })
  } catch {
    return '—'
  }
}
