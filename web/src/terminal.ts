import { FitAddon } from '@xterm/addon-fit'
import { Unicode11Addon } from '@xterm/addon-unicode11'
import { Terminal } from '@xterm/xterm'

// A dark theme in both color schemes: it reads well on a light page too.
const THEME = {
  background: '#14171f',
  foreground: '#d7dce5',
  cursor: '#d7dce5',
  cursorAccent: '#14171f',
  selectionBackground: '#3a4a6b',
  black: '#1c2029',
  red: '#e06c75',
  green: '#98c379',
  yellow: '#e5c07b',
  blue: '#61afef',
  magenta: '#c678dd',
  cyan: '#56b6c2',
  white: '#abb2bf',
  brightBlack: '#5c6370',
  brightRed: '#ef8a93',
  brightGreen: '#b1d98f',
  brightYellow: '#f0d08f',
  brightBlue: '#7fc0fa',
  brightMagenta: '#d68ef0',
  brightCyan: '#74cfdb',
  brightWhite: '#e6e9ef',
}

/** Shown in the terminal pane's bar and used as its accessible description. */
export const LEAVE_HINT = 'Shift+Esc leaves the terminal'

type KeyLike = Pick<KeyboardEvent, 'type' | 'key' | 'shiftKey' | 'ctrlKey' | 'altKey' | 'metaKey'>

/**
 * The chord that gets a keyboard user out of the terminal: Shift+Escape on key
 * down, nothing else held. xterm sends Tab, Shift+Tab and Escape to the shell
 * (completion, vim), so without a chord of our own focus can never leave
 * (WCAG 2.1.2, No Keyboard Trap). A plain Escape and every Tab stay the shell's.
 */
export function isLeaveKey(ev: KeyLike): boolean {
  return ev.type === 'keydown' && ev.key === 'Escape' && ev.shiftKey && !ev.ctrlKey && !ev.altKey && !ev.metaKey
}

export interface TerminalParts {
  term: Terminal
  fit: FitAddon
}

/**
 * The terminal as the workspace page wants it.
 *
 * - Unicode 11 widths. xterm 6 defaults to Unicode 6, where emoji and other
 *   newer wide characters take one cell; the shell (glibc wcwidth) counts two,
 *   so readline's cursor tracking and any column alignment go wrong. The
 *   Unicode API is still "proposed" in xterm, hence allowProposedApi.
 * - Shift+Escape leaves the terminal (`isLeaveKey`): the key is not sent to the
 *   shell, the terminal gives up focus and `onLeave` puts it somewhere else.
 */
export function createTerminal(onLeave: () => void): TerminalParts {
  const term = new Terminal({
    allowProposedApi: true,
    cursorBlink: true,
    fontFamily: 'ui-monospace, SFMono-Regular, Menlo, Consolas, "Liberation Mono", monospace',
    fontSize: 14,
    scrollback: 5000,
    theme: THEME,
  })
  const fit = new FitAddon()
  term.loadAddon(fit)
  term.loadAddon(new Unicode11Addon())
  term.unicode.activeVersion = '11'

  term.attachCustomKeyEventHandler((ev) => {
    if (!isLeaveKey(ev)) return true // true = xterm handles the key as usual
    ev.preventDefault()
    term.blur()
    onLeave()
    return false
  })
  return { term, fit }
}
