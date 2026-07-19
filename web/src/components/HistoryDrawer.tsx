import { useCallback, useEffect, useState } from 'react'
import { useTranslation } from 'react-i18next'
import type { RegisterJob, RegisterStep } from '../types'
import { deleteRegisterJob, getJob, listJobs, retryRegisterJob } from '../api'
import { providerDisplay } from '../utils'
import {
  IconCheck,
  IconChevronRight,
  IconClose,
  IconHistory,
  IconRotate,
  IconSpinner,
  IconTrash,
  IconX,
} from './Icons'

interface Props {
  open: boolean
  onClose: () => void
  // onRetryStarted is invoked with the new job id when a retry kicks off so
  // the caller (App.tsx) can open the live progress modal again.
  onRetryStarted?: (jobId: string) => void
}

export function HistoryDrawer({ open, onClose, onRetryStarted }: Props) {
  const { t } = useTranslation()
  const [jobs, setJobs] = useState<RegisterJob[]>([])
  const [loading, setLoading] = useState(false)
  const [error, setError] = useState<string | null>(null)
  const [expandedId, setExpandedId] = useState<string | null>(null)
  const [details, setDetails] = useState<Record<string, RegisterJob | undefined>>({})
  // pendingAction tracks per-job retry/delete in-flight state so we can show
  // a spinner and disable both buttons while the request is outstanding.
  const [pendingAction, setPendingAction] = useState<Record<string, 'retry' | 'delete' | undefined>>({})
  // rowError surfaces backend rejections (e.g. 410 Gone for sidecar-less
  // retries) inline with the row instead of swallowing them.
  const [rowError, setRowError] = useState<Record<string, string | undefined>>({})

  const reload = useCallback(async () => {
    setLoading(true)
    setError(null)
    try {
      const list = await listJobs(50)
      setJobs(list)
    } catch (e: any) {
      setError(e?.message ?? t('history.load_failed'))
    } finally {
      setLoading(false)
    }
  }, [])

  useEffect(() => {
    if (!open) return
    reload()
  }, [open, reload])

  // Auto-poll while any job is still running so retry/start jobs show live
  // OK/Fail counters without forcing a manual refresh. Cheap: hits the
  // already-cached in-memory list and only stays alive while the drawer is
  // open AND there's actually a running job.
  useEffect(() => {
    if (!open) return
    const hasRunning = jobs.some((j) => j.state === 'running')
    if (!hasRunning) return
    const t = setInterval(() => { reload() }, 2000)
    return () => clearInterval(t)
  }, [open, jobs, reload])

  useEffect(() => {
    if (!open) return
    const onKey = (e: KeyboardEvent) => {
      if (e.key === 'Escape') onClose()
    }
    window.addEventListener('keydown', onKey)
    return () => window.removeEventListener('keydown', onKey)
  }, [open, onClose])

  const toggleExpand = async (id: string) => {
    if (expandedId === id) {
      setExpandedId(null)
      return
    }
    setExpandedId(id)
    if (!details[id]) {
      try {
        const detail = await getJob(id)
        setDetails((prev) => ({ ...prev, [id]: detail }))
      } catch (e) {
        // ignore; UI will show "load failed" inline
      }
    }
  }

  const handleRetry = useCallback(async (id: string) => {
    setRowError((p) => ({ ...p, [id]: undefined }))
    setPendingAction((p) => ({ ...p, [id]: 'retry' }))
    try {
      const resp = await retryRegisterJob(id)
      onRetryStarted?.(resp.job_id)
      // Refresh list so the new job shows up at the top.
      reload()
    } catch (e: any) {
      setRowError((p) => ({ ...p, [id]: e?.message ?? t('history.retry_failed') }))
    } finally {
      setPendingAction((p) => ({ ...p, [id]: undefined }))
    }
  }, [reload, onRetryStarted])

  const handleDelete = useCallback(async (id: string) => {
    if (!window.confirm(t('history.confirm_delete'))) {
      return
    }
    setRowError((p) => ({ ...p, [id]: undefined }))
    setPendingAction((p) => ({ ...p, [id]: 'delete' }))
    try {
      await deleteRegisterJob(id)
      setJobs((prev) => prev.filter((j) => j.id !== id))
      setDetails((prev) => {
        const next = { ...prev }
        delete next[id]
        return next
      })
      if (expandedId === id) setExpandedId(null)
    } catch (e: any) {
      setRowError((p) => ({ ...p, [id]: e?.message ?? t('history.delete_failed') }))
    } finally {
      setPendingAction((p) => ({ ...p, [id]: undefined }))
    }
  }, [expandedId])

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
            <IconHistory size={16} />
            <span className="text-[14px] font-semibold tracking-tight">{t('history.title')}</span>
            {!loading && (
              <span className="text-[11px] text-text-muted font-normal">({jobs.length})</span>
            )}
          </div>
          <div className="flex items-center gap-1">
            <button
              onClick={reload}
              disabled={loading}
              className="text-text-secondary hover:text-text-primary bg-transparent border-none cursor-pointer p-1 flex items-center disabled:opacity-50"
              title={t('history.refresh')}
            >
              <IconRotate size={14} className={loading ? 'animate-spin' : ''} />
            </button>
            <button
              onClick={onClose}
              className="text-text-secondary hover:text-text-primary bg-transparent border-none cursor-pointer p-1 flex items-center"
              title={t('history.close')}
            >
              <IconClose size={16} />
            </button>
          </div>
        </div>

        <div className="flex-1 overflow-auto">
          {error && (
            <div className="m-4 px-3 py-2 bg-err/10 border border-err/30 rounded-md text-[12px] text-err">{error}</div>
          )}
          {!loading && !error && jobs.length === 0 && (
            <div className="text-center py-16 text-text-secondary text-[13px]">{t('history.no_tasks')}</div>
          )}
          {jobs.length > 0 && (
            <div className="divide-y divide-white/[.05]">
              {jobs.map((job) => (
                <JobRow
                  key={job.id}
                  job={job}
                  expanded={expandedId === job.id}
                  detail={details[job.id]}
                  pendingAction={pendingAction[job.id]}
                  rowError={rowError[job.id]}
                  onToggle={() => toggleExpand(job.id)}
                  onRetry={() => handleRetry(job.id)}
                  onDelete={() => handleDelete(job.id)}
                />
              ))}
            </div>
          )}
        </div>
      </div>
    </div>
  )
}

function JobRow({
  job,
  expanded,
  detail,
  pendingAction,
  rowError,
  onToggle,
  onRetry,
  onDelete,
}: {
  job: RegisterJob
  expanded: boolean
  detail: RegisterJob | undefined
  pendingAction: 'retry' | 'delete' | undefined
  rowError: string | undefined
  onToggle: () => void
  onRetry: () => void
  onDelete: () => void
}) {
  const { t, i18n } = useTranslation()
  const dateLocale = i18n.language === 'zh' ? 'zh-CN' : 'en-US'
  const created = new Date(job.created_at).toLocaleString(dateLocale, { hour12: false })
  const stateLabel =
    job.state === 'running' ? t('history.status_running') : job.state === 'cancelled' ? t('history.status_cancelled') : t('history.status_completed')
  const stateClass =
    job.state === 'running'
      ? 'text-notion-blue bg-notion-blue/10'
      : job.fail > 0
      ? 'text-warn bg-warn/10'
      : 'text-ok bg-ok/10'

  const isRunning = job.state === 'running'
  const canRetry = !isRunning && job.fail > 0 && pendingAction == null
  const canDelete = !isRunning && pendingAction == null
  const proxyShort = job.proxy ? maskProxy(job.proxy) : ''

  return (
    <div className="px-4 py-3">
      <div className="flex items-start gap-2">
        <button
          onClick={onToggle}
          className="flex-1 min-w-0 flex items-center gap-3 text-left bg-transparent border-none p-0 cursor-pointer"
        >
          <span className={`text-text-secondary transition-transform ${expanded ? 'rotate-90' : ''}`}>
            <IconChevronRight size={14} />
          </span>
          <div className="flex-1 min-w-0">
            <div className="flex items-center gap-2 flex-wrap">
              <span className="text-[12px] text-text-primary tabular-nums">{created}</span>
              <span className={`text-[10px] uppercase tracking-wider px-1.5 py-0.5 rounded ${stateClass}`}>
                {stateLabel}
              </span>
              {job.provider && (
                <span className="text-[10px] uppercase tracking-wider px-1.5 py-0.5 rounded bg-white/[.06] text-text-secondary">
                  {providerDisplay(job.provider)}
                </span>
              )}
              <span className="text-[11px] text-text-secondary tabular-nums">
                OK <span className="text-ok">{job.ok}</span> / {t('history.failed_steps', { count: job.fail })} / {job.total}
              </span>
              <span className="text-[11px] text-text-muted">{t('history.concurrency', { count: job.concurrency })}</span>
              {proxyShort && (
                <span
                  className="text-[10px] px-1.5 py-0.5 rounded bg-white/[.04] text-text-muted font-mono"
                  title={t('history.proxy', { url: job.proxy })}
                >
                  via {proxyShort}
                </span>
              )}
            </div>
          </div>
        </button>
        <div className="flex items-center gap-1 shrink-0">
          <button
            type="button"
            onClick={(e) => { e.stopPropagation(); onRetry() }}
            disabled={!canRetry}
            title={
              isRunning
                ? t('history.cannot_retry_running')
                : job.fail === 0
                ? t('history.no_failed_steps')
                : t('history.retry_failed_steps')
            }
            className="text-text-secondary hover:text-text-primary bg-transparent border-none cursor-pointer p-1 flex items-center disabled:opacity-30 disabled:cursor-not-allowed"
          >
            {pendingAction === 'retry' ? (
              <IconSpinner size={13} className="animate-spin" />
            ) : (
              <IconRotate size={13} />
            )}
          </button>
          <button
            type="button"
            onClick={(e) => { e.stopPropagation(); onDelete() }}
            disabled={!canDelete}
            title={isRunning ? t('history.cannot_delete_running') : t('history.delete_task')}
            className="text-text-secondary hover:text-err bg-transparent border-none cursor-pointer p-1 flex items-center disabled:opacity-30 disabled:cursor-not-allowed"
          >
            {pendingAction === 'delete' ? (
              <IconSpinner size={13} className="animate-spin" />
            ) : (
              <IconTrash size={13} />
            )}
          </button>
        </div>
      </div>
      {rowError && (
        <div className="ml-6 mt-1 px-2 py-1 text-[11px] text-err bg-err/10 border border-err/30 rounded">
          {rowError}
        </div>
      )}
      {expanded && (
        <div className="mt-2 ml-6 border border-border rounded-md divide-y divide-white/[.05] max-h-[420px] overflow-auto">
          {(detail || job).steps.map((s, i) => (
            <DetailStep key={`${s.email}-${i}`} step={s} index={i} />
          ))}
          {!detail && (
            <div className="px-3 py-2 text-[11px] text-text-muted flex items-center gap-1">
              <IconSpinner size={11} className="animate-spin" /> {t('history.loading_snapshot')}
            </div>
          )}
        </div>
      )}
    </div>
  )
}

// maskProxy renders only host:port from a possibly-credentialled URL so we
// don't leak passwords in the history list. The full URL is still available
// via the title tooltip for operators who need it.
function maskProxy(raw: string): string {
  try {
    const u = new URL(raw)
    return `${u.hostname}${u.port ? ':' + u.port : ''}`
  } catch {
    return raw.length > 32 ? raw.slice(0, 32) + '…' : raw
  }
}

function DetailStep({ step, index }: { step: RegisterStep; index: number }) {
  const { t } = useTranslation()
  const [open, setOpen] = useState(false)
  let icon: React.ReactNode
  let label: string
  let cls = ''
  switch (step.status) {
    case 'ok':
      icon = <IconCheck size={11} />
      label = t('history.step_success')
      cls = 'text-ok'
      break
    case 'fail':
      icon = <IconX size={11} />
      label = t('history.step_failed')
      cls = 'text-err'
      break
    case 'running':
      icon = <IconSpinner size={11} className="animate-spin" />
      label = t('history.step_running')
      cls = 'text-notion-blue'
      break
    default:
      icon = null
      label = t('history.step_wait')
      cls = 'text-text-muted'
  }
  return (
    <div className={`px-3 py-2 ${step.status === 'fail' ? 'bg-err/[.04]' : ''}`}>
      <div className="flex items-center gap-2">
        <span className="text-[10px] text-text-muted w-7 tabular-nums shrink-0">#{index + 1}</span>
        <span className="text-[12px] text-text-primary font-mono truncate flex-1 min-w-0">{step.email || '—'}</span>
        <span className={`inline-flex items-center gap-1 text-[11px] ${cls} shrink-0`}>
          {icon}
          {label}
        </span>
      </div>
      {step.status === 'fail' && step.message && (
        <div className="mt-1 ml-9">
          <button
            onClick={() => setOpen((v) => !v)}
            className="text-[11px] text-text-secondary hover:text-text-primary inline-flex items-center gap-1 bg-transparent border-none p-0 cursor-pointer"
          >
            <IconChevronRight size={11} className={open ? 'rotate-90 transition-transform' : 'transition-transform'} />
            {open ? t('modal.register.collapse') : t('modal.register.view_details')}
          </button>
          {open && (
            <pre className="mt-1 p-2 bg-bg-input border border-border rounded text-[11px] text-text-secondary whitespace-pre-wrap break-all max-h-48 overflow-auto font-mono">
              {step.message}
            </pre>
          )}
        </div>
      )}
      {step.status === 'ok' && step.file && (
        <div className="mt-0.5 ml-9 text-[10px] text-text-muted truncate">→ {step.file}</div>
      )}
    </div>
  )
}
