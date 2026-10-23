/** Mirrors internal/auth: the server stays the authority, this only gives early hints. */

export const USERNAME_MIN = 3
export const USERNAME_MAX = 32
export const PASSWORD_MIN_BYTES = 8
export const PASSWORD_MAX_BYTES = 72

const USERNAME_RE = /^[a-z][a-z0-9]*(-[a-z0-9]+)*$/

export const USERNAME_HINT = '3 to 32 characters: lowercase letters, digits and single hyphens, starting with a letter.'
export const PASSWORD_HINT = '8 to 72 bytes. A long passphrase is better than a clever short one.'

export function usernameProblem(username: string): string | null {
  if (username.length < USERNAME_MIN || username.length > USERNAME_MAX) {
    return `Username must be ${USERNAME_MIN} to ${USERNAME_MAX} characters.`
  }
  if (!USERNAME_RE.test(username)) {
    return 'Username may only use lowercase letters, digits and single hyphens, and must start with a letter and not end with a hyphen.'
  }
  return null
}

/** bcrypt works on bytes, so the limit is on UTF-8 bytes, not characters. */
export function passwordBytes(password: string): number {
  return new TextEncoder().encode(password).length
}

export function passwordProblem(password: string): string | null {
  const n = passwordBytes(password)
  if (n < PASSWORD_MIN_BYTES) return `Password must be at least ${PASSWORD_MIN_BYTES} bytes.`
  if (n > PASSWORD_MAX_BYTES) {
    return `Password is ${n} bytes; the limit is ${PASSWORD_MAX_BYTES} bytes (non-ASCII characters take more than one).`
  }
  return null
}
