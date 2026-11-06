<script lang="ts">
  import { onMount } from 'svelte';

  export let message: string = '';
  export let type: 'info' | 'success' | 'warning' | 'error' = 'info';
  export let duration: number = 3000;
  export let onDismiss: () => void = () => {};

  onMount(() => {
    if (duration > 0) {
      const timer = setTimeout(onDismiss, duration);
      return () => clearTimeout(timer);
    }
  });
</script>

{#if message}
  <div class="toast {type}" role="status" aria-live="polite">
    <div class="toast-content">
      {#if type === 'success'}
        <span class="toast-icon" aria-hidden="true">✓</span>
      {:else if type === 'warning'}
        <span class="toast-icon" aria-hidden="true">▲</span>
      {:else if type === 'error'}
        <span class="toast-icon" aria-hidden="true">✕</span>
      {:else}
        <span class="toast-icon" aria-hidden="true">ℹ</span>
      {/if}
      <span class="toast-text">{message}</span>
    </div>
    <button type="button" class="toast-close" on:click={onDismiss} aria-label="Dismiss notification">
      ×
    </button>
  </div>
{/if}

<style>
  .toast {
    display: inline-flex;
    align-items: center;
    justify-content: space-between;
    gap: 12px;
    padding: 10px 14px;
    background: var(--bg-surface);
    color: var(--text-primary);
    border: 1px solid var(--border-base);
    border-radius: var(--radius-md);
    box-shadow: var(--shadow-md);
    font-size: 13px;
    line-height: 1.4;
    animation: toast-slide-in var(--duration-base) var(--easing-standard);
  }

  .toast.success {
    border-color: var(--success);
    background: var(--bg-surface);
  }

  .toast.warning {
    border-color: var(--warning);
  }

  .toast.error {
    border-color: var(--danger);
  }

  .toast-content {
    display: flex;
    align-items: center;
    gap: 8px;
  }

  .toast-icon {
    font-weight: 700;
    font-size: 13px;
  }

  .toast.success .toast-icon {
    color: var(--success);
  }

  .toast.warning .toast-icon {
    color: var(--warning);
  }

  .toast.error .toast-icon {
    color: var(--danger);
  }

  .toast.info .toast-icon {
    color: var(--accent);
  }

  .toast-close {
    background: transparent;
    border: none;
    color: var(--text-muted);
    font-size: 16px;
    padding: 0 4px;
    cursor: pointer;
    border-radius: var(--radius-sm);
  }

  .toast-close:hover {
    color: var(--text-primary);
  }

  @keyframes toast-slide-in {
    from {
      opacity: 0;
      transform: translateY(-8px);
    }
    to {
      opacity: 1;
      transform: translateY(0);
    }
  }

  @media (prefers-reduced-motion: reduce) {
    .toast {
      animation: none;
    }
  }
</style>
