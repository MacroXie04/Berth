/**
 * ttyd (1.7) WebSocket protocol, as a pure module: no DOM, no React, no
 * sockets. See TtydSession (ttydSession.ts) for the connection handling.
 *
 * Connect to ws(s)://<host>/ws/<user>/terminal/ws with subprotocol "tty".
 * Client -> server, the first message:  {"AuthToken":"","columns":N,"rows":M}
 * Client -> server afterwards: one command byte, then the payload:
 *   '0' INPUT (bytes)   '1' RESIZE_TERMINAL (JSON {"columns","rows"})
 *   '2' PAUSE           '3' RESUME
 * Server -> client: one command byte, then the payload:
 *   '0' OUTPUT (bytes)  '1' SET_WINDOW_TITLE (text)  '2' SET_PREFERENCES (JSON)
 *
 * Like ttyd's own client, everything is sent as binary frames; the server reads
 * the command from the first byte whatever the frame type.
 */

export const SUBPROTOCOL = 'tty'

export const ClientCommand = {
  INPUT: '0',
  RESIZE_TERMINAL: '1',
  PAUSE: '2',
  RESUME: '3',
} as const

export const ServerCommand = {
  OUTPUT: '0',
  SET_WINDOW_TITLE: '1',
  SET_PREFERENCES: '2',
} as const

const encoder = new TextEncoder()

/** The workspace's usernames are a restricted charset (CLAUDE.md); this is the same rule. */
const USERNAME_RE = /^[a-z][a-z0-9]*(-[a-z0-9]+)*$/

/** ws(s) URL of a user's terminal socket (the proxy strips /ws/<user>/terminal). */
export function terminalUrl(loc: { protocol: string; host: string }, username: string): string {
  if (!USERNAME_RE.test(username)) throw new Error('invalid username')
  const scheme = loc.protocol === 'https:' ? 'wss:' : 'ws:'
  return `${scheme}//${loc.host}/ws/${username}/terminal/ws`
}

function withCommand(cmd: string, payload: Uint8Array): Uint8Array {
  const out = new Uint8Array(payload.length + 1)
  out[0] = cmd.charCodeAt(0)
  out.set(payload, 1)
  return out
}

/** First message after the socket opens. ttyd identifies it by the leading "{". */
export function encodeHandshake(columns: number, rows: number, authToken = ''): Uint8Array {
  return encoder.encode(JSON.stringify({ AuthToken: authToken, columns, rows }))
}

/** Keyboard/paste input (a string) or raw bytes (xterm's binary mouse reports). */
export function encodeInput(data: string | Uint8Array): Uint8Array {
  return withCommand(ClientCommand.INPUT, typeof data === 'string' ? encoder.encode(data) : data)
}

export function encodeResize(columns: number, rows: number): Uint8Array {
  return withCommand(ClientCommand.RESIZE_TERMINAL, encoder.encode(JSON.stringify({ columns, rows })))
}

export const PAUSE: Uint8Array = encoder.encode(ClientCommand.PAUSE)
export const RESUME: Uint8Array = encoder.encode(ClientCommand.RESUME)

/** xterm's onBinary hands over a string whose char codes are bytes (0-255). */
export function binaryStringToBytes(s: string): Uint8Array {
  const out = new Uint8Array(s.length)
  for (let i = 0; i < s.length; i++) out[i] = s.charCodeAt(i) & 0xff
  return out
}

/**
 * Decodes terminal output across frame boundaries: a multi-byte UTF-8
 * character may be split between two OUTPUT frames, and the incomplete tail
 * must wait for the next chunk instead of turning into U+FFFD.
 */
export class Utf8Stream {
  private decoder = new TextDecoder('utf-8')
  push(bytes: Uint8Array): string {
    return this.decoder.decode(bytes, { stream: true })
  }
  /** Drop a pending partial character (new connection, new shell). */
  reset(): void {
    this.decoder = new TextDecoder('utf-8')
  }
}

export type ServerMessage =
  | { kind: 'output'; text: string; bytes: number }
  | { kind: 'title'; title: string }
  | { kind: 'preferences'; preferences: Record<string, unknown> }
  | { kind: 'ignored' }

const MAX_TITLE = 120

/**
 * The window title is chosen by whatever runs in the workspace. It is only ever
 * rendered as text, but keep it to one short printable line.
 */
export function sanitizeTitle(raw: string): string {
  // eslint-disable-next-line no-control-regex
  const clean = raw.replace(/[\u0000-\u001f\u007f-\u009f‎‏‪-‮⁦-⁩]/g, ' ').trim()
  return clean.length > MAX_TITLE ? `${clean.slice(0, MAX_TITLE)}…` : clean
}

/**
 * Parses one frame from the server. `stream` carries the UTF-8 state of the
 * OUTPUT channel. Text frames are accepted too (their first character is the
 * command). Unknown commands and malformed preferences are ignored: the peer
 * is code running in a user's workspace, not a trusted party.
 */
export function decodeServerMessage(data: ArrayBuffer | Uint8Array | string, stream: Utf8Stream): ServerMessage {
  const bytes =
    typeof data === 'string' ? encoder.encode(data) : data instanceof Uint8Array ? data : new Uint8Array(data)
  if (bytes.length === 0) return { kind: 'ignored' }
  const cmd = String.fromCharCode(bytes[0] ?? 0)
  const payload = bytes.subarray(1)
  switch (cmd) {
    case ServerCommand.OUTPUT:
      return { kind: 'output', text: stream.push(payload), bytes: payload.length }
    case ServerCommand.SET_WINDOW_TITLE:
      return { kind: 'title', title: sanitizeTitle(new TextDecoder().decode(payload)) }
    case ServerCommand.SET_PREFERENCES:
      try {
        const parsed: unknown = JSON.parse(new TextDecoder().decode(payload))
        if (parsed && typeof parsed === 'object' && !Array.isArray(parsed)) {
          return { kind: 'preferences', preferences: parsed as Record<string, unknown> }
        }
      } catch {
        // fall through
      }
      return { kind: 'ignored' }
    default:
      return { kind: 'ignored' }
  }
}

/**
 * ttyd's flow control, so `cat bigfile` cannot make the page fall behind
 * without bound: after every `limit` bytes the write is tracked with a
 * callback; more than `highWater` outstanding writes -> PAUSE the server, and
 * once xterm has caught up to below `lowWater` -> RESUME.
 */
export class FlowControl {
  private written = 0
  private pending = 0
  private paused = false

  constructor(
    private readonly limit = 100_000,
    private readonly highWater = 10,
    private readonly lowWater = 4,
  ) {}

  /**
   * Account for `size` bytes about to be written. Returns whether the caller
   * must pass a completion callback to the terminal write (and call
   * `done()` from it), plus a command to send now, if any.
   */
  onWrite(size: number): { track: boolean; send: Uint8Array | null } {
    this.written += size
    if (this.written <= this.limit) return { track: false, send: null }
    this.written = 0
    this.pending++
    if (this.pending > this.highWater && !this.paused) {
      this.paused = true
      return { track: true, send: PAUSE }
    }
    return { track: true, send: null }
  }

  /** The terminal finished a tracked write. */
  done(): Uint8Array | null {
    this.pending = Math.max(this.pending - 1, 0)
    if (this.paused && this.pending < this.lowWater) {
      this.paused = false
      return RESUME
    }
    return null
  }

  /** A new connection starts with a running server. */
  reset(): void {
    this.written = 0
    this.pending = 0
    this.paused = false
  }
}

/** Reconnect delay: 0.5 s, 1 s, 2 s, ... capped at 10 s. `attempt` counts from 0. */
export function backoffDelay(attempt: number, baseMs = 500, capMs = 10_000): number {
  const n = Number.isFinite(attempt) ? Math.max(0, Math.floor(attempt)) : 0
  return Math.min(capMs, baseMs * 2 ** Math.min(n, 16))
}
