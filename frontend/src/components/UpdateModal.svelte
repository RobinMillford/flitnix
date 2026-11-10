<script lang="ts">
  import { onMount, onDestroy } from 'svelte';
  import { focusTrap } from '../focusTrap';
  import { formatBytes, speed, openExternalURL } from '../utils';
  import ProgressBar from './ProgressBar.svelte';
  import type { update, version } from '../../wailsjs/go/models';
  import { CheckForUpdates, DownloadUpdate, ApplyUpdate } from '../../wailsjs/go/main/App';
  import { EventsOn, EventsOff } from '../../wailsjs/runtime/runtime';

  export let isOpen: boolean = false;
  export let currentVersion: string = '0.2.7.4';
  export let onClose: () => void = () => {};

  let checkResult: update.CheckResult | null = null;
  let progress: update.Progress | null = null;
  let checking: boolean = false;
  let downloading: boolean = false;
  let applying: boolean = false;
  let errorMessage: string = '';

  $: isReadyToInstall = progress?.status === 'ready_to_install';
  $: isVerifying = progress?.status === 'verifying';
  $: isDownloading = progress?.status === 'downloading';

  onMount(() => {
    EventsOn('update:progress', handleProgressEvent);
    if (isOpen) {
      runCheck(false);
    }
  });

  onDestroy(() => {
    EventsOff('update:progress');
  });

  function handleProgressEvent(p: update.Progress) {
    progress = p;
    if (p.status === 'error') {
      errorMessage = p.error || 'Update process encountered an error.';
      downloading = false;
    } else if (p.status === 'ready_to_install') {
      downloading = false;
    }
  }

  export async function runCheck(force: boolean = true) {
    checking = true;
    errorMessage = '';
    progress = null;
    try {
      checkResult = await CheckForUpdates(force);
      if (checkResult.error) {
        errorMessage = checkResult.error;
      }
    } catch (e: any) {
      errorMessage = e?.message || String(e);
    } finally {
      checking = false;
    }
  }

  async function startDownload() {
    if (!checkResult?.updateAvailable) return;
    downloading = true;
    errorMessage = '';
    progress = {
      status: 'downloading',
      downloadedBytes: 0,
      totalBytes: checkResult.release?.asset?.size || 0,
      percentage: 0,
      speedBytesPerSec: 0,
      stagedFilePath: '',
      targetInstallPath: '',
      error: ''
    };
    try {
      const res = await DownloadUpdate();
      progress = res;
      if (res.status === 'error') {
        errorMessage = res.error || 'Download failed';
      }
    } catch (e: any) {
      errorMessage = e?.message || 'Download failed';
    } finally {
      downloading = false;
    }
  }

  async function handleApply() {
    applying = true;
    errorMessage = '';
    try {
      await ApplyUpdate();
      onClose();
    } catch (e: any) {
      errorMessage = e?.message || 'Failed to apply update';
    } finally {
      applying = false;
    }
  }

  function openReleasePage() {
    if (checkResult?.release?.htmlUrl) {
      openExternalURL(checkResult.release.htmlUrl);
    }
  }
</script>

{#if isOpen}
  <div
    class="modal-overlay"
    role="presentation"
    on:click|self={onClose}
  >
    <div
      class="update-dialog"
      role="dialog"
      aria-labelledby="update-dialog-title"
      aria-modal="true"
      use:focusTrap={{ onEscape: onClose }}
    >
      <div class="update-header">
        <div class="header-left">
          <div class="update-icon" aria-hidden="true">
            <svg width="20" height="20" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2">
              <path d="M21 12a9 9 0 0 0-9-9 9.75 9.75 0 0 0-6.74 2.74L3 8"/>
              <path d="M3 3v5h5"/>
              <path d="M3 12a9 9 0 0 0 9 9 9.75 9.75 0 0 0 6.74-2.74L21 16"/>
              <path d="M16 21h5v-5"/>
            </svg>
          </div>
          <div>
            <h2 id="update-dialog-title">Software Update</h2>
            <span class="header-subtitle">Flitnix Version Manager</span>
          </div>
        </div>
        <button type="button" class="close-btn" on:click={onClose} aria-label="Close dialog">✕</button>
      </div>

      <div class="update-body">
        {#if checking}
          <div class="state-container">
            <div class="spinner" aria-hidden="true"></div>
            <h3>Checking for updates…</h3>
            <p class="muted">Connecting to the official Flitnix release server…</p>
          </div>
        {:else if errorMessage}
          <div class="state-container error">
            <div class="status-badge error">Check Failed</div>
            <h3>Unable to Check for Updates</h3>
            <p class="error-msg">{errorMessage}</p>
            <div class="action-row">
              <button type="button" class="btn secondary" on:click={() => runCheck(true)}>Retry Check</button>
              {#if checkResult?.release?.htmlUrl}
                <button type="button" class="btn ghost" on:click={openReleasePage}>Open GitHub Releases</button>
              {/if}
            </div>
          </div>
        {:else if progress && isDownloading}
          <div class="state-container progress-state">
            <h3>Downloading Flitnix v{checkResult?.release?.version || 'Update'}</h3>
            <p class="muted">
              {formatBytes(progress.downloadedBytes)} of {formatBytes(progress.totalBytes)} · {speed(progress.speedBytesPerSec)}
            </p>
            <div style="width: 100%; margin: 12px 0;">
              <ProgressBar value={progress.percentage} showValue={true} />
            </div>
            <span class="hint">Please keep the application open during download.</span>
          </div>
        {:else if progress && isVerifying}
          <div class="state-container">
            <div class="spinner" aria-hidden="true"></div>
            <h3>Verifying Update Integrity</h3>
            <p class="muted">Validating SHA-256 digest and Ed25519 digital signature…</p>
          </div>
        {:else if progress && isReadyToInstall}
          <div class="state-container success">
            <div class="status-badge success">Verified & Ready</div>
            <h3>Flitnix v{checkResult?.release?.version} is Ready to Install</h3>
            <p class="muted">
              The update has been downloaded and cryptographically verified against the official release manifest.
            </p>
            <div class="release-summary-box">
              <div class="summary-item">
                <span>Target Version:</span>
                <strong>v{checkResult?.release?.version}</strong>
              </div>
              <div class="summary-item">
                <span>Installation Type:</span>
                <strong>{checkResult?.installType || 'Automatic'}</strong>
              </div>
            </div>
            <div class="action-row">
              <button
                type="button"
                class="btn primary"
                on:click={handleApply}
                disabled={applying}
              >
                {applying ? 'Launching…' : 'Install & Restart'}
              </button>
              <button type="button" class="btn ghost" on:click={onClose}>Remind Me Later</button>
            </div>
          </div>
        {:else if checkResult && checkResult.updateAvailable}
          <div class="update-available-flow">
            <div class="version-banner">
              <div class="version-tag current">
                <span>Current</span>
                <strong>v{currentVersion}</strong>
              </div>
              <span class="version-arrow">→</span>
              <div class="version-tag new">
                <span>New Release</span>
                <strong>v{checkResult.release?.version}</strong>
              </div>
            </div>

            <div class="release-details">
              <div class="meta-row">
                <span>Published:</span>
                <strong>{new Date(checkResult.release?.publishedAt).toLocaleDateString()}</strong>
                <span class="dot">·</span>
                <span>Download Size:</span>
                <strong>{formatBytes(checkResult.release?.asset?.size || 0)}</strong>
              </div>

              {#if checkResult.release?.releaseNotes}
                <div class="notes-container">
                  <h4 class="notes-title">Release Notes</h4>
                  <div class="notes-content">
                    {checkResult.release.releaseNotes}
                  </div>
                </div>
              {/if}
            </div>

            <div class="action-row">
              <button
                type="button"
                class="btn primary"
                on:click={startDownload}
                disabled={downloading}
              >
                Download & Install
              </button>
              <button type="button" class="btn ghost" on:click={openReleasePage}>
                View on GitHub
              </button>
              <button type="button" class="btn secondary" on:click={onClose}>
                Later
              </button>
            </div>
          </div>
        {:else if checkResult && !checkResult.updateAvailable}
          <div class="state-container up-to-date">
            <div class="status-badge success">Up to Date</div>
            <h3>You're using the latest version of Flitnix</h3>
            <p class="muted">
              Flitnix v{currentVersion} is currently the newest available release.
            </p>
            <div class="action-row">
              <button type="button" class="btn secondary" on:click={() => runCheck(true)}>
                Check Again
              </button>
              <button type="button" class="btn ghost" on:click={onClose}>
                Close
              </button>
            </div>
          </div>
        {/if}
      </div>
    </div>
  </div>
{/if}

<style>
  .modal-overlay {
    position: fixed;
    inset: 0;
    background: var(--bg-backdrop);
    backdrop-filter: blur(2px);
    display: flex;
    align-items: center;
    justify-content: center;
    z-index: 1000;
    animation: fadeIn var(--duration-fast) var(--easing-standard);
  }

  .update-dialog {
    width: 90%;
    max-width: 520px;
    background: var(--bg-surface);
    border: 1px solid var(--border-base);
    border-radius: var(--radius-xl);
    box-shadow: var(--shadow-lg);
    display: flex;
    flex-direction: column;
    overflow: hidden;
    animation: slideUp var(--duration-base) var(--easing-standard);
  }

  .update-header {
    display: flex;
    justify-content: space-between;
    align-items: center;
    padding: 16px 20px;
    border-bottom: 1px solid var(--border-subtle);
    background: var(--bg-subtle);
  }

  .header-left {
    display: flex;
    align-items: center;
    gap: 12px;
  }

  .update-icon {
    width: 36px;
    height: 36px;
    border-radius: var(--radius-md);
    background: var(--accent-subtle);
    color: var(--accent);
    display: flex;
    align-items: center;
    justify-content: center;
  }

  .update-header h2 {
    margin: 0;
    font-size: 16px;
    font-weight: 600;
    color: var(--text-primary);
  }

  .header-subtitle {
    font-size: 12px;
    color: var(--text-muted);
  }

  .close-btn {
    background: transparent;
    border: none;
    color: var(--text-muted);
    font-size: 18px;
    cursor: pointer;
    padding: 4px 8px;
    border-radius: var(--radius-sm);
  }

  .close-btn:hover {
    color: var(--text-primary);
  }

  .update-body {
    padding: 24px 20px;
    display: flex;
    flex-direction: column;
    gap: 16px;
  }

  .state-container {
    display: flex;
    flex-direction: column;
    align-items: center;
    text-align: center;
    gap: 10px;
    padding: 12px 0;
  }

  .state-container h3 {
    margin: 0;
    font-size: 17px;
    font-weight: 600;
    color: var(--text-primary);
  }

  .muted {
    margin: 0;
    font-size: 13.5px;
    color: var(--text-secondary);
  }

  .error-msg {
    margin: 0;
    font-size: 13px;
    color: var(--danger);
  }

  .status-badge {
    padding: 4px 10px;
    border-radius: 9999px;
    font-size: 11px;
    font-weight: 600;
    text-transform: uppercase;
    letter-spacing: 0.5px;
  }

  .status-badge.success {
    background: var(--success-subtle);
    color: var(--success);
    border: 1px solid var(--success-border);
  }

  .status-badge.error {
    background: var(--danger-subtle);
    color: var(--danger);
  }

  .spinner {
    width: 32px;
    height: 32px;
    border: 3px solid var(--border-base);
    border-top-color: var(--accent);
    border-radius: 50%;
    animation: spin 0.8s linear infinite;
  }

  .version-banner {
    display: flex;
    align-items: center;
    justify-content: center;
    gap: 16px;
    padding: 14px;
    background: var(--bg-subtle);
    border-radius: var(--radius-lg);
    border: 1px solid var(--border-subtle);
  }

  .version-tag {
    display: flex;
    flex-direction: column;
    align-items: center;
    gap: 2px;
  }

  .version-tag span {
    font-size: 11px;
    color: var(--text-muted);
    text-transform: uppercase;
  }

  .version-tag strong {
    font-size: 16px;
    font-family: var(--font-mono);
    color: var(--text-primary);
  }

  .version-tag.new strong {
    color: var(--accent);
  }

  .version-arrow {
    font-size: 18px;
    color: var(--text-muted);
  }

  .release-details {
    display: flex;
    flex-direction: column;
    gap: 12px;
  }

  .meta-row {
    font-size: 12.5px;
    color: var(--text-secondary);
    display: flex;
    align-items: center;
    justify-content: center;
    gap: 6px;
  }

  .meta-row strong {
    color: var(--text-primary);
  }

  .dot {
    color: var(--text-muted);
  }

  .notes-container {
    background: var(--bg-subtle);
    border: 1px solid var(--border-subtle);
    border-radius: var(--radius-md);
    padding: 12px 14px;
    max-height: 180px;
    overflow-y: auto;
  }

  .notes-title {
    margin: 0 0 6px 0;
    font-size: 12px;
    color: var(--text-secondary);
    text-transform: uppercase;
    letter-spacing: 0.5px;
  }

  .notes-content {
    font-size: 13px;
    color: var(--text-primary);
    white-space: pre-wrap;
    line-height: 1.5;
  }

  .release-summary-box {
    width: 100%;
    background: var(--bg-subtle);
    border: 1px solid var(--border-subtle);
    border-radius: var(--radius-md);
    padding: 12px 16px;
    display: flex;
    flex-direction: column;
    gap: 6px;
    box-sizing: border-box;
  }

  .summary-item {
    display: flex;
    justify-content: space-between;
    font-size: 13px;
    color: var(--text-secondary);
  }

  .summary-item strong {
    color: var(--text-primary);
    font-family: var(--font-mono);
  }

  .action-row {
    display: flex;
    justify-content: center;
    gap: 10px;
    margin-top: 8px;
  }

  .btn {
    padding: 8px 16px;
    border-radius: var(--radius-md);
    font-size: 13.5px;
    font-weight: 500;
    cursor: pointer;
    border: 1px solid transparent;
    transition: background-color var(--duration-fast) var(--easing-standard);
  }

  .btn.primary {
    background: var(--accent);
    color: var(--accent-text);
  }

  .btn.primary:hover:not(:disabled) {
    background: var(--accent-hover);
  }

  .btn.secondary {
    background: var(--bg-surface-active);
    color: var(--text-primary);
    border-color: var(--border-base);
  }

  .btn.secondary:hover {
    background: var(--bg-surface-hover);
  }

  .btn.ghost {
    background: transparent;
    color: var(--text-secondary);
  }

  .btn.ghost:hover {
    color: var(--text-primary);
    background: var(--bg-surface-hover);
  }

  .btn:disabled {
    opacity: 0.6;
    cursor: not-allowed;
  }

  .hint {
    font-size: 12px;
    color: var(--text-muted);
  }

  @keyframes fadeIn {
    from { opacity: 0; }
    to { opacity: 1; }
  }

  @keyframes slideUp {
    from { transform: translateY(12px); opacity: 0; }
    to { transform: translateY(0); opacity: 1; }
  }

  @keyframes spin {
    to { transform: rotate(360deg); }
  }

  @media (prefers-reduced-motion: reduce) {
    .modal-overlay,
    .update-dialog {
      animation: none;
    }
    .spinner {
      animation-duration: 2s;
    }
  }
</style>
