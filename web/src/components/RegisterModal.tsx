import { useState, useEffect, useMemo, useCallback } from 'react'
import { useTranslation } from 'react-i18next'
import type { ProviderInfo, RegisterStep } from '../types'
import { fetchSettings, listProviders, startRegisterJob } from '../api'
import { useJobStream } from '../hooks/useJobStream'
import {
  IconAlert,
  IconCheck,
  IconChevronRight,
  IconClose,
  IconPlay,
  IconSpinner,
  IconUserPlus,
  IconX,
} from './Icons'

interface Props {
  open: boolean
  onClose: () => void
  onJobFinished: () => void // parent should refresh the account list
}

const MS_PLACEHOLDER = `每行一个账号，使用四个连字符分隔字段：
email----password----client_id----refresh_token

example@hotmail.com----P@ssw0rd----abcd-1234----0.AYI...`

// FUTURE_PROVIDERS lists OAuth integrations we plan to ship but haven't yet.
// They render in the tab bar as disabled chips so users can see what's
// coming and we can land the UI without waiting for backend work.
const FUTURE_PROVIDERS: ProviderInfo[] = [
  { id: 'google', display: 'Google', format_hint: '', recommended_concurrency: 1, enabled: false },
  { id: 'github', display: 'GitHub', format_hint: '', recommended_concurrency: 1, enabled: false },
]

export function RegisterModal({ open, onClose, onJobFinished }: Props) {
  const { t } = useTranslation()
  const [input, setInput] = useState('')
  const [concurrency, setConcurrency] = useState(1)
  const [proxy, setProxy] = useState('')
  const [submitError, setSubmitError] = useState<string | null>(null)
  const [submitting, setSubmitting] = useState(false)
  const [jobId, setJobId] = useState<string | null>(null)
  const [providers, setProviders] = useState<ProviderInfo[]>([])
  const [selectedProvider, setSelectedProvider] = useState<string>('microsoft')
  // globalProxy mirrors AppConfig.Proxy.NotionProxy so the modal can hint
  // "blank means we'll fall back to <global>" without forcing the user
  // to retype the URL. Fetched lazily on open; falls back to "" on error.
  const [globalProxy, setGlobalProxy] = useState<string>('')
  const stream = useJobStream(open ? jobId : null)
  const job = stream.job

  // Load enabled providers when the modal opens. The dashboard call doubles
  // as a sanity check that the bundled binary actually exposes the registry
  // endpoint; failures fall back silently to the hardcoded Microsoft tab so
  // the UI is always usable.
  useEffect(() => {
    if (!open) return
    let cancelled = false
    listProviders()
      .then((list) => {
        if (cancelled) return
        if (Array.isArray(list) && list.length > 0) {
          setProviders(list)
          if (!list.some((p) => p.id === selectedProvider)) {
            setSelectedProvider(list[0].id)
          }
        }
      })
      .catch(() => { /* keep fallback */ })
    fetchSettings()
      .then((s) => { if (!cancelled) setGlobalProxy(s.notion_proxy ?? '') })
      .catch(() => { /* leave empty; modal still works */ })
    return () => { cancelled = true }
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [open])

  const enabledProviders = providers.length > 0 ? providers : [
    { id: 'microsoft', display: 'Microsoft', format_hint: '', recommended_concurrency: 1, enabled: true },
  ]
  const activeProvider = enabledProviders.find((p) => p.id === selectedProvider) ?? enabledProviders[0]
  const allTabs: ProviderInfo[] = [...enabledProviders, ...FUTURE_PROVIDERS.filter((f) => !enabledProviders.some((p) => p.id === f.id))]

  // Notify parent when the active job moves to a terminal state so it can
  // refresh the account list.
  useEffect(() => {
    if (job && job.state !== 'running') {
      onJobFinished()
    }
  }, [job?.state, onJobFinished])

  const reset = useCallback(() => {
    setInput('')
    setProxy('')
    setSubmitError(null)
    setSubmitting(false)
    setJobId(null)
  }, [])

  // Esc closes the modal; only when not actively submitting.
  useEffect(() => {
    if (!open) return
    const onKey = (e: KeyboardEvent) => {
      if (e.key === 'Escape' && !submitting) {
        onClose()
      }
    }
    window.addEventListener('keydown', onKey)
    return () => window.removeEventListener('keydown', onKey)
  }, [open, submitting, onClose])

  // Reset transient state when modal closes.
  useEffect(() => {
    if (!open) {
      const t = setTimeout(reset, 250)
      return () => clearTimeout(t)
    }
  }, [open, reset])

  const handleSubmit = async () => {
    setSubmitError(null)
    if (!activeProvider) {
      setSubmitError(t('modal.register.no_provider'))
      return
    }
    if (!input.trim()) {
      setSubmitError(t('modal.register.paste_credentials_warning'))
      return
    }
    const trimmedProxy = proxy.trim()
    if (trimmedProxy && !/^(https?|socks5h?):\/\//i.test(trimmedProxy)) {
      setSubmitError(t('modal.register.proxy_url_invalid'))
      return
    }
    setSubmitting(true)
    try {
      const resp = await startRegisterJob(activeProvider.id, input, concurrency, trimmedProxy || undefined)
      setJobId(resp.job_id)
    } catch (e: any) {
      setSubmitError(e?.message ?? t('modal.register.submit_failed'))
    } finally {
      setSubmitting(false)
    }
  }

  if (!open) return null

  return (
    <div
      className="fixed inset-0 z-[100] flex items-center justify-center bg-black/60 backdrop-blur-sm px-4"
      onClick={onClose}
    >
      <div
        className="w-full max-w-[800px] max-h-[90vh] bg-bg-secondary border border-border rounded-xl shadow-2xl flex flex-col overflow-hidden"
        onClick={(e) => e.stopPropagation()}
      >
        {/* Header */}
        <div className="flex items-center justify-between px-5 py-3.5 border-b border-border">
          <div className="flex items-center gap-2 text-text-primary">
            <IconUserPlus size={16} />
            <span className="text-[14px] font-semibold tracking-tight">{t('modal.register.title')}</span>
          </div>
          <button
            onClick={onClose}
            className="text-text-secondary hover:text-text-primary bg-transparent border-none cursor-pointer p-1 flex items-center"
            title={t('modal.register.close')}
          >
            <IconClose size={16} />
          </button>
        </div>

        {/* Body */}
        <div className="flex-1 overflow-auto">
          {!job ? (
            <>
              <ProviderTabs
                tabs={allTabs}
                selected={activeProvider?.id ?? 'microsoft'}
                onSelect={setSelectedProvider}
              />
              <InputForm
                provider={activeProvider}
                input={input}
                onInput={setInput}
                concurrency={concurrency}
                onConcurrency={setConcurrency}
                proxy={proxy}
                onProxy={setProxy}
                globalProxy={globalProxy}
                submitError={submitError}
              />
            </>
          ) : (
            <ProgressPanel job={job} streamConnected={stream.connected} streamError={stream.error} />
          )}
        </div>

        {/* Footer */}
        <div className="flex items-center justify-between gap-2 px-5 py-3 border-t border-border bg-bg-secondary">
          {!job ? (
            <>
              <span className="text-[11px] text-text-muted">{t('modal.register.modal_close_note')}</span>
              <div className="flex gap-2">
                <button
                  onClick={onClose}
                  className="px-3 py-1.5 bg-transparent text-text-secondary hover:text-text-primary rounded-md text-[13px] cursor-pointer border border-border"
                >
                  {t('modal.register.cancel')}
                </button>
                <button
                  onClick={handleSubmit}
                  disabled={submitting || !input.trim()}
                  className="inline-flex items-center gap-1.5 px-4 py-1.5 bg-white hover:bg-white/90 text-[#111] rounded-md text-[13px] font-medium cursor-pointer transition-colors border-none disabled:opacity-40 disabled:cursor-not-allowed"
                >
                  {submitting ? <IconSpinner size={13} className="animate-spin" /> : <IconPlay size={13} />}
                  {t('modal.register.start_register')}
                </button>
              </div>
            </>
          ) : (
            <>
              <span className="text-[11px] text-text-muted tabular-nums">
                {job.state === 'running' ? t('modal.register.running_in_background') : t('modal.register.completed')}
              </span>
              <button
                onClick={onClose}
                className="px-4 py-1.5 bg-white hover:bg-white/90 text-[#111] rounded-md text-[13px] font-medium cursor-pointer border-none"
              >
                {job.state === 'running' ? t('modal.register.run_in_background') : t('modal.register.complete')}
              </button>
            </>
          )}
        </div>
      </div>
    </div>
  )
}

function ProviderTabs({
  tabs,
  selected,
  onSelect,
}: {
  tabs: ProviderInfo[]
  selected: string
  onSelect: (id: string) => void
}) {
  const { t } = useTranslation()
  return (
    <div className="px-5 pt-4 border-b border-border">
      <div className="flex items-end gap-1">
        {tabs.map((p) => {
          const isSelected = p.id === selected
          const isEnabled = p.enabled
          const baseCls =
            'px-3 py-2 text-[12px] font-medium border-none bg-transparent border-b-2 transition-colors cursor-pointer'
          const cls = isSelected
            ? 'border-white text-text-primary'
            : isEnabled
            ? 'border-transparent text-text-secondary hover:text-text-primary'
            : 'border-transparent text-text-muted cursor-not-allowed opacity-50'
          return (
            <button
              key={p.id}
              type="button"
              disabled={!isEnabled}
              onClick={() => isEnabled && onSelect(p.id)}
              className={`${baseCls} ${cls}`}
              title={isEnabled ? p.display : `${p.display}（${t('modal.register.coming_soon')}）`}
            >
              {p.display}
              {!isEnabled && <span className="ml-1 text-[10px] tracking-tight">{t('modal.register.coming_soon')}</span>}
            </button>
          )
        })}
      </div>
    </div>
  )
}

function InputForm({
  provider,
  input,
  onInput,
  concurrency,
  onConcurrency,
  proxy,
  onProxy,
  globalProxy,
  submitError,
}: {
  provider: ProviderInfo | undefined
  input: string
  onInput: (v: string) => void
  concurrency: number
  onConcurrency: (n: number) => void
  proxy: string
  onProxy: (v: string) => void
  globalProxy: string
  submitError: string | null
}) {
  const { t } = useTranslation()
  const lineCount = useMemo(() => {
    const n = input.split(/\r?\n/).filter((l) => l.trim().length > 0).length
    return n
  }, [input])

  const placeholder = provider?.format_hint?.trim()
    ? `${provider.format_hint}\n\nexample@hotmail.com----P@ssw0rd----abcd-1234----0.AYI...`
    : t('modal.register.credentials_placeholder')

  return (
    <div className="p-5 space-y-4">
      <div>
        <label className="block text-[12px] text-text-secondary mb-1.5 font-medium">
          {provider ? t('modal.register.credentials', { provider: provider.display }) : t('modal.register.credentials_generic')}
        </label>
        <textarea
          value={input}
          onChange={(e) => onInput(e.target.value)}
          placeholder={placeholder}
          rows={12}
          className="w-full bg-bg-input border border-border rounded-md px-3 py-2 text-[12px] text-text-primary outline-none focus:border-white/20 transition-colors placeholder:text-text-muted font-mono leading-relaxed resize-y"
          spellCheck={false}
        />
        <div className="flex justify-between items-center mt-1">
          <span className="text-[11px] text-text-muted">{t('modal.register.rows_pending', { count: lineCount })}</span>
        </div>
      </div>

      <div className="flex items-center gap-3">
        <label className="text-[12px] text-text-secondary font-medium">{t('modal.register.concurrency')}</label>
        <input
          type="number"
          min={1}
          value={concurrency}
          onChange={(e) => {
            const n = parseInt(e.target.value, 10)
            onConcurrency(Number.isFinite(n) && n > 0 ? n : 1)
          }}
          className="w-20 bg-bg-input border border-border rounded-md px-2 py-1 text-[13px] text-text-primary outline-none focus:border-white/20 transition-colors text-center tabular-nums"
        />
        <span className="inline-flex items-center gap-1 text-[11px] text-text-muted">
          <IconAlert size={12} className="text-warn" />
          {t('modal.register.risk_advice')}
        </span>
      </div>

      <div>
        <label className="block text-[12px] text-text-secondary mb-1.5 font-medium">
          {t('modal.register.proxy_url')}
        </label>
        <input
          type="text"
          value={proxy}
          onChange={(e) => onProxy(e.target.value)}
          placeholder={
            globalProxy
              ? t('modal.register.proxy_placeholder_global', { url: globalProxy })
              : t('modal.register.proxy_placeholder_direct')
          }
          className="w-full bg-bg-input border border-border rounded-md px-3 py-1.5 text-[12px] text-text-primary outline-none focus:border-white/20 transition-colors placeholder:text-text-muted font-mono"
          spellCheck={false}
          autoComplete="off"
        />
        <div className="text-[11px] text-text-muted mt-1">
          {globalProxy ? t('modal.register.proxy_desc_fallback_global') : t('modal.register.proxy_desc_fallback_direct')}
        </div>
      </div>

      {submitError && (
        <div className="px-3 py-2 bg-err/10 border border-err/30 rounded-md text-[12px] text-err">
          {submitError}
        </div>
      )}
    </div>
  )
}

function ProgressPanel({
  job,
  streamConnected,
  streamError,
}: {
  job: ReturnType<typeof useJobStream>['job']
  streamConnected: boolean
  streamError: string | null
}) {
  const { t } = useTranslation()
  if (!job) return null
  const total = job.total
  const done = job.done
  const ok = job.ok
  const fail = job.fail
  const pct = total > 0 ? Math.round((done / total) * 100) : 0

  return (
    <div className="p-5 space-y-3">
      <div className="space-y-2">
        <div className="flex justify-between items-center">
          <div className="flex items-center gap-2">
            <span className="text-[13px] font-semibold tracking-tight">
              {job.state === 'running' ? t('modal.register.registering') : job.state === 'cancelled' ? t('modal.register.cancelled') : t('modal.register.register_completed')}
            </span>
            {job.state === 'running' && <IconSpinner size={13} className="animate-spin text-text-muted" />}
          </div>
          <span className="text-[12px] text-text-muted tabular-nums">
            {t('modal.register.progress_info', { current: done, total, ok, failed: fail, concurrency: job.concurrency })}
          </span>
        </div>
        <div className="h-1.5 bg-white/[.06] rounded-full overflow-hidden">
          <div
            className={`h-full rounded-full transition-all duration-500 ${job.state === 'running' ? 'bg-notion-blue' : fail > 0 ? 'bg-warn' : 'bg-ok'}`}
            style={{ width: `${pct}%` }}
          />
        </div>
        {!streamConnected && job.state === 'running' && (
          <div className="text-[11px] text-warn flex items-center gap-1">
            <IconAlert size={11} /> {t('modal.register.stream_disconnected')} {streamError && `(${streamError})`}
          </div>
        )}
      </div>

      <div className="border border-border rounded-md divide-y divide-white/[.05] max-h-[420px] overflow-auto">
        {job.steps.map((s, i) => (
          <StepRow key={`${s.email}-${i}`} step={s} index={i} />
        ))}
      </div>
    </div>
  )
}

function StepRow({ step, index }: { step: RegisterStep; index: number }) {
  const { t } = useTranslation()
  const [expanded, setExpanded] = useState(false)
  const startedAt = step.started_at
  const endedAt = step.ended_at
  const durationMs = startedAt && endedAt ? endedAt - startedAt : null

  let statusEl: React.ReactNode
  let bg = ''
  switch (step.status) {
    case 'pending':
      statusEl = <span className="text-text-muted text-[11px]">{t('modal.register.step_wait')}</span>
      break
    case 'running':
      statusEl = (
        <span className="inline-flex items-center gap-1 text-[11px] text-notion-blue">
          <IconSpinner size={11} className="animate-spin" /> {t('modal.register.step_running')}
        </span>
      )
      break
    case 'ok':
      statusEl = (
        <span className="inline-flex items-center gap-1 text-[11px] text-ok">
          <IconCheck size={11} /> {t('modal.register.step_success')}
        </span>
      )
      break
    case 'fail':
      statusEl = (
        <span className="inline-flex items-center gap-1 text-[11px] text-err">
          <IconX size={11} /> {t('modal.register.step_failed')}
        </span>
      )
      bg = 'bg-err/[.04]'
      break
  }

  return (
    <div className={`px-3 py-2 ${bg}`}>
      <div className="flex items-center gap-2">
        <span className="text-[10px] text-text-muted w-7 tabular-nums shrink-0">#{index + 1}</span>
        <span className="text-[12px] text-text-primary font-mono truncate flex-1 min-w-0">{step.email || '—'}</span>
        {durationMs != null && <span className="text-[10px] text-text-muted tabular-nums shrink-0">{(durationMs / 1000).toFixed(1)}s</span>}
        <span className="shrink-0">{statusEl}</span>
      </div>
      {step.status === 'fail' && step.message && (
        <div className="mt-1 ml-9">
          <button
            onClick={() => setExpanded((v) => !v)}
            className="text-[11px] text-text-secondary hover:text-text-primary inline-flex items-center gap-1 bg-transparent border-none p-0 cursor-pointer"
          >
            <IconChevronRight size={11} className={expanded ? 'rotate-90 transition-transform' : 'transition-transform'} />
            {expanded ? t('modal.register.collapse') : t('modal.register.view_details')}
          </button>
          {expanded && (
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
