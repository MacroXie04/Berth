import { act, renderHook } from '@testing-library/react'
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'
import { ApiError, type Api } from './api'
import type { Health, WorkspaceState, WorkspaceStatus } from './types'
import { useWorkspace } from './useWorkspace'

function ws(state: WorkspaceState, health: Health = 'none'): WorkspaceStatus {
  return { state, health, mem_limit_mb: 2048, cpu_limit: 2, last_active_at: null, last_stopped_at: null, usage: null }
}

interface Mock {
  api: Api
  workspace: ReturnType<typeof vi.fn>
  start: ReturnType<typeof vi.fn>
  stop: ReturnType<typeof vi.fn>
  heartbeat: ReturnType<typeof vi.fn>
  set(s: WorkspaceStatus): void
}

function mockApi(initial: WorkspaceStatus): Mock {
  let current = initial
  const workspace = vi.fn(async () => current)
  const start = vi.fn(async () => ws('starting'))
  const stop = vi.fn(async () => ws('stopped'))
  const heartbeat = vi.fn(async () => {})
  return {
    api: { workspace, start, stop, heartbeat } as unknown as Api,
    workspace,
    start,
    stop,
    heartbeat,
    set: (s) => {
      current = s
    },
  }
}

let visibility: 'visible' | 'hidden' = 'visible'
function setVisibility(v: 'visible' | 'hidden') {
  visibility = v
  document.dispatchEvent(new Event('visibilitychange'))
}

/** Advance fake time and let promises settle, inside act. */
async function advance(ms: number) {
  await act(async () => {
    await vi.advanceTimersByTimeAsync(ms)
  })
}

beforeEach(() => {
  vi.useFakeTimers()
  visibility = 'visible'
  Object.defineProperty(document, 'visibilityState', { configurable: true, get: () => visibility })
})
afterEach(() => {
  vi.useRealTimers()
})

describe('polling', () => {
  it('reads the status on mount', async () => {
    const m = mockApi(ws('running', 'healthy'))
    const { result } = renderHook(() => useWorkspace(m.api))
    expect(result.current.status).toBeNull()
    await advance(0)
    expect(m.workspace).toHaveBeenCalledTimes(1)
    expect(result.current.status?.state).toBe('running')
  })

  it('polls every 10 s while running', async () => {
    const m = mockApi(ws('running', 'healthy'))
    renderHook(() => useWorkspace(m.api))
    await advance(0)
    expect(m.workspace).toHaveBeenCalledTimes(1)
    await advance(9_900)
    expect(m.workspace).toHaveBeenCalledTimes(1)
    await advance(100)
    expect(m.workspace).toHaveBeenCalledTimes(2)
    await advance(10_000)
    expect(m.workspace).toHaveBeenCalledTimes(3)
  })

  it('polls every 2 s while starting, then slows to 10 s once running', async () => {
    const m = mockApi(ws('starting'))
    const { result } = renderHook(() => useWorkspace(m.api))
    await advance(0)
    await advance(2_000)
    await advance(2_000)
    expect(m.workspace).toHaveBeenCalledTimes(3)
    m.set(ws('running', 'healthy'))
    await advance(2_000)
    expect(result.current.status?.state).toBe('running')
    expect(m.workspace).toHaveBeenCalledTimes(4)
    await advance(2_000)
    expect(m.workspace).toHaveBeenCalledTimes(4) // no longer every 2 s
    await advance(8_000)
    expect(m.workspace).toHaveBeenCalledTimes(5)
  })

  it.each(['offline', 'stopped'] as const)('does not poll while %s', async (state) => {
    const m = mockApi(ws(state))
    renderHook(() => useWorkspace(m.api))
    await advance(0)
    await advance(120_000)
    expect(m.workspace).toHaveBeenCalledTimes(1)
  })

  it('stops polling when unmounted', async () => {
    const m = mockApi(ws('running', 'healthy'))
    const { unmount } = renderHook(() => useWorkspace(m.api))
    await advance(0)
    unmount()
    await advance(120_000)
    expect(m.workspace).toHaveBeenCalledTimes(1)
  })

  it('does not poll while the tab is hidden, and refreshes at once when it returns', async () => {
    const m = mockApi(ws('running', 'healthy'))
    renderHook(() => useWorkspace(m.api))
    await advance(0)
    act(() => setVisibility('hidden'))
    await advance(60_000)
    expect(m.workspace).toHaveBeenCalledTimes(1)
    act(() => setVisibility('visible'))
    await advance(0)
    expect(m.workspace).toHaveBeenCalledTimes(2)
  })

  it('does nothing at all when mounted in a hidden tab, until it becomes visible', async () => {
    visibility = 'hidden'
    const m = mockApi(ws('running', 'healthy'))
    renderHook(() => useWorkspace(m.api))
    await advance(60_000)
    expect(m.workspace).not.toHaveBeenCalled()
    expect(m.heartbeat).not.toHaveBeenCalled()
    act(() => setVisibility('visible'))
    await advance(0)
    expect(m.workspace).toHaveBeenCalledTimes(1)
  })

  it('keeps the last status and reports a poll error, then recovers', async () => {
    const m = mockApi(ws('running', 'healthy'))
    const { result } = renderHook(() => useWorkspace(m.api))
    await advance(0)
    m.workspace.mockRejectedValueOnce(new ApiError(0, 'Cannot reach the server. Check your connection.'))
    await advance(10_000)
    expect(result.current.status?.state).toBe('running')
    expect(result.current.pollError).toMatch(/cannot reach/i)
    await advance(10_000)
    expect(result.current.pollError).toBeNull()
  })

  it('retries when the very first request fails', async () => {
    const m = mockApi(ws('running', 'healthy'))
    m.workspace.mockRejectedValueOnce(new ApiError(502, 'The server is unavailable (HTTP 502). Try again in a moment.'))
    const { result } = renderHook(() => useWorkspace(m.api))
    await advance(0)
    expect(result.current.status).toBeNull()
    await advance(2_000)
    expect(result.current.status?.state).toBe('running')
  })

  it('stops polling after a 401 (the app is about to show the login page)', async () => {
    const m = mockApi(ws('running', 'healthy'))
    renderHook(() => useWorkspace(m.api))
    await advance(0)
    m.workspace.mockRejectedValue(new ApiError(401, 'not logged in'))
    await advance(10_000)
    expect(m.workspace).toHaveBeenCalledTimes(2)
    await advance(120_000)
    expect(m.workspace).toHaveBeenCalledTimes(2)
  })
})

describe('heartbeat', () => {
  it('beats when running and visible: once at once, then every 30 s', async () => {
    const m = mockApi(ws('running', 'healthy'))
    renderHook(() => useWorkspace(m.api))
    await advance(0)
    expect(m.heartbeat).toHaveBeenCalledTimes(1)
    await advance(29_900)
    expect(m.heartbeat).toHaveBeenCalledTimes(1)
    await advance(100)
    expect(m.heartbeat).toHaveBeenCalledTimes(2)
    await advance(30_000)
    expect(m.heartbeat).toHaveBeenCalledTimes(3)
  })

  it('also beats while starting', async () => {
    const m = mockApi(ws('starting'))
    renderHook(() => useWorkspace(m.api))
    await advance(0)
    await advance(30_000)
    expect(m.heartbeat).toHaveBeenCalledTimes(2)
  })

  it.each(['offline', 'stopped'] as const)('does not beat while %s', async (state) => {
    const m = mockApi(ws(state))
    renderHook(() => useWorkspace(m.api))
    await advance(0)
    await advance(120_000)
    expect(m.heartbeat).not.toHaveBeenCalled()
  })

  it('stops while the tab is hidden and resumes (with an immediate beat) when visible again', async () => {
    const m = mockApi(ws('running', 'healthy'))
    renderHook(() => useWorkspace(m.api))
    await advance(0)
    expect(m.heartbeat).toHaveBeenCalledTimes(1)
    act(() => setVisibility('hidden'))
    await advance(120_000)
    expect(m.heartbeat).toHaveBeenCalledTimes(1)
    act(() => setVisibility('visible'))
    await advance(0)
    expect(m.heartbeat).toHaveBeenCalledTimes(2)
    await advance(30_000)
    expect(m.heartbeat).toHaveBeenCalledTimes(3)
  })

  it('stops on unmount', async () => {
    const m = mockApi(ws('running', 'healthy'))
    const { unmount } = renderHook(() => useWorkspace(m.api))
    await advance(0)
    unmount()
    await advance(120_000)
    expect(m.heartbeat).toHaveBeenCalledTimes(1)
  })

  it('stops once the workspace is no longer running', async () => {
    const m = mockApi(ws('running', 'healthy'))
    renderHook(() => useWorkspace(m.api))
    await advance(0)
    m.set(ws('stopped'))
    await advance(10_000) // the poll notices
    const n = m.heartbeat.mock.calls.length
    await advance(120_000)
    expect(m.heartbeat).toHaveBeenCalledTimes(n)
  })

  it('a failing heartbeat is swallowed', async () => {
    const m = mockApi(ws('running', 'healthy'))
    m.heartbeat.mockRejectedValue(new ApiError(500, 'internal error'))
    const { result } = renderHook(() => useWorkspace(m.api))
    await advance(0)
    expect(result.current.actionError).toBeNull()
  })
})

describe('Power on / Power off', () => {
  it('Power on calls start, goes busy, and takes the answer', async () => {
    const m = mockApi(ws('stopped'))
    let release!: (s: WorkspaceStatus) => void
    m.start.mockImplementation(() => new Promise<WorkspaceStatus>((r) => (release = r)))
    const { result } = renderHook(() => useWorkspace(m.api))
    await advance(0)
    act(() => result.current.powerOn())
    expect(m.start).toHaveBeenCalledTimes(1)
    expect(result.current.busy).toBe('starting')
    await act(async () => release(ws('starting')))
    expect(result.current.busy).toBeNull()
    expect(result.current.status?.state).toBe('starting')
  })

  it('a second Power on while one is in flight is ignored', async () => {
    const m = mockApi(ws('stopped'))
    m.start.mockImplementation(() => new Promise<WorkspaceStatus>(() => {}))
    const { result } = renderHook(() => useWorkspace(m.api))
    await advance(0)
    act(() => result.current.powerOn())
    act(() => result.current.powerOn())
    expect(m.start).toHaveBeenCalledTimes(1)
  })

  it('a poll answer that was overtaken by Power on does not flip the state back', async () => {
    const m = mockApi(ws('stopped'))
    const { result } = renderHook(() => useWorkspace(m.api))
    await advance(0)
    // a status request is in flight (and will answer "stopped") ...
    let answer!: (s: WorkspaceStatus) => void
    m.workspace.mockImplementationOnce(() => new Promise<WorkspaceStatus>((r) => (answer = r)))
    act(() => document.dispatchEvent(new Event('visibilitychange'))) // no change, just to be safe
    act(() => setVisibility('hidden'))
    act(() => setVisibility('visible'))
    await advance(0)
    // ... then the user powers on and gets "starting"
    await act(async () => result.current.powerOn())
    expect(result.current.status?.state).toBe('starting')
    await act(async () => answer(ws('stopped')))
    expect(result.current.status?.state).toBe('starting')
  })

  it('Power on failing with 503 shows a readable error and keeps the call to action', async () => {
    const m = mockApi(ws('offline'))
    m.start.mockRejectedValue(new ApiError(503, 'the server has no capacity for another workspace'))
    const { result } = renderHook(() => useWorkspace(m.api))
    await advance(0)
    await act(async () => result.current.powerOn())
    expect(result.current.actionError).toMatch(/no capacity/)
    expect(result.current.busy).toBeNull()
    expect(result.current.status?.state).toBe('offline')
    act(() => result.current.dismissError())
    expect(result.current.actionError).toBeNull()
  })

  it('after a 409 the status is read again (somebody else may be starting it)', async () => {
    const m = mockApi(ws('offline'))
    m.start.mockRejectedValue(new ApiError(409, 'busy'))
    const { result } = renderHook(() => useWorkspace(m.api))
    await advance(0)
    m.set(ws('starting'))
    await act(async () => result.current.powerOn())
    await advance(0)
    expect(result.current.actionError).toMatch(/busy/i)
    expect(result.current.status?.state).toBe('starting')
  })

  it('Power off goes busy "stopping", stops heartbeats, and lands on stopped', async () => {
    const m = mockApi(ws('running', 'healthy'))
    let release!: (s: WorkspaceStatus) => void
    m.stop.mockImplementation(() => new Promise<WorkspaceStatus>((r) => (release = r)))
    const { result } = renderHook(() => useWorkspace(m.api))
    await advance(0)
    expect(m.heartbeat).toHaveBeenCalledTimes(1)
    act(() => result.current.powerOff())
    expect(result.current.busy).toBe('stopping')
    await advance(60_000)
    expect(m.heartbeat).toHaveBeenCalledTimes(1)
    expect(m.workspace).toHaveBeenCalledTimes(1) // no polling during the request either
    await act(async () => release(ws('stopped')))
    expect(result.current.busy).toBeNull()
    expect(result.current.status?.state).toBe('stopped')
    await advance(60_000)
    expect(m.workspace).toHaveBeenCalledTimes(1)
  })

  it('Power off failing reports it and re-reads the real state', async () => {
    const m = mockApi(ws('running', 'healthy'))
    m.stop.mockRejectedValue(new ApiError(409, 'busy'))
    const { result } = renderHook(() => useWorkspace(m.api))
    await advance(0)
    await act(async () => result.current.powerOff())
    await advance(0)
    expect(result.current.actionError).toMatch(/busy/i)
    expect(result.current.status?.state).toBe('running')
    expect(m.workspace.mock.calls.length).toBeGreaterThanOrEqual(2)
  })

  it('a 401 during Power on is left to the app (no confusing inline error)', async () => {
    const m = mockApi(ws('offline'))
    m.start.mockRejectedValue(new ApiError(401, 'not logged in'))
    const { result } = renderHook(() => useWorkspace(m.api))
    await advance(0)
    await act(async () => result.current.powerOn())
    expect(result.current.actionError).toBeNull()
  })
})
