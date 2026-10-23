import { useCallback, useEffect, useRef, useState } from 'react'
import { ApiError, type Api } from './api'
import type { WorkspaceStatus } from './types'
import {
  HEARTBEAT_MS,
  heartbeatActive,
  pollDelay,
  startErrorMessage,
  stopErrorMessage,
  type Busy,
} from './workspaceView'

export interface WorkspaceController {
  /** Last status the server reported; null until the first answer. */
  status: WorkspaceStatus | null
  busy: Busy
  /** The last failed Power on / Power off, as readable text. */
  actionError: string | null
  /** The last status poll failed (network or server error), as readable text. */
  pollError: string | null
  /** Tab is visible (polling and heartbeats pause while it is not). */
  visible: boolean
  powerOn(): void
  powerOff(): void
  dismissError(): void
}

function isVisible(): boolean {
  return typeof document === 'undefined' || document.visibilityState !== 'hidden'
}

/** Who the page was opened for, and what to do when the server answers for somebody else. */
export interface Identity {
  username: string
  /** Another tab logged in as another user: the page must start over (App: back to the boot flow). */
  onMismatch(): void
}

/**
 * Keeps the workspace status fresh and the user's activity known to the server.
 *
 * - Fetches the status when it mounts and whenever the tab becomes visible,
 *   then polls: every 2 s while starting, every 10 s while running, not at all
 *   while offline/stopped. One request at a time (a timer chain, not an
 *   interval): a status call on a running workspace takes about a second.
 *   While a request has failed it asks every 5 s, whatever the state.
 * - Sends POST /api/workspace/heartbeat every 30 s while the workspace is
 *   starting or running AND the tab is visible, and once when that becomes true.
 * - Stale answers are dropped: one that was overtaken by a Power on/off
 *   (`epoch`), and one for which a newer status request was issued meanwhile
 *   (`refreshSeq`: a tab that becomes visible asks at once, even if the timer's
 *   request is still on its way, and the two may answer in either order).
 * - Every answer carries the account it was for. The cookie is shared by all
 *   tabs of the browser, so a tab that was opened for alice keeps talking to
 *   whoever logged in last. When `identity` is given and the answer is for
 *   someone else, nothing is applied, the status is cleared (the panes and the
 *   heartbeat stop) and `identity.onMismatch` is called. An answer without a
 *   username (an older backend) is taken as it is.
 * - Everything stops on unmount.
 */
export function useWorkspace(api: Api, identity?: Identity): WorkspaceController {
  const [status, setStatus] = useState<WorkspaceStatus | null>(null)
  const [busy, setBusy] = useState<Busy>(null)
  const [actionError, setActionError] = useState<string | null>(null)
  const [pollError, setPollError] = useState<string | null>(null)
  const [visible, setVisible] = useState(isVisible)
  const [tick, setTick] = useState(0)

  const alive = useRef(true)
  const epoch = useRef(0) // bumped by every action: older poll answers are stale
  const refreshSeq = useRef(0) // numbers the status requests: only the newest one's answer counts
  const busyRef = useRef<Busy>(null)
  const authLost = useRef(false)
  const identityLost = useRef(false)
  const identityRef = useRef(identity)
  useEffect(() => {
    identityRef.current = identity
  })

  useEffect(() => {
    alive.current = true
    return () => {
      alive.current = false
    }
  }, [])

  // True when `s` is the answer for another account; the page has then been told and the status dropped.
  const foreign = useCallback((s: WorkspaceStatus): boolean => {
    const id = identityRef.current
    if (!id || s.username === undefined || s.username === id.username) return false
    identityLost.current = true
    setStatus(null)
    setPollError(null)
    id.onMismatch()
    return true
  }, [])

  const refresh = useCallback(async () => {
    if (authLost.current || identityLost.current) return
    const mine = epoch.current
    const seq = ++refreshSeq.current
    const current = () => alive.current && mine === epoch.current && seq === refreshSeq.current
    try {
      const s = await api.workspace()
      if (!current()) return
      if (foreign(s)) return
      setStatus(s)
      setPollError(null)
    } catch (err) {
      if (!current()) return
      if (err instanceof ApiError && err.status === 401) {
        authLost.current = true // the app is about to show the login page
        return
      }
      setPollError(err instanceof ApiError ? err.message : 'Could not read the workspace status.')
    } finally {
      if (current()) setTick((t) => t + 1)
    }
  }, [api, foreign])

  // Tab visibility.
  useEffect(() => {
    const onChange = () => setVisible(isVisible())
    document.addEventListener('visibilitychange', onChange)
    return () => document.removeEventListener('visibilitychange', onChange)
  }, [])

  // Fetch now: on mount, and again whenever the tab comes back.
  useEffect(() => {
    if (visible) void refresh()
  }, [visible, refresh])

  // Next poll. `tick` changes after every finished request, which re-arms this.
  const state = status?.state ?? null
  const failing = pollError !== null
  useEffect(() => {
    if (!visible || busy !== null) return
    const delay = pollDelay(state, failing)
    if (delay === null) return
    const t = setTimeout(() => void refresh(), delay)
    return () => clearTimeout(t)
  }, [tick, visible, busy, state, failing, refresh])

  // Heartbeat.
  const beat = heartbeatActive(state, visible, busy)
  useEffect(() => {
    if (!beat) return
    const send = () => {
      api.heartbeat().catch(() => {
        // best effort; a 401 is handled centrally by the API client
      })
    }
    send()
    const id = setInterval(send, HEARTBEAT_MS)
    return () => clearInterval(id)
  }, [beat, api])

  const run = useCallback(
    async (kind: Exclude<Busy, null>, call: () => Promise<WorkspaceStatus>, describe: (e: unknown) => string) => {
      if (busyRef.current !== null || identityLost.current) return
      busyRef.current = kind
      epoch.current++
      setBusy(kind)
      setActionError(null)
      let failed = false
      try {
        const s = await call()
        if (alive.current && !foreign(s)) {
          setStatus(s)
          setPollError(null) // a fresh answer: whatever failed before is history
        }
      } catch (err) {
        failed = true
        if (alive.current && !(err instanceof ApiError && err.status === 401)) setActionError(describe(err))
      } finally {
        busyRef.current = null
        epoch.current++
        if (alive.current) {
          setBusy(null)
          setTick((t) => t + 1)
        }
      }
      // After a failure the truth is on the server (busy, or still running).
      if (failed && alive.current) void refresh()
    },
    [refresh, foreign],
  )

  const powerOn = useCallback(() => void run('starting', api.start, startErrorMessage), [run, api])
  const powerOff = useCallback(() => void run('stopping', api.stop, stopErrorMessage), [run, api])
  const dismissError = useCallback(() => setActionError(null), [])

  return { status, busy, actionError, pollError, visible, powerOn, powerOff, dismissError }
}
