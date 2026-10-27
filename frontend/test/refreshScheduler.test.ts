import test from 'node:test'
import assert from 'node:assert/strict'
import { createRefreshScheduler } from '../src/refreshScheduler.ts'

interface Deferred<T> {
  promise: Promise<T>
  resolve: (value: T) => void
  reject: (reason?: unknown) => void
}

function deferred<T>(): Deferred<T> {
  let resolve!: (value: T) => void
  let reject!: (reason?: unknown) => void
  const promise = new Promise<T>((res, rej) => { resolve = res; reject = rej })
  return { promise, resolve, reject }
}

function flush(): Promise<void> {
  return new Promise<void>((resolve) => setImmediate(resolve))
}

async function until(condition: () => boolean, label: string, timeoutMs = 3000): Promise<void> {
  const deadline = Date.now() + timeoutMs
  while (!condition()) {
    if (Date.now() > deadline) assert.fail(`timed out waiting for ${label}`)
    await new Promise<void>((resolve) => setTimeout(resolve, 5))
  }
}

test('a round slower than the 800 ms poll still commits', async () => {
  let round = 0
  const commits: string[] = []
  const request = createRefreshScheduler(
    async () => {
      round++
      if (round === 1) await new Promise((resolve) => setTimeout(resolve, 850))
      return `snapshot ${round}`
    },
    (value) => commits.push(value),
    (error) => { throw error }
  )

  request()
  // A poll tick arrives while the first round is still in flight.
  await new Promise((resolve) => setTimeout(resolve, 100))
  request()

  await until(() => commits.length === 2, 'both rounds to commit')
  assert.deepEqual(commits, ['snapshot 1', 'snapshot 2'])
})

test('requests during an in-flight round coalesce into one follow-up without overlap', async () => {
  let active = 0
  let maxActive = 0
  let loads = 0
  const gates: Deferred<string>[] = []
  const commits: string[] = []
  const request = createRefreshScheduler(
    () => {
      loads++
      active++
      maxActive = Math.max(maxActive, active)
      const gate = deferred<string>()
      gates.push(gate)
      return gate.promise.finally(() => { active-- })
    },
    (value) => commits.push(value),
    (error) => { throw error }
  )

  request(); request(); request(); request()
  assert.equal(loads, 1, 'only one round may run at a time')
  assert.equal(active, 1)

  gates[0].resolve('first')
  await until(() => loads === 2, 'one follow-up round')

  // Further requests during the follow-up coalesce into one more round.
  request(); request(); request()
  assert.equal(loads, 2, 'requests during a round must not overlap it')

  gates[1].resolve('second')
  await until(() => loads === 3, 'second follow-up round')
  gates[2].resolve('third')
  await until(() => commits.length === 3, 'third commit')
  await flush()

  assert.deepEqual(commits, ['first', 'second', 'third'])
  assert.equal(loads, 3, 'each burst produced at most one extra round')
  assert.equal(maxActive, 1, 'rounds never overlapped')
})

test('a snapshot from an earlier round cannot commit after a newer round', async () => {
  let active = 0
  let maxActive = 0
  const gates: Deferred<string>[] = []
  const commits: string[] = []
  const request = createRefreshScheduler(
    () => {
      active++
      maxActive = Math.max(maxActive, active)
      const gate = deferred<string>()
      gates.push(gate)
      return gate.promise.finally(() => { active-- })
    },
    (value) => commits.push(value),
    (error) => { throw error }
  )

  request()
  gates[0].resolve('older snapshot')
  await until(() => commits.length === 1, 'older commit')

  request()
  await until(() => gates.length === 2, 'newer round to start')
  gates[1].resolve('newer snapshot')
  await until(() => commits.length === 2, 'newer commit')
  await flush()

  assert.deepEqual(commits, ['older snapshot', 'newer snapshot'])
  assert.equal(maxActive, 1, 'an older snapshot cannot land after a newer one')
})

test('an action-triggered refresh is not lost while a round is finishing', async () => {
  const gates: Deferred<string>[] = []
  const commits: string[] = []
  let request: () => void
  request = createRefreshScheduler(
    () => {
      const gate = deferred<string>()
      gates.push(gate)
      return gate.promise
    },
    (value) => {
      commits.push(value)
      // An approve/cancel promise can resolve between the load and the end of
      // the round; its .then(refresh) must still schedule a follow-up.
      if (value === 'before action') request()
    },
    (error) => { throw error }
  )

  request()
  gates[0].resolve('before action')
  await until(() => gates.length === 2, 'action follow-up round')
  gates[1].resolve('after action')
  await until(() => commits.length === 2, 'action follow-up commit')

  assert.deepEqual(commits, ['before action', 'after action'])
})

test('a failed round does not block queued or later refreshes', async () => {
  const gates: Deferred<string>[] = []
  const failures: unknown[] = []
  const commits: string[] = []
  const request = createRefreshScheduler(
    () => {
      const gate = deferred<string>()
      gates.push(gate)
      return gate.promise
    },
    (value) => commits.push(value),
    (error) => failures.push(error)
  )

  request()
  // An action arrives while the round that is about to fail is in flight.
  request()
  gates[0].reject(new Error('ipc down'))
  await until(() => gates.length === 2, 'queued round after failure')

  gates[1].resolve('recovered')
  await until(() => commits.length === 1, 'recovered commit')
  assert.equal(failures.length, 1)
  assert.equal(commits[0], 'recovered')

  // The scheduler remains usable after the failure and the follow-up.
  request()
  await until(() => gates.length === 3, 'later round')
  gates[2].resolve('later')
  await until(() => commits.length === 2, 'later commit')
  assert.equal(commits[1], 'later')
})

test('disposal drops pending follow-ups and later requests', async () => {
  let disposed = false
  let loads = 0
  const gates: Deferred<string>[] = []
  const commits: string[] = []
  const request = createRefreshScheduler(
    () => {
      loads++
      const gate = deferred<string>()
      gates.push(gate)
      return gate.promise
    },
    (value) => commits.push(value),
    (error) => { throw error },
    () => disposed
  )

  request()
  request() // queued follow-up
  disposed = true
  gates[0].resolve('late')
  await flush(); await flush()

  assert.equal(commits.length, 0)
  assert.equal(loads, 1)

  request()
  await flush()
  assert.equal(loads, 1, 'no refresh is scheduled after teardown')
})
