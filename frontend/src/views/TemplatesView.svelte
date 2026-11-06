<script lang="ts">
  import type { guest } from '../../wailsjs/go/models'
  import ConfirmModal from '../components/ConfirmModal.svelte'

  export let templates: guest.Template[] = []
  export let loading: boolean = false
  export let startFromTemplate: (id: string) => void = () => {}
  export let duplicateTemplate: (id: string) => void = () => {}
  export let deleteTemplate: (id: string) => void = () => {}
  export let showToast: (msg: string, type?: any) => void = () => {}

  let searchQuery: string = ''
  let selectedCategory: string = 'all'
  let sortBy: 'recent' | 'name' | 'duration' = 'recent'
  let currentPage: number = 1
  const pageSize: number = 12

  let templateToDelete: guest.Template | null = null
  let confirmDeleteOpen: boolean = false

  function getCategory(t: guest.Template): string {
    const text = `${t.name} ${t.title} ${t.description || ''}`.toLowerCase()
    if (text.includes('class') || text.includes('assign') || text.includes('student') || text.includes('exam')) return 'classroom'
    if (text.includes('meet') || text.includes('agenda') || text.includes('board')) return 'meeting'
    if (text.includes('photo') || text.includes('media') || text.includes('image')) return 'photo'
    return 'general'
  }

  function getCategoryIcon(cat: string): string {
    switch (cat) {
      case 'classroom': return '📚'
      case 'meeting': return '💼'
      case 'photo': return '📷'
      default: return '📁'
    }
  }

  $: filteredTemplates = templates.filter((t) => {
    // 1. Search filter
    if (searchQuery.trim()) {
      const q = searchQuery.toLowerCase()
      const matchName = t.name?.toLowerCase().includes(q)
      const matchTitle = t.title?.toLowerCase().includes(q)
      const matchDesc = t.description?.toLowerCase().includes(q)
      if (!matchName && !matchTitle && !matchDesc) return false
    }

    // 2. Category filter
    if (selectedCategory !== 'all') {
      const cat = getCategory(t)
      if (cat !== selectedCategory) return false
    }

    return true
  }).sort((a, b) => {
    if (sortBy === 'name') {
      return (a.name || '').localeCompare(b.name || '')
    }
    if (sortBy === 'duration') {
      return (b.durationMinutes || 0) - (a.durationMinutes || 0)
    }
    // Default 'recent'
    const aTime = a.createdAt ? new Date(a.createdAt).getTime() : 0
    const bTime = b.createdAt ? new Date(b.createdAt).getTime() : 0
    return bTime - aTime
  })

  $: totalPages = Math.max(1, Math.ceil(filteredTemplates.length / pageSize))
  $: paginatedTemplates = filteredTemplates.slice((currentPage - 1) * pageSize, currentPage * pageSize)

  // Reset to page 1 on filter/search change
  $: if (searchQuery || selectedCategory || sortBy) {
    currentPage = 1
  }

  function requestDelete(t: guest.Template) {
    templateToDelete = t
    confirmDeleteOpen = true
  }

  function confirmDelete() {
    if (templateToDelete) {
      deleteTemplate(templateToDelete.id)
      showToast(`Deleted template "${templateToDelete.name}"`, 'info')
      templateToDelete = null
    }
  }
</script>

<div class="templates-view-container">
  <!-- Controls Bar: Search, Category Chips, Sort -->
  <div class="templates-controls-card">
    <div class="search-sort-row">
      <div class="search-box">
        <svg class="search-icon" width="16" height="16" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2">
          <circle cx="11" cy="11" r="8"/>
          <line x1="21" y1="21" x2="16.65" y2="16.65"/>
        </svg>
        <input
          type="text"
          class="search-input"
          placeholder="Search templates by name, title, or keywords…"
          bind:value={searchQuery}
        />
        {#if searchQuery}
          <button type="button" class="clear-search-btn" on:click={() => { searchQuery = '' }}>×</button>
        {/if}
      </div>

      <div class="sort-wrap">
        <label for="template-sort">Sort by:</label>
        <select id="template-sort" class="text-input compact" bind:value={sortBy}>
          <option value="recent">Recently Added</option>
          <option value="name">Name (A – Z)</option>
          <option value="duration">Duration</option>
        </select>
      </div>
    </div>

    <!-- Category Filter Chips -->
    <div class="category-chips-row">
      <button
        type="button"
        class="cat-chip"
        class:active={selectedCategory === 'all'}
        on:click={() => { selectedCategory = 'all' }}
      >
        All Templates ({templates.length})
      </button>
      <button
        type="button"
        class="cat-chip"
        class:active={selectedCategory === 'classroom'}
        on:click={() => { selectedCategory = 'classroom' }}
      >
        📚 Classroom
      </button>
      <button
        type="button"
        class="cat-chip"
        class:active={selectedCategory === 'meeting'}
        on:click={() => { selectedCategory = 'meeting' }}
      >
        💼 Meetings
      </button>
      <button
        type="button"
        class="cat-chip"
        class:active={selectedCategory === 'photo'}
        on:click={() => { selectedCategory = 'photo' }}
      >
        📷 Photos
      </button>
      <button
        type="button"
        class="cat-chip"
        class:active={selectedCategory === 'general'}
        on:click={() => { selectedCategory = 'general' }}
      >
        📁 General
      </button>
    </div>
  </div>

  <!-- Templates Grid -->
  {#if loading}
    <div class="loading-state">Loading templates…</div>
  {:else if paginatedTemplates.length > 0}
    <div class="templates-grid">
      {#each paginatedTemplates as t (t.id)}
        {@const cat = getCategory(t)}
        <div class="template-card">
          <div class="t-card-head">
            <span class="t-icon">{getCategoryIcon(cat)}</span>
            <div class="t-title-col">
              <strong class="t-name" title={t.name}>{t.name}</strong>
              <span class="t-category-badge {cat}">{cat}</span>
            </div>
          </div>

          <p class="t-desc">{t.description || t.title || 'Preconfigured collection workflow.'}</p>

          <!-- Meta Pills -->
          <div class="t-meta-chips">
            <span class="t-chip tabular">⏱ {t.durationMinutes || 60}m</span>
            <span class="t-chip">{t.collisionPolicy === 'reject' ? 'Strict Reject' : 'Preserve Both'}</span>
            <span class="t-chip tabular">📝 {t.formSchema?.length || 0} Fields</span>
            {#if (t.expectedParticipants?.length || 0) > 0}
              <span class="t-chip tabular">👥 {t.expectedParticipants?.length} Roster</span>
            {/if}
          </div>

          <div class="t-card-actions">
            <button
              type="button"
              class="button primary small"
              on:click={() => startFromTemplate(t.id)}
              title="Launch guest drop box room immediately with this template"
            >
              Start Room
            </button>
            <div class="secondary-btn-group">
              <button
                type="button"
                class="button ghost small"
                on:click={() => duplicateTemplate(t.id)}
                title="Create a duplicate copy of this template"
              >
                Duplicate
              </button>
              <button
                type="button"
                class="button danger small icon-only"
                on:click={() => requestDelete(t)}
                title="Delete template"
                aria-label="Delete {t.name}"
              >
                🗑
              </button>
            </div>
          </div>
        </div>
      {/each}
    </div>

    <!-- Pagination Footer -->
    {#if totalPages > 1}
      <div class="pagination-bar">
        <span class="pagination-info tabular">
          Showing {(currentPage - 1) * pageSize + 1}–{Math.min(currentPage * pageSize, filteredTemplates.length)} of {filteredTemplates.length} templates
        </span>
        <div class="pagination-controls">
          <button
            type="button"
            class="button ghost small"
            on:click={() => { currentPage = Math.max(1, currentPage - 1) }}
            disabled={currentPage === 1}
          >
            ← Previous
          </button>
          <span class="page-current tabular">Page {currentPage} of {totalPages}</span>
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
    <div class="empty-templates-card">
      <div class="empty-icon">📋</div>
      <h3>No matching templates found</h3>
      <p>
        {#if searchQuery}
          No templates match your search "{searchQuery}". Try a different keyword or clear your filter.
        {:else}
          No templates have been saved yet. Templates saved from Guest Drop Box rooms will appear here.
        {/if}
      </p>
      {#if searchQuery}
        <button type="button" class="button ghost small" on:click={() => { searchQuery = ''; selectedCategory = 'all'; }}>
          Clear Filters
        </button>
      {/if}
    </div>
  {/if}
</div>

<!-- Confirm Delete Modal -->
<ConfirmModal
  isOpen={confirmDeleteOpen}
  title="Delete Template?"
  message={`Are you sure you want to permanently delete "${templateToDelete?.name}"? This action cannot be undone.`}
  confirmText="Delete Template"
  danger={true}
  onConfirm={confirmDelete}
  onCancel={() => { confirmDeleteOpen = false }}
/>

<style>
  .templates-view-container {
    display: flex;
    flex-direction: column;
    gap: 16px;
    width: 100%;
  }

  .templates-controls-card {
    background: var(--bg-surface);
    border: 1px solid var(--border-base);
    border-radius: var(--radius-lg, 14px);
    box-shadow: var(--shadow-sm);
    padding: 16px 20px;
    display: flex;
    flex-direction: column;
    gap: 12px;
  }

  .search-sort-row {
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
    padding: 9px 36px 9px 36px;
    background: var(--bg-surface-elevated);
    border: 1px solid var(--border-base);
    border-radius: var(--radius-md, 10px);
    font-size: 13.5px;
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

  .sort-wrap {
    display: flex;
    align-items: center;
    gap: 8px;
    font-size: 12.5px;
    color: var(--text-secondary);
  }

  .category-chips-row {
    display: flex;
    gap: 8px;
    flex-wrap: wrap;
  }

  .cat-chip {
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

  .cat-chip:hover {
    border-color: var(--accent);
  }

  .cat-chip.active {
    background: var(--accent);
    border-color: var(--accent);
    color: var(--accent-text);
  }

  /* TEMPLATES GRID */
  .templates-grid {
    display: grid;
    grid-template-columns: repeat(auto-fill, minmax(280px, 1fr));
    gap: 16px;
  }

  .template-card {
    background: var(--bg-surface);
    border: 1px solid var(--border-base);
    border-radius: var(--radius-lg, 14px);
    box-shadow: var(--shadow-sm);
    padding: 18px 20px;
    display: flex;
    flex-direction: column;
    gap: 12px;
    transition: all var(--duration-fast, 150ms) ease;
  }

  .template-card:hover {
    transform: translateY(-2px);
    box-shadow: var(--shadow-md);
    border-color: var(--accent-border);
  }

  .t-card-head {
    display: flex;
    align-items: center;
    gap: 12px;
  }

  .t-icon {
    font-size: 24px;
    line-height: 1;
  }

  .t-title-col {
    flex: 1;
    min-width: 0;
    display: flex;
    flex-direction: column;
    gap: 2px;
  }

  .t-name {
    font-size: 14px;
    font-weight: 700;
    color: var(--text-primary);
    white-space: nowrap;
    overflow: hidden;
    text-overflow: ellipsis;
  }

  .t-category-badge {
    align-self: flex-start;
    font-size: 10px;
    font-weight: 700;
    text-transform: uppercase;
    padding: 1px 6px;
    border-radius: 4px;
    background: var(--bg-surface-elevated);
    color: var(--text-muted);
  }

  .t-category-badge.classroom {
    background: var(--accent-subtle);
    color: var(--accent);
  }

  .t-category-badge.meeting {
    background: var(--success-subtle);
    color: var(--success);
  }

  .t-category-badge.photo {
    background: var(--warning-subtle);
    color: var(--warning);
  }

  .t-desc {
    margin: 0;
    font-size: 12px;
    color: var(--text-secondary);
    line-height: 1.4;
    display: -webkit-box;
    -webkit-line-clamp: 2;
    line-clamp: 2;
    -webkit-box-orient: vertical;
    overflow: hidden;
    min-height: 34px;
  }

  .t-meta-chips {
    display: flex;
    flex-wrap: wrap;
    gap: 6px;
  }

  .t-chip {
    font-size: 11px;
    padding: 3px 8px;
    border-radius: 4px;
    background: var(--bg-surface-elevated);
    border: 1px solid var(--border-subtle);
    color: var(--text-secondary);
  }

  .t-card-actions {
    display: flex;
    justify-content: space-between;
    align-items: center;
    border-top: 1px solid var(--border-subtle);
    padding-top: 12px;
    margin-top: auto;
  }

  .secondary-btn-group {
    display: flex;
    gap: 6px;
  }

  .icon-only {
    padding: 4px 8px;
  }

  /* PAGINATION */
  .pagination-bar {
    display: flex;
    justify-content: space-between;
    align-items: center;
    padding: 12px 18px;
    background: var(--bg-surface);
    border: 1px solid var(--border-base);
    border-radius: var(--radius-lg, 14px);
  }

  .pagination-info {
    font-size: 12.5px;
    color: var(--text-secondary);
  }

  .pagination-controls {
    display: flex;
    align-items: center;
    gap: 12px;
  }

  .page-current {
    font-size: 12px;
    font-weight: 600;
    color: var(--text-primary);
  }

  /* EMPTY STATE */
  .empty-templates-card {
    background: var(--bg-surface);
    border: 1px solid var(--border-base);
    border-radius: var(--radius-lg, 14px);
    padding: 48px 24px;
    text-align: center;
    display: flex;
    flex-direction: column;
    align-items: center;
    gap: 10px;
  }

  .empty-icon {
    font-size: 36px;
  }

  .empty-templates-card h3 {
    margin: 0;
    font-size: 16px;
    font-weight: 700;
  }

  .empty-templates-card p {
    margin: 0;
    font-size: 13px;
    color: var(--text-secondary);
    max-width: 440px;
  }

  .loading-state {
    padding: 36px;
    text-align: center;
    color: var(--text-muted);
  }
</style>
