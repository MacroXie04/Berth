import { useCallback, useEffect, useMemo, useState } from 'react'
import { ApiError, createApi } from './api'
import { LoginPage } from './components/LoginPage'
import { WorkspacePage } from './components/WorkspacePage'
import type { User } from './types'

type Screen =
  | { kind: 'boot' }
  | { kind: 'anon'; notice: string | null }
  | { kind: 'authed'; user: User }
  | { kind: 'unreachable'; message: string }

export const SESSION_EXPIRED = 'Your session expired. Please log in again.'

export function App() {
  const [screen, setScreen] = useState<Screen>({ kind: 'boot' })

  // 401 from any authenticated call: back to the login page. Only a user who
  // was logged in is told their session expired; at first load it is just login.
  const onUnauthorized = useCallback(() => {
    setScreen((s) => {
      if (s.kind !== 'authed') return s
      return { kind: 'anon', notice: SESSION_EXPIRED }
    })
  }, [])

  const api = useMemo(() => createApi({ onUnauthorized }), [onUnauthorized])

  const boot = useCallback(async () => {
    setScreen({ kind: 'boot' })
    try {
      const { user } = await api.me()
      setScreen({ kind: 'authed', user })
    } catch (err) {
      if (err instanceof ApiError && err.status === 401) {
        setScreen({ kind: 'anon', notice: null })
      } else {
        setScreen({
          kind: 'unreachable',
          message: err instanceof ApiError ? err.message : 'Could not reach the server.',
        })
      }
    }
  }, [api])

  useEffect(() => {
    void boot()
  }, [boot])

  switch (screen.kind) {
    case 'boot':
      return (
        <main className="auth">
          <div className="auth__card" aria-busy="true">
            <h1 className="auth__brand">Berth</h1>
            <p className="auth__tagline" role="status">
              Loading…
            </p>
          </div>
        </main>
      )
    case 'unreachable':
      return (
        <main className="auth">
          <div className="auth__card">
            <h1 className="auth__brand">Berth</h1>
            <p className="banner banner--error" role="alert">
              {screen.message}
            </p>
            <button type="button" className="btn btn--primary btn--block" onClick={() => void boot()}>
              Try again
            </button>
          </div>
        </main>
      )
    case 'anon':
      return <LoginPage api={api} notice={screen.notice} onLoggedIn={(user) => setScreen({ kind: 'authed', user })} />
    case 'authed':
      return (
        <WorkspacePage
          key={screen.user.username}
          api={api}
          username={screen.user.username}
          onLoggedOut={() => setScreen({ kind: 'anon', notice: null })}
          onIdentityChanged={() => void boot()}
        />
      )
  }
}
