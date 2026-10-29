/**
 * Flitnix frontend utility and formatting functions.
 */

export function formatBytes(n: number): string {
  if (n < 1024) return `${n} B`
  const units = ['KiB', 'MiB', 'GiB', 'TiB']
  let value = n
  let i = -1
  do {
    value /= 1024
    i++
  } while (value >= 1024 && i < units.length - 1)
  return `${value.toFixed(value >= 100 ? 0 : 1)} ${units[i]}`
}

export function percent(done: number, total: number): number {
  if (total <= 0) return 100
  return Math.min(100, Math.max(0, Math.round((done / total) * 100)))
}

export function speed(value: number): string {
  return value > 0 ? `${formatBytes(value)}/s` : 'Calculating speed…'
}

export function formatEta(seconds: number): string {
  if (seconds <= 0 || !isFinite(seconds)) return '< 1s'
  if (seconds < 60) return `${seconds}s`
  const m = Math.floor(seconds / 60)
  const s = seconds % 60
  return `${m}m ${s < 10 ? '0' : ''}${s}s`
}

export function calcEta(done: number, total: number, speedBytesPerSec: number): string {
  if (speedBytesPerSec <= 0 || done <= 0 || done >= total || total <= 0) return ''
  const remaining = total - done
  const sec = Math.round(remaining / speedBytesPerSec)
  if (sec > 86400 * 7) return ''
  return formatEta(sec)
}

export function fmtTime(ts: unknown): string {
  if (typeof ts !== 'string') return ''
  return new Date(ts).toLocaleTimeString([], { hour: '2-digit', minute: '2-digit' })
}

export function fmtRemaining(expiresAt: unknown, nowMs: number = Date.now()): string {
  if (!expiresAt) return ''
  const exp = new Date(expiresAt as string).getTime()
  if (isNaN(exp)) return ''
  const diff = Math.max(0, Math.floor((exp - nowMs) / 1000))
  if (diff === 0) return 'Expired'
  const m = Math.floor(diff / 60)
  const s = diff % 60
  return `${m}m ${s < 10 ? '0' : ''}${s}s remaining`
}

/**
 * Parses an RFC 9530 Content-Digest header (e.g. sha-256=:BASE64:)
 * and returns the lowercase hex-encoded SHA-256 digest, or null if invalid.
 */
export function parseContentDigest(header: string | null | undefined): string | null {
  if (!header) return null
  const clean = header.trim()
  const lower = clean.toLowerCase()
  if (lower.includes('sha-256=:')) {
    const start = lower.indexOf('sha-256=:') + 'sha-256=:'.length
    const end = clean.indexOf(':', start)
    if (end === -1) return null
    const b64 = clean.slice(start, end).trim()
    try {
      const raw = atob(b64)
      if (raw.length !== 32) return null
      let hex = ''
      for (let i = 0; i < raw.length; i++) {
        hex += raw.charCodeAt(i).toString(16).padStart(2, '0')
      }
      return hex.toLowerCase()
    } catch {
      return null
    }
  }
  return null
}

export function verifyDigestMatch(actualHex: string | null | undefined, expectedHex: string | null | undefined): boolean {
  if (!actualHex || !expectedHex) return false
  return actualHex.trim().toLowerCase() === expectedHex.trim().toLowerCase()
}
