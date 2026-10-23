import { act, fireEvent, render, screen, within } from '@testing-library/react'
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'
import { App, SESSION_EXPIRED } from './App'

vi.mock('./components/TerminalPane', () => ({
  TerminalPane: () => <div data-testid="terminal" />,
}))

interface Call {
  method: string
  path: string
  body: unknown
  contentType: string | null
}

type Handler = (call: Call) => Response | Promise<Response>

const json = (status: number, body: unknown) => new Response(JSON.stringify(body), { status })

const ME = {
  user: { id: 1, username: 'alice', is_admin: true },
  workspace: { state: 'stopped', mem_limit_mb: 2048, cpu_limit: 2, last_active_at: null },
}
const STOPPED = {
  workspace: {
    state: 'stopped',
    health: 'none',
    mem_limit_mb: 2048,
    cpu_limit: 2,
    last_active_at: null,
    last_stopped_at: null,
    usage: null,
  },
}

let calls: Call[]
let handlers: Record<string, Handler>

function install() {
  calls = []
  vi.stubGlobal(
    'fetch',
    vi.fn(async (input: RequestInfo | URL, init?: RequestInit) => {
      const path = String(input)
      const method = init?.method ?? 'GET'
      const headers = (init?.headers ?? {}) as Record<string, string>
      const call: Call = {
        method,
        path,
        body: init?.body ? JSON.parse(String(init.body)) : undefined,
        contentType: headers['Content-Type'] ?? null,
      }
      calls.push(call)
      const h = handlers[`${method} ${path}`]
      if (!h) return json(404, { error: 'not found' })
      return h(call)
    }),
  )
}

async function flush() {
  await act(async () => {
    await vi.advanceTimersByTimeAsync(0)
  })
}

async function boot() {
  render(<App />)
  await flush()
}

const field = (name: RegExp) => screen.getByLabelText(name) as HTMLInputElement
const type = (el: HTMLElement, value: string) => fireEvent.change(el, { target: { value } })

beforeEach(() => {
  vi.useFakeTimers()
  localStorage.clear()
  handlers = {
    'GET /api/workspace': () => json(200, STOPPED),
  }
  install()
})
afterEach(() => {
  vi.useRealTimers()
  vi.unstubAllGlobals()
})

describe('boot', () => {
  it('GET /api/me 200: shows the workspace page for that user', async () => {
    handlers['GET /api/me'] = () => json(200, ME)
    await boot()
    expect(within(screen.getByRole('banner')).getByText('alice')).toBeInTheDocument()
    expect(screen.getByRole('button', { name: 'Power on' })).toBeInTheDocument()
  })

  it('GET /api/me 401: shows the login page, without a "session expired" message', async () => {
    handlers['GET /api/me'] = () => json(401, { error: 'not logged in' })
    await boot()
    expect(screen.getByRole('button', { name: 'Log in' })).toBeInTheDocument()
    expect(screen.queryByText(SESSION_EXPIRED)).toBeNull()
  })

  it('server unreachable at load: an error with a retry, not the login page', async () => {
    let up = false
    handlers['GET /api/me'] = () => (up ? json(200, ME) : new Response('Bad Gateway', { status: 502 }))
    await boot()
    expect(screen.getByRole('alert')).toHaveTextContent(/unavailable/i)
    expect(screen.queryByLabelText(/username/i)).toBeNull()
    up = true
    fireEvent.click(screen.getByRole('button', { name: 'Try again' }))
    await flush()
    expect(screen.getByRole('button', { name: 'Power on' })).toBeInTheDocument()
  })
})

describe('session expiry', () => {
  it('a 401 from a workspace call returns to the login page with a message', async () => {
    handlers['GET /api/me'] = () => json(200, ME)
    await boot()
    expect(screen.getByRole('button', { name: 'Power on' })).toBeInTheDocument()
    handlers['POST /api/workspace/start'] = () => json(401, { error: 'not logged in' })
    fireEvent.click(screen.getByRole('button', { name: 'Power on' }))
    await flush()
    expect(screen.getByText(SESSION_EXPIRED)).toBeInTheDocument()
    expect(screen.getByLabelText(/username/i)).toBeInTheDocument()
    expect(screen.queryByRole('button', { name: 'Power on' })).toBeNull()
  })

  it('a 401 from the status poll does the same', async () => {
    handlers['GET /api/me'] = () => json(200, ME)
    handlers['GET /api/workspace'] = () =>
      json(200, { workspace: { ...STOPPED.workspace, state: 'running', health: 'healthy', neko_password: 'x' } })
    await boot()
    expect(screen.getByTestId('terminal')).toBeInTheDocument()
    handlers['GET /api/workspace'] = () => json(401, { error: 'not logged in' })
    await act(async () => {
      await vi.advanceTimersByTimeAsync(10_000)
    })
    expect(screen.getByText(SESSION_EXPIRED)).toBeInTheDocument()
    expect(screen.queryByTestId('terminal')).toBeNull()
    const before = calls.length
    await act(async () => {
      await vi.advanceTimersByTimeAsync(120_000)
    })
    expect(calls.length).toBe(before) // nothing keeps polling or sending heartbeats
  })

  it('logging out returns to a clean login page', async () => {
    handlers['GET /api/me'] = () => json(200, ME)
    handlers['POST /api/auth/logout'] = () => new Response(null, { status: 204 })
    await boot()
    fireEvent.click(screen.getByRole('button', { name: 'Log out' }))
    await flush()
    expect(screen.getByRole('button', { name: 'Log in' })).toBeInTheDocument()
    expect(screen.queryByText(SESSION_EXPIRED)).toBeNull()
    const logout = calls.find((c) => c.path === '/api/auth/logout')!
    expect(logout.contentType).toBe('application/json')
  })
})

describe('login', () => {
  beforeEach(() => {
    handlers['GET /api/me'] = () => json(401, { error: 'not logged in' })
  })

  it('logs in and shows the workspace', async () => {
    handlers['POST /api/auth/login'] = () => json(200, { user: ME.user })
    await boot()
    type(field(/username/i), 'alice')
    type(field(/password/i), 'correct horse')
    fireEvent.submit(field(/username/i).closest('form')!)
    await flush()
    expect(calls.find((c) => c.path === '/api/auth/login')).toMatchObject({
      method: 'POST',
      body: { username: 'alice', password: 'correct horse' },
      contentType: 'application/json',
    })
    expect(within(screen.getByRole('banner')).getByText('alice')).toBeInTheDocument()
  })

  it('a wrong password shows the server message inline and is not mistaken for an expired session', async () => {
    handlers['POST /api/auth/login'] = () => json(401, { error: 'invalid username or password' })
    await boot()
    type(field(/username/i), 'alice')
    type(field(/password/i), 'wrong password')
    fireEvent.submit(field(/username/i).closest('form')!)
    await flush()
    expect(screen.getByRole('alert')).toHaveTextContent('invalid username or password')
    expect(screen.queryByText(SESSION_EXPIRED)).toBeNull()
    expect(field(/password/i).value).toBe('') // cleared for another try
  })

  it('does not call the server with empty fields', async () => {
    await boot()
    fireEvent.submit(field(/username/i).closest('form')!)
    await flush()
    expect(calls.filter((c) => c.path === '/api/auth/login')).toHaveLength(0)
    expect(screen.getByRole('alert')).toHaveTextContent(/enter your username and password/i)
  })
})

describe('register', () => {
  beforeEach(() => {
    handlers['GET /api/me'] = () => json(401, { error: 'not logged in' })
  })

  async function toRegister() {
    await boot()
    fireEvent.click(screen.getByRole('tab', { name: 'Register' }))
  }
  const form = () => field(/username/i).closest('form')!

  it('shows the username and password rules', async () => {
    await toRegister()
    expect(screen.getByText(/3 to 32 characters/i)).toBeInTheDocument()
    expect(screen.getByText(/8 to 72 bytes/i)).toBeInTheDocument()
  })

  it('registers, then logs in (register does not log in by itself)', async () => {
    handlers['POST /api/auth/register'] = () => json(201, { user: ME.user, workspace: ME.workspace })
    handlers['POST /api/auth/login'] = () => json(200, { user: ME.user })
    await toRegister()
    type(field(/username/i), 'alice')
    type(field(/password/i), 'correct horse')
    fireEvent.submit(form())
    await flush()
    const order = calls.filter((c) => c.path.startsWith('/api/auth/')).map((c) => c.path)
    expect(order).toEqual(['/api/auth/register', '/api/auth/login'])
    expect(within(screen.getByRole('banner')).getByText('alice')).toBeInTheDocument()
  })

  it('blocks a bad username or short password before any request, with inline hints', async () => {
    await toRegister()
    type(field(/username/i), 'Alice_1')
    type(field(/password/i), 'short')
    fireEvent.submit(form())
    await flush()
    expect(calls.filter((c) => c.path.startsWith('/api/auth/'))).toHaveLength(0)
    expect(screen.getByText(/lowercase letters, digits and single hyphens, and must start/i)).toBeInTheDocument()
    expect(screen.getByText(/at least 8 bytes/i)).toBeInTheDocument()
    expect(field(/username/i)).toHaveAttribute('aria-invalid', 'true')
  })

  it('counts password bytes, not characters (bcrypt limit is 72 bytes)', async () => {
    await toRegister()
    type(field(/username/i), 'alice')
    type(field(/password/i), '€'.repeat(25)) // 25 characters, 75 bytes
    fireEvent.submit(form())
    await flush()
    expect(calls.filter((c) => c.path.startsWith('/api/auth/'))).toHaveLength(0)
    expect(screen.getByText(/75 bytes; the limit is 72/i)).toBeInTheDocument()
  })

  it('a taken username is reported on the username field', async () => {
    handlers['POST /api/auth/register'] = () => json(409, { error: 'username already taken' })
    await toRegister()
    type(field(/username/i), 'alice')
    type(field(/password/i), 'correct horse')
    fireEvent.submit(form())
    await flush()
    expect(screen.getByText('That username is already taken.')).toBeInTheDocument()
    expect(field(/username/i)).toHaveAttribute('aria-invalid', 'true')
    expect(calls.filter((c) => c.path === '/api/auth/login')).toHaveLength(0)
  })

  it('other server errors are shown as the form error', async () => {
    handlers['POST /api/auth/register'] = () => json(400, { error: 'password must be at least 8 bytes' })
    await toRegister()
    type(field(/username/i), 'alice')
    type(field(/password/i), 'correct horse')
    fireEvent.submit(form())
    await flush()
    expect(screen.getByRole('alert')).toHaveTextContent('password must be at least 8 bytes')
  })

  it('if the account was created but the login fails, says so and switches to the login form', async () => {
    handlers['POST /api/auth/register'] = () => json(201, { user: ME.user, workspace: ME.workspace })
    handlers['POST /api/auth/login'] = () => json(500, { error: 'internal error' })
    await toRegister()
    type(field(/username/i), 'alice')
    type(field(/password/i), 'correct horse')
    fireEvent.submit(form())
    await flush()
    expect(screen.getByText(/your account was created/i)).toBeInTheDocument()
    expect(screen.getByRole('alert')).toHaveTextContent('internal error')
    expect(screen.queryByText(/3 to 32 characters/i)).toBeNull() // back on the login form
  })
})
