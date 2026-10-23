/**
 * Pure logic of the resizable terminal/browser split. The first pane is the
 * terminal, the second the browser. `ratio` is the first pane's share (0..1) of
 * the space that is left after the divider.
 */

/** 'row': side by side (vertical separator). 'column': stacked (horizontal separator). */
export type Layout = 'row' | 'column'

export const DEFAULT_RATIO = 0.5
export const DIVIDER_PX = 10
export const MIN_PANE_PX: Record<Layout, number> = { row: 240, column: 140 }
export const STEP = 0.05
export const STEP_LARGE = 0.1
export const STORAGE_KEY = 'berth.split'

/** Allowed ratio range so that both panes keep at least `minPx` of `available`. */
export function ratioBounds(available: number, minPx: number): { min: number; max: number } {
  if (!Number.isFinite(available) || available <= 2 * minPx) return { min: DEFAULT_RATIO, max: DEFAULT_RATIO }
  const min = minPx / available
  return { min, max: 1 - min }
}

export function clampRatio(ratio: number, available: number, minPx: number): number {
  const { min, max } = ratioBounds(available, minPx)
  const r = Number.isFinite(ratio) ? ratio : DEFAULT_RATIO
  return Math.min(max, Math.max(min, r))
}

/**
 * Ratio for a pointer at `pointer` (client coordinate along the split axis) in
 * a container that starts at `start` and is `size` long; the divider is
 * centred under the pointer.
 */
export function ratioFromPointer(
  pointer: number,
  start: number,
  size: number,
  minPx: number,
  dividerPx = DIVIDER_PX,
): number {
  const available = size - dividerPx
  if (available <= 0) return DEFAULT_RATIO
  return clampRatio((pointer - start - dividerPx / 2) / available, available, minPx)
}

/**
 * New ratio for a key pressed on the separator, or null when the key does not
 * apply (so the caller leaves the event alone). The WAI-ARIA window splitter
 * keys: a vertical separator (side by side) moves with Left/Right, a
 * horizontal one (stacked) with Up/Down; Home/End jump to the extremes.
 */
export function ratioForKey(
  key: string,
  ratio: number,
  layout: Layout,
  available: number,
  minPx: number,
  large = false,
): number | null {
  const step = large ? STEP_LARGE : STEP
  const dec = layout === 'row' ? 'ArrowLeft' : 'ArrowUp'
  const inc = layout === 'row' ? 'ArrowRight' : 'ArrowDown'
  let next: number
  switch (key) {
    case dec:
      next = ratio - step
      break
    case inc:
      next = ratio + step
      break
    case 'Home':
      next = 0
      break
    case 'End':
      next = 1
      break
    default:
      return null
  }
  return clampRatio(next, available, minPx)
}

/** aria-valuenow etc.: whole percent of the first pane. */
export function toPercent(ratio: number): number {
  return Math.round(ratio * 100)
}

// --- persistence: localStorage can throw (private mode, blocked site data) ---

type StorageLike = Pick<Storage, 'getItem' | 'setItem'>

function defaultStorage(): StorageLike | null {
  try {
    return window.localStorage
  } catch {
    return null
  }
}

/** The saved ratio, or the default when missing, unreadable or out of range. */
export function loadRatio(storage: StorageLike | null = defaultStorage()): number {
  try {
    const raw = storage?.getItem(STORAGE_KEY)
    if (raw == null) return DEFAULT_RATIO
    const n = Number(raw)
    return Number.isFinite(n) && n > 0 && n < 1 ? n : DEFAULT_RATIO
  } catch {
    return DEFAULT_RATIO
  }
}

export function saveRatio(ratio: number, storage: StorageLike | null = defaultStorage()): void {
  try {
    storage?.setItem(STORAGE_KEY, String(Math.round(ratio * 10000) / 10000))
  } catch {
    // not persisted; the split still works for this page view
  }
}
