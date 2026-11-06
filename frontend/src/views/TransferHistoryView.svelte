<script lang="ts">
  import type { main } from '../../wailsjs/go/models'
  import { formatBytes, fmtTime } from '../utils'

  export let uploads: main.UploadView[] = []
  export let downloads: main.DownloadView[] = []
  export let batches: main.BatchView[] = []

  export let handleOpenDownloadFolder: () => void = () => {}
  export let copyTextToClipboard: (text: string) => void = () => {}
  export let showToast: (msg: string, type?: any) => void = () => {}

  let searchQuery: string = ''
  let filterDirection: 'all' | 'incoming' | 'outgoing' | 'batches' = 'all'
  let filterStatus: 'all' | 'completed' | 'failed' | 'canceled' = 'all'

  interface HistoryItem {
    id: string
    name: string
    direction: 'incoming' | 'outgoing' | 'batch'
    size: number
    transferred: number
    state: string
    createdAt: any
    sha256?: string
    error?: string
    isBatch?: boolean
    batchItemsCount?: number
  }

  let allHistoryItems: HistoryItem[] = []

  $: allHistoryItems = [
    // Batches
    ...batches.filter(b => ['completed', 'failed', 'canceled', 'rejected'].includes(b.state)).map(b => ({
      id: b.id,
      name: b.rootName || `Batch (${b.totalItemCount} files)`,
      direction: 'batch' as const,
      size: b.declaredTotalBytes || 0,
      transferred: b.transferredBytes || 0,
      state: b.state,
      createdAt: b.createdAt,
      isBatch: true,
      batchItemsCount: b.totalItemCount,
      error: b.error
    })),
    // Single uploads
    ...uploads.filter(u => !['pending', 'approved', 'uploading'].includes(u.state)).map(u => ({
      id: u.id,
      name: u.name,
      direction: 'incoming' as const,
      size: u.size,
      transferred: u.uploaded,
      state: u.state,
      createdAt: u.created,
      sha256: u.sha256,
      error: u.reason
    })),
    // Downloads
    ...downloads.filter(d => !['starting', 'transferring', 'finalizing'].includes(d.state)).map(d => ({
      id: d.id,
      name: d.name,
      direction: 'outgoing' as const,
      size: d.size,
      transferred: d.written,
      state: d.state,
      createdAt: d.created,
      error: d.reason
    }))
  ].sort((a, b) => {
    const at = a.createdAt ? new Date(a.createdAt).getTime() : 0
    const bt = b.createdAt ? new Date(b.createdAt).getTime() : 0
    return bt - at
  })

  $: filteredItems = allHistoryItems.filter(item => {
    // 1. Search filter
    if (searchQuery.trim()) {
      const q = searchQuery.toLowerCase()
      if (!item.name.toLowerCase().includes(q)) return false
    }

    // 2. Direction filter
    if (filterDirection === 'incoming' && item.direction !== 'incoming') return false
    if (filterDirection === 'outgoing' && item.direction !== 'outgoing') return false
    if (filterDirection === 'batches' && item.direction !== 'batch') return false

    // 3. Status filter
    if (filterStatus !== 'all') {
      if (filterStatus === 'completed' && item.state !== 'completed' && item.state !== 'sent') return false
      if (filterStatus === 'failed' && item.state !== 'failed') return false
      if (filterStatus === 'canceled' && item.state !== 'canceled' && item.state !== 'rejected') return false
    }

    return true
  })
</script>

<div class="history-view-container">
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
          placeholder="Search transferred files by name…"
          bind:value={searchQuery}
        />
        {#if searchQuery}
          <button type="button" class="clear-search-btn" on:click={() => { searchQuery = '' }}>×</button>
        {/if}
      </div>

      <div class="open-folder-wrap">
        <button type="button" class="button secondary small" on:click={handleOpenDownloadFolder}>
          📁 Open Download Folder
        </button>
      </div>
    </div>

    <!-- Direction & Status Filter Chips -->
    <div class="filters-row">
      <div class="chip-group">
        <button
          type="button"
          class="filter-chip"
          class:active={filterDirection === 'all'}
          on:click={() => { filterDirection = 'all' }}
        >
          All Transfers ({allHistoryItems.length})
        </button>
        <button
          type="button"
          class="filter-chip"
          class:active={filterDirection === 'incoming'}
          on:click={() => { filterDirection = 'incoming' }}
        >
          ↙ Incoming from Phone
        </button>
        <button
          type="button"
          class="filter-chip"
          class:active={filterDirection === 'outgoing'}
          on:click={() => { filterDirection = 'outgoing' }}
        >
          ↗ Sent to Phone
        </button>
        <button
          type="button"
          class="filter-chip"
          class:active={filterDirection === 'batches'}
          on:click={() => { filterDirection = 'batches' }}
        >
          📦 Batches
        </button>
      </div>

      <div class="status-select-wrap">
        <label for="status-filter">Status:</label>
        <select id="status-filter" class="text-input compact" bind:value={filterStatus}>
          <option value="all">All Statuses</option>
          <option value="completed">Completed</option>
          <option value="failed">Failed</option>
          <option value="canceled">Canceled</option>
        </select>
      </div>
    </div>
  </div>

  <!-- Items List Card -->
  <div class="history-list-card">
    <div class="list-header-row">
      <h3>Transfer Records ({filteredItems.length})</h3>
    </div>

    <div class="items-flow">
      {#each filteredItems as item (item.id)}
        <div class="history-record-row">
          <div class="record-direction-icon" class:incoming={item.direction === 'incoming'} class:batch={item.direction === 'batch'}>
            {#if item.direction === 'incoming'}
              ↙
            {:else if item.direction === 'batch'}
              📦
            {:else}
              ↗
            {/if}
          </div>

          <div class="record-info-col">
            <div class="name-status-row">
              <strong class="record-title" title={item.name}>{item.name}</strong>
              <span class="status-badge {item.state}">{item.state === 'sent' ? 'sent to browser' : item.state}</span>
            </div>

            <div class="record-details-row">
              <span class="detail-item tabular">{formatBytes(item.transferred)} of {formatBytes(item.size)}</span>
              <span class="detail-sep">·</span>
              <span class="detail-item tabular">{fmtTime(item.createdAt)}</span>
              {#if item.isBatch}
                <span class="detail-sep">·</span>
                <span class="detail-item tabular">{item.batchItemsCount} files in batch</span>
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
            {#if (item.state === 'completed' || item.state === 'sent') && item.direction !== 'outgoing'}
              <button
                type="button"
                class="button ghost small"
                on:click={handleOpenDownloadFolder}
                title="Show in download folder"
              >
                Show in Folder
              </button>
            {/if}
          </div>
        </div>
      {:else}
        <div class="empty-history-box">
          <div class="empty-icon">📂</div>
          <h4>No transfer records found</h4>
          <p>
            {#if searchQuery}
              No transfers match your search "{searchQuery}".
            {:else}
              Transfers completed or attempted during active sessions will appear here with cryptographic hashes and file locations.
            {/if}
          </p>
        </div>
      {/each}
    </div>
  </div>
</div>

<style>
  .history-view-container {
    display: flex;
    flex-direction: column;
    gap: 16px;
    width: 100%;
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
    gap: 16px;
    flex-wrap: wrap;
  }

  .search-box {
    flex: 1;
    min-width: 260px;
    position: relative;
    display: flex;
    align-items: center;
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
    gap: 16px;
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
  }

  /* LIST CARD */
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
    transition: border-color var(--duration-fast, 150ms) ease;
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
    min-width: 0;
    display: flex;
    flex-direction: column;
    gap: 3px;
  }

  .name-status-row {
    display: flex;
    align-items: center;
    gap: 10px;
  }

  .record-title {
    font-size: 13.5px;
    color: var(--text-primary);
    white-space: nowrap;
    overflow: hidden;
    text-overflow: ellipsis;
  }

  .status-badge {
    font-size: 10px;
    font-weight: 700;
    padding: 1px 6px;
    border-radius: 4px;
    text-transform: uppercase;
  }

  .status-badge.completed, .status-badge.sent {
    background: var(--success-subtle);
    color: var(--success);
  }

  .status-badge.failed {
    background: var(--danger-subtle);
    color: var(--danger);
  }

  .status-badge.canceled, .status-badge.rejected {
    background: var(--bg-surface-secondary);
    color: var(--text-muted);
  }

  .record-details-row {
    display: flex;
    align-items: center;
    gap: 6px;
    font-size: 12px;
    color: var(--text-secondary);
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
    gap: 8px;
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
