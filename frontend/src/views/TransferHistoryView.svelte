<script lang="ts">
  import { onMount } from 'svelte'
  import type { main } from '../../wailsjs/go/models'
  import {
    QueryTransferHistory,
    ClearTransferHistory,
    DeleteHistoryRecord,
    OpenHistoryFile,
    RevealHistoryFile,
    ReshareHistoryFile
  } from '../../wailsjs/go/main/App.js'
  import { formatBytes, fmtTime } from '../utils'

  export let historyRevision: number = 0
  export let handleOpenDownloadFolder: () => void = () => {}
  export let copyTextToClipboard: (text: string) => void = () => {}
  export let onReshared: () => void = () => {}
  export let showToast: (msg: string, type?: any) => void = () => {}

  const PAGE_SIZE = 25

  let searchQuery: string = ''
  let deviceFilter: string = ''
  let dateRange: string = 'all'
  let filterDirection: 'all' | 'incoming' | 'outgoing' | 'batch' | 'peer_outgoing' = 'all'
  let filterStatus: 'all' | 'completed' | 'failed' | 'canceled' = 'all'
  let currentPage: number = 0

  let records: main.HistoryRecordView[] = []
  let totalCount: number = 0
  let stats = {
    totalRecords: 0,
    completedCount: 0,
    failedCount: 0,
    canceledCount: 0,
    totalBytesTransferred: 0,
    incomingBytes: 0,
    outgoingBytes: 0
  }
  let storeSizeBytes: number = 0
  let loading: boolean = false
  let confirmClear: boolean = false
  let actionBusy: Record<string, boolean> = {}

  async function loadHistory(): Promise<void> {
    loading = true
    try {
      const res = await QueryTransferHistory({
        query: searchQuery,
        direction: filterDirection,
        status: filterStatus,
        device: deviceFilter,
        dateRange,
        limit: PAGE_SIZE,
        offset: currentPage * PAGE_SIZE
      } as main.HistoryFilterView)
      records = res?.records ?? []
      totalCount = res?.total ?? 0
      if (res?.stats) {
        stats = res.stats
      }
      storeSizeBytes = res?.storeSizeBytes ?? 0
    } catch (err: any) {
      showToast(err?.message || String(err), 'error')
    } finally {
      loading = false
    }
  }

  function resetPageAndLoad(): void {
    currentPage = 0
    void loadHistory()
  }

  $: if (historyRevision >= 0) {
    void loadHistory()
  }

  onMount(() => {
    void loadHistory()
  })

  async function handleOpenFile(item: main.HistoryRecordView): Promise<void> {
    actionBusy[item.id] = true
    try {
      await OpenHistoryFile(item.id)
    } catch (err: any) {
      showToast(err?.message || String(err), 'error')
      await loadHistory()
    } finally {
      actionBusy[item.id] = false
    }
  }

  async function handleRevealFile(item: main.HistoryRecordView): Promise<void> {
    actionBusy[item.id] = true
    try {
      await RevealHistoryFile(item.id)
    } catch (err: any) {
      showToast(err?.message || String(err), 'error')
      await loadHistory()
    } finally {
      actionBusy[item.id] = false
    }
  }

  async function handleReshareFile(item: main.HistoryRecordView): Promise<void> {
    actionBusy[item.id] = true
    try {
      await ReshareHistoryFile(item.id)
      onReshared()
      showToast(`Added "${item.displayName}" to Personal Transfer share queue`, 'success')
    } catch (err: any) {
      showToast(err?.message || String(err), 'error')
      await loadHistory()
    } finally {
      actionBusy[item.id] = false
    }
  }

  async function handleDeleteRecord(item: main.HistoryRecordView): Promise<void> {
    actionBusy[item.id] = true
    try {
      await DeleteHistoryRecord(item.id)
      await loadHistory()
    } catch (err: any) {
      showToast(err?.message || String(err), 'error')
    } finally {
      actionBusy[item.id] = false
    }
  }

  async function handleClearAll(): Promise<void> {
    try {
      await ClearTransferHistory()
      confirmClear = false
      currentPage = 0
      await loadHistory()
      showToast('Transfer history cleared (files on disk untouched)', 'info')
    } catch (err: any) {
      showToast(err?.message || String(err), 'error')
    }
  }

  $: totalPages = Math.max(1, Math.ceil(totalCount / PAGE_SIZE))
</script>

<div class="history-view-container">
  <!-- Summary Stats Strip -->
  <div class="history-stats-grid tabular">
    <div class="stat-pill">
      <span class="stat-label">Total Transfers</span>
      <strong>{stats.totalRecords}</strong>
    </div>
    <div class="stat-pill">
      <span class="stat-label">Completed</span>
      <strong class="stat-ok">{stats.completedCount}</strong>
    </div>
    <div class="stat-pill">
      <span class="stat-label">Failed / Canceled</span>
      <strong>{stats.failedCount + stats.canceledCount}</strong>
    </div>
    <div class="stat-pill">
      <span class="stat-label">Data Transferred</span>
      <strong>{formatBytes(stats.totalBytesTransferred)}</strong>
    </div>
  </div>

  <!-- Controls Card -->
  <div class="history-controls-card">
    <div class="search-actions-row">
      <div class="search-box">
        <svg class="search-icon" width="16" height="16" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2">
          <circle cx="11" cy="11" r="8"/>
          <line x1="21" y1="21" x2="16.65" y2="16.65"/>
        </svg>
        <input
          type="text"
          class="search-input"
          placeholder="Search files, device name, or SHA-256…"
          bind:value={searchQuery}
          on:input={resetPageAndLoad}
        />
        {#if searchQuery}
          <button type="button" class="clear-search-btn" on:click={() => { searchQuery = ''; resetPageAndLoad() }}>×</button>
        {/if}
      </div>

      <input
        type="text"
        class="device-filter-input"
        placeholder="Device filter…"
        aria-label="Filter by device name"
        bind:value={deviceFilter}
        on:input={resetPageAndLoad}
      />

      <div class="open-folder-wrap" style="display: flex; gap: 8px; flex-wrap: wrap;">
        <button type="button" class="button secondary small" on:click={handleOpenDownloadFolder}>
          📁 Open Receive Folder
        </button>
        {#if confirmClear}
          <button type="button" class="button danger small" on:click={handleClearAll}>
            Confirm Clear All
          </button>
          <button type="button" class="button ghost small" on:click={() => { confirmClear = false }}>
            Cancel
          </button>
        {:else if stats.totalRecords > 0}
          <button type="button" class="button ghost small" on:click={() => { confirmClear = true }}>
            Clear History
          </button>
        {/if}
      </div>
    </div>

    <!-- Direction, Date Range & Status Filters -->
    <div class="filters-row">
      <div class="chip-group">
        <button type="button" class="filter-chip" class:active={filterDirection === 'all'} on:click={() => { filterDirection = 'all'; resetPageAndLoad() }}>
          All ({stats.totalRecords})
        </button>
        <button type="button" class="filter-chip" class:active={filterDirection === 'incoming'} on:click={() => { filterDirection = 'incoming'; resetPageAndLoad() }}>
          ↙ Incoming
        </button>
        <button type="button" class="filter-chip" class:active={filterDirection === 'batch'} on:click={() => { filterDirection = 'batch'; resetPageAndLoad() }}>
          📦 Batches
        </button>
        <button type="button" class="filter-chip" class:active={filterDirection === 'outgoing'} on:click={() => { filterDirection = 'outgoing'; resetPageAndLoad() }}>
          ↗ Sent to Phone
        </button>
        <button type="button" class="filter-chip" class:active={filterDirection === 'peer_outgoing'} on:click={() => { filterDirection = 'peer_outgoing'; resetPageAndLoad() }}>
          💻 Computer Sharing
        </button>
      </div>

      <div class="status-select-wrap">
        <label for="date-filter">Period:</label>
        <select id="date-filter" class="text-input compact" bind:value={dateRange} on:change={resetPageAndLoad}>
          <option value="all">All Time</option>
          <option value="24h">Last 24 Hours</option>
          <option value="7d">Last 7 Days</option>
          <option value="30d">Last 30 Days</option>
        </select>

        <label for="status-filter">Status:</label>
        <select id="status-filter" class="text-input compact" bind:value={filterStatus} on:change={resetPageAndLoad}>
          <option value="all">All Statuses</option>
          <option value="completed">Completed</option>
          <option value="failed">Failed</option>
          <option value="canceled">Canceled / Declined</option>
        </select>
      </div>
    </div>
  </div>

  <!-- Items List Card -->
  <div class="history-list-card">
    <div class="list-header-row" style="display: flex; justify-content: space-between; align-items: center;">
      <h3>Transfer History ({totalCount})</h3>
      {#if storeSizeBytes > 0}
        <span class="detail-item tabular">History index: {formatBytes(storeSizeBytes)}</span>
      {/if}
    </div>

    <div class="items-flow">
      {#each records as item (item.id)}
        <div class="history-record-row">
          <div class="record-direction-icon" class:incoming={item.direction === 'incoming'} class:batch={item.direction === 'batch'}>
            {#if item.direction === 'incoming'}
              ↙
            {:else if item.direction === 'batch'}
              📦
            {:else if item.direction === 'peer_outgoing'}
              💻
            {:else}
              ↗
            {/if}
          </div>

          <div class="record-info-col">
            <div class="name-status-row">
              <strong class="record-title" title={item.displayName}>{item.displayName}</strong>
              <span class="status-badge {item.status}">{item.status}</span>
              {#if item.deviceName}
                <span class="device-pill">{item.deviceName}</span>
              {/if}
            </div>

            <div class="record-details-row">
              <span class="detail-item tabular">{formatBytes(item.transferred || item.size)} of {formatBytes(item.size)}</span>
              <span class="detail-sep">·</span>
              <span class="detail-item tabular">{fmtTime(item.timestamp)}</span>
              {#if item.itemCount > 1}
                <span class="detail-sep">·</span>
                <span class="detail-item tabular">{item.itemCount} items</span>
              {/if}
              {#if item.error}
                <span class="detail-sep">·</span>
                <span class="detail-error">{item.error}</span>
              {/if}
            </div>

            {#if item.sha256}
              <div class="sha-box">
                <button
                  type="button"
                  class="sha-btn tabular"
                  on:click={() => { copyTextToClipboard(item.sha256 || ''); showToast('Copied SHA-256', 'info'); }}
                  title="Click to copy SHA-256 hash"
                >
                  SHA256: {item.sha256.slice(0, 16)}…
                </button>
              </div>
            {/if}
          </div>

          <div class="record-actions-col">
            {#if item.canOpen}
              <button type="button" class="button secondary small" disabled={actionBusy[item.id]} on:click={() => handleOpenFile(item)}>
                Open
              </button>
            {/if}
            {#if item.canReveal}
              <button type="button" class="button ghost small" disabled={actionBusy[item.id]} on:click={() => handleRevealFile(item)}>
                Reveal
              </button>
            {/if}
            {#if item.canReshare}
              <button type="button" class="button ghost small" disabled={actionBusy[item.id]} on:click={() => handleReshareFile(item)}>
                Reshare
              </button>
            {/if}
            <button type="button" class="button ghost small" title="Remove entry from history" disabled={actionBusy[item.id]} on:click={() => handleDeleteRecord(item)}>
              ×
            </button>
          </div>
        </div>
      {:else}
        <div class="empty-history-box">
          <div class="empty-icon">📂</div>
          <h4>{loading ? 'Loading history…' : 'No transfer records found'}</h4>
          <p>
            {#if searchQuery || deviceFilter || filterDirection !== 'all' || filterStatus !== 'all' || dateRange !== 'all'}
              No transfers match your current filters.
            {:else}
              Completed, failed, and canceled transfers are saved across restarts so you can inspect, reveal, and reshare files anytime.
            {/if}
          </p>
        </div>
      {/each}
    </div>

    {#if totalCount > PAGE_SIZE}
      <div class="pagination-row">
        <button
          type="button"
          class="button secondary small"
          disabled={currentPage === 0 || loading}
          on:click={() => { currentPage = Math.max(0, currentPage - 1); void loadHistory() }}
        >
          ← Previous
        </button>
        <span class="detail-item tabular">Page {currentPage + 1} of {totalPages}</span>
        <button
          type="button"
          class="button secondary small"
          disabled={(currentPage + 1) >= totalPages || loading}
          on:click={() => { currentPage += 1; void loadHistory() }}
        >
          Next →
        </button>
      </div>
    {/if}
  </div>
</div>

<style>
  .history-view-container {
    display: flex;
    flex-direction: column;
    gap: 16px;
    width: 100%;
  }

  .history-stats-grid {
    display: grid;
    grid-template-columns: repeat(auto-fit, minmax(150px, 1fr));
    gap: 10px;
  }

  .stat-pill {
    background: var(--bg-surface);
    border: 1px solid var(--border-base);
    border-radius: var(--radius-md, 10px);
    padding: 12px 14px;
    display: flex;
    flex-direction: column;
    gap: 2px;
  }

  .stat-label {
    font-size: 11.5px;
    color: var(--text-muted);
  }

  .stat-pill strong {
    font-size: 18px;
    color: var(--text-primary);
  }

  .stat-pill strong.stat-ok {
    color: var(--success);
  }

  .history-controls-card {
    background: var(--bg-surface);
    border: 1px solid var(--border-base);
    border-radius: var(--radius-lg, 14px);
    box-shadow: var(--shadow-sm);
    padding: 16px 20px;
    display: flex;
    flex-direction: column;
    gap: 12px;
  }

  .search-actions-row {
    display: flex;
    justify-content: space-between;
    align-items: center;
    gap: 12px;
    flex-wrap: wrap;
  }

  .search-box {
    flex: 1;
    min-width: 240px;
    position: relative;
    display: flex;
    align-items: center;
  }

  .device-filter-input {
    min-width: 150px;
    padding: 8px 12px;
    background: var(--bg-surface-elevated);
    border: 1px solid var(--border-base);
    border-radius: var(--radius-md, 10px);
    font-size: 13px;
    color: var(--text-primary);
  }

  .search-icon {
    position: absolute;
    left: 12px;
    color: var(--text-muted);
    pointer-events: none;
  }

  .search-input {
    width: 100%;
    padding: 9px 36px;
    background: var(--bg-surface-elevated);
    border: 1px solid var(--border-base);
    border-radius: var(--radius-md, 10px);
    font-size: 13px;
    color: var(--text-primary);
  }

  .search-input:focus {
    outline: 2px solid var(--accent);
    background: var(--bg-surface);
  }

  .clear-search-btn {
    position: absolute;
    right: 10px;
    background: transparent;
    border: none;
    font-size: 16px;
    color: var(--text-muted);
    cursor: pointer;
  }

  .filters-row {
    display: flex;
    justify-content: space-between;
    align-items: center;
    gap: 14px;
    flex-wrap: wrap;
  }

  .chip-group {
    display: flex;
    gap: 8px;
    flex-wrap: wrap;
  }

  .filter-chip {
    padding: 5px 12px;
    border-radius: 999px;
    background: var(--bg-surface-elevated);
    border: 1px solid var(--border-base);
    font-size: 12px;
    font-weight: 600;
    color: var(--text-secondary);
    cursor: pointer;
    transition: all var(--duration-fast, 150ms) ease;
  }

  .filter-chip:hover {
    border-color: var(--accent);
  }

  .filter-chip.active {
    background: var(--accent);
    border-color: var(--accent);
    color: var(--accent-text);
  }

  .status-select-wrap {
    display: flex;
    align-items: center;
    gap: 8px;
    font-size: 12.5px;
    color: var(--text-secondary);
    flex-wrap: wrap;
  }

  .history-list-card {
    background: var(--bg-surface);
    border: 1px solid var(--border-base);
    border-radius: var(--radius-lg, 14px);
    box-shadow: var(--shadow-sm);
    padding: 20px 24px;
    display: flex;
    flex-direction: column;
    gap: 14px;
  }

  .list-header-row h3 {
    margin: 0;
    font-size: 16px;
    font-weight: 700;
  }

  .items-flow {
    display: flex;
    flex-direction: column;
    gap: 10px;
  }

  .history-record-row {
    display: flex;
    align-items: center;
    gap: 14px;
    padding: 12px 16px;
    background: var(--bg-surface-elevated);
    border: 1px solid var(--border-subtle);
    border-radius: var(--radius-md, 10px);
    flex-wrap: wrap;
  }

  .history-record-row:hover {
    border-color: var(--accent-border);
  }

  .record-direction-icon {
    width: 36px;
    height: 36px;
    border-radius: 50%;
    background: var(--accent-subtle);
    color: var(--accent);
    display: flex;
    align-items: center;
    justify-content: center;
    font-size: 16px;
    font-weight: 700;
  }

  .record-direction-icon.incoming {
    background: var(--success-subtle);
    color: var(--success);
  }

  .record-direction-icon.batch {
    background: var(--warning-subtle);
    color: var(--warning);
  }

  .record-info-col {
    flex: 1;
    min-width: 220px;
    display: flex;
    flex-direction: column;
    gap: 3px;
  }

  .name-status-row {
    display: flex;
    align-items: center;
    gap: 8px;
    flex-wrap: wrap;
  }

  .record-title {
    font-size: 13.5px;
    color: var(--text-primary);
    white-space: nowrap;
    overflow: hidden;
    text-overflow: ellipsis;
    max-width: 400px;
  }

  .device-pill {
    font-size: 11px;
    padding: 1px 7px;
    border-radius: 999px;
    background: var(--bg-surface);
    border: 1px solid var(--border-base);
    color: var(--text-secondary);
  }

  .status-badge {
    font-size: 10px;
    font-weight: 700;
    padding: 1px 6px;
    border-radius: 4px;
    text-transform: uppercase;
  }

  .status-badge.completed {
    background: var(--success-subtle);
    color: var(--success);
  }

  .status-badge.failed {
    background: var(--danger-subtle);
    color: var(--danger);
  }

  .status-badge.canceled, .status-badge.rejected, .status-badge.discarded {
    background: var(--bg-surface-secondary);
    color: var(--text-muted);
  }

  .record-details-row {
    display: flex;
    align-items: center;
    gap: 6px;
    font-size: 12px;
    color: var(--text-secondary);
    flex-wrap: wrap;
  }

  .detail-sep {
    color: var(--text-muted);
  }

  .detail-error {
    color: var(--danger);
  }

  .sha-box {
    margin-top: 2px;
  }

  .sha-btn {
    background: transparent;
    border: none;
    font-family: monospace;
    font-size: 10.5px;
    color: var(--text-muted);
    cursor: pointer;
    padding: 0;
  }

  .sha-btn:hover {
    color: var(--accent);
    text-decoration: underline;
  }

  .record-actions-col {
    display: flex;
    gap: 6px;
    flex-wrap: wrap;
  }

  .pagination-row {
    display: flex;
    align-items: center;
    justify-content: space-between;
    padding-top: 8px;
    border-top: 1px solid var(--border-subtle);
  }

  .empty-history-box {
    padding: 36px;
    text-align: center;
    display: flex;
    flex-direction: column;
    align-items: center;
    gap: 8px;
  }

  .empty-icon {
    font-size: 32px;
  }

  .empty-history-box h4 {
    margin: 0;
    font-size: 15px;
  }

  .empty-history-box p {
    margin: 0;
    font-size: 12.5px;
    color: var(--text-muted);
    max-width: 440px;
  }
</style>
