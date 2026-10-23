import { describe, expect, it } from 'vitest'
import { formatBytes, formatCpu, formatElapsed } from './format'

describe('format', () => {
  it('formats bytes in binary units', () => {
    expect(formatBytes(0)).toBe('0 B')
    expect(formatBytes(1536)).toBe('1.5 KiB')
    expect(formatBytes(226959360)).toBe('216 MiB')
    expect(formatBytes(2147483648)).toBe('2.0 GiB')
    expect(formatBytes(-1)).toBe('-')
    expect(formatBytes(Number.NaN)).toBe('-')
  })
  it('formats CPU as a percentage of one core', () => {
    expect(formatCpu(0.4939)).toBe('0.5%')
    expect(formatCpu(12.4)).toBe('12%')
    expect(formatCpu(187.2)).toBe('187%')
    expect(formatCpu(Number.NaN)).toBe('-')
  })
  it('formats elapsed time', () => {
    expect(formatElapsed(0)).toBe('0 s')
    expect(formatElapsed(59.9)).toBe('59 s')
    expect(formatElapsed(61)).toBe('1 min 01 s')
  })
})
