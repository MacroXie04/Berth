import { Terminal } from '@xterm/xterm'
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'
import { createTerminal, isLeaveKey } from './terminal'

// xterm.js reads matchMedia when it opens, which jsdom does not have.
beforeEach(() => {
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

const KEYCODE: Record<string, number> = { Escape: 27, Tab: 9 }

function keyEvent(type: string, key: string, mods: { shift?: boolean; ctrl?: boolean; alt?: boolean; meta?: boolean } = {}) {
  return new KeyboardEvent(type, {
    key,
    code: key,
    keyCode: KEYCODE[key] ?? 0,
    shiftKey: !!mods.shift,
    ctrlKey: !!mods.ctrl,
    altKey: !!mods.alt,
    metaKey: !!mods.meta,
    bubbles: true,
    cancelable: true,
  })
}

function written(term: Terminal, text: string): Promise<void> {
  return new Promise((resolve) => term.write(text, resolve))
}

describe('isLeaveKey', () => {
  it('is Shift+Escape on key down and nothing else', () => {
    expect(isLeaveKey(keyEvent('keydown', 'Escape', { shift: true }))).toBe(true)
  })

  it.each([
    ['Escape (vim, readline)', keyEvent('keydown', 'Escape')],
    ['Tab (completion)', keyEvent('keydown', 'Tab')],
    ['Shift+Tab', keyEvent('keydown', 'Tab', { shift: true })],
    ['Ctrl+Shift+Escape', keyEvent('keydown', 'Escape', { shift: true, ctrl: true })],
    ['Alt+Shift+Escape', keyEvent('keydown', 'Escape', { shift: true, alt: true })],
    ['Meta+Shift+Escape', keyEvent('keydown', 'Escape', { shift: true, meta: true })],
    ['Shift+Escape on key up', keyEvent('keyup', 'Escape', { shift: true })],
    ['Shift+Escape on key press', keyEvent('keypress', 'Escape', { shift: true })],
  ])('is not %s', (_name, ev) => {
    expect(isLeaveKey(ev)).toBe(false)
  })
})

describe('createTerminal: character widths', () => {
  it('counts an emoji as two cells (Unicode 11), like the shell does', async () => {
    const { term } = createTerminal(() => {})
    expect(term.unicode.activeVersion).toBe('11')
    await written(term, '\u{1F600}') // 😀
    expect(term.buffer.active.cursorX).toBe(2)
    term.dispose()
  })

  it('wide CJK characters take two cells and ASCII one', async () => {
    const { term } = createTerminal(() => {})
    await written(term, 'a漢b')
    expect(term.buffer.active.cursorX).toBe(4)
    term.dispose()
  })

  it('the default xterm.js counts that emoji as one cell, which is why the addon is needed', async () => {
    const plain = new Terminal({ allowProposedApi: true })
    await written(plain, '\u{1F600}')
    expect(plain.unicode.activeVersion).toBe('6')
    expect(plain.buffer.active.cursorX).toBe(1)
    plain.dispose()
  })
})

describe('createTerminal: leaving the terminal with the keyboard', () => {
  function open(onLeave = vi.fn()) {
    const { term } = createTerminal(onLeave)
    const host = document.createElement('div')
    document.body.appendChild(host)
    term.open(host)
    const data: string[] = []
    term.onData((d) => data.push(d))
    const textarea = term.textarea!
    textarea.focus()
    return {
      term,
      textarea,
      data,
      onLeave,
      press: (ev: KeyboardEvent) => {
        textarea.dispatchEvent(ev)
        return ev
      },
      done: () => {
        term.dispose()
        host.remove()
      },
    }
  }

  it('Shift+Escape is not sent to the shell, the terminal gives up focus, and the page is told', () => {
    const t = open()
    expect(t.textarea).toHaveFocus()
    const ev = t.press(keyEvent('keydown', 'Escape', { shift: true }))
    expect(t.data).toEqual([])
    expect(t.onLeave).toHaveBeenCalledTimes(1)
    expect(t.textarea).not.toHaveFocus()
    expect(ev.defaultPrevented).toBe(true)
    t.done()
  })

  it('the key up of the chord does not leave a second time or reach the shell', () => {
    const t = open()
    t.press(keyEvent('keydown', 'Escape', { shift: true }))
    t.press(keyEvent('keyup', 'Escape', { shift: true }))
    expect(t.onLeave).toHaveBeenCalledTimes(1)
    expect(t.data).toEqual([])
    t.done()
  })

  it('a plain Escape still goes to the shell (vim), and does not leave', () => {
    const t = open()
    t.press(keyEvent('keydown', 'Escape'))
    expect(t.data).toEqual(['\x1b'])
    expect(t.onLeave).not.toHaveBeenCalled()
    expect(t.textarea).toHaveFocus()
    t.done()
  })

  it('Tab (completion) and Shift+Tab still go to the shell and keep focus in the terminal', () => {
    const t = open()
    const tab = t.press(keyEvent('keydown', 'Tab'))
    const back = t.press(keyEvent('keydown', 'Tab', { shift: true }))
    expect(t.data).toEqual(['\t', '\x1b[Z'])
    expect(tab.defaultPrevented).toBe(true) // xterm keeps the browser from moving focus
    expect(back.defaultPrevented).toBe(true)
    expect(t.onLeave).not.toHaveBeenCalled()
    expect(t.textarea).toHaveFocus()
    t.done()
  })

  it('Ctrl+Shift+Escape is not the chord', () => {
    const t = open()
    t.press(keyEvent('keydown', 'Escape', { shift: true, ctrl: true }))
    expect(t.onLeave).not.toHaveBeenCalled()
    t.done()
  })
})
