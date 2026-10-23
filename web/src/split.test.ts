import { describe, expect, it } from 'vitest'
import {
  DEFAULT_RATIO,
  STEP,
  STEP_LARGE,
  STORAGE_KEY,
  clampRatio,
  loadRatio,
  ratioBounds,
  ratioForKey,
  ratioFromPointer,
  saveRatio,
  toPercent,
} from './split'

function memoryStorage(initial: Record<string, string> = {}) {
  const data = { ...initial }
  return {
    data,
    getItem: (k: string) => (k in data ? data[k]! : null),
    setItem: (k: string, v: string) => {
      data[k] = v
    },
  }
}

const throwing = {
  getItem: () => {
    throw new Error('blocked')
  },
  setItem: () => {
    throw new Error('blocked')
  },
}

describe('ratioBounds / clampRatio', () => {
  it('keeps both panes at least minPx wide', () => {
    const { min, max } = ratioBounds(1000, 240)
    expect(min).toBeCloseTo(0.24)
    expect(max).toBeCloseTo(0.76)
    expect(clampRatio(0.01, 1000, 240)).toBeCloseTo(0.24)
    expect(clampRatio(0.99, 1000, 240)).toBeCloseTo(0.76)
    expect(clampRatio(0.4, 1000, 240)).toBe(0.4)
  })
  it('falls back to 50/50 when there is not room for two minimum panes', () => {
    expect(ratioBounds(400, 240)).toEqual({ min: 0.5, max: 0.5 })
    expect(clampRatio(0.1, 400, 240)).toBe(0.5)
    expect(clampRatio(0.9, 0, 240)).toBe(0.5)
  })
  it('replaces NaN with the default', () => {
    expect(clampRatio(Number.NaN, 1000, 240)).toBe(DEFAULT_RATIO)
  })
})

describe('ratioFromPointer', () => {
  it('centres the divider under the pointer', () => {
    // container 0..1010, divider 10 => available 1000; pointer at 505 => first pane 500
    expect(ratioFromPointer(505, 0, 1010, 100)).toBeCloseTo(0.5)
    expect(ratioFromPointer(305, 0, 1010, 100)).toBeCloseTo(0.3)
  })
  it('accounts for the container offset', () => {
    expect(ratioFromPointer(605, 100, 1010, 100)).toBeCloseTo(0.5)
  })
  it('clamps at both ends so neither pane disappears', () => {
    expect(ratioFromPointer(-500, 0, 1010, 240)).toBeCloseTo(0.24)
    expect(ratioFromPointer(5000, 0, 1010, 240)).toBeCloseTo(0.76)
  })
  it('does not divide by zero in a collapsed container', () => {
    expect(ratioFromPointer(5, 0, 10, 100)).toBe(DEFAULT_RATIO)
  })
})

describe('ratioForKey', () => {
  const av = 1000
  const min = 100
  it('side by side: Left/Right move the divider, Up/Down do nothing', () => {
    expect(ratioForKey('ArrowRight', 0.5, 'row', av, min)).toBeCloseTo(0.5 + STEP)
    expect(ratioForKey('ArrowLeft', 0.5, 'row', av, min)).toBeCloseTo(0.5 - STEP)
    expect(ratioForKey('ArrowUp', 0.5, 'row', av, min)).toBeNull()
    expect(ratioForKey('ArrowDown', 0.5, 'row', av, min)).toBeNull()
  })
  it('stacked: Up/Down move the divider, Left/Right do nothing', () => {
    expect(ratioForKey('ArrowDown', 0.5, 'column', av, min)).toBeCloseTo(0.5 + STEP)
    expect(ratioForKey('ArrowUp', 0.5, 'column', av, min)).toBeCloseTo(0.5 - STEP)
    expect(ratioForKey('ArrowLeft', 0.5, 'column', av, min)).toBeNull()
  })
  it('Shift takes the large step', () => {
    expect(ratioForKey('ArrowRight', 0.5, 'row', av, min, true)).toBeCloseTo(0.5 + STEP_LARGE)
  })
  it('Home and End jump to the extremes allowed by the minimum size', () => {
    expect(ratioForKey('Home', 0.5, 'row', av, min)).toBeCloseTo(0.1)
    expect(ratioForKey('End', 0.5, 'row', av, min)).toBeCloseTo(0.9)
  })
  it('never leaves the allowed range', () => {
    expect(ratioForKey('ArrowLeft', 0.1, 'row', av, min)).toBeCloseTo(0.1)
    expect(ratioForKey('ArrowRight', 0.9, 'row', av, min)).toBeCloseTo(0.9)
  })
  it('ignores other keys', () => {
    expect(ratioForKey('a', 0.5, 'row', av, min)).toBeNull()
    expect(ratioForKey('Enter', 0.5, 'row', av, min)).toBeNull()
  })
})

describe('toPercent', () => {
  it('rounds to a whole percent', () => {
    expect(toPercent(0.333)).toBe(33)
    expect(toPercent(0.5)).toBe(50)
  })
})

describe('persistence', () => {
  it('round-trips a ratio', () => {
    const st = memoryStorage()
    saveRatio(0.3333333, st)
    expect(st.data[STORAGE_KEY]).toBe('0.3333')
    expect(loadRatio(st)).toBeCloseTo(0.3333)
  })
  it('falls back to the default for missing, malformed or out-of-range values', () => {
    expect(loadRatio(memoryStorage())).toBe(DEFAULT_RATIO)
    for (const v of ['abc', '', '0', '1', '-0.2', '7', 'NaN', 'Infinity']) {
      expect(loadRatio(memoryStorage({ [STORAGE_KEY]: v }))).toBe(DEFAULT_RATIO)
    }
  })
  it('survives storage that throws (private mode, blocked site data)', () => {
    expect(loadRatio(throwing)).toBe(DEFAULT_RATIO)
    expect(() => saveRatio(0.4, throwing)).not.toThrow()
    expect(loadRatio(null)).toBe(DEFAULT_RATIO)
    expect(() => saveRatio(0.4, null)).not.toThrow()
  })
})
