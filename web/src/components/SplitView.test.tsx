import { readFileSync } from 'node:fs'
import { resolve } from 'node:path'
import { createRef } from 'react'
import { fireEvent, render, screen } from '@testing-library/react'
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'
import { STORAGE_KEY } from '../split'
import { STACK_QUERY, SplitView } from './SplitView'

function rect(width: number, height: number) {
  return { left: 0, top: 0, width, height, right: width, bottom: height, x: 0, y: 0, toJSON() {} } as DOMRect
}

function mockMatchMedia(stacked: boolean) {
  const impl = (q: string) => ({
    matches: q === STACK_QUERY ? stacked : false,
    media: q,
    addEventListener: () => {},
    removeEventListener: () => {},
  })
  vi.stubGlobal('matchMedia', impl)
  window.matchMedia = impl as unknown as typeof window.matchMedia
}

function setup(opts: { stacked?: boolean; width?: number; height?: number } = {}) {
  mockMatchMedia(opts.stacked ?? false)
  vi.spyOn(HTMLElement.prototype, 'getBoundingClientRect').mockReturnValue(rect(opts.width ?? 1010, opts.height ?? 610))
  const utils = render(<SplitView label="Resize panes" first={<p>terminal pane</p>} second={<p>browser pane</p>} />)
  const sep = screen.getByRole('separator', { name: 'Resize panes' })
  return { ...utils, sep }
}

beforeEach(() => {
  localStorage.clear()
})
afterEach(() => {
  vi.unstubAllGlobals()
  // @ts-expect-error cleanup of the stub
  delete window.matchMedia
})

const now = (el: HTMLElement) => Number(el.getAttribute('aria-valuenow'))

describe('SplitView separator semantics', () => {
  it('is a focusable vertical separator with value, bounds and a name when side by side', () => {
    const { sep } = setup()
    expect(sep).toHaveAttribute('tabindex', '0')
    expect(sep).toHaveAttribute('aria-orientation', 'vertical')
    expect(now(sep)).toBe(50)
    // available = 1010 - 10 = 1000, min pane 240 => 24..76
    expect(sep).toHaveAttribute('aria-valuemin', '24')
    expect(sep).toHaveAttribute('aria-valuemax', '76')
    expect(screen.getByText('terminal pane')).toBeInTheDocument()
    expect(screen.getByText('browser pane')).toBeInTheDocument()
  })

  it('is a horizontal separator when stacked on a narrow screen', () => {
    const { sep, container } = setup({ stacked: true, width: 360, height: 610 })
    expect(sep).toHaveAttribute('aria-orientation', 'horizontal')
    expect(container.querySelector('.split')).toHaveClass('split--column')
    // available 600, min pane 140 => about 23..77
    expect(now(sep)).toBe(50)
    expect(Number(sep.getAttribute('aria-valuemin'))).toBe(23)
  })
})

describe('keyboard', () => {
  it('Left/Right change the split by 5 percent points; Shift by 10', () => {
    const { sep } = setup()
    fireEvent.keyDown(sep, { key: 'ArrowRight' })
    expect(now(sep)).toBe(55)
    fireEvent.keyDown(sep, { key: 'ArrowLeft' })
    fireEvent.keyDown(sep, { key: 'ArrowLeft' })
    expect(now(sep)).toBe(45)
    fireEvent.keyDown(sep, { key: 'ArrowRight', shiftKey: true })
    expect(now(sep)).toBe(55)
  })

  it('Home and End go to the minimum and maximum, and never beyond', () => {
    const { sep } = setup()
    fireEvent.keyDown(sep, { key: 'Home' })
    expect(now(sep)).toBe(24)
    fireEvent.keyDown(sep, { key: 'ArrowLeft' })
    expect(now(sep)).toBe(24)
    fireEvent.keyDown(sep, { key: 'End' })
    expect(now(sep)).toBe(76)
    fireEvent.keyDown(sep, { key: 'ArrowRight' })
    expect(now(sep)).toBe(76)
  })

  it('stacked: Up/Down move it and Left/Right do not', () => {
    const { sep } = setup({ stacked: true, width: 360, height: 610 })
    fireEvent.keyDown(sep, { key: 'ArrowRight' })
    expect(now(sep)).toBe(50)
    fireEvent.keyDown(sep, { key: 'ArrowDown' })
    expect(now(sep)).toBe(55)
    fireEvent.keyDown(sep, { key: 'ArrowUp' })
    fireEvent.keyDown(sep, { key: 'ArrowUp' })
    expect(now(sep)).toBe(45)
  })

  it('handled keys do not scroll the page, other keys are left alone', () => {
    const { sep } = setup()
    const handled = fireEvent.keyDown(sep, { key: 'ArrowRight' })
    const other = fireEvent.keyDown(sep, { key: 'Tab' })
    expect(handled).toBe(false) // default prevented
    expect(other).toBe(true)
  })

  it('double-click resets to 50/50', () => {
    const { sep } = setup()
    fireEvent.keyDown(sep, { key: 'End' })
    expect(now(sep)).toBe(76)
    fireEvent.doubleClick(sep)
    expect(now(sep)).toBe(50)
    expect(localStorage.getItem(STORAGE_KEY)).toBe('0.5')
  })
})

describe('pointer dragging', () => {
  it('follows the pointer, clamps to the minimum pane size, and persists on release', () => {
    const { sep, container } = setup()
    expect(container.querySelector('.split__shield')).toBeNull()

    fireEvent.pointerDown(sep, { pointerId: 1, button: 0, pointerType: 'mouse', clientX: 505 })
    // while dragging a transparent shield covers the panes so the iframe cannot swallow pointer events
    expect(container.querySelector('.split__shield')).not.toBeNull()
    expect(sep.className).toContain('active')

    fireEvent.pointerMove(sep, { pointerId: 1, clientX: 305 })
    expect(now(sep)).toBe(30)
    expect(localStorage.getItem(STORAGE_KEY)).toBeNull() // not saved on every move

    fireEvent.pointerMove(sep, { pointerId: 1, clientX: 5 })
    expect(now(sep)).toBe(24)
    fireEvent.pointerMove(sep, { pointerId: 1, clientX: 5000 })
    expect(now(sep)).toBe(76)

    fireEvent.pointerUp(sep, { pointerId: 1 })
    expect(container.querySelector('.split__shield')).toBeNull()
    expect(Number(localStorage.getItem(STORAGE_KEY))).toBeCloseTo(0.76)

    // after the release, moving does nothing
    fireEvent.pointerMove(sep, { pointerId: 1, clientX: 305 })
    expect(now(sep)).toBe(76)
  })

  it('works along the vertical axis when stacked', () => {
    const { sep } = setup({ stacked: true, width: 360, height: 610 })
    fireEvent.pointerDown(sep, { pointerId: 1, button: 0, pointerType: 'touch', clientY: 305 })
    fireEvent.pointerMove(sep, { pointerId: 1, clientY: 185 })
    // (185 - 5) / 600 = 0.3
    expect(now(sep)).toBe(30)
    fireEvent.pointerUp(sep, { pointerId: 1 })
  })

  it('ignores moves that never started with a press, and non-primary mouse buttons', () => {
    const { sep, container } = setup()
    fireEvent.pointerMove(sep, { pointerId: 1, clientX: 100 })
    expect(now(sep)).toBe(50)
    fireEvent.pointerDown(sep, { pointerId: 1, button: 2, pointerType: 'mouse', clientX: 505 })
    expect(container.querySelector('.split__shield')).toBeNull()
  })
})

describe('persistence', () => {
  it('restores the saved ratio', () => {
    localStorage.setItem(STORAGE_KEY, '0.3')
    const { sep } = setup()
    expect(now(sep)).toBe(30)
  })

  it('keeps a saved ratio that does not fit the current width inside the allowed range', () => {
    localStorage.setItem(STORAGE_KEY, '0.05')
    const { sep } = setup()
    expect(now(sep)).toBe(24)
  })

  it('saves keyboard changes', () => {
    const { sep } = setup()
    fireEvent.keyDown(sep, { key: 'ArrowRight' })
    expect(Number(localStorage.getItem(STORAGE_KEY))).toBeCloseTo(0.55)
  })

  it('still works when localStorage throws', () => {
    const getItem = vi.spyOn(Storage.prototype, 'getItem').mockImplementation(() => {
      throw new Error('blocked')
    })
    const setItem = vi.spyOn(Storage.prototype, 'setItem').mockImplementation(() => {
      throw new Error('blocked')
    })
    const { sep } = setup()
    expect(now(sep)).toBe(50)
    fireEvent.keyDown(sep, { key: 'ArrowRight' })
    expect(now(sep)).toBe(55)
    expect(getItem).toHaveBeenCalled()
    expect(setItem).toHaveBeenCalled()
  })
})

describe('the pane divider as a place for focus (Shift+Esc in the terminal lands here)', () => {
  it('hands its element to dividerRef, and it takes focus', () => {
    mockMatchMedia(false)
    vi.spyOn(HTMLElement.prototype, 'getBoundingClientRect').mockReturnValue(rect(1010, 610))
    const ref = createRef<HTMLDivElement>()
    render(<SplitView label="Resize panes" dividerRef={ref} first={<p>t</p>} second={<p>b</p>} />)
    const sep = screen.getByRole('separator', { name: 'Resize panes' })
    expect(ref.current).toBe(sep)
    ref.current!.focus()
    expect(sep).toHaveFocus()
  })
})

describe('when the panes are stacked', () => {
  const css = readFileSync(resolve(import.meta.dirname, '../styles.css'), 'utf8')

  /** Evaluates STACK_QUERY (max-width and min-height conditions only) for a viewport. */
  function stackedAt(width: number, height: number): boolean {
    const maxWidth = Number(/\(max-width:\s*(\d+)px\)/.exec(STACK_QUERY)?.[1])
    const minHeight = Number(/\(min-height:\s*(\d+)px\)/.exec(STACK_QUERY)?.[1])
    return width <= maxWidth && height >= minHeight
  }

  it('narrow and tall screens stack; phones held sideways and wide screens do not', () => {
    expect(stackedAt(360, 740)).toBe(true) // phone, portrait
    expect(stackedAt(768, 1024)).toBe(true) // tablet, portrait
    expect(stackedAt(667, 375)).toBe(false) // phone, landscape: two 140 px panes do not fit under the status bar
    expect(stackedAt(740, 360)).toBe(false)
    expect(stackedAt(1400, 800)).toBe(false) // desktop
  })

  it('styles.css switches the flex direction with exactly the same query as STACK_QUERY', () => {
    // CSS cannot import the constant; this is what keeps the two from drifting apart.
    const rule = new RegExp(
      `@media ${STACK_QUERY.replace(/[()]/g, '\\$&')}\\s*\\{\\s*\\.split\\s*\\{\\s*flex-direction:\\s*column;`,
    )
    expect(css).toMatch(rule)
  })

  it('and no other rule in styles.css decides on the same breakpoint', () => {
    expect(css.match(/max-width:\s*900px/g)).toHaveLength(1)
    expect(css.match(/min-height:\s*480px/g)).toHaveLength(1)
  })

  it('the side-by-side direction is the default, so a short landscape viewport keeps it', () => {
    expect(css).toMatch(/\.split\s*\{[^}]*flex-direction:\s*row;/)
  })
})
