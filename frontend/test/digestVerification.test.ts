import test from 'node:test'
import assert from 'node:assert/strict'
import { createHash } from 'node:crypto'
import { parseContentDigest, verifyDigestMatch } from '../src/utils.ts'

test('Client digest verification: exact match reports verified', () => {
  const content = Buffer.from('hello world verified content')
  const expectedHex = createHash('sha256').update(content).digest('hex')

  // Simulate incremental reader
  const hasher = createHash('sha256')
  const chunkSize = 8
  for (let offset = 0; offset < content.length; offset += chunkSize) {
    hasher.update(content.subarray(offset, offset + chunkSize))
  }
  const actualHex = hasher.digest('hex')

  assert.equal(verifyDigestMatch(actualHex, expectedHex), true)
})

test('Client digest verification: corrupted bytes report mismatch failure', () => {
  const original = Buffer.from('authentic data stream')
  const expectedHex = createHash('sha256').update(original).digest('hex')

  const tampered = Buffer.from('authentic data strXXm')
  const actualHex = createHash('sha256').update(tampered).digest('hex')

  assert.equal(verifyDigestMatch(actualHex, expectedHex), false)
})

test('Client digest verification: truncated stream reports mismatch and never claims verified', () => {
  const original = Buffer.from('full 100-byte stream that gets cut off in transit')
  const expectedHex = createHash('sha256').update(original).digest('hex')

  const truncated = original.subarray(0, 20)
  const actualHex = createHash('sha256').update(truncated).digest('hex')

  assert.equal(verifyDigestMatch(actualHex, expectedHex), false)
})

test('Client digest verification: empty file (0 bytes) is correctly verified', () => {
  const emptyExpectedHex = createHash('sha256').update(Buffer.alloc(0)).digest('hex')
  const actualHex = createHash('sha256').digest('hex')

  assert.equal(verifyDigestMatch(actualHex, emptyExpectedHex), true)
  assert.equal(emptyExpectedHex, 'e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855')
})

test('Client digest verification: missing or invalid digest handles gracefully without verified state', () => {
  const actualHex = createHash('sha256').update('data').digest('hex')

  assert.equal(verifyDigestMatch(actualHex, null), false)
  assert.equal(verifyDigestMatch(actualHex, ''), false)
  assert.equal(verifyDigestMatch(actualHex, 'not-a-hash'), false)
  assert.equal(parseContentDigest(''), null)
  assert.equal(parseContentDigest(undefined), null)
})

test('Download lifecycle state machine handles terminal failure before verification', () => {
  // Simulates client download states:
  type DownloadState = 'waiting' | 'downloading' | 'verifying' | 'verified' | 'failed' | 'interrupted' | 'unavailable'

  function transition(current: DownloadState, event: 'started' | 'chunk' | 'network_error' | 'eof' | 'verify_ok' | 'verify_fail'): DownloadState {
    switch (current) {
      case 'waiting':
        if (event === 'started') return 'downloading'
        if (event === 'network_error') return 'interrupted'
        return current
      case 'downloading':
        if (event === 'network_error') return 'interrupted'
        if (event === 'eof') return 'verifying'
        return current
      case 'verifying':
        if (event === 'verify_ok') return 'verified'
        if (event === 'verify_fail') return 'failed'
        return current
      default:
        return current
    }
  }

  // Happy path
  let state: DownloadState = 'waiting'
  state = transition(state, 'started')
  assert.equal(state, 'downloading')
  state = transition(state, 'eof')
  assert.equal(state, 'verifying')
  state = transition(state, 'verify_ok')
  assert.equal(state, 'verified')

  // Interrupted mid-stream
  let interruptedState: DownloadState = 'waiting'
  interruptedState = transition(interruptedState, 'started')
  interruptedState = transition(interruptedState, 'network_error')
  assert.equal(interruptedState, 'interrupted')
  // Cannot transition to verified from interrupted
  interruptedState = transition(interruptedState, 'verify_ok')
  assert.notEqual(interruptedState, 'verified')

  // Checksum mismatch
  let failedState: DownloadState = 'waiting'
  failedState = transition(failedState, 'started')
  failedState = transition(failedState, 'eof')
  failedState = transition(failedState, 'verify_fail')
  assert.equal(failedState, 'failed')
})
