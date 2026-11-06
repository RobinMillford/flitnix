<script lang="ts">
  export let value: number = 0; // 0 to 100
  export let max: number = 100;
  export let label: string = '';
  export let size: 'sm' | 'md' | 'lg' = 'md';
  export let variant: 'primary' | 'success' | 'warning' | 'danger' = 'primary';
  export let showValue: boolean = false;

  $: bounded = Math.max(0, Math.min(max, value));
  $: pct = max > 0 ? (bounded / max) * 100 : 0;
</script>

<div class="progress-container {size}">
  {#if label || showValue}
    <div class="progress-header">
      {#if label}
        <span class="progress-label">{label}</span>
      {/if}
      {#if showValue}
        <span class="progress-val">{Math.round(pct)}%</span>
      {/if}
    </div>
  {/if}
  <div
    class="progress-track"
    role="progressbar"
    aria-valuenow={Math.round(pct)}
    aria-valuemin={0}
    aria-valuemax={100}
    aria-label={label || 'Progress'}
  >
    <div
      class="progress-fill {variant}"
      style="width: {pct}%;"
    ></div>
  </div>
</div>

<style>
  .progress-container {
    width: 100%;
    display: flex;
    flex-direction: column;
    gap: 6px;
  }

  .progress-header {
    display: flex;
    justify-content: space-between;
    align-items: center;
    font-size: 12px;
    font-weight: 500;
    color: var(--text-secondary);
  }

  .progress-val {
    font-family: var(--font-mono);
    font-size: 11px;
    color: var(--text-primary);
  }

  .progress-track {
    width: 100%;
    background: var(--bg-surface-active);
    border-radius: 9999px;
    overflow: hidden;
    position: relative;
  }

  .sm .progress-track {
    height: 4px;
  }

  .md .progress-track {
    height: 6px;
  }

  .lg .progress-track {
    height: 10px;
  }

  .progress-fill {
    height: 100%;
    border-radius: 9999px;
    background: var(--accent);
    transition: width var(--duration-base) var(--easing-standard);
  }

  .progress-fill.success {
    background: var(--success);
  }

  .progress-fill.warning {
    background: var(--warning);
  }

  .progress-fill.danger {
    background: var(--danger);
  }

  @media (prefers-reduced-motion: reduce) {
    .progress-fill {
      transition: none;
    }
  }
</style>
