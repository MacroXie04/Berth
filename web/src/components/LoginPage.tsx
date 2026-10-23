import { useId, useRef, useState, type FormEvent, type KeyboardEvent } from 'react'
import { ApiError, type Api } from '../api'
import type { User } from '../types'
import {
  PASSWORD_HINT,
  USERNAME_HINT,
  passwordBytes,
  passwordProblem,
  usernameProblem,
  PASSWORD_MAX_BYTES,
} from '../validation'

type Mode = 'login' | 'register'

interface Props {
  api: Api
  /** Shown above the form, e.g. "Your session expired". */
  notice?: string | null
  onLoggedIn(user: User): void
}

/** Login and registration in one form. Registering does not log in: it creates the account, then logs in. */
export function LoginPage({ api, notice, onLoggedIn }: Props) {
  const [mode, setMode] = useState<Mode>('login')
  const [username, setUsername] = useState('')
  const [password, setPassword] = useState('')
  const [touched, setTouched] = useState({ username: false, password: false })
  const [formError, setFormError] = useState<string | null>(null)
  const [usernameError, setUsernameError] = useState<string | null>(null)
  const [info, setInfo] = useState<string | null>(null)
  const [busy, setBusy] = useState(false)
  const usernameRef = useRef<HTMLInputElement>(null)
  const passwordRef = useRef<HTMLInputElement>(null)
  const ids = { u: useId(), p: useId(), uh: useId(), ph: useId(), tabLogin: useId(), tabRegister: useId() }
  const tabRefs = { login: useRef<HTMLButtonElement>(null), register: useRef<HTMLButtonElement>(null) }

  const register = mode === 'register'
  // Format rules only matter when creating an account; at login the server decides.
  const uProblem = register && touched.username ? usernameProblem(username) : null
  const pProblem = register && touched.password ? passwordProblem(password) : null
  const uMessage = usernameError ?? uProblem

  function switchMode(next: Mode) {
    setMode(next)
    setFormError(null)
    setUsernameError(null)
    setInfo(null)
    setTouched({ username: false, password: false })
  }

  // WAI-ARIA tabs: arrows (and Home/End) move between the two tabs and select the one that gets focus.
  function onTabKey(e: KeyboardEvent<HTMLDivElement>) {
    let next: Mode | null = null
    if (e.key === 'ArrowRight' || e.key === 'ArrowLeft') next = register ? 'login' : 'register'
    else if (e.key === 'Home') next = 'login'
    else if (e.key === 'End') next = 'register'
    if (next === null) return
    e.preventDefault()
    switchMode(next)
    tabRefs[next].current?.focus()
  }

  async function submit(e: FormEvent) {
    e.preventDefault()
    if (busy) return
    setFormError(null)
    setUsernameError(null)
    setInfo(null)

    if (username === '' || password === '') {
      setTouched({ username: true, password: true })
      setFormError('Enter your username and password.')
      ;(username === '' ? usernameRef : passwordRef).current?.focus()
      return
    }
    if (register) {
      const up = usernameProblem(username)
      const pp = passwordProblem(password)
      if (up || pp) {
        setTouched({ username: true, password: true })
        ;(up ? usernameRef : passwordRef).current?.focus()
        return
      }
    }

    setBusy(true)
    try {
      if (register) {
        try {
          await api.register(username, password)
        } catch (err) {
          if (err instanceof ApiError && err.status === 409) {
            setUsernameError('That username is already taken.')
            usernameRef.current?.focus()
          } else {
            setFormError(messageOf(err))
          }
          return
        }
        try {
          const { user } = await api.login(username, password)
          onLoggedIn(user)
        } catch (err) {
          switchMode('login')
          setInfo('Your account was created, but signing in failed. Try logging in.')
          setFormError(messageOf(err))
        }
        return
      }
      const { user } = await api.login(username, password)
      onLoggedIn(user)
    } catch (err) {
      setFormError(messageOf(err))
      setPassword('')
      passwordRef.current?.focus()
    } finally {
      setBusy(false)
    }
  }

  return (
    <main className="auth">
      <div className="auth__card">
        <h1 className="auth__brand">Berth</h1>
        <p className="auth__tagline">Your Linux workstation in the browser.</p>

        {notice && (
          <p className="banner banner--warn" role="status">
            {notice}
          </p>
        )}
        {info && (
          <p className="banner banner--info" role="status">
            {info}
          </p>
        )}

        <div className="tabs" role="tablist" aria-label="Log in or register" onKeyDown={onTabKey}>
          <button
            type="button"
            role="tab"
            id={ids.tabLogin}
            ref={tabRefs.login}
            className="tabs__tab"
            aria-selected={!register}
            aria-controls="auth-panel"
            tabIndex={register ? -1 : 0}
            onClick={() => switchMode('login')}
          >
            Log in
          </button>
          <button
            type="button"
            role="tab"
            id={ids.tabRegister}
            ref={tabRefs.register}
            className="tabs__tab"
            aria-selected={register}
            aria-controls="auth-panel"
            tabIndex={register ? 0 : -1}
            onClick={() => switchMode('register')}
          >
            Register
          </button>
        </div>

        <div id="auth-panel" role="tabpanel" aria-labelledby={register ? ids.tabRegister : ids.tabLogin}>
          <form onSubmit={submit} noValidate>
            <div className="field">
              <label htmlFor={ids.u}>Username</label>
              <input
                id={ids.u}
                ref={usernameRef}
                name="username"
                type="text"
                value={username}
                onChange={(e) => {
                  setUsername(e.target.value)
                  setUsernameError(null)
                }}
                onBlur={() => setTouched((t) => ({ ...t, username: true }))}
                autoComplete="username"
                autoCapitalize="none"
                autoCorrect="off"
                spellCheck={false}
                maxLength={64}
                required
                aria-invalid={uMessage ? true : undefined}
                aria-describedby={register ? `${ids.uh}${uMessage ? ` ${ids.uh}-err` : ''}` : undefined}
                disabled={busy}
              />
              {register && (
                <p id={ids.uh} className="field__hint">
                  {USERNAME_HINT}
                </p>
              )}
              {uMessage && (
                <p id={`${ids.uh}-err`} className="field__error">
                  {uMessage}
                </p>
              )}
            </div>

            <div className="field">
              <label htmlFor={ids.p}>Password</label>
              <input
                id={ids.p}
                ref={passwordRef}
                name="password"
                type="password"
                value={password}
                onChange={(e) => setPassword(e.target.value)}
                onBlur={() => setTouched((t) => ({ ...t, password: true }))}
                autoComplete={register ? 'new-password' : 'current-password'}
                required
                aria-invalid={pProblem ? true : undefined}
                aria-describedby={register ? `${ids.ph}${pProblem ? ` ${ids.ph}-err` : ''}` : undefined}
                disabled={busy}
              />
              {register && (
                <p id={ids.ph} className="field__hint">
                  {PASSWORD_HINT}{' '}
                  <span
                    className={
                      passwordBytes(password) > PASSWORD_MAX_BYTES ? 'field__count field__count--over' : 'field__count'
                    }
                  >
                    {passwordBytes(password)}/{PASSWORD_MAX_BYTES} bytes
                  </span>
                </p>
              )}
              {pProblem && (
                <p id={`${ids.ph}-err`} className="field__error">
                  {pProblem}
                </p>
              )}
            </div>

            {formError && (
              <p className="banner banner--error" role="alert">
                {formError}
              </p>
            )}

            <button type="submit" className="btn btn--primary btn--block" disabled={busy}>
              {busy ? (register ? 'Creating account…' : 'Logging in…') : register ? 'Create account' : 'Log in'}
            </button>
          </form>
        </div>
      </div>
    </main>
  )
}

function messageOf(err: unknown): string {
  return err instanceof ApiError ? err.message : 'Something went wrong. Try again.'
}
