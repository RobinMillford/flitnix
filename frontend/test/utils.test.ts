import test from 'node:test'
import assert from 'node:assert/strict'
import {
  formatBytes, percent, speed, formatEta, calcEta, fmtTime, fmtRemaining
} from '../src/utils.ts'

test('formatBytes formats binary unit boundaries accurately', () => {
  assert.equal(formatBytes(0), '0 B')
  assert.equal(formatBytes(500), '500 B')
  assert.equal(formatBytes(1023), '1023 B')
  assert.equal(formatBytes(1024), '1.0 KiB')
  assert.equal(formatBytes(1536), '1.5 KiB')
  assert.equal(formatBytes(1024 * 1024), '1.0 MiB')
  assert.equal(formatBytes(500 * 1024 * 1024), '500 MiB')
  assert.equal(formatBytes(1024 * 1024 * 1024 * 2.5), '2.5 GiB')
})

test('percent calculates bounded progress percentages', () => {
  assert.equal(percent(0, 100), 0)
  assert.equal(percent(50, 100), 50)
  assert.equal(percent(100, 100), 100)
  assert.equal(percent(120, 100), 100)
  assert.equal(percent(-10, 100), 0)
  assert.equal(percent(0, 0), 100) // 0-byte transfer completes immediately
})

test('speed formats byte rates', () => {
  assert.equal(speed(0), 'Calculating speed…')
  assert.equal(speed(-5), 'Calculating speed…')
  assert.equal(speed(1024 * 1024 * 5), '5.0 MiB/s')
})

test('formatEta formats seconds into readable intervals', () => {
  assert.equal(formatEta(0), '< 1s')
  assert.equal(formatEta(-5), '< 1s')
  assert.equal(formatEta(15), '15s')
  assert.equal(formatEta(59), '59s')
  assert.equal(formatEta(60), '1m 00s')
  assert.equal(formatEta(75), '1m 15s')
  assert.equal(formatEta(125), '2m 05s')
})

test('calcEta calculates ETA from remaining bytes and speed', () => {
  assert.equal(calcEta(100, 100, 50), '') // already done
  assert.equal(calcEta(50, 100, 0), '') // speed 0
  assert.equal(calcEta(0, 1000, 100), '10s') // 1000 B at 100 B/s = 10s
  assert.equal(calcEta(200, 1000, 100), '8s') // 800 B at 100 B/s = 8s
})

test('fmtTime formats valid ISO timestamps and handles invalid gracefully', () => {
  assert.equal(fmtTime(null), '')
  assert.equal(fmtTime(12345), '')
  const formatted = fmtTime('2026-10-29T10:00:00Z')
  assert.ok(typeof formatted === 'string' && formatted.length > 0)
})

test('fmtRemaining computes remaining time countdown', () => {
  assert.equal(fmtRemaining(null), '')
  assert.equal(fmtRemaining('invalid-date'), '')

  const now = 1700000000000
  const future = new Date(now + 90 * 1000).toISOString()
  assert.equal(fmtRemaining(future, now), '1m 30s remaining')

  const past = new Date(now - 1000).toISOString()
  assert.equal(fmtRemaining(past, now), 'Expired')
})
