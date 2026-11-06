<script lang="ts">
  import { focusTrap } from '../focusTrap'
  import type { doctor } from '../../wailsjs/go/models'
  import { RunConnectionDiagnostics } from '../../wailsjs/go/main/App'

  export let isOpen: boolean = false
  export let report: doctor.DiagnosticReport | null = null
  export let onClose: () => void = () => {}
  export let onRegenerateQR: () => void = () => {}

  let loading: boolean = false
  let errorMsg: string = ''

  export async function runDiagnostics() {
    loading = true
    errorMsg = ''
    try {
      report = await RunConnectionDiagnostics()
    } catch (err: any) {
      errorMsg = err?.message || String(err)
    } finally {
      loading = false
    }
  }

  function handleKeydown(e: KeyboardEvent) {
    if (e.key === 'Escape') {
      e.stopPropagation()
      onClose()
    }
  }
</script>

<svelte:window on:keydown={handleKeydown} />

{#if isOpen}
  <!-- svelte-ignore a11y_click_events_have_key_events -->
  <div class="modal-backdrop" role="presentation" on:click={onClose} on:keydown={(e) => { if (e.key === 'Escape') onClose(); }}>
    <div
      class="modal-card doctor-modal"
      role="dialog"
      tabindex="-1"
      aria-modal="true"
      aria-labelledby="doctor-title"
      use:focusTrap={{ onEscape: onClose }}
      on:click|stopPropagation
    >
      <div class="doctor-header">
        <div class="doctor-title-group">
          <div class="doctor-badge {report?.overallStatus?.toLowerCase() || 'pass'}">
            {#if report?.overallStatus === 'FAIL'}
              ✕
            {:else if report?.overallStatus === 'WARNING'}
              ⚠
            {:else}
              ✓
            {/if}
          </div>
          <div>
            <h3 id="doctor-title">Connection Doctor</h3>
            <p>7-point network, socket & firewall diagnostics</p>
          </div>
        </div>
        <button type="button" class="close-btn" on:click={onClose} aria-label="Close dialog">×</button>
      </div>

      <div class="doctor-content">
        {#if errorMsg}
          <div class="doctor-error-banner">
            <span>⚠</span>
            <span>{errorMsg}</span>
          </div>
        {/if}

        <div class="doctor-status-banner {report?.overallStatus?.toLowerCase() || 'pass'}">
          <div class="status-summary">
            <strong>System Status: {report?.overallStatus || 'READY'}</strong>
            <p>
              {#if report?.overallStatus === 'FAIL'}
                Critical network issues detected. Transfers may not be reachable.
              {:else if report?.overallStatus === 'WARNING'}
                Warnings detected. Check interface binding or Wi-Fi isolation.
              {:else}
                All network diagnostics passed. Flitnix is ready for high-speed local transfer.
              {/if}
            </p>
          </div>
          <button
            type="button"
            class="button ghost small"
            on:click={runDiagnostics}
            disabled={loading}
          >
            {loading ? 'Testing…' : '↻ Re-test'}
          </button>
        </div>

        <div class="diagnostic-list">
          {#each (report?.items || []) as item (item.id)}
            <div class="diagnostic-row {item.status?.toLowerCase()}">
              <div class="diag-icon-cell">
                {#if item.status === 'PASS'}
                  <span class="icon-pass">✓</span>
                {:else if item.status === 'WARNING'}
                  <span class="icon-warn">⚠</span>
                {:else}
                  <span class="icon-fail">✕</span>
                {/if}
              </div>
              <div class="diag-info">
                <div class="diag-title-row">
                  <span class="diag-title">{item.title}</span>
                  <span class="diag-tag {item.status?.toLowerCase()}">{item.status}</span>
                </div>
                <div class="diag-message">{item.message}</div>
                {#if item.remediation}
                  <div class="diag-remediation">
                    <strong>Suggestion:</strong> {item.remediation}
                  </div>
                {/if}
              </div>
            </div>
          {:else}
            <div class="empty">No diagnostic items recorded. Click Re-test to run checks.</div>
          {/each}
        </div>
      </div>

      <div class="doctor-footer">
        <button
          type="button"
          class="button secondary small"
          on:click={() => { onRegenerateQR(); onClose(); }}
        >
          Regenerate QR Code
        </button>
        <button type="button" class="button primary small" on:click={onClose}>
          Done
        </button>
      </div>
    </div>
  </div>
{/if}

<style>
  .modal-backdrop {
    position: fixed;
    inset: 0;
    background: var(--bg-backdrop);
    backdrop-filter: blur(4px);
    display: flex;
    align-items: center;
    justify-content: center;
    z-index: 999;
    padding: 20px;
    animation: fadeIn 150ms ease;
  }

  .doctor-modal {
    background: var(--bg-modal);
    color: var(--text-primary);
    border: 1px solid var(--border-base);
    border-radius: var(--radius-lg, 14px);
    box-shadow: var(--shadow-lg);
    width: 100%;
    max-width: 620px;
    max-height: 85vh;
    display: flex;
    flex-direction: column;
    animation: scaleIn 150ms ease;
  }

  .doctor-header {
    padding: 20px 24px;
    border-bottom: 1px solid var(--border-base);
    display: flex;
    justify-content: space-between;
    align-items: center;
  }

  .doctor-title-group {
    display: flex;
    align-items: center;
    gap: 14px;
  }

  .doctor-badge {
    width: 38px;
    height: 38px;
    border-radius: 50%;
    display: flex;
    align-items: center;
    justify-content: center;
    font-size: 16px;
    font-weight: 700;
  }

  .doctor-badge.pass {
    background: var(--success-subtle);
    color: var(--success);
  }

  .doctor-badge.warning {
    background: var(--warning-subtle);
    color: var(--warning);
  }

  .doctor-badge.fail {
    background: var(--danger-subtle);
    color: var(--danger);
  }

  .doctor-title-group h3 {
    margin: 0;
    font-size: 18px;
    font-weight: 700;
  }

  .doctor-title-group p {
    margin: 2px 0 0;
    font-size: 12.5px;
    color: var(--text-secondary);
  }

  .close-btn {
    background: transparent;
    border: none;
    font-size: 24px;
    color: var(--text-secondary);
    cursor: pointer;
    line-height: 1;
    padding: 4px;
    border-radius: 6px;
  }

  .close-btn:hover {
    color: var(--text-primary);
    background: var(--bg-surface-elevated);
  }

  .doctor-content {
    padding: 20px 24px;
    overflow-y: auto;
    flex: 1;
    display: flex;
    flex-direction: column;
    gap: 16px;
  }

  .doctor-status-banner {
    padding: 14px 18px;
    border-radius: var(--radius-md, 10px);
    display: flex;
    justify-content: space-between;
    align-items: center;
    gap: 16px;
    border: 1px solid transparent;
  }

  .doctor-status-banner.pass {
    background: var(--success-subtle);
    border-color: var(--success-border);
  }

  .doctor-status-banner.warning {
    background: var(--warning-subtle);
    border-color: var(--warning-border);
  }

  .doctor-status-banner.fail {
    background: var(--danger-subtle);
    border-color: var(--danger-border);
  }

  .status-summary strong {
    font-size: 13.5px;
    display: block;
    margin-bottom: 2px;
  }

  .status-summary p {
    margin: 0;
    font-size: 12.5px;
    color: var(--text-secondary);
  }

  .diagnostic-list {
    display: flex;
    flex-direction: column;
    gap: 10px;
  }

  .diagnostic-row {
    display: flex;
    gap: 12px;
    padding: 12px 14px;
    border-radius: var(--radius-sm, 6px);
    background: var(--bg-surface-elevated);
    border: 1px solid var(--border-subtle);
  }

  .diag-icon-cell {
    padding-top: 2px;
  }

  .icon-pass { color: var(--success); font-weight: 700; }
  .icon-warn { color: var(--warning); font-weight: 700; }
  .icon-fail { color: var(--danger); font-weight: 700; }

  .diag-info {
    flex: 1;
    min-width: 0;
  }

  .diag-title-row {
    display: flex;
    justify-content: space-between;
    align-items: center;
    margin-bottom: 4px;
  }

  .diag-title {
    font-size: 13px;
    font-weight: 600;
  }

  .diag-tag {
    font-size: 10.5px;
    font-weight: 700;
    padding: 1px 6px;
    border-radius: 4px;
    text-transform: uppercase;
  }

  .diag-tag.pass {
    background: var(--success-subtle);
    color: var(--success);
  }

  .diag-tag.warning {
    background: var(--warning-subtle);
    color: var(--warning);
  }

  .diag-tag.fail {
    background: var(--danger-subtle);
    color: var(--danger);
  }

  .diag-message {
    font-size: 12px;
    color: var(--text-secondary);
    line-height: 1.4;
  }

  .diag-remediation {
    margin-top: 6px;
    font-size: 11.5px;
    color: var(--accent);
    background: var(--accent-subtle);
    padding: 6px 10px;
    border-radius: 4px;
  }

  .doctor-footer {
    padding: 16px 24px;
    border-top: 1px solid var(--border-base);
    display: flex;
    justify-content: space-between;
    align-items: center;
  }

  @keyframes fadeIn {
    from { opacity: 0; }
    to { opacity: 1; }
  }

  @keyframes scaleIn {
    from { transform: scale(0.97); opacity: 0; }
    to { transform: scale(1); opacity: 1; }
  }

  @media (prefers-reduced-motion: reduce) {
    .modal-backdrop, .doctor-modal {
      animation: none !important;
    }
  }
</style>
