<script lang="ts">
  import type { main, netenv, version, power, transfer } from '../../wailsjs/go/models'
  import { openExternalURL, formatBytes } from '../utils'
  import logoUrl from '../assets/images/logo-universal.png'

  export let settingsState: main.SettingsView
  export let interfaces: netenv.View[] = []
  export let networkState: netenv.NetworkState | null = null
  export let refreshingNetwork: boolean = false
  export let onRefreshNetwork: () => void = () => {}
  export let appVersionInfo: version.Info | null = null
  export let updateAvailableBadge: boolean = false
  export let savingSettings: boolean = false
  export let settingsSavedAlert: string = ''
  export let settingsError: string = ''
  export let sleepInhibition: power.Status | null = null
  export let storageStatus: transfer.StorageStatus | null = null
  export let onRefreshStorage: () => void = () => {}

  export let handleSelectDownloadDir: () => void = () => {}
  export let handleOpenDownloadFolder: () => void = () => {}
  export let handleSaveSettings: () => void = () => {}
  export let handleResetSettings: () => void = () => {}
  export let applyTheme: (theme: string) => void = () => {}
  export let openUpdateModal: () => void = () => {}

  let activeSection: 'general' | 'storage' | 'network' | 'appearance' | 'about' = 'general'

  $: uniqueInterfaces = interfaces.filter((item, index, self) =>
    index === self.findIndex((t) => t.name === item.name)
  )
</script>

<div class="settings-view-container">
  <!-- Alerts -->
  {#if settingsSavedAlert}
    <div class="settings-alert success">
      <span>✓</span>
      <span>{settingsSavedAlert}</span>
    </div>
  {/if}
  {#if settingsError}
    <div class="settings-alert error">
      <span>⚠</span>
      <span>{settingsError}</span>
    </div>
  {/if}

  <!-- Segmented Section Navigation -->
  <div class="settings-nav-bar">
    <button
      type="button"
      class="settings-nav-tab"
      class:active={activeSection === 'general'}
      on:click={() => { activeSection = 'general' }}
    >
      ⚙️ General
    </button>
    <button
      type="button"
      class="settings-nav-tab"
      class:active={activeSection === 'storage'}
      on:click={() => { activeSection = 'storage' }}
    >
      📁 Storage & Transfers
    </button>
    <button
      type="button"
      class="settings-nav-tab"
      class:active={activeSection === 'network'}
      on:click={() => { activeSection = 'network' }}
    >
      🌐 Network
    </button>
    <button
      type="button"
      class="settings-nav-tab"
      class:active={activeSection === 'appearance'}
      on:click={() => { activeSection = 'appearance' }}
    >
      🎨 Appearance
    </button>
    <button
      type="button"
      class="settings-nav-tab"
      class:active={activeSection === 'about'}
      on:click={() => { activeSection = 'about' }}
    >
      ℹ️ About & Updates
      {#if updateAvailableBadge}
        <span class="tab-update-dot"></span>
      {/if}
    </button>
  </div>

  <!-- SECTION 1: GENERAL -->
  {#if activeSection === 'general'}
    <div class="settings-card-body">
      <div class="card-section-head">
        <h4>Session Automation</h4>
        <p>Configure how Flitnix behaves when opened on your computer.</p>
      </div>

      <div class="setting-row">
        <label class="toggle-control-label">
          <input
            type="checkbox"
            bind:checked={settingsState.autoStartSession}
          />
          <div class="toggle-text-block">
            <strong>Automatically start pairing session on launch</strong>
            <span>Prepares the local server immediately when Flitnix opens so your phone can scan and connect right away.</span>
          </div>
        </label>
      </div>

      <div class="card-section-head" style="margin-top: 22px;">
        <h4>Always-Ready Tray & Autostart</h4>
        <p>Keep Flitnix available in the background so your devices can connect without opening the window first.</p>
      </div>

      <div class="setting-row">
        <label class="toggle-control-label">
          <input
            type="checkbox"
            bind:checked={settingsState.minimizeToTray}
            disabled={settingsState.traySupported === false}
          />
          <div class="toggle-text-block">
            <strong>Minimize to system tray / menu bar on close</strong>
            <span>Hides the window while keeping LAN discovery and incoming transfer approvals running.</span>
          </div>
        </label>
        {#if settingsState.traySupported === false}
          <span class="setting-hint">System tray indicator is unavailable in this desktop session; closing will quit Flitnix.</span>
        {/if}
      </div>

      <div class="setting-row" style="margin-top: 12px;">
        <label class="toggle-control-label">
          <input
            type="checkbox"
            bind:checked={settingsState.launchOnLogin}
            disabled={settingsState.autostartSupported === false}
          />
          <div class="toggle-text-block">
            <strong>Launch Flitnix silently when logging in</strong>
            <span>Starts Flitnix minimized to the tray on desktop login (no background telemetry or cloud login).</span>
          </div>
        </label>
      </div>

      <div class="card-section-head" style="margin-top: 22px;">
        <h4>Desktop Notifications</h4>
        <p>Control native OS alerts and interactive transfer approval prompts.</p>
      </div>

      <div class="setting-row">
        <label class="toggle-control-label">
          <input
            type="checkbox"
            bind:checked={settingsState.notificationsEnabled}
          />
          <div class="toggle-text-block">
            <strong>Enable native OS notifications</strong>
            <span>Notify when transfers arrive, finish, or need your decision while Flitnix is in the background.</span>
          </div>
        </label>
      </div>

      <div class="setting-row" style="margin-top: 12px;">
        <label class="toggle-control-label">
          <input
            type="checkbox"
            bind:checked={settingsState.quietMode}
            disabled={!settingsState.notificationsEnabled}
          />
          <div class="toggle-text-block">
            <strong>Pause notifications (Do Not Disturb)</strong>
            <span>Temporarily silence all desktop popups while still recording transfers in History.</span>
          </div>
        </label>
      </div>

      <div class="setting-row" style="margin-top: 12px;">
        <label class="toggle-control-label">
          <input
            type="checkbox"
            bind:checked={settingsState.hideNotificationFilenames}
            disabled={!settingsState.notificationsEnabled}
          />
          <div class="toggle-text-block">
            <strong>Hide file and sender names in notifications</strong>
            <span>Masks sensitive filenames, Quick Text snippets, and device names in OS banners.</span>
          </div>
        </label>
      </div>

      {#if settingsState.notificationsEnabled}
        <div class="setting-row" style="margin-top: 12px; padding-left: 16px; display: grid; grid-template-columns: repeat(auto-fit, minmax(220px, 1fr)); gap: 10px;">
          <label class="toggle-control-label">
            <input type="checkbox" bind:checked={settingsState.notifyIncomingTransfers} />
            <span class="toggle-text-block"><strong>Incoming transfer requests</strong></span>
          </label>
          <label class="toggle-control-label">
            <input type="checkbox" bind:checked={settingsState.notifyCompletedTransfers} />
            <span class="toggle-text-block"><strong>Completed transfers</strong></span>
          </label>
          <label class="toggle-control-label">
            <input type="checkbox" bind:checked={settingsState.notifyFailedTransfers} />
            <span class="toggle-text-block"><strong>Transfer failures</strong></span>
          </label>
          <label class="toggle-control-label">
            <input type="checkbox" bind:checked={settingsState.notifyPairingRequests} />
            <span class="toggle-text-block"><strong>Device pairing alerts</strong></span>
          </label>
          <label class="toggle-control-label">
            <input type="checkbox" bind:checked={settingsState.notifyGuestRequests} />
            <span class="toggle-text-block"><strong>Guest Drop Box alerts</strong></span>
          </label>
          <label class="toggle-control-label">
            <input type="checkbox" bind:checked={settingsState.notifyQuickText} />
            <span class="toggle-text-block"><strong>Quick Text while hidden</strong></span>
          </label>
        </div>
      {/if}

      {#if sleepInhibition}
        <div class="card-section-head" style="margin-top: 20px;">
          <h4>System Power & Sleep</h4>
          <p>Desktop sleep prevention during active network transfers.</p>
        </div>
        <div class="setting-row">
          <div class="info-block">
            <span class="info-label">Current Sleep Inhibition:</span>
            <span class="status-tag {sleepInhibition.active ? 'ready' : 'neutral'}">
              {sleepInhibition.active ? 'Active (Sleep Inhibited)' : 'Idle (Normal Sleep Allowed)'}
            </span>
          </div>
        </div>
      {/if}
    </div>

  <!-- SECTION 2: STORAGE & TRANSFERS -->
  {:else if activeSection === 'storage'}
    <div class="settings-card-body">
      <div class="card-section-head">
        <h4>Receive Directory</h4>
        <p>Incoming files and folders from paired devices and drop box guests are saved here.</p>
      </div>

      <div class="setting-row">
        <div class="path-control-box">
          <input
            type="text"
            class="text-input"
            readonly
            value={settingsState.downloadDir || settingsState.effectiveDir || 'Default (Downloads/Flitnix)'}
          />
          <div class="path-btn-group">
            <button type="button" class="button secondary small" on:click={handleSelectDownloadDir}>
              Browse…
            </button>
            <button type="button" class="button ghost small" on:click={handleOpenDownloadFolder}>
              Open Folder
            </button>
            {#if settingsState.downloadDir}
              <button
                type="button"
                class="button ghost small"
                on:click={() => { settingsState.downloadDir = ''; }}
              >
                Reset Default
              </button>
            {/if}
          </div>
        </div>
        <span class="setting-hint">Effective path: <code>{settingsState.effectiveDir || settingsState.downloadDir}</code></span>
      </div>

      <div class="card-section-head" style="margin-top: 22px;">
        <h4>File Metadata & Timestamps</h4>
        <p>Control how received file attributes are written to your storage.</p>
      </div>

      <div class="setting-row">
        <label class="toggle-control-label">
          <input
            type="checkbox"
            bind:checked={settingsState.preserveModTimes}
          />
          <div class="toggle-text-block">
            <strong>Preserve original file dates</strong>
            <span>Keep each received file's original modification timestamp from the sender instead of setting it to the receive time.</span>
          </div>
        </label>
      </div>

      <div class="card-section-head" style="margin-top: 22px;">
        <div style="display: flex; align-items: center; justify-content: space-between; gap: 12px;">
          <div>
            <h4>Storage Headroom & Protection</h4>
            <p>Incoming transfers check available space and hold temporary reservations to prevent filling your disk.</p>
          </div>
          <button type="button" class="button secondary small" on:click={onRefreshStorage}>
            Refresh Space
          </button>
        </div>
      </div>

      <div class="setting-row">
        {#if storageStatus}
          <div class="storage-card">
            <div class="storage-card-head">
              <span class="status-tag {storageStatus.status === 'critical' ? 'fail' : storageStatus.status === 'low' ? 'warn' : storageStatus.available ? 'ready' : 'neutral'}">
                {#if !storageStatus.available}
                  Storage Unknown
                {:else if storageStatus.status === 'critical'}
                  Critical Space
                {:else if storageStatus.status === 'low'}
                  Low Disk Space
                {:else}
                  Healthy
                {/if}
              </span>
              <span class="storage-target-note">
                {storageStatus.targetExists ? 'Directory ready' : 'Directory created on first transfer'}
                {#if storageStatus.probeDir && !storageStatus.targetExists}
                  · Checking parent <code>{storageStatus.probeDir}</code>
                {/if}
              </span>
            </div>

            {#if storageStatus.available}
              {@const usedPct = storageStatus.totalBytes > 0 ? Math.min(100, Math.round((storageStatus.usedBytes / storageStatus.totalBytes) * 100)) : 0}
              <div class="storage-bar-track" role="progressbar" aria-valuemin="0" aria-valuemax="100" aria-valuenow={usedPct}>
                <div class="storage-bar-fill {storageStatus.status}" style="width: {usedPct}%"></div>
              </div>

              <div class="storage-metrics-grid tabular">
                <div>
                  <span class="metric-label">Available Headroom</span>
                  <strong>{formatBytes(storageStatus.headroomBytes)}</strong>
                </div>
                <div>
                  <span class="metric-label">OS Free Space</span>
                  <strong>{formatBytes(storageStatus.freeBytes)}</strong>
                </div>
                <div>
                  <span class="metric-label">Active Reservations</span>
                  <strong>{formatBytes(storageStatus.reservedBytes)}</strong>
                </div>
                <div>
                  <span class="metric-label">Total Volume</span>
                  <strong>{formatBytes(storageStatus.totalBytes)}</strong>
                </div>
              </div>
            {:else}
              <p class="setting-hint">
                Disk capacity metrics are unavailable for this filesystem{storageStatus.error ? ` (${storageStatus.error})` : ''}. Transfers remain permitted, with OS write-error protection active.
              </p>
            {/if}
          </div>
        {:else}
          <span class="setting-hint">Checking disk headroom…</span>
        {/if}
      </div>
    </div>

  <!-- SECTION 3: NETWORK -->
  {:else if activeSection === 'network'}
    <div class="settings-card-body">
      <div class="card-section-head">
        <h4>Preferred Network Interface</h4>
        <p>Choose the network adapter used for pairing and file transfers, or let Flitnix select automatically.</p>
      </div>

      <div class="setting-row">
        <div style="display: flex; gap: 10px; align-items: center; max-width: 580px;">
          <select id="iface-select" class="text-input" bind:value={settingsState.preferredInterface}>
            <option value="">Automatic Selection (Recommended)</option>
            {#each uniqueInterfaces as iface (iface.name)}
              <option value={iface.name}>
                {iface.friendlyName || iface.name} ({iface.ip}{iface.family ? ` · ${iface.family}` : ''})
              </option>
            {/each}
            {#if settingsState.preferredInterface && !uniqueInterfaces.some(i => i.name === settingsState.preferredInterface)}
              <option value={settingsState.preferredInterface}>
                {settingsState.preferredInterface} (Currently unavailable)
              </option>
            {/if}
          </select>
          <button
            type="button"
            class="button secondary small"
            on:click={onRefreshNetwork}
            disabled={refreshingNetwork}
            title="Refresh network status"
            aria-label="Refresh network status"
            style="white-space: nowrap; display: inline-flex; align-items: center; gap: 6px;"
          >
            <svg width="14" height="14" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" class:spinning={refreshingNetwork}>
              <path d="M21.5 2v6h-6M21.34 15.57a10 10 0 1 1-.57-8.38l5.67-5.67"/>
            </svg>
            {refreshingNetwork ? 'Refreshing…' : 'Refresh'}
          </button>
        </div>
        {#if networkState?.status === 'selected_unavailable' && networkState?.error}
          <div class="setting-hint" style="color: var(--warning-text); margin-top: 6px;">
            {networkState.error}
          </div>
        {/if}
        <span class="setting-hint">Flitnix automatically chooses the active LAN/Wi-Fi adapter when set to Automatic.</span>
      </div>
    </div>

  <!-- SECTION 4: APPEARANCE -->
  {:else if activeSection === 'appearance'}
    <div class="settings-card-body">
      <div class="card-section-head">
        <h4>Color Scheme & Theme</h4>
        <p>Choose the color scheme for the Flitnix desktop interface.</p>
      </div>

      <div class="theme-cards-grid">
        <button
          type="button"
          class="theme-card"
          class:active={settingsState.theme === 'system'}
          on:click={() => { settingsState.theme = 'system'; applyTheme('system'); }}
        >
          <div class="theme-icon">💻</div>
          <strong>System Default</strong>
          <span>Automatically matches your OS dark or light appearance</span>
        </button>

        <button
          type="button"
          class="theme-card"
          class:active={settingsState.theme === 'light'}
          on:click={() => { settingsState.theme = 'light'; applyTheme('light'); }}
        >
          <div class="theme-icon">☀️</div>
          <strong>Light Theme</strong>
          <span>Clean Slate light surface with high-contrast text</span>
        </button>

        <button
          type="button"
          class="theme-card"
          class:active={settingsState.theme === 'dark'}
          on:click={() => { settingsState.theme = 'dark'; applyTheme('dark'); }}
        >
          <div class="theme-icon">🌙</div>
          <strong>Dark Theme</strong>
          <span>Deep Slate dark background optimized for night workflows</span>
        </button>
      </div>

      <div class="card-section-head" style="margin-top: 24px;">
        <h4>Display Language</h4>
        <p>Interface display language.</p>
      </div>

      <div class="setting-row">
        <div style="max-width: 320px;">
          <select id="language-select" class="text-input" bind:value={settingsState.locale}>
            <option value="system">System Default (English)</option>
            <option value="en">English (US)</option>
          </select>
        </div>
      </div>
    </div>

  <!-- SECTION 5: ABOUT & UPDATES -->
  {:else if activeSection === 'about'}
    <div class="settings-card-body">
      <div class="about-hero-box">
        <img src={logoUrl} alt="Flitnix Logo" class="about-logo-img" />
        <div class="about-meta-col">
          <h3>Flitnix</h3>
          <p>High-speed, private, offline-first local network file transfer.</p>
          <span class="version-tag monospace">
            {appVersionInfo ? `Version ${appVersionInfo.version}` : 'Version 0.2.7.4'}
          </span>
        </div>
      </div>

      <div class="about-details-grid">
        <!-- Update Subsystem Card -->
        <div class="about-subcard">
          <span class="subcard-title">Software Updates</span>
          <p>Check for official signed releases with SHA-256 and Ed25519 verification.</p>
          <div class="update-action-row">
            <button type="button" class="button primary small" on:click={openUpdateModal}>
              {updateAvailableBadge ? 'Update Available — Review' : 'Check for Updates'}
            </button>
            <span class="update-subtext">Queries GitHub Releases directly</span>
          </div>
        </div>

        <!-- Open Source & Security Card -->
        <div class="about-subcard">
          <span class="subcard-title">Privacy & Security Guarantee</span>
          <p>Zero cloud intermediaries, zero telemetry tracking, and zero external servers. All data streams strictly within your local network or mobile hotspot.</p>
          <div class="about-links-row">
            <button
              type="button"
              class="button ghost small"
              on:click={() => openExternalURL('https://github.com/RobinMillford/flitnix')}
            >
              GitHub Repository ↗
            </button>
          </div>
        </div>
      </div>
    </div>
  {/if}

  <!-- Persistent Settings Action Bar -->
  <div class="settings-action-bar">
    <button
      type="button"
      class="button primary"
      disabled={savingSettings}
      on:click={handleSaveSettings}
    >
      {savingSettings ? 'Saving Preferences…' : 'Save Preferences'}
    </button>
    <button
      type="button"
      class="button ghost"
      on:click={handleResetSettings}
    >
      Reset to Defaults
    </button>
  </div>
</div>

<style>
  .settings-view-container {
    display: flex;
    flex-direction: column;
    gap: 16px;
    width: 100%;
  }

  .settings-alert {
    padding: 12px 16px;
    border-radius: var(--radius-md, 10px);
    display: flex;
    align-items: center;
    gap: 10px;
    font-size: 13px;
  }

  .settings-alert.success {
    background: var(--success-subtle);
    border: 1px solid var(--success-border);
    color: var(--success);
  }

  .settings-alert.error {
    background: var(--danger-subtle);
    border: 1px solid var(--danger-border);
    color: var(--danger);
  }

  /* SEGMENTED NAVIGATION */
  .settings-nav-bar {
    display: flex;
    background: var(--bg-surface);
    border: 1px solid var(--border-base);
    border-radius: var(--radius-lg, 14px);
    padding: 6px;
    gap: 6px;
    overflow-x: auto;
    box-shadow: var(--shadow-sm);
  }

  .settings-nav-tab {
    display: inline-flex;
    align-items: center;
    gap: 8px;
    padding: 8px 16px;
    border-radius: var(--radius-md, 10px);
    background: transparent;
    border: none;
    font-size: 13px;
    font-weight: 600;
    color: var(--text-secondary);
    cursor: pointer;
    white-space: nowrap;
    transition: all var(--duration-fast, 150ms) ease;
    position: relative;
  }

  .settings-nav-tab:hover {
    color: var(--text-primary);
    background: var(--bg-surface-hover);
  }

  .settings-nav-tab.active {
    background: var(--accent);
    color: var(--accent-text);
  }

  .tab-update-dot {
    width: 7px;
    height: 7px;
    border-radius: 50%;
    background: var(--danger);
  }

  /* CARD BODY */
  .settings-card-body {
    background: var(--bg-surface);
    border: 1px solid var(--border-base);
    border-radius: var(--radius-lg, 14px);
    box-shadow: var(--shadow-sm);
    padding: 24px 28px;
    display: flex;
    flex-direction: column;
    gap: 16px;
  }

  .card-section-head h4 {
    margin: 0 0 4px;
    font-size: 16px;
    font-weight: 700;
  }

  .card-section-head p {
    margin: 0;
    font-size: 13px;
    color: var(--text-secondary);
  }

  .setting-row {
    display: flex;
    flex-direction: column;
    gap: 8px;
  }

  .toggle-control-label {
    display: flex;
    align-items: flex-start;
    gap: 12px;
    cursor: pointer;
    padding: 4px 0;
  }

  .toggle-control-label input {
    width: 18px;
    height: 18px;
    margin-top: 2px;
    accent-color: var(--accent);
    cursor: pointer;
  }

  .toggle-text-block strong {
    display: block;
    font-size: 13.5px;
    color: var(--text-primary);
  }

  .toggle-text-block span {
    display: block;
    font-size: 12px;
    color: var(--text-secondary);
    line-height: 1.4;
    margin-top: 2px;
  }

  .info-block {
    display: flex;
    align-items: center;
    gap: 10px;
    font-size: 13px;
  }

  .path-control-box {
    display: flex;
    gap: 10px;
    align-items: center;
    flex-wrap: wrap;
  }

  .path-control-box input {
    flex: 1;
    min-width: 280px;
  }

  .path-btn-group {
    display: flex;
    gap: 8px;
  }

  .setting-hint {
    font-size: 11.5px;
    color: var(--text-muted);
  }

  /* THEME CARDS */
  .theme-cards-grid {
    display: grid;
    grid-template-columns: repeat(3, 1fr);
    gap: 14px;
  }

  @media (max-width: 700px) {
    .theme-cards-grid {
      grid-template-columns: 1fr;
    }
  }

  .theme-card {
    background: var(--bg-surface-elevated);
    border: 1.5px solid var(--border-base);
    border-radius: var(--radius-md, 10px);
    padding: 16px;
    text-align: left;
    cursor: pointer;
    display: flex;
    flex-direction: column;
    gap: 6px;
    transition: all var(--duration-fast, 150ms) ease;
  }

  .theme-card:hover {
    border-color: var(--accent);
  }

  .theme-card.active {
    border-color: var(--accent);
    background: var(--bg-surface-selected);
  }

  .theme-icon {
    font-size: 24px;
  }

  .theme-card strong {
    font-size: 13.5px;
    color: var(--text-primary);
  }

  .theme-card span {
    font-size: 11.5px;
    color: var(--text-secondary);
    line-height: 1.35;
  }

  /* ABOUT HERO */
  .about-hero-box {
    display: flex;
    align-items: center;
    gap: 18px;
    padding: 16px;
    background: var(--bg-surface-elevated);
    border-radius: var(--radius-md, 10px);
    border: 1px solid var(--border-subtle);
  }

  .about-logo-img {
    width: 52px;
    height: 52px;
    object-fit: contain;
  }

  .about-meta-col h3 {
    margin: 0;
    font-size: 18px;
    font-weight: 700;
  }

  .about-meta-col p {
    margin: 2px 0 6px;
    font-size: 12.5px;
    color: var(--text-secondary);
  }

  .version-tag {
    font-size: 11.5px;
    font-weight: 600;
    color: var(--accent);
    background: var(--accent-subtle);
    padding: 2px 8px;
    border-radius: 4px;
  }

  .about-details-grid {
    display: grid;
    grid-template-columns: 1fr 1fr;
    gap: 16px;
  }

  @media (max-width: 700px) {
    .about-details-grid {
      grid-template-columns: 1fr;
    }
  }

  .about-subcard {
    background: var(--bg-surface-elevated);
    border: 1px solid var(--border-subtle);
    border-radius: var(--radius-md, 10px);
    padding: 16px;
    display: flex;
    flex-direction: column;
    gap: 10px;
  }

  .subcard-title {
    font-size: 13.5px;
    font-weight: 700;
  }

  .about-subcard p {
    margin: 0;
    font-size: 12px;
    color: var(--text-secondary);
    line-height: 1.4;
  }

  .update-action-row {
    display: flex;
    align-items: center;
    gap: 10px;
    margin-top: auto;
  }

  .update-subtext {
    font-size: 11px;
    color: var(--text-muted);
  }

  /* ACTION BAR */
  .settings-action-bar {
    background: var(--bg-surface);
    border: 1px solid var(--border-base);
    border-radius: var(--radius-lg, 14px);
    padding: 14px 24px;
    display: flex;
    justify-content: space-between;
    align-items: center;
    box-shadow: var(--shadow-sm);
  }

  .storage-card {
    background: var(--bg-surface-elevated);
    border: 1px solid var(--border-base);
    border-radius: var(--radius-md, 10px);
    padding: 14px 16px;
    display: flex;
    flex-direction: column;
    gap: 12px;
  }

  .storage-card-head {
    display: flex;
    align-items: center;
    justify-content: space-between;
    gap: 10px;
    flex-wrap: wrap;
  }

  .storage-target-note {
    font-size: 11.5px;
    color: var(--text-secondary);
  }

  .storage-bar-track {
    width: 100%;
    height: 8px;
    border-radius: 999px;
    background: var(--bg-app);
    overflow: hidden;
    border: 1px solid var(--border-subtle);
  }

  .storage-bar-fill {
    height: 100%;
    background: var(--accent);
    border-radius: 999px;
    transition: width 200ms ease;
  }

  .storage-bar-fill.low {
    background: var(--warning, #f59e0b);
  }

  .storage-bar-fill.critical {
    background: var(--danger, #ef4444);
  }

  .storage-metrics-grid {
    display: grid;
    grid-template-columns: repeat(auto-fit, minmax(140px, 1fr));
    gap: 12px;
  }

  .storage-metrics-grid div {
    display: flex;
    flex-direction: column;
    gap: 2px;
  }

  .metric-label {
    font-size: 11px;
    color: var(--text-muted);
  }

  .storage-metrics-grid strong {
    font-size: 13.5px;
  }
</style>
