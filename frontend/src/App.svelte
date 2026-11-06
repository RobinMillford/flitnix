<script lang="ts">
  import { onMount } from 'svelte'
  import { t, setLocale, tFn } from './i18n'
  import type { main, netenv, doctor, guest, audit, devices, power, version, update } from '../wailsjs/go/models'
  import { OnFileDrop, OnFileDropOff } from '../wailsjs/runtime/runtime.js'
  import logoUrl from './assets/images/logo-universal.png'
  import { createRefreshScheduler } from './refreshScheduler'
  import ConnectionDoctorModal from './components/ConnectionDoctorModal.svelte'
  import UpdateModal from './components/UpdateModal.svelte'
  import Toast from './components/Toast.svelte'
  import { formatBytes } from './utils'

  // Dedicated Views
  import PersonalTransferView from './views/PersonalTransferView.svelte'
  import GuestDropBoxView from './views/GuestDropBoxView.svelte'
  import TemplatesView from './views/TemplatesView.svelte'
  import TrustedDevicesView from './views/TrustedDevicesView.svelte'
  import TransferHistoryView from './views/TransferHistoryView.svelte'
  import AuditHistoryView from './views/AuditHistoryView.svelte'
  import SettingsView from './views/SettingsView.svelte'

  import {
    StartSession, EndSession, ListSessions, ListPendingUploads, RecentUploads,
    RecentDownloads, ApproveUpload, RejectUpload, CancelUpload,
    ListPendingBatches, ListBatches, ApproveBatch, RejectBatch, CancelBatch,
    PickFilesForSharing, PickFolderForSharing, AddDroppedSources,
    ListSharedFiles, ListSharedSources, RevokeSharedFile, RevokeSharedSource, ReceiveDirectory,
    ListNetworkInterfaces, SessionQR, ServerAddress,
    StartGuestRoomWithConfig, GetGuestRoomState, ApproveGuest, RejectGuest, RevokeGuest,
    ApproveGuestUpload, RejectGuestUpload, EndGuestRoom, PickGuestDestination, OpenGuestFolder,
    PickExportDestination, ExportGuestCollection,
    ListGuestPresets, ListGuestTemplates, SaveGuestTemplate, DeleteGuestTemplate, DuplicateGuestTemplate, StartGuestRoomFromTemplate,
    ListAuditEvents, ClearAuditHistory,
    RunConnectionDiagnostics, SendTextMessage, ListTextMessages, ClearTextMessages,
    ListTrustedDevices, RemoveTrustedDevice, SetDeviceAutoApprove,
    ApprovePairing, DenyPairing, GetPairingStatus, ReArmPairing,
    GetSettings, SaveSettings, ResetSettings, SelectDownloadDirectory, OpenDownloadFolder,
    SleepInhibition, GetAppVersion, CheckForUpdates
  } from '../wailsjs/go/main/App.js'

  const validTabs = ['personal', 'guest', 'templates', 'devices', 'history', 'audit', 'settings'] as const
  type TabName = typeof validTabs[number]

  function getInitialTab(): TabName {
    if (typeof window !== 'undefined') {
      const q = new URLSearchParams(window.location.search).get('tab') as TabName
      if (q && validTabs.includes(q)) return q
    }
    return 'personal'
  }

  function getInitialTheme(): string {
    if (typeof window !== 'undefined') {
      const t = new URLSearchParams(window.location.search).get('theme')
      if (t === 'dark' || t === 'light') return t
    }
    return 'system'
  }

  function applyTheme(theme: string): void {
    if (typeof document !== 'undefined') {
      if (theme === 'dark' || theme === 'light') {
        document.documentElement.setAttribute('data-theme', theme)
      } else {
        document.documentElement.removeAttribute('data-theme')
      }
    }
  }

  if (typeof document !== 'undefined') {
    applyTheme(getInitialTheme())
  }

  let activeTab: TabName = getInitialTab()
  let trustedDevices: devices.Device[] = []
  let loadingDevices = false
  let pairingStatus: devices.PairingStatusView | null = null
  let pairingActionLoading = false
  let rearmingLoading = false
  let sleepInhibition: power.Status | null = null

  // Persistent Settings state
  let settingsState: main.SettingsView = {
    version: 1,
    theme: getInitialTheme(),
    locale: 'system',
    downloadDir: '',
    effectiveDir: '',
    autoStartSession: false,
    preferredInterface: ''
  }
  let settingsSavedAlert = ''
  let settingsError = ''
  let savingSettings = false

  // Update & App Version state
  let appVersionInfo: version.Info | null = null
  let updateModalOpen = typeof window !== 'undefined' && new URLSearchParams(window.location.search).get('modal') === 'update'
  let updateAvailableBadge = false
  let updateCheckResult: update.CheckResult | null = null

  // Toast Notification state
  let toastMessage = ''
  let toastType: 'info' | 'success' | 'warning' | 'error' = 'info'

  function showToast(msg: string, type: 'info' | 'success' | 'warning' | 'error' = 'info'): void {
    toastMessage = msg
    toastType = type
  }

  function dismissToast(): void {
    toastMessage = ''
  }

  // Accessibility & live announcements
  let liveAnnouncement = ''
  let previousPendingTotal = -1

  const tabOrder: Array<'personal' | 'guest' | 'templates' | 'devices' | 'history' | 'audit' | 'settings'> = [
    'personal', 'guest', 'templates', 'devices', 'history', 'audit', 'settings'
  ]

  function switchTab(target: 'personal' | 'guest' | 'templates' | 'devices' | 'history' | 'audit' | 'settings'): void {
    activeTab = target
    if (target === 'templates') loadTemplates()
    else if (target === 'audit') loadAuditLogs()
    else if (target === 'devices') loadTrustedDevices()
    else if (target === 'settings') loadSettings()
  }

  function handleTabKeydown(e: KeyboardEvent): void {
    const currentIndex = tabOrder.indexOf(activeTab)
    if (currentIndex === -1) return
    let nextIndex = currentIndex

    if (e.key === 'ArrowRight') {
      e.preventDefault()
      nextIndex = (currentIndex + 1) % tabOrder.length
    } else if (e.key === 'ArrowLeft') {
      e.preventDefault()
      nextIndex = (currentIndex - 1 + tabOrder.length) % tabOrder.length
    } else if (e.key === 'Home') {
      e.preventDefault()
      nextIndex = 0
    } else if (e.key === 'End') {
      e.preventDefault()
      nextIndex = tabOrder.length - 1
    } else {
      // Allow browser vertical scrolling on ArrowUp / ArrowDown in horizontal tablist
      return
    }

    const nextTab = tabOrder[nextIndex]
    switchTab(nextTab)
    const btn = document.getElementById(`tab-${nextTab}`)
    btn?.focus()
  }

  // Personal transfer state
  let sessions: main.SessionView[] = []
  let pending: main.UploadView[] = []
  let pendingBatches: main.BatchView[] = []
  let batches: main.BatchView[] = []
  let uploads: main.UploadView[] = []
  let downloads: main.DownloadView[] = []
  let files: main.SharedFileView[] = []
  let sources: main.SharedSourceView[] = []
  let receiveDir = ''
  let errorText = ''
  let starting = false
  let picking = false
  let pickingFolder = false
  let busy: Record<string, boolean> = {}
  let interfaces: netenv.View[] = []
  let loadingIfaces = false
  let ifaceError = ''
  let selectedIp = ''
  let serverAddress = ''
  let qrUrl = ''
  let qrError = ''
  let qrKey = ''
  let activeInterfaceName = ''
  let activeAddress = ''
  let networkRefreshGeneration = 0
  let disposed = false
  const SESSION_PORT = '43821'

  // Quick Text state
  let textMessages: main.TextMessageView[] = []
  let textInput = ''
  let sendingText = false

  // Guest Drop Box state
  let guestRoomState: main.GuestRoomStateView | null = null
  let guestStarting = false
  let guestDestDir = ''
  let guestDurationMinutes = 60
  let guestCopiedLink = false
  let presets: guest.Preset[] = []
  let selectedPresetId = ''
  let guestTitle = 'Classroom Assignment'
  let guestDescription = 'Submit your assignment files. Provide your student ID and notes.'
  let guestCollisionPolicy: 'preserve_both' | 'reject' = 'preserve_both'
  let guestFormSchema: guest.FormField[] = []
  let exporting = false
  let exportSuccessMsg = ''

  // Templates state
  let templates: guest.Template[] = []
  let loadingTemplates = false

  // Audit Log state
  let auditEntries: audit.Entry[] = []
  let auditTotal = 0
  let auditLoading = false
  let auditFilterType = ''
  let auditFilterOutcome = ''

  // Connection Doctor state
  let doctorOpen = typeof window !== 'undefined' && new URLSearchParams(window.location.search).get('modal') === 'doctor'
  let doctorLoading = false
  let doctorReport: doctor.DiagnosticReport | null = null
  let doctorError = ''

  function stringOfError(err: unknown): string {
    if (err instanceof Error) return err.message
    if (typeof err === 'string') return err
    return String(err)
  }
  function setBusy(key: string, value: boolean): void {
    const next = { ...busy }
    if (value) next[key] = true
    else delete next[key]
    busy = next
  }
  function isBusy(key: string): boolean { return !!busy[key] }
  function report(err: unknown, key = '') {
    errorText = stringOfError(err)
    if (key) setBusy(key, false)
  }

  const refresh = createRefreshScheduler(
    () => Promise.all([
      ListSessions(), ListPendingUploads(), RecentUploads(), RecentDownloads(),
      ListSharedFiles(), ListSharedSources(), ReceiveDirectory(), ServerAddress(),
      ListPendingBatches(), ListBatches(), GetGuestRoomState(), GetPairingStatus(),
      SleepInhibition()
    ]),
    ([s, p, u, d, f, sourceList, dir, addr, b, allBatches, grState, pStatus, sleepStatus]) => {
      sessions = s ?? []; pending = p ?? []; uploads = u ?? []; downloads = d ?? []
      files = f ?? []; sources = sourceList ?? []; pendingBatches = b ?? []
      batches = allBatches ?? []
      receiveDir = dir; serverAddress = addr ?? ''
      guestRoomState = grState ?? null
      pairingStatus = pStatus ?? null
      sleepInhibition = sleepStatus ?? null
      if (!guestDestDir && dir) guestDestDir = dir

      const currentPendingTotal = (pending?.length ?? 0) + (pendingBatches?.length ?? 0) + (guestRoomState?.pendingUploads?.length ?? 0)
      if (previousPendingTotal >= 0 && currentPendingTotal > previousPendingTotal) {
        liveAnnouncement = tFn('announcements.newTransfer')
      }
      previousPendingTotal = currentPendingTotal

      if (!sessions.length) { qrKey = ''; qrUrl = ''; qrError = '' }
      else if (!sessions.some((x) => x.key === qrKey)) fetchQR(sessions[0].key)

      if (sessions.length > 0) {
        ListTextMessages(sessions[0].key).then(msgs => {
          textMessages = msgs ?? []
        }).catch(() => {})
      } else {
        textMessages = []
      }
    },
    (err) => { errorText = stringOfError(err) },
    () => disposed
  )

  async function fetchQR(key: string): Promise<void> {
    if (!key) return
    qrKey = key
    try {
      const data = await SessionQR(key)
      if (qrKey === key) { qrUrl = data; qrError = '' }
    } catch (err) {
      if (qrKey === key) { qrUrl = ''; qrError = stringOfError(err) }
    }
  }

  async function refreshNetwork(): Promise<void> {
    const generation = ++networkRefreshGeneration
    loadingIfaces = true
    ifaceError = ''
    try {
      const next = await ListNetworkInterfaces()
      if (generation !== networkRefreshGeneration) return
      interfaces = next ?? []
      if (next.length === 1) {
        selectedIp = next[0].ip
      } else if (settingsState.preferredInterface && next.some((item) => item.name === settingsState.preferredInterface)) {
        const prefMatch = next.find((item) => item.name === settingsState.preferredInterface && (item.family === 'IPv4' || !item.family)) ?? next.find((item) => item.name === settingsState.preferredInterface)
        if (prefMatch) {
          selectedIp = prefMatch.ip
        }
      } else if (!next.some((item) => item.ip === selectedIp)) {
        selectedIp = ''
      }
    } catch (err) {
      if (generation !== networkRefreshGeneration) return
      interfaces = []; selectedIp = ''; ifaceError = stringOfError(err)
    } finally {
      if (generation === networkRefreshGeneration) loadingIfaces = false
    }
  }

  async function startSession(): Promise<void> {
    if (starting) return
    if (interfaces.length > 1 && !selectedIp) {
      errorText = 'Choose the network interface that connects to the phone, then start.'
      return
    }
    starting = true
    try {
      await refreshNetwork()
      if (ifaceError) throw new Error(ifaceError)
      if (interfaces.length === 0) throw new Error('No network available. Connect to Wi-Fi or Ethernet to continue.')
      if (interfaces.length > 1 && !selectedIp) {
        throw new Error('Choose the network interface that connects to the phone, then start.')
      }
      const v = await StartSession(selectedIp)
      activeInterfaceName = v.interfaceName ?? ''; activeAddress = v.address ?? ''
      await fetchQR(v.key)
      liveAnnouncement = tFn('announcements.sessionStarted')
      refresh()
    } catch (err) {
      report(err)
    } finally {
      starting = false
    }
  }

  function endSession(key: string): void {
    const k = `end:${key}`; if (isBusy(k)) return
    setBusy(k, true)
    EndSession(key).then(() => {
      liveAnnouncement = tFn('announcements.sessionEnded')
      refresh()
    }).catch((e) => report(e, k)).finally(() => setBusy(k, false))
  }

  function decide(u: main.UploadView, approve: boolean): void {
    const k = `${approve ? 'approve' : 'reject'}:${u.id}`; if (isBusy(k)) return
    setBusy(k, true)
    const call = approve ? ApproveUpload(u.id) : RejectUpload(u.id)
    call.then(() => {
      liveAnnouncement = approve ? tFn('announcements.uploadApproved', { name: u.name || 'file' }) : tFn('announcements.uploadRejected', { name: u.name || 'file' })
      refresh()
    }).catch((e) => report(e, k)).finally(() => setBusy(k, false))
  }

  function decideBatch(batch: main.BatchView, approve: boolean): void {
    const k = `${approve ? 'approve' : 'reject'}-batch:${batch.id}`; if (isBusy(k)) return
    setBusy(k, true)
    const call = approve ? ApproveBatch(batch.id) : RejectBatch(batch.id)
    call.then(() => {
      liveAnnouncement = approve ? tFn('announcements.batchApproved', { name: batch.rootName || 'batch' }) : tFn('announcements.batchRejected', { name: batch.rootName || 'batch' })
      refresh()
    }).catch((e) => report(e, k)).finally(() => setBusy(k, false))
  }

  function cancelBatch(batch: main.BatchView): void {
    const k = `cancel-batch:${batch.id}`; if (isBusy(k)) return
    setBusy(k, true)
    CancelBatch(batch.id).then(() => {
      liveAnnouncement = tFn('announcements.batchCanceled', { name: batch.rootName || 'batch' })
      refresh()
    }).catch((e) => report(e, k)).finally(() => setBusy(k, false))
  }

  function cancelUpload(id: string): void {
    const k = `cancel:${id}`; if (isBusy(k)) return
    setBusy(k, true)
    CancelUpload(id).then(() => {
      liveAnnouncement = tFn('announcements.uploadCanceled')
      refresh()
    }).catch((e) => report(e, k)).finally(() => setBusy(k, false))
  }

  function pickFiles(): void {
    if (picking) return
    picking = true; PickFilesForSharing().then(refresh).catch(report).finally(() => { picking = false })
  }

  function pickFolder(): void {
    if (pickingFolder) return
    pickingFolder = true
    PickFolderForSharing().then(refresh).catch(report).finally(() => { pickingFolder = false })
  }

  function revokeSource(id: string): void {
    const k = `revoke-source:${id}`; if (isBusy(k)) return
    setBusy(k, true); RevokeSharedSource(id).then(refresh).catch((e) => report(e, k)).finally(() => setBusy(k, false))
  }

  function handleDrop(_x: number, _y: number, paths: string[]): void {
    if (!paths.length) return
    AddDroppedSources(paths).then(() => {
      liveAnnouncement = tFn('announcements.itemsAdded', { count: paths.length })
      refresh()
    }).catch(report)
  }

  function revoke(id: string): void {
    const k = `revoke:${id}`; if (isBusy(k)) return
    setBusy(k, true); RevokeSharedFile(id).then(refresh).catch((e) => report(e, k)).finally(() => setBusy(k, false))
  }

  async function clearAllShared(): Promise<void> {
    const revokes: Promise<any>[] = []
    for (const f of files) {
      revokes.push(RevokeSharedFile(f.id))
    }
    for (const s of sources) {
      revokes.push(RevokeSharedSource(s.id))
    }
    try {
      await Promise.all(revokes)
      showToast('Cleared all shared items', 'info')
      refresh()
    } catch (e) {
      report(e)
    }
  }

  // Quick Text actions
  async function sendTextAction(): Promise<void> {
    if (!textInput.trim() || sendingText || !sessions.length) return
    sendingText = true
    try {
      await SendTextMessage(sessions[0].key, textInput.trim())
      textInput = ''
      refresh()
    } catch (err) {
      report(err)
    } finally {
      sendingText = false
    }
  }

  async function clearTextAction(): Promise<void> {
    if (!sessions.length) return
    try {
      await ClearTextMessages(sessions[0].key)
      textMessages = []
      refresh()
    } catch (err) {
      report(err)
    }
  }

  function copyTextToClipboard(text: string): void {
    if (navigator.clipboard && navigator.clipboard.writeText) {
      navigator.clipboard.writeText(text)
    }
  }

  // Connection Doctor actions
  async function openDoctor(): Promise<void> {
    doctorOpen = true
    await runDoctor()
  }

  async function runDoctor(): Promise<void> {
    doctorLoading = true
    doctorError = ''
    try {
      doctorReport = await RunConnectionDiagnostics()
    } catch (err) {
      doctorError = stringOfError(err)
    } finally {
      doctorLoading = false
    }
  }

  async function regenerateQRAction(): Promise<void> {
    if (sessions.length > 0 && qrKey) {
      await fetchQR(qrKey)
    }
  }

  // Guest Drop Box actions
  async function chooseGuestDestination(): Promise<void> {
    try {
      const picked = await PickGuestDestination()
      if (picked) guestDestDir = picked
    } catch (err) {
      report(err)
    }
  }

  async function loadPresets(): Promise<void> {
    try {
      presets = await ListGuestPresets()
      if (presets && presets.length > 0 && !selectedPresetId) {
        applyPreset(presets[0])
      }
    } catch (err) {
      console.error('Failed to load presets:', err)
    }
  }

  function applyPreset(p: guest.Preset) {
    selectedPresetId = p.id
    guestTitle = p.title || p.name
    guestDescription = p.description || ''
    guestDurationMinutes = p.durationMinutes || 60
    guestCollisionPolicy = (p.collisionPolicy as 'preserve_both' | 'reject') || 'preserve_both'
    guestFormSchema = p.formSchema || []
  }

  async function startGuestRoomAction(): Promise<void> {
    if (guestStarting) return
    guestStarting = true
    try {
      await StartGuestRoomWithConfig({
        destinationDir: guestDestDir,
        durationMinutes: guestDurationMinutes,
        title: guestTitle,
        description: guestDescription,
        collisionPolicy: guestCollisionPolicy,
        formSchema: guestFormSchema,
        expectedParticipants: []
      } as any)
      refresh()
    } catch (err) {
      report(err)
    } finally {
      guestStarting = false
    }
  }

  async function exportCollectionAction(_roomId?: string): Promise<void> {
    if (exporting) return
    try {
      const targetPath = await PickExportDestination()
      if (!targetPath) return
      exporting = true
      exportSuccessMsg = ''
      const res = await ExportGuestCollection(targetPath)
      exportSuccessMsg = `Exported ${res.totalFiles} files (${formatBytes(res.totalBytes)})`
      setTimeout(() => { exportSuccessMsg = '' }, 5000)
    } catch (err) {
      report(err)
    } finally {
      exporting = false
    }
  }

  async function loadTemplates(): Promise<void> {
    loadingTemplates = true
    try {
      templates = await ListGuestTemplates()
    } catch (err) {
      report(err)
    } finally {
      loadingTemplates = false
    }
  }

  async function startFromTemplateAction(tplId: string): Promise<void> {
    try {
      await StartGuestRoomFromTemplate(tplId, guestDestDir)
      activeTab = 'guest'
      showToast('Started room from template', 'success')
      refresh()
    } catch (err) {
      report(err)
    }
  }

  async function duplicateTemplateAction(tplId: string): Promise<void> {
    const match = templates.find(t => t.id === tplId)
    const currentName = match?.name || 'Template'
    const newName = prompt('Enter a name for the duplicated template:', `${currentName} (Copy)`)
    if (!newName) return
    try {
      await DuplicateGuestTemplate(tplId, newName)
      showToast('Template duplicated', 'success')
      await loadTemplates()
    } catch (err) {
      report(err)
    }
  }

  async function deleteTemplateAction(tplId: string): Promise<void> {
    try {
      await DeleteGuestTemplate(tplId)
      showToast('Template deleted', 'info')
      await loadTemplates()
    } catch (err) {
      report(err)
    }
  }

  async function loadAuditLogs(): Promise<void> {
    auditLoading = true
    try {
      const filter: any = { limit: 100, offset: 0 }
      if (auditFilterType) filter.eventType = auditFilterType
      if (auditFilterOutcome) filter.outcome = auditFilterOutcome
      const res = await ListAuditEvents(filter)
      auditEntries = res.entries || []
      auditTotal = res.total || 0
    } catch (err) {
      report(err)
    } finally {
      auditLoading = false
    }
  }

  async function clearAuditAction(): Promise<void> {
    try {
      await ClearAuditHistory()
      showToast('Audit history cleared', 'info')
      await loadAuditLogs()
    } catch (err) {
      report(err)
    }
  }

  async function loadTrustedDevices(): Promise<void> {
    loadingDevices = true
    try {
      const [res, pStatus] = await Promise.all([
        ListTrustedDevices(),
        GetPairingStatus()
      ])
      trustedDevices = res ?? []
      pairingStatus = pStatus ?? null
    } catch (err) {
      report(err)
    } finally {
      loadingDevices = false
    }
  }

  async function handleApprovePairing(clientIP: string): Promise<void> {
    if (!clientIP || pairingActionLoading) return
    pairingActionLoading = true
    try {
      await ApprovePairing(clientIP)
      pairingStatus = await GetPairingStatus()
      if (activeTab === 'devices') {
        await loadTrustedDevices()
      }
      showToast('Pairing request approved', 'success')
    } catch (err) {
      report(err)
    } finally {
      pairingActionLoading = false
    }
  }

  async function handleDenyPairing(clientIP: string): Promise<void> {
    if (!clientIP || pairingActionLoading) return
    pairingActionLoading = true
    try {
      await DenyPairing(clientIP)
      pairingStatus = await GetPairingStatus()
      showToast('Pairing request denied', 'info')
    } catch (err) {
      report(err)
    } finally {
      pairingActionLoading = false
    }
  }

  async function handleReArmPairing(): Promise<void> {
    if (rearmingLoading) return
    rearmingLoading = true
    try {
      await ReArmPairing()
      pairingStatus = await GetPairingStatus()
      showToast('Pairing re-armed successfully', 'success')
    } catch (err) {
      report(err)
    } finally {
      rearmingLoading = false
    }
  }

  async function toggleDeviceAutoApprove(id: string, currentVal: boolean): Promise<void> {
    try {
      await SetDeviceAutoApprove(id, !currentVal)
      await loadTrustedDevices()
      showToast(currentVal ? 'Auto-approve disabled' : 'Auto-approve enabled', 'info')
    } catch (err) {
      report(err)
    }
  }

  async function removeDeviceAction(id: string): Promise<void> {
    try {
      await RemoveTrustedDevice(id)
      await loadTrustedDevices()
      showToast('Device removed from trusted list', 'info')
    } catch (err) {
      report(err)
    }
  }

  function pageTitle(tab: string): string {
    switch (tab) {
      case 'personal': return tFn('nav.personal')
      case 'guest': return tFn('nav.guest')
      case 'templates': return tFn('nav.templates')
      case 'devices': return tFn('nav.devices')
      case 'history': return tFn('nav.history')
      case 'audit': return tFn('nav.audit')
      case 'settings': return tFn('nav.settings')
      default: return tFn('app.title')
    }
  }

  function pageDescription(tab: string): string {
    switch (tab) {
      case 'personal': return tFn('header.personalDesc')
      case 'guest': return tFn('header.guestDesc')
      case 'templates': return tFn('header.templatesDesc')
      case 'devices': return tFn('header.devicesDesc')
      case 'history': return tFn('header.historyDesc')
      case 'audit': return tFn('header.auditDesc')
      case 'settings': return tFn('header.settingsDesc')
      default: return tFn('app.tagline')
    }
  }

  function toggleThemeQuick(): void {
    const current = document.documentElement.getAttribute('data-theme')
    const nextTheme = current === 'dark' ? 'light' : 'dark'
    settingsState.theme = nextTheme
    applyTheme(nextTheme)
    SaveSettings(settingsState).catch(() => {})
  }

  async function loadSettings(): Promise<void> {
    const urlTheme = typeof window !== 'undefined' ? new URLSearchParams(window.location.search).get('theme') : null
    try {
      const s = await GetSettings()
      settingsState = s
      applyTheme(urlTheme || s.theme)
      if (s.locale) setLocale(s.locale)
      if (s.preferredInterface && interfaces.length > 0 && !selectedIp) {
        const match = interfaces.find((item) => item.name === s.preferredInterface && (item.family === 'IPv4' || !item.family)) ?? interfaces.find((item) => item.name === s.preferredInterface)
        if (match) selectedIp = match.ip
      }
    } catch (err) {
      if (urlTheme) applyTheme(urlTheme)
      settingsError = stringOfError(err)
    }
  }

  async function handleSelectDownloadDir(): Promise<void> {
    try {
      const selected = await SelectDownloadDirectory()
      if (selected) {
        settingsState.downloadDir = selected
        settingsState.effectiveDir = selected
      }
    } catch (err) {
      settingsError = stringOfError(err)
    }
  }

  async function handleOpenDownloadFolder(): Promise<void> {
    try {
      await OpenDownloadFolder()
    } catch (err) {
      settingsError = stringOfError(err)
    }
  }

  async function handleSaveSettings(): Promise<void> {
    savingSettings = true
    settingsSavedAlert = ''
    settingsError = ''
    try {
      await SaveSettings(settingsState)
      applyTheme(settingsState.theme)
      if (settingsState.locale) setLocale(settingsState.locale)
      const reloaded = await GetSettings()
      settingsState = reloaded
      receiveDir = reloaded.effectiveDir
      settingsSavedAlert = 'Settings saved successfully.'
      setTimeout(() => { settingsSavedAlert = '' }, 3000)
    } catch (err) {
      settingsError = stringOfError(err)
    } finally {
      savingSettings = false
    }
  }

  async function handleResetSettings(): Promise<void> {
    settingsSavedAlert = ''
    settingsError = ''
    try {
      const defaults = await ResetSettings()
      settingsState = defaults
      applyTheme(defaults.theme)
      setLocale(defaults.locale || 'system')
      receiveDir = defaults.effectiveDir
      settingsSavedAlert = 'Settings restored to defaults.'
      setTimeout(() => { settingsSavedAlert = '' }, 3000)
    } catch (err) {
      settingsError = stringOfError(err)
    }
  }

  async function endGuestRoomAction(_id?: string): Promise<void> {
    const k = 'end-guest-room'
    if (isBusy(k)) return
    setBusy(k, true)
    try {
      await EndGuestRoom()
      refresh()
    } catch (err) {
      report(err, k)
    } finally {
      setBusy(k, false)
    }
  }

  async function approveGuestAction(id: string): Promise<void> {
    const k = `approve-guest:${id}`
    if (isBusy(k)) return
    setBusy(k, true)
    try {
      await ApproveGuest(id)
      liveAnnouncement = tFn('announcements.guestApproved')
      refresh()
    } catch (err) {
      report(err, k)
    } finally {
      setBusy(k, false)
    }
  }

  async function rejectGuestAction(id: string): Promise<void> {
    const k = `reject-guest:${id}`
    if (isBusy(k)) return
    setBusy(k, true)
    try {
      await RejectGuest(id)
      liveAnnouncement = tFn('announcements.guestRejected')
      refresh()
    } catch (err) {
      report(err, k)
    } finally {
      setBusy(k, false)
    }
  }

  async function revokeGuestAction(id: string): Promise<void> {
    const k = `revoke-guest:${id}`
    if (isBusy(k)) return
    setBusy(k, true)
    try {
      await RevokeGuest(id)
      liveAnnouncement = tFn('announcements.guestRevoked')
      refresh()
    } catch (err) {
      report(err, k)
    } finally {
      setBusy(k, false)
    }
  }

  async function approveGuestUploadAction(id: string): Promise<void> {
    const k = `approve-guest-upload:${id}`
    if (isBusy(k)) return
    setBusy(k, true)
    try {
      await ApproveGuestUpload(id)
      liveAnnouncement = tFn('announcements.guestUploadApproved')
      refresh()
    } catch (err) {
      report(err, k)
    } finally {
      setBusy(k, false)
    }
  }

  async function rejectGuestUploadAction(id: string): Promise<void> {
    const k = `reject-guest-upload:${id}`
    if (isBusy(k)) return
    setBusy(k, true)
    try {
      await RejectGuestUpload(id)
      liveAnnouncement = tFn('announcements.guestUploadRejected')
      refresh()
    } catch (err) {
      report(err, k)
    } finally {
      setBusy(k, false)
    }
  }

  async function openGuestFolderAction(): Promise<void> {
    try {
      await OpenGuestFolder()
    } catch (err) {
      report(err)
    }
  }

  function copyGuestLink(url: string | undefined): void {
    if (!url) return
    copyTextToClipboard(url)
    guestCopiedLink = true
    setTimeout(() => { guestCopiedLink = false }, 2000)
  }

  $: currentNetwork = interfaces.length === 1
    ? interfaces[0]
    : interfaces.find((item) => item.ip === selectedIp) ?? (interfaces[0] || null)

  $: guestPendingCount = (guestRoomState?.pendingGuests?.length ?? 0) + (guestRoomState?.pendingUploads?.length ?? 0)

  onMount(() => {
    loadSettings()
    refresh()
    refreshNetwork()
    loadPresets()
    loadTemplates()
    GetAppVersion().then((v) => {
      appVersionInfo = v
    }).catch(report)
    CheckForUpdates(false).then((res) => {
      if (res && res.updateAvailable) {
        updateAvailableBadge = true
        updateCheckResult = res
      }
    }).catch(() => {})
    const id = setInterval(refresh, 800)
    OnFileDrop(handleDrop, true)
    return () => {
      disposed = true
      clearInterval(id)
      OnFileDropOff()
    }
  })
</script>

<div class="app-shell">
  <!-- SIDEBAR -->
  <aside class="app-sidebar" aria-label={$t('nav.tabsLabel')}>
    <div class="sidebar-brand">
      <img class="brand-logo-img" src={logoUrl} alt="Flitnix" />
      <div class="brand-info">
        <h1>{$t('app.title')}</h1>
        <span class="brand-badge">LAN Transfer</span>
      </div>
    </div>

    <!-- Navigation Tab List -->
    <nav class="sidebar-nav-container" aria-label={$t('nav.tabsLabel')}>
      <div class="sidebar-nav" role="tablist" aria-label={$t('nav.tabsLabel')} aria-orientation="vertical" tabindex="-1" on:keydown={handleTabKeydown}>
        <!-- TRANSFER SECTION -->
        <div class="sidebar-section-title">{$t('sidebar.sectionTransfer')}</div>

        <button
          type="button"
          id="tab-personal"
          role="tab"
          aria-selected={activeTab === 'personal'}
          aria-controls="panel-personal"
          tabindex={activeTab === 'personal' ? 0 : -1}
          class="nav-tab-btn"
          class:active={activeTab === 'personal'}
          on:click={() => switchTab('personal')}
        >
          <span class="nav-tab-icon" aria-hidden="true">
            <svg width="18" height="18" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round">
              <polyline points="17 1 21 5 17 9"></polyline>
              <path d="M3 11V9a4 4 0 0 1 4-4h14"></path>
              <polyline points="7 23 3 19 7 15"></polyline>
              <path d="M21 13v2a4 4 0 0 1-4 4H3"></path>
            </svg>
          </span>
          <span class="nav-tab-label">{$t('nav.personal')}</span>
          {#if (pending.length + pendingBatches.length) > 0}
            <span class="tab-badge pulse-green">{pending.length + pendingBatches.length}</span>
          {:else if sessions.length > 0}
            <span class="tab-dot-online" title="Session active"></span>
          {/if}
        </button>

        <button
          type="button"
          id="tab-guest"
          role="tab"
          aria-selected={activeTab === 'guest'}
          aria-controls="panel-guest"
          tabindex={activeTab === 'guest' ? 0 : -1}
          class="nav-tab-btn"
          class:active={activeTab === 'guest'}
          on:click={() => switchTab('guest')}
        >
          <span class="nav-tab-icon" aria-hidden="true">
            <svg width="18" height="18" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round">
              <path d="M21 15v4a2 2 0 0 1-2 2H5a2 2 0 0 1-2-2v-4"></path>
              <polyline points="7 10 12 15 17 10"></polyline>
              <line x1="12" y1="15" x2="12" y2="3"></line>
            </svg>
          </span>
          <span class="nav-tab-label">{$t('nav.guest')}</span>
          {#if guestPendingCount > 0}
            <span class="tab-badge pulse-green">{guestPendingCount}</span>
          {:else if guestRoomState?.room && guestRoomState.room.state !== 'ended'}
            <span class="tab-badge warm">LIVE</span>
          {/if}
        </button>

        <button
          type="button"
          id="tab-templates"
          role="tab"
          aria-selected={activeTab === 'templates'}
          aria-controls="panel-templates"
          tabindex={activeTab === 'templates' ? 0 : -1}
          class="nav-tab-btn"
          class:active={activeTab === 'templates'}
          on:click={() => switchTab('templates')}
        >
          <span class="nav-tab-icon" aria-hidden="true">
            <svg width="18" height="18" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round">
              <rect x="3" y="3" width="18" height="18" rx="2" ry="2"></rect>
              <line x1="3" y1="9" x2="21" y2="9"></line>
              <line x1="9" y1="21" x2="9" y2="9"></line>
            </svg>
          </span>
          <span class="nav-tab-label">{$t('nav.templates')}</span>
          {#if templates.length > 0}
            <span class="tab-badge muted-count">{templates.length}</span>
          {/if}
        </button>

        <!-- MANAGEMENT SECTION -->
        <div class="sidebar-section-title">{$t('sidebar.sectionManagement')}</div>

        <button
          type="button"
          id="tab-devices"
          role="tab"
          aria-selected={activeTab === 'devices'}
          aria-controls="panel-devices"
          tabindex={activeTab === 'devices' ? 0 : -1}
          class="nav-tab-btn"
          class:active={activeTab === 'devices'}
          on:click={() => switchTab('devices')}
        >
          <span class="nav-tab-icon" aria-hidden="true">
            <svg width="18" height="18" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round">
              <rect x="2" y="3" width="20" height="14" rx="2" ry="2"></rect>
              <line x1="8" y1="21" x2="16" y2="21"></line>
              <line x1="12" y1="17" x2="12" y2="21"></line>
            </svg>
          </span>
          <span class="nav-tab-label">{$t('nav.devices')}</span>
          {#if pairingStatus?.requiresApproval}
            <span class="tab-badge alert-amber">1</span>
          {/if}
        </button>

        <button
          type="button"
          id="tab-history"
          role="tab"
          aria-selected={activeTab === 'history'}
          aria-controls="panel-history"
          tabindex={activeTab === 'history' ? 0 : -1}
          class="nav-tab-btn"
          class:active={activeTab === 'history'}
          on:click={() => switchTab('history')}
        >
          <span class="nav-tab-icon" aria-hidden="true">
            <svg width="18" height="18" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round">
              <circle cx="12" cy="12" r="10"></circle>
              <polyline points="12 6 12 12 16 14"></polyline>
            </svg>
          </span>
          <span class="nav-tab-label">{$t('nav.history')}</span>
        </button>

        <button
          type="button"
          id="tab-audit"
          role="tab"
          aria-selected={activeTab === 'audit'}
          aria-controls="panel-audit"
          tabindex={activeTab === 'audit' ? 0 : -1}
          class="nav-tab-btn"
          class:active={activeTab === 'audit'}
          on:click={() => switchTab('audit')}
        >
          <span class="nav-tab-icon" aria-hidden="true">
            <svg width="18" height="18" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round">
              <path d="M12 22s8-4 8-10V5l-8-3-8 3v7c0 6 8 10 8 10z"></path>
            </svg>
          </span>
          <span class="nav-tab-label">{$t('nav.audit')}</span>
        </button>

        <!-- SYSTEM SECTION -->
        <div class="sidebar-section-title">{$t('sidebar.sectionSystem')}</div>

        <button
          type="button"
          id="tab-settings"
          role="tab"
          aria-selected={activeTab === 'settings'}
          aria-controls="panel-settings"
          tabindex={activeTab === 'settings' ? 0 : -1}
          class="nav-tab-btn"
          class:active={activeTab === 'settings'}
          on:click={() => switchTab('settings')}
        >
          <span class="nav-tab-icon" aria-hidden="true">
            <svg width="18" height="18" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round">
              <circle cx="12" cy="12" r="3"></circle>
              <path d="M19.4 15a1.65 1.65 0 0 0 .33 1.82l.06.06a2 2 0 0 1 0 2.83 2 2 0 0 1-2.83 0l-.06-.06a1.65 1.65 0 0 0-1.82-.33 1.65 1.65 0 0 0-1 1.51V21a2 2 0 0 1-2 2 2 2 0 0 1-2-2v-.09A1.65 1.65 0 0 0 9 19.4a1.65 1.65 0 0 0-1.82.33l-.06.06a2 2 0 0 1-2.83 0 2 2 0 0 1 0-2.83l.06-.06a1.65 1.65 0 0 0 .33-1.82 1.65 1.65 0 0 0-1.51-1H3a2 2 0 0 1-2-2 2 2 0 0 1 2-2h.09A1.65 1.65 0 0 0 4.6 9a1.65 1.65 0 0 0-.33-1.82l-.06-.06a2 2 0 0 1 0-2.83 2 2 0 0 1 2.83 0l.06.06a1.65 1.65 0 0 0 1.82.33H9a1.65 1.65 0 0 0 1-1.51V3a2 2 0 0 1 2-2 2 2 0 0 1 2 2v.09a1.65 1.65 0 0 0 1 1.51 1.65 1.65 0 0 0 1.82-.33l.06-.06a2 2 0 0 1 2.83 0 2 2 0 0 1 0 2.83l-.06.06a1.65 1.65 0 0 0-.33 1.82V9a1.65 1.65 0 0 0 1.51 1H21a2 2 0 0 1 2 2 2 2 0 0 1-2 2h-.09a1.65 1.65 0 0 0-1.51 1z"></path>
            </svg>
          </span>
          <span class="nav-tab-label">{$t('nav.settings')}</span>
        </button>
      </div>
    </nav>

    <!-- SIDEBAR FOOTER -->
    <div class="sidebar-footer">
      <button
        type="button"
        class="doctor-nav-btn"
        on:click={openDoctor}
        aria-label={$t('nav.doctorLabel', { status: doctorReport?.overallStatus || 'ready' })}
      >
        <div style="display: flex; align-items: center; gap: 8px;">
          <span class="status-indicator-dot" class:warn={doctorReport?.overallStatus === 'WARNING'} class:fail={doctorReport?.overallStatus === 'FAIL'}></span>
          <span>{$t('nav.doctor')}</span>
        </div>
        <span class="status-tag {doctorReport?.overallStatus?.toLowerCase() || 'ready'}">
          {doctorReport?.overallStatus || 'Ready'}
        </span>
      </button>

      <div class="sidebar-app-version">
        <span>Flitnix {appVersionInfo ? `v${appVersionInfo.version}` : $t('sidebar.version')}</span>
        {#if updateAvailableBadge}
          <button
            type="button"
            class="update-pill-badge"
            on:click={() => { updateModalOpen = true }}
            title="Update available"
          >
            Update
          </button>
        {/if}
      </div>
    </div>
  </aside>

  <!-- MAIN VIEWPORT -->
  <div class="main-viewport">
    <header class="main-header">
      <div class="header-title-area">
        <h2>{pageTitle(activeTab)}</h2>
        <p>{pageDescription(activeTab)}</p>
      </div>

      <div class="header-actions">
        {#if currentNetwork}
          <div class="network-header-pill" title="{currentNetwork.friendlyName || currentNetwork.name} ({currentNetwork.ip})">
            <span class="pulse-dot"></span>
            <span>{currentNetwork.friendlyName || currentNetwork.type || 'Connected'}</span>
          </div>
        {/if}

        <div class="status-pill" class:online={sessions.length > 0}>
          <span class="pulse-dot"></span>
          <span>{sessions.length > 0 ? $t('app.sessionActive') : $t('app.sessionInactive')}</span>
        </div>

        <button
          type="button"
          class="theme-toggle-header-btn"
          on:click={toggleThemeQuick}
          title={settingsState.theme === 'dark' ? 'Switch to Light Mode' : 'Switch to Dark Mode'}
          aria-label="Toggle light/dark theme"
        >
          {#if settingsState.theme === 'dark'}
            <svg width="16" height="16" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round">
              <circle cx="12" cy="12" r="5"></circle>
              <line x1="12" y1="1" x2="12" y2="3"></line>
              <line x1="12" y1="21" x2="12" y2="23"></line>
              <line x1="4.22" y1="4.22" x2="5.64" y2="5.64"></line>
              <line x1="18.36" y1="18.36" x2="19.78" y2="19.78"></line>
              <line x1="1" y1="12" x2="3" y2="12"></line>
              <line x1="21" y1="12" x2="23" y2="12"></line>
              <line x1="4.22" y1="19.78" x2="5.64" y2="18.36"></line>
              <line x1="18.36" y1="5.64" x2="19.78" y2="4.22"></line>
            </svg>
          {:else}
            <svg width="16" height="16" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round">
              <path d="M21 12.79A9 9 0 1 1 11.21 3 7 7 0 0 0 21 12.79z"></path>
            </svg>
          {/if}
        </button>
      </div>

      <div class="sr-only" role="status" aria-live="polite" aria-atomic="true">
        {liveAnnouncement}
      </div>
    </header>

    <div class="main-content-scroll">
      <div class="main-content-inner content-limit-wrap">
        {#if errorText}
          <div class="alert danger" role="alert">
            <span>{errorText}</span>
            <button type="button" class="alert-dismiss" on:click={() => errorText = ''} aria-label={$t('app.dismiss')}>×</button>
          </div>
        {/if}

        {#if activeTab === 'personal'}
          <div id="panel-personal" role="tabpanel" aria-labelledby="tab-personal" tabindex="0">
            <PersonalTransferView
              {sessions}
              {pending}
              {pendingBatches}
              {batches}
              {uploads}
              {downloads}
              {files}
              {sources}
              {interfaces}
              {selectedIp}
              {serverAddress}
              {qrUrl}
              {qrError}
              {qrKey}
              {starting}
              {picking}
              {pickingFolder}
              {textMessages}
              bind:textInput
              {sendingText}
              {isBusy}
              {startSession}
              {endSession}
              {fetchQR}
              {pickFiles}
              {pickFolder}
              {revoke}
              {revokeSource}
              {clearAllShared}
              {decide}
              {decideBatch}
              {cancelUpload}
              {cancelBatch}
              {sendTextAction}
              {clearTextAction}
              {copyTextToClipboard}
              {showToast}
              {handleOpenDownloadFolder}
              goToHistory={() => switchTab('history')}
            />
          </div>
        {:else if activeTab === 'guest'}
          <div id="panel-guest" role="tabpanel" aria-labelledby="tab-guest" tabindex="0">
            <GuestDropBoxView
              {guestRoomState}
              {presets}
              bind:selectedPresetId
              bind:guestTitle
              bind:guestDescription
              bind:guestCollisionPolicy
              bind:guestFormSchema
              bind:guestDestDir
              bind:guestDurationMinutes
              {guestStarting}
              {guestCopiedLink}
              {exporting}
              {exportSuccessMsg}
              startRoom={startGuestRoomAction}
              endRoom={endGuestRoomAction}
              approveGuest={approveGuestAction}
              rejectGuest={rejectGuestAction}
              revokeGuest={revokeGuestAction}
              approveUpload={approveGuestUploadAction}
              rejectUpload={rejectGuestUploadAction}
              pickDestination={chooseGuestDestination}
              openFolder={openGuestFolderAction}
              exportCollection={exportCollectionAction}
              copyGuestLink={copyGuestLink}
              {copyTextToClipboard}
              {showToast}
            />
          </div>
        {:else if activeTab === 'templates'}
          <div id="panel-templates" role="tabpanel" aria-labelledby="tab-templates" tabindex="0">
            <TemplatesView
              {templates}
              loading={loadingTemplates}
              startFromTemplate={startFromTemplateAction}
              duplicateTemplate={duplicateTemplateAction}
              deleteTemplate={deleteTemplateAction}
              {showToast}
            />
          </div>
        {:else if activeTab === 'devices'}
          <div id="panel-devices" role="tabpanel" aria-labelledby="tab-devices" tabindex="0">
            <TrustedDevicesView
              {trustedDevices}
              {loadingDevices}
              {pairingStatus}
              {pairingActionLoading}
              {rearmingLoading}
              loadTrustedDevices={loadTrustedDevices}
              removeDeviceAction={removeDeviceAction}
              setDeviceAutoApproveAction={toggleDeviceAutoApprove}
              approvePairingAction={handleApprovePairing}
              denyPairingAction={handleDenyPairing}
              reArmPairingAction={handleReArmPairing}
              {showToast}
            />
          </div>
        {:else if activeTab === 'history'}
          <div id="panel-history" role="tabpanel" aria-labelledby="tab-history" tabindex="0">
            <TransferHistoryView
              {uploads}
              {downloads}
              {batches}
              {handleOpenDownloadFolder}
              {copyTextToClipboard}
              {showToast}
            />
          </div>
        {:else if activeTab === 'audit'}
          <div id="panel-audit" role="tabpanel" aria-labelledby="tab-audit" tabindex="0">
            <AuditHistoryView
              {auditEntries}
              totalEntries={auditTotal}
              loading={auditLoading}
              bind:filterType={auditFilterType}
              bind:filterOutcome={auditFilterOutcome}
              loadAuditLogs={loadAuditLogs}
              clearAuditLogsAction={clearAuditAction}
              {copyTextToClipboard}
              {showToast}
            />
          </div>
        {:else if activeTab === 'settings'}
          <div id="panel-settings" role="tabpanel" aria-labelledby="tab-settings" tabindex="0">
            <SettingsView
              bind:settingsState
              {interfaces}
              {appVersionInfo}
              {updateAvailableBadge}
              {savingSettings}
              {settingsSavedAlert}
              {settingsError}
              {sleepInhibition}
              {handleSelectDownloadDir}
              {handleOpenDownloadFolder}
              {handleSaveSettings}
              {handleResetSettings}
              {applyTheme}
              openUpdateModal={() => { updateModalOpen = true }}
            />
          </div>
        {/if}
      </div>
    </div>
  </div>

  <!-- Connection Doctor Modal -->
  <ConnectionDoctorModal
    isOpen={doctorOpen}
    report={doctorReport}
    onClose={() => { doctorOpen = false }}
    onRegenerateQR={regenerateQRAction}
  />

  <!-- Update Modal -->
  <UpdateModal
    isOpen={updateModalOpen}
    currentVersion={appVersionInfo?.version || '2.7.0'}
    onClose={() => { updateModalOpen = false }}
  />

  <!-- Global Toast Notification -->
  <div class="toast-fixed-container">
    <Toast message={toastMessage} type={toastType} onDismiss={dismissToast} />
  </div>
</div>
