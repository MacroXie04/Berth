import { act, render, screen } from '@testing-library/react'
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'
import { LEAVE_HINT } from '../terminal'
import { TerminalPane } from './TerminalPane'

// The real pane, with real xterm.js, in jsdom: only the things jsdom lacks are stubbed.

class FakeWebSocket {
  static all: FakeWebSocket[] = []
  binaryType = 'blob'
  readyState = 0
  onopen: ((ev: unknown) => void) | null = null
  onmessage: ((ev: unknown) => void) | null = null
  onclose: ((ev: unknown) => void) | null = null
  onerror: ((ev: unknown) => void) | null = null
  sent: Uint8Array[] = []
  constructor(
    readonly url: string,
    readonly protocols: string[],
  ) {
    FakeWebSocket.all.push(this)
  }
  send(data: Uint8Array) {
    this.sent.push(data)
  }
  close() {
    this.readyState = 3
  }
  open() {
    this.readyState = 1
    this.onopen?.({})
  }
}

const dec = new TextDecoder()
/** What the page typed at the shell: INPUT frames ("0" + data), as text. */
const typed = (s: FakeWebSocket) => s.sent.map((m) => dec.decode(m)).filter((t) => t.startsWith('0'))

beforeEach(() => {
  FakeWebSocket.all = []
  vi.stubGlobal('WebSocket', FakeWebSocket)
  vi.stubGlobal(
    'ResizeObserver',
    class {
      observe() {}
      unobserve() {}
      disconnect() {}
    },
  )
  vi.stubGlobal('matchMedia', (q: string) => ({
    matches: false,
    media: q,
    addEventListener() {},
    removeEventListener() {},
    addListener() {},
    removeListener() {},
  }))
})
afterEach(() => {
  vi.unstubAllGlobals()
})

function key(type: string, k: string, shift = false) {
  return new KeyboardEvent(type, {
    key: k,
    code: k,
    keyCode: k === 'Escape' ? 27 : k === 'Tab' ? 9 : 0,
    shiftKey: shift,
    bubbles: true,
    cancelable: true,
  })
}

const textarea = () => document.querySelector<HTMLTextAreaElement>('.xterm-helper-textarea')!

describe('TerminalPane: leaving the keyboard trap', () => {
  it('says how to leave, in the pane bar and as the description of the pane and of the input', () => {
    render(<TerminalPane username="alice" />)
    const hint = screen.getByText(LEAVE_HINT)
    expect(hint).toBeVisible()
    expect(LEAVE_HINT).toBe('Shift+Esc leaves the terminal')
    expect(screen.getByRole('region', { name: 'Terminal' })).toHaveAccessibleDescription(LEAVE_HINT)
    expect(textarea()).toHaveAccessibleDescription(LEAVE_HINT)
  })

  it('Shift+Escape in the terminal calls onLeave and sends nothing to the shell', () => {
    const onLeave = vi.fn()
    render(<TerminalPane username="alice" onLeave={onLeave} />)
    const sock = FakeWebSocket.all[0]!
    act(() => sock.open())
    expect(textarea()).toHaveFocus() // the terminal takes focus when it connects
    act(() => {
      textarea().dispatchEvent(key('keydown', 'Escape', true))
    })
    expect(onLeave).toHaveBeenCalledTimes(1)
    expect(textarea()).not.toHaveFocus()
    expect(typed(sock)).toEqual([])
  })

  it('uses the latest onLeave, not the one of the first render', () => {
    const first = vi.fn()
    const second = vi.fn()
    const { rerender } = render(<TerminalPane username="alice" onLeave={first} />)
    rerender(<TerminalPane username="alice" onLeave={second} />)
    act(() => {
      textarea().dispatchEvent(key('keydown', 'Escape', true))
    })
    expect(first).not.toHaveBeenCalled()
    expect(second).toHaveBeenCalledTimes(1)
  })

  it('without onLeave the terminal still gives up focus', () => {
    render(<TerminalPane username="alice" />)
    act(() => FakeWebSocket.all[0]!.open())
    expect(textarea()).toHaveFocus()
    act(() => {
      textarea().dispatchEvent(key('keydown', 'Escape', true))
    })
    expect(textarea()).not.toHaveFocus()
  })

  it('plain Escape and Tab are still typed into the shell', () => {
    const onLeave = vi.fn()
    render(<TerminalPane username="alice" onLeave={onLeave} />)
    const sock = FakeWebSocket.all[0]!
    act(() => sock.open())
    act(() => {
      textarea().dispatchEvent(key('keydown', 'Escape'))
      textarea().dispatchEvent(key('keydown', 'Tab'))
    })
    expect(typed(sock)).toEqual(['0\x1b', '0\t'])
    expect(onLeave).not.toHaveBeenCalled()
  })
})
