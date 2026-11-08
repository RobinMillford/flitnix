<script lang="ts">
  import type { main, netenv } from '../../wailsjs/go/models'
  import { formatBytes, percent, speed, calcEta, fmtTime, openExternalURL } from '../utils'
  import ProgressBar from '../components/ProgressBar.svelte'

  export let sessions: main.SessionView[] = []
  export let pending: main.UploadView[] = []
  export let pendingBatches: main.BatchView[] = []
  export let batches: main.BatchView[] = []
  export let uploads: main.UploadView[] = []
  export let downloads: main.DownloadView[] = []
  export let files: main.SharedFileView[] = []
  export let sources: main.SharedSourceView[] = []
  export let interfaces: netenv.View[] = []
  export let networkState: netenv.NetworkState | null = null
  export let refreshingNetwork: boolean = false
  export let onRefreshNetwork: () => void = () => {}
  export let selectedIp: string = ''
  export let serverAddress: string = ''
  export let qrUrl: string = ''
  export let qrError: string = ''
  export let qrKey: string = ''
  export let starting: boolean = false
  export let picking: boolean = false
  export let pickingFolder: boolean = false
  export let textMessages: main.TextMessageView[] = []
  export let textInput: string = ''
  export let sendingText: boolean = false
  export let isBusy: (key: string) => boolean = () => false

  export let startSession: () => void = () => {}
  export let endSession: (key: string) => void = () => {}
  export let fetchQR: (key: string) => void = () => {}
  export let pickFiles: () => void = () => {}
  export let pickFolder: () => void = () => {}
  export let revoke: (id: string) => void = () => {}
  export let revokeSource: (id: string) => void = () => {}
  export let clearAllShared: () => void = () => {}
  export let decide: (u: main.UploadView, approve: boolean) => void = () => {}
  export let decideBatch: (b: main.BatchView, approve: boolean) => void = () => {}
  export let cancelUpload: (id: string) => void = () => {}
  export let cancelBatch: (b: main.BatchView) => void = () => {}
  export let sendTextAction: () => void = () => {}
  export let clearTextAction: () => void = () => {}
  export let copyTextToClipboard: (text: string) => void = () => {}
  export let showToast: (msg: string, type?: any) => void = () => {}
  export let handleOpenDownloadFolder: () => void = () => {}
  export let goToHistory: () => void = () => {}

  let networkDetailsOpen: boolean = false

  $: currentNetwork = interfaces.length === 1
    ? interfaces[0]
    : interfaces.find((item) => item.ip === selectedIp) ?? (interfaces[0] || null)

  $: totalSharedFiles = files.length + sources.reduce((acc, s) => acc + (s.itemCount || 1), 0)
  $: totalSharedBytes = files.reduce((acc, f) => acc + (f.size || 0), 0) + sources.reduce((acc, s) => acc + (s.totalBytes || 0), 0)

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
  function currentItem(b: main.BatchView): main.BatchItemView | undefined {
    return b.items.find((item) => item.state === 'transferring')
  }
  function doneItems(b: main.BatchView): number {
    return b.items.filter((item) => item.state === 'completed').length
  }

  function getExt(name: string): string {
    const parts = name.split('.')
    return parts.length > 1 ? parts.pop()?.toUpperCase() || 'FILE' : 'FILE'
  }

  function copySessionUrl() {
    if (!serverAddress) return
    const url = serverAddress.startsWith('http') ? serverAddress : `http://${serverAddress}`
    copyTextToClipboard(url)
    showToast('Copied connection address to clipboard', 'success')
  }
</script>

<div class="personal-view-container">
  <!-- 1. HERO CONNECTION HUB -->
  <section class="connection-hub-card" class:active-session={sessions.length > 0}>
    {#if sessions.length === 0}
      <!-- INACTIVE: READY TO CONNECT -->
      <div class="hub-inactive-grid">
        <div class="hub-info-col">
          <div class="hub-pill-row">
            {#if refreshingNetwork}
              <span class="hub-status-chip scanning">
                <span class="chip-spinner"></span>
                Detecting network…
              </span>
            {:else if networkState?.status === 'no_network' || interfaces.length === 0}
              <span class="hub-status-chip disconnected">
                <span class="chip-pulse off"></span>
                No Network Connection
              </span>
            {:else if networkState?.status === 'selected_unavailable'}
              <span class="hub-status-chip warning">
                <span class="chip-pulse warn"></span>
                Preferred Interface Unavailable
              </span>
            {:else}
              <span class="hub-status-chip ready">
                <span class="chip-pulse"></span>
                Ready to Pair
              </span>
            {/if}

            {#if currentNetwork && interfaces.length > 0 && networkState?.status !== 'no_network'}
              <span class="hub-network-chip">
                <svg width="14" height="14" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2">
                  <path d="M5 12.55a11 11 0 0 1 14.08 0"/>
                  <path d="M1.42 9a16 16 0 0 1 21.16 0"/>
                  <path d="M8.53 16.11a6 6 0 0 1 6.95 0"/>
                  <line x1="12" y1="20" x2="12.01" y2="20"/>
                </svg>
                {currentNetwork.friendlyName || currentNetwork.name || 'Wi-Fi'} ({currentNetwork.ip})
              </span>
            {/if}

            <button
              type="button"
              class="hub-refresh-btn"
              class:spinning={refreshingNetwork}
              on:click={onRefreshNetwork}
              disabled={refreshingNetwork}
              title="Refresh network status"
              aria-label="Refresh network status"
            >
              <svg width="14" height="14" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2">
                <path d="M21.5 2v6h-6M21.34 15.57a10 10 0 1 1-.57-8.38l5.67-5.67"/>
              </svg>
            </button>
          </div>

          {#if networkState?.status === 'selected_unavailable' && networkState?.error}
            <div class="hub-fallback-hint">
              {networkState.error}
            </div>
          {/if}

          <h2 class="hub-heading">Fast, Private Local File Transfer</h2>
          {#if networkState?.status === 'no_network' || interfaces.length === 0}
            <p class="hub-desc" style="color: var(--danger-text);">
              Flitnix requires a usable local network interface (such as Wi-Fi or Ethernet) to transfer files directly to your phone. Connect this computer to a local network and refresh.
            </p>
          {:else}
            <p class="hub-desc">
              Pair your desktop directly with any phone browser. Transfers stream at full local Wi-Fi speeds without cloud servers, accounts, or mobile app installation.
            </p>
          {/if}

          <div class="hub-network-selector-row">
            {#if interfaces.length > 1}
              <div class="network-select-wrap">
                <label for="hub-iface-select">Select Network Adapter:</label>
                <select id="hub-iface-select" class="text-input compact" bind:value={selectedIp}>
                  {#each interfaces as iface (iface.ip)}
                    <option value={iface.ip}>
                      {iface.friendlyName || iface.name} ({iface.ip})
                    </option>
                  {/each}
                </select>
              </div>
            {/if}

            <button
              type="button"
              class="details-toggle-btn"
              on:click={() => { networkDetailsOpen = !networkDetailsOpen }}
            >
              {networkDetailsOpen ? 'Hide Network Details ▲' : 'Advanced Network Details ▼'}
            </button>
          </div>

          {#if networkDetailsOpen && currentNetwork}
            <div class="network-details-drawer">
              <dl class="details-grid">
                <div><dt>Adapter</dt><dd>{currentNetwork.name || 'Default'}</dd></div>
                <div><dt>Family</dt><dd>{currentNetwork.family || 'IPv4'}</dd></div>
                <div><dt>Address</dt><dd>{currentNetwork.ip}</dd></div>
                <div><dt>Prefix / CIDR</dt><dd>{currentNetwork.cidr || 'N/A'}</dd></div>
                <div><dt>Type</dt><dd>{currentNetwork.type || 'Local LAN'}</dd></div>
                <div><dt>QR Suitable</dt><dd>{currentNetwork.suitableForQR !== false ? 'Yes (Local LAN)' : 'No'}</dd></div>
              </dl>
            </div>
          {/if}
        </div>

        <div class="hub-action-col">
          <div class="hub-action-box">
            <div class="action-icon-circle">
              <svg width="28" height="28" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2">
                <rect x="2" y="2" width="20" height="8" rx="2" ry="2"/>
                <rect x="2" y="14" width="20" height="8" rx="2" ry="2"/>
                <line x1="6" y1="6" x2="6.01" y2="6"/>
                <line x1="6" y1="18" x2="6.01" y2="18"/>
              </svg>
            </div>
            <button
              type="button"
              class="button primary large full-width"
              on:click={startSession}
              disabled={starting || interfaces.length === 0 || networkState?.status === 'no_network'}
            >
              {#if starting}
                Starting Session…
              {:else if networkState?.status === 'no_network' || interfaces.length === 0}
                No Network Available
              {:else}
                Start Transfer Session
              {/if}
            </button>
            <span class="hub-action-tip">
              {#if networkState?.status === 'no_network' || interfaces.length === 0}
                Connect to Wi-Fi or Ethernet to enable transfers
              {:else}
                Prepares local connection on port 43821
              {/if}
            </span>
          </div>
        </div>
      </div>
    {:else}
      <!-- ACTIVE: SESSION LIVE WITH QR CODE -->
      <div class="hub-active-grid">
        <div class="hub-qr-col">
          <div class="qr-frame-box">
            {#if qrUrl}
              <img src={qrUrl} alt="Pairing QR Code" class="active-qr-img" />
            {:else if qrError}
              <div class="qr-loading" style="color: var(--danger-text); font-size: 11px;">{qrError}</div>
            {:else}
              <div class="qr-loading">Generating QR code…</div>
            {/if}
            <div class="qr-corner top-left"></div>
            <div class="qr-corner top-right"></div>
            <div class="qr-corner bottom-left"></div>
            <div class="qr-corner bottom-right"></div>
          </div>
          <span class="qr-caption">Scan with phone camera</span>
        </div>

        <div class="hub-active-details-col">
          <div class="active-status-bar">
            {#if refreshingNetwork}
              <span class="hub-status-chip scanning">
                <span class="chip-spinner"></span>
                Detecting network…
              </span>
            {:else if networkState?.status === 'no_network' || interfaces.length === 0}
              <span class="hub-status-chip disconnected">
                <span class="chip-pulse off"></span>
                Network Disconnected
              </span>
            {:else}
              <span class="hub-status-chip online">
                <span class="chip-pulse active"></span>
                Session Live & Listening
              </span>
            {/if}
            <span class="muted" style="font-size: 12px;">Port 43821 · Ephemeral Token</span>
            <button
              type="button"
              class="hub-refresh-btn"
              class:spinning={refreshingNetwork}
              on:click={onRefreshNetwork}
              disabled={refreshingNetwork}
              title="Refresh network status"
              aria-label="Refresh network status"
            >
              <svg width="14" height="14" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2">
                <path d="M21.5 2v6h-6M21.34 15.57a10 10 0 1 1-.57-8.38l5.67-5.67"/>
              </svg>
            </button>
          </div>

          <h3 class="active-heading">Connect your phone to begin</h3>
          {#if networkState?.status === 'no_network' || interfaces.length === 0}
            <p class="active-instructions" style="color: var(--danger-text);">
              Network connection was lost. Reconnect this computer to Wi-Fi or phone hotspot to continue transfers.
            </p>
          {:else}
            <p class="active-instructions">
              1. Scan the QR code with your phone camera, or open your mobile browser to the address below.<br>
              2. Both devices must be on the same local network (<strong>{currentNetwork?.friendlyName || 'Wi-Fi'}</strong>).
            </p>
          {/if}

          <div class="active-url-box">
            {#if networkState?.status === 'no_network' || !serverAddress}
              <span class="url-text" style="color: var(--danger-text); font-style: italic;">Waiting for network reconnection…</span>
            {:else}
              <span class="url-text">http://{serverAddress}</span>
              <button type="button" class="button ghost small copy-btn" on:click={copySessionUrl}>
                Copy Address
              </button>
            {/if}
          </div>

          <div class="active-controls-row">
            <button type="button" class="button ghost small" on:click={() => fetchQR(qrKey)}>
              ↻ Regenerate QR
            </button>
            <button
              type="button"
              class="button danger small"
              on:click={() => endSession(qrKey)}
              disabled={isBusy(`end:${qrKey}`)}
            >
              End Session
            </button>
          </div>
        </div>
      </div>
    {/if}
  </section>

  <!-- 2. ACTIVE TRANSFERS DOCK (Dynamic when transfers are running) -->
  {#if uploads.some(activeUpload) || downloads.some(activeDownload) || batches.some(activeBatch)}
    <section class="active-transfers-dock" aria-live="polite">
      <div class="dock-header">
        <div class="dock-title-group">
          <span class="dock-pulse"></span>
          <h4>Active Transfers</h4>
        </div>
        <span class="dock-badge">
          {uploads.filter(activeUpload).length + downloads.filter(activeDownload).length + batches.filter(activeBatch).length} in progress
        </span>
      </div>

      <div class="dock-cards-list">
        <!-- Outgoing Downloads -->
        {#each downloads.filter(activeDownload) as d (d.id)}
          <div class="transfer-card outgoing">
            <div class="transfer-card-header">
              <div class="transfer-direction-badge outgoing">
                <span>↗ Sending to Phone</span>
              </div>
              <span class="transfer-meta-rate tabular">{speed(d.speed)}</span>
            </div>

            <div class="transfer-filename-row">
              <strong class="transfer-name">{d.name}</strong>
              <span class="transfer-percent tabular">{percent(d.written, d.size)}%</span>
            </div>

            <ProgressBar value={percent(d.written, d.size)} variant="primary" size="sm" />

            <div class="transfer-card-footer">
              <span class="transfer-bytes tabular">
                {formatBytes(d.written)} of {formatBytes(d.size)}
                {#if calcEta(d.written, d.size, d.speed)}
                  · ETA {calcEta(d.written, d.size, d.speed)}
                {/if}
              </span>
              <span class="transfer-substatus">{d.state}</span>
            </div>
          </div>
        {/each}

        <!-- Incoming Batches -->
        {#each batches.filter(activeBatch) as b (b.id)}
          <div class="transfer-card incoming">
            <div class="transfer-card-header">
              <div class="transfer-direction-badge incoming">
                <span>↙ Receiving Batch from Phone</span>
              </div>
              <button type="button" class="cancel-text-btn" on:click={() => cancelBatch(b)}>Cancel</button>
            </div>

            <div class="transfer-filename-row">
              <strong class="transfer-name">Batch: {batchName(b)} ({doneItems(b)} of {b.totalItemCount} files)</strong>
              <span class="transfer-percent tabular">{percent(b.transferredBytes, b.declaredTotalBytes)}%</span>
            </div>

            <ProgressBar value={percent(b.transferredBytes, b.declaredTotalBytes)} variant="primary" size="sm" />

            <div class="transfer-card-footer">
              <span class="transfer-bytes tabular">
                {formatBytes(b.transferredBytes)} of {formatBytes(b.declaredTotalBytes)}
              </span>
              <span class="transfer-substatus">
                {currentItem(b) ? `Receiving ${currentItem(b)?.displayName}` : 'Transferring…'}
              </span>
            </div>
          </div>
        {/each}

        <!-- Incoming Single Uploads -->
        {#each uploads.filter(activeUpload) as u (u.id)}
          <div class="transfer-card incoming">
            <div class="transfer-card-header">
              <div class="transfer-direction-badge incoming">
                <span>↙ Receiving from Phone</span>
              </div>
              <div class="header-right-actions">
                <span class="transfer-meta-rate tabular">{speed(u.speed)}</span>
                <button type="button" class="cancel-text-btn" on:click={() => cancelUpload(u.id)}>Cancel</button>
              </div>
            </div>

            <div class="transfer-filename-row">
              <strong class="transfer-name">{u.name}</strong>
              <span class="transfer-percent tabular">{u.state === 'uploading' ? `${percent(u.uploaded, u.size)}%` : u.state}</span>
            </div>

            <ProgressBar value={percent(u.uploaded, u.size)} variant="primary" size="sm" />

            <div class="transfer-card-footer">
              <span class="transfer-bytes tabular">
                {formatBytes(u.uploaded)} of {formatBytes(u.size)}
                {#if calcEta(u.uploaded, u.size, u.speed)}
                  · ETA {calcEta(u.uploaded, u.size, u.speed)}
                {/if}
              </span>
              <span class="transfer-substatus">{u.state}</span>
            </div>
          </div>
        {/each}
      </div>
    </section>
  {/if}

  <!-- 3. MAIN TWO-COLUMN WORKSPACE: SEND vs RECEIVE -->
  <div class="transfer-workspace-grid">
    <!-- LEFT: SEND TO PHONE (Shared Files & Folders) -->
    <div class="workspace-column send-column">
      <div class="column-card drop-target-box">
        <div class="card-head-row">
          <div>
            <span class="eyebrow-accent">Send to Phone</span>
            <h3>Shared Files & Folders</h3>
          </div>
          <span class="count-pill">{totalSharedFiles}</span>
        </div>
        <p class="column-desc">Files added here can be downloaded directly on your paired phone.</p>

        <!-- Drop & Pick Area -->
        <div class="drop-zone-interactive">
          <div class="drop-zone-icon">
            <svg width="24" height="24" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2">
              <path d="M21 15v4a2 2 0 0 1-2 2H5a2 2 0 0 1-2-2v-4"/>
              <polyline points="17 8 12 3 7 8"/>
              <line x1="12" y1="3" x2="12" y2="15"/>
            </svg>
          </div>
          <div class="drop-zone-text">
            <strong>Drag and drop files or folders here</strong>
            <span>or choose items from your computer</span>
          </div>
          <div class="drop-zone-btn-row">
            <button
              type="button"
              class="button secondary small"
              on:click={pickFiles}
              disabled={picking || pickingFolder}
            >
              {picking ? 'Opening…' : '+ Choose Files'}
            </button>
            <button
              type="button"
              class="button ghost small"
              on:click={pickFolder}
              disabled={picking || pickingFolder}
            >
              {pickingFolder ? 'Reading…' : '+ Choose Folder'}
            </button>
          </div>
        </div>

        <!-- Shared Items Summary & List -->
        {#if files.length > 0 || sources.length > 0}
          <div class="shared-summary-strip">
            <span>Selected: <strong>{totalSharedFiles} items</strong> ({formatBytes(totalSharedBytes)})</span>
            <button type="button" class="button ghost small clear-btn" on:click={clearAllShared}>
              Clear All
            </button>
          </div>

          <div class="shared-items-list">
            <!-- Files -->
            {#each files as f (f.id)}
              <div class="shared-item-row">
                <span class="file-ext-chip">{getExt(f.name)}</span>
                <div class="item-details">
                  <span class="item-name" title={f.name}>{f.name}</span>
                  <span class="item-size tabular">{formatBytes(f.size)}</span>
                </div>
                <button
                  type="button"
                  class="remove-item-btn"
                  on:click={() => revoke(f.id)}
                  disabled={isBusy(`revoke:${f.id}`)}
                  title="Remove file"
                  aria-label="Remove {f.name}"
                >
                  ×
                </button>
              </div>
            {/each}

            <!-- Folder Sources -->
            {#each sources.filter((s) => s.kind === 'folder') as s (s.id)}
              <div class="shared-item-row folder-row">
                <span class="file-ext-chip folder">DIR</span>
                <div class="item-details">
                  <span class="item-name" title={s.rootName || s.name}>{s.rootName || s.name}</span>
                  <span class="item-size tabular">{s.itemCount} items · {formatBytes(s.totalBytes)}</span>
                </div>
                <button
                  type="button"
                  class="remove-item-btn"
                  on:click={() => revokeSource(s.id)}
                  disabled={isBusy(`revoke-source:${s.id}`)}
                  title="Remove folder"
                  aria-label="Remove {s.rootName || s.name}"
                >
                  ×
                </button>
              </div>
            {/each}
          </div>
        {:else}
          <div class="empty-state-compact">
            <span>No files shared yet. Choose files above to share with your phone.</span>
          </div>
        {/if}
      </div>
    </div>

    <!-- RIGHT: RECEIVE FROM PHONE (Approval Inbox & Quick Notes) -->
    <div class="workspace-column receive-column">
      <!-- Approval Inbox Card -->
      <div class="column-card inbox-card">
        <div class="card-head-row">
          <div>
            <span class="eyebrow-blue">Receive from Phone</span>
            <h3>Approval Inbox</h3>
          </div>
          <span class="count-pill">{pending.length + pendingBatches.length}</span>
        </div>
        <p class="column-desc">Review and confirm transfer requests before files are written to disk.</p>

        <div class="inbox-requests-list">
          {#each pending as u (u.id)}
            <div class="inbox-request-item">
              <div class="request-icon">↙</div>
              <div class="request-info">
                <strong>{u.name}</strong>
                <span class="tabular">{formatBytes(u.size)} · Pending Approval</span>
              </div>
              <div class="request-actions">
                <button
                  type="button"
                  class="button success small"
                  on:click={() => decide(u, true)}
                  disabled={isBusy(`approve:${u.id}`)}
                >
                  Approve
                </button>
                <button
                  type="button"
                  class="button danger small"
                  on:click={() => decide(u, false)}
                  disabled={isBusy(`reject:${u.id}`)}
                >
                  Reject
                </button>
              </div>
            </div>
          {/each}

          {#each pendingBatches as b (b.id)}
            <div class="inbox-request-item batch-item">
              <div class="request-icon">↙</div>
              <div class="request-info">
                <strong>Batch: {batchName(b)}</strong>
                <span class="tabular">{formatBytes(b.declaredTotalBytes)} · {b.totalItemCount} files</span>
              </div>
              <div class="request-actions">
                <button
                  type="button"
                  class="button success small"
                  on:click={() => decideBatch(b, true)}
                  disabled={isBusy(`approve-batch:${b.id}`)}
                >
                  Approve
                </button>
                <button
                  type="button"
                  class="button danger small"
                  on:click={() => decideBatch(b, false)}
                  disabled={isBusy(`reject-batch:${b.id}`)}
                >
                  Reject
                </button>
              </div>
            </div>
          {/each}

          {#if pending.length === 0 && pendingBatches.length === 0}
            <div class="empty-state-compact">
              <div class="empty-icon-shield">✓</div>
              <span>No pending requests. Incoming transfers will ask for your approval here.</span>
            </div>
          {/if}
        </div>
      </div>

      <!-- Quick Text & Links Card -->
      <div class="column-card notes-card">
        <div class="card-head-row">
          <div>
            <span class="eyebrow-neutral">Quick Text & Links</span>
            <h3>Notes & Clipboard</h3>
          </div>
          {#if textMessages.length > 0}
            <button type="button" class="button ghost small clear-btn" on:click={clearTextAction}>
              Clear
            </button>
          {/if}
        </div>

        <div class="quick-text-widget">
          <div class="text-compose-box">
            <textarea
              class="text-input notes-textarea"
              placeholder={sessions.length ? 'Type a note or paste a link to share with your phone… (Enter to send)' : 'Start a session above to send notes to your phone.'}
              bind:value={textInput}
              maxlength="65536"
              disabled={!sessions.length}
              on:keydown={(e) => {
                if (e.key === 'Enter' && !e.shiftKey) {
                  e.preventDefault()
                  sendTextAction()
                }
              }}
            ></textarea>
            <div class="compose-bar">
              <span class="char-count tabular">{textInput.length} / 65536</span>
              <button
                type="button"
                class="button primary small"
                on:click={sendTextAction}
                disabled={!textInput.trim() || sendingText || !sessions.length}
              >
                {sendingText ? 'Sending…' : 'Send'}
              </button>
            </div>
          </div>

          <!-- Message Feed -->
          {#if textMessages.length > 0}
            <div class="notes-feed">
              {#each [...textMessages].reverse() as msg (msg.id)}
                <div class="note-bubble {msg.sender}">
                  <div class="note-head">
                    <span class="note-sender">{msg.sender === 'desktop' ? 'PC (Desktop)' : 'Phone'}</span>
                    <span class="note-time tabular">{fmtTime(msg.createdAt)}</span>
                  </div>
                  <div class="note-content">{msg.content}</div>
                  <div class="note-actions">
                    <button
                      type="button"
                      class="mini-action-btn"
                      on:click={() => { copyTextToClipboard(msg.content); showToast('Copied note', 'info'); }}
                    >
                      Copy
                    </button>
                    {#if msg.isLink && msg.url}
                      <button
                        type="button"
                        class="mini-action-btn link"
                        on:click={() => { if (msg.url) openExternalURL(msg.url) }}
                      >
                        Open Link ↗
                      </button>
                    {/if}
                  </div>
                </div>
              {/each}
            </div>
          {:else}
            <div class="notes-empty">
              <span class="notes-empty-icon" aria-hidden="true">💬</span>
              <p class="notes-empty-text">
                {sessions.length
                  ? 'No notes or links shared yet. Send one above, or send one from your phone.'
                  : 'Start a session above to exchange notes and links with your phone.'}
              </p>
            </div>
          {/if}
        </div>
      </div>
    </div>
  </div>

  <!-- 4. RECENT ACTIVITY STRIP (Bottom Preview) -->
  <section class="recent-strip-card">
    <div class="strip-header">
      <div>
        <h4>Recent Transfers</h4>
        <p>Latest files transferred during this session</p>
      </div>
      <button type="button" class="button ghost small" on:click={goToHistory}>
        View Full Transfer History →
      </button>
    </div>

    <div class="strip-items-grid">
      {#each [...uploads.filter((u) => !activeUpload(u)), ...downloads.filter((d) => !activeDownload(d))].slice(-3).reverse() as item (item.id)}
        <div class="strip-row">
          <span class="strip-icon">{'fileId' in item ? '↗' : '↙'}</span>
          <div class="strip-info">
            <span class="strip-name" title={item.name}>{item.name}</span>
            <span class="strip-meta tabular">
              {formatBytes('fileId' in item ? item.written : item.uploaded)} · {item.state}
            </span>
          </div>
          {#if item.state === 'completed' && !('fileId' in item)}
            <button
              type="button"
              class="button ghost small"
              style="padding: 2px 6px; font-size: 11px;"
              on:click={handleOpenDownloadFolder}
            >
              Show
            </button>
          {/if}
        </div>
      {:else}
        <div class="empty-strip">No completed transfers yet in this session.</div>
      {/each}
    </div>
  </section>
</div>

<style>
  .personal-view-container {
    display: flex;
    flex-direction: column;
    gap: 20px;
    width: 100%;
  }

  /* 1. HERO CONNECTION HUB */
  .connection-hub-card {
    background: var(--bg-surface);
    border: 1px solid var(--border-base);
    border-radius: var(--radius-lg, 14px);
    box-shadow: var(--shadow-sm);
    padding: 24px 28px;
    transition: border-color var(--duration-fast, 150ms) ease;
  }

  .connection-hub-card.active-session {
    border-color: var(--accent-border);
    background: linear-gradient(180deg, var(--bg-surface) 0%, var(--accent-subtle) 100%);
  }

  .hub-inactive-grid {
    display: grid;
    grid-template-columns: 1fr 280px;
    gap: 32px;
    align-items: center;
  }

  .hub-pill-row {
    display: flex;
    align-items: center;
    gap: 10px;
    margin-bottom: 12px;
    flex-wrap: wrap;
  }

  .hub-status-chip {
    display: inline-flex;
    align-items: center;
    gap: 6px;
    padding: 4px 10px;
    border-radius: 999px;
    font-size: 12px;
    font-weight: 700;
  }

  .hub-status-chip.ready {
    background: var(--accent-subtle);
    color: var(--accent);
  }

  .hub-status-chip.online {
    background: var(--success-subtle);
    color: var(--success);
  }

  .hub-status-chip.disconnected {
    background: var(--danger-subtle);
    color: var(--danger-text);
  }

  .hub-status-chip.warning {
    background: var(--warning-subtle);
    color: var(--warning-text);
  }

  .hub-status-chip.scanning {
    background: var(--bg-surface-elevated);
    color: var(--text-muted);
  }

  .chip-pulse {
    width: 7px;
    height: 7px;
    border-radius: 50%;
    background: currentColor;
  }

  .chip-pulse.active {
    box-shadow: 0 0 0 3px var(--success-border);
  }

  .chip-pulse.off {
    background: var(--danger-text);
  }

  .chip-pulse.warn {
    background: var(--warning-text);
  }

  .chip-spinner {
    width: 8px;
    height: 8px;
    border: 1.5px solid var(--border-default);
    border-top-color: var(--accent);
    border-radius: 50%;
    animation: spin 0.8s linear infinite;
  }

  .hub-refresh-btn {
    display: inline-flex;
    align-items: center;
    justify-content: center;
    width: 26px;
    height: 26px;
    border-radius: var(--radius-sm);
    border: 1px solid var(--border-default);
    background: var(--bg-surface);
    color: var(--text-secondary);
    cursor: pointer;
    transition: all var(--duration-fast, 150ms) var(--ease-default);
    padding: 0;
  }

  .hub-refresh-btn:hover:not(:disabled) {
    background: var(--bg-surface-hover);
    color: var(--text-primary);
    border-color: var(--border-strong);
  }

  .hub-refresh-btn:focus-visible {
    outline: 2px solid var(--focus-ring);
    outline-offset: 1px;
  }

  .hub-refresh-btn.spinning svg {
    animation: spin 0.8s linear infinite;
  }

  @keyframes spin {
    to { transform: rotate(360deg); }
  }

  .hub-fallback-hint {
    margin: -4px 0 12px;
    font-size: 12px;
    line-height: 1.4;
    color: var(--warning-text);
    background: var(--warning-subtle);
    padding: 6px 12px;
    border-radius: var(--radius-sm);
    border: 1px solid var(--warning-border);
  }

  .hub-network-chip {
    display: inline-flex;
    align-items: center;
    gap: 6px;
    padding: 4px 10px;
    border-radius: 999px;
    font-size: 12px;
    font-weight: 600;
    background: var(--bg-surface-elevated);
    color: var(--text-secondary);
  }

  .hub-heading {
    margin: 0 0 8px;
    font-size: 22px;
    font-weight: 700;
    letter-spacing: -0.02em;
    color: var(--text-primary);
  }

  .hub-desc {
    margin: 0 0 16px;
    font-size: 13.5px;
    line-height: 1.5;
    color: var(--text-secondary);
    max-width: 620px;
  }

  .hub-network-selector-row {
    display: flex;
    align-items: center;
    gap: 16px;
    flex-wrap: wrap;
  }

  .network-select-wrap {
    display: flex;
    align-items: center;
    gap: 8px;
    font-size: 12.5px;
  }

  .details-toggle-btn {
    background: transparent;
    border: none;
    font-size: 12px;
    font-weight: 600;
    color: var(--accent);
    cursor: pointer;
    padding: 4px 0;
  }

  .network-details-drawer {
    margin-top: 14px;
    padding: 12px 16px;
    background: var(--bg-surface-elevated);
    border: 1px solid var(--border-subtle);
    border-radius: var(--radius-md, 10px);
  }

  .details-grid {
    display: grid;
    grid-template-columns: repeat(auto-fit, minmax(140px, 1fr));
    gap: 8px 16px;
    margin: 0;
  }

  .details-grid dt {
    font-size: 11px;
    font-weight: 600;
    color: var(--text-muted);
    text-transform: uppercase;
  }

  .details-grid dd {
    margin: 2px 0 0;
    font-size: 12.5px;
    font-weight: 500;
    color: var(--text-primary);
  }

  .hub-action-box {
    display: flex;
    flex-direction: column;
    align-items: center;
    text-align: center;
    gap: 12px;
    background: var(--bg-surface-elevated);
    border: 1px solid var(--border-subtle);
    border-radius: var(--radius-md, 10px);
    padding: 24px 20px;
  }

  .action-icon-circle {
    width: 52px;
    height: 52px;
    border-radius: 50%;
    background: var(--accent-subtle);
    color: var(--accent);
    display: flex;
    align-items: center;
    justify-content: center;
  }

  .hub-action-tip {
    font-size: 11px;
    color: var(--text-muted);
  }

  /* ACTIVE CONNECTION GRID */
  .hub-active-grid {
    display: grid;
    grid-template-columns: 190px 1fr;
    gap: 32px;
    align-items: center;
  }

  .hub-qr-col {
    display: flex;
    flex-direction: column;
    align-items: center;
    gap: 8px;
  }

  .qr-frame-box {
    width: 170px;
    height: 170px;
    background: #ffffff;
    padding: 8px;
    border-radius: 12px;
    border: 2px solid var(--border-base);
    box-shadow: var(--shadow-md);
    position: relative;
    display: flex;
    align-items: center;
    justify-content: center;
  }

  .active-qr-img {
    width: 100%;
    height: 100%;
    object-fit: contain;
    border-radius: 6px;
  }

  .qr-loading {
    font-size: 11px;
    color: var(--text-muted);
    text-align: center;
  }

  .qr-corner {
    position: absolute;
    width: 14px;
    height: 14px;
    border-color: var(--accent);
    border-style: solid;
    pointer-events: none;
  }

  .qr-corner.top-left { top: -2px; left: -2px; border-width: 3px 0 0 3px; border-top-left-radius: 6px; }
  .qr-corner.top-right { top: -2px; right: -2px; border-width: 3px 3px 0 0; border-top-right-radius: 6px; }
  .qr-corner.bottom-left { bottom: -2px; left: -2px; border-width: 0 0 3px 3px; border-bottom-left-radius: 6px; }
  .qr-corner.bottom-right { bottom: -2px; right: -2px; border-width: 0 3px 3px 0; border-bottom-right-radius: 6px; }

  .qr-caption {
    font-size: 11.5px;
    font-weight: 600;
    color: var(--text-secondary);
  }

  .active-status-bar {
    display: flex;
    align-items: center;
    justify-content: space-between;
    margin-bottom: 8px;
  }

  .active-heading {
    margin: 0 0 6px;
    font-size: 20px;
    font-weight: 700;
  }

  .active-instructions {
    margin: 0 0 14px;
    font-size: 13px;
    line-height: 1.5;
    color: var(--text-secondary);
  }

  .active-url-box {
    display: flex;
    align-items: center;
    justify-content: space-between;
    background: var(--bg-surface-elevated);
    border: 1px solid var(--border-base);
    border-radius: var(--radius-md, 10px);
    padding: 8px 14px;
    margin-bottom: 16px;
  }

  .url-text {
    font-family: ui-monospace, SFMono-Regular, monospace;
    font-size: 14px;
    font-weight: 600;
    color: var(--accent);
  }

  .active-controls-row {
    display: flex;
    gap: 12px;
  }

  /* 2. ACTIVE TRANSFERS DOCK */
  .active-transfers-dock {
    background: var(--bg-surface);
    border: 1.5px solid var(--accent);
    border-radius: var(--radius-lg, 14px);
    box-shadow: var(--shadow-md);
    padding: 18px 24px;
    display: flex;
    flex-direction: column;
    gap: 14px;
    animation: fadeIn 150ms ease;
  }

  .dock-header {
    display: flex;
    justify-content: space-between;
    align-items: center;
  }

  .dock-title-group {
    display: flex;
    align-items: center;
    gap: 8px;
  }

  .dock-pulse {
    width: 8px;
    height: 8px;
    border-radius: 50%;
    background: var(--accent);
    box-shadow: 0 0 0 4px var(--accent-subtle);
  }

  .dock-title-group h4 {
    margin: 0;
    font-size: 15px;
    font-weight: 700;
  }

  .dock-badge {
    font-size: 11.5px;
    font-weight: 600;
    color: var(--accent);
    background: var(--accent-subtle);
    padding: 2px 8px;
    border-radius: 999px;
  }

  .dock-cards-list {
    display: flex;
    flex-direction: column;
    gap: 12px;
  }

  .transfer-card {
    background: var(--bg-surface-elevated);
    border: 1px solid var(--border-base);
    border-radius: var(--radius-md, 10px);
    padding: 12px 16px;
    display: flex;
    flex-direction: column;
    gap: 8px;
  }

  .transfer-card-header {
    display: flex;
    justify-content: space-between;
    align-items: center;
  }

  .transfer-direction-badge {
    font-size: 11px;
    font-weight: 700;
    padding: 2px 8px;
    border-radius: 4px;
    text-transform: uppercase;
  }

  .transfer-direction-badge.outgoing {
    background: var(--accent-subtle);
    color: var(--accent);
  }

  .transfer-direction-badge.incoming {
    background: var(--success-subtle);
    color: var(--success);
  }

  .transfer-meta-rate {
    font-size: 12px;
    font-weight: 700;
    color: var(--text-primary);
  }

  .transfer-filename-row {
    display: flex;
    justify-content: space-between;
    align-items: center;
    gap: 12px;
  }

  .transfer-name {
    font-size: 13.5px;
    white-space: nowrap;
    overflow: hidden;
    text-overflow: ellipsis;
  }

  .transfer-percent {
    font-size: 13px;
    font-weight: 700;
    color: var(--accent);
  }

  .transfer-card-footer {
    display: flex;
    justify-content: space-between;
    font-size: 11.5px;
    color: var(--text-secondary);
  }

  .cancel-text-btn {
    background: transparent;
    border: none;
    font-size: 11.5px;
    font-weight: 600;
    color: var(--danger);
    cursor: pointer;
  }

  .cancel-text-btn:hover {
    text-decoration: underline;
  }

  /* 3. TWO-COLUMN WORKSPACE */
  .transfer-workspace-grid {
    display: grid;
    grid-template-columns: 1fr 1fr;
    gap: 20px;
  }

  @media (max-width: 900px) {
    .transfer-workspace-grid {
      grid-template-columns: 1fr;
    }
  }

  .column-card {
    background: var(--bg-surface);
    border: 1px solid var(--border-base);
    border-radius: var(--radius-lg, 14px);
    box-shadow: var(--shadow-sm);
    padding: 20px 22px;
    display: flex;
    flex-direction: column;
    gap: 14px;
  }

  .card-head-row {
    display: flex;
    justify-content: space-between;
    align-items: flex-start;
  }

  .eyebrow-accent {
    font-size: 11px;
    font-weight: 700;
    color: var(--accent);
    text-transform: uppercase;
    letter-spacing: 0.04em;
    display: block;
    margin-bottom: 2px;
  }

  .eyebrow-blue {
    font-size: 11px;
    font-weight: 700;
    color: var(--accent);
    text-transform: uppercase;
    letter-spacing: 0.04em;
    display: block;
    margin-bottom: 2px;
  }

  .eyebrow-neutral {
    font-size: 11px;
    font-weight: 700;
    color: var(--text-muted);
    text-transform: uppercase;
    letter-spacing: 0.04em;
    display: block;
    margin-bottom: 2px;
  }

  .card-head-row h3 {
    margin: 0;
    font-size: 17px;
    font-weight: 700;
    color: var(--text-primary);
  }

  .count-pill {
    font-size: 12px;
    font-weight: 700;
    padding: 2px 8px;
    border-radius: 999px;
    background: var(--bg-surface-elevated);
    color: var(--text-secondary);
  }

  .column-desc {
    margin: 0;
    font-size: 12.5px;
    color: var(--text-secondary);
    line-height: 1.4;
  }

  /* DROP ZONE INTERACTIVE */
  .drop-zone-interactive {
    border: 1.5px dashed var(--accent);
    background: var(--accent-subtle);
    border-radius: var(--radius-md, 10px);
    padding: 20px 16px;
    display: flex;
    flex-direction: column;
    align-items: center;
    text-align: center;
    gap: 10px;
    transition: background var(--duration-fast, 150ms) ease;
  }

  .drop-zone-interactive:hover {
    background: var(--bg-surface-hover);
  }

  .drop-zone-icon {
    width: 40px;
    height: 40px;
    border-radius: 50%;
    background: var(--accent-subtle);
    color: var(--accent);
    display: flex;
    align-items: center;
    justify-content: center;
  }

  .drop-zone-text strong {
    font-size: 13.5px;
    display: block;
    color: var(--text-primary);
  }

  .drop-zone-text span {
    font-size: 11.5px;
    color: var(--text-muted);
  }

  .drop-zone-btn-row {
    display: flex;
    gap: 8px;
    margin-top: 4px;
  }

  .shared-summary-strip {
    display: flex;
    justify-content: space-between;
    align-items: center;
    padding: 8px 12px;
    background: var(--bg-surface-elevated);
    border-radius: var(--radius-sm, 6px);
    font-size: 12px;
    color: var(--text-secondary);
  }

  .clear-btn {
    padding: 2px 8px;
    font-size: 11px;
  }

  .shared-items-list {
    display: flex;
    flex-direction: column;
    gap: 6px;
    max-height: 240px;
    overflow-y: auto;
  }

  .shared-item-row {
    display: flex;
    align-items: center;
    gap: 10px;
    padding: 8px 12px;
    background: var(--bg-surface-elevated);
    border: 1px solid var(--border-subtle);
    border-radius: var(--radius-sm, 6px);
  }

  .file-ext-chip {
    font-size: 10px;
    font-weight: 700;
    padding: 3px 6px;
    border-radius: 4px;
    background: var(--accent-subtle);
    color: var(--accent);
    font-family: monospace;
  }

  .file-ext-chip.folder {
    background: var(--warning-subtle);
    color: var(--warning);
  }

  .item-details {
    flex: 1;
    min-width: 0;
    display: flex;
    flex-direction: column;
  }

  .item-name {
    font-size: 12.5px;
    font-weight: 600;
    color: var(--text-primary);
    white-space: nowrap;
    overflow: hidden;
    text-overflow: ellipsis;
  }

  .item-size {
    font-size: 11px;
    color: var(--text-muted);
  }

  .remove-item-btn {
    background: transparent;
    border: none;
    font-size: 18px;
    color: var(--text-muted);
    cursor: pointer;
    line-height: 1;
    padding: 2px 6px;
    border-radius: 4px;
  }

  .remove-item-btn:hover {
    color: var(--danger);
    background: var(--danger-subtle);
  }

  /* INBOX & REQUESTS */
  .inbox-requests-list {
    display: flex;
    flex-direction: column;
    gap: 8px;
  }

  .inbox-request-item {
    display: flex;
    align-items: center;
    gap: 12px;
    padding: 10px 14px;
    background: var(--bg-surface-elevated);
    border: 1px solid var(--border-base);
    border-radius: var(--radius-md, 10px);
  }

  .request-icon {
    width: 32px;
    height: 32px;
    border-radius: 50%;
    background: var(--success-subtle);
    color: var(--success);
    display: flex;
    align-items: center;
    justify-content: center;
    font-weight: 700;
  }

  .request-info {
    flex: 1;
    min-width: 0;
    display: flex;
    flex-direction: column;
  }

  .request-info strong {
    font-size: 13px;
    white-space: nowrap;
    overflow: hidden;
    text-overflow: ellipsis;
  }

  .request-info span {
    font-size: 11.5px;
    color: var(--text-secondary);
  }

  .request-actions {
    display: flex;
    gap: 6px;
  }

  .empty-state-compact {
    padding: 18px;
    text-align: center;
    background: var(--bg-surface-elevated);
    border-radius: var(--radius-sm, 6px);
    font-size: 12.5px;
    color: var(--text-muted);
    display: flex;
    flex-direction: column;
    align-items: center;
    gap: 6px;
  }

  .empty-icon-shield {
    width: 28px;
    height: 28px;
    border-radius: 50%;
    background: var(--success-subtle);
    color: var(--success);
    display: flex;
    align-items: center;
    justify-content: center;
    font-size: 14px;
    font-weight: 700;
  }

  /* QUICK TEXT / NOTES */
  .quick-text-widget {
    display: flex;
    flex-direction: column;
    gap: 10px;
  }

  .text-compose-box {
    display: flex;
    flex-direction: column;
    gap: 6px;
  }

  .notes-textarea {
    resize: vertical;
    min-height: 54px;
    font-size: 12.5px;
    font-family: inherit;
    padding: 8px 10px;
  }

  .compose-bar {
    display: flex;
    justify-content: space-between;
    align-items: center;
  }

  .char-count {
    font-size: 11px;
    color: var(--text-muted);
  }

  .notes-feed {
    display: flex;
    flex-direction: column;
    gap: 8px;
    max-height: 160px;
    overflow-y: auto;
    padding-right: 4px;
  }

  .note-bubble {
    padding: 8px 12px;
    border-radius: var(--radius-sm, 6px);
    background: var(--bg-surface-elevated);
    border: 1px solid var(--border-subtle);
    display: flex;
    flex-direction: column;
    gap: 4px;
  }

  .note-bubble.phone {
    border-left: 3px solid var(--accent);
  }

  .note-bubble.desktop {
    border-left: 3px solid var(--border-strong);
  }

  .note-bubble.phone .note-sender {
    color: var(--accent);
  }

  .notes-empty {
    padding: 14px 10px;
    text-align: center;
    background: var(--bg-surface-elevated);
    border-radius: var(--radius-sm, 6px);
    border: 1px dashed var(--border-subtle);
    display: flex;
    flex-direction: column;
    align-items: center;
    gap: 4px;
  }

  .notes-empty-icon {
    font-size: 15px;
    opacity: 0.75;
  }

  .notes-empty-text {
    margin: 0;
    font-size: 11.5px;
    color: var(--text-muted);
    line-height: 1.4;
  }

  .note-head {
    display: flex;
    justify-content: space-between;
    font-size: 10.5px;
    color: var(--text-muted);
  }

  .note-sender {
    font-weight: 600;
    text-transform: capitalize;
  }

  .note-content {
    font-size: 12.5px;
    line-height: 1.4;
    word-break: break-word;
  }

  .note-actions {
    display: flex;
    gap: 8px;
    margin-top: 2px;
  }

  .mini-action-btn {
    background: transparent;
    border: none;
    font-size: 11px;
    color: var(--accent);
    cursor: pointer;
    padding: 0;
  }

  .mini-action-btn:hover {
    text-decoration: underline;
  }

  /* 4. RECENT ACTIVITY STRIP */
  .recent-strip-card {
    background: var(--bg-surface);
    border: 1px solid var(--border-base);
    border-radius: var(--radius-lg, 14px);
    padding: 16px 22px;
    display: flex;
    flex-direction: column;
    gap: 12px;
  }

  .strip-header {
    display: flex;
    justify-content: space-between;
    align-items: center;
  }

  .strip-header h4 {
    margin: 0;
    font-size: 14.5px;
    font-weight: 700;
  }

  .strip-header p {
    margin: 2px 0 0;
    font-size: 12px;
    color: var(--text-secondary);
  }

  .strip-items-grid {
    display: grid;
    grid-template-columns: repeat(auto-fit, minmax(240px, 1fr));
    gap: 10px;
  }

  .strip-row {
    display: flex;
    align-items: center;
    gap: 10px;
    padding: 8px 12px;
    background: var(--bg-surface-elevated);
    border-radius: var(--radius-sm, 6px);
    border: 1px solid var(--border-subtle);
  }

  .strip-icon {
    font-size: 14px;
    color: var(--accent);
    font-weight: 700;
  }

  .strip-info {
    flex: 1;
    min-width: 0;
    display: flex;
    flex-direction: column;
  }

  .strip-name {
    font-size: 12px;
    font-weight: 600;
    white-space: nowrap;
    overflow: hidden;
    text-overflow: ellipsis;
  }

  .strip-meta {
    font-size: 11px;
    color: var(--text-muted);
  }

  .empty-strip {
    font-size: 12px;
    color: var(--text-muted);
    font-style: italic;
    padding: 4px 0;
  }

  @keyframes fadeIn {
    from { opacity: 0; }
    to { opacity: 1; }
  }

  @media (prefers-reduced-motion: reduce) {
    .active-transfers-dock {
      animation: none !important;
    }
  }
</style>
