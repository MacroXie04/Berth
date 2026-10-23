import { describe, expect, it, vi } from 'vitest'
import { clearNekoSession, NEKO_SESSION_KEY, nekoUrl } from './neko'

describe('nekoUrl', () => {
  it('points at the browser prefix WITH the trailing slash and carries usr/pwd/embed', () => {
    expect(nekoUrl('alice', 'secret')).toBe('/ws/alice/browser/?usr=alice&pwd=secret&embed=1')
  })

  it('percent-encodes the password so it cannot add parameters or break the URL', () => {
    const url = nekoUrl('alice', 'a&b=c d#e/é')
    expect(url.startsWith('/ws/alice/browser/?')).toBe(true)
    const q = new URL(url, 'http://x').searchParams
    expect(q.get('pwd')).toBe('a&b=c d#e/é')
    expect(Array.from(q.keys()).sort()).toEqual(['embed', 'pwd', 'usr'])
    expect(url).not.toContain('#')
  })

  it('has no query at all when there is no password', () => {
    expect(nekoUrl('alice', undefined)).toBe('/ws/alice/browser/')
    expect(nekoUrl('alice', '')).toBe('/ws/alice/browser/')
  })
})

describe('clearNekoSession', () => {
  it("removes neko's stored session token and nothing else", () => {
    const removeItem = vi.fn()
    clearNekoSession({ removeItem })
    expect(removeItem).toHaveBeenCalledTimes(1)
    expect(removeItem).toHaveBeenCalledWith(NEKO_SESSION_KEY)
    expect(NEKO_SESSION_KEY).toBe('neko_session')
  })

  it('works on the real localStorage', () => {
    window.localStorage.setItem(NEKO_SESSION_KEY, 'token')
    window.localStorage.setItem('berth.split', '0.4')
    clearNekoSession()
    expect(window.localStorage.getItem(NEKO_SESSION_KEY)).toBeNull()
    expect(window.localStorage.getItem('berth.split')).toBe('0.4')
  })

  it('survives storage that throws', () => {
    expect(() =>
      clearNekoSession({
        removeItem: () => {
          throw new DOMException('blocked', 'SecurityError')
        },
      }),
    ).not.toThrow()
  })
})
