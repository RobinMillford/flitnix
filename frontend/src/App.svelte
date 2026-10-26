<script lang="ts">
  import { onMount } from 'svelte'
  import type { main, netenv } from '../wailsjs/go/models'
  import { OnFileDrop, OnFileDropOff } from '../wailsjs/runtime/runtime.js'
  import {
    StartSession, EndSession, ListSessions, ListPendingUploads, RecentUploads,
    RecentDownloads, ApproveUpload, RejectUpload, CancelUpload,
    ListPendingBatches, ListBatches, ApproveBatch, RejectBatch, CancelBatch,
    PickFilesForSharing, PickFolderForSharing, AddDroppedSources,
    ListSharedFiles, ListSharedSources, RevokeSharedFile, RevokeSharedSource, ReceiveDirectory,
    ListNetworkInterfaces, SessionQR, ServerAddress
  } from '../wailsjs/go/main/App.js'

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
  const SESSION_PORT = '43821'

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
  async function refresh(): Promise<void> {
    try {
      const [s, p, u, d, f, sourceList, dir, addr, b, allBatches] = await Promise.all([
        ListSessions(), ListPendingUploads(), RecentUploads(), RecentDownloads(),
        ListSharedFiles(), ListSharedSources(), ReceiveDirectory(), ServerAddress(), ListPendingBatches(), ListBatches()
      ])
      sessions = s; pending = p; uploads = u; downloads = d; files = f; sources = sourceList; pendingBatches = b
      batches = allBatches
      receiveDir = dir; serverAddress = addr ?? ''
      if (!s.length) { qrKey = ''; qrUrl = ''; qrError = '' }
      else if (!s.some((x) => x.key === qrKey)) fetchQR(s[0].key)
    } catch (err) { errorText = stringOfError(err) }
  }
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
      interfaces = next
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
      await refresh()
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
  function formatBytes(n: number): string {
    if (n < 1024) return `${n} B`
    const units = ['KiB', 'MiB', 'GiB', 'TiB']; let value = n; let i = -1
    do { value /= 1024; i++ } while (value >= 1024 && i < units.length - 1)
    return `${value.toFixed(value >= 100 ? 0 : 1)} ${units[i]}`
  }
  function percent(done: number, total: number): number {
    if (total <= 0) return 100
    return Math.min(100, Math.max(0, Math.round((done / total) * 100)))
  }
  function speed(value: number): string {
    return value > 0 ? `${formatBytes(value)}/s` : 'Calculating speed…'
  }
  function fmtTime(ts: unknown): string {
    if (typeof ts !== 'string') return ''
    return new Date(ts).toLocaleTimeString([], { hour: '2-digit', minute: '2-digit' })
  }
  function activeUpload(u: main.UploadView): boolean {
    return ['pending', 'approved', 'uploading'].includes(u.state)
  }
  function activeDownload(d: main.DownloadView): boolean { return ['starting', 'transferring', 'finalizing'].includes(d.state) }
  // A batch is active only once it is approved and still has work to do.
  // Pending, rejected, canceled and completed batches are never "active".
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
  $: currentNetwork = interfaces.length === 1
    ? interfaces[0]
    : interfaces.find((item) => item.ip === selectedIp) ?? null

  onMount(() => {
    refresh()
    refreshNetwork()
    const id = setInterval(refresh, 800)
    OnFileDrop(handleDrop, true)
    return () => {
      clearInterval(id)
      OnFileDropOff()
    }
  })
</script>

<main class="shell">
  <header class="topbar">
    <div>
      <div class="brand"><span class="brand-mark">&gt;_</span><h1>Flitnix</h1></div>
      <p>Fast, private file transfer over your local network.</p>
    </div>
    <div class:online={sessions.length > 0} class="status">
      <span class="status-dot"></span>{sessions.length > 0 ? 'Session active' : 'Session inactive'}
    </div>
  </header>

  {#if errorText}<div class="alert" role="alert"><span>{errorText}</span><button on:click={() => errorText = ''} aria-label="Dismiss">×</button></div>{/if}

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

  <section class="panel">
    <div class="section-heading"><div><span class="eyebrow">Live telemetry</span><h2>Active transfers</h2></div><span class="pill">{uploads.filter(activeUpload).length + downloads.filter(activeDownload).length + batches.filter(activeBatch).length} active</span></div>
    {#each uploads.filter(activeUpload) as u (u.id)}
      <div class="transfer"><div class="transfer-head"><div><strong>↙ {u.name}</strong><span>Phone → computer · {formatBytes(u.uploaded)} of {formatBytes(u.size)}{#if u.state === 'uploading'} · {speed(u.speed)}{/if}</span></div><b>{u.state === 'uploading' ? `${percent(u.uploaded, u.size)}%` : u.state}</b></div>{#if u.state === 'uploading'}<div class="bar"><i style={`width:${percent(u.uploaded, u.size)}%`}></i></div>{/if}<div class="transfer-foot"><span>{u.state === 'approved' ? 'Approved — waiting for phone' : u.state === 'pending' ? 'Awaiting approval' : 'Receiving bytes'}</span>{#if ['approved', 'uploading'].includes(u.state)}<button class="text-button" on:click={() => cancelUpload(u.id)}>Cancel</button>{/if}</div></div>
    {/each}
    {#each batches.filter(activeBatch) as b (b.id)}
      <div class="transfer"><div class="transfer-head"><div><strong>↙ Batch · {batchName(b)}</strong><span>Phone → computer · {doneItems(b)} of {b.totalItemCount} files · {formatBytes(b.transferredBytes)} of {formatBytes(b.declaredTotalBytes)}</span></div><b>{percent(b.transferredBytes, b.declaredTotalBytes)}%</b></div><div class="bar"><i style={`width:${percent(b.transferredBytes, b.declaredTotalBytes)}%`}></i></div><div class="transfer-foot"><span>{currentItem(b) ? `Receiving ${currentItem(b)?.relativePath || currentItem(b)?.displayName}` : 'Approved — waiting for the phone to send the next file'}</span><button class="text-button" on:click={() => cancelBatch(b)} disabled={isBusy(`cancel-batch:${b.id}`)}>Cancel</button></div></div>
    {/each}
    {#each downloads.filter(activeDownload) as d (d.id)}
      <div class="transfer"><div class="transfer-head"><div><strong>↗ {d.name}</strong><span>Computer → phone · {d.kind === 'archive' ? 'ZIP archive' : 'Individual file'} · {formatBytes(d.written)} of {formatBytes(d.size)} · {speed(d.speed)}</span></div><b>{d.state === 'starting' || d.state === 'finalizing' ? d.state : `${percent(d.written, d.size)}%`}</b></div><div class="bar"><i style={`width:${percent(d.written, d.size)}%`}></i></div><div class="transfer-foot"><span>{d.state === 'starting' ? 'Preparing server-side transfer' : d.state === 'finalizing' ? 'Finalizing server-side transfer' : 'Sending to the phone browser'}</span><span>Browser save completion is not confirmed by the desktop.</span></div></div>
    {/each}
    {#if !uploads.some(activeUpload) && !downloads.some(activeDownload) && !batches.some(activeBatch)}<div class="empty">No active transfers. Start a download or approve an incoming file to see live progress.</div>{/if}
  </section>

  <section class="panel history">
    <div class="section-heading"><div><span class="eyebrow">History</span><h2>Recent transfers</h2></div><span class="muted">Session history</span></div>
    {#each batches.filter(finishedBatch).slice(-12).reverse() as b (b.id)}
      <div class="history-row"><span class="history-icon">↙</span><div class="grow"><strong>Batch · {batchName(b)}</strong><span>{doneItems(b)} of {b.totalItemCount} files · {formatBytes(b.transferredBytes)} of {formatBytes(b.declaredTotalBytes)} · {fmtTime(b.createdAt)}</span></div><span class="badge {b.state}">{b.state}</span></div>
    {/each}
    {#each [...uploads.filter((u) => !activeUpload(u)), ...downloads.filter((d) => !activeDownload(d))].slice(-12).reverse() as item (item.id)}
      <div class="history-row"><span class="history-icon">{'fileId' in item ? '↗' : '↙'}</span><div class="grow"><strong>{item.name}</strong><span>{'fileId' in item ? `${item.kind === 'archive' ? 'ZIP archive' : 'Individual file'} · ` : ''}{formatBytes('fileId' in item ? item.written : item.uploaded)} of {formatBytes(item.size)} · {fmtTime(item.created)}</span></div><span class="badge {item.state}">{item.state === 'sent' ? 'sent to browser' : item.state}</span></div>
    {/each}
    {#if !batches.some(finishedBatch) && !uploads.some((u) => !activeUpload(u)) && !downloads.some((d) => !activeDownload(d))}<div class="empty">No completed transfers in this session.</div>{/if}
  </section>

  {#if receiveDir}<footer>Received files are saved to <code>{receiveDir}</code>. Paths stay on this computer.</footer>{/if}
</main>
