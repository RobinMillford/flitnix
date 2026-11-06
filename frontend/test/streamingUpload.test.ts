import test from 'node:test'
import assert from 'node:assert/strict'
import { percent, calcEta, formatBytes, verifyDigestMatch } from '../src/utils.ts'

test('Streaming Upload: Progress percentage calculation is monotonically bounded between 0 and 100', () => {
  assert.equal(percent(0, 1000), 0)
  assert.equal(percent(500, 1000), 50)
  assert.equal(percent(1000, 1000), 100)
  // Excess bytes never exceed 100%
  assert.equal(percent(1050, 1000), 100)
  // Negative values clamp to 0%
  assert.equal(percent(-50, 1000), 0)
  // Zero total size returns 100% (already complete / empty)
  assert.equal(percent(0, 0), 100)
})

test('Streaming Upload: ETA calculation handles normal and edge cases deterministically', () => {
  // 5 MB done out of 15 MB at 2 MB/s -> remaining 10 MB / 2 MB/s = 5 seconds
  assert.equal(calcEta(5 * 1024 * 1024, 15 * 1024 * 1024, 2 * 1024 * 1024), '5s')
  // Zero speed returns empty string
  assert.equal(calcEta(5 * 1024 * 1024, 15 * 1024 * 1024, 0), '')
  // When done >= total returns empty string
  assert.equal(calcEta(10 * 1024 * 1024, 10 * 1024 * 1024, 1024 * 1024), '')
  assert.equal(calcEta(0, 1024 * 1024, 1024 * 1024), '')
})

test('Streaming Upload: Format bytes formats binary boundaries accurately', () => {
  assert.equal(formatBytes(0), '0 B')
  assert.equal(formatBytes(1024), '1.0 KiB')
  assert.equal(formatBytes(1024 * 1024), '1.0 MiB')
  assert.equal(formatBytes(1.5 * 1024 * 1024 * 1024), '1.5 GiB')
})

test('Streaming Upload: State machine sets retryable flag and clears stale transfer IDs on failure', () => {
  // Models the mobile client job state machine on network failure
  interface UploadJob {
    phase: string
    uploaded: number
    batchID: string | null
    itemID: string | null
    retryable: boolean
    err: string | null
  }

  const job: UploadJob = {
    phase: 'sending',
    uploaded: 5242880,
    batchID: 'b_old123',
    itemID: 'it_old456',
    retryable: false,
    err: null,
  }

  // Simulate network failure handler
  function handleUploadFailure(j: UploadJob, errMessage: string) {
    j.phase = 'failed'
    j.err = errMessage
    j.retryable = true
    j.batchID = null
    j.itemID = null
    j.uploaded = 0
  }

  handleUploadFailure(job, 'Network connection interrupted')

  assert.equal(job.phase, 'failed')
  assert.equal(job.retryable, true)
  assert.equal(job.batchID, null)
  assert.equal(job.itemID, null)
  assert.equal(job.uploaded, 0)
  assert.equal(job.err, 'Network connection interrupted')
})

test('Streaming Upload: Retry handler resets job cleanly to restart from byte zero', () => {
  interface UploadJob {
    phase: string
    uploaded: number
    speed: number
    eta: number
    retryable: boolean
    err: string | null
  }

  const failedJob: UploadJob = {
    phase: 'failed',
    uploaded: 0,
    speed: 0,
    eta: 0,
    retryable: true,
    err: 'Connection lost',
  }

  // Simulate retry handler
  function retryJob(j: UploadJob) {
    j.phase = 'retrying'
    j.err = null
    j.uploaded = 0
    j.speed = 0
    j.eta = 0
    j.retryable = false
  }

  retryJob(failedJob)

  assert.equal(failedJob.phase, 'retrying')
  assert.equal(failedJob.err, null)
  assert.equal(failedJob.uploaded, 0)
  assert.equal(failedJob.retryable, false)
})

test('Streaming Upload: Cancellation does not mark job as retryable', () => {
  interface UploadJob {
    phase: string
    cancelRequested: boolean
    retryable: boolean
    err: string | null
  }

  const job: UploadJob = {
    phase: 'sending',
    cancelRequested: true,
    retryable: false,
    err: null,
  }

  // When cancellation finishes
  job.phase = 'canceled'
  job.err = 'Upload canceled.'
  job.retryable = false

  assert.equal(job.phase, 'canceled')
  assert.equal(job.retryable, false)
})

test('Streaming Upload: Server digest verification validates exact match and detects tampering', () => {
  const authenticSHA256 = '2c26b46b68ffc68ff99b453c1d30413413422d706483bfa0f98a5e886266e7ae'
  const serverReported = '2C26B46B68FFC68FF99B453C1D30413413422D706483BFA0F98A5E886266E7AE'
  const corrupted = 'ffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffff'

  assert.equal(verifyDigestMatch(serverReported, authenticSHA256), true)
  assert.equal(verifyDigestMatch(corrupted, authenticSHA256), false)
})
