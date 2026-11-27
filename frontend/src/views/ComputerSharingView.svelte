<script lang="ts">
  import { onMount } from 'svelte'
  import type { main, discovery, devices, client, session } from '../../wailsjs/go/models'
  import { formatBytes, percent, speed, calcEta } from '../utils'
  import ProgressBar from '../components/ProgressBar.svelte'

  export let localPeerInfo: main.LocalPeerInfo | null = null
  export let nearbyPeers: discovery.Peer[] = []
  export let peerTransfers: main.PeerTransferJob[] = []
  export let stagedPaths: string[] = []
  export let visibilityState: devices.VisibilityState | null = null
  export let shortCode: session.ShortCodeView | null = null
  export let pending: main.UploadView[] = []
  export let pendingBatches: main.BatchView[] = []
  export let serverAddress: string = ''
  export let scanning: boolean = false
  export let sending: boolean = false
  export let pairingSasPrompt: main.PeerPairSASPrompt | null = null

  export let ensurePeerServerAction: () => void = () => {}
  export let ensurePeerShortCodeAction: () => void = () => {}
  export let regenerateShortCodeAction: (key: string) => void = () => {}
  export let triggerScanAction: () => void = () => {}
  export let setVisibilityModeAction: (mode: string) => void = () => {}
  export let pickFilesAction: () => void = () => {}
  export let pickFolderAction: () => void = () => {}
  export let clearStagedAction: () => void = () => {}
  export let sendToPeerAction: (req: main.PeerSendRequest) => void = () => {}
  export let pairWithPeerAction: (req: main.PeerPairRequest) => void = () => {}
  export let inspectPeerTargetAction: (addr: string, port: number) => Promise<client.PeerProbeInfo | null> = async () => null
  export let cancelPeerTransferAction: (jobId: string) => void = () => {}
  export let clearCompletedAction: () => void = () => {}
  export let resolvePairSasAction: (requestId: string, approved: boolean) => void = () => {}
  export let decide: (u: main.UploadView, approve: boolean) => void = () => {}
  export let decideBatch: (b: main.BatchView, approve: boolean) => void = () => {}
  export let handleOpenDownloadFolder: () => void = () => {}
  export let isBusy: (key: string) => boolean = () => false

  let selectedPeer: discovery.Peer | null = null
  let manualAddress: string = ''
  let manualShortCode: string = ''
  let probedInfo: client.PeerProbeInfo | null = null
  let probingTarget: boolean = false
  let probeError: string = ''
  let tofuConfirmed: boolean = false

  onMount(() => {
    ensurePeerServerAction()
  })

  function osIcon(os: string): string {
    switch ((os || '').toLowerCase()) {
      case 'windows': return '🪟'
      case 'darwin': return '🍎'
      case 'linux': return '🐧'
      default: return '💻'
    }
  }

  function osLabel(os: string): string {
    switch ((os || '').toLowerCase()) {
      case 'windows': return 'Windows'
      case 'darwin': return 'macOS'
      case 'linux': return 'Linux'
      default: return os || 'Desktop'
    }
  }

  function shortFp(hid: string): string {
    if (!hid) return 'Unverified'
    if (hid.length <= 16) return hid
    return `${hid.slice(0, 6)}·${hid.slice(-6)}`
  }

  function selectDiscoveredPeer(p: discovery.Peer) {
    selectedPeer = p
    manualAddress = `${p.address}:${p.port || 43821}`
    probedInfo = null
    probeError = ''
    if (p.trusted && !p.nameConflictWarning) {
      tofuConfirmed = true
      return
    }
    // Untrusted discovery hints MUST be verified over TLS 1.3 and confirmed (or paired via SAS) before sending.
    tofuConfirmed = false
    void verifyDiscoveredPeerTLS(p)
  }

  async function verifyDiscoveredPeerTLS(p: discovery.Peer) {
    probingTarget = true
    probeError = ''
    try {
      const res = await inspectPeerTargetAction(p.address, p.port || 43821)
      if (!res || !res.hostId) return
      if (res.hostId.toLowerCase() !== (p.hostId || '').toLowerCase() ||
          res.publicKey.toLowerCase() !== (p.publicKey || '').toLowerCase()) {
        probeError = 'Security warning: live TLS 1.3 key at this address does not match the discovery announcement (possible spoofed beacon).'
        probedInfo = null
        tofuConfirmed = false
        return
      }
      probedInfo = res
    } catch (err: any) {
      probeError = err?.message || String(err)
    } finally {
      probingTarget = false
    }
  }

  async function inspectManualTarget() {
    if (!manualAddress.trim() || probingTarget) return
    probingTarget = true
    probeError = ''
    probedInfo = null
    tofuConfirmed = false
    try {
      const res = await inspectPeerTargetAction(manualAddress.trim(), 0)
      probedInfo = res
      if (res?.hostId) {
        selectedPeer = null
      }
    } catch (err: any) {
      probeError = err?.message || String(err)
    } finally {
      probingTarget = false
    }
  }

  function handleSend() {
    if (!stagedPaths.length || sending) return
    const addr = selectedPeer ? `${selectedPeer.address}:${selectedPeer.port || 43821}` : manualAddress.trim()
    if (!addr) return
    sendToPeerAction({
      address: addr,
      port: selectedPeer?.port || 0,
      expectedHostId: selectedPeer?.hostId || probedInfo?.hostId || '',
      expectedPubKey: selectedPeer?.publicKey || probedInfo?.publicKey || '',
      peerName: selectedPeer?.deviceName || addr,
      shortCode: manualShortCode.replace(/\s+/g, ''),
      paths: stagedPaths
    })
  }

  function handlePairSelected() {
    const addr = selectedPeer ? `${selectedPeer.address}:${selectedPeer.port || 43821}` : manualAddress.trim()
    if (!addr) return
    pairWithPeerAction({
      address: addr,
      port: selectedPeer?.port || 0,
      expectedHostId: selectedPeer?.hostId || probedInfo?.hostId || '',
      expectedPubKey: selectedPeer?.publicKey || probedInfo?.publicKey || '',
      peerName: selectedPeer?.deviceName || addr,
      shortCode: manualShortCode.replace(/\s+/g, '')
    })
  }

  function pathLabel(p: string): string {
    const parts = p.split(/[\\/]/)
    return parts[parts.length - 1] || p
  }

  function jobStateLabel(state: string): string {
    switch (state) {
      case 'connecting': return 'Connecting over TLS 1.3…'
      case 'authenticating': return 'Verifying peer identity…'
      case 'awaiting_approval': return 'Waiting for remote confirmation…'
      case 'transferring': return 'Streaming…'
      case 'reconnecting': return 'Reconnecting…'
      case 'completed': return 'Completed & SHA-256 Verified'
      case 'failed': return 'Failed'
      case 'canceled': return 'Canceled'
      default: return state
    }
  }

  $: activeTargetLabel = selectedPeer
    ? `${selectedPeer.deviceName} (${selectedPeer.address})`
    : manualAddress.trim()
      ? manualAddress.trim()
      : 'No recipient chosen'

  $: canSend = stagedPaths.length > 0 && !sending && (
    (!!selectedPeer && (selectedPeer.trusted || tofuConfirmed || manualShortCode.replace(/\s+/g, '').length === 6)) ||
    (!!manualAddress.trim() && (tofuConfirmed || manualShortCode.replace(/\s+/g, '').length === 6))
  )
</script>

<div class="computer-sharing-view">
  <!-- TOP: LOCAL COMPUTER IDENTITY & DISCOVERY CONTROL -->
  <section class="peer-hero-card">
    <div class="peer-hero-left">
      <div class="peer-pill-row">
        <span class="peer-status-chip" class:online={!!serverAddress}>
          <span class="dot"></span>
          {serverAddress ? `Listening TLS 1.3 + HTTP on ${serverAddress}` : 'Starting local peer listener…'}
        </span>
        {#if localPeerInfo}
          <span class="peer-fp-chip tabular" title="Cryptographic Ed25519 Host ID">
            {osIcon(localPeerInfo.os)} {localPeerInfo.deviceName} · ID {localPeerInfo.fingerprint}
          </span>
        {/if}
      </div>
      <h2>Direct Desktop-to-Desktop Sharing</h2>
      <p>
        Discover nearby computers running Flitnix on your local network or connect directly by IP address and 6-digit code over authenticated TLS 1.3.
      </p>
    </div>

    <div class="peer-hero-actions">
      <div class="vis-pills-group" role="group" aria-label="Computer visibility">
        <button type="button" class="vis-btn" class:active={visibilityState?.mode === 'hidden'} on:click={() => setVisibilityModeAction('hidden')}>Hidden</button>
        <button type="button" class="vis-btn" class:active={!visibilityState?.mode || visibilityState?.mode === 'trusted'} on:click={() => setVisibilityModeAction('trusted')}>Trusted Only</button>
        <button type="button" class="vis-btn" class:active={visibilityState?.mode === 'temporary_everyone'} on:click={() => setVisibilityModeAction('temporary_everyone')}>Everyone (10m)</button>
        <button type="button" class="vis-btn" class:active={visibilityState?.mode === 'everyone'} on:click={() => setVisibilityModeAction('everyone')}>Everyone</button>
      </div>
      <div class="hero-btn-row">
        <button type="button" class="button secondary small" on:click={triggerScanAction} disabled={scanning}>
          {scanning ? 'Scanning…' : '↻ Scan Nearby'}
        </button>
        <button type="button" class="button ghost small" on:click={handleOpenDownloadFolder}>
          Open Receive Folder
        </button>
      </div>
    </div>
  </section>

  <!-- MAIN TWO-COLUMN WORKSPACE -->
  <div class="peer-workspace-grid">
    <!-- LEFT: NEARBY COMPUTERS + MANUAL TARGET -->
    <div class="peer-col">
      <section class="peer-card">
        <div class="card-head">
          <div>
            <span class="eyebrow">Nearby LAN Discovery</span>
            <h3>Computers on This Network</h3>
          </div>
          <span class="count-badge">{nearbyPeers.length}</span>
        </div>

        {#if visibilityState?.mode === 'hidden'}
          <div class="info-banner">
            Visibility is <strong>Hidden</strong>. Public LAN announcements are paused; switch to <strong>Everyone</strong> to discover nearby computers or enter an IP + 6-digit code below.
          </div>
        {/if}

        <div class="peers-list">
          {#each nearbyPeers as peer (peer.hostId)}
            <div class="peer-item" class:selected={selectedPeer?.hostId === peer.hostId}>
              <div class="peer-item-main">
                <div class="peer-os-avatar">{osIcon(peer.os)}</div>
                <div class="peer-item-meta">
                  <div class="peer-name-row">
                    <strong>{peer.deviceName || 'Flitnix Desktop'}</strong>
                    {#if peer.trusted}
                      <span class="trust-badge trusted">Trusted</span>
                    {:else if peer.nameConflictWarning}
                      <span class="trust-badge conflict">⚠ Spoofed Name Risk (Unverified)</span>
                    {:else}
                      <span class="trust-badge">Unverified Hint</span>
                    {/if}
                  </div>
                  <span class="peer-sub tabular">
                    {osLabel(peer.os)} · {peer.address}:{peer.port} · ID {shortFp(peer.hostId)}
                  </span>
                </div>
              </div>
              <div class="peer-item-actions">
                <button type="button" class="button primary small" on:click={() => selectDiscoveredPeer(peer)}>
                  {selectedPeer?.hostId === peer.hostId ? 'Selected ✓' : 'Select'}
                </button>
                {#if !peer.trusted}
                  <button type="button" class="button ghost small" on:click={() => { selectDiscoveredPeer(peer); handlePairSelected() }}>
                    Pair
                  </button>
                {/if}
              </div>
            </div>
          {:else}
            <div class="empty-box">
              No nearby computers discovered yet. Ensure both desktops are on the same local Wi-Fi/LAN with visibility set to <strong>Everyone</strong> or already paired in <strong>Trusted Only</strong>.
            </div>
          {/each}
        </div>
      </section>

      <!-- Direct Receive Code for this Computer -->
      <section class="peer-card">
        <div class="card-head">
          <div>
            <span class="eyebrow">Receive From Another Computer</span>
            <h3>This Computer's 6-Digit Share Code</h3>
          </div>
          {#if shortCode?.code && !shortCode.used && !shortCode.expired}
            <button type="button" class="button ghost small" on:click={() => regenerateShortCodeAction(shortCode?.sessionKey || '')}>
              ↻ New Code
            </button>
          {:else}
            <button type="button" class="button secondary small" on:click={ensurePeerShortCodeAction}>
              Generate 6-Digit Code
            </button>
          {/if}
        </div>
        {#if shortCode?.code && !shortCode.used && !shortCode.expired}
          <div class="code-share-box">
            <strong class="code-digits tabular">{shortCode.code.slice(0, 3)} {shortCode.code.slice(3)}</strong>
            <span class="peer-sub">Enter on another desktop (works even in Hidden or Trusted Only mode). Single-use · 3 min TTL.</span>
          </div>
        {:else}
          <div class="empty-box">
            Generate a temporary single-use 6-digit code so another computer can connect to {localPeerInfo?.address || 'this computer'} without switching to Everyone mode.
          </div>
        {/if}
      </section>

      <!-- Manual IP & 6-Digit Code Fallback -->
      <section class="peer-card">
        <div class="card-head">
          <div>
            <span class="eyebrow">Direct Connection &amp; Code Fallback</span>
            <h3>Connect Manually by Address</h3>
          </div>
        </div>
        <div class="manual-form">
          <label class="field-lbl">
            <span>Remote Computer IP or host:port</span>
            <div class="input-row">
              <input
                type="text"
                class="text-input"
                bind:value={manualAddress}
                placeholder="e.g. 192.168.1.42:43821"
                on:input={() => { selectedPeer = null; probedInfo = null; tofuConfirmed = false }}
              />
              <button type="button" class="button ghost small" on:click={inspectManualTarget} disabled={probingTarget || !manualAddress.trim()}>
                {probingTarget ? 'Checking…' : 'Verify Identity'}
              </button>
            </div>
          </label>

          <label class="field-lbl">
            <span>6-Digit Connection Code (optional — works in Hidden mode)</span>
            <input
              type="text"
              class="text-input tabular"
              maxlength="7"
              bind:value={manualShortCode}
              placeholder="482 915"
            />
          </label>

          {#if probeError}
            <div class="error-box">{probeError}</div>
          {/if}

          {#if probedInfo}
            <div class="tofu-verify-box">
              <strong>TLS 1.3 Peer Fingerprint (TOFU)</strong>
              <span class="tabular">Host ID: <code>{shortFp(probedInfo.hostId)}</code></span>
              <span class="tabular">Key: <code>{probedInfo.publicKey.slice(0, 16)}…</code></span>
              <label class="tofu-check">
                <input type="checkbox" bind:checked={tofuConfirmed} />
                <span>I confirm this Host ID matches the target computer</span>
              </label>
              <button type="button" class="button secondary small" on:click={handlePairSelected}>
                Pair &amp; Trust This Computer
              </button>
            </div>
          {/if}
        </div>
      </section>
    </div>

    <!-- RIGHT: STAGED FILES, ACTIVE PEER TRANSFERS & INCOMING CONFIRMATIONS -->
    <div class="peer-col">
      <section class="peer-card">
        <div class="card-head">
          <div>
            <span class="eyebrow">Send to Computer</span>
            <h3>Files &amp; Folders to Send</h3>
          </div>
          <span class="target-pill">Target: {activeTargetLabel}</span>
        </div>

        <div class="picker-row">
          <button type="button" class="button secondary small" on:click={pickFilesAction}>
            + Choose Files
          </button>
          <button type="button" class="button secondary small" on:click={pickFolderAction}>
            + Choose Folder
          </button>
          {#if stagedPaths.length > 0}
            <button type="button" class="button ghost small" on:click={clearStagedAction}>
              Clear ({stagedPaths.length})
            </button>
          {/if}
        </div>

        <p class="drop-hint">Tip: You can also drag and drop files or folders directly onto this window.</p>

        {#if stagedPaths.length > 0}
          <ul class="staged-list">
            {#each stagedPaths as itemPath (itemPath)}
              <li class="staged-row">
                <span class="staged-name">{pathLabel(itemPath)}</span>
                <span class="staged-path" title={itemPath}>{itemPath}</span>
              </li>
            {/each}
          </ul>
          <button type="button" class="button primary full-width" disabled={!canSend} on:click={handleSend}>
            {sending ? 'Starting Transfer…' : `Send ${stagedPaths.length} ${stagedPaths.length === 1 ? 'Item' : 'Items'} Over TLS 1.3`}
          </button>
        {:else}
          <div class="empty-box">
            Pick files or a folder above, select a computer on the left, and send directly over TLS 1.3.
          </div>
        {/if}
      </section>

      <!-- Outbound Peer Transfer Progress Queue -->
      <section class="peer-card">
        <div class="card-head">
          <div>
            <span class="eyebrow">Outbound Transfers</span>
            <h3>Desktop Transfer Activity</h3>
          </div>
          {#if peerTransfers.some((j) => ['completed', 'failed', 'canceled'].includes(j.state))}
            <button type="button" class="button ghost small" on:click={clearCompletedAction}>
              Clear Finished
            </button>
          {/if}
        </div>

        <div class="jobs-list">
          {#each peerTransfers as job (job.id)}
            <div class="job-item">
              <div class="job-head">
                <strong>{job.rootName || `${job.totalItems} ${job.totalItems === 1 ? 'file' : 'files'}`} → {job.peerName}</strong>
                <span class="job-state {job.state}">{jobStateLabel(job.state)}</span>
              </div>
              <ProgressBar
                value={percent(job.sentBytes, job.totalBytes)}
                variant={job.state === 'failed' || job.state === 'canceled' ? 'danger' : job.state === 'completed' ? 'success' : 'primary'}
                size="sm"
              />
              <div class="job-meta tabular">
                <span>
                  {formatBytes(job.sentBytes)} / {formatBytes(job.totalBytes)} · {job.completedItems}/{job.totalItems} items
                  {#if job.state === 'transferring' && job.speed > 0}
                    · {speed(job.speed)} · ETA {calcEta(job.sentBytes, job.totalBytes, job.speed)}
                  {/if}
                </span>
                {#if !['completed', 'failed', 'canceled'].includes(job.state)}
                  <button type="button" class="button danger small" on:click={() => cancelPeerTransferAction(job.id)}>
                    Cancel
                  </button>
                {/if}
              </div>
              {#if job.currentFile}
                <div class="job-subfile">Sending: {job.currentFile}</div>
              {/if}
              {#if job.error}
                <div class="error-box">{job.error}</div>
              {/if}
            </div>
          {:else}
            <div class="empty-box">No outbound desktop transfers yet.</div>
          {/each}
        </div>
      </section>

      <!-- Incoming Approval Inbox shortcut so users can receive from peers while on this tab -->
      {#if pending.length > 0 || pendingBatches.length > 0}
        <section class="peer-card">
          <div class="card-head">
            <div>
              <span class="eyebrow">Incoming Requests</span>
              <h3>Awaiting Your Approval</h3>
            </div>
            <span class="count-badge">{pending.length + pendingBatches.length}</span>
          </div>
          {#each pending as u (u.id)}
            <div class="incoming-row">
              <div>
                <strong>{u.name}</strong>
                <span class="peer-sub tabular">{formatBytes(u.size)} · {u.trustedDevice ? (u.deviceName || 'Trusted Peer') : 'Untrusted Peer'}</span>
              </div>
              <div class="peer-item-actions">
                <button type="button" class="button success small" on:click={() => decide(u, true)} disabled={isBusy(`approve:${u.id}`)}>Approve</button>
                <button type="button" class="button danger small" on:click={() => decide(u, false)} disabled={isBusy(`reject:${u.id}`)}>Reject</button>
              </div>
            </div>
          {/each}
          {#each pendingBatches as b (b.id)}
            <div class="incoming-row">
              <div>
                <strong>{b.rootName || `${b.totalItemCount} files`}</strong>
                <span class="peer-sub tabular">{formatBytes(b.declaredTotalBytes)} · {b.trustedDevice ? (b.deviceName || 'Trusted Peer') : 'Untrusted Peer'}</span>
              </div>
              <div class="peer-item-actions">
                <button type="button" class="button success small" on:click={() => decideBatch(b, true)} disabled={isBusy(`approve-batch:${b.id}`)}>Approve</button>
                <button type="button" class="button danger small" on:click={() => decideBatch(b, false)} disabled={isBusy(`reject-batch:${b.id}`)}>Reject</button>
              </div>
            </div>
          {/each}
        </section>
      {/if}
    </div>
  </div>

  <!-- Outbound SAS Verification Modal when pairing with another computer -->
  {#if pairingSasPrompt}
    <div class="global-pairing-overlay" role="dialog" aria-modal="true" aria-label="Confirm Computer Pairing Code">
      <div class="global-pairing-modal">
        <div class="global-pairing-badge">🔐 Verify Remote Computer SAS</div>
        <h3>Pairing with {pairingSasPrompt.peerName || 'Remote Computer'}</h3>
        <p>Confirm this 6-digit code matches the approval prompt on the remote desktop before trusting:</p>
        <div class="global-pairing-sas tabular">{pairingSasPrompt.sas}</div>
        <div class="global-pairing-actions">
          <button type="button" class="button success" on:click={() => resolvePairSasAction(pairingSasPrompt?.requestId || '', true)}>
            Codes Match — Pair
          </button>
          <button type="button" class="button danger" on:click={() => resolvePairSasAction(pairingSasPrompt?.requestId || '', false)}>
            Cancel
          </button>
        </div>
      </div>
    </div>
  {/if}
</div>

<style>
  .computer-sharing-view {
    display: flex;
    flex-direction: column;
    gap: 18px;
  }

  .peer-hero-card {
    background: var(--bg-surface);
    border: 1px solid var(--border-base);
    border-radius: var(--radius-lg, 14px);
    padding: 20px 24px;
    display: flex;
    justify-content: space-between;
    align-items: flex-start;
    gap: 20px;
    flex-wrap: wrap;
  }

  .peer-hero-left {
    flex: 1;
    min-width: 280px;
  }

  .peer-hero-left h2 {
    margin: 10px 0 6px;
    font-size: 20px;
    font-weight: 700;
  }

  .peer-hero-left p {
    margin: 0;
    font-size: 13px;
    color: var(--text-secondary);
    line-height: 1.45;
  }

  .peer-pill-row {
    display: flex;
    flex-wrap: wrap;
    gap: 8px;
    align-items: center;
  }

  .peer-status-chip {
    display: inline-flex;
    align-items: center;
    gap: 6px;
    padding: 4px 10px;
    border-radius: 999px;
    font-size: 11.5px;
    font-weight: 600;
    background: var(--bg-surface-elevated);
    color: var(--text-secondary);
  }

  .peer-status-chip .dot {
    width: 7px;
    height: 7px;
    border-radius: 50%;
    background: var(--text-muted);
  }

  .peer-status-chip.online {
    background: var(--success-subtle);
    color: var(--success);
  }

  .peer-status-chip.online .dot {
    background: var(--success);
  }

  .peer-fp-chip {
    font-size: 11.5px;
    font-weight: 600;
    padding: 4px 10px;
    border-radius: 999px;
    background: var(--accent-subtle);
    color: var(--accent);
  }

  .peer-hero-actions {
    display: flex;
    flex-direction: column;
    align-items: flex-end;
    gap: 10px;
  }

  .vis-pills-group {
    display: flex;
    flex-wrap: wrap;
    gap: 6px;
  }

  .vis-btn {
    border: 1px solid var(--border-base);
    background: var(--bg-surface-elevated);
    color: var(--text-secondary);
    font-size: 11.5px;
    font-weight: 600;
    padding: 5px 10px;
    border-radius: 999px;
    cursor: pointer;
  }

  .vis-btn.active {
    background: var(--accent);
    color: #fff;
    border-color: var(--accent);
  }

  .hero-btn-row {
    display: flex;
    gap: 8px;
  }

  .peer-workspace-grid {
    display: grid;
    grid-template-columns: repeat(2, minmax(0, 1fr));
    gap: 18px;
  }

  @media (max-width: 960px) {
    .peer-workspace-grid {
      grid-template-columns: 1fr;
    }
  }

  .peer-col {
    display: flex;
    flex-direction: column;
    gap: 16px;
  }

  .peer-card {
    background: var(--bg-surface);
    border: 1px solid var(--border-base);
    border-radius: var(--radius-lg, 14px);
    padding: 18px 20px;
    display: flex;
    flex-direction: column;
    gap: 12px;
  }

  .card-head {
    display: flex;
    justify-content: space-between;
    align-items: center;
    gap: 12px;
  }

  .eyebrow {
    font-size: 11px;
    font-weight: 700;
    text-transform: uppercase;
    letter-spacing: 0.05em;
    color: var(--accent);
  }

  .card-head h3 {
    margin: 2px 0 0;
    font-size: 15.5px;
    font-weight: 700;
  }

  .count-badge, .target-pill {
    font-size: 11.5px;
    font-weight: 700;
    padding: 3px 9px;
    border-radius: 999px;
    background: var(--bg-surface-elevated);
    color: var(--text-secondary);
  }

  .info-banner {
    padding: 10px 12px;
    border-radius: 8px;
    background: var(--warning-subtle);
    color: var(--text-primary);
    font-size: 12px;
    line-height: 1.45;
  }

  .peers-list, .jobs-list {
    display: flex;
    flex-direction: column;
    gap: 10px;
  }

  .peer-item {
    display: flex;
    justify-content: space-between;
    align-items: center;
    gap: 12px;
    padding: 12px;
    border-radius: 10px;
    border: 1px solid var(--border-subtle);
    background: var(--bg-surface-elevated);
  }

  .peer-item.selected {
    border-color: var(--accent);
    background: var(--accent-subtle);
  }

  .peer-item-main {
    display: flex;
    align-items: center;
    gap: 10px;
    min-width: 0;
  }

  .peer-os-avatar {
    font-size: 22px;
    width: 36px;
    height: 36px;
    border-radius: 50%;
    background: var(--bg-surface);
    display: flex;
    align-items: center;
    justify-content: center;
    flex-shrink: 0;
  }

  .peer-item-meta {
    display: flex;
    flex-direction: column;
    min-width: 0;
  }

  .peer-name-row {
    display: flex;
    align-items: center;
    gap: 6px;
    flex-wrap: wrap;
  }

  .peer-sub {
    font-size: 11.5px;
    color: var(--text-muted);
  }

  .trust-badge {
    font-size: 10px;
    font-weight: 700;
    padding: 1px 6px;
    border-radius: 999px;
    background: var(--bg-surface);
    border: 1px solid var(--border-base);
    color: var(--text-secondary);
  }

  .trust-badge.trusted {
    background: var(--success-subtle);
    border-color: var(--success-border);
    color: var(--success);
  }

  .peer-item-actions {
    display: flex;
    gap: 6px;
    flex-shrink: 0;
  }

  .manual-form {
    display: flex;
    flex-direction: column;
    gap: 10px;
  }

  .field-lbl {
    display: flex;
    flex-direction: column;
    gap: 4px;
    font-size: 12px;
    font-weight: 600;
    color: var(--text-secondary);
  }

  .input-row, .picker-row {
    display: flex;
    gap: 8px;
    align-items: center;
  }

  .input-row .text-input {
    flex: 1;
  }

  .drop-hint {
    margin: 0;
    font-size: 11.5px;
    color: var(--text-muted);
  }

  .staged-list {
    list-style: none;
    margin: 0;
    padding: 0;
    display: flex;
    flex-direction: column;
    gap: 6px;
    max-height: 180px;
    overflow-y: auto;
  }

  .staged-row {
    display: flex;
    flex-direction: column;
    padding: 7px 10px;
    border-radius: 8px;
    background: var(--bg-surface-elevated);
    border: 1px solid var(--border-subtle);
  }

  .staged-name {
    font-size: 12.5px;
    font-weight: 600;
  }

  .staged-path {
    font-size: 11px;
    color: var(--text-muted);
    white-space: nowrap;
    overflow: hidden;
    text-overflow: ellipsis;
  }

  .tofu-verify-box {
    padding: 10px 12px;
    border-radius: 8px;
    border: 1px solid var(--accent);
    background: var(--accent-subtle);
    display: flex;
    flex-direction: column;
    gap: 6px;
    font-size: 12px;
  }

  .tofu-check {
    display: flex;
    align-items: center;
    gap: 6px;
    cursor: pointer;
    font-weight: 600;
  }

  .job-item, .incoming-row {
    padding: 10px 12px;
    border-radius: 10px;
    background: var(--bg-surface-elevated);
    border: 1px solid var(--border-subtle);
    display: flex;
    flex-direction: column;
    gap: 6px;
  }

  .incoming-row {
    flex-direction: row;
    justify-content: space-between;
    align-items: center;
  }

  .job-head, .job-meta {
    display: flex;
    justify-content: space-between;
    align-items: center;
    gap: 8px;
    font-size: 12px;
  }

  .job-state {
    font-size: 11px;
    font-weight: 700;
    color: var(--accent);
  }

  .job-state.completed { color: var(--success); }
  .job-state.failed, .job-state.canceled { color: var(--danger); }

  .job-subfile {
    font-size: 11px;
    color: var(--text-muted);
  }

  .empty-box {
    padding: 14px;
    border-radius: 8px;
    background: var(--bg-surface-elevated);
    font-size: 12.5px;
    color: var(--text-muted);
    line-height: 1.45;
  }

  .error-box {
    padding: 8px 10px;
    border-radius: 6px;
    background: var(--danger-subtle);
    color: var(--danger-text);
    font-size: 12px;
  }

  .trust-badge.conflict {
    background: var(--danger-subtle);
    color: var(--danger-text);
  }

  .code-share-box {
    display: flex;
    flex-direction: column;
    gap: 6px;
    padding: 12px 14px;
    border-radius: 10px;
    background: var(--bg-surface-elevated);
    border: 1px dashed var(--accent);
  }

  .code-digits {
    font-size: 24px;
    letter-spacing: 0.14em;
    color: var(--accent);
  }
</style>
