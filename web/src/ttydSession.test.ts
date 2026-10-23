import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'
import { PAUSE, RESUME } from './ttyd'
import { TtydSession, type SessionState, type SocketLike } from './ttydSession'

const enc = new TextEncoder()
const dec = new TextDecoder()

class FakeSocket implements SocketLike {
  static all: FakeSocket[] = []
  binaryType = 'blob'
  readyState = 0
  onopen: ((ev: unknown) => void) | null = null
  onmessage: ((ev: { data: unknown }) => void) | null = null
  onclose: ((ev: unknown) => void) | null = null
  onerror: ((ev: unknown) => void) | null = null
  sent: Uint8Array[] = []
  closed = false
  constructor(
    readonly url: string,
    readonly protocols: string[],
  ) {
    FakeSocket.all.push(this)
  }
  send(data: Uint8Array) {
    this.sent.push(data)
  }
  close() {
    this.closed = true
    this.readyState = 3
  }
  // test helpers
  open() {
    this.readyState = 1
    this.onopen?.({})
  }
  drop() {
    this.readyState = 3
    this.onclose?.({})
  }
  receive(cmd: string, payload: Uint8Array | string = '') {
    const p = typeof payload === 'string' ? enc.encode(payload) : payload
    const out = new Uint8Array(p.length + 1)
    out[0] = cmd.charCodeAt(0)
    out.set(p, 1)
    this.onmessage?.({ data: out.buffer })
  }
}

function setup(over: Partial<ConstructorParameters<typeof TtydSession>[0]> = {}) {
  const states: SessionState[] = []
  const written: string[] = []
  const titles: string[] = []
  const dones: Array<(() => void) | undefined> = []
  let size = { cols: 100, rows: 30 }
  const session = new TtydSession({
    url: 'ws://h/ws/alice/terminal/ws',
    getSize: () => size,
    write: (t, done) => {
      written.push(t)
      dones.push(done)
    },
    onState: (s) => states.push(s),
    onTitle: (t) => titles.push(t),
    createSocket: (url, protocols) => new FakeSocket(url, protocols),
    ...over,
  })
  return {
    session,
    states,
    written,
    titles,
    dones,
    setSize: (s: { cols: number; rows: number }) => (size = s),
    last: () => FakeSocket.all[FakeSocket.all.length - 1]!,
  }
}

beforeEach(() => {
  vi.useFakeTimers()
  FakeSocket.all = []
})
afterEach(() => {
  vi.useRealTimers()
})

describe('TtydSession', () => {
  it('connects with the tty subprotocol and binary frames, and sends the handshake with the current size on open', () => {
    const t = setup()
    t.session.start()
    const s = t.last()
    expect(s.url).toBe('ws://h/ws/alice/terminal/ws')
    expect(s.protocols).toEqual(['tty'])
    expect(s.binaryType).toBe('arraybuffer')
    expect(t.session.current.status).toBe('connecting')
    expect(s.sent).toHaveLength(0)

    t.setSize({ cols: 132, rows: 43 })
    s.open()
    expect(t.session.current.status).toBe('open')
    expect(s.sent).toHaveLength(1)
    expect(JSON.parse(dec.decode(s.sent[0]))).toEqual({ AuthToken: '', columns: 132, rows: 43 })
  })

  it('frames input with the INPUT command and drops it while not connected', () => {
    const t = setup()
    t.session.start()
    const s = t.last()
    t.session.input('early') // not open yet
    s.open()
    t.session.input('echo hi\r')
    t.session.inputBinary('\xe0\x21')
    expect(s.sent).toHaveLength(3) // handshake + 2
    expect(dec.decode(s.sent[1])).toBe('0echo hi\r')
    expect(Array.from(s.sent[2]!)).toEqual([0x30, 0xe0, 0x21])
  })

  it('sends a resize only when the size changed since the handshake or the last resize', () => {
    const t = setup()
    t.session.start()
    const s = t.last()
    s.open() // handshake carries 100x30
    t.session.resize(100, 30)
    expect(s.sent).toHaveLength(1)
    t.session.resize(120, 40)
    t.session.resize(120, 40)
    expect(s.sent).toHaveLength(2)
    expect(dec.decode(s.sent[1])).toBe('1{"columns":120,"rows":40}')
  })

  it('writes output to the terminal, decoding multi-byte characters split between frames', () => {
    const t = setup()
    t.session.start()
    const s = t.last()
    s.open()
    const bytes = enc.encode('hello-€')
    s.receive('0', bytes.subarray(0, bytes.length - 1))
    s.receive('0', bytes.subarray(bytes.length - 1))
    expect(t.written.join('')).toBe('hello-€')
  })

  it('forwards the (sanitised) window title and ignores preferences', () => {
    const t = setup()
    t.session.start()
    const s = t.last()
    s.open()
    s.receive('1', 'alice@box: ~')
    s.receive('2', '{"fontSize":99}')
    expect(t.titles).toEqual(['alice@box: ~'])
  })

  it('pauses the server when the terminal falls behind and resumes when it catches up', () => {
    const t = setup()
    t.session.start()
    const s = t.last()
    s.open()
    const big = new Uint8Array(100_001).fill(0x61)
    for (let i = 0; i < 11; i++) s.receive('0', big)
    expect(s.sent.some((m) => m.length === 1 && m[0] === PAUSE[0])).toBe(true)
    for (const d of t.dones) d?.()
    expect(s.sent.some((m) => m.length === 1 && m[0] === RESUME[0])).toBe(true)
  })

  it('does not throw when the socket is already closing by the time it opens, and then retries', () => {
    const t = setup()
    t.session.start()
    const s = t.last()
    s.send = () => {
      throw new Error('InvalidStateError')
    }
    s.readyState = 2 // CLOSING
    expect(() => s.onopen?.({})).not.toThrow()
    expect(t.session.current.status).not.toBe('open')
    s.drop()
    expect(t.session.current.status).toBe('reconnecting')
    vi.advanceTimersByTime(500)
    expect(FakeSocket.all).toHaveLength(2)
  })

  it('a send that throws is swallowed', () => {
    const t = setup()
    t.session.start()
    const s = t.last()
    s.open()
    s.send = () => {
      throw new Error('boom')
    }
    expect(() => t.session.input('x')).not.toThrow()
  })

  describe('reconnecting', () => {
    it('retries with exponential backoff capped at 10 s', () => {
      const t = setup()
      t.session.start()
      expect(FakeSocket.all).toHaveLength(1)
      const delays = [500, 1000, 2000, 4000, 8000, 10000, 10000]
      for (const [i, d] of delays.entries()) {
        t.last().drop()
        expect(t.session.current).toEqual({ status: 'reconnecting', attempt: i + 1 })
        vi.advanceTimersByTime(d - 1)
        expect(FakeSocket.all).toHaveLength(i + 1) // not yet
        vi.advanceTimersByTime(1)
        expect(FakeSocket.all).toHaveLength(i + 2)
      }
    })

    it('a connection that stays up resets the backoff; a flapping one does not', () => {
      const t = setup()
      t.session.start()
      t.last().drop() // attempt 1, delay 500
      vi.advanceTimersByTime(500)
      t.last().open()
      t.last().drop() // opened and closed at once: not stable
      expect(t.session.current.attempt).toBe(2)
      vi.advanceTimersByTime(1000)
      t.last().open()
      vi.advanceTimersByTime(6000) // stable
      t.last().drop()
      expect(t.session.current).toEqual({ status: 'reconnecting', attempt: 1 })
      vi.advanceTimersByTime(500)
      expect(FakeSocket.all).toHaveLength(4)
    })

    it('"Reconnect" connects at once and starts the backoff over', () => {
      const t = setup()
      t.session.start()
      t.last().drop()
      vi.advanceTimersByTime(500)
      t.last().drop()
      const before = FakeSocket.all.length
      t.session.reconnectNow()
      expect(FakeSocket.all).toHaveLength(before + 1)
      expect(t.session.current).toEqual({ status: 'reconnecting', attempt: 0 })
      // the pending timer of the dropped socket was cancelled: no second connect
      vi.advanceTimersByTime(20_000)
      expect(FakeSocket.all).toHaveLength(before + 1)
    })

    it('ignores events from a socket that was replaced', () => {
      const t = setup()
      t.session.start()
      const old = t.last()
      t.session.reconnectNow()
      const fresh = t.last()
      old.drop() // late event from the old socket
      expect(t.session.current.status).toBe('reconnecting')
      fresh.open()
      expect(t.session.current.status).toBe('open')
    })

    it('survives a socket constructor that throws', () => {
      let calls = 0
      const t = setup({
        createSocket: (url, protocols) => {
          calls++
          if (calls === 1) throw new Error('SecurityError')
          return new FakeSocket(url, protocols)
        },
      })
      t.session.start()
      expect(t.session.current.status).toBe('reconnecting')
      vi.advanceTimersByTime(500)
      expect(FakeSocket.all).toHaveLength(1)
    })
  })

  describe('dispose (unmount or power off)', () => {
    it('closes the socket and never reconnects', () => {
      const t = setup()
      t.session.start()
      const s = t.last()
      s.open()
      t.session.dispose()
      expect(s.closed).toBe(true)
      expect(t.session.current.status).toBe('closed')
      s.drop() // even a late close event from the dead socket does nothing
      vi.advanceTimersByTime(60_000)
      expect(FakeSocket.all).toHaveLength(1)
    })

    it('cancels a pending retry', () => {
      const t = setup()
      t.session.start()
      t.last().drop()
      t.session.dispose()
      vi.advanceTimersByTime(60_000)
      expect(FakeSocket.all).toHaveLength(1)
    })

    it('ignores input, start and reconnect afterwards', () => {
      const t = setup()
      t.session.start()
      const s = t.last()
      s.open()
      t.session.dispose()
      t.session.input('x')
      t.session.start()
      t.session.reconnectNow()
      expect(FakeSocket.all).toHaveLength(1)
      expect(s.sent).toHaveLength(1) // only the handshake
    })
  })
})
