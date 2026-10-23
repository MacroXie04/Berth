import { useEffect, useRef, useState } from 'react'
import { formatBytes, formatCpu } from '../format'
import type { WorkspaceStatus } from '../types'
import { badgeFor, canPowerOff, healthLabel, type Busy } from '../workspaceView'

interface Props {
  username: string
  status: WorkspaceStatus | null
  busy: Busy
  /** The last status poll failed. */
  stale: boolean
  logoutError: string | null
  onPowerOff(): void
  onLogout(): void
}

export function StatusBar({ username, status, busy, stale, logoutError, onPowerOff, onLogout }: Props) {
  const [confirming, setConfirming] = useState(false)
  const powerOffRef = useRef<HTMLButtonElement>(null)
  const cancelRef = useRef<HTMLButtonElement>(null)
  const wasConfirming = useRef(false)

  const badge = badgeFor(status, busy)
  const health = status?.state === 'running' ? healthLabel(status.health) : null
  const usage = status?.state === 'running' ? status.usage : null
  const allowPowerOff = canPowerOff(status, busy)

  // The confirmation vanishes if powering off stops being possible (the
  // workspace stopped by itself, or the request is under way).
  useEffect(() => {
    if (!allowPowerOff) setConfirming(false)
  }, [allowPowerOff])

  // Keyboard users: focus the safe choice when the question appears, and put
  // focus back on the button when it is dismissed.
  useEffect(() => {
    if (confirming) cancelRef.current?.focus()
    else if (wasConfirming.current) powerOffRef.current?.focus()
    wasConfirming.current = confirming
  }, [confirming])

  const memPct = usage && usage.mem_limit_bytes > 0 ? Math.min(100, (usage.mem_bytes / usage.mem_limit_bytes) * 100) : 0

  return (
    <header className="statusbar">
      <div className="statusbar__id">
        <h1 className="statusbar__brand">Berth</h1>
        <span className="statusbar__user" title="Signed in as">
          {username}
        </span>
      </div>

      <div className="statusbar__state">
        <span className={`badge badge--${badge.tone}`}>
          <span className="badge__dot" aria-hidden="true" />
          {badge.label}
        </span>
        {health && <span className={`health health--${status?.health}`}>{health}</span>}
        {stale && (
          <span className="health health--unhealthy" title="The last status update failed">
            Not up to date
          </span>
        )}
      </div>

      {usage && (
        <dl className="usage" aria-label="Resource usage">
          <div
            className="usage__item"
            title={`Memory: ${formatBytes(usage.mem_bytes)} of ${formatBytes(usage.mem_limit_bytes)}`}
          >
            <dt>Memory</dt>
            <dd>
              {formatBytes(usage.mem_bytes)} <span className="usage__of">/ {formatBytes(usage.mem_limit_bytes)}</span>
              <span className="meter" aria-hidden="true">
                <span className="meter__bar" style={{ width: `${memPct}%` }} />
              </span>
            </dd>
          </div>
          <div
            className="usage__item"
            title={`CPU: ${formatCpu(usage.cpu_percent)} of one core${status ? ` (limit ${status.cpu_limit} cores)` : ''}`}
          >
            <dt>CPU</dt>
            <dd>{formatCpu(usage.cpu_percent)}</dd>
          </div>
        </dl>
      )}

      <div className="statusbar__actions">
        {confirming ? (
          <div className="confirm" role="group" aria-label="Confirm power off">
            <span className="confirm__text">Power off? Running programs stop; your files are kept.</span>
            <button
              type="button"
              className="btn btn--danger"
              onClick={() => {
                setConfirming(false)
                onPowerOff()
              }}
            >
              Yes, power off
            </button>
            <button type="button" className="btn" ref={cancelRef} onClick={() => setConfirming(false)}>
              Cancel
            </button>
          </div>
        ) : (
          <button
            type="button"
            className="btn"
            ref={powerOffRef}
            disabled={!allowPowerOff}
            onClick={() => setConfirming(true)}
          >
            Power off
          </button>
        )}
        <button type="button" className="btn btn--quiet" onClick={onLogout}>
          Log out
        </button>
      </div>
      {logoutError && (
        <p className="statusbar__error banner banner--error" role="alert">
          {logoutError}
        </p>
      )}
    </header>
  )
}
