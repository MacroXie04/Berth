import '@xterm/xterm/css/xterm.css'
import type { Terminal } from '@xterm/xterm'
import { useEffect, useId, useRef, useState } from 'react'
import { LEAVE_HINT, createTerminal } from '../terminal'
import { terminalUrl } from '../ttyd'
import { TtydSession, type SessionState } from '../ttydSession'

const FIT_DEBOUNCE_MS = 60

interface Props {
  username: string
  /**
   * Shift+Escape in the terminal: focus must go somewhere (the page puts it on
   * the pane divider). Without it the terminal just gives up focus.
   */
  onLeave?: () => void
}

/**
 * The terminal: xterm.js on top of our own ttyd client (ttyd.ts / ttydSession.ts).
 * It only exists while the workspace is running; unmounting closes the socket,
 * so a power-off never turns into a reconnect loop.
 */
export function TerminalPane({ username, onLeave }: Props) {
  const hintId = useId()
  const onLeaveRef = useRef(onLeave)
  useEffect(() => {
    onLeaveRef.current = onLeave
  })
  const hostRef = useRef<HTMLDivElement>(null)
  const termRef = useRef<Terminal | null>(null)
  const sessionRef = useRef<TtydSession | null>(null)
  const [state, setState] = useState<SessionState>({ status: 'connecting', attempt: 0 })
  const [title, setTitle] = useState('')

  useEffect(() => {
    const host = hostRef.current
    if (!host) return

    const { term, fit } = createTerminal(() => onLeaveRef.current?.())
    term.open(host)
    // The keyboard hint is part of what a screen reader announces for the input.
    term.textarea?.setAttribute('aria-describedby', hintId)
    termRef.current = term

    const doFit = () => {
      // Hidden or zero-sized hosts have no usable dimensions yet.
      const dims = fit.proposeDimensions()
      if (dims && Number.isFinite(dims.cols) && Number.isFinite(dims.rows) && dims.cols > 0 && dims.rows > 0) {
        fit.fit()
      }
    }
    doFit()

    let disconnected = false
    let focusedOnce = false
    const session = new TtydSession({
      url: terminalUrl(window.location, username),
      getSize: () => ({ cols: term.cols, rows: term.rows }),
      write: (text, done) => term.write(text, done),
      onTitle: setTitle,
      onState: (s) => {
        setState(s)
        if (s.status === 'reconnecting') disconnected = true
        if (s.status === 'open') {
          if (disconnected) {
            // A new connection is a new shell: start from a clean screen.
            term.reset()
            disconnected = false
          }
          if (!focusedOnce) {
            focusedOnce = true
            term.focus()
          }
        }
      },
    })
    sessionRef.current = session

    const subs = [
      term.onData((d) => session.input(d)),
      term.onBinary((d) => session.inputBinary(d)),
      term.onResize(({ cols, rows }) => session.resize(cols, rows)),
    ]

    // Refit when the pane changes size (divider, window), debounced so a drag
    // does not flood the server with resize messages.
    let timer: ReturnType<typeof setTimeout> | null = null
    const ro = new ResizeObserver(() => {
      if (timer !== null) clearTimeout(timer)
      timer = setTimeout(() => {
        timer = null
        doFit()
      }, FIT_DEBOUNCE_MS)
    })
    ro.observe(host)

    session.start()

    return () => {
      if (timer !== null) clearTimeout(timer)
      ro.disconnect()
      subs.forEach((d) => d.dispose())
      session.dispose()
      sessionRef.current = null
      termRef.current = null
      term.dispose()
    }
  }, [username, hintId])

  const connected = state.status === 'open'

  return (
    <section className="pane pane--terminal" aria-label="Terminal" aria-describedby={hintId}>
      <div className="pane__bar">
        <span className="pane__title">Terminal</span>
        {title && (
          <span className="pane__sub" title={title}>
            {title}
          </span>
        )}
        <span id={hintId} className="pane__hint" title={LEAVE_HINT}>
          {LEAVE_HINT}
        </span>
      </div>
      <div className="pane__body" onClick={() => termRef.current?.focus()}>
        <div ref={hostRef} className="term-host" />
        {!connected && (
          <div className="overlay" role="status">
            <p className="overlay__text">
              {state.status === 'connecting' ? 'Connecting to the terminal…' : 'Disconnected, reconnecting…'}
            </p>
            {state.status === 'reconnecting' && state.attempt > 1 && (
              <p className="overlay__sub">Attempt {state.attempt}</p>
            )}
            <button
              type="button"
              className="btn btn--primary"
              onClick={(e) => {
                e.stopPropagation()
                sessionRef.current?.reconnectNow()
              }}
            >
              Reconnect
            </button>
          </div>
        )}
      </div>
    </section>
  )
}
