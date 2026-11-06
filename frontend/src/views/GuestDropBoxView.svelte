<script lang="ts">
  import type { main, guest } from '../../wailsjs/go/models'
  import { formatBytes, fmtTime, fmtRemaining } from '../utils'
  import MobileDevicePreview from '../components/MobileDevicePreview.svelte'
  import ConfirmModal from '../components/ConfirmModal.svelte'

  export let guestRoomState: main.GuestRoomStateView | null = null
  export let presets: guest.Preset[] = []
  export let selectedPresetId: string = ''
  export let guestTitle: string = 'Classroom Assignment'
  export let guestDescription: string = 'Submit your assignment files. Provide your student ID and notes.'
  export let guestCollisionPolicy: 'preserve_both' | 'reject' = 'preserve_both'
  export let guestFormSchema: guest.FormField[] = []
  export let guestDestDir: string = ''
  export let guestDurationMinutes: number = 60
  export let guestStarting: boolean = false
  export let guestCopiedLink: boolean = false
  export let exporting: boolean = false
  export let exportSuccessMsg: string = ''

  export let startRoom: () => void = () => {}
  export let endRoom: (id: string) => void = () => {}
  export let approveGuest: (id: string) => void = () => {}
  export let rejectGuest: (id: string) => void = () => {}
  export let revokeGuest: (id: string) => void = () => {}
  export let approveUpload: (uploadId: string) => void = () => {}
  export let rejectUpload: (uploadId: string) => void = () => {}
  export let pickDestination: () => void = () => {}
  export let openFolder: () => void = () => {}
  export let exportCollection: (roomId: string) => void = () => {}
  export let copyGuestLink: (url: string | undefined) => void = () => {}
  export let copyTextToClipboard: (text: string) => void = () => {}
  export let showToast: (msg: string, type?: any) => void = () => {}

  let wizardStep: 1 | 2 | 3 = 1
  let confirmEndRoomOpen = false

  $: isRoomActive = !!(guestRoomState?.room && guestRoomState.room.state !== 'ended' && guestRoomState.room.state !== 'expired' && guestRoomState.room.state !== 'Ended')

  function selectPreset(preset: guest.Preset) {
    selectedPresetId = preset.id
    guestTitle = preset.title
    guestDescription = preset.description
    guestCollisionPolicy = preset.collisionPolicy === 'reject' ? 'reject' : 'preserve_both'
    guestFormSchema = [...(preset.formSchema || [])]
  }

  function handleEndRoomConfirm() {
    if (guestRoomState?.room?.id) {
      endRoom(guestRoomState.room.id)
      showToast('Guest drop box room ended', 'info')
    }
  }

  function toggleField(label: string, defaultType: string = 'text') {
    const idx = guestFormSchema.findIndex((f) => f.label.toLowerCase() === label.toLowerCase())
    if (idx >= 0) {
      guestFormSchema = guestFormSchema.filter((_, i) => i !== idx)
    } else {
      guestFormSchema = [
        ...guestFormSchema,
        {
          id: `field_${Date.now()}`,
          label,
          type: defaultType,
          required: true,
          placeholder: `Enter ${label.toLowerCase()}…`
        } as guest.FormField
      ]
    }
  }

  function hasField(label: string): boolean {
    return guestFormSchema.some((f) => f.label.toLowerCase() === label.toLowerCase())
  }
</script>

<div class="guest-view-container">
  {#if !isRoomActive}
    <!-- =====================================================================
         GUEST ROOM SETUP WIZARD (3-Step Guided Flow)
         ===================================================================== -->
    <div class="wizard-container">
      <!-- Step Navigation Header -->
      <div class="wizard-stepper-bar">
        <button
          type="button"
          class="step-nav-btn"
          class:active={wizardStep === 1}
          class:completed={wizardStep > 1}
          on:click={() => { wizardStep = 1 }}
        >
          <span class="step-num">1</span>
          <span class="step-label">Room & Presets</span>
        </button>

        <div class="stepper-line" class:filled={wizardStep > 1}></div>

        <button
          type="button"
          class="step-nav-btn"
          class:active={wizardStep === 2}
          class:completed={wizardStep > 2}
          on:click={() => { wizardStep = 2 }}
        >
          <span class="step-num">2</span>
          <span class="step-label">Guest Experience</span>
        </button>

        <div class="stepper-line" class:filled={wizardStep > 2}></div>

        <button
          type="button"
          class="step-nav-btn"
          class:active={wizardStep === 3}
          on:click={() => { wizardStep = 3 }}
        >
          <span class="step-num">3</span>
          <span class="step-label">Storage & Launch</span>
        </button>
      </div>

      <!-- STEP 1: ROOM & PRESETS -->
      {#if wizardStep === 1}
        <div class="step-card">
          <div class="step-header">
            <h3>Choose a Workflow Preset</h3>
            <p>Select a preconfigured room setup or customize your own collection title and description.</p>
          </div>

          <!-- Preset Cards Grid -->
          <div class="preset-cards-grid">
            {#each presets as p (p.id)}
              <button
                type="button"
                class="preset-card"
                class:selected={selectedPresetId === p.id}
                on:click={() => selectPreset(p)}
              >
                <div class="preset-icon">
                  {#if p.id.includes('classroom') || p.title.toLowerCase().includes('class')}
                    📚
                  {:else if p.id.includes('photo') || p.title.toLowerCase().includes('photo')}
                    📷
                  {:else if p.id.includes('meeting') || p.title.toLowerCase().includes('meeting')}
                    💼
                  {:else if p.id.includes('team') || p.title.toLowerCase().includes('team')}
                    👥
                  {:else}
                    📁
                  {/if}
                </div>
                <div class="preset-info">
                  <strong>{p.title}</strong>
                  <p>{p.description}</p>
                </div>
                <div class="preset-check">
                  {selectedPresetId === p.id ? '✓' : ''}
                </div>
              </button>
            {/each}
          </div>

          <div class="form-section-group">
            <div class="form-row">
              <label for="guest-room-title">Collection Title <span class="req">*</span></label>
              <input
                id="guest-room-title"
                type="text"
                class="text-input"
                bind:value={guestTitle}
                placeholder="e.g. Midterm Assignment Submissions"
              />
            </div>

            <div class="form-row">
              <label for="guest-room-desc">Description & Instructions</label>
              <textarea
                id="guest-room-desc"
                class="text-input"
                style="height: 60px;"
                bind:value={guestDescription}
                placeholder="Instructions displayed to guests when opening the upload room…"
              ></textarea>
            </div>
          </div>

          <div class="wizard-footer-actions right">
            <button
              type="button"
              class="button primary"
              on:click={() => { wizardStep = 2 }}
              disabled={!guestTitle.trim()}
            >
              Continue to Guest Experience →
            </button>
          </div>
        </div>

      <!-- STEP 2: GUEST EXPERIENCE & LIVE MOBILE PREVIEW -->
      {:else if wizardStep === 2}
        <div class="step-card step-two-layout">
          <div class="form-side-column">
            <div class="step-header">
              <h3>Configure Guest Submission Fields</h3>
              <p>Choose what information attendees must provide before uploading files to this computer.</p>
            </div>

            <!-- Field Toggles -->
            <div class="fields-picker-box">
              <span class="subhead-label">Standard Submission Fields:</span>
              <div class="field-chips-row">
                <button
                  type="button"
                  class="field-toggle-chip"
                  class:active={hasField('Student ID')}
                  on:click={() => toggleField('Student ID')}
                >
                  {hasField('Student ID') ? '✓' : '+'} Student ID
                </button>
                <button
                  type="button"
                  class="field-toggle-chip"
                  class:active={hasField('Full Name')}
                  on:click={() => toggleField('Full Name')}
                >
                  {hasField('Full Name') ? '✓' : '+'} Full Name
                </button>
                <button
                  type="button"
                  class="field-toggle-chip"
                  class:active={hasField('Topic / Title')}
                  on:click={() => toggleField('Topic / Title')}
                >
                  {hasField('Topic / Title') ? '✓' : '+'} Topic / Title
                </button>
                <button
                  type="button"
                  class="field-toggle-chip"
                  class:active={hasField('Submission Notes')}
                  on:click={() => toggleField('Submission Notes', 'textarea')}
                >
                  {hasField('Submission Notes') ? '✓' : '+'} Submission Notes
                </button>
              </div>
            </div>

            <!-- Active Form Fields List -->
            <div class="active-fields-summary">
              <span class="subhead-label">Configured Fields ({guestFormSchema.length}):</span>
              {#if guestFormSchema.length > 0}
                <div class="fields-list-box">
                  {#each guestFormSchema as field, idx (field.id || idx)}
                    <div class="field-item-row">
                      <div class="field-drag-label">
                        <strong>{field.label}</strong>
                        <span class="field-type-tag">{field.type}</span>
                      </div>
                      <button
                        type="button"
                        class="remove-field-btn"
                        on:click={() => { guestFormSchema = guestFormSchema.filter((_, i) => i !== idx) }}
                        title="Remove field"
                      >
                        ×
                      </button>
                    </div>
                  {/each}
                </div>
              {:else}
                <div class="empty-fields-note">No fields configured. Guests will upload files with default nickname only.</div>
              {/if}
            </div>

            <div class="wizard-footer-actions">
              <button type="button" class="button ghost" on:click={() => { wizardStep = 1 }}>
                ← Back to Presets
              </button>
              <button type="button" class="button primary" on:click={() => { wizardStep = 3 }}>
                Continue to Storage & Launch →
              </button>
            </div>
          </div>

          <!-- LIVE MOBILE PREVIEW -->
          <div class="preview-side-column">
            <div class="preview-label-tag">Live Attendee Mobile Preview</div>
            <MobileDevicePreview
              title={guestTitle}
              description={guestDescription}
              fields={guestFormSchema}
              durationMinutes={guestDurationMinutes}
            />
          </div>
        </div>

      <!-- STEP 3: STORAGE, DURATION & LAUNCH -->
      {:else if wizardStep === 3}
        <div class="step-card">
          <div class="step-header">
            <h3>Storage & Room Duration</h3>
            <p>Define destination folder, duplicate collision handling, and room expiration.</p>
          </div>

          <div class="storage-options-grid">
            <!-- Destination Folder Card -->
            <div class="option-subcard">
              <span class="subcard-title">Destination Folder</span>
              <p class="subcard-desc">Files uploaded by guests will be saved into private subfolders under this directory.</p>
              <div class="path-display-box">
                <span class="path-text" title={guestDestDir}>{guestDestDir || 'Downloads/Flitnix'}</span>
                <div class="path-actions">
                  <button type="button" class="button secondary small" on:click={pickDestination}>
                    Change Folder…
                  </button>
                  <button type="button" class="button ghost small" on:click={openFolder}>
                    Open Folder
                  </button>
                </div>
              </div>
            </div>

            <!-- Collision Policy Card -->
            <div class="option-subcard">
              <span class="subcard-title">File Duplicate Policy</span>
              <p class="subcard-desc">Choose how the server behaves when a guest uploads a file with a name that already exists.</p>
              <div class="policy-radio-group">
                <label class="policy-card" class:selected={guestCollisionPolicy === 'preserve_both'}>
                  <input
                    type="radio"
                    name="collision"
                    value="preserve_both"
                    bind:group={guestCollisionPolicy}
                  />
                  <div>
                    <strong>Preserve Both (Recommended)</strong>
                    <span>Automatically numbers duplicates: <code>report (1).pdf</code></span>
                  </div>
                </label>
                <label class="policy-card" class:selected={guestCollisionPolicy === 'reject'}>
                  <input
                    type="radio"
                    name="collision"
                    value="reject"
                    bind:group={guestCollisionPolicy}
                  />
                  <div>
                    <strong>Strict Reject</strong>
                    <span>Rejects duplicate uploads to prevent multiple versions.</span>
                  </div>
                </label>
              </div>
            </div>

            <!-- Room Duration Selector -->
            <div class="option-subcard">
              <span class="subcard-title">Room Active Duration</span>
              <p class="subcard-desc">After this duration, the room expires automatically and stops accepting guest connections.</p>
              <div class="duration-pills-row">
                {#each [15, 30, 60, 120, 240] as mins}
                  <button
                    type="button"
                    class="duration-pill"
                    class:active={guestDurationMinutes === mins}
                    on:click={() => { guestDurationMinutes = mins }}
                  >
                    {mins >= 60 ? `${mins / 60} ${mins === 60 ? 'Hour' : 'Hours'}` : `${mins} Minutes`}
                  </button>
                {/each}
              </div>
            </div>
          </div>

          <!-- Launch Summary Strip -->
          <div class="launch-summary-strip">
            <div class="summary-meta">
              <strong>Ready to launch: {guestTitle}</strong>
              <span>Duration: {guestDurationMinutes} min · {guestFormSchema.length} fields · {guestCollisionPolicy === 'preserve_both' ? 'Preserve both' : 'Strict reject'}</span>
            </div>
            <button
              type="button"
              class="button primary large"
              on:click={startRoom}
              disabled={guestStarting || !guestTitle.trim()}
            >
              {guestStarting ? 'Launching Room…' : 'Start Guest Drop Box Room'}
            </button>
          </div>

          <div class="wizard-footer-actions">
            <button type="button" class="button ghost" on:click={() => { wizardStep = 2 }}>
              ← Back to Guest Experience
            </button>
          </div>
        </div>
      {/if}
    </div>

  {:else}
    <!-- =====================================================================
         ACTIVE GUEST ROOM COMMAND CENTER DASHBOARD
         ===================================================================== -->
    <div class="active-room-dashboard">
      <!-- Active Room Header Banner -->
      <div class="dashboard-header-card">
        <div class="dash-title-group">
          <div class="dash-status-row">
            <span class="active-pulse-badge">
              <span class="chip-pulse active"></span>
              Live Guest Drop Box Room
            </span>
            {#if guestRoomState?.room?.expiresAt}
              <span class="countdown-chip tabular">
                ⏱ {fmtRemaining(guestRoomState.room.expiresAt)}
              </span>
            {/if}
          </div>
          <h2>{guestRoomState?.room?.title || 'Guest Drop Box'}</h2>
          <p>{guestRoomState?.room?.description || 'Upload files directly to this computer.'}</p>
        </div>

        <div class="dash-header-actions">
          <button
            type="button"
            class="button secondary small"
            on:click={() => exportCollection(guestRoomState?.room?.id || '')}
            disabled={exporting}
          >
            {exporting ? 'Exporting…' : 'Export Collection (ZIP)'}
          </button>
          <button
            type="button"
            class="button danger small"
            on:click={() => { confirmEndRoomOpen = true }}
          >
            End Room
          </button>
        </div>
      </div>

      {#if exportSuccessMsg}
        <div class="alert success" style="margin-bottom: 16px;">
          <span>✓</span>
          <span>{exportSuccessMsg}</span>
        </div>
      {/if}

      <!-- Quick QR & Share Hub -->
      <div class="active-room-hub-grid">
        <div class="room-qr-card">
          <div class="qr-display-frame">
            {#if guestRoomState?.room?.qrDataUrl}
              <img src={guestRoomState.room.qrDataUrl} alt="Room QR Code" class="room-qr-img" />
            {:else}
              <div class="qr-placeholder">Generating QR…</div>
            {/if}
          </div>
          <span class="qr-subtext">Display on classroom projector or meeting screen</span>
        </div>

        <div class="room-link-details-card">
          <h4>Invite Attendees</h4>
          <p>Guests can join by scanning the QR code, or by navigating to this link in any browser on the same Wi-Fi:</p>

          <div class="invite-url-box">
            <span class="invite-url tabular">{guestRoomState?.room?.inviteUrl || 'http://…'}</span>
            <button
              type="button"
              class="button ghost small copy-btn"
              on:click={() => { copyGuestLink(guestRoomState?.room?.inviteUrl); showToast('Copied invite link', 'success'); }}
            >
              {guestCopiedLink ? '✓ Copied' : 'Copy Link'}
            </button>
          </div>

          <div class="room-stats-summary-row">
            <div class="stat-box">
              <span class="stat-num tabular">{guestRoomState?.pendingGuests?.length || 0}</span>
              <span class="stat-lbl">Pending Guests</span>
            </div>
            <div class="stat-box">
              <span class="stat-num tabular">{guestRoomState?.approvedGuests?.length || 0}</span>
              <span class="stat-lbl">Connected</span>
            </div>
            <div class="stat-box">
              <span class="stat-num tabular">{guestRoomState?.recentUploads?.length || 0}</span>
              <span class="stat-lbl">Files Received</span>
            </div>
          </div>
        </div>
      </div>

      <!-- Live Guests & Uploads Activity -->
      <div class="room-activity-grid">
        <!-- Left: Pending Approvals & Guests -->
        <div class="activity-column">
          <!-- Pending Uploads Awaiting Approval -->
          {#if (guestRoomState?.pendingUploads?.length || 0) > 0}
            <div class="activity-card" style="border-color: var(--amber);">
              <div class="act-card-head">
                <h4 style="color: var(--warning-text);">Pending Upload Approvals</h4>
                <span class="count-pill">{guestRoomState?.pendingUploads?.length}</span>
              </div>
              <div class="act-list">
                {#each (guestRoomState?.pendingUploads || []) as u (u.id)}
                  <div class="pending-guest-row">
                    <div class="guest-avatar">📄</div>
                    <div class="guest-meta">
                      <strong>{u.fileName}</strong>
                      <span class="tabular">From {u.guestName || 'Guest'} · {formatBytes(u.size)}</span>
                    </div>
                    <div class="guest-row-actions">
                      <button type="button" class="button success small" on:click={() => approveUpload(u.id)}>
                        Approve
                      </button>
                      <button type="button" class="button danger small" on:click={() => rejectUpload(u.id)}>
                        Reject
                      </button>
                    </div>
                  </div>
                {/each}
              </div>
            </div>
          {/if}

          <!-- Pending Join Requests -->
          <div class="activity-card">
            <div class="act-card-head">
              <h4>Pending Guest Approvals</h4>
              <span class="count-pill">{guestRoomState?.pendingGuests?.length || 0}</span>
            </div>
            <div class="act-list">
              {#each (guestRoomState?.pendingGuests || []) as g (g.id)}
                <div class="pending-guest-row">
                  <div class="guest-avatar">👤</div>
                  <div class="guest-meta">
                    <strong>{g.nickname || 'Guest'}</strong>
                    <span class="tabular">Requested {fmtTime(g.createdAt)}</span>
                  </div>
                  <div class="guest-row-actions">
                    <button type="button" class="button success small" on:click={() => approveGuest(g.id)}>
                      Approve
                    </button>
                    <button type="button" class="button danger small" on:click={() => rejectGuest(g.id)}>
                      Reject
                    </button>
                  </div>
                </div>
              {:else}
                <div class="empty-compact">No pending guest join requests.</div>
              {/each}
            </div>
          </div>

          <!-- Connected Guests -->
          <div class="activity-card">
            <div class="act-card-head">
              <h4>Connected Guests</h4>
              <span class="count-pill">{guestRoomState?.approvedGuests?.length || 0}</span>
            </div>
            <div class="act-list">
              {#each (guestRoomState?.approvedGuests || []) as g (g.id)}
                <div class="connected-guest-row">
                  <div class="guest-avatar connected">✓</div>
                  <div class="guest-meta">
                    <strong>{g.nickname}</strong>
                    <span class="tabular">Connected {fmtTime(g.createdAt)}</span>
                  </div>
                  <button type="button" class="button ghost small" on:click={() => revokeGuest(g.id)}>
                    Revoke
                  </button>
                </div>
              {:else}
                <div class="empty-compact">No guests connected yet.</div>
              {/each}
            </div>
          </div>
        </div>

        <!-- Right: Received Files -->
        <div class="activity-column">
          <div class="activity-card">
            <div class="act-card-head">
              <h4>Received Uploads</h4>
              <div class="head-right">
                <span class="count-pill">{guestRoomState?.recentUploads?.length || 0}</span>
                <button type="button" class="button ghost small" on:click={openFolder}>
                  Open Folder
                </button>
              </div>
            </div>

            <div class="act-list">
              {#each (guestRoomState?.recentUploads || []) as up (up.id)}
                <div class="upload-file-row">
                  <div class="file-icon-box">📁</div>
                  <div class="file-meta-col">
                    <strong>{up.fileName}</strong>
                    <span class="file-sub-info tabular">
                      From {up.guestName || 'Guest'} · {formatBytes(up.size)} · {fmtTime(up.createdAt)}
                    </span>
                    {#if up.sha256}
                      <button
                        type="button"
                        class="sha-copy-chip tabular"
                        on:click={() => { copyTextToClipboard(up.sha256 || ''); showToast('Copied SHA-256', 'info'); }}
                      >
                        SHA256: {up.sha256.slice(0, 10)}…
                      </button>
                    {/if}
                  </div>
                  <span class="upload-status-tag {up.state}">{up.state}</span>
                </div>
              {:else}
                <div class="empty-compact">No files received in this room yet.</div>
              {/each}
            </div>
          </div>
        </div>
      </div>
    </div>
  {/if}
</div>

<!-- End Room Confirmation Modal -->
<ConfirmModal
  isOpen={confirmEndRoomOpen}
  title="End Guest Drop Box Room?"
  message="Ending the room immediately stops accepting new guests and file uploads. Existing received files will be kept safely on disk."
  confirmText="End Room"
  danger={true}
  onConfirm={handleEndRoomConfirm}
  onCancel={() => { confirmEndRoomOpen = false }}
/>

<style>
  .guest-view-container {
    width: 100%;
    display: flex;
    flex-direction: column;
    gap: 20px;
  }

  /* WIZARD STEPPER */
  .wizard-stepper-bar {
    display: flex;
    align-items: center;
    justify-content: center;
    gap: 16px;
    padding: 12px 24px;
    background: var(--bg-surface);
    border: 1px solid var(--border-base);
    border-radius: var(--radius-lg, 14px);
    margin-bottom: 8px;
  }

  .step-nav-btn {
    display: inline-flex;
    align-items: center;
    gap: 10px;
    background: transparent;
    border: none;
    cursor: pointer;
    padding: 6px 12px;
    border-radius: 999px;
    color: var(--text-muted);
    font-size: 13.5px;
    font-weight: 600;
    transition: all var(--duration-fast, 150ms) ease;
  }

  .step-nav-btn.active {
    color: var(--accent);
    background: var(--accent-subtle);
  }

  .step-nav-btn.completed {
    color: var(--success);
  }

  .step-num {
    width: 24px;
    height: 24px;
    border-radius: 50%;
    display: flex;
    align-items: center;
    justify-content: center;
    font-size: 11.5px;
    font-weight: 700;
    background: var(--bg-surface-elevated);
    color: inherit;
  }

  .step-nav-btn.active .step-num {
    background: var(--accent);
    color: var(--accent-text);
  }

  .step-nav-btn.completed .step-num {
    background: var(--success);
    color: #ffffff;
  }

  .stepper-line {
    width: 48px;
    height: 2px;
    background: var(--border-base);
    border-radius: 2px;
  }

  .stepper-line.filled {
    background: var(--success);
  }

  /* STEP CARD */
  .step-card {
    background: var(--bg-surface);
    border: 1px solid var(--border-base);
    border-radius: var(--radius-lg, 14px);
    box-shadow: var(--shadow-sm);
    padding: 24px 28px;
    display: flex;
    flex-direction: column;
    gap: 20px;
  }

  .step-header h3 {
    margin: 0 0 4px;
    font-size: 18px;
    font-weight: 700;
  }

  .step-header p {
    margin: 0;
    font-size: 13px;
    color: var(--text-secondary);
  }

  /* PRESETS GRID */
  .preset-cards-grid {
    display: grid;
    grid-template-columns: repeat(auto-fit, minmax(200px, 1fr));
    gap: 12px;
  }

  .preset-card {
    background: var(--bg-surface-elevated);
    border: 1.5px solid var(--border-base);
    border-radius: var(--radius-md, 10px);
    padding: 14px;
    text-align: left;
    cursor: pointer;
    display: flex;
    align-items: flex-start;
    gap: 12px;
    position: relative;
    transition: all var(--duration-fast, 150ms) ease;
  }

  .preset-card:hover {
    border-color: var(--accent);
  }

  .preset-card.selected {
    border-color: var(--accent);
    background: var(--bg-surface-selected);
  }

  .preset-icon {
    font-size: 24px;
  }

  .preset-info strong {
    font-size: 13.5px;
    display: block;
    margin-bottom: 3px;
    color: var(--text-primary);
  }

  .preset-info p {
    margin: 0;
    font-size: 11.5px;
    color: var(--text-secondary);
    line-height: 1.35;
  }

  .preset-check {
    position: absolute;
    top: 10px;
    right: 12px;
    font-weight: 700;
    color: var(--accent);
  }

  .form-section-group {
    display: flex;
    flex-direction: column;
    gap: 14px;
  }

  .form-row {
    display: flex;
    flex-direction: column;
    gap: 6px;
  }

  .form-row label {
    font-size: 12.5px;
    font-weight: 600;
    color: var(--text-primary);
  }

  .req {
    color: var(--danger);
  }

  .wizard-footer-actions {
    display: flex;
    justify-content: space-between;
    align-items: center;
    border-top: 1px solid var(--border-base);
    padding-top: 16px;
  }

  .wizard-footer-actions.right {
    justify-content: flex-end;
  }

  /* STEP 2: SPLIT LAYOUT */
  .step-two-layout {
    display: grid;
    grid-template-columns: 1fr 320px;
    gap: 32px;
  }

  @media (max-width: 900px) {
    .step-two-layout {
      grid-template-columns: 1fr;
    }
  }

  .form-side-column {
    display: flex;
    flex-direction: column;
    gap: 18px;
  }

  .subhead-label {
    font-size: 12px;
    font-weight: 700;
    text-transform: uppercase;
    letter-spacing: 0.03em;
    color: var(--text-muted);
    display: block;
    margin-bottom: 8px;
  }

  .field-chips-row {
    display: flex;
    flex-wrap: wrap;
    gap: 8px;
  }

  .field-toggle-chip {
    padding: 6px 12px;
    border-radius: 999px;
    background: var(--bg-surface-elevated);
    border: 1px solid var(--border-base);
    font-size: 12px;
    font-weight: 600;
    color: var(--text-secondary);
    cursor: pointer;
    transition: all var(--duration-fast, 150ms) ease;
  }

  .field-toggle-chip:hover {
    border-color: var(--accent);
  }

  .field-toggle-chip.active {
    background: var(--accent-subtle);
    border-color: var(--accent);
    color: var(--accent);
  }

  .fields-list-box {
    display: flex;
    flex-direction: column;
    gap: 6px;
  }

  .field-item-row {
    display: flex;
    justify-content: space-between;
    align-items: center;
    padding: 8px 12px;
    background: var(--bg-surface-elevated);
    border: 1px solid var(--border-subtle);
    border-radius: var(--radius-sm, 6px);
  }

  .field-drag-label {
    display: flex;
    align-items: center;
    gap: 8px;
  }

  .field-drag-label strong {
    font-size: 12.5px;
  }

  .field-type-tag {
    font-size: 10px;
    background: var(--bg-surface-secondary);
    padding: 1px 6px;
    border-radius: 4px;
    color: var(--text-muted);
  }

  .remove-field-btn {
    background: transparent;
    border: none;
    font-size: 18px;
    color: var(--text-muted);
    cursor: pointer;
  }

  .remove-field-btn:hover {
    color: var(--danger);
  }

  .empty-fields-note {
    font-size: 12px;
    color: var(--text-muted);
    font-style: italic;
  }

  .preview-side-column {
    display: flex;
    flex-direction: column;
    align-items: center;
    gap: 10px;
  }

  .preview-label-tag {
    font-size: 11px;
    font-weight: 700;
    text-transform: uppercase;
    color: var(--text-muted);
  }

  /* STEP 3: STORAGE & DURATION */
  .storage-options-grid {
    display: flex;
    flex-direction: column;
    gap: 16px;
  }

  .option-subcard {
    background: var(--bg-surface-elevated);
    border: 1px solid var(--border-base);
    border-radius: var(--radius-md, 10px);
    padding: 16px;
  }

  .subcard-title {
    font-size: 13.5px;
    font-weight: 700;
    display: block;
    margin-bottom: 2px;
  }

  .subcard-desc {
    margin: 0 0 12px;
    font-size: 12px;
    color: var(--text-secondary);
  }

  .path-display-box {
    display: flex;
    align-items: center;
    justify-content: space-between;
    background: var(--bg-surface-input);
    border: 1px solid var(--border-input);
    border-radius: var(--radius-sm, 6px);
    padding: 8px 12px;
    gap: 12px;
  }

  .path-text {
    font-family: monospace;
    font-size: 12px;
    color: var(--text-primary);
    white-space: nowrap;
    overflow: hidden;
    text-overflow: ellipsis;
  }

  .path-actions {
    display: flex;
    gap: 6px;
  }

  .policy-radio-group {
    display: grid;
    grid-template-columns: 1fr 1fr;
    gap: 12px;
  }

  .policy-card {
    display: flex;
    align-items: flex-start;
    gap: 10px;
    padding: 12px;
    background: var(--bg-surface);
    border: 1.5px solid var(--border-base);
    border-radius: var(--radius-sm, 6px);
    cursor: pointer;
  }

  .policy-card.selected {
    border-color: var(--accent);
    background: var(--bg-surface-selected);
  }

  .policy-card strong {
    font-size: 12.5px;
    display: block;
    margin-bottom: 2px;
  }

  .policy-card span {
    font-size: 11.5px;
    color: var(--text-secondary);
  }

  .duration-pills-row {
    display: flex;
    flex-wrap: wrap;
    gap: 8px;
  }

  .duration-pill {
    padding: 6px 14px;
    border-radius: 999px;
    background: var(--bg-surface-elevated);
    border: 1px solid var(--border-base);
    font-size: 12px;
    font-weight: 600;
    color: var(--text-secondary);
    cursor: pointer;
    transition: all var(--duration-fast, 150ms) ease;
  }

  .duration-pill.active {
    background: var(--accent);
    border-color: var(--accent);
    color: var(--accent-text);
  }

  .launch-summary-strip {
    display: flex;
    justify-content: space-between;
    align-items: center;
    background: var(--accent-subtle);
    border: 1px solid var(--accent-border);
    border-radius: var(--radius-md, 10px);
    padding: 16px 20px;
    margin-top: 8px;
  }

  .summary-meta strong {
    display: block;
    font-size: 14px;
  }

  .summary-meta span {
    font-size: 12px;
    color: var(--text-secondary);
  }

  /* ACTIVE DASHBOARD */
  .active-room-dashboard {
    display: flex;
    flex-direction: column;
    gap: 20px;
  }

  .dashboard-header-card {
    background: var(--bg-surface);
    border: 1px solid var(--border-base);
    border-radius: var(--radius-lg, 14px);
    padding: 20px 24px;
    display: flex;
    justify-content: space-between;
    align-items: center;
    box-shadow: var(--shadow-sm);
  }

  .dash-status-row {
    display: flex;
    align-items: center;
    gap: 10px;
    margin-bottom: 6px;
  }

  .active-pulse-badge {
    display: inline-flex;
    align-items: center;
    gap: 6px;
    padding: 3px 10px;
    border-radius: 999px;
    background: var(--success-subtle);
    color: var(--success);
    font-size: 11.5px;
    font-weight: 700;
  }

  .countdown-chip {
    font-size: 12px;
    font-weight: 600;
    color: var(--text-secondary);
  }

  .dash-title-group h2 {
    margin: 0 0 4px;
    font-size: 20px;
    font-weight: 700;
  }

  .dash-title-group p {
    margin: 0;
    font-size: 13px;
    color: var(--text-secondary);
  }

  .dash-header-actions {
    display: flex;
    gap: 10px;
  }

  .active-room-hub-grid {
    display: grid;
    grid-template-columns: 220px 1fr;
    gap: 20px;
  }

  .room-qr-card {
    background: var(--bg-surface);
    border: 1px solid var(--border-base);
    border-radius: var(--radius-lg, 14px);
    padding: 18px;
    display: flex;
    flex-direction: column;
    align-items: center;
    gap: 10px;
    text-align: center;
  }

  .qr-display-frame {
    width: 170px;
    height: 170px;
    background: #ffffff;
    padding: 6px;
    border-radius: 12px;
    border: 2px solid var(--border-base);
    box-shadow: var(--shadow-sm);
    display: flex;
    align-items: center;
    justify-content: center;
  }

  .room-qr-img {
    width: 100%;
    height: 100%;
    object-fit: contain;
  }

  .qr-subtext {
    font-size: 11px;
    color: var(--text-muted);
  }

  .room-link-details-card {
    background: var(--bg-surface);
    border: 1px solid var(--border-base);
    border-radius: var(--radius-lg, 14px);
    padding: 20px 24px;
    display: flex;
    flex-direction: column;
    gap: 12px;
  }

  .room-link-details-card h4 {
    margin: 0;
    font-size: 16px;
    font-weight: 700;
  }

  .room-link-details-card p {
    margin: 0;
    font-size: 13px;
    color: var(--text-secondary);
  }

  .invite-url-box {
    display: flex;
    align-items: center;
    justify-content: space-between;
    background: var(--bg-surface-elevated);
    border: 1px solid var(--border-base);
    border-radius: var(--radius-md, 10px);
    padding: 8px 14px;
  }

  .invite-url {
    font-family: monospace;
    font-size: 13.5px;
    color: var(--accent);
    font-weight: 600;
  }

  .room-stats-summary-row {
    display: grid;
    grid-template-columns: repeat(3, 1fr);
    gap: 12px;
    margin-top: 4px;
  }

  .stat-box {
    background: var(--bg-surface-elevated);
    border: 1px solid var(--border-subtle);
    border-radius: var(--radius-sm, 6px);
    padding: 10px 14px;
    display: flex;
    flex-direction: column;
  }

  .stat-num {
    font-size: 20px;
    font-weight: 700;
    color: var(--text-primary);
  }

  .stat-lbl {
    font-size: 11px;
    color: var(--text-muted);
  }

  .room-activity-grid {
    display: grid;
    grid-template-columns: 1fr 1fr;
    gap: 20px;
  }

  .activity-column {
    display: flex;
    flex-direction: column;
    gap: 16px;
  }

  .activity-card {
    background: var(--bg-surface);
    border: 1px solid var(--border-base);
    border-radius: var(--radius-lg, 14px);
    padding: 18px 20px;
    display: flex;
    flex-direction: column;
    gap: 12px;
  }

  .act-card-head {
    display: flex;
    justify-content: space-between;
    align-items: center;
  }

  .act-card-head h4 {
    margin: 0;
    font-size: 15px;
    font-weight: 700;
  }

  .head-right {
    display: flex;
    align-items: center;
    gap: 8px;
  }

  .act-list {
    display: flex;
    flex-direction: column;
    gap: 8px;
    max-height: 280px;
    overflow-y: auto;
  }

  .pending-guest-row, .connected-guest-row, .upload-file-row {
    display: flex;
    align-items: center;
    gap: 10px;
    padding: 8px 12px;
    background: var(--bg-surface-elevated);
    border: 1px solid var(--border-subtle);
    border-radius: var(--radius-sm, 6px);
  }

  .guest-avatar {
    width: 28px;
    height: 28px;
    border-radius: 50%;
    background: var(--accent-subtle);
    color: var(--accent);
    display: flex;
    align-items: center;
    justify-content: center;
    font-size: 12px;
  }

  .guest-avatar.connected {
    background: var(--success-subtle);
    color: var(--success);
  }

  .guest-meta {
    flex: 1;
    min-width: 0;
    display: flex;
    flex-direction: column;
  }

  .guest-meta strong {
    font-size: 12.5px;
  }

  .guest-meta span {
    font-size: 11px;
    color: var(--text-muted);
  }

  .guest-row-actions {
    display: flex;
    gap: 6px;
  }

  .file-icon-box {
    font-size: 18px;
  }

  .file-meta-col {
    flex: 1;
    min-width: 0;
    display: flex;
    flex-direction: column;
    gap: 2px;
  }

  .file-meta-col strong {
    font-size: 12.5px;
    white-space: nowrap;
    overflow: hidden;
    text-overflow: ellipsis;
  }

  .file-sub-info {
    font-size: 11px;
    color: var(--text-muted);
  }

  .sha-copy-chip {
    align-self: flex-start;
    background: transparent;
    border: none;
    font-family: monospace;
    font-size: 10px;
    color: var(--text-muted);
    cursor: pointer;
    padding: 0;
  }

  .sha-copy-chip:hover {
    text-decoration: underline;
    color: var(--accent);
  }

  .upload-status-tag {
    font-size: 10px;
    font-weight: 700;
    padding: 2px 6px;
    border-radius: 4px;
    text-transform: uppercase;
  }

  .upload-status-tag.completed {
    background: var(--success-subtle);
    color: var(--success);
  }

  .empty-compact {
    font-size: 12px;
    color: var(--text-muted);
    font-style: italic;
    padding: 8px 0;
    text-align: center;
  }

  .chip-pulse {
    width: 6px;
    height: 6px;
    border-radius: 50%;
    background: currentColor;
  }

  .chip-pulse.active {
    box-shadow: 0 0 0 3px var(--success-border);
  }
</style>
