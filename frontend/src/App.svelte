<script lang="ts">
  import { onMount } from 'svelte'
  import type { main, netenv } from '../wailsjs/go/models'
  import {
    StartSession,
    EndSession,
    ListSessions,
    ListPendingUploads,
    RecentUploads,
    ApproveUpload,
    RejectUpload,
    CancelUpload,
    PickFilesForSharing,
    ListSharedFiles,
    RevokeSharedFile,
    ReceiveDirectory,
    ListNetworkInterfaces,
    SessionQR,
    ServerAddress
  } from '../wailsjs/go/main/App.js'

  let sessions: main.SessionView[] = []
  let pending: main.UploadView[] = []
  let transfers: main.UploadView[] = []
  let files: main.SharedFileView[] = []
  let receiveDir = ''
  let errorText = ''
  let starting = false
  let picking = false
  // In-flight action keys ("approve:<id>", "end:<key>", ...) used to
  // disable the buttons whose request is running.
  let busy: Record<string, boolean> = {}

  // Stage 4 live connection state.
  let interfaces: netenv.View[] = []
  let loadingIfaces = false
  let ifaceError = ''
  // '' means "let the desktop pick the default" — only offered when a
  // single interface exists; several interfaces demand an explicit choice.
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

  function isBusy(key: string): boolean {
    return !!busy[key]
  }

  function hostPort(): string {
    if (serverAddress) return serverAddress
    if (activeAddress) return `${activeAddress}:${SESSION_PORT}`
    return `…:${SESSION_PORT}`
  }

  async function refresh(): Promise<void> {
    try {
      const [s, p, r, f, dir, addr] = await Promise.all([
        ListSessions(),
        ListPendingUploads(),
        RecentUploads(),
        ListSharedFiles(),
        ReceiveDirectory(),
        ServerAddress()
      ])
      sessions = s
      pending = p
      transfers = r
      files = f
      receiveDir = dir
      serverAddress = addr ?? ''
      if (s.length === 0) {
        if (qrKey || qrUrl || qrError) {
          qrKey = ''
          qrUrl = ''
          qrError = ''
        }
      } else if (!s.some((x) => x.key === qrKey)) {
        // First session (or a page reload): fetch the pairing QR only once
        // the server is confirmed running.
        fetchQR(s[0].key)
      }
    } catch (err) {
      errorText = stringOfError(err)
    }
  }

  async function fetchQR(key: string): Promise<void> {
    if (!key) return
    qrKey = key
    try {
      const data = await SessionQR(key)
      if (qrKey === key) {
        qrUrl = data
        qrError = ''
      }
    } catch (err) {
      if (qrKey === key) {
        qrUrl = ''
        qrError = stringOfError(err)
      }
    }
  }

  async function loadInterfaces(): Promise<void> {
    loadingIfaces = true
    try {
      const views = await ListNetworkInterfaces()
      interfaces = views
      ifaceError = ''
      // One usable interface preselects itself; several demand an
      // explicit user choice (the select stays unset).
      selectedIp = views.length === 1 ? views[0].ip : ''
    } catch (err) {
      interfaces = []
      selectedIp = ''
      ifaceError = stringOfError(err)
    } finally {
      loadingIfaces = false
    }
  }

  function report(err: unknown, key: string = ''): void {
    errorText = stringOfError(err)
    if (key) setBusy(key, false)
  }

  function startSession(now: boolean): void {
    if (starting) return
    if (interfaces.length > 1 && !selectedIp) {
      errorText = 'Choose the network interface that connects to the phone, then start.'
      return
    }
    starting = true
    StartSession(selectedIp)
      .then((v) => {
        activeInterfaceName = v.interfaceName ?? ''
        activeAddress = v.address ?? ''
        return fetchQR(v.key)
      })
      .then(refresh)
      .catch((err) => report(err))
      .finally(() => {
        starting = false
        if (now) refresh()
      })
  }

  function endSession(key: string): void {
    const k = `end:${key}`
    if (isBusy(k)) return
    setBusy(k, true)
    EndSession(key)
      .then(refresh)
      .catch((err) => report(err, k))
      .finally(() => setBusy(k, false))
  }

  function decide(u: main.UploadView, approve: boolean): void {
    const k = `${approve ? 'approve' : 'reject'}:${u.id}`
    if (isBusy(k)) return
    setBusy(k, true)
    const call = approve ? ApproveUpload(u.id) : RejectUpload(u.id)
    call
      .then(refresh)
      .catch((err) => report(err, k))
      .finally(() => setBusy(k, false))
  }

  function cancelUpload(id: string): void {
    const k = `cancel:${id}`
    if (isBusy(k)) return
    setBusy(k, true)
    CancelUpload(id)
      .then(refresh)
      .catch((err) => report(err, k))
      .finally(() => setBusy(k, false))
  }

  function pickFiles(): void {
    if (picking) return
    picking = true
    PickFilesForSharing()
      .then(refresh)
      .catch((err) => report(err))
      .finally(() => {
        picking = false
        refresh()
      })
  }

  function revoke(id: string): void {
    const k = `revoke:${id}`
    if (isBusy(k)) return
    setBusy(k, true)
    RevokeSharedFile(id)
      .then(refresh)
      .catch((err) => report(err, k))
      .finally(() => setBusy(k, false))
  }

  function formatBytes(n: number): string {
    if (n < 1024) return `${n} B`
    const units = ['KiB', 'MiB', 'GiB', 'TiB']
    let value = n
    let i = -1
    do {
      value /= 1024
      i++
    } while (value >= 1024 && i < units.length - 1)
    return `${value.toFixed(value >= 100 ? 0 : 1)} ${units[i]}`
  }

  function fmtTime(ts: unknown): string {
    if (typeof ts !== 'string') return ''
    const d = new Date(ts)
    return d.toLocaleTimeString([], { hour: '2-digit', minute: '2-digit' })
  }

  function percent(u: main.UploadView): number {
    if (u.size <= 0) return 0
    return Math.min(100, Math.round((u.uploaded / u.size) * 100))
  }

  function isCancelable(u: main.UploadView): boolean {
    return u.state === 'approved' || u.state === 'uploading'
  }

  onMount(() => {
    refresh()
    loadInterfaces()
    const id = setInterval(refresh, 800)
    return () => clearInterval(id)
  })
</script>

<main class="wrap">
  <header>
    <h1>Flitnix</h1>
    <div class="sub">Send files between this desktop and your phone's browser — local only, no cloud.</div>
  </header>

  {#if errorText}
    <div class="banner" role="alert">
      <span>{errorText}</span>
      <button class="banner-close" on:click={() => (errorText = '')}>✕</button>
    </div>
  {/if}

  <section class="card">
    <h2>Connection</h2>

    <div class="row">
      <div class="grow">
        <div class="name">Network interface</div>
        <div class="meta">The network this laptop will share with the phone. Verify before starting.</div>
      </div>
      {#if interfaces.length > 0}
        <select class="select" bind:value={selectedIp} aria-label="Network interface">
          {#if interfaces.length > 1}
            <option value="" disabled selected>Choose one…</option>
          {/if}
          {#each interfaces as itf (itf.ip)}
            <option value={itf.ip}>{itf.name} — {itf.ip}</option>
          {/each}
        </select>
      {:else}
        <span class="meta">{loadingIfaces ? 'Scanning…' : 'No usable network found'}</span>
      {/if}
      <button class="btn" on:click={() => loadInterfaces()} disabled={loadingIfaces}>
        {loadingIfaces ? '…' : 'Rescan'}
      </button>
    </div>
    {#if ifaceError}
      <div class="warning">{ifaceError}</div>
    {/if}
    {#if interfaces.length === 0 && !loadingIfaces && !ifaceError}
      <div class="warning">Connect this laptop to the phone's hotspot (or another local network), then Rescan.</div>
    {/if}

    {#if sessions.length > 0}
      {#if qrUrl}
        <div class="qr-row">
          <img class="qr" src={qrUrl} alt="Pairing QR code — scan it with the phone's camera" />
          <div class="grow">
            <div class="chips">
              <span class="chip ok">● {activeInterfaceName || 'running'}</span>
              <span class="chip">{hostPort()}</span>
            </div>
            <div class="url"><code>http://{hostPort()}/</code></div>
            <div class="meta">Scan to open the transfer page on the phone. Every incoming file asks for approval here first.</div>
            <div class="actions">
              <button class="btn" on:click={() => fetchQR(qrKey)} disabled={!qrKey}>Regenerate QR</button>
              <button class="btn reject" on:click={() => endSession(qrKey)} disabled={isBusy(`end:${qrKey}`)}>
                {isBusy(`end:${qrKey}`) ? 'Stopping…' : 'Stop session'}
              </button>
            </div>
          </div>
        </div>
      {:else}
        <div class="row">
          <div class="grow">
            <div class="meta">{qrError || 'Preparing the QR code…'}</div>
          </div>
          <button class="btn" on:click={() => fetchQR(qrKey)} disabled={!qrKey}>Retry QR</button>
        </div>
      {/if}
    {/if}

    {#each sessions as s (s.key)}
      <div class="row">
        <div class="grow">
          <div class="name">Session {s.key.slice(0, 8)}…</div>
          <div class="meta">
            since {fmtTime(s.created)} · idle-expires {fmtTime(s.expiresAt)}{#if s.pinnedTransfers > 0}
              · {s.pinnedTransfers} transfer{s.pinnedTransfers > 1 ? 's' : ''} running (kept alive){/if}
          </div>
        </div>
        <button class="btn" on:click={() => endSession(s.key)} disabled={isBusy(`end:${s.key}`)}>
          {isBusy(`end:${s.key}`) ? 'Ending…' : 'End'}
        </button>
      </div>
    {:else}
      <div class="empty">No active session. Start one after picking the network interface.</div>
    {/each}

    <div class="actions">
      <button
        class="btn primary"
        on:click={() => startSession(true)}
        disabled={starting || (interfaces.length > 1 && !selectedIp)}
      >
        {starting ? 'Starting…' : 'Start session'}
      </button>
    </div>

    <div class="warning">
      ✋ Transfers use plain HTTP inside your own hotspot or LAN — connect only devices you control, and stop the session when done.
    </div>
  </section>

  <section class="card">
    <h2>Incoming files</h2>
    {#each pending as u (u.id)}
      <div class="row">
        <div class="grow">
          <div class="name">{u.name}</div>
          <div class="meta">{formatBytes(u.size)} · wants to save into the receive folder</div>
        </div>
        <div class="btn-group">
          <button class="btn approve" on:click={() => decide(u, true)} disabled={isBusy(`approve:${u.id}`)}>
            {isBusy(`approve:${u.id}`) ? '…' : 'Allow'}
          </button>
          <button class="btn reject" on:click={() => decide(u, false)} disabled={isBusy(`reject:${u.id}`)}>
            {isBusy(`reject:${u.id}`) ? '…' : 'Deny'}
          </button>
        </div>
      </div>
    {:else}
      <div class="empty">Nothing is waiting for approval.</div>
    {/each}
  </section>

  <section class="card">
    <h2>Transfers</h2>
    {#each transfers.filter((u) => u.state !== 'pending') as u (u.id)}
      <div class="row">
        <div class="grow">
          <div class="name">{u.name}</div>
          <div class="meta">
            {formatBytes(u.size)}{#if u.state === 'uploading'} · {formatBytes(u.uploaded)} received ({percent(u)}%){/if}{#if u.reason}
              · {u.reason}{/if}
          </div>
          {#if u.state === 'uploading'}
            <div class="bar"><div class="fill" style="width:{percent(u)}%"></div></div>
          {/if}
          {#if u.state === 'approved'}
            <div class="meta">waiting for the phone to start sending…</div>
          {/if}
        </div>
        <span class="badge {u.state}">{u.state}</span>
        {#if isCancelable(u)}
          <button class="btn reject" on:click={() => cancelUpload(u.id)} disabled={isBusy(`cancel:${u.id}`)}>
            {isBusy(`cancel:${u.id}`) ? '…' : 'Cancel'}
          </button>
        {/if}
      </div>
    {:else}
      <div class="empty">No transfers yet.</div>
    {/each}
  </section>

  <section class="card">
    <h2>Shared files</h2>
    {#each files as f (f.id)}
      <div class="row">
        <div class="grow">
          <div class="name">{f.name}</div>
          <div class="meta">{formatBytes(f.size)}</div>
        </div>
        <button class="btn reject" on:click={() => revoke(f.id)} disabled={isBusy(`revoke:${f.id}`)}>
          {isBusy(`revoke:${f.id}`) ? '…' : 'Remove'}
        </button>
      </div>
    {:else}
      <div class="empty">No files shared yet.</div>
    {/each}
    <div class="actions">
      <button class="btn" on:click={() => pickFiles()} disabled={picking}>
        {picking ? 'Picking…' : 'Choose files…'}
      </button>
    </div>
  </section>

  {#if receiveDir}
    <footer class="foot">Received files are saved to <code>{receiveDir}</code></footer>
  {/if}
</main>

<style>
  .wrap {
    max-width: 660px;
    margin: 0 auto;
    padding: 32px 20px 48px;
  }

  header {
    margin-bottom: 8px;
  }

  h1 {
    font-size: 28px;
    margin: 0 0 6px;
  }

  .sub {
    opacity: 0.65;
    font-size: 14px;
  }

  .card {
    background: rgba(255, 255, 255, 0.04);
    border: 1px solid rgba(255, 255, 255, 0.08);
    border-radius: 10px;
    padding: 14px 18px 16px;
    margin: 16px 0;
    text-align: left;
  }

  .card h2 {
    font-size: 14px;
    letter-spacing: 0.08em;
    text-transform: uppercase;
    opacity: 0.6;
    margin: 2px 0 10px;
  }

  .row {
    display: flex;
    align-items: center;
    gap: 10px;
    padding: 8px 0;
    border-top: 1px solid rgba(255, 255, 255, 0.06);
  }

  .row:first-of-type {
    border-top: none;
  }

  .grow {
    flex: 1;
    min-width: 0;
  }

  .name {
    font-weight: 600;
    overflow-wrap: anywhere;
  }

  .meta {
    font-size: 12px;
    opacity: 0.65;
    margin-top: 2px;
    overflow-wrap: anywhere;
  }

  .empty {
    font-size: 13px;
    opacity: 0.55;
    padding: 6px 0;
  }

  .actions {
    margin-top: 10px;
    display: flex;
    gap: 10px;
    flex-wrap: wrap;
  }

  .btn {
    background: rgba(255, 255, 255, 0.08);
    color: inherit;
    border: 1px solid rgba(255, 255, 255, 0.15);
    border-radius: 6px;
    padding: 6px 12px;
    font-size: 13px;
    cursor: pointer;
    white-space: nowrap;
  }

  .btn:hover:not(:disabled) {
    background: rgba(255, 255, 255, 0.14);
  }

  .btn:disabled {
    opacity: 0.45;
    cursor: default;
  }

  .primary {
    background: #61c191;
    border-color: #61c191;
    color: #0f2a1d;
    font-weight: 700;
  }

  .primary:hover:not(:disabled) {
    background: #6fd0a0;
  }

  .approve {
    background: #4e7bd2;
    border-color: #4e7bd2;
  }

  .reject {
    background: rgba(190, 62, 62, 0.2);
    border-color: rgba(190, 62, 62, 0.55);
  }

  .btn-group {
    display: flex;
    gap: 6px;
  }

  .select {
    background: rgba(255, 255, 255, 0.08);
    color: inherit;
    border: 1px solid rgba(255, 255, 255, 0.15);
    border-radius: 6px;
    padding: 6px 8px;
    font-size: 13px;
    max-width: 220px;
  }

  .select option {
    background: #1b2330;
    color: inherit;
  }

  .qr-row {
    display: flex;
    gap: 14px;
    align-items: flex-start;
    padding: 10px 0 4px;
  }

  .qr {
    width: 160px;
    height: 160px;
    flex: none;
    border-radius: 8px;
    background: #ffffff;
    padding: 8px;
  }

  .chips {
    display: flex;
    flex-wrap: wrap;
    gap: 6px;
    margin-bottom: 4px;
  }

  .chip {
    font-size: 11px;
    padding: 2px 8px;
    border-radius: 999px;
    background: rgba(255, 255, 255, 0.09);
    border: 1px solid rgba(255, 255, 255, 0.12);
    max-width: 100%;
    overflow: hidden;
    text-overflow: ellipsis;
  }

  .chip.ok {
    background: rgba(97, 193, 145, 0.18);
    border-color: rgba(97, 193, 145, 0.4);
    color: #8fe0b4;
  }

  .url {
    margin: 2px 0 4px;
  }

  .warning {
    font-size: 12px;
    margin-top: 10px;
    padding: 7px 10px;
    border-radius: 6px;
    background: rgba(240, 180, 41, 0.12);
    border: 1px solid rgba(240, 180, 41, 0.4);
    overflow-wrap: anywhere;
  }

  .badge {
    font-size: 11px;
    padding: 3px 8px;
    border-radius: 999px;
    letter-spacing: 0.04em;
    white-space: nowrap;
  }

  .badge.pending,
  .badge.approved {
    background: rgba(78, 123, 210, 0.25);
    color: #9dbdf5;
  }

  .badge.uploading {
    background: rgba(97, 193, 145, 0.2);
    color: #8fe0b4;
  }

  .badge.rejected,
  .badge.failed {
    background: rgba(190, 62, 62, 0.25);
    color: #f0a3a3;
  }

  .badge.completed {
    background: rgba(97, 193, 145, 0.25);
    color: #b9ecd0;
  }

  .badge.canceled {
    background: rgba(255, 255, 255, 0.1);
    color: rgba(255, 255, 255, 0.55);
  }

  .bar {
    height: 4px;
    border-radius: 2px;
    background: rgba(255, 255, 255, 0.09);
    margin-top: 6px;
    overflow: hidden;
  }

  .fill {
    height: 100%;
    background: #61c191;
    transition: width 0.2s ease;
  }

  .banner {
    display: flex;
    align-items: center;
    gap: 10px;
    text-align: left;
    background: rgba(190, 62, 62, 0.22);
    border: 1px solid rgba(190, 62, 62, 0.5);
    border-radius: 8px;
    padding: 8px 12px;
    margin: 12px 0;
    font-size: 13px;
  }

  .banner span {
    flex: 1;
    overflow-wrap: anywhere;
  }

  .banner-close {
    background: none;
    color: inherit;
    border: none;
    cursor: pointer;
    font-size: 13px;
    opacity: 0.75;
  }

  .note {
    font-size: 12px;
    opacity: 0.5;
    margin-top: 8px;
  }

  .foot {
    font-size: 12px;
    opacity: 0.5;
    margin-top: 18px;
  }

  code {
    background: rgba(255, 255, 255, 0.07);
    border-radius: 4px;
    padding: 1px 5px;
    font-size: 11px;
  }
</style>
