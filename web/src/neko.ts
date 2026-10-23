/**
 * The one place that knows how to address the neko web UI of a workspace.
 *
 * neko serves under NEKO_SERVER_PATH_PREFIX=/ws/<username>/browser and 404s
 * everywhere else. Its assets are relative, so the page must be loaded WITH the
 * trailing slash after "browser". The reverse proxy does not strip the prefix.
 *
 * `usr` and `pwd` are neko's auto-login query parameters (the member password
 * it was started with); neko removes them from its address after logging in.
 * `embed=1` hides neko's own header, side panel and bars and keeps the video
 * with its Fullscreen and Picture-in-Picture buttons. Keep the URL construction
 * here and nowhere else: this is the only place the neko password is ever
 * turned into text, and it ends up solely in the iframe's src.
 */
export function nekoUrl(username: string, nekoPassword: string | undefined): string {
  const base = `/ws/${encodeURIComponent(username)}/browser/`
  if (!nekoPassword) return base
  const q = new URLSearchParams({ usr: username, pwd: nekoPassword, embed: '1' })
  return `${base}?${q.toString()}`
}

/** The key neko's client keeps its session token under, on the page's origin. */
export const NEKO_SESSION_KEY = 'neko_session'

/**
 * Forget neko's stored session. neko shares the origin (and so localStorage)
 * with this page: a stored token is reused by the next load, which makes a second
 * tab take over the first tab's session and (measured) can leave a WebRTC UDP
 * socket bound on the workspace. Clearing it before the frame loads gives every
 * load its own session, and clearing it on logout leaves nothing behind.
 * localStorage can throw (blocked storage, private windows); that is fine.
 */
export function clearNekoSession(storage?: Pick<Storage, 'removeItem'>): void {
  try {
    ;(storage ?? window.localStorage).removeItem(NEKO_SESSION_KEY)
  } catch {
    /* nothing stored, nothing to clear */
  }
}
