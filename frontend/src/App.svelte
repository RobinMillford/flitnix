<script lang="ts">
  import { onMount } from 'svelte'
  import { focusTrap } from './focusTrap'
  import { t, setLocale, tFn } from './i18n'
  import type { main, netenv, doctor, guest, audit, devices } from '../wailsjs/go/models'
  import { OnFileDrop, OnFileDropOff } from '../wailsjs/runtime/runtime.js'
  import logoUrl from './assets/images/logo-universal.png'
  import { createRefreshScheduler } from './refreshScheduler'
  import {
    formatBytes, percent, speed, formatEta, calcEta, fmtTime, fmtRemaining
  } from './utils'
  import {
    StartSession, EndSession, ListSessions, ListPendingUploads, RecentUploads,
    RecentDownloads, ApproveUpload, RejectUpload, CancelUpload,
    ListPendingBatches, ListBatches, ApproveBatch, RejectBatch, CancelBatch,
    PickFilesForSharing, PickFolderForSharing, AddDroppedSources,
    ListSharedFiles, ListSharedSources, RevokeSharedFile, RevokeSharedSource, ReceiveDirectory,
    ListNetworkInterfaces, SessionQR, ServerAddress,
    StartGuestRoom, StartGuestRoomWithConfig, GetGuestRoomState, ApproveGuest, RejectGuest, RevokeGuest,
    ApproveGuestUpload, RejectGuestUpload, EndGuestRoom, PickGuestDestination, OpenGuestFolder,
    PickExportDestination, ExportGuestCollection,
    ListGuestPresets, ListGuestTemplates, SaveGuestTemplate, DeleteGuestTemplate, DuplicateGuestTemplate, StartGuestRoomFromTemplate,
    AddExpectedParticipant, RemoveExpectedParticipant, UpdateExpectedParticipant,
    ListAuditEvents, ClearAuditHistory,
    RunConnectionDiagnostics, SendTextMessage, ListTextMessages, ClearTextMessages,
    ListTrustedDevices, RemoveTrustedDevice, SetDeviceAutoApprove,
    GetSettings, SaveSettings, ResetSettings, SelectDownloadDirectory, OpenDownloadFolder
  } from '../wailsjs/go/main/App.js'

  let activeTab: 'personal' | 'guest' | 'templates' | 'audit' | 'devices' | 'settings' = 'personal'
  let trustedDevices: devices.Device[] = []
  let loadingDevices = false

  // Persistent Settings state
  let settingsState: main.SettingsView = {
    version: 1,
    theme: 'system',
    locale: 'system',
    downloadDir: '',
    effectiveDir: '',
    autoStartSession: false,
    preferredInterface: ''
  }
  let settingsSavedAlert = ''
  let settingsError = ''
  let savingSettings = false

  // Accessibility & live announcements
  let liveAnnouncement = ''
  let previousPendingTotal = -1

  const tabOrder: Array<'personal' | 'guest' | 'templates' | 'audit' | 'devices' | 'settings'> = [
    'personal', 'guest', 'templates', 'audit', 'devices', 'settings'
  ]

  function switchTab(target: 'personal' | 'guest' | 'templates' | 'audit' | 'devices' | 'settings'): void {
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
  let networkChooserOpen = false
  let networkRefreshGeneration = 0
  let disposed = false
  const SESSION_PORT = '43821'

  // Quick Text state
  let textMessages: main.TextMessageView[] = []
  let textInput = ''
  let sendingText = false

  // Guest Drop Box & Event Collection state
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
  let newParticipantName = ''
  let newParticipantIdentifier = ''
  let addingParticipant = false
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
  let doctorOpen = false
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
  function hostPort(): string {
    if (serverAddress) return serverAddress
    if (!activeAddress) return `…:${SESSION_PORT}`
    if (activeAddress.includes(':') && !activeAddress.startsWith('[')) {
      return `[${activeAddress}]:${SESSION_PORT}`
    }
    return `${activeAddress}:${SESSION_PORT}`
  }

  const refresh = createRefreshScheduler(
    () => Promise.all([
      ListSessions(), ListPendingUploads(), RecentUploads(), RecentDownloads(),
      ListSharedFiles(), ListSharedSources(), ReceiveDirectory(), ServerAddress(),
      ListPendingBatches(), ListBatches(), GetGuestRoomState()
    ]),
    ([s, p, u, d, f, sourceList, dir, addr, b, allBatches, grState]) => {
      sessions = s ?? []; pending = p ?? []; uploads = u ?? []; downloads = d ?? []
      files = f ?? []; sources = sourceList ?? []; pendingBatches = b ?? []
      batches = allBatches ?? []
      receiveDir = dir; serverAddress = addr ?? ''
      guestRoomState = grState ?? null
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
        networkChooserOpen = false
      } else if (settingsState.preferredInterface && next.some((item) => item.name === settingsState.preferredInterface)) {
        const prefMatch = next.find((item) => item.name === settingsState.preferredInterface && (item.family === 'IPv4' || !item.family)) ?? next.find((item) => item.name === settingsState.preferredInterface)
        if (prefMatch) {
          selectedIp = prefMatch.ip
        }
      } else if (!next.some((item) => item.ip === selectedIp)) {
        selectedIp = ''
        networkChooserOpen = false
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

  function activeUpload(u: main.UploadView): boolean {
    return ['pending', 'approved', 'uploading'].includes(u.state)
  }
  function activeDownload(d: main.DownloadView): boolean {
    return ['starting', 'transferring', 'finalizing'].includes(d.state)
  }
  function activeBatch(b: main.BatchView): boolean {
    return b.approvalState === 'approved' && ['queued', 'transferring'].includes(b.state)
  }
  function finishedBatch(b: main.BatchView): boolean {
    return ['completed', 'failed', 'canceled', 'rejected'].includes(b.state)
  }
  function batchName(b: main.BatchView): string {
    return b.rootName || `${b.totalItemCount} ${b.totalItemCount === 1 ? 'file' : 'files'}`
  }
  function doneItems(b: main.BatchView): number {
    return b.items.filter((item) => item.state === 'completed').length
  }
  function currentItem(b: main.BatchView): main.BatchItemView | undefined {
    return b.items.find((item) => item.state === 'transferring')
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

  async function addParticipantAction(): Promise<void> {
    if (!newParticipantName.trim() || addingParticipant) return
    addingParticipant = true
    try {
      await AddExpectedParticipant(newParticipantName.trim(), '', newParticipantIdentifier.trim())
      newParticipantName = ''
      newParticipantIdentifier = ''
      refresh()
    } catch (err) {
      report(err)
    } finally {
      addingParticipant = false
    }
  }

  async function removeParticipantAction(id: string): Promise<void> {
    try {
      await RemoveExpectedParticipant(id)
      refresh()
    } catch (err) {
      report(err)
    }
  }

  async function exportCollectionAction(): Promise<void> {
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
      refresh()
    } catch (err) {
      report(err)
    }
  }

  async function duplicateTemplateAction(tplId: string, currentName: string): Promise<void> {
    const newName = prompt('Enter a name for the duplicated template:', `${currentName} (Copy)`)
    if (!newName) return
    try {
      await DuplicateGuestTemplate(tplId, newName)
      await loadTemplates()
    } catch (err) {
      report(err)
    }
  }

  async function deleteTemplateAction(tplId: string): Promise<void> {
    if (!confirm('Are you sure you want to delete this template?')) return
    try {
      await DeleteGuestTemplate(tplId)
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
    if (!confirm('Are you sure you want to clear the local audit log history? This cannot be undone.')) return
    try {
      await ClearAuditHistory()
      await loadAuditLogs()
    } catch (err) {
      report(err)
    }
  }

  async function loadTrustedDevices(): Promise<void> {
    loadingDevices = true
    try {
      const res = await ListTrustedDevices()
      trustedDevices = res ?? []
    } catch (err) {
      report(err)
    } finally {
      loadingDevices = false
    }
  }

  async function toggleDeviceAutoApprove(id: string, currentVal: boolean): Promise<void> {
    try {
      await SetDeviceAutoApprove(id, !currentVal)
      await loadTrustedDevices()
    } catch (err) {
      report(err)
    }
  }

  async function removeDeviceAction(id: string): Promise<void> {
    try {
      await RemoveTrustedDevice(id)
      await loadTrustedDevices()
    } catch (err) {
      report(err)
    }
  }

  function applyTheme(theme: string): void {
    if (theme === 'dark' || theme === 'light') {
      document.documentElement.setAttribute('data-theme', theme)
    } else {
      document.documentElement.removeAttribute('data-theme')
    }
  }

  async function loadSettings(): Promise<void> {
    try {
      const s = await GetSettings()
      settingsState = s
      applyTheme(s.theme)
      if (s.locale) setLocale(s.locale)
      if (s.preferredInterface && interfaces.length > 0 && !selectedIp) {
        const match = interfaces.find((item) => item.name === s.preferredInterface && (item.family === 'IPv4' || !item.family)) ?? interfaces.find((item) => item.name === s.preferredInterface)
        if (match) selectedIp = match.ip
      }
    } catch (err) {
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

  async function endGuestRoomAction(): Promise<void> {
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
    : interfaces.find((item) => item.ip === selectedIp) ?? null

  $: guestPendingCount = (guestRoomState?.pendingGuests?.length ?? 0) + (guestRoomState?.pendingUploads?.length ?? 0)

  $: uniqueInterfaces = interfaces.filter((item, index, self) =>
    index === self.findIndex((t) => t.name === item.name)
  )

  onMount(() => {
    loadSettings()
    refresh()
    refreshNetwork()
    loadPresets()
    loadTemplates()
    const id = setInterval(refresh, 800)
    OnFileDrop(handleDrop, true)
    return () => {
      disposed = true
      clearInterval(id)
      OnFileDropOff()
    }
  })
</script>

<main class="shell">
  <header class="topbar">
    <div>
      <div class="brand"><img class="brand-logo" src={logoUrl} alt="Flitnix"/><h1>{$t('app.title')}</h1></div>
      <p>{$t('app.tagline')}</p>
    </div>
    <div class="topbar-actions" style="display: flex; align-items: center; gap: 12px;">
      <div class:online={sessions.length > 0} class="status">
        <span class="status-dot"></span>{sessions.length > 0 ? $t('app.sessionActive') : $t('app.sessionInactive')}
      </div>
    </div>
    <div class="sr-only" role="status" aria-live="polite" aria-atomic="true">
      {liveAnnouncement}
    </div>
  </header>

  <!-- Navigation bar with Tabs and Connection Doctor launcher -->
  <nav class="nav-bar" aria-label={$t('nav.tabsLabel')}>
    <div class="tab-list" role="tablist" aria-label={$t('nav.tabsLabel')} aria-orientation="horizontal" tabindex="-1" on:keydown={handleTabKeydown}>
      <button
        type="button"
        id="tab-personal"
        role="tab"
        aria-selected={activeTab === 'personal'}
        aria-controls="panel-personal"
        tabindex={activeTab === 'personal' ? 0 : -1}
        class="tab-btn"
        class:active={activeTab === 'personal'}
        on:click={() => switchTab('personal')}
      >
        <span>{$t('nav.personal')}</span>
        {#if pending.length + pendingBatches.length > 0}
          <span class="tab-badge">{pending.length + pendingBatches.length}</span>
        {/if}
      </button>

      <button
        type="button"
        id="tab-guest"
        role="tab"
        aria-selected={activeTab === 'guest'}
        aria-controls="panel-guest"
        tabindex={activeTab === 'guest' ? 0 : -1}
        class="tab-btn"
        class:active={activeTab === 'guest'}
        on:click={() => switchTab('guest')}
      >
        <span>{$t('nav.guest')}</span>
        {#if guestPendingCount > 0}
          <span class="tab-badge">{guestPendingCount}</span>
        {/if}
      </button>

      <button
        type="button"
        id="tab-templates"
        role="tab"
        aria-selected={activeTab === 'templates'}
        aria-controls="panel-templates"
        tabindex={activeTab === 'templates' ? 0 : -1}
        class="tab-btn"
        class:active={activeTab === 'templates'}
        on:click={() => switchTab('templates')}
      >
        <span>{$t('nav.templates')}</span>
      </button>

      <button
        type="button"
        id="tab-audit"
        role="tab"
        aria-selected={activeTab === 'audit'}
        aria-controls="panel-audit"
        tabindex={activeTab === 'audit' ? 0 : -1}
        class="tab-btn"
        class:active={activeTab === 'audit'}
        on:click={() => switchTab('audit')}
      >
        <span>{$t('nav.audit')}</span>
      </button>

      <button
        type="button"
        id="tab-devices"
        role="tab"
        aria-selected={activeTab === 'devices'}
        aria-controls="panel-devices"
        tabindex={activeTab === 'devices' ? 0 : -1}
        class="tab-btn"
        class:active={activeTab === 'devices'}
        on:click={() => switchTab('devices')}
      >
        <span>{$t('nav.devices')}</span>
        {#if trustedDevices.length > 0}
          <span class="tab-badge">{trustedDevices.length}</span>
        {/if}
      </button>

      <button
        type="button"
        id="tab-settings"
        role="tab"
        aria-selected={activeTab === 'settings'}
        aria-controls="panel-settings"
        tabindex={activeTab === 'settings' ? 0 : -1}
        class="tab-btn"
        class:active={activeTab === 'settings'}
        on:click={() => switchTab('settings')}
      >
        <span>{$t('nav.settings')}</span>
      </button>
    </div>

    <button
      type="button"
      class="doctor-launch-btn"
      on:click={openDoctor}
      aria-label={$t('nav.doctorLabel', { status: doctorReport?.overallStatus || 'ready' })}
    >
      <span class="doctor-dot" class:warn={doctorReport?.overallStatus === 'WARNING'} class:fail={doctorReport?.overallStatus === 'FAIL'}></span>
      <span>{$t('nav.doctor')}</span>
    </button>
  </nav>

  {#if errorText}<div class="alert" role="alert"><span>{errorText}</span><button on:click={() => errorText = ''} aria-label={$t('app.dismiss')}>×</button></div>{/if}

  <!-- Connection Doctor Modal -->
  {#if doctorOpen}
    <div
      class="modal-overlay"
      role="presentation"
      on:click|self={() => doctorOpen = false}
    >
      <div
        class="doctor-dialog"
        role="dialog"
        aria-labelledby="doctor-title"
        aria-modal="true"
        use:focusTrap={{ onEscape: () => doctorOpen = false }}
      >
        <div class="doctor-header">
          <div style="display: flex; align-items: center; gap: 10px;">
            <span class="doctor-dot" class:warn={doctorReport?.overallStatus === 'WARNING'} class:fail={doctorReport?.overallStatus === 'FAIL'}></span>
            <h2 id="doctor-title">{$t('doctor.title')}</h2>
          </div>
          <button class="button ghost small" on:click={() => doctorOpen = false} aria-label={$t('app.close')}>✕</button>
        </div>

        <div class="doctor-body">
          {#if doctorLoading}
            <div class="empty" style="text-align: center;">{$t('doctor.scanning')}</div>
          {:else if doctorError}
            <div class="alert"><span>{doctorError}</span></div>
          {:else if doctorReport}
            <div class="doctor-banner {doctorReport.overallStatus.toLowerCase()}">
              <strong>Network Diagnostic: {doctorReport.overallStatus}</strong>
              <p>{doctorReport.summary}</p>
            </div>

            {#each doctorReport.items as item (item.id)}
              <div class="doctor-check-item {item.status.toLowerCase()}">
                <div class="doctor-check-head">
                  <strong>{item.title}</strong>
                  <span class="badge {item.status.toLowerCase()}">{item.status}</span>
                </div>
                <p class="doctor-check-msg">{item.message}</p>
                {#if item.remediation}
                  <div class="doctor-remediation">
                    <strong>{$t('doctor.recommendedAction')}</strong> {item.remediation}
                  </div>
                {/if}
              </div>
            {/each}
          {/if}
        </div>

        <div class="doctor-footer">
          <div style="display: flex; gap: 8px;">
            <button class="button secondary small" on:click={runDoctor} disabled={doctorLoading}>
              {doctorLoading ? $t('doctor.checking') : $t('doctor.runDiagnostics')}
            </button>
            <button class="button ghost small" on:click={regenerateQRAction} disabled={!sessions.length && (!guestRoomState || !guestRoomState.room)}>
              {$t('doctor.regenerateQR')}
            </button>
          </div>
          <button class="button ghost small" on:click={() => doctorOpen = false}>{$t('app.close')}</button>
        </div>
      </div>
    </div>
  {/if}

  <!-- TAB 1: PERSONAL TRANSFER -->
  {#if activeTab === 'personal'}
    <div id="panel-personal" role="tabpanel" aria-labelledby="tab-personal" tabindex="0">
      <section class="connection panel">
      <div class="section-heading"><div><span class="eyebrow connection-eyebrow"><span class="network-glyph" aria-hidden="true">⌁</span> {$t('connection.eyebrow')}</span><h2>{$t('connection.readyToPair')}</h2></div><span class="pill">{sessions.length ? $t('app.sessionActive') : $t('connection.ready')}</span></div>
      <div class="connection-grid">
        <div>
          <span class="connection-label">{$t('connection.currentNetwork')}</span>
          {#if currentNetwork}
            <div class="network-identity">
              <span class="network-dot" aria-hidden="true"></span>
              <div><strong>{currentNetwork.friendlyName || `${currentNetwork.type || 'Network'} · Name unavailable`}</strong><span>{currentNetwork.type || 'Network'} · Connected{currentNetwork.friendlyNameSource === 'Connection profile' ? ' · Profile name' : ''}</span></div>
            </div>
            <button class="button ghost change-network" on:click={() => networkChooserOpen = !networkChooserOpen}>
              {networkChooserOpen ? $t('connection.hideNetworks') : $t('connection.changeNetwork')}
            </button>
          {:else if loadingIfaces}
            <div class="network-identity"><span class="network-dot unavailable"></span><div><strong>{$t('connection.scanning')}</strong><span>{$t('connection.scanningSub')}</span></div></div>
          {:else if interfaces.length > 1}
            <div class="network-identity"><span class="network-dot unavailable"></span><div><strong>{$t('connection.chooseNetwork')}</strong><span>{$t('connection.multipleConnections')}</span></div></div>
            <button class="button ghost change-network" on:click={() => networkChooserOpen = true}>{$t('connection.changeNetwork')}</button>
          {:else}
            <div class="network-identity"><span class="network-dot unavailable"></span><div><strong>{$t('connection.noNetwork')}</strong><span>{$t('connection.connectPrompt')}</span></div></div>
          {/if}
          {#if networkChooserOpen || interfaces.length > 1}
            <div class="network-picker">
              <label for="interface">{$t('connection.chooseNetwork')}</label>
              <select id="interface" bind:value={selectedIp} aria-label={$t('connection.chooseNetwork')}>
                <option value="" disabled>{$t('connection.selectNetworkPlaceholder')}</option>
                {#each interfaces as itf (itf.ip)}
                  <option value={itf.ip} disabled={itf.suitableForQR === false}>
                    {itf.friendlyName || `${itf.type || 'Network'} · Name unavailable`} · {itf.family || 'IPv4'} ({itf.ip})
                    {#if itf.suitableForQR === false} — {itf.warning || 'Not suitable for phone QR'}{/if}
                  </option>
                {/each}
              </select>
              <button class="button ghost" on:click={refreshNetwork} disabled={loadingIfaces}>{loadingIfaces ? $t('connection.refreshing') : $t('connection.rescan')}</button>
            </div>
          {/if}
          {#if currentNetwork}
            <details class="network-details">
              <summary>{$t('connection.details')}</summary>
              <dl>
                <div><dt>{$t('connection.interface')}</dt><dd>{currentNetwork.name || 'Unavailable'}</dd></div>
                <div><dt>{$t('connection.addressFamily')}</dt><dd>{currentNetwork.family || 'IPv4'}</dd></div>
                <div><dt>{$t('connection.ipAddress')}</dt><dd>{currentNetwork.ip || 'Unavailable'}</dd></div>
                <div><dt>{$t('connection.prefixCidr')}</dt><dd>{currentNetwork.cidr || 'Unavailable'}</dd></div>
                <div><dt>{$t('connection.connectionType')}</dt><dd>{currentNetwork.type || 'Network'}</dd></div>
                <div><dt>{$t('connection.profileName')}</dt><dd>{currentNetwork.connectionName || 'Unavailable'}</dd></div>
                <div><dt>{$t('connection.suitableForQR')}</dt><dd>{currentNetwork.suitableForQR !== false ? $t('connection.suitableYes') : $t('connection.suitableNo')}</dd></div>
                {#if currentNetwork.warning}<div><dt>{$t('connection.notice')}</dt><dd>{currentNetwork.warning}</dd></div>{/if}
                <div><dt>{$t('connection.nameSource')}</dt><dd>{currentNetwork.friendlyNameSource || 'Unavailable'}</dd></div>
                <div><dt>{$t('connection.metadata')}</dt><dd>{currentNetwork.metadataAvailable ? 'Available' : 'Name unavailable'}</dd></div>
              </dl>
            </details>
          {/if}
          {#if ifaceError}<p class="error-text">{ifaceError}</p>{/if}
        </div>
        {#if sessions.length && qrUrl}
          <div class="pairing">
            <img class="qr" src={qrUrl} alt="Pairing QR code" />
            <div><strong>{$t('connection.scanToConnect')}</strong><p class="muted">http://{hostPort()}</p><p class="muted">{$t('connection.incomingApprovalNotice')}</p>
              <div class="button-row"><button class="button ghost" on:click={() => fetchQR(qrKey)}>{$t('connection.regenerate')}</button><button class="button danger" on:click={() => endSession(qrKey)} disabled={isBusy(`end:${qrKey}`)}>{$t('connection.stopSession')}</button></div>
            </div>
          </div>
        {:else if sessions.length}<p class="muted">{qrError || $t('connection.preparingQR')}</p>{/if}
      </div>
      {#if !sessions.length}<div class="empty compact">{$t('connection.chooseInterfacePrompt')}</div>{/if}
      <div class="button-row"><button class="button primary" on:click={startSession} disabled={starting || (interfaces.length > 1 && !selectedIp)}>{starting ? $t('connection.starting') : $t('connection.startSession')}</button></div>
      <div class="notice">{$t('connection.plainHttpNotice')}</div>
    </section>

    <div class="workflow-grid">
      <section class="panel workflow drop-target">
        <div class="section-heading"><div><span class="eyebrow accent">{$t('workflow.eyebrow')}</span><h2>{$t('workflow.sharedFiles')}</h2></div><span class="count">{files.length}</span></div>
        <p class="muted">{$t('workflow.sharedFilesDesc')}</p>
        {#each files as f (f.id)}
          <div class="file-row"><div class="file-icon">↗</div><div class="grow"><strong>{f.name}</strong><span>{formatBytes(f.size)}</span></div><button class="button danger small" on:click={() => revoke(f.id)} disabled={isBusy(`revoke:${f.id}`)}>{$t('workflow.remove')}</button></div>
        {:else}<div class="empty">{$t('workflow.noSharedFiles')}</div>{/each}
        <div class="button-row"><button class="button secondary" on:click={pickFiles} disabled={picking || pickingFolder}>{picking ? $t('workflow.openingPicker') : $t('workflow.chooseFiles')}</button><button class="button ghost" on:click={pickFolder} disabled={picking || pickingFolder}>{pickingFolder ? $t('workflow.readingFolder') : $t('workflow.chooseFolder')}</button></div>
        <div class="drop-hint">{$t('workflow.dropHint')}</div>
        {#each sources.filter((source) => source.kind === 'folder') as source (source.id)}
          <div class="file-row source-row"><div class="file-icon">▣</div><div class="grow"><strong>{source.rootName || source.name}</strong><span>{$t('workflow.folderItemSummary', { count: source.itemCount, size: formatBytes(source.totalBytes) })}</span></div><button class="button danger small" on:click={() => revokeSource(source.id)} disabled={isBusy(`revoke-source:${source.id}`)}>{$t('workflow.remove')}</button></div>
        {/each}
      </section>

      <section class="panel workflow">
        <div class="section-heading"><div><span class="eyebrow blue">{$t('workflow.receiveEyebrow')}</span><h2>{$t('workflow.inboxTitle')}</h2></div><span class="count">{pending.length + pendingBatches.length}</span></div>
        <p class="muted">{$t('workflow.inboxDesc')}</p>
        {#each pending as u (u.id)}
          <div class="request-row"><div class="file-icon">↙</div><div class="grow"><strong>{u.name}</strong><span>{formatBytes(u.size)} · {$t('workflow.waitingApproval')}</span></div><div class="button-row"><button class="button primary small" on:click={() => decide(u, true)} disabled={isBusy(`approve:${u.id}`)}>{$t('workflow.allow')}</button><button class="button danger small" on:click={() => decide(u, false)} disabled={isBusy(`reject:${u.id}`)}>{$t('workflow.deny')}</button></div></div>
        {/each}
        {#each pendingBatches as b (b.id)}
          <div class="request-row"><div class="file-icon">↙</div><div class="grow"><strong>Batch · {batchName(b)}</strong><span>{formatBytes(b.declaredTotalBytes)} · {$t('workflow.waitingApproval')}</span><small>{b.items.map((item) => item.relativePath || item.displayName).join(', ')}</small></div><div class="button-row"><button class="button primary small" on:click={() => decideBatch(b, true)} disabled={isBusy(`approve-batch:${b.id}`)}>{$t('workflow.allowBatch')}</button><button class="button danger small" on:click={() => decideBatch(b, false)} disabled={isBusy(`reject-batch:${b.id}`)}>{$t('workflow.deny')}</button><button class="text-button" on:click={() => cancelBatch(b)} disabled={isBusy(`cancel-batch:${b.id}`)}>{$t('workflow.cancel')}</button></div></div>
        {/each}
        {#if !pending.length && !pendingBatches.length}<div class="empty">{$t('workflow.noPending')}</div>{/if}
      </section>
    </div>

    <!-- Quick Text & Link Sharing Panel -->
    <section class="panel">
      <div class="section-heading">
        <div>
          <span class="eyebrow">{$t('quickText.eyebrow')}</span>
          <h2>{$t('quickText.notesTitle')}</h2>
        </div>
        {#if textMessages.length > 0}
          <button class="button ghost small" on:click={clearTextAction}>{$t('quickText.clearHistory')}</button>
        {/if}
      </div>

      <div class="text-transfer-panel">
        <div class="text-compose-row">
          <textarea
            class="text-textarea"
            placeholder={$t('quickText.placeholder')}
            bind:value={textInput}
            maxlength="65536"
            disabled={!sessions.length}
          ></textarea>
          <div class="text-compose-footer">
            <span class="muted" style="font-size: 12.5px;">{$t('quickText.charsLimit', { count: textInput.length })}</span>
            <button
              class="button primary small"
              on:click={sendTextAction}
              disabled={!textInput.trim() || sendingText || !sessions.length}
            >
              {sendingText ? $t('quickText.sending') : $t('quickText.send')}
            </button>
          </div>
        </div>

        {#if !sessions.length}
          <div class="empty compact">{$t('quickText.noSessionHint')}</div>
        {:else if textMessages.length === 0}
          <div class="empty compact">{$t('quickText.noMessages')}</div>
        {:else}
          <div class="text-feed">
            {#each textMessages as msg (msg.id)}
              <div class="text-card">
                <div class="text-card-head">
                  <span class="text-sender-badge {msg.sender}">{msg.sender}</span>
                  <span>{fmtTime(msg.createdAt)}</span>
                </div>
                <div class="text-content-body">{msg.content}</div>
                <div class="text-card-actions">
                  <button class="button ghost small" on:click={() => copyTextToClipboard(msg.content)}>{$t('quickText.copy')}</button>
                  {#if msg.isLink && msg.url}
                    <button class="button secondary small" on:click={() => window.open(msg.url, '_blank', 'noopener,noreferrer')}>{$t('quickText.openLink')}</button>
                  {/if}
                </div>
              </div>
            {/each}
          </div>
        {/if}
      </div>
    </section>

    <!-- Live Telemetry -->
    <section class="panel">
      <div class="section-heading"><div><span class="eyebrow">{$t('telemetry.eyebrow')}</span><h2>{$t('telemetry.activeTransfers')}</h2></div><span class="pill">{$t('telemetry.activeCount', { count: uploads.filter(activeUpload).length + downloads.filter(activeDownload).length + batches.filter(activeBatch).length })}</span></div>
      {#each uploads.filter(activeUpload) as u (u.id)}
        <div class="transfer">
          <div class="transfer-head">
            <div>
              <strong>↙ {u.name}</strong>
              <span>
                Phone → computer · {formatBytes(u.uploaded)} of {formatBytes(u.size)}
                {#if u.state === 'uploading'}
                  · {speed(u.speed)}
                  {#if calcEta(u.uploaded, u.size, u.speed)}
                    · ETA {calcEta(u.uploaded, u.size, u.speed)}
                  {/if}
                {/if}
              </span>
            </div>
            <b>{u.state === 'uploading' ? `${percent(u.uploaded, u.size)}%` : u.state}</b>
          </div>
          {#if u.state === 'uploading'}<div class="bar"><i style={`width:${percent(u.uploaded, u.size)}%`}></i></div>{/if}
          <div class="transfer-foot">
            <span>{u.state === 'approved' ? 'Approved — waiting for phone' : u.state === 'pending' ? 'Awaiting approval' : 'Receiving bytes'}</span>
            {#if ['approved', 'uploading'].includes(u.state)}<button class="text-button" on:click={() => cancelUpload(u.id)}>{$t('telemetry.cancel')}</button>{/if}
          </div>
        </div>
      {/each}

      {#each batches.filter(activeBatch) as b (b.id)}
        <div class="transfer">
          <div class="transfer-head">
            <div><strong>↙ Batch · {batchName(b)}</strong><span>Phone → computer · {doneItems(b)} of {b.totalItemCount} files · {formatBytes(b.transferredBytes)} of {formatBytes(b.declaredTotalBytes)}</span></div>
            <b>{percent(b.transferredBytes, b.declaredTotalBytes)}%</b>
          </div>
          <div class="bar"><i style={`width:${percent(b.transferredBytes, b.declaredTotalBytes)}%`}></i></div>
          <div class="transfer-foot">
            <span>{currentItem(b) ? `Receiving ${currentItem(b)?.relativePath || currentItem(b)?.displayName}` : 'Approved — waiting for the phone to send the next file'}</span>
            <button class="text-button" on:click={() => cancelBatch(b)} disabled={isBusy(`cancel-batch:${b.id}`)}>{$t('telemetry.cancel')}</button>
          </div>
        </div>
      {/each}

      {#each downloads.filter(activeDownload) as d (d.id)}
        <div class="transfer">
          <div class="transfer-head">
            <div>
              <strong>↗ {d.name}</strong>
              <span>
                Computer → phone · {d.kind === 'archive' ? 'ZIP archive' : 'Individual file'} · {formatBytes(d.written)} of {formatBytes(d.size)} · {speed(d.speed)}
                {#if calcEta(d.written, d.size, d.speed)}
                  · ETA {calcEta(d.written, d.size, d.speed)}
                {/if}
              </span>
            </div>
            <b>{d.state === 'starting' || d.state === 'finalizing' ? d.state : `${percent(d.written, d.size)}%`}</b>
          </div>
          <div class="bar"><i style={`width:${percent(d.written, d.size)}%`}></i></div>
          <div class="transfer-foot">
            <span>{d.state === 'starting' ? 'Preparing server-side transfer' : d.state === 'finalizing' ? 'Finalizing server-side transfer' : 'Sending to the phone browser'}</span>
            <span>Browser save completion is not confirmed by the desktop.</span>
          </div>
        </div>
      {/each}

      {#if !uploads.some(activeUpload) && !downloads.some(activeDownload) && !batches.some(activeBatch)}
        <div class="empty">{$t('telemetry.noActiveTransfers')}</div>
      {/if}
    </section>

    <!-- History -->
    <section class="panel history">
      <div class="section-heading"><div><span class="eyebrow">{$t('history.eyebrow')}</span><h2>{$t('history.title')}</h2></div><span class="muted">{$t('history.sessionHistory')}</span></div>
      {#each batches.filter(finishedBatch).slice(-12).reverse() as b (b.id)}
        <div class="history-row">
          <span class="history-icon">↙</span>
          <div class="grow">
            <strong>Batch · {batchName(b)}</strong>
            <span>{doneItems(b)} of {b.totalItemCount} files · {formatBytes(b.transferredBytes)} of {formatBytes(b.declaredTotalBytes)} · {fmtTime(b.createdAt)}</span>
            {#if b.items && b.items.some(i => i.sha256)}
              <div style="margin-top: 4px; display: flex; flex-wrap: wrap; gap: 4px;">
                {#each b.items.filter(i => i.sha256) as it}
                  <button
                    type="button"
                    class="button ghost small"
                    style="padding: 2px 4px; font-size: 10px; font-family: monospace; color: var(--text-muted);"
                    title="Click to copy SHA256 for {it.displayName}"
                    on:click={() => copyTextToClipboard(it.sha256 || '')}
                  >
                    {it.displayName}: {(it.sha256 || '').slice(0, 8)}…
                  </button>
                {/each}
              </div>
            {/if}
            {#if b.error}<small class="error-text" style="display:block; margin-top:2px;">{b.error}</small>{/if}
          </div>
          <span class="badge {b.state}">{b.state}</span>
        </div>
      {/each}

      {#each [...uploads.filter((u) => !activeUpload(u)), ...downloads.filter((d) => !activeDownload(d))].slice(-12).reverse() as item (item.id)}
        <div class="history-row">
          <span class="history-icon">{'fileId' in item ? '↗' : '↙'}</span>
          <div class="grow">
            <strong>{item.name}</strong>
            <span>{'fileId' in item ? `${item.kind === 'archive' ? 'ZIP archive' : 'Individual file'} · ` : ''}{formatBytes('fileId' in item ? item.written : item.uploaded)} of {formatBytes(item.size)} · {fmtTime(item.created)}</span>
            {#if 'sha256' in item && item.sha256}
              <div style="margin-top: 2px;">
                <button
                  type="button"
                  class="button ghost small"
                  style="padding: 2px 4px; font-size: 11px; font-family: monospace; color: var(--text-muted);"
                  title="Click to copy SHA256 hash"
                  on:click={() => copyTextToClipboard(item.sha256 || '')}
                >
                  SHA256: {item.sha256.slice(0, 12)}…
                </button>
              </div>
            {/if}
            {#if 'reason' in item && item.reason}<small class="error-text" style="display:block; margin-top:2px;">{item.reason}</small>{/if}
          </div>
          <span class="badge {item.state}">{item.state === 'sent' ? 'sent to browser' : item.state}</span>
        </div>
      {/each}

      {#if !batches.some(finishedBatch) && !uploads.some((u) => !activeUpload(u)) && !downloads.some((d) => !activeDownload(d))}
        <div class="empty">{$t('history.noCompleted')}</div>
      {/if}
      </section>
    </div>

  <!-- TAB 2: GUEST DROP BOX -->
  {:else if activeTab === 'guest'}
    <div id="panel-guest" role="tabpanel" aria-labelledby="tab-guest" tabindex="0">
    {#if !guestRoomState?.room || guestRoomState.room.state === 'ended' || guestRoomState.room.state === 'expired' || guestRoomState.room.state === 'Ended'}
      <!-- Room Setup Card -->
      <section class="panel">
        <div class="section-heading">
          <div>
            <span class="eyebrow accent">Guest Drop Box</span>
            <h2>Collect files from any phone</h2>
          </div>
          <span class="pill">Inactive</span>
        </div>
        <p class="muted">
          Start a temporary drop box room. Anyone in the room scans the QR code with their phone camera to upload files without installing an app. Files are saved in isolated guest subfolders.
        </p>

        <div style="margin-top: 18px; display: grid; gap: 16px;">
          <!-- Presets -->
          <div>
            <span class="connection-label">Collection Presets</span>
            <div class="guest-presets" style="margin-top: 6px;">
              {#each presets as p (p.id)}
                <button
                  class="guest-preset-btn"
                  class:active={selectedPresetId === p.id}
                  on:click={() => applyPreset(p)}
                >
                  {p.name}
                </button>
              {/each}
            </div>
          </div>

          <!-- Title and Description -->
          <div style="display: grid; grid-template-columns: 1fr 1fr; gap: 12px;">
            <div>
              <label for="room-title-input" class="connection-label">Collection Title</label>
              <input
                id="room-title-input"
                type="text"
                class="text-input"
                style="width: 100%; margin-top: 4px;"
                bind:value={guestTitle}
                placeholder="e.g. Classroom Assignment Submission"
              />
            </div>
            <div>
              <label for="room-policy-select" class="connection-label">Collision Policy</label>
              <select
                id="room-policy-select"
                class="text-input"
                style="width: 100%; margin-top: 4px;"
                bind:value={guestCollisionPolicy}
              >
                <option value="preserve_both">Preserve Both (auto-number variants: file (1).ext)</option>
                <option value="reject">Strict Reject (reject duplicate file names)</option>
              </select>
            </div>
          </div>

          <div>
            <label for="room-desc-input" class="connection-label">Instructions / Guidelines for Guests</label>
            <textarea
              id="room-desc-input"
              class="text-input"
              style="width: 100%; margin-top: 4px; resize: vertical;"
              rows="2"
              bind:value={guestDescription}
              placeholder="Instructions displayed on guest phones..."
            ></textarea>
          </div>

          {#if guestFormSchema && guestFormSchema.length > 0}
            <div class="meta-chip">
              <span>Configured Submission Fields: <strong>{guestFormSchema.map(f => f.label + (f.required ? ' *' : '')).join(', ')}</strong></span>
            </div>
          {/if}

          <div>
            <span class="connection-label">Destination Folder</span>
            <div style="display: flex; align-items: center; gap: 12px; margin-top: 6px;">
              <span class="guest-dest-path">{guestDestDir || receiveDir || 'Downloads/Flitnix'}</span>
              <button class="button ghost small" on:click={chooseGuestDestination}>Change Folder</button>
            </div>
            <p class="muted" style="margin-top: 4px; font-size: 12.5px;">
              Files will be stored under <code>&lt;Folder&gt;/Flitnix Guest Drop/&lt;Room&gt;/&lt;Guest&gt;/</code>.
            </p>
          </div>

          <div>
            <span class="connection-label">Room Duration Presets</span>
            <div class="guest-presets" style="margin-top: 6px;">
              <button class="guest-preset-btn" class:active={guestDurationMinutes === 15} on:click={() => guestDurationMinutes = 15}>15 Minutes</button>
              <button class="guest-preset-btn" class:active={guestDurationMinutes === 30} on:click={() => guestDurationMinutes = 30}>30 Minutes</button>
              <button class="guest-preset-btn" class:active={guestDurationMinutes === 60} on:click={() => guestDurationMinutes = 60}>1 Hour</button>
              <button class="guest-preset-btn" class:active={guestDurationMinutes === 120} on:click={() => guestDurationMinutes = 120}>2 Hours</button>
              <button class="guest-preset-btn" class:active={guestDurationMinutes === 240} on:click={() => guestDurationMinutes = 240}>4 Hours</button>
            </div>
          </div>

          <div class="button-row">
            <button class="button primary" on:click={startGuestRoomAction} disabled={guestStarting}>
              {guestStarting ? 'Creating Room…' : 'Start Guest Drop Box Room'}
            </button>
          </div>
        </div>
      </section>
    {:else}
      <!-- Active Room Dashboard -->
      <section class="panel">
        <div class="section-heading">
          <div>
            <span class="eyebrow accent">Active Room · {guestRoomState.room.id}</span>
            <h2>{guestRoomState.room.title || 'Guest Drop Box'}</h2>
          </div>
          <div style="display: flex; align-items: center; gap: 10px;">
            <span class="pill" style="border-color: var(--teal); color: #7ff0df;">
              {guestRoomState.room.state} · {fmtRemaining(guestRoomState.room.expiresAt)}
            </span>
            <button class="button danger small" on:click={endGuestRoomAction} disabled={isBusy('end-guest-room')}>
              End Room
            </button>
          </div>
        </div>

        {#if guestRoomState.room.description}
          <p class="muted" style="margin-top: 4px;">{guestRoomState.room.description}</p>
        {/if}

        <div class="collection-meta-bar">
          <span class="meta-chip">Collision Policy: <strong>{guestRoomState.room.collisionPolicy === 'reject' ? 'Strict Reject' : 'Preserve Both'}</strong></span>
          {#if guestRoomState.room.deadline}
            <span class="meta-chip">Deadline: <strong>{fmtTime(guestRoomState.room.deadline)}</strong></span>
          {/if}
          <div style="margin-left: auto; display: flex; align-items: center; gap: 10px;">
            <button class="button primary small" on:click={exportCollectionAction} disabled={exporting}>
              {exporting ? 'Exporting ZIP…' : 'Export Collection (ZIP)'}
            </button>
            {#if exportSuccessMsg}
              <span style="color: #34d399; font-size: 12px; font-weight: 600;">{exportSuccessMsg}</span>
            {/if}
          </div>
        </div>

        <div class="guest-room-active-grid" style="margin-top: 14px;">
          <div class="guest-qr-card">
            {#if guestRoomState.room.qrDataUrl}
              <img class="guest-qr-img" src={guestRoomState.room.qrDataUrl} alt="Guest Drop Box QR Code" />
            {/if}
            <div>
              <strong>Scan to Drop Files</strong>
              <p class="muted" style="margin: 4px 0 8px;">Upload directly from phone browser.</p>
              <button class="button secondary small" on:click={() => copyGuestLink(guestRoomState?.room?.inviteUrl)}>
                {guestCopiedLink ? 'Copied Link!' : 'Copy Invite Link'}
              </button>
            </div>
          </div>

          <div>
            <span class="connection-label">Storage Destination</span>
            <div style="display: flex; align-items: center; gap: 10px; margin-top: 6px;">
              <span class="guest-dest-path">{guestRoomState.room.roomFolder}</span>
              <button class="button ghost small" on:click={openGuestFolderAction}>Open Folder</button>
            </div>
            <p class="muted" style="margin-top: 6px; font-size: 12.5px;">
              Each guest is isolated to their own named subfolder. Host approval is required for all uploads.
            </p>
          </div>
        </div>
      </section>

      <!-- Roster / Expected Participants Section -->
      <section class="panel workflow" style="margin-top: 14px;">
        <div class="section-heading">
          <div>
            <span class="eyebrow blue">Submission Tracking</span>
            <h2>Expected Participants Roster</h2>
          </div>
          <span class="count">{guestRoomState.roster?.length ?? 0}</span>
        </div>

        <!-- Add Participant Row -->
        <div class="form-input-row" style="margin: 10px 0 14px;">
          <input
            type="text"
            class="text-input"
            bind:value={newParticipantName}
            placeholder="Participant Name (e.g. John Doe)"
          />
          <input
            type="text"
            class="text-input"
            bind:value={newParticipantIdentifier}
            placeholder="Identifier / Email (e.g. CS101-042)"
          />
          <button
            class="button secondary small"
            on:click={addParticipantAction}
            disabled={!newParticipantName.trim() || addingParticipant}
          >
            {addingParticipant ? 'Adding…' : 'Add to Roster'}
          </button>
        </div>

        {#if guestRoomState.roster && guestRoomState.roster.length > 0}
          <div class="table-wrap">
            <table class="data-table">
              <thead>
                <tr>
                  <th>Participant</th>
                  <th>Identifier</th>
                  <th>Status</th>
                  <th>Files Submitted</th>
                  <th>Action</th>
                </tr>
              </thead>
              <tbody>
                {#each guestRoomState.roster as p (p.id)}
                  <tr>
                    <td><strong>{p.name}</strong></td>
                    <td><code>{p.identifier || '—'}</code></td>
                    <td>
                      <span class="status-pill {p.status}">
                        {p.status.replace(/_/g, ' ')}
                      </span>
                    </td>
                    <td>{p.submissionCount} file{p.submissionCount === 1 ? '' : 's'}</td>
                    <td>
                      <button
                        class="button ghost small"
                        style="color: var(--danger); padding: 2px 6px;"
                        on:click={() => removeParticipantAction(p.id)}
                        aria-label="Remove participant"
                      >
                        Remove
                      </button>
                    </td>
                  </tr>
                {/each}
              </tbody>
            </table>
          </div>
        {:else}
          <div class="empty compact">
            No expected participants pre-loaded. Guests can still join anonymously or with custom nicknames.
          </div>
        {/if}
      </section>

      <!-- Guest Approval Queues Grid -->
      <div class="guest-subsections" style="margin-top: 14px;">
        <!-- Section 1: Guest Requests -->
        <section class="panel workflow">
          <div class="section-heading">
            <div>
              <span class="eyebrow accent">Access Control</span>
              <h2>Guest Requests</h2>
            </div>
            <span class="count">{guestRoomState.pendingGuests?.length ?? 0}</span>
          </div>

          {#if guestRoomState.pendingGuests && guestRoomState.pendingGuests.length > 0}
            {#each guestRoomState.pendingGuests as g (g.id)}
              <div class="guest-item-card">
                <div>
                  <strong>{g.nickname || 'Guest'}</strong>
                  <span>Requested {fmtTime(g.createdAt)}</span>
                </div>
                <div class="button-row" style="margin-top: 0;">
                  <button class="button primary small" on:click={() => approveGuestAction(g.id)} disabled={isBusy(`approve-guest:${g.id}`)}>Approve</button>
                  <button class="button danger small" on:click={() => rejectGuestAction(g.id)} disabled={isBusy(`reject-guest:${g.id}`)}>Decline</button>
                </div>
              </div>
            {/each}
          {:else}
            <div class="empty">No guests are waiting to join.</div>
          {/if}

          <!-- Approved Guests List -->
          <div style="margin-top: 18px; border-top: 1px solid var(--line); padding-top: 14px;">
            <div class="section-heading" style="margin-bottom: 10px;">
              <span class="eyebrow">Approved Guests</span>
              <span class="count">{guestRoomState.approvedGuests?.length ?? 0}</span>
            </div>
            {#if guestRoomState.approvedGuests && guestRoomState.approvedGuests.length > 0}
              {#each guestRoomState.approvedGuests as g (g.id)}
                <div class="guest-item-card">
                  <div>
                    <strong>{g.nickname || 'Guest'}</strong>
                    <span>Connected since {fmtTime(g.createdAt)}</span>
                  </div>
                  <button class="button danger small" on:click={() => revokeGuestAction(g.id)} disabled={isBusy(`revoke-guest:${g.id}`)}>
                    Revoke
                  </button>
                </div>
              {/each}
            {:else}
              <div class="empty compact">No active guests yet.</div>
            {/if}
          </div>
        </section>

        <!-- Section 2: Upload Requests & Progress -->
        <section class="panel workflow">
          <div class="section-heading">
            <div>
              <span class="eyebrow blue">Upload Inbox</span>
              <h2>File Requests</h2>
            </div>
            <span class="count">{guestRoomState.pendingUploads?.length ?? 0}</span>
          </div>

          {#if guestRoomState.pendingUploads && guestRoomState.pendingUploads.length > 0}
            {#each guestRoomState.pendingUploads as up (up.id)}
              <div class="guest-item-card">
                <div>
                  <strong>{up.fileName}</strong>
                  <span>{formatBytes(up.size)} · from {up.guestName || 'Guest'} · {fmtTime(up.createdAt)}</span>
                  {#if up.formValues && Object.keys(up.formValues).length > 0}
                    <div style="margin-top: 4px; font-size: 11.5px; color: var(--accent);">
                      {#each Object.entries(up.formValues) as [k, v]}
                        <span style="margin-right: 8px;">{k}: <strong>{v}</strong></span>
                      {/each}
                    </div>
                  {/if}
                </div>
                <div class="button-row" style="margin-top: 0;">
                  <button class="button primary small" on:click={() => approveGuestUploadAction(up.id)} disabled={isBusy(`approve-guest-upload:${up.id}`)}>Accept</button>
                  <button class="button danger small" on:click={() => rejectGuestUploadAction(up.id)} disabled={isBusy(`reject-guest-upload:${up.id}`)}>Reject</button>
                </div>
              </div>
            {/each}
          {:else}
            <div class="empty">No incoming files waiting for approval.</div>
          {/if}

          <!-- Active & Completed Uploads -->
          <div style="margin-top: 18px; border-top: 1px solid var(--line); padding-top: 14px;">
            <div class="section-heading" style="margin-bottom: 10px;">
              <span class="eyebrow">Transfers</span>
              <span class="count">{guestRoomState.recentUploads?.length ?? 0}</span>
            </div>
            {#if guestRoomState.recentUploads && guestRoomState.recentUploads.length > 0}
              {#each guestRoomState.recentUploads as up (up.id)}
                <div class="transfer" style="padding: 10px 0;">
                  <div class="transfer-head">
                    <div>
                      <strong>↙ {up.fileName}</strong>
                      <span>
                        {up.guestName || 'Guest'} · {formatBytes(up.transferred)} of {formatBytes(up.size)}
                        {#if up.state === 'uploading' && up.speed > 0} · {speed(up.speed)}{/if}
                      </span>
                      {#if up.sha256}
                        <div style="margin-top: 2px;">
                          <button
                            type="button"
                            class="button ghost small"
                            style="padding: 2px 4px; font-size: 11px; font-family: monospace; color: var(--text-muted);"
                            title="Click to copy SHA256 hash"
                            on:click={() => copyTextToClipboard(up.sha256 || '')}
                          >
                            SHA256: {up.sha256.slice(0, 12)}…
                          </button>
                        </div>
                      {/if}
                    </div>
                    <span class="badge {up.state}">{up.state}</span>
                  </div>
                  {#if up.state === 'uploading'}
                    <div class="bar"><i style={`width:${percent(up.transferred, up.size)}%`}></i></div>
                  {/if}
                  {#if up.error}
                    <small class="error-text" style="display:block; margin-top:2px;">{up.error}</small>
                  {/if}
                </div>
              {/each}
            {:else}
              <div class="empty compact">No active or recent guest uploads.</div>
            {/if}
          </div>
        </section>
      </div>
    {/if}
    </div>

  <!-- TAB 3: TEMPLATES -->
  {:else if activeTab === 'templates'}
    <div class="panel" id="panel-templates" role="tabpanel" aria-labelledby="tab-templates" tabindex="0">
      <div class="section-heading">
        <div>
          <span class="eyebrow accent">{$t('templates.eyebrow')}</span>
          <h2>{$t('templates.title')}</h2>
        </div>
        <span class="count">{templates.length}</span>
      </div>
      <p class="muted">
        {$t('templates.desc')}
      </p>

      {#if loadingTemplates}
        <div class="empty">{$t('templates.loading')}</div>
      {:else if templates.length > 0}
        <div class="template-grid">
          {#each templates as tpl (tpl.id)}
            <div class="template-card">
              <div>
                <h3>{tpl.name}</h3>
                <p>{tpl.description || 'No description provided.'}</p>
                <div style="margin-top: 10px; display: flex; flex-direction: column; gap: 4px; font-size: 12px; color: var(--text-secondary);">
                  <span>{$t('templates.duration', { duration: tpl.durationMinutes })}</span>
                  <span>{$t('templates.collision', { policy: tpl.collisionPolicy === 'reject' ? 'Strict Reject' : 'Preserve Both' })}</span>
                  {#if tpl.formSchema && tpl.formSchema.length > 0}
                    <span>{$t('templates.fieldsCount', { count: tpl.formSchema.length })}</span>
                  {/if}
                  {#if tpl.expectedParticipants && tpl.expectedParticipants.length > 0}
                    <span>{$t('templates.rosterCount', { count: tpl.expectedParticipants.length })}</span>
                  {/if}
                </div>
              </div>
              <div class="button-row" style="margin-top: 12px;">
                <button class="button primary small" on:click={() => startFromTemplateAction(tpl.id)}>
                  {$t('templates.startRoom')}
                </button>
                <button class="button secondary small" on:click={() => duplicateTemplateAction(tpl.id, tpl.name)}>
                  {$t('templates.duplicate')}
                </button>
                <button class="button ghost small" style="color: var(--danger);" on:click={() => deleteTemplateAction(tpl.id)}>
                  {$t('templates.delete')}
                </button>
              </div>
            </div>
          {/each}
        </div>
      {:else}
        <div class="empty">{$t('templates.noTemplates')}</div>
      {/if}
    </div>

  <!-- TAB 4: AUDIT HISTORY -->
  {:else if activeTab === 'audit'}
    <div class="panel" id="panel-audit" role="tabpanel" aria-labelledby="tab-audit" tabindex="0">
      <div class="section-heading">
        <div>
          <span class="eyebrow accent">{$t('audit.eyebrow')}</span>
          <h2>{$t('audit.title')}</h2>
        </div>
        <div style="display: flex; gap: 10px; align-items: center;">
          <button class="button secondary small" on:click={loadAuditLogs} disabled={auditLoading}>
            {auditLoading ? $t('audit.loading') : $t('audit.refresh')}
          </button>
          <button class="button danger small" on:click={clearAuditAction}>
            {$t('audit.clearHistory')}
          </button>
        </div>
      </div>
      <p class="muted">
        {$t('audit.desc')}
      </p>

      <div style="display: flex; gap: 12px; margin: 14px 0;" class="form-input-row">
        <select class="text-input" bind:value={auditFilterType} on:change={loadAuditLogs} style="max-width: 200px;">
          <option value="">{$t('audit.allEvents')}</option>
          <option value="room_created">Room Created</option>
          <option value="room_ended">Room Ended</option>
          <option value="guest_joined">Guest Joined</option>
          <option value="guest_approved">Guest Approved</option>
          <option value="upload_declared">Upload Declared</option>
          <option value="upload_approved">Upload Approved</option>
          <option value="upload_completed">Upload Completed</option>
          <option value="export_completed">Export Completed</option>
        </select>

        <select class="text-input" bind:value={auditFilterOutcome} on:change={loadAuditLogs} style="max-width: 160px;">
          <option value="">All Outcomes</option>
          <option value="success">Success</option>
          <option value="rejected">Rejected</option>
          <option value="failed">Failed</option>
          <option value="info">Info</option>
        </select>
        <span class="muted" style="margin-left: auto; font-size: 13px;">Showing {auditEntries.length} of {auditTotal} total events</span>
      </div>

      {#if auditLoading}
        <div class="empty">{$t('audit.loading')}</div>
      {:else if auditEntries.length > 0}
        <div class="table-wrap">
          <table class="data-table">
            <thead>
              <tr>
                <th>Timestamp</th>
                <th>Event Type</th>
                <th>Room ID</th>
                <th>Guest</th>
                <th>Outcome</th>
                <th>Details</th>
              </tr>
            </thead>
            <tbody>
              {#each auditEntries as entry (entry.id)}
                <tr>
                  <td style="white-space: nowrap; font-size: 12px; color: var(--text-muted);">{fmtTime(entry.timestamp)}</td>
                  <td><strong>{entry.eventType}</strong></td>
                  <td><code>{entry.roomId || '—'}</code></td>
                  <td>{entry.guestName || entry.guestId || '—'}</td>
                  <td>
                    <span class="status-pill {entry.outcome}">
                      {entry.outcome}
                    </span>
                  </td>
                  <td style="font-size: 12px; word-break: break-all;">
                    {entry.fileName ? `${entry.fileName} (${formatBytes(entry.fileSize || 0)}) ` : ''}
                    {entry.details || ''}
                  </td>
                </tr>
              {/each}
            </tbody>
          </table>
        </div>
      {:else}
        <div class="empty">{$t('audit.noEvents')}</div>
      {/if}
    </div>

  <!-- TAB 5: TRUSTED DEVICES -->
  {:else if activeTab === 'devices'}
    <div class="panel" id="panel-devices" role="tabpanel" aria-labelledby="tab-devices" tabindex="0">
      <div class="section-heading">
        <div>
          <span class="eyebrow">{$t('devices.eyebrow')}</span>
          <h2>{$t('devices.title')}</h2>
        </div>
        <button
          type="button"
          class="button secondary small"
          on:click={loadTrustedDevices}
          disabled={loadingDevices}
        >
          {loadingDevices ? $t('devices.refreshing') : $t('devices.refresh')}
        </button>
      </div>
      <p class="muted">
        {$t('devices.desc')}
      </p>

      {#if loadingDevices}
        <div class="empty">{$t('devices.refreshing')}</div>
      {:else if trustedDevices.length > 0}
        <div class="table-wrap" style="margin-top: 16px;">
          <table class="data-table">
            <thead>
              <tr>
                <th>{$t('devices.deviceName')}</th>
                <th>{$t('devices.fingerprint')}</th>
                <th>{$t('devices.pairedAt')}</th>
                <th>{$t('devices.lastSeen')}</th>
                <th>{$t('devices.autoApprove')}</th>
                <th style="text-align: right;">{$t('devices.actions')}</th>
              </tr>
            </thead>
            <tbody>
              {#each trustedDevices as dev (dev.id)}
                <tr>
                  <td><strong>{dev.name}</strong></td>
                  <td><code>{dev.id.slice(0, 16)}…</code></td>
                  <td style="font-size: 12px; color: var(--text-muted);">{fmtTime(dev.pairedAt)}</td>
                  <td style="font-size: 12px; color: var(--text-muted);">{fmtTime(dev.lastSeen)}</td>
                  <td>
                    <button
                      type="button"
                      class="button ghost small"
                      style="font-size: 11px; padding: 2px 8px;"
                      on:click={() => toggleDeviceAutoApprove(dev.id, dev.autoApprove)}
                    >
                      {dev.autoApprove ? $t('devices.enabled') : $t('devices.disabled')}
                    </button>
                  </td>
                  <td style="text-align: right;">
                    <button
                      type="button"
                      class="button ghost small"
                      style="color: var(--danger-color, #e06c75);"
                      on:click={() => removeDeviceAction(dev.id)}
                    >
                      {$t('devices.revoke')}
                    </button>
                  </td>
                </tr>
              {/each}
            </tbody>
          </table>
        </div>
      {:else}
        <div class="empty">{$t('devices.noDevices')}</div>
      {/if}
    </div>

  <!-- TAB 6: SETTINGS -->
  {:else if activeTab === 'settings'}
    <div class="panel" id="panel-settings" role="tabpanel" aria-labelledby="tab-settings" tabindex="0">
      <div class="section-heading">
        <div>
          <span class="eyebrow accent">{$t('settings.eyebrow')}</span>
          <h2>{$t('settings.title')}</h2>
        </div>
      </div>

      {#if settingsSavedAlert}
        <div class="alert" style="background: rgba(45, 212, 191, 0.15); border: 1px solid var(--success); color: var(--success); margin-bottom: 16px;" role="status">
          <span>{settingsSavedAlert}</span>
        </div>
      {/if}

      {#if settingsError}
        <div class="alert" style="background: var(--danger-bg); border: 1px solid var(--danger-border); color: var(--danger-text); margin-bottom: 16px;" role="alert">
          <span>{settingsError}</span>
          <button type="button" on:click={() => settingsError = ''} aria-label={$t('app.dismiss')}>×</button>
        </div>
      {/if}

      <div style="display: flex; flex-direction: column; gap: 24px;">
        <!-- Download Directory -->
        <div>
          <label for="download-dir-input" class="connection-label">{$t('settings.receiveDir')}</label>
          <p style="margin: 0 0 10px; font-size: 13px; color: var(--text-secondary);">
            {$t('settings.receiveDirDesc')}
          </p>
          <div class="form-input-row" style="margin-bottom: 8px;">
            <input
              id="download-dir-input"
              type="text"
              class="text-input"
              bind:value={settingsState.downloadDir}
              placeholder={settingsState.effectiveDir || 'Default: ~/Downloads/Flitnix'}
              style="font-family: monospace; font-size: 13px;"
            />
            <button type="button" class="button" on:click={handleSelectDownloadDir}>
              {$t('settings.browse')}
            </button>
            <button type="button" class="button ghost" on:click={handleOpenDownloadFolder}>
              {$t('settings.openFolder')}
            </button>
          </div>
          <div style="display: flex; gap: 12px; align-items: center;">
            <span style="font-size: 12px; color: var(--text-tertiary);">
              {$t('settings.currentPath', { path: settingsState.effectiveDir || receiveDir })}
            </span>
            {#if settingsState.downloadDir}
              <button
                type="button"
                class="button ghost small"
                style="padding: 2px 8px; font-size: 11px;"
                on:click={() => settingsState.downloadDir = ''}
              >
                {$t('settings.resetDefault')}
              </button>
            {/if}
          </div>
        </div>

        <!-- Appearance / Theme -->
        <div style="border-top: 1px solid var(--line); padding-top: 20px;">
          <label for="theme-select" class="connection-label">{$t('settings.appearance')}</label>
          <p style="margin: 0 0 10px; font-size: 13px; color: var(--text-secondary);">
            {$t('settings.appearanceDesc')}
          </p>
          <div style="max-width: 280px;">
            <select id="theme-select" class="text-input" bind:value={settingsState.theme} style="width: 100%;">
              <option value="system">{$t('settings.themeSystem')}</option>
              <option value="dark">{$t('settings.themeDark')}</option>
              <option value="light">{$t('settings.themeLight')}</option>
            </select>
          </div>
        </div>

        <!-- Language / Locale -->
        <div style="border-top: 1px solid var(--line); padding-top: 20px;">
          <label for="language-select" class="connection-label">{$t('settings.language')}</label>
          <p style="margin: 0 0 10px; font-size: 13px; color: var(--text-secondary);">
            {$t('settings.languageDesc')}
          </p>
          <div style="max-width: 280px;">
            <select
              id="language-select"
              class="text-input"
              bind:value={settingsState.locale}
              on:change={() => setLocale(settingsState.locale)}
              style="width: 100%;"
            >
              <option value="system">{$t('settings.langSystem')}</option>
              <option value="en">{$t('settings.langEn')}</option>
            </select>
          </div>
        </div>

        <!-- Preferred Network Interface -->
        <div style="border-top: 1px solid var(--line); padding-top: 20px;">
          <label for="iface-select" class="connection-label">{$t('settings.preferredInterface')}</label>
          <p style="margin: 0 0 10px; font-size: 13px; color: var(--text-secondary);">
            {$t('settings.preferredInterfaceDesc')}
          </p>
          <div style="max-width: 380px;">
            <select id="iface-select" class="text-input" bind:value={settingsState.preferredInterface} style="width: 100%;">
              <option value="">{$t('settings.autoInterface')}</option>
              {#each uniqueInterfaces as iface (iface.name)}
                <option value={iface.name}>
                  {iface.name} ({iface.ip}{iface.family ? ` · ${iface.family}` : ''})
                </option>
              {/each}
              {#if settingsState.preferredInterface && !uniqueInterfaces.some(i => i.name === settingsState.preferredInterface)}
                <option value={settingsState.preferredInterface}>
                  {$t('settings.ifaceUnavailable', { name: settingsState.preferredInterface })}
                </option>
              {/if}
            </select>
          </div>
        </div>

        <!-- Session Automation -->
        <div style="border-top: 1px solid var(--line); padding-top: 20px;">
          <span class="connection-label">{$t('settings.sessionAutomation')}</span>
          <label style="display: flex; align-items: center; gap: 10px; cursor: pointer; margin-top: 8px;">
            <input
              type="checkbox"
              bind:checked={settingsState.autoStartSession}
              style="width: 18px; height: 18px; cursor: pointer;"
            />
            <span style="font-size: 14px; color: var(--text-main); font-weight: 600;">
              {$t('settings.autoStartSession')}
            </span>
          </label>
          <p style="margin: 6px 0 0 28px; font-size: 13px; color: var(--text-secondary);">
            {$t('settings.autoStartSessionDesc')}
          </p>
        </div>

        <!-- Actions -->
        <div style="border-top: 1px solid var(--line); padding-top: 20px; display: flex; justify-content: space-between; align-items: center;">
          <button
            type="button"
            class="button"
            disabled={savingSettings}
            on:click={handleSaveSettings}
          >
            {savingSettings ? $t('settings.saving') : $t('settings.savePreferences')}
          </button>

          <button
            type="button"
            class="button ghost"
            style="color: var(--text-secondary);"
            on:click={handleResetSettings}
          >
            {$t('settings.resetDefaults')}
          </button>
        </div>
      </div>
    </div>
  {/if}

  {#if receiveDir}
    <footer>{$t('footer.savedPath', { path: receiveDir })}</footer>
  {/if}
</main>
