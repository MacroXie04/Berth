import { fireEvent, render, screen } from '@testing-library/react'
import { describe, expect, it, vi } from 'vitest'
import type { WorkspaceState, WorkspaceStatus } from '../types'
import type { Busy } from '../workspaceView'
import { StatusBar } from './StatusBar'

function status(state: WorkspaceState): WorkspaceStatus {
  return {
    state,
    health: state === 'running' ? 'healthy' : 'none',
    mem_limit_mb: 2048,
    cpu_limit: 2,
    last_active_at: null,
    last_stopped_at: null,
    usage: null,
  }
}

function bar(s: WorkspaceStatus | null, busy: Busy = null, onPowerOff = vi.fn()) {
  return (
    <StatusBar
      username="alice"
      status={s}
      busy={busy}
      stale={false}
      logoutError={null}
      onPowerOff={onPowerOff}
      onLogout={() => {}}
    />
  )
}

const confirmation = () => screen.queryByRole('group', { name: /confirm power off/i })

describe('the power-off confirmation', () => {
  it('opens on Power off, and Yes calls onPowerOff once and closes it', () => {
    const onPowerOff = vi.fn()
    render(bar(status('running'), null, onPowerOff))
    fireEvent.click(screen.getByRole('button', { name: 'Power off' }))
    expect(confirmation()).not.toBeNull()
    fireEvent.click(screen.getByRole('button', { name: /yes, power off/i }))
    expect(onPowerOff).toHaveBeenCalledTimes(1)
    expect(confirmation()).toBeNull()
  })

  it('goes away when the workspace stops by itself while the question is open (a poll reports "stopped")', () => {
    const onPowerOff = vi.fn()
    const { rerender } = render(bar(status('running'), null, onPowerOff))
    fireEvent.click(screen.getByRole('button', { name: 'Power off' }))
    expect(confirmation()).not.toBeNull()

    rerender(bar(status('stopped'), null, onPowerOff))
    expect(confirmation()).toBeNull()
    expect(screen.queryByRole('button', { name: /yes, power off/i })).toBeNull()
    expect(screen.getByRole('button', { name: 'Power off' })).toBeDisabled()

    // and it does not come back by itself when the workspace is running again
    rerender(bar(status('running'), null, onPowerOff))
    expect(confirmation()).toBeNull()
    expect(onPowerOff).not.toHaveBeenCalled()
  })

  it('goes away when the container is gone (offline) as well', () => {
    const { rerender } = render(bar(status('running')))
    fireEvent.click(screen.getByRole('button', { name: 'Power off' }))
    rerender(bar(status('offline')))
    expect(confirmation()).toBeNull()
  })

  it('goes away while a Power off is under way', () => {
    const { rerender } = render(bar(status('running')))
    fireEvent.click(screen.getByRole('button', { name: 'Power off' }))
    rerender(bar(status('running'), 'stopping'))
    expect(confirmation()).toBeNull()
  })

  it('Cancel closes it without powering off', () => {
    const onPowerOff = vi.fn()
    render(bar(status('running'), null, onPowerOff))
    fireEvent.click(screen.getByRole('button', { name: 'Power off' }))
    fireEvent.click(screen.getByRole('button', { name: 'Cancel' }))
    expect(confirmation()).toBeNull()
    expect(onPowerOff).not.toHaveBeenCalled()
  })
})
