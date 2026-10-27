/**
 * Serializes refresh rounds and coalesces requests made while one is running.
 *
 * The desktop asks for a refresh on an 800 ms timer and again after every
 * action (approve, cancel, revoke, ...). Letting those rounds overlap means a
 * slow round is superseded by each later tick and its reply is discarded, so
 * the UI can stop updating entirely under sustained IPC latency. Here at most
 * one round runs at a time. Any number of requests made during that round
 * become exactly one follow-up round, never an unbounded queue.
 *
 * Because rounds are serialized, a reply from an older round can never commit
 * after a newer one; there is no second round in flight to race with it.
 */
export function createRefreshScheduler<T>(
  load: () => Promise<T>,
  commit: (value: T) => void,
  fail: (error: unknown) => void,
  isDisposed: () => boolean = () => false
): () => void {
  let running = false
  let queued = false

  function request(): void {
    if (isDisposed()) return
    if (running) {
      queued = true
      return
    }
    void run()
  }

  async function run(): Promise<void> {
    running = true
    try {
      do {
        queued = false
        try {
          const value = await load()
          if (isDisposed()) return
          commit(value)
        } catch (error) {
          if (isDisposed()) return
          fail(error)
        }
      } while (queued && !isDisposed())
    } finally {
      running = false
    }
  }

  return request
}
