<script lang="ts">
  import { onMount } from 'svelte'
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
      refresh()
    } catch (err) {
      report(err)
    } finally {
      starting = false
    }
  }

  function endSession(key: string): void {
    const k = `end:${key}`; if (isBusy(k)) return
    setBusy(k, true); EndSession(key).then(refresh).catch((e) => report(e, k)).finally(() => setBusy(k, false))
  }

  function decide(u: main.UploadView, approve: boolean): void {
    const k = `${approve ? 'approve' : 'reject'}:${u.id}`; if (isBusy(k)) return
    setBusy(k, true)
    const call = approve ? ApproveUpload(u.id) : RejectUpload(u.id)
    call.then(refresh).catch((e) => report(e, k)).finally(() => setBusy(k, false))
  }

  function decideBatch(batch: main.BatchView, approve: boolean): void {
    const k = `${approve ? 'approve' : 'reject'}-batch:${batch.id}`; if (isBusy(k)) return
    setBusy(k, true)
    const call = approve ? ApproveBatch(batch.id) : RejectBatch(batch.id)
    call.then(refresh).catch((e) => report(e, k)).finally(() => setBusy(k, false))
  }

  function cancelBatch(batch: main.BatchView): void {
    const k = `cancel-batch:${batch.id}`; if (isBusy(k)) return
    setBusy(k, true)
    CancelBatch(batch.id).then(refresh).catch((e) => report(e, k)).finally(() => setBusy(k, false))
  }

  function cancelUpload(id: string): void {
    const k = `cancel:${id}`; if (isBusy(k)) return
    setBusy(k, true); CancelUpload(id).then(refresh).catch((e) => report(e, k)).finally(() => setBusy(k, false))
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
    AddDroppedSources(paths).then(refresh).catch(report)
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
      <div class="brand"><img class="brand-logo" src={logoUrl} alt="Flitnix"/><h1>Flitnix</h1></div>
      <p>Fast, private file transfer over your local network.</p>
    </div>
    <div class="topbar-actions" style="display: flex; align-items: center; gap: 12px;">
      <div class:online={sessions.length > 0} class="status">
        <span class="status-dot"></span>{sessions.length > 0 ? 'Session active' : 'Session inactive'}
      </div>
    </div>
  </header>

  <!-- Navigation bar with Tabs and Connection Doctor launcher -->
  <nav class="nav-bar" aria-label="Main Navigation">
    <div class="tab-list" role="tablist">
      <button
        type="button"
        role="tab"
        aria-selected={activeTab === 'personal'}
        class="tab-btn"
        class:active={activeTab === 'personal'}
        on:click={() => activeTab = 'personal'}
      >
        <span>Personal Transfer</span>
        {#if pending.length + pendingBatches.length > 0}
          <span class="tab-badge">{pending.length + pendingBatches.length}</span>
        {/if}
      </button>

      <button
        type="button"
        role="tab"
        aria-selected={activeTab === 'guest'}
        class="tab-btn"
        class:active={activeTab === 'guest'}
        on:click={() => activeTab = 'guest'}
      >
        <span>Guest Drop Box</span>
        {#if guestPendingCount > 0}
          <span class="tab-badge">{guestPendingCount}</span>
        {/if}
      </button>

      <button
        type="button"
        role="tab"
        aria-selected={activeTab === 'templates'}
        class="tab-btn"
        class:active={activeTab === 'templates'}
        on:click={() => { activeTab = 'templates'; loadTemplates(); }}
      >
        <span>Templates</span>
      </button>

      <button
        type="button"
        role="tab"
        aria-selected={activeTab === 'audit'}
        class="tab-btn"
        class:active={activeTab === 'audit'}
        on:click={() => { activeTab = 'audit'; loadAuditLogs(); }}
      >
        <span>Audit History</span>
      </button>

      <button
        type="button"
        role="tab"
        aria-selected={activeTab === 'devices'}
        class="tab-btn"
        class:active={activeTab === 'devices'}
        on:click={() => { activeTab = 'devices'; loadTrustedDevices(); }}
      >
        <span>Trusted Devices</span>
        {#if trustedDevices.length > 0}
          <span class="tab-badge">{trustedDevices.length}</span>
        {/if}
      </button>

      <button
        type="button"
        role="tab"
        aria-selected={activeTab === 'settings'}
        class="tab-btn"
        class:active={activeTab === 'settings'}
        on:click={() => { activeTab = 'settings'; loadSettings(); }}
      >
        <span>Settings</span>
      </button>
    </div>

    <button type="button" class="doctor-launch-btn" on:click={openDoctor} aria-label="Open Connection Doctor">
      <span class="doctor-dot" class:warn={doctorReport?.overallStatus === 'WARNING'} class:fail={doctorReport?.overallStatus === 'FAIL'}></span>
      <span>Connection Doctor</span>
    </button>
  </nav>

  {#if errorText}<div class="alert" role="alert"><span>{errorText}</span><button on:click={() => errorText = ''} aria-label="Dismiss">×</button></div>{/if}

  <!-- Connection Doctor Modal -->
  {#if doctorOpen}
    <div
      class="modal-overlay"
      role="presentation"
      on:click|self={() => doctorOpen = false}
      on:keydown={(e) => { if (e.key === 'Escape') doctorOpen = false }}
    >
      <div class="doctor-dialog" role="dialog" aria-labelledby="doctor-title" aria-modal="true">
        <div class="doctor-header">
          <div style="display: flex; align-items: center; gap: 10px;">
            <span class="doctor-dot" class:warn={doctorReport?.overallStatus === 'WARNING'} class:fail={doctorReport?.overallStatus === 'FAIL'}></span>
            <h2 id="doctor-title">Connection Doctor</h2>
          </div>
          <button class="button ghost small" on:click={() => doctorOpen = false} aria-label="Close dialog">✕</button>
        </div>

        <div class="doctor-body">
          {#if doctorLoading}
            <div class="empty" style="text-align: center;">Scanning network interfaces, port 43821, and local connectivity…</div>
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
                    <strong>Recommended Action:</strong> {item.remediation}
                  </div>
                {/if}
              </div>
            {/each}
          {/if}
        </div>

        <div class="doctor-footer">
          <div style="display: flex; gap: 8px;">
            <button class="button secondary small" on:click={runDoctor} disabled={doctorLoading}>
              {doctorLoading ? 'Checking…' : 'Run Diagnostics'}
            </button>
            <button class="button ghost small" on:click={regenerateQRAction} disabled={!sessions.length && (!guestRoomState || !guestRoomState.room)}>
              Regenerate QR
            </button>
          </div>
          <button class="button ghost small" on:click={() => doctorOpen = false}>Close</button>
        </div>
      </div>
    </div>
  {/if}

  <!-- TAB 1: PERSONAL TRANSFER -->
  {#if activeTab === 'personal'}
    <section class="connection panel">
      <div class="section-heading"><div><span class="eyebrow connection-eyebrow"><span class="network-glyph" aria-hidden="true">⌁</span> Connection</span><h2>Ready to pair</h2></div><span class="pill">{sessions.length ? 'Session active' : 'Ready'}</span></div>
      <div class="connection-grid">
        <div>
          <span class="connection-label">Current network</span>
          {#if currentNetwork}
            <div class="network-identity">
              <span class="network-dot" aria-hidden="true"></span>
              <div><strong>{currentNetwork.friendlyName || `${currentNetwork.type || 'Network'} · Name unavailable`}</strong><span>{currentNetwork.type || 'Network'} · Connected{currentNetwork.friendlyNameSource === 'Connection profile' ? ' · Profile name' : ''}</span></div>
            </div>
            <button class="button ghost change-network" on:click={() => networkChooserOpen = !networkChooserOpen}>
              {networkChooserOpen ? 'Hide networks' : 'Change network'}
            </button>
          {:else if loadingIfaces}
            <div class="network-identity"><span class="network-dot unavailable"></span><div><strong>Looking for a network…</strong><span>Scanning available connections</span></div></div>
          {:else if interfaces.length > 1}
            <div class="network-identity"><span class="network-dot unavailable"></span><div><strong>Choose a network</strong><span>Multiple connections are available</span></div></div>
            <button class="button ghost change-network" on:click={() => networkChooserOpen = true}>Change network</button>
          {:else}
            <div class="network-identity"><span class="network-dot unavailable"></span><div><strong>No network available</strong><span>Connect to Wi-Fi or Ethernet to continue</span></div></div>
          {/if}
          {#if networkChooserOpen || interfaces.length > 1}
            <div class="network-picker">
              <label for="interface">Choose a network</label>
              <select id="interface" bind:value={selectedIp} aria-label="Choose a network">
                <option value="" disabled>Select a network…</option>
                {#each interfaces as itf (itf.ip)}
                  <option value={itf.ip} disabled={itf.suitableForQR === false}>
                    {itf.friendlyName || `${itf.type || 'Network'} · Name unavailable`} · {itf.family || 'IPv4'} ({itf.ip})
                    {#if itf.suitableForQR === false} — {itf.warning || 'Not suitable for phone QR'}{/if}
                  </option>
                {/each}
              </select>
              <button class="button ghost" on:click={refreshNetwork} disabled={loadingIfaces}>{loadingIfaces ? 'Refreshing…' : 'Rescan'}</button>
            </div>
          {/if}
          {#if currentNetwork}
            <details class="network-details">
              <summary>Network details</summary>
              <dl>
                <div><dt>Interface</dt><dd>{currentNetwork.name || 'Unavailable'}</dd></div>
                <div><dt>Address family</dt><dd>{currentNetwork.family || 'IPv4'}</dd></div>
                <div><dt>IP address</dt><dd>{currentNetwork.ip || 'Unavailable'}</dd></div>
                <div><dt>Prefix / CIDR</dt><dd>{currentNetwork.cidr || 'Unavailable'}</dd></div>
                <div><dt>Connection type</dt><dd>{currentNetwork.type || 'Network'}</dd></div>
                <div><dt>Profile name</dt><dd>{currentNetwork.connectionName || 'Unavailable'}</dd></div>
                <div><dt>Suitable for QR</dt><dd>{currentNetwork.suitableForQR !== false ? 'Yes (LAN reachable)' : 'No'}</dd></div>
                {#if currentNetwork.warning}<div><dt>Notice</dt><dd>{currentNetwork.warning}</dd></div>{/if}
                <div><dt>Name source</dt><dd>{currentNetwork.friendlyNameSource || 'Unavailable'}</dd></div>
                <div><dt>Metadata</dt><dd>{currentNetwork.metadataAvailable ? 'Available' : 'Name unavailable'}</dd></div>
              </dl>
            </details>
          {/if}
          {#if ifaceError}<p class="error-text">{ifaceError}</p>{/if}
        </div>
        {#if sessions.length && qrUrl}
          <div class="pairing">
            <img class="qr" src={qrUrl} alt="Pairing QR code" />
            <div><strong>Scan to connect</strong><p class="muted">http://{hostPort()}</p><p class="muted">Incoming files always ask for approval.</p>
              <div class="button-row"><button class="button ghost" on:click={() => fetchQR(qrKey)}>Regenerate</button><button class="button danger" on:click={() => endSession(qrKey)} disabled={isBusy(`end:${qrKey}`)}>Stop session</button></div>
            </div>
          </div>
        {:else if sessions.length}<p class="muted">{qrError || 'Preparing the pairing QR…'}</p>{/if}
      </div>
      {#if !sessions.length}<div class="empty compact">Choose the interface connected to your phone, then start a session.</div>{/if}
      <div class="button-row"><button class="button primary" on:click={startSession} disabled={starting || (interfaces.length > 1 && !selectedIp)}>{starting ? 'Starting…' : 'Start session'}</button></div>
      <div class="notice">⚠ Plain HTTP is used on your trusted hotspot or LAN. Connect only devices you control.</div>
    </section>

    <div class="workflow-grid">
      <section class="panel workflow drop-target">
        <div class="section-heading"><div><span class="eyebrow accent">Send to phone</span><h2>Shared files</h2></div><span class="count">{files.length}</span></div>
        <p class="muted">Files listed here are available for direct download from the phone.</p>
        {#each files as f (f.id)}
          <div class="file-row"><div class="file-icon">↗</div><div class="grow"><strong>{f.name}</strong><span>{formatBytes(f.size)}</span></div><button class="button danger small" on:click={() => revoke(f.id)} disabled={isBusy(`revoke:${f.id}`)}>Remove</button></div>
        {:else}<div class="empty">No files shared yet. Choose files or drop them here.</div>{/each}
        <div class="button-row"><button class="button secondary" on:click={pickFiles} disabled={picking || pickingFolder}>{picking ? 'Opening picker…' : '+ Choose files'}</button><button class="button ghost" on:click={pickFolder} disabled={picking || pickingFolder}>{pickingFolder ? 'Reading folder…' : '+ Choose folder'}</button></div>
        <div class="drop-hint">Drop files or folders here. Folders are prepared privately for a future archive download.</div>
        {#each sources.filter((source) => source.kind === 'folder') as source (source.id)}
          <div class="file-row source-row"><div class="file-icon">▣</div><div class="grow"><strong>{source.rootName || source.name}</strong><span>Folder · {source.itemCount} files · {formatBytes(source.totalBytes)} · streamed ZIP available from the phone</span></div><button class="button danger small" on:click={() => revokeSource(source.id)} disabled={isBusy(`revoke-source:${source.id}`)}>Remove</button></div>
        {/each}
      </section>

      <section class="panel workflow">
        <div class="section-heading"><div><span class="eyebrow blue">Receive from phone</span><h2>Approval inbox</h2></div><span class="count">{pending.length + pendingBatches.length}</span></div>
        <p class="muted">Review each request before any file contents reach this computer.</p>
        {#each pending as u (u.id)}
          <div class="request-row"><div class="file-icon">↙</div><div class="grow"><strong>{u.name}</strong><span>{formatBytes(u.size)} · waiting for approval</span></div><div class="button-row"><button class="button primary small" on:click={() => decide(u, true)} disabled={isBusy(`approve:${u.id}`)}>Allow</button><button class="button danger small" on:click={() => decide(u, false)} disabled={isBusy(`reject:${u.id}`)}>Deny</button></div></div>
        {/each}
        {#each pendingBatches as b (b.id)}
          <div class="request-row"><div class="file-icon">↙</div><div class="grow"><strong>Batch · {batchName(b)}</strong><span>{formatBytes(b.declaredTotalBytes)} · waiting for approval</span><small>{b.items.map((item) => item.relativePath || item.displayName).join(', ')}</small></div><div class="button-row"><button class="button primary small" on:click={() => decideBatch(b, true)} disabled={isBusy(`approve-batch:${b.id}`)}>Allow batch</button><button class="button danger small" on:click={() => decideBatch(b, false)} disabled={isBusy(`reject-batch:${b.id}`)}>Deny</button><button class="text-button" on:click={() => cancelBatch(b)} disabled={isBusy(`cancel-batch:${b.id}`)}>Cancel</button></div></div>
        {/each}
        {#if !pending.length && !pendingBatches.length}<div class="empty">No files are waiting for approval.</div>{/if}
      </section>
    </div>

    <!-- Quick Text & Link Sharing Panel -->
    <section class="panel">
      <div class="section-heading">
        <div>
          <span class="eyebrow">Quick Text & Links</span>
          <h2>Instant bidirectional notes</h2>
        </div>
        {#if textMessages.length > 0}
          <button class="button ghost small" on:click={clearTextAction}>Clear History</button>
        {/if}
      </div>

      <div class="text-transfer-panel">
        <div class="text-compose-row">
          <textarea
            class="text-textarea"
            placeholder="Type text or paste a link (http/https) to share with paired phone…"
            bind:value={textInput}
            maxlength="65536"
            disabled={!sessions.length}
          ></textarea>
          <div class="text-compose-footer">
            <span class="muted" style="font-size: 12.5px;">{textInput.length} / 65536 characters</span>
            <button
              class="button primary small"
              on:click={sendTextAction}
              disabled={!textInput.trim() || sendingText || !sessions.length}
            >
              {sendingText ? 'Sending…' : 'Send Text'}
            </button>
          </div>
        </div>

        {#if !sessions.length}
          <div class="empty compact">Start a pairing session above to share text and links with your phone.</div>
        {:else if textMessages.length === 0}
          <div class="empty compact">No text messages shared in this session yet.</div>
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
                  <button class="button ghost small" on:click={() => copyTextToClipboard(msg.content)}>Copy</button>
                  {#if msg.isLink && msg.url}
                    <button class="button secondary small" on:click={() => window.open(msg.url, '_blank', 'noopener,noreferrer')}>Open Link ↗</button>
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
      <div class="section-heading"><div><span class="eyebrow">Live telemetry</span><h2>Active transfers</h2></div><span class="pill">{uploads.filter(activeUpload).length + downloads.filter(activeDownload).length + batches.filter(activeBatch).length} active</span></div>
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
            {#if ['approved', 'uploading'].includes(u.state)}<button class="text-button" on:click={() => cancelUpload(u.id)}>Cancel</button>{/if}
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
            <button class="text-button" on:click={() => cancelBatch(b)} disabled={isBusy(`cancel-batch:${b.id}`)}>Cancel</button>
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
        <div class="empty">No active transfers. Start a download or approve an incoming file to see live progress.</div>
      {/if}
    </section>

    <!-- History -->
    <section class="panel history">
      <div class="section-heading"><div><span class="eyebrow">History</span><h2>Recent transfers</h2></div><span class="muted">Session history</span></div>
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
        <div class="empty">No completed transfers in this session.</div>
      {/if}
    </section>

  <!-- TAB 2: GUEST DROP BOX -->
  {:else if activeTab === 'guest'}
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

  <!-- TAB 3: TEMPLATES -->
  {:else if activeTab === 'templates'}
    <section class="panel">
      <div class="section-heading">
        <div>
          <span class="eyebrow accent">Reusable Schemas</span>
          <h2>Collection Templates & Presets</h2>
        </div>
        <span class="count">{templates.length}</span>
      </div>
      <p class="muted">
        Save room settings, submission forms, and rosters as templates to quickly launch recurring collection drop boxes.
      </p>

      {#if loadingTemplates}
        <div class="empty">Loading saved templates…</div>
      {:else if templates.length > 0}
        <div class="template-grid">
          {#each templates as tpl (tpl.id)}
            <div class="template-card">
              <div>
                <h3>{tpl.name}</h3>
                <p>{tpl.description || 'No description provided.'}</p>
                <div style="margin-top: 10px; display: flex; flex-direction: column; gap: 4px; font-size: 12px; color: var(--text-secondary);">
                  <span>Duration: <strong>{tpl.durationMinutes} min</strong></span>
                  <span>Collision Policy: <strong>{tpl.collisionPolicy === 'reject' ? 'Strict Reject' : 'Preserve Both'}</strong></span>
                  {#if tpl.formSchema && tpl.formSchema.length > 0}
                    <span>Form Fields: <strong>{tpl.formSchema.length} configured</strong></span>
                  {/if}
                  {#if tpl.expectedParticipants && tpl.expectedParticipants.length > 0}
                    <span>Roster: <strong>{tpl.expectedParticipants.length} participants</strong></span>
                  {/if}
                </div>
              </div>
              <div class="button-row" style="margin-top: 12px;">
                <button class="button primary small" on:click={() => startFromTemplateAction(tpl.id)}>
                  Start Room
                </button>
                <button class="button secondary small" on:click={() => duplicateTemplateAction(tpl.id, tpl.name)}>
                  Duplicate
                </button>
                <button class="button ghost small" style="color: var(--danger);" on:click={() => deleteTemplateAction(tpl.id)}>
                  Delete
                </button>
              </div>
            </div>
          {/each}
        </div>
      {:else}
        <div class="empty">No saved templates yet.</div>
      {/if}
    </section>

  <!-- TAB 4: AUDIT HISTORY -->
  {:else if activeTab === 'audit'}
    <section class="panel">
      <div class="section-heading">
        <div>
          <span class="eyebrow accent">Searchable Local Audit Log</span>
          <h2>Transfer & Collection History</h2>
        </div>
        <div style="display: flex; gap: 10px; align-items: center;">
          <button class="button secondary small" on:click={loadAuditLogs} disabled={auditLoading}>
            {auditLoading ? 'Loading…' : 'Refresh'}
          </button>
          <button class="button danger small" on:click={clearAuditAction}>
            Clear History
          </button>
        </div>
      </div>
      <p class="muted">
        Append-only local JSONL audit trail. Network tokens and private transfer contents are never recorded.
      </p>

      <div style="display: flex; gap: 12px; margin: 14px 0;" class="form-input-row">
        <select class="text-input" bind:value={auditFilterType} on:change={loadAuditLogs} style="max-width: 200px;">
          <option value="">All Event Types</option>
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
        <div class="empty">Loading audit events…</div>
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
        <div class="empty">No audit events match the current filter.</div>
      {/if}
    </section>

  <!-- TAB 5: TRUSTED DEVICES -->
  {:else if activeTab === 'devices'}
    <section class="panel" aria-label="Trusted Devices">
      <div class="section-heading">
        <div>
          <span class="eyebrow">Security & Pairing</span>
          <h2>Cryptographically Trusted Devices</h2>
        </div>
        <button
          type="button"
          class="button secondary small"
          on:click={loadTrustedDevices}
          disabled={loadingDevices}
        >
          {loadingDevices ? 'Refreshing…' : '↻ Refresh'}
        </button>
      </div>
      <p class="muted">
        Devices paired via cryptographic Ed25519 keys. Paired devices can be granted automatic approval to bypass the manual transfer prompt.
      </p>

      {#if loadingDevices}
        <div class="empty">Loading paired devices…</div>
      {:else if trustedDevices.length > 0}
        <div class="table-wrap" style="margin-top: 16px;">
          <table class="data-table">
            <thead>
              <tr>
                <th>Device Name</th>
                <th>Fingerprint (ID)</th>
                <th>Paired At</th>
                <th>Last Seen</th>
                <th>Auto-Approve</th>
                <th style="text-align: right;">Actions</th>
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
                      {dev.autoApprove ? '✓ Enabled' : '○ Disabled'}
                    </button>
                  </td>
                  <td style="text-align: right;">
                    <button
                      type="button"
                      class="button ghost small"
                      style="color: var(--danger-color, #e06c75);"
                      on:click={() => removeDeviceAction(dev.id)}
                    >
                      Revoke
                    </button>
                  </td>
                </tr>
              {/each}
            </tbody>
          </table>
        </div>
      {:else}
        <div class="empty">No trusted devices paired yet. Devices paired through connection authentication will appear here.</div>
      {/if}
    </section>

  <!-- TAB 6: SETTINGS -->
  {:else if activeTab === 'settings'}
    <section class="panel" aria-label="Application Settings">
      <div class="section-heading">
        <div>
          <span class="eyebrow accent">Preferences</span>
          <h2>Application Settings</h2>
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
          <button type="button" on:click={() => settingsError = ''} aria-label="Dismiss">×</button>
        </div>
      {/if}

      <div style="display: flex; flex-direction: column; gap: 24px;">
        <!-- Download Directory -->
        <div>
          <label for="download-dir-input" class="connection-label">Receive Directory</label>
          <p style="margin: 0 0 10px; font-size: 13px; color: var(--text-secondary);">
            Incoming files and folders from paired devices and guests are saved here.
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
              Browse…
            </button>
            <button type="button" class="button ghost" on:click={handleOpenDownloadFolder}>
              Open Folder
            </button>
          </div>
          <div style="display: flex; gap: 12px; align-items: center;">
            <span style="font-size: 12px; color: var(--text-tertiary);">
              Current effective path: <code>{settingsState.effectiveDir || receiveDir}</code>
            </span>
            {#if settingsState.downloadDir}
              <button
                type="button"
                class="button ghost small"
                style="padding: 2px 8px; font-size: 11px;"
                on:click={() => settingsState.downloadDir = ''}
              >
                Reset to Default
              </button>
            {/if}
          </div>
        </div>

        <!-- Appearance / Theme -->
        <div style="border-top: 1px solid var(--line); padding-top: 20px;">
          <label for="theme-select" class="connection-label">Appearance & Theme</label>
          <p style="margin: 0 0 10px; font-size: 13px; color: var(--text-secondary);">
            Choose the color scheme for the Flitnix user interface.
          </p>
          <div style="max-width: 280px;">
            <select id="theme-select" class="text-input" bind:value={settingsState.theme} style="width: 100%;">
              <option value="system">System Default (Automatic)</option>
              <option value="dark">Dark Theme</option>
              <option value="light">Light Theme</option>
            </select>
          </div>
        </div>

        <!-- Preferred Network Interface -->
        <div style="border-top: 1px solid var(--line); padding-top: 20px;">
          <label for="iface-select" class="connection-label">Preferred Network Interface</label>
          <p style="margin: 0 0 10px; font-size: 13px; color: var(--text-secondary);">
            Choose the network adapter used for pairing and transfers, or let Flitnix select automatically.
          </p>
          <div style="max-width: 380px;">
            <select id="iface-select" class="text-input" bind:value={settingsState.preferredInterface} style="width: 100%;">
              <option value="">Automatic Selection (Default)</option>
              {#each uniqueInterfaces as iface (iface.name)}
                <option value={iface.name}>
                  {iface.name} ({iface.ip}{iface.family ? ` · ${iface.family}` : ''})
                </option>
              {/each}
              {#if settingsState.preferredInterface && !uniqueInterfaces.some(i => i.name === settingsState.preferredInterface)}
                <option value={settingsState.preferredInterface}>
                  {settingsState.preferredInterface} (Currently unavailable)
                </option>
              {/if}
            </select>
          </div>
        </div>

        <!-- Session Automation -->
        <div style="border-top: 1px solid var(--line); padding-top: 20px;">
          <span class="connection-label">Session Automation</span>
          <label style="display: flex; align-items: center; gap: 10px; cursor: pointer; margin-top: 8px;">
            <input
              type="checkbox"
              bind:checked={settingsState.autoStartSession}
              style="width: 18px; height: 18px; cursor: pointer;"
            />
            <span style="font-size: 14px; color: var(--text-main); font-weight: 600;">
              Automatically start pairing session on launch
            </span>
          </label>
          <p style="margin: 6px 0 0 28px; font-size: 13px; color: var(--text-secondary);">
            When enabled, Flitnix prepares a connection immediately when opened so devices can pair right away.
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
            {savingSettings ? 'Saving…' : 'Save Preferences'}
          </button>

          <button
            type="button"
            class="button ghost"
            style="color: var(--text-secondary);"
            on:click={handleResetSettings}
          >
            Reset to Defaults
          </button>
        </div>
      </div>
    </section>
  {/if}

  {#if receiveDir}
    <footer>Received files are saved to <code>{receiveDir}</code>. Paths stay on this computer.</footer>
  {/if}
</main>
