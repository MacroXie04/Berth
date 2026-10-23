import { act, fireEvent, render, screen, within } from '@testing-library/react'
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'
import { ApiError, type Api } from '../api'
import { NEKO_SESSION_KEY, nekoUrl } from '../neko'
import type { Health, WorkspaceState, WorkspaceStatus } from '../types'
import { WorkspacePage } from './WorkspacePage'

// xterm.js needs a real layout engine; the terminal is covered by the ttyd tests
// and the browser smoke test.
vi.mock('./TerminalPane', () => ({
  TerminalPane: ({ username }: { username: string }) => <div data-testid="terminal" data-user={username} />,
}))

const NEKO_PW = 'neko-secret-pw'

function ws(state: WorkspaceState, health: Health = 'none', extra: Partial<WorkspaceStatus> = {}): WorkspaceStatus {
  return {
    state,
    health,
    mem_limit_mb: 2048,
    cpu_limit: 2,
    last_active_at: null,
    last_stopped_at: null,
    usage: null,
    ...(state === 'starting' || state === 'running' ? { neko_password: NEKO_PW } : {}),
    ...extra,
  }
}

const running = (extra: Partial<WorkspaceStatus> = {}) =>
  ws('running', 'healthy', {
    usage: { cpu_percent: 0.4939, mem_bytes: 226959360, mem_limit_bytes: 2147483648, pids: 112 },
    ...extra,
  })

function mockApi(initial: WorkspaceStatus) {
  let current = initial
  const m = {
    workspace: vi.fn(async () => current),
    start: vi.fn(async () => ws('starting')),
    stop: vi.fn(async () => ws('stopped')),
    heartbeat: vi.fn(async () => {}),
    logout: vi.fn(async () => {}),
    set: (s: WorkspaceStatus) => {
      current = s
    },
  }
  return { ...m, api: m as unknown as Api }
}

async function renderPage(initial: WorkspaceStatus, onLoggedOut = vi.fn()) {
  const m = mockApi(initial)
  const utils = render(<WorkspacePage api={m.api} username="alice" onLoggedOut={onLoggedOut} />)
  await act(async () => {
    await vi.advanceTimersByTimeAsync(0)
  })
  return { ...m, ...utils, onLoggedOut }
}

async function advance(ms: number) {
  await act(async () => {
    await vi.advanceTimersByTimeAsync(ms)
  })
}

beforeEach(() => {
  vi.useFakeTimers()
  localStorage.clear()
})
afterEach(() => {
  vi.useRealTimers()
})

describe('state to UI', () => {
  it('shows the first answer as "checking" until it arrives', () => {
    const m = mockApi(ws('offline'))
    render(<WorkspacePage api={m.api} username="alice" onLoggedOut={() => {}} />)
    expect(screen.getByText('Checking your workspace…')).toBeInTheDocument()
    expect(screen.queryByRole('button', { name: 'Power on' })).toBeNull()
  })

  it.each([
    ['offline', /not running/i, /removed/i],
    ['stopped', /powered off/i, /it was stopped/i],
  ] as const)('%s: a clear call to action and a promise that data is kept', async (state, title, text) => {
    await renderPage(ws(state))
    expect(screen.getByRole('heading', { name: title })).toBeInTheDocument()
    expect(screen.getByText(text)).toBeInTheDocument()
    expect(screen.getByText(/your files are kept/i)).toBeInTheDocument()
    expect(screen.getByRole('button', { name: 'Power on' })).toBeEnabled()
    expect(screen.queryByTestId('terminal')).toBeNull()
    expect(screen.queryByTitle('Remote browser')).toBeNull()
    expect(
      within(screen.getByRole('banner')).getByText(state === 'offline' ? 'Offline' : 'Stopped'),
    ).toBeInTheDocument()
  })

  it('starting: progress with elapsed seconds, no panes, polls until running', async () => {
    const m = await renderPage(ws('starting'))
    expect(screen.getByRole('heading', { name: /starting your workspace/i })).toBeInTheDocument()
    expect(screen.queryByTestId('terminal')).toBeNull()
    expect(screen.queryByTitle('Remote browser')).toBeNull()
    expect(screen.getByText('0 s')).toBeInTheDocument()
    await advance(5_000)
    expect(screen.getByText('5 s')).toBeInTheDocument()

    m.set(running())
    await advance(2_000)
    expect(screen.getByTestId('terminal')).toBeInTheDocument()
    expect(screen.getByTitle('Remote browser')).toBeInTheDocument()
    expect(screen.queryByRole('heading', { name: /starting/i })).toBeNull()
  })

  it('says so when starting takes unusually long', async () => {
    await renderPage(ws('starting'))
    expect(screen.queryByText(/longer than usual/i)).toBeNull()
    await advance(61_000)
    expect(screen.getByText(/longer than usual/i)).toBeInTheDocument()
  })

  it('running + healthy: terminal and browser side by side with a separator between them', async () => {
    await renderPage(running())
    const sep = screen.getByRole('separator')
    expect(sep).toHaveAttribute('aria-orientation', 'vertical')
    expect(screen.getByTestId('terminal')).toHaveAttribute('data-user', 'alice')
    expect(screen.getByRole('button', { name: 'Power off' })).toBeEnabled()
    expect(screen.queryByRole('button', { name: 'Power on' })).toBeNull()
  })

  it('running but the healthcheck is still "starting": no panes yet (no routes exist)', async () => {
    await renderPage(ws('running', 'starting'))
    expect(screen.queryByTestId('terminal')).toBeNull()
    expect(screen.queryByTitle('Remote browser')).toBeNull()
    expect(screen.getByRole('heading', { name: /starting/i })).toBeInTheDocument()
  })

  it('running + unhealthy: a visible warning and the panes stay', async () => {
    await renderPage(ws('running', 'unhealthy'))
    expect(screen.getByRole('alert')).toHaveTextContent(/health check is failing/i)
    expect(screen.getByTestId('terminal')).toBeInTheDocument()
    expect(screen.getByTitle('Remote browser')).toBeInTheDocument()
    expect(screen.getAllByText('Unhealthy').length).toBeGreaterThan(0)
  })

  it('no unhealthy warning for a healthy workspace', async () => {
    await renderPage(running())
    expect(screen.queryByRole('alert')).toBeNull()
  })

  it('the panes disappear when the workspace stops on its own (idle reaper)', async () => {
    const m = await renderPage(running())
    expect(screen.getByTestId('terminal')).toBeInTheDocument()
    m.set(ws('stopped'))
    await advance(10_000)
    expect(screen.queryByTestId('terminal')).toBeNull()
    expect(screen.queryByTitle('Remote browser')).toBeNull()
    expect(screen.getByRole('button', { name: 'Power on' })).toBeInTheDocument()
  })
})

describe('status bar', () => {
  it('shows user, state, health, memory and CPU', async () => {
    await renderPage(running())
    const bar = screen.getByRole('banner')
    expect(within(bar).getByText('alice')).toBeInTheDocument()
    expect(within(bar).getByText('Running')).toBeInTheDocument()
    expect(within(bar).getByText('Healthy')).toBeInTheDocument()
    expect(within(bar).getByText(/216 MiB/)).toBeInTheDocument()
    expect(within(bar).getByText(/2\.0 GiB/)).toBeInTheDocument()
    expect(within(bar).getByText('0.5%')).toBeInTheDocument()
  })

  it('has no usage numbers when the workspace is not running', async () => {
    await renderPage(ws('stopped'))
    expect(screen.queryByRole('group', { name: /resource usage/i })).toBeNull()
    expect(screen.queryByText('Memory')).toBeNull()
  })

  it('flags a status that could not be refreshed', async () => {
    const m = await renderPage(running())
    m.workspace.mockRejectedValue(new ApiError(0, 'Cannot reach the server. Check your connection.'))
    await advance(10_000)
    expect(screen.getByText('Not up to date')).toBeInTheDocument()
    expect(screen.getByTestId('terminal')).toBeInTheDocument() // the panes are not torn down for a blip
  })
})

describe('Power on', () => {
  it('starts the workspace and shows progress at once', async () => {
    const m = await renderPage(ws('stopped'))
    let release!: (s: WorkspaceStatus) => void
    m.start.mockImplementation(() => new Promise<WorkspaceStatus>((r) => (release = r)))
    fireEvent.click(screen.getByRole('button', { name: 'Power on' }))
    expect(m.start).toHaveBeenCalledTimes(1)
    expect(screen.getByRole('heading', { name: /starting your workspace/i })).toBeInTheDocument()
    await act(async () => release(ws('starting')))
    expect(screen.getByRole('heading', { name: /starting your workspace/i })).toBeInTheDocument()
  })

  it('503: a readable message, and the button is there to try again', async () => {
    const m = await renderPage(ws('offline'))
    m.start.mockRejectedValue(new ApiError(503, 'the server has no capacity for another workspace'))
    fireEvent.click(screen.getByRole('button', { name: 'Power on' }))
    await advance(0)
    expect(screen.getByRole('alert')).toHaveTextContent(/no capacity for another workspace/i)
    expect(screen.getByRole('alert')).toHaveTextContent(/try again/i)
    expect(screen.getByRole('button', { name: 'Power on' })).toBeEnabled()
    fireEvent.click(screen.getByRole('button', { name: 'Dismiss' }))
    expect(screen.queryByRole('alert')).toBeNull()
  })

  it('409: says it is busy and then shows what the server is doing', async () => {
    const m = await renderPage(ws('offline'))
    m.start.mockRejectedValue(new ApiError(409, 'the workspace is busy starting or stopping; try again in a moment'))
    m.set(ws('starting'))
    fireEvent.click(screen.getByRole('button', { name: 'Power on' }))
    await advance(0)
    expect(screen.getByRole('alert')).toHaveTextContent(/busy/i)
    expect(screen.getByRole('heading', { name: /starting your workspace/i })).toBeInTheDocument()
  })

  it('415 and other errors: the message from the server', async () => {
    const m = await renderPage(ws('offline'))
    m.start.mockRejectedValue(new ApiError(415, 'Content-Type must be application/json'))
    fireEvent.click(screen.getByRole('button', { name: 'Power on' }))
    await advance(0)
    expect(screen.getByRole('alert')).toHaveTextContent('Content-Type must be application/json')
  })
})

describe('browser pane', () => {
  it('is an iframe on the neko URL with the documented attributes and no sandbox', async () => {
    await renderPage(running())
    const frame = screen.getByTitle('Remote browser') as HTMLIFrameElement
    expect(frame.tagName).toBe('IFRAME')
    expect(frame.getAttribute('src')).toBe(nekoUrl('alice', NEKO_PW))
    expect(frame.getAttribute('allow')).toBe('clipboard-write; fullscreen')
    expect(frame.getAttribute('referrerpolicy')).toBe('no-referrer')
    expect(frame.hasAttribute('sandbox')).toBe(false)
  })

  it('the neko password appears only in the iframe URL', async () => {
    await renderPage(running())
    expect(document.body.textContent).not.toContain(NEKO_PW)
    expect(document.body.innerHTML.split(NEKO_PW).length - 1).toBe(1) // exactly one place: the iframe src
    expect(JSON.stringify({ ...localStorage })).not.toContain(NEKO_PW)
    expect(JSON.stringify({ ...sessionStorage })).not.toContain(NEKO_PW)
  })

  it("forgets neko's stored session before the frame loads, and when the pane goes away", async () => {
    window.localStorage.setItem(NEKO_SESSION_KEY, 'stale-token')
    const m = await renderPage(running())
    expect(screen.getByTitle('Remote browser')).toBeInTheDocument()
    expect(window.localStorage.getItem(NEKO_SESSION_KEY)).toBeNull() // gone before the iframe existed

    window.localStorage.setItem(NEKO_SESSION_KEY, 'token-of-this-session')
    m.set(ws('stopped'))
    fireEvent.click(screen.getByRole('button', { name: 'Power off' }))
    fireEvent.click(screen.getByRole('button', { name: /yes, power off/i }))
    await advance(0)
    await advance(2_000)
    expect(screen.queryByTitle('Remote browser')).not.toBeInTheDocument()
    expect(window.localStorage.getItem(NEKO_SESSION_KEY)).toBeNull()
  })

  it('does not reload when the status is refreshed', async () => {
    await renderPage(running())
    const before = screen.getByTitle('Remote browser')
    await advance(10_000)
    await advance(10_000)
    expect(screen.getByTitle('Remote browser')).toBe(before)
  })
})

describe('Power off', () => {
  it('asks inline first (not window.confirm) and Cancel changes nothing', async () => {
    const confirm = vi.spyOn(window, 'confirm')
    const m = await renderPage(running())
    fireEvent.click(screen.getByRole('button', { name: 'Power off' }))
    const group = screen.getByRole('group', { name: /confirm power off/i })
    expect(within(group).getByText(/your files are kept/i)).toBeInTheDocument()
    expect(m.stop).not.toHaveBeenCalled()
    expect(screen.getByRole('button', { name: 'Cancel' })).toHaveFocus() // the safe choice
    fireEvent.click(screen.getByRole('button', { name: 'Cancel' }))
    expect(screen.queryByRole('group', { name: /confirm power off/i })).toBeNull()
    expect(screen.getByRole('button', { name: 'Power off' })).toHaveFocus()
    expect(m.stop).not.toHaveBeenCalled()
    expect(screen.getByTestId('terminal')).toBeInTheDocument()
    expect(confirm).not.toHaveBeenCalled()
  })

  it('confirming stops the workspace, tears the panes down at once, and ends on the call to action', async () => {
    const m = await renderPage(running())
    let release!: (s: WorkspaceStatus) => void
    m.stop.mockImplementation(() => new Promise<WorkspaceStatus>((r) => (release = r)))
    fireEvent.click(screen.getByRole('button', { name: 'Power off' }))
    fireEvent.click(screen.getByRole('button', { name: /yes, power off/i }))
    expect(m.stop).toHaveBeenCalledTimes(1)
    // the terminal (and its socket) is gone before the server answers: no reconnect loop
    expect(screen.queryByTestId('terminal')).toBeNull()
    expect(screen.queryByTitle('Remote browser')).toBeNull()
    expect(screen.getByRole('heading', { name: /powering off/i })).toBeInTheDocument()
    await act(async () => release(ws('stopped')))
    expect(screen.getByRole('button', { name: 'Power on' })).toBeInTheDocument()
    expect(screen.getByRole('button', { name: 'Power on' })).toHaveFocus()
  })

  it('a failed power off says why and brings the panes back', async () => {
    const m = await renderPage(running())
    m.stop.mockRejectedValue(new ApiError(409, 'busy'))
    fireEvent.click(screen.getByRole('button', { name: 'Power off' }))
    fireEvent.click(screen.getByRole('button', { name: /yes, power off/i }))
    await advance(0)
    expect(screen.getByRole('alert')).toHaveTextContent(/busy/i)
    expect(screen.getByTestId('terminal')).toBeInTheDocument()
  })

  it('is disabled when there is nothing to power off', async () => {
    await renderPage(ws('stopped'))
    expect(screen.getByRole('button', { name: 'Power off' })).toBeDisabled()
  })
})

describe('Log out', () => {
  it('logs out through the API and tells the app', async () => {
    const m = await renderPage(running())
    fireEvent.click(screen.getByRole('button', { name: 'Log out' }))
    await advance(0)
    expect(m.logout).toHaveBeenCalledTimes(1)
    expect(m.onLoggedOut).toHaveBeenCalledTimes(1)
  })

  it("leaves no neko session token behind", async () => {
    const m = await renderPage(running())
    window.localStorage.setItem(NEKO_SESSION_KEY, 'token')
    fireEvent.click(screen.getByRole('button', { name: 'Log out' }))
    await advance(0)
    expect(m.onLoggedOut).toHaveBeenCalledTimes(1)
    expect(window.localStorage.getItem(NEKO_SESSION_KEY)).toBeNull()
  })

  it('keeps the token if logging out failed (the session is still live)', async () => {
    const m = await renderPage(running())
    m.logout.mockRejectedValue(new ApiError(500, 'internal error'))
    window.localStorage.setItem(NEKO_SESSION_KEY, 'token')
    fireEvent.click(screen.getByRole('button', { name: 'Log out' }))
    await advance(0)
    expect(window.localStorage.getItem(NEKO_SESSION_KEY)).toBe('token')
  })

  it('stays on the page and says so when logging out fails', async () => {
    const m = await renderPage(running())
    m.logout.mockRejectedValue(new ApiError(500, 'internal error'))
    fireEvent.click(screen.getByRole('button', { name: 'Log out' }))
    await advance(0)
    expect(m.onLoggedOut).not.toHaveBeenCalled()
    expect(screen.getByRole('alert')).toHaveTextContent(/could not log out/i)
  })
})

describe('accessibility', () => {
  it('announces state changes in a polite live region', async () => {
    const m = await renderPage(ws('stopped'))
    const live = document.querySelector('[aria-live="polite"]')!
    expect(live).toHaveTextContent(/stopped/i)
    m.start.mockResolvedValue(ws('starting'))
    fireEvent.click(screen.getByRole('button', { name: 'Power on' }))
    await advance(0)
    expect(live).toHaveTextContent(/starting/i)
  })
})
