import type { Health, MeWorkspace, User, WorkspaceState, WorkspaceStatus } from './types'

/** An error response (or a failure to get one) from the backend. */
export class ApiError extends Error {
  readonly status: number
  constructor(status: number, message: string) {
    super(message)
    this.name = 'ApiError'
    this.status = status
  }
  /** status 0 = the request never got an HTTP response. */
  get isNetwork(): boolean {
    return this.status === 0
  }
}

const MAX_MESSAGE = 300

/**
 * Pulls a readable message out of an error response. The backend answers
 * {"error": "..."}; anything else (an HTML 502 page from the proxy, an empty
 * body) gets a generic message. Never returns more than a short line, because
 * the text ends up on screen.
 */
export function errorMessage(status: number, body: string): string {
  try {
    const parsed: unknown = JSON.parse(body)
    if (parsed && typeof parsed === 'object' && 'error' in parsed) {
      const e = (parsed as { error: unknown }).error
      if (typeof e === 'string' && e.trim() !== '') {
        const s = e.trim()
        return s.length > MAX_MESSAGE ? `${s.slice(0, MAX_MESSAGE)}…` : s
      }
    }
  } catch {
    // not JSON
  }
  if (status === 502 || status === 503 || status === 504) {
    return `The server is unavailable (HTTP ${status}). Try again in a moment.`
  }
  return `Request failed (HTTP ${status}).`
}

export interface ApiOptions {
  fetch?: typeof fetch
  /**
   * Called when an authenticated endpoint answers 401 (the session ended).
   * Not called for login/register, where 401 only means wrong credentials.
   */
  onUnauthorized?: () => void
}

interface RequestOptions {
  body?: unknown
  /** Login and register: a 401 is an answer, not an expired session. */
  anonymous?: boolean
  signal?: AbortSignal
}

export interface Api {
  me(signal?: AbortSignal): Promise<{ user: User; workspace: MeWorkspace }>
  register(username: string, password: string): Promise<{ user: User }>
  login(username: string, password: string): Promise<{ user: User }>
  logout(): Promise<void>
  workspace(signal?: AbortSignal): Promise<WorkspaceStatus>
  start(): Promise<WorkspaceStatus>
  stop(): Promise<WorkspaceStatus>
  heartbeat(): Promise<void>
}

/**
 * The API client. Every request is same-origin with cookies. Every non-GET
 * request declares Content-Type: application/json, including the body-less
 * ones (power on/off, heartbeat, logout): the backend refuses anything else, so
 * a cross-site HTML form cannot trigger them.
 */
export function createApi(opts: ApiOptions = {}): Api {
  const doFetch = opts.fetch ?? ((input, init) => fetch(input, init))

  async function request(method: 'GET' | 'POST', path: string, o: RequestOptions = {}): Promise<unknown> {
    const headers: Record<string, string> = { Accept: 'application/json' }
    const init: RequestInit = {
      method,
      headers,
      credentials: 'same-origin',
      cache: 'no-store',
      signal: o.signal,
    }
    if (method !== 'GET') {
      headers['Content-Type'] = 'application/json'
      init.body = o.body === undefined ? '{}' : JSON.stringify(o.body)
    }

    let res: Response
    try {
      res = await doFetch(path, init)
    } catch (err) {
      if (err instanceof DOMException && err.name === 'AbortError') throw err
      throw new ApiError(0, 'Cannot reach the server. Check your connection.')
    }

    const text = await res.text().catch(() => '')
    if (!res.ok) {
      if (res.status === 401 && !o.anonymous) opts.onUnauthorized?.()
      throw new ApiError(res.status, errorMessage(res.status, text))
    }
    if (text === '') return undefined
    try {
      return JSON.parse(text) as unknown
    } catch {
      throw new ApiError(res.status, 'The server sent an unreadable response.')
    }
  }

  return {
    me: async (signal) => {
      const body = await request('GET', '/api/me', { signal })
      return { user: parseUser(get(body, 'user')), workspace: parseMeWorkspace(get(body, 'workspace')) }
    },
    register: async (username, password) => {
      const body = await request('POST', '/api/auth/register', { body: { username, password }, anonymous: true })
      return { user: parseUser(get(body, 'user')) }
    },
    login: async (username, password) => {
      const body = await request('POST', '/api/auth/login', { body: { username, password }, anonymous: true })
      return { user: parseUser(get(body, 'user')) }
    },
    logout: async () => {
      await request('POST', '/api/auth/logout', { anonymous: true })
    },
    workspace: async (signal) => parseStatus(get(await request('GET', '/api/workspace', { signal }), 'workspace')),
    start: async () => parseStatus(get(await request('POST', '/api/workspace/start'), 'workspace')),
    stop: async () => parseStatus(get(await request('POST', '/api/workspace/stop'), 'workspace')),
    heartbeat: async () => {
      await request('POST', '/api/workspace/heartbeat')
    },
  }
}

// --- response validation: a proxy page or a changed backend must surface as a
// readable error, not as a crash deep in the UI. ---

const STATES: readonly WorkspaceState[] = ['offline', 'starting', 'running', 'stopped']
const HEALTHS: readonly Health[] = ['none', 'starting', 'healthy', 'unhealthy']

function bad(): never {
  throw new ApiError(200, 'The server sent an unexpected response.')
}

function isRecord(x: unknown): x is Record<string, unknown> {
  return typeof x === 'object' && x !== null && !Array.isArray(x)
}

function get(body: unknown, key: string): Record<string, unknown> {
  if (!isRecord(body)) return bad()
  const v = body[key]
  return isRecord(v) ? v : bad()
}

function num(v: unknown, fallback = 0): number {
  return typeof v === 'number' && Number.isFinite(v) ? v : fallback
}

function strOrNull(v: unknown): string | null {
  return typeof v === 'string' ? v : null
}

function parseState(v: unknown): WorkspaceState {
  return STATES.includes(v as WorkspaceState) ? (v as WorkspaceState) : bad()
}

function parseUser(r: Record<string, unknown>): User {
  if (typeof r.username !== 'string') return bad()
  return { id: num(r.id), username: r.username, is_admin: r.is_admin === true }
}

function parseMeWorkspace(r: Record<string, unknown>): MeWorkspace {
  return {
    state: parseState(r.state),
    mem_limit_mb: num(r.mem_limit_mb),
    cpu_limit: num(r.cpu_limit),
    last_active_at: strOrNull(r.last_active_at),
  }
}

export function parseStatus(r: Record<string, unknown>): WorkspaceStatus {
  const u = r.usage
  return {
    state: parseState(r.state),
    health: HEALTHS.includes(r.health as Health) ? (r.health as Health) : 'none',
    mem_limit_mb: num(r.mem_limit_mb),
    cpu_limit: num(r.cpu_limit),
    last_active_at: strOrNull(r.last_active_at),
    last_stopped_at: strOrNull(r.last_stopped_at),
    usage: isRecord(u)
      ? {
          cpu_percent: num(u.cpu_percent),
          mem_bytes: num(u.mem_bytes),
          mem_limit_bytes: num(u.mem_limit_bytes),
          pids: num(u.pids),
        }
      : null,
    neko_password: typeof r.neko_password === 'string' && r.neko_password !== '' ? r.neko_password : undefined,
  }
}
