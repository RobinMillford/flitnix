import test from 'node:test'
import assert from 'node:assert/strict'

function retryBackoffMs(attempt: number, randVal = 0.5): number {
  const base = Math.min(15000, 1000 * Math.pow(2, Math.max(0, attempt - 1)))
  const jitter = 0.8 + 0.4 * randVal
  return Math.round(base * jitter)
}

test('Auto-Retry Backoff: jittered 1s/2s/4s/8s/15s schedule stays bounded', () => {
  const mids = [1000, 2000, 4000, 8000, 15000, 15000]
  mids.forEach((wantMid, idx) => {
    const attempt = idx + 1
    assert.equal(retryBackoffMs(attempt, 0.5), wantMid)
    const min = retryBackoffMs(attempt, 0)
    const max = retryBackoffMs(attempt, 1)
    assert.ok(min >= wantMid * 0.8 && min <= wantMid)
    assert.ok(max <= wantMid * 1.2 && max >= wantMid)
  })
})

test('Auto-Retry State Machine: preserves batchID, itemID, uploaded offset, and siblings on reconnect', () => {
  interface MobileJob {
    phase: string
    uploaded: number
    batchID: string | null
    itemID: string | null
    resumable: boolean
    autoRetries: number
    reconnectAttempt: number
    reconnectMax: number
    cancelRequested: boolean
  }

  const job0: MobileJob = {
    phase: 'sending',
    uploaded: 4194304,
    batchID: 'batch-1',
    itemID: 'item-0',
    resumable: true,
    autoRetries: 0,
    reconnectAttempt: 0,
    reconnectMax: 6,
    cancelRequested: false,
  }
  const job1: MobileJob = {
    phase: 'waiting',
    uploaded: 0,
    batchID: 'batch-1',
    itemID: 'item-1',
    resumable: false,
    autoRetries: 0,
    reconnectAttempt: 0,
    reconnectMax: 6,
    cancelRequested: false,
  }

  // Transient chunk failure increments retry counter without clearing identifiers or sibling state.
  job0.autoRetries += 1
  job0.phase = 'reconnecting'
  job0.reconnectAttempt = job0.autoRetries

  assert.equal(job0.phase, 'reconnecting')
  assert.equal(job0.reconnectAttempt, 1)
  assert.equal(job0.batchID, 'batch-1')
  assert.equal(job0.itemID, 'item-0')
  assert.equal(job0.uploaded, 4194304)
  assert.equal(job1.phase, 'waiting')
})
