export interface DesktopRealtimeEvent {
  id?: number
  type?: string
  ts?: number
  scope?: string
  payload?: unknown
}

export interface DesktopEventCoalescerOptions {
  progressIntervalMs?: number
  onRefresh: () => void
  now?: () => number
  setTimeoutFn?: (fn: () => void, ms: number) => ReturnType<typeof setTimeout>
  clearTimeoutFn?: (id: ReturnType<typeof setTimeout>) => void
  onEvent?: (ev: DesktopRealtimeEvent) => void
}

export interface DesktopEventCoalescer {
  handleEvent: (ev: DesktopRealtimeEvent | null | undefined) => void
  dispose: () => void
}

function isProgressEvent(ev: DesktopRealtimeEvent): boolean {
  const t = ev.type ?? ''
  if (t.endsWith('.progress')) return true
  if (t === 'peer.transfer.updated' && ev.payload && typeof ev.payload === 'object') {
    const state = (ev.payload as { state?: unknown }).state
    return state === 'sending' || state === 'receiving'
  }
  return false
}

export function createDesktopEventCoalescer(opts: DesktopEventCoalescerOptions): DesktopEventCoalescer {
  const progressIntervalMs = Math.max(50, opts.progressIntervalMs ?? 200)
  const now = opts.now ?? (() => Date.now())
  const setTimer = opts.setTimeoutFn ?? ((fn, ms) => setTimeout(fn, ms))
  const clearTimer = opts.clearTimeoutFn ?? ((id) => clearTimeout(id))

  let lastSeenId = 0
  let lastProgressRefreshAt = -Infinity
  let progressTimer: ReturnType<typeof setTimeout> | null = null
  let disposed = false

  function clearProgressTimer(): void {
    if (progressTimer !== null) {
      clearTimer(progressTimer)
      progressTimer = null
    }
  }

  function flushRefresh(): void {
    if (disposed) return
    clearProgressTimer()
    lastProgressRefreshAt = now()
    opts.onRefresh()
  }

  function handleEvent(ev: DesktopRealtimeEvent | null | undefined): void {
    if (disposed || !ev || !ev.type) return

    if (typeof ev.id === 'number' && ev.id > 0) {
      if (ev.id <= lastSeenId) return
      lastSeenId = ev.id
    }

    if (opts.onEvent) {
      opts.onEvent(ev)
    }

    if (ev.type === 'resync') {
      flushRefresh()
      return
    }

    if (isProgressEvent(ev)) {
      const elapsed = now() - lastProgressRefreshAt
      if (elapsed >= progressIntervalMs) {
        flushRefresh()
      } else if (progressTimer === null) {
        progressTimer = setTimer(() => {
          progressTimer = null
          if (!disposed) {
            lastProgressRefreshAt = now()
            opts.onRefresh()
          }
        }, Math.max(10, progressIntervalMs - elapsed))
      }
      return
    }

    flushRefresh()
  }

  function dispose(): void {
    disposed = true
    clearProgressTimer()
  }

  return { handleEvent, dispose }
}
