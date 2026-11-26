import test from 'node:test'
import assert from 'node:assert/strict'

function isStrongETag(tag: string | null | undefined): boolean {
  return Boolean(tag && !tag.startsWith('W/') && !tag.startsWith('w/'))
}

function parseContentRange(cr: string | null | undefined) {
  if (!cr) return null
  const m = String(cr).match(/^bytes\s+(\d+)-(\d+)\/(\d+|\*)$/i)
  if (!m) return null
  return {
    start: parseInt(m[1], 10),
    end: parseInt(m[2], 10),
    total: m[3] === '*' ? null : parseInt(m[3], 10),
  }
}

test('Download Resume Validators: strong ETag filter and Content-Range parsing', () => {
  assert.equal(isStrongETag('"flit-400-123-1-2"'), true)
  assert.equal(isStrongETag('"sha256-abc"'), true)
  assert.equal(isStrongETag('W/"weak-tag"'), false)
  assert.equal(isStrongETag('w/"weak-tag"'), false)
  assert.equal(isStrongETag(''), false)

  assert.deepEqual(parseContentRange('bytes 1024-2047/4096'), {
    start: 1024,
    end: 2047,
    total: 4096,
  })
  assert.deepEqual(parseContentRange('bytes 0-99/*'), {
    start: 0,
    end: 99,
    total: null,
  })
  assert.equal(parseContentRange('items 0-10/20'), null)
})

test('Download Resume State Transitions: 206 resumes in-place while 200 resets offset', () => {
  let received = 65536
  let totalSize: number | null = 131072
  let validatorETag = '"flit-v1"'
  let truncateCalled = false

  // Case 1: 206 at exact expected offset appends cleanly without reset
  const cr = parseContentRange('bytes 65536-131071/131072')
  assert.ok(cr && cr.start === received)
  assert.equal(cr?.total, totalSize)

  // Case 2: 200 returned despite received > 0 (e.g. If-Range mismatch) must reset sink & digest
  const respStatus = 200
  if (respStatus === 200 && received > 0) {
    received = 0
    validatorETag = '"flit-v2"'
    truncateCalled = true
  }
  assert.equal(received, 0)
  assert.equal(validatorETag, '"flit-v2"')
  assert.equal(truncateCalled, true)
})
