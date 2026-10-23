import { ApiError } from './api'
import type { Health, WorkspaceState, WorkspaceStatus } from './types'

export const POLL_STARTING_MS = 2000
export const POLL_RUNNING_MS = 10_000
export const HEARTBEAT_MS = 30_000
/** After this long in "starting" the page admits that it is slower than usual. */
export const SLOW_START_SECONDS = 60

/** An action in flight: the start request, or a power-off the user confirmed. */
export type Busy = 'starting' | 'stopping' | null

export type View =
  /** First status not known yet. */
  | 'loading'
  /** Offline or stopped: offer "Power on". */
  | 'cta'
  | 'starting'
  | 'stopping'
  /** Running and ready: terminal + browser. */
  | 'panes'

/**
 * Delay until the next status poll for a workspace in `state`, or null when
 * nothing needs polling (offline/stopped change only when the user acts).
 * Unknown state (no status yet, e.g. the first request failed) retries fast.
 */
export function pollDelay(state: WorkspaceState | null): number | null {
  switch (state) {
    case null:
    case 'starting':
      return POLL_STARTING_MS
    case 'running':
      return POLL_RUNNING_MS
    default:
      return null
  }
}

/** Whether the tab should be sending heartbeats. */
export function heartbeatActive(state: WorkspaceState | null, visible: boolean, busy: Busy): boolean {
  return visible && busy !== 'stopping' && (state === 'starting' || state === 'running')
}

/**
 * What the main area shows. The container's healthcheck decides readiness: a
 * workspace that is "running" but still reports health "starting" has no
 * routes yet, so panes (and the iframe) are not mounted for it. "unhealthy"
 * keeps the panes: the banner explains, the terminal reconnects by itself.
 */
export function viewFor(status: WorkspaceStatus | null, busy: Busy): View {
  if (busy === 'stopping') return 'stopping'
  if (busy === 'starting') return 'starting'
  if (!status) return 'loading'
  switch (status.state) {
    case 'starting':
      return 'starting'
    case 'running':
      return status.health === 'starting' ? 'starting' : 'panes'
    default:
      return 'cta'
  }
}

export type Tone = 'ok' | 'warn' | 'bad' | 'idle' | 'busy'

export interface Badge {
  label: string
  tone: Tone
}

export function badgeFor(status: WorkspaceStatus | null, busy: Busy): Badge {
  if (busy === 'stopping') return { label: 'Powering off', tone: 'busy' }
  if (busy === 'starting') return { label: 'Starting', tone: 'busy' }
  if (!status) return { label: 'Checking', tone: 'idle' }
  switch (status.state) {
    case 'running':
      return status.health === 'starting' ? { label: 'Starting', tone: 'busy' } : { label: 'Running', tone: 'ok' }
    case 'starting':
      return { label: 'Starting', tone: 'busy' }
    case 'stopped':
      return { label: 'Stopped', tone: 'idle' }
    case 'offline':
      return { label: 'Offline', tone: 'idle' }
  }
}

export function healthLabel(health: Health): string | null {
  switch (health) {
    case 'healthy':
      return 'Healthy'
    case 'unhealthy':
      return 'Unhealthy'
    case 'starting':
      return 'Checking health'
    default:
      return null
  }
}

export function isUnhealthy(status: WorkspaceStatus | null): boolean {
  return status?.state === 'running' && status.health === 'unhealthy'
}

/** Power off is possible while the container is starting or running. */
export function canPowerOff(status: WorkspaceStatus | null, busy: Busy): boolean {
  return busy === null && (status?.state === 'running' || status?.state === 'starting')
}

export function canPowerOn(status: WorkspaceStatus | null, busy: Busy): boolean {
  return busy === null && (status?.state === 'offline' || status?.state === 'stopped')
}

/** Text for the polite live region: announced when the view changes. */
export function announcement(view: View, status: WorkspaceStatus | null): string {
  switch (view) {
    case 'loading':
      return ''
    case 'starting':
      return 'Starting your workspace'
    case 'stopping':
      return 'Powering off your workspace'
    case 'panes':
      return 'Your workspace is running'
    case 'cta':
      return status?.state === 'stopped' ? 'Your workspace is stopped' : 'Your workspace is offline'
  }
}

/** Readable text for a failed "Power on". */
export function startErrorMessage(err: unknown): string {
  if (!(err instanceof ApiError)) return 'Could not start the workspace.'
  switch (err.status) {
    case 0:
      return err.message
    case 409:
      return 'The workspace is busy starting or stopping. Checking its state again in a moment; try again if it stays off.'
    case 503:
      return `${capitalize(err.message)} Try again in a few minutes.`
    default:
      return `Could not start the workspace: ${err.message}`
  }
}

/** Readable text for a failed "Power off". */
export function stopErrorMessage(err: unknown): string {
  if (!(err instanceof ApiError)) return 'Could not power off the workspace.'
  switch (err.status) {
    case 0:
      return err.message
    case 409:
      return 'The workspace is busy right now (it is still starting). Try powering off again in a moment.'
    default:
      return `Could not power off the workspace: ${err.message}`
  }
}

function capitalize(s: string): string {
  const t = s.trim()
  if (t === '') return t
  const withStop = /[.!?]$/.test(t) ? t : `${t}.`
  return withStop.charAt(0).toUpperCase() + withStop.slice(1)
}

export function elapsedSeconds(sinceMs: number, nowMs: number): number {
  return Math.max(0, Math.floor((nowMs - sinceMs) / 1000))
}
