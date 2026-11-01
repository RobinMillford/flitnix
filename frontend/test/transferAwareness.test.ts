import test from 'node:test'
import assert from 'node:assert/strict'
import {
  TransferAwarenessController,
  computeDesktopSleepNotice,
  type WakeLockSentinelLike,
  type WakeLockNavigatorLike
} from '../src/transferAwareness.ts'

class MockWakeLockSentinel implements WakeLockSentinelLike {
  public released = false
  private listeners: Array<() => void> = []

  public async release(): Promise<void> {
    if (this.released) return
    this.released = true
    for (const listener of [...this.listeners]) {
      listener()
    }
  }

  public addEventListener(type: 'release', listener: () => void): void {
    if (type === 'release') {
      this.listeners.push(listener)
    }
  }

  public removeEventListener(type: 'release', listener: () => void): void {
    if (type === 'release') {
      this.listeners = this.listeners.filter((l) => l !== listener)
    }
  }
}

function createMockNavigator(shouldSucceed = true, rejectError = new Error('Permission denied')): {
  navigator: WakeLockNavigatorLike
  sentinels: MockWakeLockSentinel[]
  requestCount: () => number
} {
  let count = 0
  const sentinels: MockWakeLockSentinel[] = []

  const navigator: WakeLockNavigatorLike = {
    wakeLock: {
      request: async (type: 'screen') => {
        count++
        if (!shouldSucceed) {
          throw rejectError
        }
        const s = new MockWakeLockSentinel()
        sentinels.push(s)
        return s
      }
    }
  }

  return {
    navigator,
    sentinels,
    requestCount: () => count
  }
}

test('1. Wake Lock API unavailable in insecure context or without wakeLock property', async () => {
  // Case A: insecure context (e.g. LAN HTTP)
  const mockA = createMockNavigator()
  const controllerA = new TransferAwarenessController({
    isSecureContext: false,
    navigator: mockA.navigator
  })
  assert.equal(controllerA.isSupported(), false)
  await controllerA.setUserEnabled(true)
  await controllerA.addTransfer('up-1', 'upload')
  assert.equal(controllerA.isWakeLockActive(), false)
  assert.equal(mockA.requestCount(), 0)
  assert.equal(controllerA.getState().badgeText, 'Best-effort')

  // Case B: missing navigator.wakeLock
  const controllerB = new TransferAwarenessController({
    isSecureContext: true,
    navigator: {}
  })
  assert.equal(controllerB.isSupported(), false)
  assert.equal(controllerB.getState().badgeText, 'Best-effort')
})

test('2. Wake Lock request succeeds when enabled and transfers are active', async () => {
  const mock = createMockNavigator(true)
  const controller = new TransferAwarenessController({
    isSecureContext: true,
    navigator: mock.navigator
  })
  assert.equal(controller.isSupported(), true)

  // Enabling without active transfers does not acquire yet
  await controller.setUserEnabled(true)
  assert.equal(controller.isWakeLockActive(), false)
  assert.equal(mock.requestCount(), 0)
  assert.equal(controller.getState().badgeText, 'Screen wake standby')

  // Adding an active transfer triggers acquisition
  await controller.addTransfer('up-1', 'upload')
  assert.equal(controller.isWakeLockActive(), true)
  assert.equal(mock.requestCount(), 1)
  assert.equal(controller.getState().badgeText, 'Screen wake active')
})

test('3. Wake Lock request rejected handles error gracefully without throwing', async () => {
  const rejectError = new Error('Battery Saver Active')
  const mock = createMockNavigator(false, rejectError)
  const controller = new TransferAwarenessController({
    isSecureContext: true,
    navigator: mock.navigator
  })

  await controller.setUserEnabled(true)
  await controller.addTransfer('up-1', 'upload')

  assert.equal(controller.isWakeLockActive(), false)
  assert.equal(mock.requestCount(), 1)
  assert.equal(controller.getState().lastError, 'Battery Saver Active')
  assert.equal(controller.getState().badgeText, 'Screen wake standby')
})

test('4. Automatic wake lock release when all transfers complete', async () => {
  const mock = createMockNavigator(true)
  const controller = new TransferAwarenessController({
    isSecureContext: true,
    navigator: mock.navigator
  })

  await controller.setUserEnabled(true)
  await controller.addTransfer('up-1', 'upload')
  assert.equal(controller.isWakeLockActive(), true)
  assert.equal(mock.sentinels[0]?.released, false)

  // Remove the transfer (complete)
  await controller.removeTransfer('up-1')
  assert.equal(controller.isWakeLockActive(), false)
  assert.equal(mock.sentinels[0]?.released, true)
  assert.equal(controller.getState().badgeText, 'Screen wake standby')
})

test('5. Manual release when user disables toggle while transfers are active', async () => {
  const mock = createMockNavigator(true)
  const controller = new TransferAwarenessController({
    isSecureContext: true,
    navigator: mock.navigator
  })

  await controller.setUserEnabled(true)
  await controller.addTransfer('up-1', 'upload')
  assert.equal(controller.isWakeLockActive(), true)

  // User explicitly turns toggle off
  await controller.setUserEnabled(false)
  assert.equal(controller.isWakeLockActive(), false)
  assert.equal(mock.sentinels[0]?.released, true)
  assert.equal(controller.getState().badgeText, 'Active transfer')
})

test('6. Visibility changes: hides release wake lock and flags backgrounded state', async () => {
  const mock = createMockNavigator(true)
  const controller = new TransferAwarenessController({
    isSecureContext: true,
    navigator: mock.navigator
  })

  await controller.setUserEnabled(true)
  await controller.addTransfer('up-1', 'upload')
  assert.equal(controller.isWakeLockActive(), true)

  // Tab hidden (user switches apps or turns off screen)
  await controller.onVisibilityChange('hidden')
  assert.equal(controller.isWakeLockActive(), false)
  assert.equal(controller.getState().isBackgrounded, true)
  assert.equal(controller.getState().wasBackgroundedDuringTransfer, true)
})

test('7. Reacquisition after visibility restoration when user enabled wake lock', async () => {
  const mock = createMockNavigator(true)
  let reconciled = false
  const controller = new TransferAwarenessController({
    isSecureContext: true,
    navigator: mock.navigator,
    onReconcile: () => { reconciled = true }
  })

  await controller.setUserEnabled(true)
  await controller.addTransfer('up-1', 'upload')
  assert.equal(controller.isWakeLockActive(), true)
  assert.equal(mock.requestCount(), 1)

  // Move to background
  await controller.onVisibilityChange('hidden')
  assert.equal(controller.isWakeLockActive(), false)

  // Return to foreground
  await controller.onVisibilityChange('visible')
  assert.equal(controller.isWakeLockActive(), true)
  assert.equal(mock.requestCount(), 2)
  assert.equal(reconciled, true)
  assert.equal(
    controller.getState().bannerMessage,
    'Flitnix was moved to the background. Transfer status has been refreshed.'
  )
})

test('8. Multiple concurrent transfers keep wake lock active without duplicate requests', async () => {
  const mock = createMockNavigator(true)
  const controller = new TransferAwarenessController({
    isSecureContext: true,
    navigator: mock.navigator
  })

  await controller.setUserEnabled(true)
  await controller.addTransfer('up-1', 'upload')
  await controller.addTransfer('up-2', 'upload')
  await controller.addTransfer('dl-1', 'download')

  assert.equal(controller.getTransferCount(), 3)
  assert.equal(controller.getState().uploadsCount, 2)
  assert.equal(controller.getState().downloadsCount, 1)
  assert.equal(controller.isWakeLockActive(), true)
  // Only 1 sentinel requested, no redundant acquisitions
  assert.equal(mock.requestCount(), 1)
})

test('9. Completion of one transfer while another remains active retains wake lock', async () => {
  const mock = createMockNavigator(true)
  const controller = new TransferAwarenessController({
    isSecureContext: true,
    navigator: mock.navigator
  })

  await controller.setUserEnabled(true)
  await controller.addTransfer('up-1', 'upload')
  await controller.addTransfer('dl-1', 'download')
  assert.equal(controller.isWakeLockActive(), true)

  // Finish first transfer
  await controller.removeTransfer('up-1')
  // dl-1 is still active, so wake lock must stay held
  assert.equal(controller.isWakeLockActive(), true)
  assert.equal(mock.sentinels[0]?.released, false)

  // Finish second transfer
  await controller.removeTransfer('dl-1')
  // Now all transfers are done, sentinel is released
  assert.equal(controller.isWakeLockActive(), false)
  assert.equal(mock.sentinels[0]?.released, true)
})

test('10. Cancellation of all transfers clears sentinel promptly', async () => {
  const mock = createMockNavigator(true)
  const controller = new TransferAwarenessController({
    isSecureContext: true,
    navigator: mock.navigator
  })

  await controller.setUserEnabled(true)
  await controller.addTransfer('up-1', 'upload')
  await controller.addTransfer('up-2', 'upload')
  assert.equal(controller.isWakeLockActive(), true)

  await controller.clearTransfers()
  assert.equal(controller.getTransferCount(), 0)
  assert.equal(controller.isWakeLockActive(), false)
  assert.equal(mock.sentinels[0]?.released, true)
})

test('11. Server-side state reconciliation after returning to the page', async () => {
  let reconcileTriggerCount = 0
  const controller = new TransferAwarenessController({
    isSecureContext: true,
    onReconcile: () => {
      reconcileTriggerCount++
    }
  })

  // Case A: idle page backgrounded -> no reconciliation banner when returning
  await controller.onVisibilityChange('hidden')
  await controller.onVisibilityChange('visible')
  assert.equal(reconcileTriggerCount, 0)
  assert.equal(controller.getState().bannerMessage, null)

  // Case B: active transfer backgrounded -> triggers reconciliation banner upon return
  await controller.addTransfer('up-1', 'upload')
  await controller.onVisibilityChange('hidden')
  await controller.onVisibilityChange('visible')
  assert.equal(reconcileTriggerCount, 1)
  assert.equal(
    controller.getState().bannerMessage,
    'Flitnix was moved to the background. Transfer status has been refreshed.'
  )

  // Dismissing banner works
  controller.dismissBanner()
  assert.equal(controller.getState().bannerMessage, null)
})

test('12. Desktop inhibition status reporting logic', () => {
  const mockT = (key: string, params?: Record<string, any>) => {
    if (key === 'telemetry.desktopSleepActive') return 'Desktop sleep prevention active'
    if (key === 'telemetry.desktopSleepActiveDesc') return 'System sleep is inhibited while transfers are in progress.'
    if (key === 'telemetry.desktopSleepHolders') return `${params?.count} active transfers`
    if (key === 'telemetry.desktopSleepWarning') return 'Desktop sleep prevention warning'
    if (key === 'telemetry.desktopSleepWarningDesc') return `Inhibition error: ${params?.error}`
    return key
  }

  // No active transfers -> never visible
  const noticeIdle = computeDesktopSleepNotice(
    { supported: true, active: true, holders: 1 },
    false,
    mockT
  )
  assert.equal(noticeIdle.visible, false)
  assert.equal(noticeIdle.statusType, 'none')

  // Active transfers with active sleep inhibition
  const noticeActive = computeDesktopSleepNotice(
    { supported: true, active: true, holders: 2 },
    true,
    mockT
  )
  assert.equal(noticeActive.visible, true)
  assert.equal(noticeActive.statusType, 'active')
  assert.equal(noticeActive.badgeText, '● Desktop sleep prevention active')
  assert.equal(noticeActive.holdersText, '2 active transfers')
  assert.equal(noticeActive.description, 'System sleep is inhibited while transfers are in progress.')

  // Active transfers with inhibition error
  const noticeError = computeDesktopSleepNotice(
    { supported: true, active: false, holders: 0, error: 'DBus service unavailable' },
    true,
    mockT
  )
  assert.equal(noticeError.visible, true)
  assert.equal(noticeError.statusType, 'warning')
  assert.equal(noticeError.badgeText, '▲ Desktop sleep prevention warning')
  assert.equal(noticeError.description, 'Inhibition error: DBus service unavailable')
})

test('13. Upload and download flows operate independently for wake lock management', async () => {
  const mock = createMockNavigator(true)
  const controller = new TransferAwarenessController({
    isSecureContext: true,
    navigator: mock.navigator
  })

  await controller.setUserEnabled(true)

  // Flow A: Upload only
  await controller.addTransfer('batch-1', 'upload')
  assert.equal(controller.isWakeLockActive(), true)
  assert.equal(controller.getState().uploadsCount, 1)
  assert.equal(controller.getState().downloadsCount, 0)
  await controller.removeTransfer('batch-1')
  assert.equal(controller.isWakeLockActive(), false)

  // Flow B: Download only
  await controller.addTransfer('dl-zip', 'download')
  assert.equal(controller.isWakeLockActive(), true)
  assert.equal(controller.getState().uploadsCount, 0)
  assert.equal(controller.getState().downloadsCount, 1)
  await controller.removeTransfer('dl-zip')
  assert.equal(controller.isWakeLockActive(), false)
})
