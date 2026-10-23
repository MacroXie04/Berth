import { act, fireEvent, render, screen } from '@testing-library/react'
import { describe, expect, it, vi } from 'vitest'
import { ApiError, type Api } from '../api'
import { LoginPage } from './LoginPage'

const ALICE = { id: 1, username: 'alice', is_admin: false }

function setup() {
  const api = {
    login: vi.fn(async () => ({ user: ALICE })),
    register: vi.fn(async () => ({ user: ALICE })),
  }
  const onLoggedIn = vi.fn()
  const utils = render(<LoginPage api={api as unknown as Api} onLoggedIn={onLoggedIn} />)
  return { ...utils, api, onLoggedIn }
}

const username = () => screen.getByLabelText(/username/i) as HTMLInputElement
const password = () => screen.getByLabelText(/password/i) as HTMLInputElement
const form = () => username().closest('form')!
const tab = (name: string) => screen.getByRole('tab', { name })
const type = (el: HTMLElement, value: string) => fireEvent.change(el, { target: { value } })

/** Fill the form and submit it; resolves once the request (and the state updates after it) are done. */
async function submit(u = 'alice', p = 'correct horse') {
  type(username(), u)
  type(password(), p)
  await act(async () => {
    fireEvent.submit(form())
  })
}

function deferred<T>() {
  let resolve!: (v: T) => void
  let reject!: (e: unknown) => void
  const promise = new Promise<T>((res, rej) => {
    resolve = res
    reject = rej
  })
  return { promise, resolve, reject }
}

describe('focus after a failed submit (WCAG 2.4.3: the user must not be dropped at the top of the page)', () => {
  it('a failed login puts focus on the password field, which is empty again', async () => {
    const { api } = setup()
    api.login.mockRejectedValue(new ApiError(401, 'invalid username or password'))
    username().focus()
    await submit('alice', 'wrong password')
    expect(screen.getByRole('alert')).toHaveTextContent('invalid username or password')
    expect(password().value).toBe('')
    expect(password()).toHaveFocus()
    expect(password().readOnly).toBe(false) // released again
  })

  it('"username taken" while registering puts focus on the username field', async () => {
    const { api } = setup()
    api.register.mockRejectedValue(new ApiError(409, 'username already taken'))
    fireEvent.click(tab('Register'))
    password().focus()
    await submit('alice', 'correct horse')
    expect(screen.getByText('That username is already taken.')).toBeInTheDocument()
    expect(username()).toHaveFocus()
    expect(api.login).not.toHaveBeenCalled()
  })

  it('while the request is under way the fields are read-only and aria-busy, not disabled; only the button is disabled', async () => {
    const { api } = setup()
    const pending = deferred<{ user: typeof ALICE }>()
    api.login.mockImplementation(() => pending.promise)
    password().focus()
    type(username(), 'alice')
    type(password(), 'correct horse')
    await act(async () => {
      fireEvent.submit(form())
    })
    for (const el of [username(), password()]) {
      expect(el).not.toBeDisabled()
      expect(el).toHaveAttribute('readonly')
      expect(el).toHaveAttribute('aria-busy', 'true')
    }
    expect(screen.getByRole('button', { name: /logging in/i })).toBeDisabled()
    expect(password()).toHaveFocus() // focus was not thrown away by the request

    // Enter in a read-only field submits the form again: that must not send a second request.
    await act(async () => {
      fireEvent.submit(form())
    })
    expect(api.login).toHaveBeenCalledTimes(1)

    await act(async () => pending.resolve({ user: ALICE }))
    expect(username()).not.toHaveAttribute('readonly')
    expect(username()).not.toHaveAttribute('aria-busy')
  })

  it('a successful login reports the user', async () => {
    const { api, onLoggedIn } = setup()
    await submit()
    expect(api.login).toHaveBeenCalledWith('alice', 'correct horse')
    expect(onLoggedIn).toHaveBeenCalledWith(ALICE)
  })
})

describe('the Log in / Register tablist (WAI-ARIA tabs)', () => {
  const selected = () => screen.getAllByRole('tab').filter((t) => t.getAttribute('aria-selected') === 'true').map((t) => t.textContent)

  it('starts on Log in, with only the selected tab in the tab order', () => {
    setup()
    expect(selected()).toEqual(['Log in'])
    expect(tab('Log in')).toHaveAttribute('tabindex', '0')
    expect(tab('Register')).toHaveAttribute('tabindex', '-1')
  })

  it('ArrowRight and ArrowLeft switch tab and move focus to it', () => {
    setup()
    tab('Log in').focus()
    fireEvent.keyDown(tab('Log in'), { key: 'ArrowRight' })
    expect(selected()).toEqual(['Register'])
    expect(tab('Register')).toHaveFocus()
    fireEvent.keyDown(tab('Register'), { key: 'ArrowLeft' })
    expect(selected()).toEqual(['Log in'])
    expect(tab('Log in')).toHaveFocus()
  })

  it('End goes to Register and Home to Log in, whichever tab has focus', () => {
    setup()
    tab('Log in').focus()
    fireEvent.keyDown(tab('Log in'), { key: 'End' })
    expect(selected()).toEqual(['Register'])
    expect(tab('Register')).toHaveFocus()
    fireEvent.keyDown(tab('Register'), { key: 'End' })
    expect(selected()).toEqual(['Register']) // already there
    fireEvent.keyDown(tab('Register'), { key: 'Home' })
    expect(selected()).toEqual(['Log in'])
    expect(tab('Log in')).toHaveFocus()
  })

  it('other keys do nothing, and the handled keys do not scroll the page', () => {
    setup()
    tab('Log in').focus()
    fireEvent.keyDown(tab('Log in'), { key: 'ArrowDown' })
    fireEvent.keyDown(tab('Log in'), { key: 'a' })
    expect(selected()).toEqual(['Log in'])
    expect(fireEvent.keyDown(tab('Log in'), { key: 'ArrowRight' })).toBe(false) // preventDefault was called
  })

  it('shows the registration rules only on the Register tab', () => {
    setup()
    expect(screen.queryByText(/3 to 32 characters/i)).toBeNull()
    fireEvent.keyDown(tab('Log in'), { key: 'ArrowRight' })
    expect(screen.getByText(/3 to 32 characters/i)).toBeInTheDocument()
  })
})

describe('switching tabs starts clean', () => {
  it('a failed-login banner does not follow the user to the Register tab', async () => {
    const { api } = setup()
    api.login.mockRejectedValue(new ApiError(401, 'invalid username or password'))
    await submit('alice', 'wrong password')
    expect(screen.getByRole('alert')).toBeInTheDocument()
    fireEvent.click(tab('Register'))
    expect(screen.queryByRole('alert')).toBeNull()
    expect(screen.queryByText('invalid username or password')).toBeNull()
  })

  it('"username taken" does not follow the user back to Log in', async () => {
    const { api } = setup()
    api.register.mockRejectedValue(new ApiError(409, 'username already taken'))
    fireEvent.click(tab('Register'))
    await submit('alice', 'correct horse')
    expect(screen.getByText('That username is already taken.')).toBeInTheDocument()
    fireEvent.click(tab('Log in'))
    expect(screen.queryByText('That username is already taken.')).toBeNull()
    expect(username()).not.toHaveAttribute('aria-invalid')
  })

  it('the "account created, sign-in failed" notice goes away with the next tab switch', async () => {
    const { api } = setup()
    api.login.mockRejectedValue(new ApiError(500, 'internal error'))
    fireEvent.click(tab('Register'))
    await submit('alice', 'correct horse')
    expect(screen.getByText(/your account was created/i)).toBeInTheDocument()
    expect(screen.getByRole('alert')).toHaveTextContent('internal error')
    fireEvent.click(tab('Register'))
    expect(screen.queryByText(/your account was created/i)).toBeNull()
    expect(screen.queryByRole('alert')).toBeNull()
  })

  it('keyboard tab switching clears them too', async () => {
    const { api } = setup()
    api.login.mockRejectedValue(new ApiError(401, 'invalid username or password'))
    await submit('alice', 'wrong password')
    fireEvent.keyDown(tab('Log in'), { key: 'ArrowRight' })
    expect(screen.queryByRole('alert')).toBeNull()
  })
})
