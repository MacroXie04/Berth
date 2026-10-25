import { act, renderHook } from '@testing-library/react'
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'
import { ApiError, type Api } from './api'
import type { Health, WorkspaceState, WorkspaceStatus } from './types'
import { useWorkspace } from './useWorkspace'

function ws(state: WorkspaceState, health: Health = 'none', username?: string): WorkspaceStatus {
  return {
    state,
    health,
    mem_limit_mb: 2048,
    cpu_limit: 2,
    last_active_at: null,
    last_stopped_at: null,
    usage: null,
    ...(username === undefined ? {} : { username }),
  }
}

/** A promise the test settles by hand. */
function deferred<T>() {
  let resolve!: (v: T) => void
  let reject!: (e: unknown) => void
  const promise = new Promise<T>((res, rej) => {
    resolve = res
    reject = rej
  })
  return { promise, resolve, reject }
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

  it('a hidden tab polls once a minute, and refreshes at once when it returns', async () => {
    const m = mockApi(ws('running', 'healthy'))
    renderHook(() => useWorkspace(m.api))
    await advance(0)
    act(() => setVisibility('hidden'))
    await advance(0)
    expect(m.workspace).toHaveBeenCalledTimes(1) // hiding the tab asks nothing
    await advance(59_900)
    expect(m.workspace).toHaveBeenCalledTimes(1)
    await advance(100)
    expect(m.workspace).toHaveBeenCalledTimes(2) // not every 10 s: once a minute
    await advance(60_000)
    expect(m.workspace).toHaveBeenCalledTimes(3)
    act(() => setVisibility('visible'))
    await advance(0)
    expect(m.workspace).toHaveBeenCalledTimes(4)
    await advance(10_000)
    expect(m.workspace).toHaveBeenCalledTimes(5) // the normal pace again
  })

  it('mounted in a hidden tab: asks once at once (it must know the state), then once a minute', async () => {
    visibility = 'hidden'
    const m = mockApi(ws('running', 'healthy'))
    const { result } = renderHook(() => useWorkspace(m.api))
    await advance(0)
    expect(m.workspace).toHaveBeenCalledTimes(1)
    expect(result.current.status?.state).toBe('running')
    await advance(60_000)
    expect(m.workspace).toHaveBeenCalledTimes(2)
    act(() => setVisibility('visible'))
    await advance(0)
    expect(m.workspace).toHaveBeenCalledTimes(3)
  })

  it('a hidden tab does not poll an offline or stopped workspace', async () => {
    visibility = 'hidden'
    const m = mockApi(ws('stopped'))
    renderHook(() => useWorkspace(m.api))
    await advance(0)
    await advance(300_000)
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

  it('keeps beating while the tab is hidden: an open page is use', async () => {
    const m = mockApi(ws('running', 'healthy'))
    renderHook(() => useWorkspace(m.api))
    await advance(0)
    expect(m.heartbeat).toHaveBeenCalledTimes(1)
    act(() => setVisibility('hidden'))
    await advance(0)
    expect(m.heartbeat).toHaveBeenCalledTimes(1) // hiding does not restart the cadence
    await advance(120_000)
    expect(m.heartbeat).toHaveBeenCalledTimes(5) // every 30 s
    act(() => setVisibility('visible'))
    await advance(0)
    expect(m.heartbeat).toHaveBeenCalledTimes(5) // coming back does not add a beat
    await advance(30_000)
    expect(m.heartbeat).toHaveBeenCalledTimes(6)
  })

  it('a hidden tab beats from the start when it was opened in the background', async () => {
    visibility = 'hidden'
    const m = mockApi(ws('running', 'healthy'))
    renderHook(() => useWorkspace(m.api))
    await advance(0)
    expect(m.heartbeat).toHaveBeenCalledTimes(1)
    await advance(30_000)
    expect(m.heartbeat).toHaveBeenCalledTimes(2)
  })

  it('a hidden tab learns that the workspace was powered off and stops beating', async () => {
    const m = mockApi(ws('running', 'healthy'))
    const { result } = renderHook(() => useWorkspace(m.api))
    await advance(0)
    act(() => setVisibility('hidden'))
    m.set(ws('stopped')) // the server's reaper (or an admin) powered it off
    await advance(60_000) // the hidden tab's next poll
    expect(result.current.status?.state).toBe('stopped')
    const beats = m.heartbeat.mock.calls.length
    await advance(120_000)
    expect(m.heartbeat).toHaveBeenCalledTimes(beats) // no more beats for a workspace that is not up
  })

  it('a hidden tab of another account notices the change and stops', async () => {
    const m = mockApi(ws('running', 'healthy', 'alice'))
    const onMismatch = vi.fn()
    renderHook(() => useWorkspace(m.api, { username: 'alice', onMismatch }))
    await advance(0)
    act(() => setVisibility('hidden'))
    m.set(ws('running', 'healthy', 'bob')) // another tab logged in as bob; the cookie is shared
    await advance(60_000)
    expect(onMismatch).toHaveBeenCalledTimes(1)
    const beats = m.heartbeat.mock.calls.length
    await advance(120_000)
    expect(m.heartbeat).toHaveBeenCalledTimes(beats) // alice's page no longer keeps bob's workspace up
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

describe('a failed status request is retried until it works, whatever the state', () => {
  const down = () => new ApiError(0, 'Cannot reach the server. Check your connection.')

  it('a stopped workspace whose tab is shown again while the network is down: retries every 5 s, then stops asking', async () => {
    const m = mockApi(ws('stopped'))
    const { result } = renderHook(() => useWorkspace(m.api))
    await advance(0)
    expect(m.workspace).toHaveBeenCalledTimes(1)
    act(() => setVisibility('hidden'))
    m.workspace.mockRejectedValue(down())
    act(() => setVisibility('visible'))
    await advance(0)
    expect(m.workspace).toHaveBeenCalledTimes(2)
    expect(result.current.pollError).toMatch(/cannot reach/i)
    expect(result.current.status?.state).toBe('stopped')

    await advance(4_900)
    expect(m.workspace).toHaveBeenCalledTimes(2)
    await advance(100)
    expect(m.workspace).toHaveBeenCalledTimes(3) // 5 s after the failure
    await advance(5_000)
    expect(m.workspace).toHaveBeenCalledTimes(4) // and again, as long as it fails

    m.workspace.mockImplementation(async () => ws('stopped'))
    await advance(5_000)
    expect(m.workspace).toHaveBeenCalledTimes(5)
    expect(result.current.pollError).toBeNull()
    await advance(120_000)
    expect(m.workspace).toHaveBeenCalledTimes(5) // healthy and stopped again: nothing to poll
  })

  it('an offline workspace is retried too', async () => {
    const m = mockApi(ws('offline'))
    m.workspace.mockRejectedValueOnce(down())
    const { result } = renderHook(() => useWorkspace(m.api))
    await advance(0)
    expect(result.current.status).toBeNull() // the very first request failed
    await advance(2_000) // unknown state: the fast retry
    expect(result.current.status?.state).toBe('offline')
    m.workspace.mockRejectedValueOnce(down())
    act(() => setVisibility('hidden'))
    act(() => setVisibility('visible'))
    await advance(0)
    expect(result.current.pollError).not.toBeNull()
    await advance(5_000)
    expect(result.current.pollError).toBeNull()
  })

  it('a running workspace is asked again after 5 s instead of 10 s while the last answer failed', async () => {
    const m = mockApi(ws('running', 'healthy'))
    const { result } = renderHook(() => useWorkspace(m.api))
    await advance(0)
    m.workspace.mockRejectedValueOnce(down())
    await advance(10_000)
    expect(result.current.pollError).not.toBeNull()
    await advance(5_000)
    expect(result.current.pollError).toBeNull()
  })

  it('a failed Power on whose status re-read fails too keeps asking', async () => {
    const m = mockApi(ws('offline'))
    m.start.mockRejectedValue(new ApiError(503, 'the server has no capacity for another workspace'))
    const { result } = renderHook(() => useWorkspace(m.api))
    await advance(0)
    m.workspace.mockRejectedValue(down())
    await act(async () => result.current.powerOn())
    await advance(0)
    expect(result.current.pollError).not.toBeNull()
    const n = m.workspace.mock.calls.length
    await advance(5_000)
    expect(m.workspace.mock.calls.length).toBe(n + 1)
    m.workspace.mockImplementation(async () => ws('offline'))
    await advance(5_000)
    expect(result.current.pollError).toBeNull()
  })

  it('a fresh answer from Power off clears the "not up to date" flag of an earlier failed poll', async () => {
    const m = mockApi(ws('running', 'healthy'))
    const { result } = renderHook(() => useWorkspace(m.api))
    await advance(0)
    m.workspace.mockRejectedValueOnce(down())
    await advance(10_000)
    expect(result.current.pollError).not.toBeNull()
    await act(async () => result.current.powerOff())
    expect(result.current.status?.state).toBe('stopped')
    expect(result.current.pollError).toBeNull()
    await advance(120_000)
    expect(result.current.pollError).toBeNull() // and nothing polls a stopped workspace into a new error
  })
})

describe('status answers that arrive out of order', () => {
  it('two overlapping requests: the older answer does not overwrite the newer one', async () => {
    const m = mockApi(ws('running', 'healthy'))
    const { result } = renderHook(() => useWorkspace(m.api))
    await advance(0)
    const timerPoll = deferred<WorkspaceStatus>()
    const shownAgain = deferred<WorkspaceStatus>()
    m.workspace.mockImplementationOnce(() => timerPoll.promise).mockImplementationOnce(() => shownAgain.promise)
    await advance(10_000) // the timer's request is under way ...
    act(() => setVisibility('hidden'))
    act(() => setVisibility('visible')) // ... and the tab coming back asks again
    await advance(0)
    expect(m.workspace).toHaveBeenCalledTimes(3)
    await act(async () => shownAgain.resolve(ws('stopped')))
    expect(result.current.status?.state).toBe('stopped')
    await act(async () => timerPoll.resolve(ws('running', 'healthy'))) // older, answered last
    expect(result.current.status?.state).toBe('stopped')
    await advance(60_000)
    expect(result.current.status?.state).toBe('stopped')
  })

  it('an older failure does not flag a newer success as "not up to date"', async () => {
    const m = mockApi(ws('running', 'healthy'))
    const { result } = renderHook(() => useWorkspace(m.api))
    await advance(0)
    const timerPoll = deferred<WorkspaceStatus>()
    m.workspace.mockImplementationOnce(() => timerPoll.promise)
    await advance(10_000)
    act(() => setVisibility('hidden'))
    act(() => setVisibility('visible'))
    await advance(0)
    expect(result.current.pollError).toBeNull()
    await act(async () => timerPoll.reject(new ApiError(0, 'Cannot reach the server. Check your connection.')))
    expect(result.current.pollError).toBeNull()
    expect(result.current.status?.state).toBe('running')
  })

  it('a request that was under way when Power on was pressed and answers before Power on does is dropped as well', async () => {
    const m = mockApi(ws('stopped'))
    const { result } = renderHook(() => useWorkspace(m.api))
    await advance(0)
    const poll = deferred<WorkspaceStatus>()
    m.workspace.mockImplementationOnce(() => poll.promise)
    act(() => setVisibility('hidden'))
    act(() => setVisibility('visible')) // a status request is on its way ...
    await advance(0)
    const start = deferred<WorkspaceStatus>()
    m.start.mockImplementation(() => start.promise)
    act(() => result.current.powerOn()) // ... and the user presses Power on
    expect(result.current.busy).toBe('starting')
    await act(async () => poll.resolve(ws('offline'))) // the request answers first, with what was true before the press
    expect(result.current.status?.state).toBe('stopped') // not applied: the action's own answer decides
    await act(async () => start.resolve(ws('starting')))
    expect(result.current.status?.state).toBe('starting')
  })

  it('a request that started during Power off but is answered after it does not undo it', async () => {
    const m = mockApi(ws('running', 'healthy'))
    const { result } = renderHook(() => useWorkspace(m.api))
    await advance(0)
    const stop = deferred<WorkspaceStatus>()
    m.stop.mockImplementation(() => stop.promise)
    act(() => result.current.powerOff())
    expect(result.current.busy).toBe('stopping')
    const late = deferred<WorkspaceStatus>()
    m.workspace.mockImplementationOnce(() => late.promise)
    act(() => setVisibility('hidden'))
    act(() => setVisibility('visible')) // asks while the stop is still running
    await advance(0)
    expect(m.workspace).toHaveBeenCalledTimes(2)
    await act(async () => stop.resolve(ws('stopped')))
    expect(result.current.status?.state).toBe('stopped')
    await act(async () => late.resolve(ws('running', 'healthy'))) // the answer of before the stop
    expect(result.current.status?.state).toBe('stopped')
    expect(result.current.busy).toBeNull()
    await advance(60_000)
    expect(result.current.status?.state).toBe('stopped')
    expect(m.workspace).toHaveBeenCalledTimes(2) // and it did not re-arm a poll for a "running" workspace
  })
})

describe('an answer for another account (another tab logged in as someone else)', () => {
  function setupIdentity(initial: WorkspaceStatus) {
    const m = mockApi(initial)
    const onMismatch = vi.fn()
    const hook = renderHook(() => useWorkspace(m.api, { username: 'alice', onMismatch }))
    return { m, onMismatch, ...hook }
  }

  it('accepts answers for the page\'s own user', async () => {
    const { m, onMismatch, result } = setupIdentity(ws('running', 'healthy', 'alice'))
    await advance(0)
    expect(result.current.status?.state).toBe('running')
    expect(onMismatch).not.toHaveBeenCalled()
    expect(m.heartbeat).toHaveBeenCalledTimes(1)
  })

  it('accepts an answer without a username (an older backend)', async () => {
    const { onMismatch, result } = setupIdentity(ws('running', 'healthy'))
    await advance(0)
    expect(result.current.status?.state).toBe('running')
    expect(onMismatch).not.toHaveBeenCalled()
  })

  it('does nothing without an identity to check against', async () => {
    const m = mockApi(ws('running', 'healthy', 'bob'))
    const { result } = renderHook(() => useWorkspace(m.api))
    await advance(0)
    expect(result.current.status?.state).toBe('running')
  })

  it('the first answer is for someone else: nothing is applied, no heartbeat, one call to onMismatch, no more polling', async () => {
    const { m, onMismatch, result } = setupIdentity(ws('running', 'healthy', 'bob'))
    await advance(0)
    expect(onMismatch).toHaveBeenCalledTimes(1)
    expect(result.current.status).toBeNull()
    await advance(120_000)
    expect(m.heartbeat).not.toHaveBeenCalled()
    expect(m.workspace).toHaveBeenCalledTimes(1)
    expect(onMismatch).toHaveBeenCalledTimes(1)
  })

  it('a later poll answers for someone else: the status is dropped and the heartbeat stops', async () => {
    const { m, onMismatch, result } = setupIdentity(ws('running', 'healthy', 'alice'))
    await advance(0)
    expect(m.heartbeat).toHaveBeenCalledTimes(1)
    m.set(ws('running', 'healthy', 'bob'))
    await advance(10_000)
    expect(onMismatch).toHaveBeenCalledTimes(1)
    expect(result.current.status).toBeNull()
    await advance(120_000)
    expect(m.heartbeat).toHaveBeenCalledTimes(1)
  })

  it('a Power on answered for someone else is not applied, and the page does not act again', async () => {
    const { m, onMismatch, result } = setupIdentity(ws('stopped', 'none', 'alice'))
    await advance(0)
    m.start.mockResolvedValue(ws('starting', 'none', 'bob'))
    await act(async () => result.current.powerOn())
    expect(onMismatch).toHaveBeenCalledTimes(1)
    expect(result.current.status).toBeNull()
    await act(async () => result.current.powerOn())
    await act(async () => result.current.powerOff())
    expect(m.start).toHaveBeenCalledTimes(1)
    expect(m.stop).not.toHaveBeenCalled()
  })

  it('a Power off answered for someone else is not applied either', async () => {
    const { m, onMismatch, result } = setupIdentity(ws('running', 'healthy', 'alice'))
    await advance(0)
    m.stop.mockResolvedValue(ws('stopped', 'none', 'bob'))
    await act(async () => result.current.powerOff())
    expect(onMismatch).toHaveBeenCalledTimes(1)
    expect(result.current.status).toBeNull()
  })

  it('an overtaken answer for someone else is just dropped (the newer answer decides)', async () => {
    const { m, onMismatch, result } = setupIdentity(ws('running', 'healthy', 'alice'))
    await advance(0)
    const old = deferred<WorkspaceStatus>()
    m.workspace.mockImplementationOnce(() => old.promise)
    await advance(10_000)
    act(() => setVisibility('hidden'))
    act(() => setVisibility('visible'))
    await advance(0) // the newer request answers for alice
    await act(async () => old.resolve(ws('running', 'healthy', 'bob')))
    expect(onMismatch).not.toHaveBeenCalled()
    expect(result.current.status?.state).toBe('running')
  })
})
