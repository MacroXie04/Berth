import { describe, expect, it } from 'vitest'
import { ApiError } from './api'
import type { Health, WorkspaceState, WorkspaceStatus } from './types'
import {
  HEARTBEAT_MS,
  POLL_RETRY_MS,
  POLL_RUNNING_MS,
  POLL_STARTING_MS,
  announcement,
  badgeFor,
  canPowerOff,
  canPowerOn,
  ctaCopy,
  elapsedSeconds,
  heartbeatActive,
  isUnhealthy,
  pollDelay,
  startErrorMessage,
  stopErrorMessage,
  viewFor,
} from './workspaceView'

function ws(state: WorkspaceState, health: Health = 'none'): WorkspaceStatus {
  return { state, health, mem_limit_mb: 2048, cpu_limit: 2, last_active_at: null, last_stopped_at: null, usage: null }
}

describe('viewFor', () => {
  it('shows the call to action for offline and stopped', () => {
    expect(viewFor(ws('offline'), null)).toBe('cta')
    expect(viewFor(ws('stopped'), null)).toBe('cta')
  })
  it('shows progress while starting', () => {
    expect(viewFor(ws('starting'), null)).toBe('starting')
  })
  it('mounts the panes only when running and ready', () => {
    expect(viewFor(ws('running', 'healthy'), null)).toBe('panes')
    expect(viewFor(ws('running', 'none'), null)).toBe('panes')
    expect(viewFor(ws('running', 'starting'), null)).toBe('starting')
  })
  it('keeps the panes when running but unhealthy', () => {
    expect(viewFor(ws('running', 'unhealthy'), null)).toBe('panes')
  })
  it('loading until the first answer', () => {
    expect(viewFor(null, null)).toBe('loading')
  })
  it('a request in flight wins over the last known state', () => {
    expect(viewFor(ws('running', 'healthy'), 'stopping')).toBe('stopping')
    expect(viewFor(ws('stopped'), 'starting')).toBe('starting')
    expect(viewFor(null, 'starting')).toBe('starting')
  })
})

describe('badgeFor', () => {
  it('maps every state to a label and a tone', () => {
    expect(badgeFor(ws('running', 'healthy'), null)).toEqual({ label: 'Running', tone: 'ok' })
    expect(badgeFor(ws('starting'), null)).toEqual({ label: 'Starting', tone: 'busy' })
    expect(badgeFor(ws('stopped'), null)).toEqual({ label: 'Stopped', tone: 'idle' })
    expect(badgeFor(ws('offline'), null)).toEqual({ label: 'Offline', tone: 'idle' })
    expect(badgeFor(null, null).label).toBe('Checking')
    expect(badgeFor(ws('running', 'healthy'), 'stopping').label).toBe('Powering off')
  })
})

describe('banners and buttons', () => {
  it('flags unhealthy only for a running workspace', () => {
    expect(isUnhealthy(ws('running', 'unhealthy'))).toBe(true)
    expect(isUnhealthy(ws('running', 'healthy'))).toBe(false)
    expect(isUnhealthy(ws('stopped', 'unhealthy'))).toBe(false)
    expect(isUnhealthy(null)).toBe(false)
  })
  it('power off while starting or running, power on while offline or stopped, neither while busy', () => {
    expect(canPowerOff(ws('running'), null)).toBe(true)
    expect(canPowerOff(ws('starting'), null)).toBe(true)
    expect(canPowerOff(ws('stopped'), null)).toBe(false)
    expect(canPowerOff(ws('running'), 'stopping')).toBe(false)
    expect(canPowerOn(ws('offline'), null)).toBe(true)
    expect(canPowerOn(ws('stopped'), null)).toBe(true)
    expect(canPowerOn(ws('running'), null)).toBe(false)
    expect(canPowerOn(ws('stopped'), 'starting')).toBe(false)
    expect(canPowerOn(null, null)).toBe(false)
  })
  it('announces view changes', () => {
    expect(announcement('panes', ws('running'))).toMatch(/running/)
    expect(announcement('cta', ws('stopped'))).toMatch(/stopped/)
    expect(announcement('cta', ws('offline'))).toMatch(/offline/)
    expect(announcement('starting', null)).toMatch(/starting/i)
    expect(announcement('loading', null)).toBe('')
  })
})

describe('scheduling', () => {
  it('polls every 2 s while starting, every 10 s while running, never while offline or stopped', () => {
    expect(pollDelay('starting')).toBe(POLL_STARTING_MS)
    expect(pollDelay('running')).toBe(POLL_RUNNING_MS)
    expect(pollDelay('stopped')).toBeNull()
    expect(pollDelay('offline')).toBeNull()
    expect(POLL_STARTING_MS).toBe(2000)
    expect(POLL_RUNNING_MS).toBe(10_000)
  })
  it('retries quickly while the state is still unknown', () => {
    expect(pollDelay(null)).toBe(POLL_STARTING_MS)
  })
  it('while the last request failed, asks again every 5 s whatever the state (offline and stopped included)', () => {
    expect(POLL_RETRY_MS).toBe(5000)
    expect(pollDelay('offline', true)).toBe(POLL_RETRY_MS)
    expect(pollDelay('stopped', true)).toBe(POLL_RETRY_MS)
    expect(pollDelay('running', true)).toBe(POLL_RETRY_MS) // sooner than the usual 10 s
    expect(pollDelay('starting', true)).toBe(POLL_STARTING_MS) // already faster
    expect(pollDelay(null, true)).toBe(POLL_STARTING_MS)
  })
  it('a request that did not fail changes nothing', () => {
    expect(pollDelay('stopped', false)).toBeNull()
    expect(pollDelay('running', false)).toBe(POLL_RUNNING_MS)
  })
  it('heartbeats only while starting/running AND visible AND not powering off', () => {
    expect(HEARTBEAT_MS).toBe(30_000)
    expect(heartbeatActive('running', true, null)).toBe(true)
    expect(heartbeatActive('starting', true, null)).toBe(true)
    expect(heartbeatActive('running', false, null)).toBe(false)
    expect(heartbeatActive('stopped', true, null)).toBe(false)
    expect(heartbeatActive('offline', true, null)).toBe(false)
    expect(heartbeatActive(null, true, null)).toBe(false)
    expect(heartbeatActive('running', true, 'stopping')).toBe(false)
  })
  it('counts whole elapsed seconds', () => {
    expect(elapsedSeconds(1000, 4999)).toBe(3)
    expect(elapsedSeconds(5000, 1000)).toBe(0)
  })
})

describe('start / stop error messages', () => {
  it('503: capacity or container service, with a hint to retry later', () => {
    const m = startErrorMessage(new ApiError(503, 'the server has no capacity for another workspace'))
    expect(m).toMatch(/no capacity/)
    expect(m).toMatch(/try again/i)
  })
  it('409: busy, retry shortly', () => {
    expect(startErrorMessage(new ApiError(409, 'busy'))).toMatch(/busy/i)
    expect(stopErrorMessage(new ApiError(409, 'busy'))).toMatch(/busy/i)
  })
  it('415 and everything else: the server message, readable', () => {
    expect(startErrorMessage(new ApiError(415, 'Content-Type must be application/json'))).toContain(
      'Content-Type must be application/json',
    )
    expect(startErrorMessage(new ApiError(500, 'internal error'))).toContain('internal error')
  })
  it('network failure and unknown errors', () => {
    expect(startErrorMessage(new ApiError(0, 'Cannot reach the server. Check your connection.'))).toMatch(
      /cannot reach/i,
    )
    expect(startErrorMessage(new Error('boom'))).toBe('Could not start the workspace.')
    expect(stopErrorMessage(new Error('boom'))).toBe('Could not power off the workspace.')
  })
})

describe('ctaCopy', () => {
  const fresh = ws('offline')
  const used = (extra: Partial<WorkspaceStatus>) => ({ ...ws('offline'), ...extra })

  it('a workspace that was never started says so, and does not claim a container was removed', () => {
    const c = ctaCopy(fresh)
    expect(c.title).toBe('Your workspace is not running')
    expect(c.text).toBe(
      'Your workspace has not been started yet. Power on creates it; your files are kept from then on.',
    )
  })

  it.each([
    ['last_stopped_at', { last_stopped_at: '2026-05-01 10:00:00' }],
    ['last_active_at', { last_active_at: '2026-05-01 10:00:00' }],
    ['both', { last_active_at: '2026-05-01 10:00:00', last_stopped_at: '2026-05-01 10:30:00' }],
  ])('offline with %s set (it ran before) uses neutral wording', (_n, extra) => {
    expect(ctaCopy(used(extra)).text).toBe(
      'There is no running container right now. Power on creates a fresh one; everything in your home directory is kept.',
    )
  })

  it('stopped keeps its own text and promise', () => {
    const c = ctaCopy(ws('stopped'))
    expect(c.title).toBe('Your workspace is powered off')
    expect(c.text).toMatch(/^It was stopped, by you or after sitting idle\./)
    expect(c.text).toMatch(/Your files are kept/)
  })

  it('a stopped workspace is never described as "never started", even with no timestamps', () => {
    expect(ctaCopy(ws('stopped')).text).not.toMatch(/not been started/)
  })
})
