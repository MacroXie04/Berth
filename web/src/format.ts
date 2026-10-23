const UNITS = ['B', 'KiB', 'MiB', 'GiB', 'TiB'] as const

/** 217 MiB, 2.0 GiB: binary units, one decimal below 10. */
export function formatBytes(bytes: number): string {
  if (!Number.isFinite(bytes) || bytes < 0) return '-'
  let v = bytes
  let i = 0
  while (v >= 1024 && i < UNITS.length - 1) {
    v /= 1024
    i++
  }
  const digits = i === 0 || v >= 10 ? 0 : 1
  return `${v.toFixed(digits)} ${UNITS[i]}`
}

/** CPU as a percentage of one core: 0.5%, 12%, 187%. */
export function formatCpu(percent: number): string {
  if (!Number.isFinite(percent) || percent < 0) return '-'
  return `${percent < 10 ? percent.toFixed(1) : Math.round(percent)}%`
}

export function formatElapsed(seconds: number): string {
  const s = Math.max(0, Math.floor(seconds))
  if (s < 60) return `${s} s`
  return `${Math.floor(s / 60)} min ${String(s % 60).padStart(2, '0')} s`
}
