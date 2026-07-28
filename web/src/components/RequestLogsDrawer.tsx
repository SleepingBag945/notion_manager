import { useCallback, useEffect, useState } from 'react'
import { useTranslation } from 'react-i18next'
import { fetchRequestLogs } from '../api'
import type { RequestLogEntry } from '../types'
import { formatTokens } from '../utils'
import { IconActivity, IconClose, IconRotate, IconSpinner } from './Icons'

interface Props {
  open: boolean
  onClose: () => void
}

export function RequestLogsDrawer({ open, onClose }: Props) {
  const { t } = useTranslation()
  const [logs, setLogs] = useState<RequestLogEntry[]>([])
  const [total, setTotal] = useState(0)
  const [loading, setLoading] = useState(false)
  const [error, setError] = useState<string | null>(null)

  const reload = useCallback(async () => {
    setLoading(true)
    setError(null)
    try {
      const resp = await fetchRequestLogs()
      const list = Array.isArray(resp.logs) ? resp.logs : []
      setLogs(list)
      setTotal(resp.total ?? list.length)
    } catch (e: any) {
      setError(e?.message ?? t('request_logs.load_failed'))
    } finally {
      setLoading(false)
    }
  }, [t])

  useEffect(() => {
    if (!open) return
    reload()
  }, [open, reload])

  useEffect(() => {
    if (!open) return
    const onKey = (e: KeyboardEvent) => {
      if (e.key === 'Escape') onClose()
    }
    window.addEventListener('keydown', onKey)
    return () => window.removeEventListener('keydown', onKey)
  }, [open, onClose])

  if (!open) return null

  return (
    <div className="fixed inset-0 z-[90] flex justify-end" onClick={onClose}>
      <div className="absolute inset-0 bg-black/50 backdrop-blur-sm" />
      <div
        className="relative w-full max-w-[640px] h-full bg-bg-secondary border-l border-border shadow-2xl flex flex-col"
        onClick={(e) => e.stopPropagation()}
      >
        <div className="flex items-center justify-between px-5 py-3.5 border-b border-border">
          <div className="flex items-center gap-2 text-text-primary">
            <IconActivity size={16} />
            <span className="text-[14px] font-semibold tracking-tight">{t('request_logs.title')}</span>
            {!loading && (
              <span className="text-[11px] text-text-muted font-normal">({total})</span>
            )}
          </div>
          <div className="flex items-center gap-1">
            <button
              onClick={reload}
              disabled={loading}
              className="text-text-secondary hover:text-text-primary bg-transparent border-none cursor-pointer p-1 flex items-center disabled:opacity-50"
              title={t('request_logs.refresh')}
            >
              <IconRotate size={14} className={loading ? 'animate-spin' : ''} />
            </button>
            <button
              onClick={onClose}
              className="text-text-secondary hover:text-text-primary bg-transparent border-none cursor-pointer p-1 flex items-center"
              title={t('request_logs.close')}
            >
              <IconClose size={16} />
            </button>
          </div>
        </div>

        <div className="flex-1 overflow-auto">
          {error && (
            <div className="m-4 px-3 py-2 bg-err/10 border border-err/30 rounded-md text-[12px] text-err">{error}</div>
          )}
          {loading && logs.length === 0 && !error && (
            <div className="text-center py-16 text-text-secondary text-[13px] inline-flex items-center justify-center gap-2 w-full">
              <IconSpinner size={13} className="animate-spin" /> {t('request_logs.loading')}
            </div>
          )}
          {!loading && !error && logs.length === 0 && (
            <div className="text-center py-16 text-text-secondary text-[13px]">{t('request_logs.empty')}</div>
          )}
          {logs.length > 0 && (
            <div className="divide-y divide-white/[.05]">
              {logs.map((log, i) => (
                <RequestLogRow key={`${log.timestamp}-${log.account}-${log.model}-${i}`} log={log} />
              ))}
            </div>
          )}
        </div>
      </div>
    </div>
  )
}

function RequestLogRow({ log }: { log: RequestLogEntry }) {
  const { t, i18n } = useTranslation()
  const dateLocale = i18n.language === 'zh' ? 'zh-CN' : 'en-US'
  const timestamp = formatLogTime(log.timestamp, dateLocale)
  const total = log.total_tokens ?? ((log.prompt_tokens || 0) + (log.completion_tokens || 0))

  return (
    <div className="px-4 py-3">
      <div className="flex items-start justify-between gap-3">
        <div className="min-w-0 flex-1">
          <div className="flex items-center gap-2 flex-wrap">
            <span className="text-[12px] text-text-primary font-mono tabular-nums">{timestamp}</span>
            <span className="text-[10px] uppercase tracking-wider px-1.5 py-0.5 rounded bg-white/[.06] text-text-secondary">
              {log.model || t('request_logs.unknown_model')}
            </span>
          </div>
          <div className="mt-1 text-[11px] text-text-secondary truncate">
            {t('request_logs.account')}: <span className="font-mono text-text-primary">{log.account || '—'}</span>
          </div>
        </div>
        <div className="text-right shrink-0">
          <div className="text-[12px] text-notion-blue font-semibold tabular-nums">{formatTokens(total)}</div>
          <div className="text-[10px] text-text-muted tabular-nums mt-0.5">
            {t('request_logs.tokens_detail', {
              prompt: formatTokens(log.prompt_tokens || 0),
              completion: formatTokens(log.completion_tokens || 0),
            })}
          </div>
        </div>
      </div>
    </div>
  )
}

function formatLogTime(raw: string, locale: string): string {
  if (!raw) return '—'
  try {
    const d = new Date(raw)
    if (Number.isNaN(d.getTime())) return raw
    return d.toLocaleString(locale, {
      month: 'numeric',
      day: 'numeric',
      hour: '2-digit',
      minute: '2-digit',
      second: '2-digit',
      hour12: false,
    })
  } catch {
    return raw
  }
}
