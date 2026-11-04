/**
 * Transfer Awareness & Screen Wake Lock Controller
 *
 * Implements mobile browser background awareness, honest visibility transitions,
 * explicit opt-in Screen Wake Lock sentinel management, and server state reconciliation.
 */

export interface WakeLockSentinelLike {
  released: boolean
  release: () => Promise<void>
  addEventListener: (type: 'release', listener: () => void) => void
  removeEventListener: (type: 'release', listener: () => void) => void
}

export interface WakeLockNavigatorLike {
  wakeLock?: {
    request: (type: 'screen') => Promise<WakeLockSentinelLike>
  }
}

export type TransferType = 'upload' | 'download'

export interface TransferAwarenessState {
  supported: boolean
  userEnabled: boolean
  wakeLockActive: boolean
  transferCount: number
  uploadsCount: number
  downloadsCount: number
  isBackgrounded: boolean
  wasBackgroundedDuringTransfer: boolean
  bannerMessage: string | null
  badgeText: string
  badgeClass: string
  lastError: string | null
}

export interface DesktopSleepStatus {
  supported: boolean
  active: boolean
  holders: number
  reason?: string
  error?: string
}

export interface DesktopSleepNoticeModel {
  visible: boolean
  statusType: 'active' | 'warning' | 'none'
  badgeText: string
  holdersText: string
  description: string
}

export class TransferAwarenessController {
  private isSecureContext: boolean
  private nav: WakeLockNavigatorLike | undefined
  private userEnabled = false
  private sentinel: WakeLockSentinelLike | null = null
  private acquiring = false
  private activeTransfers = new Map<string, TransferType>()
  private backgrounded = false
  private backgroundedDuringTransfer = false
  private banner: string | null = null
  private lastError: string | null = null
  private onReconcile?: () => void
  private onChange?: (state: TransferAwarenessState) => void

  constructor(options?: {
    isSecureContext?: boolean
    navigator?: WakeLockNavigatorLike
    onReconcile?: () => void
    onChange?: (state: TransferAwarenessState) => void
  }) {
    this.isSecureContext = options?.isSecureContext ?? (typeof window !== 'undefined' ? window.isSecureContext : false)
    this.nav = options?.navigator ?? (typeof navigator !== 'undefined' ? navigator : undefined)
    this.onReconcile = options?.onReconcile
    this.onChange = options?.onChange
  }

  public isSupported(): boolean {
    return Boolean(this.isSecureContext && this.nav?.wakeLock && typeof this.nav.wakeLock.request === 'function')
  }

  public getTransferCount(): number {
    return this.activeTransfers.size
  }

  public hasActiveTransfers(): boolean {
    return this.activeTransfers.size > 0
  }

  public isWakeLockActive(): boolean {
    return this.sentinel !== null && !this.sentinel.released
  }

  public getState(): TransferAwarenessState {
    const supported = this.isSupported()
    const active = this.isWakeLockActive()
    const hasTransfers = this.hasActiveTransfers()

    let badgeText = 'Best-effort'
    let badgeClass = 'badge--waiting'

    if (!supported) {
      badgeText = 'Best-effort'
      badgeClass = 'badge--waiting'
    } else if (active) {
      badgeText = 'Screen wake active'
      badgeClass = 'badge--approved'
    } else if (this.userEnabled) {
      if (this.acquiring) {
        badgeText = 'Acquiring wake lock…'
        badgeClass = 'badge--uploading'
      } else if (hasTransfers) {
        badgeText = 'Screen wake standby'
        badgeClass = 'badge--waiting'
      } else {
        badgeText = 'Screen wake standby'
        badgeClass = 'badge--waiting'
      }
    } else {
      if (hasTransfers) {
        badgeText = 'Active transfer'
        badgeClass = 'badge--uploading'
      } else {
        badgeText = 'Idle'
        badgeClass = 'badge--waiting'
      }
    }

    let uploadsCount = 0
    let downloadsCount = 0
    for (const type of this.activeTransfers.values()) {
      if (type === 'upload') uploadsCount++
      else if (type === 'download') downloadsCount++
    }

    return {
      supported,
      userEnabled: this.userEnabled,
      wakeLockActive: active,
      transferCount: this.activeTransfers.size,
      uploadsCount,
      downloadsCount,
      isBackgrounded: this.backgrounded,
      wasBackgroundedDuringTransfer: this.backgroundedDuringTransfer,
      bannerMessage: this.banner,
      badgeText,
      badgeClass,
      lastError: this.lastError
    }
  }

  private notify(): void {
    if (this.onChange) {
      this.onChange(this.getState())
    }
  }

  public async setUserEnabled(enabled: boolean): Promise<void> {
    this.userEnabled = enabled
    this.lastError = null

    if (enabled) {
      if (this.hasActiveTransfers() && !this.backgrounded) {
        await this.acquireWakeLock()
      }
    } else {
      await this.releaseWakeLock()
    }
    this.notify()
  }

  public async addTransfer(id: string, type: TransferType): Promise<void> {
    this.activeTransfers.set(id, type)
    if (this.userEnabled && this.isSupported() && !this.backgrounded && !this.isWakeLockActive()) {
      await this.acquireWakeLock()
    }
    this.notify()
  }

  public async removeTransfer(id: string): Promise<void> {
    this.activeTransfers.delete(id)
    if (this.activeTransfers.size === 0) {
      await this.releaseWakeLock()
    }
    this.notify()
  }

  public async clearTransfers(): Promise<void> {
    this.activeTransfers.clear()
    await this.releaseWakeLock()
    this.notify()
  }

  public async acquireWakeLock(): Promise<boolean> {
    if (!this.isSupported() || !this.userEnabled || this.sentinel !== null || this.acquiring || this.backgrounded || !this.hasActiveTransfers()) {
      return false
    }

    this.acquiring = true
    this.lastError = null
    this.notify()

    try {
      const sentinel = await this.nav!.wakeLock!.request('screen')
      this.acquiring = false
      this.sentinel = sentinel

      const onRelease = () => {
        if (this.sentinel === sentinel) {
          this.sentinel = null
          this.notify()
        }
      }

      sentinel.addEventListener('release', onRelease)
      this.notify()
      return true
    } catch (err: any) {
      this.acquiring = false
      this.sentinel = null
      this.lastError = err?.message || 'Failed to acquire screen wake lock'
      this.notify()
      return false
    }
  }

  public async releaseWakeLock(): Promise<void> {
    if (this.sentinel !== null) {
      const s = this.sentinel
      this.sentinel = null
      try {
        await s.release()
      } catch {
        // Safe to ignore release errors
      }
    }
    this.notify()
  }

  public async onVisibilityChange(visibilityState: 'visible' | 'hidden'): Promise<void> {
    if (visibilityState === 'hidden') {
      this.backgrounded = true
      if (this.hasActiveTransfers()) {
        this.backgroundedDuringTransfer = true
      }
      if (this.sentinel !== null) {
        await this.releaseWakeLock()
      }
    } else if (visibilityState === 'visible') {
      this.backgrounded = false
      if (this.backgroundedDuringTransfer) {
        this.backgroundedDuringTransfer = false
        this.banner = 'Flitnix was moved to the background. Transfer status has been refreshed.'
        if (this.onReconcile) {
          try {
            this.onReconcile()
          } catch {
            // Reconcile callback error should not crash
          }
        }
      }

      if (this.userEnabled && this.hasActiveTransfers()) {
        await this.acquireWakeLock()
      }
    }
    this.notify()
  }

  public dismissBanner(): void {
    this.banner = null
    this.notify()
  }
}

/**
 * Computes desktop sleep inhibition presentation model.
 */
export function computeDesktopSleepNotice(
  status: DesktopSleepStatus | null | undefined,
  hasActiveTransfers: boolean,
  t: (key: string, params?: Record<string, any>) => string
): DesktopSleepNoticeModel {
  if (!hasActiveTransfers || !status) {
    return {
      visible: false,
      statusType: 'none',
      badgeText: '',
      holdersText: '',
      description: ''
    }
  }

  if (status.active) {
    return {
      visible: true,
      statusType: 'active',
      badgeText: `● ${t('telemetry.desktopSleepActive')}`,
      holdersText: status.holders > 0 ? t('telemetry.desktopSleepHolders', { count: status.holders }) : '',
      description: t('telemetry.desktopSleepActiveDesc')
    }
  }

  if (status.error) {
    return {
      visible: true,
      statusType: 'warning',
      badgeText: `▲ ${t('telemetry.desktopSleepWarning')}`,
      holdersText: '',
      description: t('telemetry.desktopSleepWarningDesc', { error: status.error })
    }
  }

  return {
    visible: false,
    statusType: 'none',
    badgeText: '',
    holdersText: '',
    description: ''
  }
}
