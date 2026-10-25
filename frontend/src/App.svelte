<script lang="ts">
  import { onMount } from 'svelte'
  import type { main } from '../wailsjs/go/models'
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
    ReceiveDirectory
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

  async function refresh(): Promise<void> {
    try {
      const [s, p, r, f, dir] = await Promise.all([
        ListSessions(),
        ListPendingUploads(),
        RecentUploads(),
        ListSharedFiles(),
        ReceiveDirectory()
      ])
      sessions = s
      pending = p
      transfers = r
      files = f
      receiveDir = dir
    } catch (err) {
      errorText = stringOfError(err)
    }
  }

  function report(err: unknown, key: string = ''): void {
    errorText = stringOfError(err)
    if (key) setBusy(key, false)
  }

  function startSession(now: boolean): void {
    if (starting) return
    starting = true
    StartSession()
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
    <h2>Sessions</h2>
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
      <div class="empty">No active session. Start one to make this desktop reachable over the local network.</div>
    {/each}
    <div class="actions">
      <button class="btn primary" on:click={() => startSession(true)} disabled={starting}>
        {starting ? 'Starting…' : 'Start session'}
      </button>
    </div>
    <div class="note">Phone pairing via QR code arrives with LAN support in the next stage.</div>
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
