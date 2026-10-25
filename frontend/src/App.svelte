<script lang="ts">
  import { onMount } from 'svelte'
  import type { main, netenv } from '../wailsjs/go/models'
  import {
    StartSession, EndSession, ListSessions, ListPendingUploads, RecentUploads,
    RecentDownloads, ApproveUpload, RejectUpload, CancelUpload,
    PickFilesForSharing, ListSharedFiles, RevokeSharedFile, ReceiveDirectory,
    ListNetworkInterfaces, SessionQR, ServerAddress
  } from '../wailsjs/go/main/App.js'

  let sessions: main.SessionView[] = []
  let pending: main.UploadView[] = []
  let uploads: main.UploadView[] = []
  let downloads: main.DownloadView[] = []
  let files: main.SharedFileView[] = []
  let receiveDir = ''
  let errorText = ''
  let starting = false
  let picking = false
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
      const [s, p, u, d, f, dir, addr] = await Promise.all([
        ListSessions(), ListPendingUploads(), RecentUploads(), RecentDownloads(),
        ListSharedFiles(), ReceiveDirectory(), ServerAddress()
      ])
      sessions = s; pending = p; uploads = u; downloads = d; files = f
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
  async function loadInterfaces(): Promise<void> {
    loadingIfaces = true
    try {
      interfaces = await ListNetworkInterfaces()
      ifaceError = ''
      selectedIp = interfaces.length === 1 ? interfaces[0].ip : ''
    } catch (err) {
      interfaces = []; selectedIp = ''; ifaceError = stringOfError(err)
    } finally { loadingIfaces = false }
  }
  function startSession(): void {
    if (starting) return
    if (interfaces.length > 1 && !selectedIp) {
      errorText = 'Choose the network interface that connects to the phone, then start.'
      return
    }
    starting = true
    StartSession(selectedIp).then((v) => {
      activeInterfaceName = v.interfaceName ?? ''; activeAddress = v.address ?? ''
      return fetchQR(v.key)
    }).then(refresh).catch(report).finally(() => { starting = false })
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
  function cancelUpload(id: string): void {
    const k = `cancel:${id}`; if (isBusy(k)) return
    setBusy(k, true); CancelUpload(id).then(refresh).catch((e) => report(e, k)).finally(() => setBusy(k, false))
  }
  function pickFiles(): void {
    if (picking) return
    picking = true; PickFilesForSharing().then(refresh).catch(report).finally(() => { picking = false })
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
  function activeDownload(d: main.DownloadView): boolean { return d.state === 'transferring' }

  onMount(() => {
    refresh(); loadInterfaces()
    const id = setInterval(refresh, 800)
    return () => clearInterval(id)
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
    <div class="section-heading"><div><span class="eyebrow">Connection</span><h2>Pair a phone</h2></div><span class="pill">{sessions.length} active</span></div>
    <div class="connection-grid">
      <div>
        <label for="interface">Network interface</label>
        <div class="control-row">
          {#if interfaces.length}
            <select id="interface" bind:value={selectedIp} aria-label="Network interface">
              {#if interfaces.length > 1}<option value="" disabled>Choose one…</option>{/if}
              {#each interfaces as itf (itf.ip)}<option value={itf.ip}>{itf.name} · {itf.ip}</option>{/each}
            </select>
          {:else}<span class="muted">{loadingIfaces ? 'Scanning…' : 'No usable network found'}</span>{/if}
          <button class="button ghost" on:click={loadInterfaces} disabled={loadingIfaces}>Rescan</button>
        </div>
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
    <section class="panel workflow">
      <div class="section-heading"><div><span class="eyebrow accent">Send to phone</span><h2>Shared files</h2></div><span class="count">{files.length}</span></div>
      <p class="muted">Files listed here are available for download from the phone.</p>
      {#each files as f (f.id)}
        <div class="file-row"><div class="file-icon">↗</div><div class="grow"><strong>{f.name}</strong><span>{formatBytes(f.size)}</span></div><button class="button danger small" on:click={() => revoke(f.id)} disabled={isBusy(`revoke:${f.id}`)}>Remove</button></div>
      {:else}<div class="empty">No files shared yet. Choose files to make them available.</div>{/each}
      <button class="button secondary" on:click={pickFiles} disabled={picking}>{picking ? 'Opening picker…' : '+ Choose files'}</button>
    </section>

    <section class="panel workflow">
      <div class="section-heading"><div><span class="eyebrow blue">Receive from phone</span><h2>Approval inbox</h2></div><span class="count">{pending.length}</span></div>
      <p class="muted">Review each request before any file contents reach this computer.</p>
      {#each pending as u (u.id)}
        <div class="request-row"><div class="file-icon">↙</div><div class="grow"><strong>{u.name}</strong><span>{formatBytes(u.size)} · waiting for approval</span></div><div class="button-row"><button class="button primary small" on:click={() => decide(u, true)} disabled={isBusy(`approve:${u.id}`)}>Allow</button><button class="button danger small" on:click={() => decide(u, false)} disabled={isBusy(`reject:${u.id}`)}>Deny</button></div></div>
      {:else}<div class="empty">No files are waiting for approval.</div>{/each}
    </section>
  </div>

  <section class="panel">
    <div class="section-heading"><div><span class="eyebrow">Live telemetry</span><h2>Active transfers</h2></div><span class="pill">{uploads.filter(activeUpload).length + downloads.filter(activeDownload).length} active</span></div>
    {#each uploads.filter(activeUpload) as u (u.id)}
      <div class="transfer"><div class="transfer-head"><div><strong>↙ {u.name}</strong><span>Phone → computer · {formatBytes(u.uploaded)} of {formatBytes(u.size)}{#if u.state === 'uploading'} · {speed(u.speed)}{/if}</span></div><b>{u.state === 'uploading' ? `${percent(u.uploaded, u.size)}%` : u.state}</b></div>{#if u.state === 'uploading'}<div class="bar"><i style={`width:${percent(u.uploaded, u.size)}%`}></i></div>{/if}<div class="transfer-foot"><span>{u.state === 'approved' ? 'Approved — waiting for phone' : u.state === 'pending' ? 'Awaiting approval' : 'Receiving bytes'}</span>{#if ['approved', 'uploading'].includes(u.state)}<button class="text-button" on:click={() => cancelUpload(u.id)}>Cancel</button>{/if}</div></div>
    {/each}
    {#each downloads.filter(activeDownload) as d (d.id)}
      <div class="transfer"><div class="transfer-head"><div><strong>↗ {d.name}</strong><span>Computer → phone · {formatBytes(d.written)} of {formatBytes(d.size)} · {speed(d.speed)}</span></div><b>{percent(d.written, d.size)}%</b></div><div class="bar"><i style={`width:${percent(d.written, d.size)}%`}></i></div><div class="transfer-foot"><span>Sending to the phone browser</span><span>Download completion is confirmed by the browser.</span></div></div>
    {/each}
    {#if !uploads.some(activeUpload) && !downloads.some(activeDownload)}<div class="empty">No active transfers. Start a download or approve an incoming file to see live progress.</div>{/if}
  </section>

  <section class="panel history">
    <div class="section-heading"><div><span class="eyebrow">History</span><h2>Recent transfers</h2></div><span class="muted">Session history</span></div>
    {#each [...uploads.filter((u) => !activeUpload(u)), ...downloads.filter((d) => !activeDownload(d))].slice(-12).reverse() as item (item.id)}
      <div class="history-row"><span class="history-icon">{'fileId' in item ? '↗' : '↙'}</span><div class="grow"><strong>{item.name}</strong><span>{formatBytes('fileId' in item ? item.written : item.uploaded)} of {formatBytes(item.size)} · {fmtTime(item.created)}</span></div><span class="badge {item.state}">{item.state === 'sent' ? 'sent to browser' : item.state}</span></div>
    {:else}<div class="empty">No completed transfers in this session.</div>{/each}
  </section>

  {#if receiveDir}<footer>Received files are saved to <code>{receiveDir}</code>. Paths stay on this computer.</footer>{/if}
</main>
