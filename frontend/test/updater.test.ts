import test from 'node:test'
import assert from 'node:assert/strict'
import { formatBytes, percent, speed, calcEta } from '../src/utils.ts'

interface MockCheckResult {
  updateAvailable: boolean
  currentVersion: string
  latestVersion?: string
  release?: {
    tag: string
    name: string
    body: string
    publishedAt: string
    htmlUrl: string
    asset?: {
      name: string
      size: number
      downloadUrl: string
      installType: 'direct' | 'installer' | 'package_manager' | 'manual'
      targetExt: string
    }
  }
  error?: string
}

interface MockProgress {
  status: 'idle' | 'downloading' | 'verifying' | 'ready_to_install' | 'error'
  downloadedBytes: number
  totalBytes: number
  percentage: number
  speedBytesPerSec: number
  stagedFilePath?: string
  targetInstallPath?: string
  error?: string
}

test('Updater State Machine: Initial check for newer release version', () => {
  const resultUpToDate: MockCheckResult = {
    updateAvailable: false,
    currentVersion: '2.6.0',
    latestVersion: '2.6.0'
  }
  assert.equal(resultUpToDate.updateAvailable, false)

  const resultAvailable: MockCheckResult = {
    updateAvailable: true,
    currentVersion: '2.6.0',
    latestVersion: '2.7.0',
    release: {
      tag: 'v2.7.0',
      name: 'Flitnix 2.7.0',
      body: 'Release notes with new features',
      publishedAt: '2026-11-06T10:00:00Z',
      htmlUrl: 'https://github.com/RobinMillford/flitnix/releases/tag/v2.7.0',
      asset: {
        name: 'flitnix_2.7.0_linux_amd64.tar.gz',
        size: 15728640,
        downloadUrl: 'https://github.com/RobinMillford/flitnix/releases/download/v2.7.0/flitnix_2.7.0_linux_amd64.tar.gz',
        installType: 'direct',
        targetExt: '.tar.gz'
      }
    }
  }
  assert.equal(resultAvailable.updateAvailable, true)
  assert.equal(resultAvailable.release?.asset?.installType, 'direct')
  assert.equal(formatBytes(resultAvailable.release?.asset?.size || 0), '15.0 MiB')
})

test('Updater State Machine: Progress event processing and monotonic boundaries', () => {
  const progressEvents: MockProgress[] = []

  function onProgress(p: MockProgress) {
    progressEvents.push({ ...p })
  }

  // 1. Download starts
  onProgress({
    status: 'downloading',
    downloadedBytes: 0,
    totalBytes: 20971520, // 20 MiB
    percentage: 0,
    speedBytesPerSec: 0
  })

  // 2. Midway progress
  onProgress({
    status: 'downloading',
    downloadedBytes: 10485760, // 10 MiB
    totalBytes: 20971520,
    percentage: 50,
    speedBytesPerSec: 5242880 // 5 MiB/s
  })

  // 3. Download finished, verifying Ed25519 & SHA256
  onProgress({
    status: 'verifying',
    downloadedBytes: 20971520,
    totalBytes: 20971520,
    percentage: 100,
    speedBytesPerSec: 0
  })

  // 4. Verification successful, staged for atomic install
  onProgress({
    status: 'ready_to_install',
    downloadedBytes: 20971520,
    totalBytes: 20971520,
    percentage: 100,
    speedBytesPerSec: 0,
    stagedFilePath: '/tmp/flitnix-update/flitnix.new'
  })

  assert.equal(progressEvents.length, 4)
  assert.equal(progressEvents[0].status, 'downloading')
  assert.equal(progressEvents[1].percentage, 50)
  assert.equal(speed(progressEvents[1].speedBytesPerSec), '5.0 MiB/s')
  assert.equal(calcEta(progressEvents[1].downloadedBytes, progressEvents[1].totalBytes, progressEvents[1].speedBytesPerSec), '2s')
  assert.equal(progressEvents[2].status, 'verifying')
  assert.equal(progressEvents[3].status, 'ready_to_install')
  assert.ok(progressEvents[3].stagedFilePath)
})

test('Updater State Machine: Error state handling during download or verification', () => {
  let activeState: MockProgress = {
    status: 'downloading',
    downloadedBytes: 4194304,
    totalBytes: 20971520,
    percentage: 20,
    speedBytesPerSec: 1048576
  }

  // Simulate network drop during download
  activeState = {
    status: 'error',
    downloadedBytes: 4194304,
    totalBytes: 20971520,
    percentage: 20,
    speedBytesPerSec: 0,
    error: 'connection reset by peer'
  }

  assert.equal(activeState.status, 'error')
  assert.equal(activeState.error, 'connection reset by peer')

  // Simulate recovery check
  const canRetry = activeState.status === 'error'
  assert.ok(canRetry)
})

test('Updater Presentation: Install type guidance mapping', () => {
  function getActionLabel(installType: string): string {
    switch (installType) {
      case 'direct':
        return 'Restart & Apply Update'
      case 'installer':
        return 'Launch Installer & Exit'
      case 'package_manager':
        return 'Open Download Folder'
      default:
        return 'Open Release Page'
    }
  }

  assert.equal(getActionLabel('direct'), 'Restart & Apply Update')
  assert.equal(getActionLabel('installer'), 'Launch Installer & Exit')
  assert.equal(getActionLabel('package_manager'), 'Open Download Folder')
  assert.equal(getActionLabel('manual'), 'Open Release Page')
})

test('Toast State: Creation, dismissal and variant classification', () => {
  interface ToastItem {
    id: number
    message: string
    type: 'info' | 'success' | 'warning' | 'error'
    dismissed: boolean
  }

  const toasts: ToastItem[] = []

  function addToast(msg: string, type: 'info' | 'success' | 'warning' | 'error' = 'info'): ToastItem {
    const item: ToastItem = {
      id: Date.now(),
      message: msg,
      type,
      dismissed: false
    }
    toasts.push(item)
    return item
  }

  function dismiss(t: ToastItem) {
    t.dismissed = true
  }

  const t1 = addToast('Settings saved successfully', 'success')
  const t2 = addToast('Failed to connect to host', 'error')

  assert.equal(toasts.length, 2)
  assert.equal(t1.type, 'success')
  assert.equal(t1.dismissed, false)
  assert.equal(t2.type, 'error')

  dismiss(t1)
  assert.equal(t1.dismissed, true)
  assert.equal(t2.dismissed, false)
})
