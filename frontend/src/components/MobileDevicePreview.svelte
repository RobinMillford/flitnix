<script lang="ts">
  import type { guest } from '../../wailsjs/go/models'

  export let title: string = 'Classroom Assignment'
  export let description: string = 'Submit your assignment files. Provide your student ID and notes.'
  export let fields: guest.FormField[] = []
  export let durationMinutes: number = 60
</script>

<div class="phone-frame">
  <div class="phone-speaker"></div>
  <div class="phone-screen">
    <!-- Mobile Browser Chrome -->
    <div class="mobile-browser-bar">
      <span class="browser-lock">🔒</span>
      <span class="browser-url">flitnix.local/guest</span>
      <span class="browser-dots">•••</span>
    </div>

    <!-- Mobile Page Content -->
    <div class="mobile-content">
      <div class="mobile-header">
        <div class="mobile-logo-badge">Flitnix Drop</div>
        <div class="mobile-timer-chip">⏱ {durationMinutes}m left</div>
      </div>

      <div class="mobile-room-card">
        <h4 class="mobile-room-title">{title || 'Guest Drop Box'}</h4>
        <p class="mobile-room-desc">{description || 'Upload files directly to this computer.'}</p>
      </div>

      <!-- Guest Form Preview -->
      <div class="mobile-form-preview">
        {#each (fields || []) as f (f.id || f.label)}
          <div class="mobile-input-group">
            <span class="mobile-label">
              {f.label} {#if f.required}<span class="req">*</span>{/if}
            </span>
            {#if f.type === 'textarea'}
              <div class="mobile-dummy-textarea">Enter {f.label.toLowerCase()}…</div>
            {:else}
              <div class="mobile-dummy-input">Enter {f.label.toLowerCase()}…</div>
            {/if}
          </div>
        {:else}
          <div class="mobile-input-group">
            <span class="mobile-label">Your Name <span class="req">*</span></span>
            <div class="mobile-dummy-input">Enter your name…</div>
          </div>
        {/each}

        <!-- Upload Target Preview -->
        <div class="mobile-drop-target">
          <div class="mobile-upload-icon">📁</div>
          <span class="mobile-upload-prompt">Tap to choose files</span>
          <span class="mobile-upload-sub">Photos, PDFs, documents up to 5 GB</span>
        </div>

        <button type="button" class="mobile-submit-btn" disabled>
          Submit & Upload Files
        </button>
      </div>
    </div>
  </div>
</div>

<style>
  .phone-frame {
    width: 290px;
    height: 520px;
    background: var(--bg-surface-secondary);
    border: 10px solid var(--border-strong);
    border-radius: 36px;
    box-shadow: var(--shadow-lg), inset 0 0 0 2px var(--border-subtle);
    position: relative;
    overflow: hidden;
    display: flex;
    flex-direction: column;
    margin: 0 auto;
  }

  .phone-speaker {
    width: 60px;
    height: 5px;
    background: var(--border-base);
    border-radius: 999px;
    position: absolute;
    top: 6px;
    left: 50%;
    transform: translateX(-50%);
    z-index: 10;
  }

  .phone-screen {
    flex: 1;
    background: var(--bg-surface);
    color: var(--text-primary);
    display: flex;
    flex-direction: column;
    overflow: hidden;
    font-family: system-ui, -apple-system, sans-serif;
  }

  .mobile-browser-bar {
    background: var(--bg-surface-elevated);
    padding: 18px 12px 6px;
    display: flex;
    align-items: center;
    justify-content: space-between;
    font-size: 10px;
    color: var(--text-muted);
    border-bottom: 1px solid var(--border-subtle);
  }

  .browser-url {
    background: var(--bg-surface-input);
    padding: 2px 10px;
    border-radius: 12px;
    border: 1px solid var(--border-subtle);
    font-family: monospace;
    font-size: 9.5px;
    color: var(--text-secondary);
  }

  .mobile-content {
    flex: 1;
    padding: 12px 14px;
    overflow-y: auto;
    display: flex;
    flex-direction: column;
    gap: 10px;
  }

  .mobile-header {
    display: flex;
    justify-content: space-between;
    align-items: center;
  }

  .mobile-logo-badge {
    font-size: 10px;
    font-weight: 700;
    color: var(--accent);
    background: var(--accent-subtle);
    padding: 2px 6px;
    border-radius: 4px;
  }

  .mobile-timer-chip {
    font-size: 9.5px;
    font-weight: 600;
    color: var(--success);
    background: var(--success-subtle);
    padding: 2px 6px;
    border-radius: 4px;
  }

  .mobile-room-card {
    background: var(--bg-surface-elevated);
    border: 1px solid var(--border-base);
    border-radius: 10px;
    padding: 10px;
  }

  .mobile-room-title {
    margin: 0 0 4px;
    font-size: 13px;
    font-weight: 700;
    color: var(--text-primary);
    line-height: 1.3;
  }

  .mobile-room-desc {
    margin: 0;
    font-size: 10px;
    color: var(--text-secondary);
    line-height: 1.35;
  }

  .mobile-form-preview {
    display: flex;
    flex-direction: column;
    gap: 8px;
  }

  .mobile-input-group {
    display: flex;
    flex-direction: column;
    gap: 3px;
  }

  .mobile-label {
    font-size: 10px;
    font-weight: 600;
    color: var(--text-secondary);
  }

  .req {
    color: var(--danger);
  }

  .mobile-dummy-input {
    background: var(--bg-surface-input);
    border: 1px solid var(--border-input);
    border-radius: 6px;
    padding: 6px 8px;
    font-size: 10px;
    color: var(--text-placeholder);
  }

  .mobile-dummy-textarea {
    background: var(--bg-surface-input);
    border: 1px solid var(--border-input);
    border-radius: 6px;
    padding: 6px 8px;
    font-size: 10px;
    color: var(--text-placeholder);
    height: 38px;
  }

  .mobile-drop-target {
    border: 1.5px dashed var(--accent-border);
    background: var(--accent-subtle);
    border-radius: 8px;
    padding: 12px 8px;
    display: flex;
    flex-direction: column;
    align-items: center;
    text-align: center;
    gap: 2px;
  }

  .mobile-upload-icon {
    font-size: 16px;
  }

  .mobile-upload-prompt {
    font-size: 10.5px;
    font-weight: 600;
    color: var(--accent);
  }

  .mobile-upload-sub {
    font-size: 8.5px;
    color: var(--text-muted);
  }

  .mobile-submit-btn {
    background: var(--accent);
    color: var(--accent-text);
    border: none;
    border-radius: 6px;
    padding: 8px;
    font-size: 11px;
    font-weight: 600;
    cursor: default;
    opacity: 0.85;
  }
</style>
