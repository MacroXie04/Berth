import { describe, expect, it, vi } from 'vitest'
import { ApiError, createApi, errorMessage } from './api'

const status = {
  state: 'running',
  health: 'healthy',
  mem_limit_mb: 2048,
  cpu_limit: 2,
  last_active_at: '2026-10-23T15:37:19Z',
  last_stopped_at: null,
  usage: { cpu_percent: 0.5, mem_bytes: 226959360, mem_limit_bytes: 2147483648, pids: 112 },
  neko_password: 'pw',
}

function respond(statusCode: number, body: unknown, raw = false): Response {
  const text = raw ? (body as string) : body === undefined ? '' : JSON.stringify(body)
  return new Response(statusCode === 204 ? null : text, { status: statusCode })
}

function client(res: () => Response | Promise<Response>, onUnauthorized = vi.fn()) {
  const fetchMock = vi.fn(async (_input: RequestInfo | URL, _init?: RequestInit) => res())
  return { api: createApi({ fetch: fetchMock as unknown as typeof fetch, onUnauthorized }), fetchMock, onUnauthorized }
}

const headersOf = (init: RequestInit | undefined) => (init?.headers ?? {}) as Record<string, string>

describe('request headers', () => {
  it('every state-changing call declares Content-Type: application/json, even without a body', async () => {
    const { api, fetchMock } = client(() =>
      respond(200, { workspace: status, user: { id: 1, username: 'alice', is_admin: false } }),
    )
    await api.start()
    await api.stop()
    await api.heartbeat().catch(() => {})
    await api.logout()
    await api.login('alice', 'correct horse')
    await api.register('alice', 'correct horse')
    expect(fetchMock).toHaveBeenCalledTimes(6)
    for (const [url, init] of fetchMock.mock.calls) {
      expect(init?.method, String(url)).toBe('POST')
      expect(headersOf(init)['Content-Type'], String(url)).toBe('application/json')
      expect(init?.credentials).toBe('same-origin')
    }
  })

  it('GET requests do not send a body', async () => {
    const { api, fetchMock } = client(() => respond(200, { workspace: status }))
    await api.workspace()
    const init = fetchMock.mock.calls[0]![1]!
    expect(init.method).toBe('GET')
    expect(init.body).toBeUndefined()
  })

  it('talks to the documented endpoints with root-relative URLs', async () => {
    const { api, fetchMock } = client(() =>
      respond(200, { workspace: status, user: { id: 1, username: 'alice', is_admin: true } }),
    )
    await api.me().catch(() => {})
    await api.workspace()
    await api.start()
    await api.stop()
    await api.login('a', 'b')
    await api.register('a', 'b')
    await api.logout()
    await api.heartbeat()
    expect(fetchMock.mock.calls.map((c) => c[0])).toEqual([
      '/api/me',
      '/api/workspace',
      '/api/workspace/start',
      '/api/workspace/stop',
      '/api/auth/login',
      '/api/auth/register',
      '/api/auth/logout',
      '/api/workspace/heartbeat',
    ])
  })

  it('sends credentials as a JSON body', async () => {
    const { api, fetchMock } = client(() => respond(200, { user: { id: 1, username: 'alice', is_admin: false } }))
    await api.login('alice', 'correct horse')
    expect(JSON.parse(String(fetchMock.mock.calls[0]![1]!.body))).toEqual({
      username: 'alice',
      password: 'correct horse',
    })
  })
})

describe('responses', () => {
  it('parses the workspace status', async () => {
    const { api } = client(() => respond(200, { workspace: status }))
    const s = await api.workspace()
    expect(s.state).toBe('running')
    expect(s.usage?.mem_bytes).toBe(226959360)
    expect(s.neko_password).toBe('pw')
  })

  it('carries the account the server answered for (top-level "username") on status, start and stop', async () => {
    const { api } = client(() => respond(200, { username: 'bob', workspace: status }))
    expect((await api.workspace()).username).toBe('bob')
    expect((await api.start()).username).toBe('bob')
    expect((await api.stop()).username).toBe('bob')
  })

  it('has no username when an older backend does not send one (or sends nonsense)', async () => {
    const old = client(() => respond(200, { workspace: status }))
    expect((await old.api.workspace()).username).toBeUndefined()
    for (const username of [7, null, '', ['bob'], { name: 'bob' }]) {
      const odd = client(() => respond(200, { username, workspace: status }))
      expect((await odd.api.workspace()).username, JSON.stringify(username)).toBeUndefined()
    }
  })

  it('does not take a username from inside the workspace object', async () => {
    const { api } = client(() => respond(200, { workspace: { ...status, username: 'bob' } }))
    expect((await api.workspace()).username).toBeUndefined()
  })

  it('tolerates a status without usage or neko password', async () => {
    const { api } = client(() =>
      respond(200, {
        workspace: {
          state: 'stopped',
          health: 'none',
          mem_limit_mb: 1,
          cpu_limit: 1,
          last_active_at: null,
          last_stopped_at: null,
          usage: null,
        },
      }),
    )
    const s = await api.workspace()
    expect(s.usage).toBeNull()
    expect(s.neko_password).toBeUndefined()
  })

  it('treats an unknown state or a non-JSON 200 as an error, not as data', async () => {
    const bad = client(() => respond(200, { workspace: { state: 'exploding' } }))
    await expect(bad.api.workspace()).rejects.toBeInstanceOf(ApiError)
    const html = client(() => respond(200, '<html>proxy page</html>', true))
    await expect(html.api.workspace()).rejects.toMatchObject({ message: 'The server sent an unreadable response.' })
  })

  it('heartbeat and logout accept an empty 204', async () => {
    const { api } = client(() => respond(204, undefined))
    await expect(api.heartbeat()).resolves.toBeUndefined()
    await expect(api.logout()).resolves.toBeUndefined()
  })
})

describe('errors', () => {
  it('extracts the server message from {"error": "..."}', async () => {
    const { api } = client(() => respond(409, { error: 'username already taken' }))
    await expect(api.register('alice', 'correct horse')).rejects.toMatchObject({
      name: 'ApiError',
      status: 409,
      message: 'username already taken',
    })
  })

  it('falls back to a readable message when the body is not the usual JSON', () => {
    expect(errorMessage(502, '<html>Bad Gateway</html>')).toContain('unavailable')
    expect(errorMessage(500, '')).toBe('Request failed (HTTP 500).')
    expect(errorMessage(400, '{"error": ""}')).toBe('Request failed (HTTP 400).')
    expect(errorMessage(400, '{"error": 5}')).toBe('Request failed (HTTP 400).')
    expect(errorMessage(400, 'null')).toBe('Request failed (HTTP 400).')
  })

  it('never shows an unbounded server string', () => {
    const msg = errorMessage(500, JSON.stringify({ error: 'x'.repeat(5000) }))
    expect(msg.length).toBeLessThan(400)
  })

  it('a network failure becomes an ApiError with status 0', async () => {
    const { api } = client(() => {
      throw new TypeError('Failed to fetch')
    })
    const err = await api.workspace().catch((e: unknown) => e)
    expect(err).toBeInstanceOf(ApiError)
    expect((err as ApiError).isNetwork).toBe(true)
    expect((err as ApiError).message).toMatch(/cannot reach/i)
  })

  it('an aborted request stays an AbortError (it is not a failure to report)', async () => {
    const { api } = client(() => {
      throw new DOMException('aborted', 'AbortError')
    })
    await expect(api.workspace()).rejects.toMatchObject({ name: 'AbortError' })
  })
})

describe('401 handling', () => {
  it('an authenticated endpoint answering 401 calls onUnauthorized (session ended)', async () => {
    const { api, onUnauthorized } = client(() => respond(401, { error: 'not logged in' }))
    await expect(api.workspace()).rejects.toMatchObject({ status: 401 })
    await expect(api.start()).rejects.toMatchObject({ status: 401 })
    await expect(api.heartbeat()).rejects.toMatchObject({ status: 401 })
    await expect(api.me()).rejects.toMatchObject({ status: 401 })
    expect(onUnauthorized).toHaveBeenCalledTimes(4)
  })

  it('a 401 from login or register is just a wrong password, not an expired session', async () => {
    const { api, onUnauthorized } = client(() => respond(401, { error: 'invalid username or password' }))
    await expect(api.login('alice', 'nope')).rejects.toMatchObject({
      status: 401,
      message: 'invalid username or password',
    })
    await expect(api.register('alice', 'nope')).rejects.toMatchObject({ status: 401 })
    expect(onUnauthorized).not.toHaveBeenCalled()
  })

  it('other errors do not look like a session ending', async () => {
    const { api, onUnauthorized } = client(() => respond(503, { error: 'the container service is unavailable' }))
    await expect(api.start()).rejects.toMatchObject({ status: 503 })
    expect(onUnauthorized).not.toHaveBeenCalled()
  })
})
