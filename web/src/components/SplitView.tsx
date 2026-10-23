import {
  useCallback,
  useId,
  useLayoutEffect,
  useRef,
  useState,
  useSyncExternalStore,
  type CSSProperties,
  type KeyboardEvent,
  type PointerEvent,
  type ReactNode,
  type Ref,
} from 'react'
import {
  DEFAULT_RATIO,
  DIVIDER_PX,
  MIN_PANE_PX,
  clampRatio,
  loadRatio,
  ratioBounds,
  ratioForKey,
  ratioFromPointer,
  saveRatio,
  toPercent,
  type Layout,
} from '../split'

/**
 * The panes are stacked on a narrow screen that is also tall enough for two
 * panes one above the other; a phone held sideways (narrow AND short) keeps
 * them side by side, because the stacked panes' minimum heights do not fit it
 * and the page would scroll.
 *
 * styles.css switches the flex direction with exactly this query; CSS cannot
 * import this constant, so a test (SplitView.test.tsx) fails if they differ.
 */
export const STACK_QUERY = '(max-width: 900px) and (min-height: 480px)'

function subscribe(query: string, cb: () => void): () => void {
  if (typeof window === 'undefined' || !window.matchMedia) return () => {}
  const mq = window.matchMedia(query)
  mq.addEventListener('change', cb)
  return () => mq.removeEventListener('change', cb)
}

function matches(query: string): boolean {
  return typeof window !== 'undefined' && !!window.matchMedia && window.matchMedia(query).matches
}

export function useStacked(): boolean {
  return useSyncExternalStore(
    (cb) => subscribe(STACK_QUERY, cb),
    () => matches(STACK_QUERY),
    () => false,
  )
}

interface Props {
  first: ReactNode
  second: ReactNode
  /** Accessible name of the separator. */
  label: string
  /** Lets the page move keyboard focus to the separator (Shift+Esc in the terminal). */
  dividerRef?: Ref<HTMLDivElement>
}

/**
 * Two panes with a draggable divider. The divider is a WAI-ARIA window
 * splitter: role=separator, focusable, arrow keys / Home / End, double-click
 * resets to 50/50. Pointer events cover mouse, touch and pen; a transparent
 * shield covers the panes while dragging so the iframe cannot swallow them.
 */
export function SplitView({ first, second, label, dividerRef }: Props) {
  const stacked = useStacked()
  const layout: Layout = stacked ? 'column' : 'row'
  const minPx = MIN_PANE_PX[layout]

  const containerRef = useRef<HTMLDivElement>(null)
  const firstId = useId()
  const [ratio, setRatio] = useState(() => loadRatio())
  const ratioRef = useRef(ratio)
  const [size, setSize] = useState(0) // container length along the split axis
  const [dragging, setDragging] = useState(false)

  // Track the container size to know how far the divider may move.
  useLayoutEffect(() => {
    const el = containerRef.current
    if (!el) return
    const measure = () => {
      const r = el.getBoundingClientRect()
      setSize(layout === 'row' ? r.width : r.height)
    }
    measure()
    if (typeof ResizeObserver === 'undefined') return
    const ro = new ResizeObserver(measure)
    ro.observe(el)
    return () => ro.disconnect()
  }, [layout])

  const available = Math.max(0, size - DIVIDER_PX)
  const shown = clampRatio(ratio, available, minPx)
  const bounds = ratioBounds(available, minPx)

  const commit = useCallback((next: number, persist: boolean) => {
    ratioRef.current = next
    setRatio(next)
    if (persist) saveRatio(next)
  }, [])

  function onPointerDown(e: PointerEvent<HTMLDivElement>) {
    if (e.pointerType === 'mouse' && e.button !== 0) return
    e.preventDefault()
    e.currentTarget.setPointerCapture?.(e.pointerId)
    setDragging(true)
    e.currentTarget.focus()
  }

  function onPointerMove(e: PointerEvent<HTMLDivElement>) {
    if (!dragging) return
    const el = containerRef.current
    if (!el) return
    const r = el.getBoundingClientRect()
    const next =
      layout === 'row'
        ? ratioFromPointer(e.clientX, r.left, r.width, minPx)
        : ratioFromPointer(e.clientY, r.top, r.height, minPx)
    commit(next, false)
  }

  function endDrag(e: PointerEvent<HTMLDivElement>) {
    if (!dragging) return
    setDragging(false)
    if (e.currentTarget.hasPointerCapture?.(e.pointerId)) e.currentTarget.releasePointerCapture(e.pointerId)
    saveRatio(ratioRef.current)
  }

  function onKeyDown(e: KeyboardEvent<HTMLDivElement>) {
    const next = ratioForKey(e.key, shown, layout, available, minPx, e.shiftKey)
    if (next === null) return
    e.preventDefault()
    commit(next, true)
  }

  const style = {
    '--split': String(shown),
    '--divider': `${DIVIDER_PX}px`,
    '--min-pane': `${minPx}px`,
  } as CSSProperties

  return (
    <div ref={containerRef} className={`split split--${layout}`} style={style}>
      <div id={firstId} className="split__pane split__pane--first">
        {first}
      </div>
      <div
        ref={dividerRef}
        role="separator"
        tabIndex={0}
        aria-label={label}
        aria-orientation={layout === 'row' ? 'vertical' : 'horizontal'}
        aria-controls={firstId}
        aria-valuenow={toPercent(shown)}
        aria-valuemin={toPercent(bounds.min)}
        aria-valuemax={toPercent(bounds.max)}
        aria-valuetext={`${toPercent(shown)} percent terminal, ${100 - toPercent(shown)} percent browser`}
        className={`split__divider${dragging ? ' split__divider--active' : ''}`}
        onPointerDown={onPointerDown}
        onPointerMove={onPointerMove}
        onPointerUp={endDrag}
        onPointerCancel={endDrag}
        onLostPointerCapture={endDrag}
        onKeyDown={onKeyDown}
        onDoubleClick={() => commit(DEFAULT_RATIO, true)}
      >
        <span className="split__grip" aria-hidden="true" />
      </div>
      <div className="split__pane split__pane--second">{second}</div>
      {dragging && <div className="split__shield" aria-hidden="true" />}
    </div>
  )
}
