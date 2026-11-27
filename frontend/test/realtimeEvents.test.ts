import test from 'node:test'
import assert from 'node:assert/strict'
import { createDesktopEventCoalescer } from '../src/realtimeEvents.ts'

test('deduplicates monotonic positive event IDs while allowing control events without ID', () => {
  let refreshes = 0
  const seen: string[] = []
  const c = createDesktopEventCoalescer({
    onRefresh: () => { refreshes++ },
    onEvent: (ev) => { if (ev.type) seen.push(ev.type) }
  })

  c.handleEvent({ id: 10, type: 'batch.pending' })
  c.handleEvent({ id: 10, type: 'batch.pending' })
  c.handleEvent({ id: 9, type: 'upload.updated' })
  c.handleEvent({ type: 'resync' })
  c.handleEvent({ id: 11, type: 'batch.updated' })

  assert.equal(refreshes, 3)
  assert.deepEqual(seen, ['batch.pending', 'resync', 'batch.updated'])
  c.dispose()
})

test('throttles burst progress events and flushes discrete state changes immediately', () => {
  let nowMs = 1000
  let refreshes = 0
  const timers: Array<() => void> = []

  const c = createDesktopEventCoalescer({
    progressIntervalMs: 200,
    now: () => nowMs,
    setTimeoutFn: (fn) => {
      timers.push(fn)
      return timers.length as unknown as ReturnType<typeof setTimeout>
    },
    clearTimeoutFn: () => {
      timers.length = 0
    },
    onRefresh: () => { refreshes++ }
  })

  c.handleEvent({ id: 1, type: 'upload.progress' })
  assert.equal(refreshes, 1)

  nowMs = 1050
  c.handleEvent({ id: 2, type: 'upload.progress' })
  c.handleEvent({ id: 3, type: 'upload.progress' })
  assert.equal(refreshes, 1)
  assert.equal(timers.length, 1)

  // A discrete state event cancels pending trailing timer and refreshes right away.
  nowMs = 1080
  c.handleEvent({ id: 4, type: 'upload.updated' })
  assert.equal(refreshes, 2)
  assert.equal(timers.length, 0)

  c.dispose()
})
