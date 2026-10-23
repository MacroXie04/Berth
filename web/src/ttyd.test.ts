import { describe, expect, it } from 'vitest'
import {
  FlowControl,
  PAUSE,
  RESUME,
  Utf8Stream,
  backoffDelay,
  binaryStringToBytes,
  decodeServerMessage,
  encodeHandshake,
  encodeInput,
  encodeResize,
  sanitizeTitle,
  terminalUrl,
} from './ttyd'

const dec = new TextDecoder()
const enc = new TextEncoder()
const frame = (cmd: string, payload: Uint8Array | string = ''): Uint8Array => {
  const p = typeof payload === 'string' ? enc.encode(payload) : payload
  const out = new Uint8Array(p.length + 1)
  out[0] = cmd.charCodeAt(0)
  out.set(p, 1)
  return out
}

describe('terminalUrl', () => {
  it('uses ws for http and wss for https, and the /ws suffix the proxy expects', () => {
    expect(terminalUrl({ protocol: 'http:', host: 'localhost:5173' }, 'alice')).toBe(
      'ws://localhost:5173/ws/alice/terminal/ws',
    )
    expect(terminalUrl({ protocol: 'https:', host: 'berth.example.com' }, 'bob-2')).toBe(
      'wss://berth.example.com/ws/bob-2/terminal/ws',
    )
  })
  it('refuses a username that could change the path', () => {
    for (const bad of ['', 'a/b', '../x', 'Alice', 'a b', 'alice?x=1', 'alice%2f', 'a--b']) {
      expect(() => terminalUrl({ protocol: 'http:', host: 'h' }, bad)).toThrow()
    }
  })
})

describe('client -> server framing', () => {
  it('handshake is the JSON ttyd expects, starting with "{"', () => {
    const bytes = encodeHandshake(120, 40)
    expect(String.fromCharCode(bytes[0]!)).toBe('{')
    expect(JSON.parse(dec.decode(bytes))).toEqual({ AuthToken: '', columns: 120, rows: 40 })
  })

  it('input is the command byte "0" followed by the UTF-8 bytes', () => {
    expect(Array.from(encodeInput('ls\r'))).toEqual([0x30, 0x6c, 0x73, 0x0d])
    // multi-byte characters are encoded as UTF-8, not as UTF-16 code units
    expect(Array.from(encodeInput('€'))).toEqual([0x30, 0xe2, 0x82, 0xac])
  })

  it('raw input bytes are kept as they are', () => {
    expect(Array.from(encodeInput(new Uint8Array([0x1b, 0x5b, 0xff])))).toEqual([0x30, 0x1b, 0x5b, 0xff])
  })

  it('binary strings from xterm become bytes (char code = byte value)', () => {
    expect(Array.from(binaryStringToBytes('\x1b[M\xe0!"'))).toEqual([0x1b, 0x5b, 0x4d, 0xe0, 0x21, 0x22])
  })

  it('resize is "1" followed by JSON columns/rows', () => {
    const bytes = encodeResize(80, 24)
    expect(String.fromCharCode(bytes[0]!)).toBe('1')
    expect(JSON.parse(dec.decode(bytes.subarray(1)))).toEqual({ columns: 80, rows: 24 })
  })

  it('pause and resume are the single bytes "2" and "3"', () => {
    expect(Array.from(PAUSE)).toEqual([0x32])
    expect(Array.from(RESUME)).toEqual([0x33])
  })
})

describe('server -> client decoding', () => {
  it('OUTPUT frames carry text after the command byte', () => {
    const s = new Utf8Stream()
    expect(decodeServerMessage(frame('0', 'hello-42\r\n').buffer as ArrayBuffer, s)).toEqual({
      kind: 'output',
      text: 'hello-42\r\n',
      bytes: 10,
    })
  })

  it('accepts a Uint8Array view and a text frame as well as an ArrayBuffer', () => {
    const s = new Utf8Stream()
    expect(decodeServerMessage(frame('0', 'a'), s)).toMatchObject({ kind: 'output', text: 'a' })
    expect(decodeServerMessage('0b', s)).toMatchObject({ kind: 'output', text: 'b' })
  })

  it('a multi-byte character split across two OUTPUT frames is decoded whole', () => {
    const euro = enc.encode('€') // e2 82 ac
    const s = new Utf8Stream()
    const a = decodeServerMessage(frame('0', euro.subarray(0, 1)), s)
    const b = decodeServerMessage(frame('0', euro.subarray(1)), s)
    expect(a).toMatchObject({ kind: 'output', text: '' })
    expect(b).toMatchObject({ kind: 'output', text: '€' })
  })

  it('a 4-byte character split three ways is still one character, with no replacement characters', () => {
    const bytes = enc.encode('x🙂y')
    const s = new Utf8Stream()
    const parts = [bytes.subarray(0, 2), bytes.subarray(2, 4), bytes.subarray(4)]
    let text = ''
    for (const p of parts) {
      const m = decodeServerMessage(frame('0', p), s)
      if (m.kind === 'output') text += m.text
    }
    expect(text).toBe('x🙂y')
    expect(text).not.toContain('�')
  })

  it('reset drops a pending partial character', () => {
    const s = new Utf8Stream()
    s.push(enc.encode('€').subarray(0, 2))
    s.reset()
    expect(s.push(enc.encode('ok'))).toBe('ok')
  })

  it('SET_WINDOW_TITLE yields a sanitised title', () => {
    const m = decodeServerMessage(frame('1', 'user@host: ~\x07'), new Utf8Stream())
    expect(m).toEqual({ kind: 'title', title: 'user@host: ~' })
  })

  it('SET_PREFERENCES yields the parsed object; garbage is ignored', () => {
    const s = new Utf8Stream()
    expect(decodeServerMessage(frame('2', '{"fontSize":20}'), s)).toEqual({
      kind: 'preferences',
      preferences: { fontSize: 20 },
    })
    expect(decodeServerMessage(frame('2', 'not json'), s)).toEqual({ kind: 'ignored' })
    expect(decodeServerMessage(frame('2', '[1,2]'), s)).toEqual({ kind: 'ignored' })
  })

  it('unknown commands and empty frames are ignored', () => {
    const s = new Utf8Stream()
    expect(decodeServerMessage(frame('9', 'x'), s)).toEqual({ kind: 'ignored' })
    expect(decodeServerMessage(new Uint8Array(0), s)).toEqual({ kind: 'ignored' })
  })
})

describe('sanitizeTitle', () => {
  it('strips control and bidi characters and bounds the length', () => {
    expect(sanitizeTitle('a\u001b[31mb‮c')).not.toMatch(/[\u001b‮]/)
    const long = sanitizeTitle('x'.repeat(500))
    expect(long.length).toBeLessThanOrEqual(121)
    expect(long.endsWith('…')).toBe(true)
  })
})

describe('FlowControl', () => {
  it('does not track small writes', () => {
    const f = new FlowControl(100, 2, 1)
    expect(f.onWrite(50)).toEqual({ track: false, send: null })
  })

  it('pauses once more than highWater tracked writes are outstanding, and resumes below lowWater', () => {
    const f = new FlowControl(10, 2, 1)
    expect(f.onWrite(11)).toEqual({ track: true, send: null }) // pending 1
    expect(f.onWrite(11)).toEqual({ track: true, send: null }) // pending 2
    expect(f.onWrite(11)).toEqual({ track: true, send: PAUSE }) // pending 3 > 2
    expect(f.onWrite(11)).toEqual({ track: true, send: null }) // already paused
    expect(f.done()).toBeNull() // pending 3
    expect(f.done()).toBeNull() // pending 2
    expect(f.done()).toBeNull() // pending 1: not below lowWater (1)
    expect(f.done()).toBe(RESUME) // pending 0
    expect(f.done()).toBeNull() // not paused any more
  })

  it('reset forgets everything', () => {
    const f = new FlowControl(10, 0, 1)
    expect(f.onWrite(11).send).toBe(PAUSE)
    f.reset()
    expect(f.done()).toBeNull()
  })
})

describe('backoffDelay', () => {
  it('doubles from 0.5 s and is capped at 10 s', () => {
    expect([0, 1, 2, 3, 4, 5, 6, 50].map((n) => backoffDelay(n))).toEqual([
      500, 1000, 2000, 4000, 8000, 10000, 10000, 10000,
    ])
  })
  it('treats nonsense attempts as the first', () => {
    expect(backoffDelay(-3)).toBe(500)
    expect(backoffDelay(Number.NaN)).toBe(500)
  })
})
