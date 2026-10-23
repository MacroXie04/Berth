import { useEffect, useRef, useState } from 'react'
import { ApiError, type Api } from '../api'
import { formatElapsed } from '../format'
import { useWorkspace } from '../useWorkspace'
import {
  SLOW_START_SECONDS,
  announcement,
  canPowerOn,
  ctaCopy,
  elapsedSeconds,
  isUnhealthy,
  viewFor,
} from '../workspaceView'
import { BrowserPane } from './BrowserPane'
import { SplitView } from './SplitView'
import { StatusBar } from './StatusBar'
import { TerminalPane } from './TerminalPane'
import { clearNekoSession } from '../neko'

interface Props {
  api: Api
  username: string
  /** Called after a successful logout. */
  onLoggedOut(): void
  /**
   * The server answered for another account than `username` (another tab of
   * this browser logged in as someone else): start over from /api/me.
   */
  onIdentityChanged(): void
}

/** Seconds since `active` last became true; 0 while inactive. */
function useElapsed(active: boolean): number {
  const [since, setSince] = useState<number | null>(null)
  const [now, setNow] = useState(() => Date.now())

  useEffect(() => {
    if (!active) {
      setSince(null)
      return
    }
    const start = Date.now()
    setSince(start)
    setNow(start)
    const id = setInterval(() => setNow(Date.now()), 1000)
    return () => clearInterval(id)
  }, [active])

  return since === null ? 0 : elapsedSeconds(since, now)
}

export function WorkspacePage({ api, username, onLoggedOut, onIdentityChanged }: Props) {
  const ws = useWorkspace(api, { username, onMismatch: onIdentityChanged })
  const { status, busy } = ws
  const [logoutError, setLogoutError] = useState<string | null>(null)

  const view = viewFor(status, busy)
  const cta = status ? ctaCopy(status) : null
  const elapsed = useElapsed(view === 'starting')
  const powerOnRef = useRef<HTMLButtonElement>(null)
  // Shift+Esc in the terminal lands here: from the divider Tab reaches the browser pane and Shift+Tab the terminal again.
  const dividerRef = useRef<HTMLDivElement>(null)

  // After a power-off the call to action takes over; put keyboard focus there.
  const prevView = useRef(view)
  useEffect(() => {
    if (prevView.current === 'stopping' && view === 'cta') powerOnRef.current?.focus()
    prevView.current = view
  }, [view])

  async function logout() {
    setLogoutError(null)
    try {
      await api.logout()
      clearNekoSession()
      onLoggedOut()
    } catch (err) {
      setLogoutError(err instanceof ApiError ? `Could not log out: ${err.message}` : 'Could not log out.')
    }
  }

  return (
    <div className="app">
      <StatusBar
        username={username}
        status={status}
        busy={busy}
        stale={ws.pollError !== null && status !== null}
        logoutError={logoutError}
        onPowerOff={ws.powerOff}
        onLogout={() => void logout()}
      />

      {isUnhealthy(status) && (
        <p className="banner banner--warn banner--bar" role="alert">
          The workspace is running but its health check is failing, so the terminal or the browser may not respond. If
          it does not recover, power off and on again; your files are kept.
        </p>
      )}
      {ws.actionError && (
        <p className="banner banner--error banner--bar" role="alert">
          {ws.actionError}{' '}
          <button type="button" className="link" onClick={ws.dismissError}>
            Dismiss
          </button>
        </p>
      )}

      <main className="stage" id="main">
        {view === 'loading' && (
          <div className="card" aria-busy="true">
            <span className="spinner" aria-hidden="true" />
            <h2 className="card__title">Checking your workspace…</h2>
            {ws.pollError && <p className="card__text">{ws.pollError} Retrying…</p>}
          </div>
        )}

        {view === 'cta' && cta && (
          <div className="card">
            <h2 className="card__title">{cta.title}</h2>
            <p className="card__text">{cta.text}</p>
            <button
              type="button"
              className="btn btn--primary btn--large"
              ref={powerOnRef}
              disabled={!canPowerOn(status, busy)}
              onClick={ws.powerOn}
            >
              Power on
            </button>
          </div>
        )}

        {view === 'starting' && (
          <div className="card" aria-busy="true">
            <span className="spinner" aria-hidden="true" />
            <h2 className="card__title">Starting your workspace…</h2>
            <p className="card__text">
              Waiting for the terminal and the browser to become ready.{' '}
              <span className="elapsed" aria-hidden="true">
                {formatElapsed(elapsed)}
              </span>
            </p>
            {elapsed >= SLOW_START_SECONDS && (
              <p className="card__text card__text--muted">
                This is taking longer than usual. It keeps trying; you can wait or power off.
              </p>
            )}
          </div>
        )}

        {view === 'stopping' && (
          <div className="card" aria-busy="true">
            <span className="spinner" aria-hidden="true" />
            <h2 className="card__title">Powering off…</h2>
            <p className="card__text">Your files are kept.</p>
          </div>
        )}

        {view === 'panes' && status && (
          <SplitView
            label="Resize the terminal and browser panes"
            dividerRef={dividerRef}
            first={<TerminalPane username={username} onLeave={() => dividerRef.current?.focus()} />}
            second={<BrowserPane username={username} nekoPassword={status.neko_password} />}
          />
        )}
      </main>

      {/* Polite announcements of state changes for screen readers. */}
      <div className="sr-only" role="status" aria-live="polite">
        {announcement(view, status)}
      </div>
    </div>
  )
}
