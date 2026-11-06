<script lang="ts">
  import { focusTrap } from '../focusTrap'

  export let isOpen: boolean = false
  export let title: string = 'Confirm Action'
  export let message: string = 'Are you sure you want to proceed?'
  export let confirmText: string = 'Confirm'
  export let cancelText: string = 'Cancel'
  export let danger: boolean = false
  export let onConfirm: () => void = () => {}
  export let onCancel: () => void = () => {}

  function handleKeydown(e: KeyboardEvent) {
    if (e.key === 'Escape') {
      e.stopPropagation()
      onCancel()
    }
  }
</script>

<svelte:window on:keydown={handleKeydown} />

{#if isOpen}
  <!-- svelte-ignore a11y_click_events_have_key_events -->
  <div class="modal-backdrop" role="presentation" on:click={onCancel} on:keydown={(e) => { if (e.key === 'Escape') onCancel(); }}>
    <div
      class="modal-card confirm-modal"
      role="alertdialog"
      tabindex="-1"
      aria-modal="true"
      aria-labelledby="confirm-modal-title"
      aria-describedby="confirm-modal-desc"
      use:focusTrap={{ onEscape: onCancel }}
      on:click|stopPropagation
    >
      <div class="confirm-icon-wrap" class:danger>
        {#if danger}
          <svg width="24" height="24" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round">
            <path d="M10.29 3.86L1.82 18a2 2 0 0 0 1.71 3h16.94a2 2 0 0 0 1.71-3L13.71 3.86a2 2 0 0 0-3.42 0z"/>
            <line x1="12" y1="9" x2="12" y2="13"/>
            <line x1="12" y1="17" x2="12.01" y2="17"/>
          </svg>
        {:else}
          <svg width="24" height="24" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round">
            <circle cx="12" cy="12" r="10"/>
            <line x1="12" y1="16" x2="12" y2="12"/>
            <line x1="12" y1="8" x2="12.01" y2="8"/>
          </svg>
        {/if}
      </div>

      <div class="confirm-body">
        <h3 id="confirm-modal-title">{title}</h3>
        <p id="confirm-modal-desc">{message}</p>
      </div>

      <div class="confirm-actions">
        <button type="button" class="button ghost" on:click={onCancel}>
          {cancelText}
        </button>
        <button
          type="button"
          class="button {danger ? 'danger' : 'primary'}"
          on:click={() => { onConfirm(); onCancel(); }}
        >
          {confirmText}
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

  .confirm-modal {
    background: var(--bg-modal);
    color: var(--text-primary);
    border: 1px solid var(--border-base);
    border-radius: var(--radius-lg, 14px);
    box-shadow: var(--shadow-lg);
    width: 100%;
    max-width: 440px;
    padding: 24px;
    display: flex;
    flex-direction: column;
    gap: 16px;
    animation: scaleIn 150ms ease;
  }

  .confirm-icon-wrap {
    width: 44px;
    height: 44px;
    border-radius: 50%;
    display: flex;
    align-items: center;
    justify-content: center;
    background: var(--accent-subtle);
    color: var(--accent);
  }

  .confirm-icon-wrap.danger {
    background: var(--danger-subtle);
    color: var(--danger);
  }

  .confirm-body h3 {
    margin: 0 0 6px;
    font-size: 17px;
    font-weight: 700;
    color: var(--text-primary);
  }

  .confirm-body p {
    margin: 0;
    font-size: 13.5px;
    line-height: 1.5;
    color: var(--text-secondary);
  }

  .confirm-actions {
    display: flex;
    justify-content: flex-end;
    gap: 10px;
    margin-top: 8px;
  }

  @keyframes fadeIn {
    from { opacity: 0; }
    to { opacity: 1; }
  }

  @keyframes scaleIn {
    from { transform: scale(0.96); opacity: 0; }
    to { transform: scale(1); opacity: 1; }
  }

  @media (prefers-reduced-motion: reduce) {
    .modal-backdrop, .confirm-modal {
      animation: none !important;
    }
  }
</style>
