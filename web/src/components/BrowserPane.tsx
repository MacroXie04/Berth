import { useEffect, useMemo, useState } from 'react'
import { clearNekoSession, nekoUrl } from '../neko'

interface Props {
  username: string
  nekoPassword: string | undefined
}

/**
 * The remote browser: neko's own web UI in an iframe. No `sandbox` attribute:
 * it would give the frame an opaque origin, and the SameSite=Lax berth_ws
 * cookie would no longer be sent with its requests. The page is the user's own
 * workspace; the real boundary is the origin question in CLAUDE.md (D4c).
 *
 * neko keeps its session token in this origin's localStorage; it is cleared
 * before the frame is created (and again when the pane goes away), so each load
 * logs in afresh instead of taking over another tab's session.
 */
export function BrowserPane({ username, nekoPassword }: Props) {
  // The URL carries the neko password, so it is built once per (user, password).
  const src = useMemo(() => nekoUrl(username, nekoPassword), [username, nekoPassword])
  const [ready, setReady] = useState(false)
  const [loaded, setLoaded] = useState(false)

  useEffect(() => {
    clearNekoSession()
    setReady(true)
    return () => clearNekoSession()
  }, [])

  return (
    <section className="pane pane--browser" aria-label="Remote browser">
      <div className="pane__bar">
        <span className="pane__title">Browser</span>
      </div>
      <div className="pane__body">
        {ready && (
          <iframe
            className="browser"
            title="Remote browser"
            src={src}
            allow="clipboard-write; fullscreen"
            referrerPolicy="no-referrer"
            onLoad={() => setLoaded(true)}
          />
        )}
        {!loaded && (
          <div className="overlay overlay--soft" role="status">
            <p className="overlay__text">Loading the browser…</p>
          </div>
        )}
      </div>
    </section>
  )
}
