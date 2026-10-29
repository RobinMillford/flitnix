<script lang="ts">
  import { onMount } from 'svelte'
  import type { main, netenv, doctor } from '../wailsjs/go/models'
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
    StartGuestRoom, GetGuestRoomState, ApproveGuest, RejectGuest, RevokeGuest,
    ApproveGuestUpload, RejectGuestUpload, EndGuestRoom, PickGuestDestination, OpenGuestFolder,
    RunConnectionDiagnostics, SendTextMessage, ListTextMessages, ClearTextMessages
  } from '../wailsjs/go/main/App.js'

  let activeTab: 'personal' | 'guest' = 'personal'

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

  // Guest Drop Box state
  let guestRoomState: main.GuestRoomStateView | null = null
  let guestStarting = false
  let guestDestDir = ''
  let guestDurationMinutes = 60
  let guestCopiedLink = false

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
    return serverAddress || (activeAddress ? `${activeAddress}:${SESSION_PORT}` : `…:${SESSION_PORT}`)
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

  async function startGuestRoomAction(): Promise<void> {
    if (guestStarting) return
    guestStarting = true
    try {
      await StartGuestRoom(guestDestDir, guestDurationMinutes)
      refresh()
    } catch (err) {
      report(err)
    } finally {
      guestStarting = false
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

  onMount(() => {
    refresh()
    refreshNetwork()
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
  <nav class="nav-bar">
    <div class="tab-list">
      <button
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
        class="tab-btn"
        class:active={activeTab === 'guest'}
        on:click={() => activeTab = 'guest'}
      >
        <span>Guest Drop Box</span>
        {#if guestPendingCount > 0}
          <span class="tab-badge">{guestPendingCount}</span>
        {/if}
      </button>
    </div>

    <button class="doctor-launch-btn" on:click={openDoctor}>
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
            <button class="button ghost small" on:click={regenerateQRAction} disabled={!sessions.length}>
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
                {#each interfaces as itf (itf.ip)}<option value={itf.ip}>{itf.friendlyName || `${itf.type || 'Network'} · Name unavailable`} · {itf.type || 'Network'}</option>{/each}
              </select>
              <button class="button ghost" on:click={refreshNetwork} disabled={loadingIfaces}>{loadingIfaces ? 'Refreshing…' : 'Rescan'}</button>
            </div>
          {/if}
          {#if currentNetwork}
            <details class="network-details">
              <summary>Network details</summary>
              <dl><div><dt>Interface</dt><dd>{currentNetwork.name || 'Unavailable'}</dd></div><div><dt>IPv4 address</dt><dd>{currentNetwork.ip || 'Unavailable'}</dd></div><div><dt>Connection type</dt><dd>{currentNetwork.type || 'Network'}</dd></div><div><dt>Profile name</dt><dd>{currentNetwork.connectionName || 'Unavailable'}</dd></div><div><dt>Name source</dt><dd>{currentNetwork.friendlyNameSource || 'Unavailable'}</dd></div><div><dt>Metadata</dt><dd>{currentNetwork.metadataAvailable ? 'Available' : 'Name unavailable'}</dd></div></dl>
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
    {#if !guestRoomState?.room || guestRoomState.room.state === 'Ended'}
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
            <div class="guest-presets">
              <button class="guest-preset-btn" class:active={guestDurationMinutes === 15} on:click={() => guestDurationMinutes = 15}>15 Minutes</button>
              <button class="guest-preset-btn" class:active={guestDurationMinutes === 30} on:click={() => guestDurationMinutes = 30}>30 Minutes</button>
              <button class="guest-preset-btn" class:active={guestDurationMinutes === 60} on:click={() => guestDurationMinutes = 60}>1 Hour (Standard)</button>
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
            <h2>Guest Drop Box Dashboard</h2>
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

        <div class="guest-room-active-grid">
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

      <!-- Guest Approval Queues Grid -->
      <div class="guest-subsections">
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
  {/if}

  {#if receiveDir}
    <footer>Received files are saved to <code>{receiveDir}</code>. Paths stay on this computer.</footer>
  {/if}
</main>
