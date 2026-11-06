<script lang="ts">
  import type { audit } from '../../wailsjs/go/models'
  import { fmtTime, formatBytes } from '../utils'
  import ConfirmModal from '../components/ConfirmModal.svelte'
  import { focusTrap } from '../focusTrap'

  export let auditEntries: audit.Entry[] = []
  export let totalEntries: number = 0
  export let loading: boolean = false
  export let filterType: string = ''
  export let filterOutcome: string = ''

  export let loadAuditLogs: () => void = () => {}
  export let clearAuditLogsAction: () => void = () => {}
  export let copyTextToClipboard: (text: string) => void = () => {}
  export let showToast: (msg: string, type?: any) => void = () => {}

  let searchQuery: string = ''
  let confirmClearOpen: boolean = false
  let inspectedEntry: audit.Entry | null = null

  // Pagination state
  let currentPage: number = 1
  const pageSize: number = 25

  $: filteredEntries = auditEntries.filter(entry => {
    // 1. Search query
    if (searchQuery.trim()) {
      const q = searchQuery.toLowerCase()
      const matchType = entry.eventType?.toLowerCase().includes(q)
      const matchRoom = entry.roomId?.toLowerCase().includes(q)
      const matchGuest = entry.guestName?.toLowerCase().includes(q) || entry.guestId?.toLowerCase().includes(q)
      const matchFile = entry.fileName?.toLowerCase().includes(q)
      const matchDetails = entry.details?.toLowerCase().includes(q)
      if (!matchType && !matchRoom && !matchGuest && !matchFile && !matchDetails) return false
    }

    // 2. Type filter
    if (filterType && entry.eventType !== filterType) return false

    // 3. Outcome filter
    if (filterOutcome && entry.outcome !== filterOutcome) return false

    return true
  })

  $: totalPages = Math.max(1, Math.ceil(filteredEntries.length / pageSize))
  $: paginatedEntries = filteredEntries.slice((currentPage - 1) * pageSize, currentPage * pageSize)

  $: if (searchQuery || filterType || filterOutcome) {
    currentPage = 1
  }

  function handleClearConfirm() {
    clearAuditLogsAction()
    showToast('Audit history cleared', 'info')
  }

  function formatEventName(type: string): string {
    return type.replace(/_/g, ' ')
  }

  function getBadgeClass(type: string): string {
    if (type.includes('approved') || type.includes('completed')) return 'badge-success'
    if (type.includes('rejected') || type.includes('revoked') || type.includes('fail')) return 'badge-danger'
    if (type.includes('joined') || type.includes('created')) return 'badge-info'
    return 'badge-neutral'
  }
</script>

<div class="audit-view-container">
  <!-- Controls Card -->
  <div class="audit-controls-card">
    <div class="search-actions-row">
      <div class="search-box">
        <svg class="search-icon" width="16" height="16" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2">
          <circle cx="11" cy="11" r="8"/>
          <line x1="21" y1="21" x2="16.65" y2="16.65"/>
        </svg>
        <input
          type="text"
          class="search-input"
          placeholder="Search audit trail by event, room, guest, or file…"
          bind:value={searchQuery}
        />
        {#if searchQuery}
          <button type="button" class="clear-search-btn" on:click={() => { searchQuery = '' }}>×</button>
        {/if}
      </div>

      <div class="header-btn-row">
        <button type="button" class="button ghost small" on:click={loadAuditLogs} disabled={loading}>
          ↻ Refresh
        </button>
        <button
          type="button"
          class="button danger small ghost-danger"
          on:click={() => { confirmClearOpen = true }}
          disabled={auditEntries.length === 0}
        >
          Clear History
        </button>
      </div>
    </div>

    <!-- Dropdown Filters -->
    <div class="filters-row">
      <div class="filter-item">
        <label for="audit-type-filter">Event Type:</label>
        <select id="audit-type-filter" class="text-input compact" bind:value={filterType}>
          <option value="">All Event Types</option>
          <option value="room_created">Room Created</option>
          <option value="room_ended">Room Ended</option>
          <option value="guest_joined">Guest Joined</option>
          <option value="guest_approved">Guest Approved</option>
          <option value="guest_rejected">Guest Rejected</option>
          <option value="guest_revoked">Guest Revoked</option>
          <option value="upload_approved">Upload Approved</option>
          <option value="upload_rejected">Upload Rejected</option>
          <option value="export_completed">Export Completed</option>
        </select>
      </div>

      <div class="filter-item">
        <label for="audit-outcome-filter">Outcome:</label>
        <select id="audit-outcome-filter" class="text-input compact" bind:value={filterOutcome}>
          <option value="">All Outcomes</option>
          <option value="success">Success</option>
          <option value="failure">Failure</option>
          <option value="rejected">Rejected</option>
          <option value="canceled">Canceled</option>
        </select>
      </div>

      <div class="filter-stats tabular">
        Showing {filteredEntries.length} of {totalEntries || auditEntries.length} total events
      </div>
    </div>
  </div>

  <!-- Audit Table Card -->
  <div class="audit-table-card">
    {#if loading}
      <div class="loading-state">Loading audit trail…</div>
    {:else if paginatedEntries.length > 0}
      <div class="table-responsive-box">
        <table class="audit-table">
          <thead>
            <tr>
              <th style="width: 130px;">Timestamp</th>
              <th style="width: 150px;">Event Type</th>
              <th style="width: 120px;">Room ID</th>
              <th style="width: 130px;">Guest</th>
              <th style="width: 90px;">Outcome</th>
              <th>Details</th>
              <th style="width: 70px; text-align: right;">Inspect</th>
            </tr>
          </thead>
          <tbody>
            {#each paginatedEntries as e (e.id)}
              <tr class="audit-tr">
                <td class="tabular time-cell">{fmtTime(e.timestamp)}</td>
                <td>
                  <span class="event-chip {getBadgeClass(e.eventType)}">
                    {formatEventName(e.eventType)}
                  </span>
                </td>
                <td>
                  {#if e.roomId}
                    <button
                      type="button"
                      class="id-copy-btn tabular"
                      on:click={() => { copyTextToClipboard(e.roomId || ''); showToast('Copied Room ID', 'info'); }}
                      title="Click to copy Room ID"
                    >
                      {e.roomId.slice(0, 8)}…
                    </button>
                  {:else}
                    <span class="muted">—</span>
                  {/if}
                </td>
                <td>
                  {#if e.guestName || e.guestId}
                    <span class="guest-name" title={e.guestId || ''}>
                      {e.guestName || (e.guestId ? e.guestId.slice(0, 8) + '…' : '—')}
                    </span>
                  {:else}
                    <span class="muted">—</span>
                  {/if}
                </td>
                <td>
                  <span class="outcome-tag {e.outcome?.toLowerCase()}">
                    {e.outcome || 'success'}
                  </span>
                </td>
                <td class="details-cell" title={e.details || ''}>
                  {e.details || '—'}
                </td>
                <td style="text-align: right;">
                  <button
                    type="button"
                    class="button ghost small inspect-btn"
                    on:click={() => { inspectedEntry = e }}
                    title="View complete event record"
                  >
                    View
                  </button>
                </td>
              </tr>
            {/each}
          </tbody>
        </table>
      </div>

      <!-- Pagination -->
      {#if totalPages > 1}
        <div class="pagination-footer">
          <span class="pagination-info tabular">
            Page {currentPage} of {totalPages} (Showing {(currentPage - 1) * pageSize + 1}–{Math.min(currentPage * pageSize, filteredEntries.length)})
          </span>
          <div class="pagination-buttons">
            <button
              type="button"
              class="button ghost small"
              on:click={() => { currentPage = Math.max(1, currentPage - 1) }}
              disabled={currentPage === 1}
            >
              ← Previous
            </button>
            <button
              type="button"
              class="button ghost small"
              on:click={() => { currentPage = Math.min(totalPages, currentPage + 1) }}
              disabled={currentPage === totalPages}
            >
              Next →
            </button>
          </div>
        </div>
      {/if}
    {:else}
      <div class="empty-audit-box">
        <div class="empty-icon">📜</div>
        <h4>No audit events match your filter</h4>
        <p>All administrative actions, device pairings, and room lifecycle changes are recorded in the local append-only audit trail.</p>
      </div>
    {/if}
  </div>
</div>

<!-- Clear History Confirmation Modal -->
<ConfirmModal
  isOpen={confirmClearOpen}
  title="Clear Local Audit Trail?"
  message="Are you sure you want to permanently clear the local JSONL audit trail? Forensic verification records will be reset."
  confirmText="Clear History"
  danger={true}
  onConfirm={handleClearConfirm}
  onCancel={() => { confirmClearOpen = false }}
/>

<!-- Event Forensic Inspector Modal -->
{#if inspectedEntry}
  <!-- svelte-ignore a11y_click_events_have_key_events -->
  <div class="modal-backdrop" role="presentation" on:click={() => { inspectedEntry = null }} on:keydown={(e) => { if (e.key === 'Escape') inspectedEntry = null }}>
    <div
      class="modal-card inspector-modal"
      role="dialog"
      tabindex="-1"
      aria-modal="true"
      aria-labelledby="inspector-title"
      use:focusTrap={{ onEscape: () => { inspectedEntry = null } }}
      on:click|stopPropagation
    >
      <div class="modal-header">
        <div class="inspector-title-group">
          <span class="event-chip {getBadgeClass(inspectedEntry.eventType)}">
            {formatEventName(inspectedEntry.eventType)}
          </span>
          <h3 id="inspector-title">Audit Event Details</h3>
        </div>
        <button type="button" class="close-btn" on:click={() => { inspectedEntry = null }}>×</button>
      </div>

      <div class="inspector-content">
        <dl class="inspector-grid">
          <div><dt>Event ID</dt><dd class="tabular monospace">{inspectedEntry.id}</dd></div>
          <div><dt>Timestamp</dt><dd class="tabular">{inspectedEntry.timestamp ? new Date(inspectedEntry.timestamp).toLocaleString() : 'N/A'}</dd></div>
          <div><dt>Outcome</dt><dd><span class="outcome-tag {inspectedEntry.outcome?.toLowerCase()}">{inspectedEntry.outcome}</span></dd></div>
          {#if inspectedEntry.roomId}<div><dt>Room ID</dt><dd class="tabular monospace">{inspectedEntry.roomId}</dd></div>{/if}
          {#if inspectedEntry.guestId}<div><dt>Guest ID</dt><dd class="tabular monospace">{inspectedEntry.guestId}</dd></div>{/if}
          {#if inspectedEntry.guestName}<div><dt>Guest Name</dt><dd>{inspectedEntry.guestName}</dd></div>{/if}
          {#if inspectedEntry.fileName}<div><dt>File Name</dt><dd>{inspectedEntry.fileName}</dd></div>{/if}
          {#if inspectedEntry.fileSize}<div><dt>File Size</dt><dd class="tabular">{formatBytes(inspectedEntry.fileSize)}</dd></div>{/if}
          {#if inspectedEntry.sha256}<div><dt>SHA-256 Digest</dt><dd class="tabular monospace">{inspectedEntry.sha256}</dd></div>{/if}
        </dl>

        {#if inspectedEntry.details}
          <div class="details-pre-box">
            <span class="details-label">Structured Event Payload:</span>
            <pre class="details-pre">{inspectedEntry.details}</pre>
          </div>
        {/if}
      </div>

      <div class="modal-footer">
        <button
          type="button"
          class="button ghost small"
          on:click={() => { copyTextToClipboard(JSON.stringify(inspectedEntry, null, 2)); showToast('Copied JSON record', 'info'); }}
        >
          Copy JSON Record
        </button>
        <button type="button" class="button primary small" on:click={() => { inspectedEntry = null }}>
          Close
        </button>
      </div>
    </div>
  </div>
{/if}

<style>
  .audit-view-container {
    display: flex;
    flex-direction: column;
    gap: 16px;
    width: 100%;
  }

  .audit-controls-card {
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

  .header-btn-row {
    display: flex;
    gap: 8px;
  }

  .ghost-danger {
    background: transparent;
    color: var(--danger);
    border: 1px solid var(--danger-border);
  }

  .ghost-danger:hover {
    background: var(--danger-subtle);
  }

  .filters-row {
    display: flex;
    align-items: center;
    gap: 16px;
    flex-wrap: wrap;
  }

  .filter-item {
    display: flex;
    align-items: center;
    gap: 8px;
    font-size: 12.5px;
    color: var(--text-secondary);
  }

  .filter-stats {
    margin-left: auto;
    font-size: 12px;
    color: var(--text-muted);
  }

  /* TABLE CARD */
  .audit-table-card {
    background: var(--bg-surface);
    border: 1px solid var(--border-base);
    border-radius: var(--radius-lg, 14px);
    box-shadow: var(--shadow-sm);
    overflow: hidden;
  }

  .table-responsive-box {
    width: 100%;
    overflow-x: auto;
  }

  .audit-table {
    width: 100%;
    border-collapse: collapse;
    font-size: 12.5px;
    text-align: left;
  }

  .audit-table th {
    padding: 12px 16px;
    background: var(--bg-surface-elevated);
    color: var(--text-muted);
    font-weight: 600;
    font-size: 11px;
    text-transform: uppercase;
    letter-spacing: 0.03em;
    border-bottom: 1px solid var(--border-subtle);
  }

  .audit-tr {
    border-bottom: 1px solid var(--border-subtle);
    transition: background var(--duration-fast, 150ms) ease;
  }

  .audit-tr:hover {
    background: var(--bg-surface-hover);
  }

  .audit-table td {
    padding: 10px 16px;
    color: var(--text-primary);
  }

  .time-cell {
    font-size: 11.5px;
    color: var(--text-muted);
    white-space: nowrap;
  }

  .event-chip {
    font-size: 10.5px;
    font-weight: 700;
    padding: 2px 8px;
    border-radius: 4px;
    text-transform: capitalize;
    white-space: nowrap;
  }

  .badge-success { background: var(--success-subtle); color: var(--success); }
  .badge-danger { background: var(--danger-subtle); color: var(--danger); }
  .badge-info { background: var(--accent-subtle); color: var(--accent); }
  .badge-neutral { background: var(--bg-surface-secondary); color: var(--text-muted); }

  .id-copy-btn {
    background: transparent;
    border: none;
    font-family: monospace;
    font-size: 11px;
    color: var(--accent);
    cursor: pointer;
    padding: 0;
  }

  .id-copy-btn:hover {
    text-decoration: underline;
  }

  .guest-name {
    font-size: 12px;
    color: var(--text-primary);
    white-space: nowrap;
  }

  .outcome-tag {
    font-size: 10px;
    font-weight: 700;
    padding: 2px 6px;
    border-radius: 4px;
    text-transform: uppercase;
  }

  .outcome-tag.success { background: var(--success-subtle); color: var(--success); }
  .outcome-tag.failure, .outcome-tag.fail { background: var(--danger-subtle); color: var(--danger); }
  .outcome-tag.rejected, .outcome-tag.canceled { background: var(--warning-subtle); color: var(--warning); }

  .details-cell {
    color: var(--text-secondary);
    max-width: 280px;
    white-space: nowrap;
    overflow: hidden;
    text-overflow: ellipsis;
  }

  .inspect-btn {
    padding: 2px 8px;
    font-size: 11px;
  }

  .pagination-footer {
    display: flex;
    justify-content: space-between;
    align-items: center;
    padding: 12px 18px;
    border-top: 1px solid var(--border-subtle);
  }

  .pagination-info {
    font-size: 12px;
    color: var(--text-muted);
  }

  .pagination-buttons {
    display: flex;
    gap: 8px;
  }

  .empty-audit-box {
    padding: 48px;
    text-align: center;
    display: flex;
    flex-direction: column;
    align-items: center;
    gap: 8px;
  }

  .empty-icon {
    font-size: 32px;
  }

  .empty-audit-box h4 {
    margin: 0;
    font-size: 15px;
  }

  .empty-audit-box p {
    margin: 0;
    font-size: 12.5px;
    color: var(--text-muted);
    max-width: 440px;
  }

  .loading-state {
    padding: 36px;
    text-align: center;
    color: var(--text-muted);
  }

  /* MODAL INSPECTOR */
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

  .inspector-modal {
    background: var(--bg-modal);
    color: var(--text-primary);
    border: 1px solid var(--border-base);
    border-radius: var(--radius-lg, 14px);
    box-shadow: var(--shadow-lg);
    width: 100%;
    max-width: 560px;
    max-height: 85vh;
    display: flex;
    flex-direction: column;
    animation: scaleIn 150ms ease;
  }

  .modal-header {
    padding: 18px 24px;
    border-bottom: 1px solid var(--border-base);
    display: flex;
    justify-content: space-between;
    align-items: center;
  }

  .inspector-title-group {
    display: flex;
    align-items: center;
    gap: 10px;
  }

  .inspector-title-group h3 {
    margin: 0;
    font-size: 17px;
    font-weight: 700;
  }

  .close-btn {
    background: transparent;
    border: none;
    font-size: 22px;
    color: var(--text-muted);
    cursor: pointer;
    line-height: 1;
  }

  .close-btn:hover {
    color: var(--text-primary);
  }

  .inspector-content {
    padding: 20px 24px;
    overflow-y: auto;
    display: flex;
    flex-direction: column;
    gap: 16px;
  }

  .inspector-grid {
    display: grid;
    grid-template-columns: 1fr 1fr;
    gap: 10px 16px;
    margin: 0;
  }

  .inspector-grid dt {
    font-size: 11px;
    font-weight: 600;
    color: var(--text-muted);
    text-transform: uppercase;
  }

  .inspector-grid dd {
    margin: 2px 0 0;
    font-size: 12.5px;
    color: var(--text-primary);
  }

  .monospace {
    font-family: monospace;
    font-size: 11.5px;
  }

  .details-pre-box {
    display: flex;
    flex-direction: column;
    gap: 6px;
  }

  .details-label {
    font-size: 11.5px;
    font-weight: 600;
    color: var(--text-muted);
    text-transform: uppercase;
  }

  .details-pre {
    background: var(--bg-surface-elevated);
    border: 1px solid var(--border-subtle);
    padding: 10px 12px;
    border-radius: var(--radius-sm, 6px);
    font-family: monospace;
    font-size: 11.5px;
    color: var(--text-primary);
    white-space: pre-wrap;
    word-break: break-all;
    margin: 0;
    max-height: 160px;
    overflow-y: auto;
  }

  .modal-footer {
    padding: 14px 24px;
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
    .modal-backdrop, .inspector-modal {
      animation: none !important;
    }
  }
</style>
