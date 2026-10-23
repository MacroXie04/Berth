import {
  backoffDelay,
  binaryStringToBytes,
  decodeServerMessage,
  encodeHandshake,
  encodeInput,
  encodeResize,
  FlowControl,
  SUBPROTOCOL,
  Utf8Stream,
} from './ttyd'

/** The subset of WebSocket the session uses (so tests can fake it). */
export interface SocketLike {
  binaryType: string
  readyState: number
  onopen: ((ev: unknown) => void) | null
  onmessage: ((ev: { data: unknown }) => void) | null
  onclose: ((ev: unknown) => void) | null
  onerror: ((ev: unknown) => void) | null
  send(data: Uint8Array): void
  close(): void
}

export type SessionStatus =
  /** The very first attempt is in flight. */
  | 'connecting'
  | 'open'
  /** The connection dropped (or never came up); a retry is scheduled or in flight. */
  | 'reconnecting'
  /** Disposed: the session will not reconnect. */
  | 'closed'

export interface SessionState {
  status: SessionStatus
  /** Failed attempts since the last stable connection. */
  attempt: number
}

export interface TtydSessionOptions {
  url: string
  /** Current terminal size, asked at the moment a socket opens. */
  getSize(): { cols: number; rows: number }
  /** Terminal output. `done` is set when the terminal must report the write finished (flow control). */
  write(text: string, done?: () => void): void
  onState(state: SessionState): void
  onTitle?(title: string): void
  createSocket?(url: string, protocols: string[]): SocketLike
  /** A connection that lasted at least this long counts as stable (resets the backoff). */
  stableAfterMs?: number
}

const OPEN = 1

/**
 * One terminal connection to a workspace's ttyd, with automatic reconnection
 * (exponential backoff, capped at 10 s). Every reconnect is a NEW shell: ttyd
 * starts one process per WebSocket and ends it when the socket closes.
 */
export class TtydSession {
  private socket: SocketLike | null = null
  private timer: ReturnType<typeof setTimeout> | null = null
  private state: SessionState = { status: 'connecting', attempt: 0 }
  private disposed = false
  private openedAt = 0
  private lastSize = { cols: 0, rows: 0 }
  private stream = new Utf8Stream()
  private flow = new FlowControl()
  private readonly stableAfterMs: number

  constructor(private readonly opts: TtydSessionOptions) {
    this.stableAfterMs = opts.stableAfterMs ?? 5000
  }

  get current(): SessionState {
    return this.state
  }

  start(): void {
    if (this.disposed || this.socket || this.timer) return
    this.connect()
  }

  /** Send keyboard or paste input. Dropped while not connected. */
  input(data: string): void {
    this.send(encodeInput(data))
  }

  /** Send raw bytes from xterm's onBinary (a string of byte-valued chars). */
  inputBinary(data: string): void {
    this.send(encodeInput(binaryStringToBytes(data)))
  }

  /** Tell the server the terminal size changed. Repeats of the same size are not sent. */
  resize(cols: number, rows: number): void {
    if (cols === this.lastSize.cols && rows === this.lastSize.rows) return
    if (this.send(encodeResize(cols, rows))) this.lastSize = { cols, rows }
  }

  /** The user pressed "Reconnect": try right now and restart the backoff. */
  reconnectNow(): void {
    if (this.disposed) return
    this.clearTimer()
    this.detach()
    this.setState({ status: 'reconnecting', attempt: 0 })
    this.connect()
  }

  dispose(): void {
    if (this.disposed) return
    this.disposed = true
    this.clearTimer()
    this.detach()
    this.setState({ status: 'closed', attempt: this.state.attempt })
  }

  private send(data: Uint8Array): boolean {
    const s = this.socket
    if (!s || s.readyState !== OPEN) return false
    try {
      s.send(data)
      return true
    } catch {
      // closing under our feet; the close event follows and starts the retry
      return false
    }
  }

  private connect(): void {
    if (this.disposed) return
    const create =
      this.opts.createSocket ?? ((url, protocols) => new WebSocket(url, protocols) as unknown as SocketLike)
    let sock: SocketLike
    try {
      sock = create(this.opts.url, [SUBPROTOCOL])
    } catch {
      this.scheduleRetry()
      return
    }
    sock.binaryType = 'arraybuffer'
    this.socket = sock

    sock.onopen = () => {
      if (sock !== this.socket) return
      this.openedAt = Date.now()
      this.stream.reset()
      this.flow.reset()
      const { cols, rows } = this.opts.getSize()
      this.lastSize = { cols, rows }
      // A socket that is already closing (the close event follows) gets no handshake.
      if (!this.send(encodeHandshake(cols, rows))) return
      this.setState({ status: 'open', attempt: this.state.attempt })
    }
    sock.onmessage = (ev) => {
      if (sock !== this.socket) return
      this.onMessage(ev.data)
    }
    sock.onclose = () => {
      if (sock !== this.socket) return
      this.socket = null
      if (this.disposed) return
      if (this.openedAt > 0 && Date.now() - this.openedAt >= this.stableAfterMs) {
        this.state = { ...this.state, attempt: 0 }
      }
      this.openedAt = 0
      this.scheduleRetry()
    }
    sock.onerror = () => {
      // Always followed by onclose, which does the work.
    }
  }

  private onMessage(data: unknown): void {
    if (!(data instanceof ArrayBuffer) && !(data instanceof Uint8Array) && typeof data !== 'string') return
    const msg = decodeServerMessage(data, this.stream)
    switch (msg.kind) {
      case 'output': {
        if (msg.text === '') return
        const fc = this.flow.onWrite(msg.bytes)
        if (fc.send) this.send(fc.send)
        this.opts.write(
          msg.text,
          fc.track
            ? () => {
                const cmd = this.flow.done()
                if (cmd) this.send(cmd)
              }
            : undefined,
        )
        return
      }
      case 'title':
        this.opts.onTitle?.(msg.title)
        return
      default:
        // Preferences are ignored on purpose: what the workspace says about fonts
        // and themes does not get to restyle Berth's page.
        return
    }
  }

  private scheduleRetry(): void {
    if (this.disposed) return
    const attempt = this.state.attempt
    this.setState({ status: 'reconnecting', attempt: attempt + 1 })
    this.clearTimer()
    this.timer = setTimeout(() => {
      this.timer = null
      this.connect()
    }, backoffDelay(attempt))
  }

  private clearTimer(): void {
    if (this.timer !== null) {
      clearTimeout(this.timer)
      this.timer = null
    }
  }

  /** Drop the current socket without triggering the reconnect logic. */
  private detach(): void {
    const s = this.socket
    this.socket = null
    this.openedAt = 0
    if (!s) return
    s.onopen = s.onmessage = s.onclose = s.onerror = null
    try {
      s.close()
    } catch {
      // already closed
    }
  }

  private setState(next: SessionState): void {
    this.state = next
    this.opts.onState(next)
  }
}
