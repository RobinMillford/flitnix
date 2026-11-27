<script lang="ts">
  import type { devices } from '../../wailsjs/go/models'
  import { fmtTime } from '../utils'
  import ConfirmModal from '../components/ConfirmModal.svelte'

  export let trustedDevices: devices.Device[] = []
  export let loadingDevices: boolean = false
  export let pairingStatus: devices.PairingStatusView | null = null
  export let pairingActionLoading: boolean = false
  export let rearmingLoading: boolean = false

  export let loadTrustedDevices: () => void = () => {}
  export let removeDeviceAction: (id: string) => void = () => {}
  export let setDeviceAutoApproveAction: (id: string, auto: boolean) => void = () => {}
  export let setDevicePolicyAction: (id: string, policy: devices.DevicePolicy) => void = () => {}
  export let approvePairingAction: (ip: string) => void = () => {}
  export let denyPairingAction: (ip: string) => void = () => {}
  export let reArmPairingAction: () => void = () => {}
  export let showToast: (msg: string, type?: any) => void = () => {}

  let deviceToRevoke: devices.Device | null = null
  let confirmRevokeOpen: boolean = false

  function requestRevoke(d: devices.Device) {
    deviceToRevoke = d
    confirmRevokeOpen = true
  }

  function confirmRevoke() {
    if (deviceToRevoke) {
      removeDeviceAction(deviceToRevoke.id)
      showToast(`Revoked trust for ${deviceToRevoke.name}`, 'info')
      deviceToRevoke = null
    }
  }

  function formatKeyFingerprint(pk: string): string {
    if (!pk) return 'Ed25519'
    return `ed25519:${pk.slice(0, 8)}…${pk.slice(-4)}`
  }

  function effectiveMaxBytes(d: devices.Device): number {
    if (d.maxAutoApproveBytes === undefined || d.maxAutoApproveBytes === 0) {
      return 100 * 1024 * 1024
    }
    return d.maxAutoApproveBytes
  }

  function updatePolicy(d: devices.Device, patch: Partial<devices.DevicePolicy>) {
    setDevicePolicyAction(d.id, {
      autoApprove: patch.autoApprove ?? d.autoApprove,
      maxAutoApproveBytes: patch.maxAutoApproveBytes ?? (d.maxAutoApproveBytes ?? 0),
      allowExecutables: patch.allowExecutables ?? !!d.allowExecutables,
      allowScripts: patch.allowScripts ?? !!d.allowScripts
    })
  }
</script>

<div class="devices-view-container">
  <!-- PENDING PAIRING NOTIFICATION (If a phone is currently requesting pairing) -->
  {#if pairingStatus?.requiresApproval}
    <div class="pairing-alert-banner">
      <div class="alert-icon-box">🔑</div>
      <div class="alert-text-col">
        <strong>Incoming Pairing Request</strong>
        <p>A mobile device <code>{pairingStatus.deviceName || pairingStatus.clientId || 'Mobile Device'}</code> is requesting to pair cryptographically (Code: <strong>{pairingStatus.sas || '---'}</strong>).</p>
      </div>
      <div class="alert-actions-row">
        <button
          type="button"
          class="button success small"
          on:click={() => approvePairingAction(pairingStatus?.requestId || '')}
          disabled={pairingActionLoading}
        >
          {pairingActionLoading ? 'Approving…' : 'Approve Pairing'}
        </button>
        <button
          type="button"
          class="button danger small"
          on:click={() => denyPairingAction(pairingStatus?.requestId || '')}
          disabled={pairingActionLoading}
        >
          Deny
        </button>
      </div>
    </div>
  {/if}

  <!-- Header Card -->
  <div class="devices-header-card">
    <div class="header-text-group">
      <h3>Cryptographically Trusted Devices</h3>
      <p>Devices authenticated with Ed25519 public keys. Trusted devices can be granted automatic approval to bypass the manual transfer prompt.</p>
    </div>
    <div class="header-actions">
      {#if pairingStatus && !pairingStatus.active}
        <button
          type="button"
          class="button secondary small"
          on:click={reArmPairingAction}
          disabled={rearmingLoading}
          title="Re-open 30-second window to accept new pairing requests"
        >
          {rearmingLoading ? 'Arming…' : 'Pair New Device'}
        </button>
      {/if}
      <button
        type="button"
        class="button ghost small"
        on:click={loadTrustedDevices}
        disabled={loadingDevices}
      >
        ↻ Refresh
      </button>
    </div>
  </div>

  <!-- DEVICES LIST OR INFORMATIVE EMPTY STATE -->
  {#if loadingDevices}
    <div class="loading-state">Loading trusted devices…</div>
  {:else if trustedDevices.length > 0}
    <div class="devices-grid">
      {#each trustedDevices as dev (dev.id)}
        <div class="device-card">
          <div class="device-card-head">
            <div class="device-avatar-icon">📱</div>
            <div class="device-identity">
              <strong class="device-name">{dev.name || 'Unnamed Device'}</strong>
              <span class="device-fingerprint tabular">{formatKeyFingerprint(dev.publicKey)}</span>
            </div>
            <button
              type="button"
              class="button danger small ghost-danger"
              on:click={() => requestRevoke(dev)}
              title="Revoke trust"
            >
              Revoke
            </button>
          </div>

          <div class="device-meta-row">
            <div class="meta-item">
              <span class="meta-lbl">Paired:</span>
              <span class="meta-val tabular">{fmtTime(dev.pairedAt)}</span>
            </div>
            <div class="meta-item">
              <span class="meta-lbl">Last Seen:</span>
              <span class="meta-val tabular">{dev.lastSeen ? fmtTime(dev.lastSeen) : 'Active now'}</span>
            </div>
          </div>

          <div class="auto-approve-toggle-box">
            <label class="toggle-label">
              <input
                type="checkbox"
                checked={dev.autoApprove}
                on:change={(e) => setDeviceAutoApproveAction(dev.id, e.currentTarget.checked)}
              />
              <span class="toggle-text">
                <strong>Automatic Approval</strong>
                <small>Allow safe files within limits from this trusted device without a manual prompt</small>
              </span>
            </label>

            {#if dev.autoApprove}
              <div class="device-policy-rules">
                <label class="policy-field">
                  <span>Max auto-approve batch size</span>
                  <select
                    class="text-input compact"
                    value={String(effectiveMaxBytes(dev))}
                    on:change={(e) => updatePolicy(dev, { maxAutoApproveBytes: Number(e.currentTarget.value) })}
                  >
                    <option value={String(10 * 1024 * 1024)}>10 MiB</option>
                    <option value={String(50 * 1024 * 1024)}>50 MiB</option>
                    <option value={String(100 * 1024 * 1024)}>100 MiB (Default)</option>
                    <option value={String(500 * 1024 * 1024)}>500 MiB</option>
                    <option value="-1">Unlimited</option>
                  </select>
                </label>

                <label class="policy-check">
                  <input
                    type="checkbox"
                    checked={!!dev.allowExecutables}
                    on:change={(e) => updatePolicy(dev, { allowExecutables: e.currentTarget.checked })}
                  />
                  <span>Allow auto-approving executables (e.g. .exe, .dmg, .apk)</span>
                </label>

                <label class="policy-check">
                  <input
                    type="checkbox"
                    checked={!!dev.allowScripts}
                    on:change={(e) => updatePolicy(dev, { allowScripts: e.currentTarget.checked })}
                  />
                  <span>Allow auto-approving scripts (e.g. .sh, .ps1, .bat)</span>
                </label>
              </div>
            {/if}
          </div>
        </div>
      {/each}
    </div>
  {:else}
    <!-- RICH INFORMATIVE EMPTY STATE -->
    <div class="devices-empty-guide">
      <div class="empty-icon-shield">🛡️</div>
      <h3>No Trusted Devices Paired Yet</h3>
      <p class="empty-intro">
        Pairing creates a cryptographic trust relationship between your desktop and mobile phone so you don't have to scan QR codes or approve transfers every single time.
      </p>

      <div class="guide-cards-grid">
        <div class="guide-card">
          <span class="guide-icon">🔒</span>
          <strong>Ed25519 Signatures</strong>
          <p>Every paired phone generates a private key stored strictly in hardware/browser keystore. Zero passwords or cloud accounts.</p>
        </div>

        <div class="guide-card">
          <span class="guide-icon">⚡</span>
          <strong>Automatic Approval</strong>
          <p>When enabled, files sent from your paired phone bypass the manual confirmation prompt and stream directly to your receive folder.</p>
        </div>

        <div class="guide-card">
          <span class="guide-icon">🛡️</span>
          <strong>Revoke Any Time</strong>
          <p>You maintain complete control. Revoking a device instantly drops its cryptographic capability on the local network.</p>
        </div>
      </div>

      <div class="how-to-pair-box">
        <strong>How to Pair:</strong>
        <span>Start a session in <strong>Personal Transfer</strong> and tap "Pair Device" on your phone's browser when scanning the QR code.</span>
      </div>
    </div>
  {/if}
</div>

<!-- Confirm Revoke Modal -->
<ConfirmModal
  isOpen={confirmRevokeOpen}
  title="Revoke Trusted Device?"
  message={`Are you sure you want to revoke trust for "${deviceToRevoke?.name}"? Future file transfers from this device will require manual pairing again.`}
  confirmText="Revoke Device"
  danger={true}
  onConfirm={confirmRevoke}
  onCancel={() => { confirmRevokeOpen = false }}
/>

<style>
  .devices-view-container {
    display: flex;
    flex-direction: column;
    gap: 16px;
    width: 100%;
  }

  .pairing-alert-banner {
    background: var(--warning-subtle);
    border: 1.5px solid var(--warning-border);
    border-radius: var(--radius-md, 10px);
    padding: 14px 18px;
    display: flex;
    align-items: center;
    gap: 16px;
  }

  .alert-icon-box {
    font-size: 24px;
  }

  .alert-text-col {
    flex: 1;
    min-width: 0;
  }

  .alert-text-col strong {
    font-size: 14px;
    display: block;
    margin-bottom: 2px;
  }

  .alert-text-col p {
    margin: 0;
    font-size: 12.5px;
    color: var(--text-secondary);
  }

  .alert-actions-row {
    display: flex;
    gap: 8px;
  }

  .devices-header-card {
    background: var(--bg-surface);
    border: 1px solid var(--border-base);
    border-radius: var(--radius-lg, 14px);
    box-shadow: var(--shadow-sm);
    padding: 20px 24px;
    display: flex;
    justify-content: space-between;
    align-items: center;
  }

  .header-text-group h3 {
    margin: 0 0 4px;
    font-size: 18px;
    font-weight: 700;
  }

  .header-text-group p {
    margin: 0;
    font-size: 13px;
    color: var(--text-secondary);
    max-width: 600px;
  }

  .header-actions {
    display: flex;
    gap: 8px;
  }

  /* DEVICES GRID */
  .devices-grid {
    display: grid;
    grid-template-columns: repeat(auto-fill, minmax(320px, 1fr));
    gap: 16px;
  }

  .device-card {
    background: var(--bg-surface);
    border: 1px solid var(--border-base);
    border-radius: var(--radius-lg, 14px);
    box-shadow: var(--shadow-sm);
    padding: 18px 20px;
    display: flex;
    flex-direction: column;
    gap: 14px;
  }

  .device-card-head {
    display: flex;
    align-items: center;
    gap: 12px;
  }

  .device-avatar-icon {
    width: 36px;
    height: 36px;
    border-radius: 50%;
    background: var(--accent-subtle);
    color: var(--accent);
    display: flex;
    align-items: center;
    justify-content: center;
    font-size: 16px;
  }

  .device-identity {
    flex: 1;
    min-width: 0;
    display: flex;
    flex-direction: column;
  }

  .device-name {
    font-size: 14px;
    font-weight: 700;
    color: var(--text-primary);
  }

  .device-fingerprint {
    font-family: monospace;
    font-size: 11px;
    color: var(--text-muted);
  }

  .ghost-danger {
    background: transparent;
    color: var(--danger);
    border: 1px solid var(--danger-border);
  }

  .ghost-danger:hover {
    background: var(--danger-subtle);
  }

  .device-meta-row {
    display: flex;
    justify-content: space-between;
    padding: 8px 12px;
    background: var(--bg-surface-elevated);
    border-radius: var(--radius-sm, 6px);
    font-size: 11.5px;
  }

  .meta-lbl {
    color: var(--text-muted);
    margin-right: 4px;
  }

  .meta-val {
    color: var(--text-primary);
    font-weight: 500;
  }

  .auto-approve-toggle-box {
    border-top: 1px solid var(--border-subtle);
    padding-top: 12px;
    display: flex;
    flex-direction: column;
    gap: 10px;
  }

  .device-policy-rules {
    background: var(--bg-surface-elevated);
    border: 1px solid var(--border-subtle);
    border-radius: var(--radius-sm, 8px);
    padding: 10px 12px;
    display: flex;
    flex-direction: column;
    gap: 8px;
  }

  .policy-field {
    display: flex;
    align-items: center;
    justify-content: space-between;
    gap: 8px;
    font-size: 12px;
    color: var(--text-secondary);
  }

  .policy-check {
    display: flex;
    align-items: center;
    gap: 8px;
    font-size: 11.5px;
    color: var(--text-secondary);
    cursor: pointer;
  }

  .toggle-label {
    display: flex;
    align-items: flex-start;
    gap: 10px;
    cursor: pointer;
  }

  .toggle-label input {
    width: 16px;
    height: 16px;
    margin-top: 2px;
    accent-color: var(--accent);
    cursor: pointer;
  }

  .toggle-text strong {
    display: block;
    font-size: 12.5px;
    color: var(--text-primary);
  }

  .toggle-text small {
    display: block;
    font-size: 11px;
    color: var(--text-muted);
    line-height: 1.35;
  }

  /* INFORMATIVE EMPTY GUIDE */
  .devices-empty-guide {
    background: var(--bg-surface);
    border: 1px solid var(--border-base);
    border-radius: var(--radius-lg, 14px);
    box-shadow: var(--shadow-sm);
    padding: 36px 32px;
    display: flex;
    flex-direction: column;
    align-items: center;
    text-align: center;
    gap: 14px;
  }

  .empty-icon-shield {
    font-size: 40px;
  }

  .devices-empty-guide h3 {
    margin: 0;
    font-size: 18px;
    font-weight: 700;
  }

  .empty-intro {
    margin: 0;
    font-size: 13.5px;
    color: var(--text-secondary);
    max-width: 580px;
    line-height: 1.5;
  }

  .guide-cards-grid {
    display: grid;
    grid-template-columns: repeat(3, 1fr);
    gap: 16px;
    width: 100%;
    max-width: 780px;
    margin-top: 12px;
    text-align: left;
  }

  @media (max-width: 750px) {
    .guide-cards-grid {
      grid-template-columns: 1fr;
    }
  }

  .guide-card {
    background: var(--bg-surface-elevated);
    border: 1px solid var(--border-subtle);
    border-radius: var(--radius-md, 10px);
    padding: 16px;
    display: flex;
    flex-direction: column;
    gap: 6px;
  }

  .guide-icon {
    font-size: 20px;
  }

  .guide-card strong {
    font-size: 13px;
    color: var(--text-primary);
  }

  .guide-card p {
    margin: 0;
    font-size: 11.5px;
    color: var(--text-secondary);
    line-height: 1.4;
  }

  .how-to-pair-box {
    margin-top: 12px;
    padding: 10px 18px;
    border-radius: 999px;
    background: var(--accent-subtle);
    font-size: 12.5px;
    color: var(--accent);
  }

  .loading-state {
    padding: 36px;
    text-align: center;
    color: var(--text-muted);
  }
</style>
