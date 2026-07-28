import type { DashboardData, JobStartResponse, ProviderInfo, RegisterJob, TokenStats } from './types'

// --- Auth API ---

const SHA256_INITIAL_HASH = [
  0x6a09e667, 0xbb67ae85, 0x3c6ef372, 0xa54ff53a,
  0x510e527f, 0x9b05688c, 0x1f83d9ab, 0x5be0cd19,
]

const SHA256_ROUND_CONSTANTS = [
  0x428a2f98, 0x71374491, 0xb5c0fbcf, 0xe9b5dba5,
  0x3956c25b, 0x59f111f1, 0x923f82a4, 0xab1c5ed5,
  0xd807aa98, 0x12835b01, 0x243185be, 0x550c7dc3,
  0x72be5d74, 0x80deb1fe, 0x9bdc06a7, 0xc19bf174,
  0xe49b69c1, 0xefbe4786, 0x0fc19dc6, 0x240ca1cc,
  0x2de92c6f, 0x4a7484aa, 0x5cb0a9dc, 0x76f988da,
  0x983e5152, 0xa831c66d, 0xb00327c8, 0xbf597fc7,
  0xc6e00bf3, 0xd5a79147, 0x06ca6351, 0x14292967,
  0x27b70a85, 0x2e1b2138, 0x4d2c6dfc, 0x53380d13,
  0x650a7354, 0x766a0abb, 0x81c2c92e, 0x92722c85,
  0xa2bfe8a1, 0xa81a664b, 0xc24b8b70, 0xc76c51a3,
  0xd192e819, 0xd6990624, 0xf40e3585, 0x106aa070,
  0x19a4c116, 0x1e376c08, 0x2748774c, 0x34b0bcb5,
  0x391c0cb3, 0x4ed8aa4a, 0x5b9cca4f, 0x682e6ff3,
  0x748f82ee, 0x78a5636f, 0x84c87814, 0x8cc70208,
  0x90befffa, 0xa4506ceb, 0xbef9a3f7, 0xc67178f2,
]

function rotateRight(value: number, bits: number): number {
  return (value >>> bits) | (value << (32 - bits))
}

function toHex(bytes: Uint8Array): string {
  return Array.from(bytes).map((b) => b.toString(16).padStart(2, '0')).join('')
}

// 在非安全上下文（如 Win10 WSL2 下通过局域网 IP 访问 HTTP）里，
// 浏览器可能禁用 crypto.subtle，这里提供纯前端 SHA-256 回退。
function sha256hexFallback(data: Uint8Array): string {
  const paddedLength = Math.ceil((data.length + 9) / 64) * 64
  const padded = new Uint8Array(paddedLength)
  padded.set(data)
  padded[data.length] = 0x80

  const bitLength = BigInt(data.length) * 8n
  for (let i = 0; i < 8; i += 1) {
    padded[padded.length - 1 - i] = Number((bitLength >> BigInt(i * 8)) & 0xffn)
  }

  const hash = SHA256_INITIAL_HASH.slice()
  const schedule = new Uint32Array(64)

  for (let offset = 0; offset < padded.length; offset += 64) {
    for (let i = 0; i < 16; i += 1) {
      const index = offset + i * 4
      schedule[i] = (
        (padded[index] << 24) |
        (padded[index + 1] << 16) |
        (padded[index + 2] << 8) |
        padded[index + 3]
      ) >>> 0
    }

    for (let i = 16; i < 64; i += 1) {
      const s0 = rotateRight(schedule[i - 15], 7) ^ rotateRight(schedule[i - 15], 18) ^ (schedule[i - 15] >>> 3)
      const s1 = rotateRight(schedule[i - 2], 17) ^ rotateRight(schedule[i - 2], 19) ^ (schedule[i - 2] >>> 10)
      schedule[i] = (schedule[i - 16] + s0 + schedule[i - 7] + s1) >>> 0
    }

    let [a, b, c, d, e, f, g, h] = hash

    for (let i = 0; i < 64; i += 1) {
      const s1 = rotateRight(e, 6) ^ rotateRight(e, 11) ^ rotateRight(e, 25)
      const choose = (e & f) ^ (~e & g)
      const temp1 = (h + s1 + choose + SHA256_ROUND_CONSTANTS[i] + schedule[i]) >>> 0
      const s0 = rotateRight(a, 2) ^ rotateRight(a, 13) ^ rotateRight(a, 22)
      const majority = (a & b) ^ (a & c) ^ (b & c)
      const temp2 = (s0 + majority) >>> 0

      h = g
      g = f
      f = e
      e = (d + temp1) >>> 0
      d = c
      c = b
      b = a
      a = (temp1 + temp2) >>> 0
    }

    hash[0] = (hash[0] + a) >>> 0
    hash[1] = (hash[1] + b) >>> 0
    hash[2] = (hash[2] + c) >>> 0
    hash[3] = (hash[3] + d) >>> 0
    hash[4] = (hash[4] + e) >>> 0
    hash[5] = (hash[5] + f) >>> 0
    hash[6] = (hash[6] + g) >>> 0
    hash[7] = (hash[7] + h) >>> 0
  }

  return hash.map((value) => value.toString(16).padStart(8, '0')).join('')
}

async function sha256hex(message: string): Promise<string> {
  const data = new TextEncoder().encode(message)
  if (globalThis.crypto?.subtle) {
    try {
      const hash = await globalThis.crypto.subtle.digest('SHA-256', data)
      return toHex(new Uint8Array(hash))
    } catch {
      // 某些浏览器/上下文会暴露 subtle，但实际调用仍会被安全策略拦下。
    }
  }
  return sha256hexFallback(data)
}

async function readJson<T>(resp: Response, fallbackMessage: string): Promise<T> {
  const text = await resp.text()
  if (!text) {
    throw new Error(fallbackMessage)
  }
  try {
    return JSON.parse(text) as T
  } catch {
    throw new Error(fallbackMessage)
  }
}

export interface AuthStatus {
  authenticated: boolean
  required: boolean
}

export async function checkAuth(): Promise<AuthStatus> {
  const resp = await fetch('/dashboard/auth/check', {
    headers: { Accept: 'application/json' },
    credentials: 'same-origin',
  })
  if (!resp.ok) throw new Error(`HTTP ${resp.status}`)
  return readJson<AuthStatus>(resp, '认证状态接口返回了无效响应')
}

export async function fetchSalt(): Promise<{ salt: string; required: boolean }> {
  const resp = await fetch('/dashboard/auth/salt', {
    headers: { Accept: 'application/json' },
    credentials: 'same-origin',
  })
  if (!resp.ok) throw new Error(`HTTP ${resp.status}`)
  return readJson<{ salt: string; required: boolean }>(resp, '登录校验配置返回了无效响应')
}

export async function login(password: string): Promise<{ ok: boolean; error?: string }> {
  const { salt } = await fetchSalt()
  const hash = await sha256hex(salt + password)
  const resp = await fetch('/dashboard/auth/login', {
    method: 'POST',
    headers: {
      Accept: 'application/json',
      'Content-Type': 'application/json',
    },
    credentials: 'same-origin',
    body: JSON.stringify({ hash }),
  })
  const data = await readJson<{ error?: string }>(resp, '登录接口返回了无效响应')
  if (!resp.ok) return { ok: false, error: data.error || 'Login failed' }
  return { ok: true }
}

export async function logout(): Promise<void> {
  await fetch('/dashboard/auth/logout', { method: 'POST', credentials: 'same-origin' })
}

// --- Dashboard API ---

export interface AccountListParams {
  page?: number
  pageSize?: number
  query?: string
}

export async function fetchDashboardData(params: AccountListParams = {}): Promise<DashboardData> {
  // Uses dashboard session cookie for auth (not API key).
  // Server-side pagination keeps the payload small for big pools — see
  // proxy.HandleAdminAccounts for the contract.
  const sp = new URLSearchParams()
  if (params.page !== undefined) sp.set('page', String(params.page))
  if (params.pageSize !== undefined) sp.set('page_size', String(params.pageSize))
  if (params.query && params.query.trim()) sp.set('q', params.query.trim())
  const qs = sp.toString()
  const url = qs ? `/admin/accounts?${qs}` : '/admin/accounts'
  const resp = await fetch(url)
  if (!resp.ok) throw new Error(`HTTP ${resp.status}`)
  return resp.json()
}

export async function triggerRefresh(): Promise<{ started: boolean; message?: string }> {
  // Uses dashboard session cookie for auth (not API key)
  const resp = await fetch('/admin/refresh', { method: 'POST' })
  return resp.json()
}

export async function fetchTokenStats(): Promise<TokenStats> {
  // Uses dashboard session cookie for auth (not API key)
  const resp = await fetch('/admin/stats')
  if (!resp.ok) throw new Error(`HTTP ${resp.status}`)
  return resp.json()
}

export function openProxy(accountId: string) {
  window.open(`/proxy/start?account_id=${encodeURIComponent(accountId)}`, '_blank')
}

export function openBestProxy() {
  window.open('/proxy/start?best=true', '_blank')
}

// --- Account Management API ---

export interface AddAccountResult {
  status?: string
  error?: string
  filename?: string
  account?: {
    name: string
    email: string
    space: string
    plan_type: string
  }
}

export async function addAccount(tokenV2: string): Promise<AddAccountResult> {
  const resp = await fetch('/admin/accounts/add', {
    method: 'POST',
    headers: { 'Content-Type': 'application/json', Accept: 'application/json' },
    credentials: 'same-origin',
    body: JSON.stringify({ token_v2: tokenV2 }),
  })
  const data = await readJson<AddAccountResult>(resp, '添加账号接口返回了无效响应')
  if (!resp.ok) return { error: data.error || `HTTP ${resp.status}` }
  return data
}

// --- Settings API ---

export interface SearchSettings {
  enable_web_search: boolean
  enable_workspace_search: boolean
  // ask_mode_default flips Notion's workflow useReadOnlyMode flag for
  // every chat request — model answers but skips edits, matching the
  // frontend "Ask" toggle. Per-request `-ask` model suffix still
  // overrides this default for a single call.
  ask_mode_default: boolean
  disable_notion_prompt: boolean
  debug_logging: boolean
  // notion_proxy is the global upstream proxy applied to every Notion-bound
  // outbound connection. Empty string means "direct dial". Editing this
  // field via /admin/settings PUT immediately drops idle pooled
  // connections so subsequent requests pick up the new upstream.
  notion_proxy: string
}

export async function fetchSettings(): Promise<SearchSettings> {
  // Uses dashboard session cookie for auth (not API key)
  const resp = await fetch('/admin/settings')
  if (!resp.ok) throw new Error(`HTTP ${resp.status}`)
  return resp.json()
}

export async function updateSettings(settings: Partial<Pick<SearchSettings, 'enable_web_search' | 'enable_workspace_search' | 'ask_mode_default' | 'debug_logging' | 'notion_proxy'>>): Promise<SearchSettings> {
  // Uses dashboard session cookie for auth (not API key)
  const resp = await fetch('/admin/settings', {
    method: 'PUT',
    headers: { 'Content-Type': 'application/json' },
    body: JSON.stringify(settings),
  })
  if (!resp.ok) {
    // Surface server-side validation errors (e.g. unsupported proxy
    // scheme) so the caller can show them in a toast and roll the input
    // back instead of silently saving.
    const text = await resp.text()
    let msg = `HTTP ${resp.status}`
    if (text) {
      try {
        const data = JSON.parse(text)
        if (data && typeof data.error === 'string') msg = data.error
      } catch { /* ignore */ }
    }
    throw new Error(msg)
  }
  return resp.json()
}

// --- Bulk Register Jobs API ---

async function jsonOrError(resp: Response): Promise<any> {
  const text = await resp.text()
  let data: any = null
  if (text) {
    try { data = JSON.parse(text) } catch { /* ignore */ }
  }
  if (!resp.ok) {
    const msg = (data && typeof data.error === 'string') ? data.error : `HTTP ${resp.status}`
    throw new Error(msg)
  }
  return data
}

export async function listProviders(): Promise<ProviderInfo[]> {
  const resp = await fetch('/admin/register/providers', {
    credentials: 'same-origin',
  })
  const data = await jsonOrError(resp)
  return Array.isArray(data?.providers) ? data.providers : []
}

export async function startRegisterJob(
  provider: string,
  input: string,
  concurrency: number,
  proxy?: string,
): Promise<JobStartResponse> {
  const body: Record<string, unknown> = { provider, input, concurrency }
  if (proxy && proxy.trim() !== '') body.proxy = proxy.trim()
  const resp = await fetch('/admin/register/start', {
    method: 'POST',
    headers: { 'Content-Type': 'application/json' },
    credentials: 'same-origin',
    body: JSON.stringify(body),
  })
  return jsonOrError(resp) as Promise<JobStartResponse>
}

export async function retryRegisterJob(id: string): Promise<JobStartResponse> {
  const resp = await fetch(`/admin/register/jobs/${encodeURIComponent(id)}/retry`, {
    method: 'POST',
    credentials: 'same-origin',
  })
  return jsonOrError(resp) as Promise<JobStartResponse>
}

export async function deleteRegisterJob(id: string): Promise<void> {
  const resp = await fetch(`/admin/register/jobs/${encodeURIComponent(id)}`, {
    method: 'DELETE',
    credentials: 'same-origin',
  })
  await jsonOrError(resp)
}

export async function listJobs(limit = 50): Promise<RegisterJob[]> {
  const resp = await fetch(`/admin/register/jobs?limit=${encodeURIComponent(String(limit))}`, {
    credentials: 'same-origin',
  })
  const data = await jsonOrError(resp)
  return Array.isArray(data) ? data : []
}

export async function getJob(id: string): Promise<RegisterJob> {
  const resp = await fetch(`/admin/register/jobs/${encodeURIComponent(id)}`, {
    credentials: 'same-origin',
  })
  return jsonOrError(resp) as Promise<RegisterJob>
}

export async function deleteAccount(accountId: string): Promise<void> {
  const resp = await fetch(`/admin/accounts/${encodeURIComponent(accountId)}`, {
    method: 'DELETE',
    credentials: 'same-origin',
  })
  await jsonOrError(resp)
}

export function jobEventsUrl(id: string): string {
  return `/admin/register/jobs/${encodeURIComponent(id)}/events`
}

export function openJobStream(id: string): EventSource {
  // EventSource always sends cookies on same-origin requests, no extra opts
  // are required for the dashboard session.
  return new EventSource(jobEventsUrl(id))
}
