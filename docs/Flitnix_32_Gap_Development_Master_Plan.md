# Flitnix --- 32-Gap Development Master Plan

**Document type:** Product and engineering roadmap\
**Basis:** Flitnix competitive analysis, codebase audit and 32-gap
feature breakdown\
**Planning status:** Proposed execution plan --- confirm implementation
status and dependencies against the current repository before each task\
**Target releases:** v0.3.0, v0.4.0, v0.4.1 and v0.5.0+\
**Total scope:** 32 gaps across 6 implementation phases, preceded by one
engineering-preparation phase

------------------------------------------------------------------------

## 1. Purpose

This document turns the 32 feature gaps identified in the Flitnix
competitive analysis into one sequenced development plan. It is intended
to be the working master reference for implementation prompts, code
reviews, testing, release decisions and progress tracking.

The roadmap follows these rules:

1.  **Reliability and safety first.** Protect data integrity,
    resumability, storage and authorization before adding convenience
    features.
2.  **Reuse the current architecture.** The report identifies Go
    standard-library HTTP, Wails v2, Svelte 5 and TypeScript as a
    suitable foundation. A framework rewrite is not part of this plan.
3.  **Finish partial implementations.** Where Flitnix already has a
    backend primitive or UI surface, complete and connect it rather than
    building a parallel subsystem.
4.  **Build prerequisites before dependents.** Where a feature has a
    dependency, deliver the underlying capability first or split the
    work into a safe foundation and a later integration.
5.  **One gap or tightly coupled feature group at a time.** Each
    implementation must be tested, reviewed, documented and committed
    before the next unrelated task begins.
6.  **Preserve Flitnix's local-first identity.** Advanced networking,
    compression, clipboard sync and automation must remain opt-in where
    specified.
7.  **Do not mark a gap complete because code exists.** A gap is
    complete only when implementation, security review, regression tests
    and acceptance criteria pass.

### What this plan is --- and is not

-   It is a phased implementation roadmap based on the source report's
    32 detailed gap cards.
-   It preserves the original gap numbers, names, priority scores,
    effort estimates and technical notes.
-   It adds an execution sequence, phase deliverables, acceptance
    criteria and dependency-resolution notes.
-   Release labels are proposed milestones, not promises of calendar
    dates.
-   The original report's detailed gap cards are retained below each
    phase so the technical context is not lost.

------------------------------------------------------------------------

## 2. Roadmap at a glance

  -----------------------------------------------------------------------------------
  Phase         Name                        Gaps Proposed      Primary outcome
                                                 release       
  ------------- -------------- ----------------- ------------- ----------------------
  0             Engineering     Preparation only Before        Safe, repeatable
                Preparation &                    development   implementation process
                Delivery Rules                                 

  1             Unbreakable                    6 v0.3.0        Resumable,
                Transfers                                      integrity-preserving
                                                               transfers

  2             Discovery &                    6 v0.3.0        Nearby discovery,
                Trusted                                        pairing and desktop
                Devices                                        sender

  3             Always-Ready                   4 v0.3.0        Background operation,
                Desktop                                        notifications and
                                                               history

  4             Secure &                       5 v0.4.0        Encrypted peers, PWA
                Advanced                                       and group sharing
                Sharing                                        

  5             Productivity &                 7 v0.4.1        Previews, queues,
                User                                           organization and
                Experience                                     platform polish

  6             Advanced &                     4 v0.5.0+       Relay, sync, media
                Optional                                       optimization and
                Features                                       compact mode
  -----------------------------------------------------------------------------------

### Priority distribution

  --------------------------------------------------------------------------------
  Priority                                     Count Meaning
  --------------------- ---------------------------- -----------------------------
  P0                                              14 Foundational/highest-impact
                                                     opportunities

  P1                                              12 Important usability, security
                                                     and productivity improvements

  P2                                               3 Advanced or more specialized
                                                     capabilities

  P3                                               3 Optional/lower-priority
                                                     enhancements

  **Total**                                   **32** Every gap is assigned to
                                                     exactly one phase
  --------------------------------------------------------------------------------

The priority tier comes from the source report's weighted scoring. Phase
order is an implementation decision and may differ from pure score rank
when dependencies or safety prerequisites require it.

### Proposed release grouping

-   **v0.3.0 --- Seamless & Unbreakable:** Phases 1, 2 and 3.
-   **v0.4.0 --- Secure & Expanded Sharing:** Phase 4.
-   **v0.4.1 --- Productivity & UX:** Phase 5.
-   **v0.5.0 and beyond --- Advanced Optional Features:** Phase 6.

These are planning labels. Actual release boundaries should be confirmed
after implementation estimates, code audits and cross-platform testing.

------------------------------------------------------------------------

## 3. Global implementation rules

### 3.1 Architecture

Keep the existing product stack as the default:

-   Go backend and standard-library HTTP server.
-   Wails v2 desktop application.
-   Svelte 5 and TypeScript frontend.
-   Existing `internal/transfer`, `internal/server`, `internal/devices`,
    `internal/guest`, `internal/recovery`, `internal/audit`,
    `internal/doctor`, `internal/client` and settings packages where
    applicable.
-   Existing mobile browser assets and worker-based upload path.

Do not replace the framework or rewrite working subsystems as part of an
individual gap unless a separate architecture decision documents why it
is necessary.

### 3.2 Definition of done for every gap

A gap may be marked complete only when all applicable items are
satisfied:

-   [ ] Existing code and partial implementation have been audited.
-   [ ] API contracts and data structures are documented.
-   [ ] Backend implementation is complete.
-   [ ] Desktop UI is complete where applicable.
-   [ ] Mobile browser behavior is complete where applicable.
-   [ ] Authorization, path confinement, input validation and resource
    limits are reviewed.
-   [ ] Unit tests pass.
-   [ ] Integration tests pass.
-   [ ] Regression tests pass.
-   [ ] Race detector and concurrency tests pass where relevant.
-   [ ] Windows/macOS/Linux build or behavior checks pass where
    relevant.
-   [ ] Android/iOS browser checks pass where relevant.
-   [ ] Documentation and changelog are updated.
-   [ ] Code is reviewed and committed.
-   [ ] No unrelated feature has been silently added to the scope.

### 3.3 Security and product guardrails

Keep these principles across all phases:

-   No mandatory user accounts or sign-in.
-   No mandatory cloud storage.
-   No advertisements.
-   No telemetry or phone-home tracking.
-   No forced image or video compression.
-   No unnecessary phone permissions.
-   No remote camera, SMS mirroring or screen mirroring.
-   No silent third-party relay selection.
-   No automatic acceptance of unknown devices.
-   No bypass of approval, trusted-device revocation or
    session-expiration checks.
-   No exposure of raw filesystem paths or session secrets in discovery
    messages, URLs or logs.
-   No unbounded memory, disk, concurrency or history growth.

------------------------------------------------------------------------

## Phase 0 --- Engineering Preparation & Delivery Rules

**Status:** Required preparation\
**Release:** Pre-development\
**Objective:** Create a safe, repeatable implementation process for the
32 gaps. This is a preparation phase, not an additional feature gap.

### Preparation sequence

1.  Create a dedicated roadmap branch and confirm the current
    release/build baseline.
2.  Run the existing test suite and record the baseline build/test
    status before each phase.
3.  Audit the current implementation before coding each gap; reuse
    existing packages and partial implementations rather than
    duplicating them.
4.  For every gap, write down the API/data-contract changes, backend
    ownership, desktop/mobile UI work, security implications,
    dependencies and acceptance tests.
5.  Keep changes small enough to review and revert independently; avoid
    bundling unrelated gaps into one large change.
6.  Update technical documentation, user-facing behavior notes and the
    changelog as each gap is completed.
7.  Run cross-platform checks for Windows, macOS and Linux, and
    real-device checks for Android/iOS browser behavior where relevant.

### Deliverables

-   Baseline build and test report.
-   A task/branch per gap or tightly coupled feature group.
-   An architecture note and acceptance checklist for each
    implementation.
-   A release checklist covering regression tests, documentation and
    packaging.

### Exit criteria

-   [ ] The current application builds before work begins.
-   [ ] Existing tests pass or known failures are documented.
-   [ ] Every gap has an owner, dependencies and test plan before
    implementation.
-   [ ] No framework rewrite is introduced without a separate,
    evidence-based decision.

------------------------------------------------------------------------

## Phase 1 --- Unbreakable Transfers

**Proposed release:** v0.3.0\
**Gap count:** 6\
**Priority mix:** P0: 4, P1: 2\
**Objective:** Make transfer reliability the first product improvement.
Prioritize disk safety, resumable upload recovery, transient network
retry, resumable downloads and selective shared-folder downloads.

### Phase execution order

1.  G26 --- implement the core free-disk-space check and pre-approval
    quota protection first. Defer the history-retention portion until G6
    is delivered in Phase 3.
2.  G1 --- wire the recovery directory into production entry points and
    implement authenticated, offset-checked resumable uploads.
3.  G7 --- add retry/pause behavior for transient network failures,
    integrated with the resumable upload offset.
4.  G2 --- enable safe HTTP byte-range downloads for mutable files using
    validators that detect file mutation.
5.  G31 --- expose a safe, read-only folder tree so a phone can select
    individual files instead of downloading a whole ZIP.
6.  G22 --- preserve validated original modification timestamps when
    publishing received files.

### Phase deliverables

-   Crash-safe, resumable uploads with durable checkpoints.
-   Automatic retry and Wi-Fi roaming recovery.
-   Resumable HTTP 206 downloads for supported mutable files.
-   Pre-transfer disk-space checks and clear insufficient-storage
    errors.
-   Read-only browsing and selective download of explicitly shared
    folder contents.
-   Original modification-date preservation.

### Phase exit criteria

-   [ ] A dropped upload resumes from the last committed offset rather
    than byte zero.
-   [ ] A process crash/restart does not publish partial or corrupted
    files.
-   [ ] Offsets cannot be used to create sparse writes, overwrite
    committed bytes or bypass session/item authorization.
-   [ ] A download never combines byte ranges from two different
    versions of a mutable file.
-   [ ] Insufficient disk space is detected before accepting a transfer
    where the platform can reliably report it.
-   [ ] Shared-folder browsing exposes opaque item identifiers, not raw
    filesystem paths, and cannot escape the shared root.
-   [ ] SHA-256/integrity verification and atomic publication remain
    intact.
-   [ ] Large-file soak tests and network-drop tests pass.

### Detailed gap specifications

### Gap 26: Storage Usage Dashboard, Disk Headroom Warnings & Auto-Cleanup

1.  **Feature Name**: Storage Usage Bar, Pre-Transfer Disk Check &
    History Auto-Retention
2.  **Description**: Show real-time free disk space in the target
    receive directory, warn/reject oversized incoming batches *before*
    approval (`507 Insufficient Storage` check up front), show storage
    breakdown in History, and configure automatic pruning of old
    history/logs.
3.  **Competitors**: AirDroid (storage panel), Blip (pre-checks).
4.  **User Problem**: Docs mention `QuotaManager`
    (`phase5-security-audit.md`), **yet `QuotaManager` does not exist in
    `internal/transfer/`**---a user can approve a 40 GB batch onto a
    drive with 12 GB free and fail mid-transfer!
5.  **Flitnix Status**: Missing (no disk-space check in `store.go` or
    `service.go`!).
6.  **Why It Improves Flitnix**: Prevents wasted multi-GB transfers and
    gives instant storage visibility.
7.  **Expected User Impact**: Medium-High.
8.  **Technical Complexity**: Low-Medium
9.  **Development Effort**: 1 week
10. **Security Implications**: Prevents local disk-exhaustion DoS
    (`507 Insufficient Storage` before allocating `.part` files).
11. **Backend Changes**: Cross-platform `diskFreeBytes(dir)`
    (`syscall.Statfs` / `GetDiskFreeSpaceExW`), pre-approval check +
    `GetStorageStatus()` in `app.go`, retention cleanup job.
12. **Frontend Changes**: Disk headroom indicator on Approval Inbox
    cards + Storage settings retention slider.
13. **Mobile Changes**: Friendly `"Desktop storage is low"` error on
    `507`.
14. **Testing**: Mocked low-disk-space tests returning `507`.
15. **Dependencies**: Persistent History (Gap 6).

-   **Scores**: UV: 4, SA: 5, CA: 2, F: 4, RM: 4 → **Weighted Score:
    3.85 (P1)**

### Gap 1: Resumable Chunked Uploads & Crash-Safe Offset Recovery

1.  **Feature Name**: Resumable Chunked Uploads & Offset Recovery
2.  **Description**: Replace single-shot byte-0 upload retries with
    chunked/offset-resumable `PATCH` (or Tus-compatible) streaming plus
    `HEAD` offset probing, backed by `.part` + `.meta` checkpoints and
    `RecoveryDir` enabled in `app.go` and `cli.go`.
3.  **Competitors**: Blip (auto-resume across drops/reboots), Croc (full
    resume), AirDrop (iOS 17+ internet resume). *LocalSend & PairDrop
    lack this---massive differentiator!*
4.  **User Problem**: Uploading a 10--100 GB video or folder over Wi-Fi
    fails if the phone locks, switches APs, or hits a 30s stall, forcing
    a frustrating restart from 0%.
5.  **Flitnix Status**: Missing in runtime (`app.js` restarts at byte 0;
    `internal/recovery/store.go` exists but is unwired in
    `app.go`/`cli.go`).
6.  **Why It Improves Flitnix**: Makes multi-gigabyte mobile-to-desktop
    transfers reliable in real-world RF environments while beating
    LocalSend and PairDrop on core reliability.
7.  **Expected User Impact**: Very High --- eliminates the #1 pain point
    for large media transfers.
8.  **Technical Complexity**: High
9.  **Development Effort**: 3--4 weeks
10. **Security Implications**: Must bind upload offsets to authenticated
    session/item lease, enforce `offset == committedOffset` (no
    arbitrary sparse writes/overwrites), and cap concurrent resumable
    `.part` storage via disk-quota checks (`507 Insufficient Storage`).
11. **Backend Changes**: Add `internal/transfer/resumable.go`
    (checkpointed `.part` + `.meta` with pipelined/periodic `Sync`),
    `PATCH`/`HEAD` handlers in `internal/server/`, wire `RecoveryDir` in
    `app.go` & `cli.go`.
12. **Frontend Changes**: Show `"Paused / Resumable (4.2 / 12.0 GB)"`
    badge and Resume/Discard actions in `PersonalTransferView.svelte`.
13. **Mobile Changes**: Update `upload-worker.js` and `upload-client.js`
    to slice `File.slice(offset, offset + chunkSize)` (e.g., 16--32 MiB
    chunks), query `HEAD` on reconnect, and auto-retry with backoff from
    `committedOffset`.
14. **Testing**: Subprocess crash tests (`os.Exit(1)` at sync/rename
    boundaries), TCP RST proxy drop-and-resume tests, race detector
    tests, 10--50 GB soak tests.
15. **Dependencies**: Automatic Retry with Backoff (Gap 7) pairs tightly
    with this.

-   **Scores**: UV: 5, SA: 5, CA: 5, F: 3, RM: 3 → **Weighted Score:
    4.50 (P0)**

### Gap 7: Automatic Stream Retry with Exponential Backoff & Network Roaming Recovery

1.  **Feature Name**: Automatic Data-Stream Retry & Wi-Fi Roaming
    Recovery
2.  **Description**: When a `PUT` upload or download stream drops due to
    transient Wi-Fi roaming, AP handoff, or socket reset (`network` /
    `timeout`), automatically pause, probe `/ping`, re-query `HEAD`
    committed offset (with Gap 1), and resume with exponential backoff
    (e.g., 1s, 2s, 4s, 8s) instead of immediately failing the whole
    batch (`FailBatch`).
3.  **Competitors**: Blip, Croc, AirDrop.
4.  **User Problem**: Walking between rooms or a 2-second Wi-Fi blip
    currently fails an entire multi-file batch (`FailBatch` cancels
    remaining items in `routes.go:1241`).
5.  **Flitnix Status**: Partial (`app.js` retries control-plane
    *polling* with `backoffMs`, and `netenv.Monitor` detects desktop IP
    changes, but data streams abort the entire batch on first socket
    error).
6.  **Why It Improves Flitnix**: Directly addresses the "Physical RF
    Wi-Fi & AP Handoff" gap noted in `README.md:134`.
7.  **Expected User Impact**: High --- transfers survive brief Wi-Fi
    dropouts transparently.
8.  **Technical Complexity**: Medium
9.  **Development Effort**: 1.5 weeks
10. **Security Implications**: Ensure failed-item retry preserves
    batch/item lease validation and does not bypass session expiration
    or device revocation checks.
11. **Backend Changes**: Add `ItemStatePaused` / soft-retry grace window
    in `coordinator.go` (do not immediately cancel all sibling batch
    items on transient stream EOF/reset).
12. **Frontend Changes**: Show `"Reconnecting (attempt 2/5)…"` state in
    Active Transfers dock.
13. **Mobile Changes**: Add retry state machine around
    `FlitnixUpload.stream` in `app.js` + `online` event listener wakeup.
14. **Testing**: Extend `phase4_streaming_network_simulation_test.go`
    TCP RST proxy tests to verify multi-file batch auto-recovery without
    manual button clicks.
15. **Dependencies**: Resumable Chunked Uploads (Gap 1) maximizes value;
    partial benefit even for small files if batch isn't torn down.

-   **Scores**: UV: 5, SA: 5, CA: 4, F: 3, RM: 3 → **Weighted Score:
    4.30 (P0)**

### Gap 2: Resumable Byte-Range Downloads (`Accept-Ranges: bytes`) for Mutable Files

1.  **Feature Name**: Resumable Byte-Range Downloads (`HTTP 206`)
2.  **Description**: Support `Range` / `If-Range` (`ETag` backed by file
    size + `mtime` + inode/quick-fingerprint) for normal desktop-shared
    single files (`Tier 2` in `archive.go`), so mobile/desktop browsers
    and download managers can pause and resume large downloads.
3.  **Competitors**: Standard web servers, Blip, Send Anywhere (relay),
    requested heavily in LocalSend `#3741`.
4.  **User Problem**: Currently `archive.go` returns
    `Accept-Ranges: none` for all mutable files; a 15 GB movie download
    that hiccups at 90% on Safari/Chrome fails completely.
5.  **Flitnix Status**: Partial (`Tier 1` immutable files support
    `http.ServeContent` ranges in `archive.go:818-840`, but normal files
    hit `Tier 2` with `Accept-Ranges: none`).
6.  **Why It Improves Flitnix**: Enables reliable 10--100 GB
    Desktop→Phone downloads using the phone's native download manager.
7.  **Expected User Impact**: High --- prevents failed multi-GB
    downloads on mobile browsers.
8.  **Technical Complexity**: Medium
9.  **Development Effort**: 1 week
10. **Security Implications**: Must validate `If-Range` / `ETag`
    (`size + mtimeNs`) and abort (`panic(http.ErrAbortHandler)` / close)
    if source file mutates mid-download so a client never stitches two
    versions of a file together.
11. **Backend Changes**: Update `archive.go` Tier 2 &
    `serveFileDownload` (`routes.go`) to validate `If-Range`, serve
    `206 Partial Content` via `http.ServeContent` wrapped in
    `progressSeeker` + `stallWriter`, and compute `Content-Digest` on
    full `200` streams.
12. **Frontend Changes**: Update `DownloadView` in
    `PersonalTransferView.svelte` to aggregate resumed range bytes
    cleanly instead of resetting to 0.
13. **Mobile Changes**: Handle `206` in `showSaveFilePicker` stream
    writer and show accurate resume state in Download Integrity card.
14. **Testing**: `download_range_test.go` cases for Tier 2 `Range`,
    `If-Range` mismatch after file edit (returns `200` fresh), and
    `416 Range Not Satisfiable`.
15. **Dependencies**: None.

-   **Scores**: UV: 5, SA: 5, CA: 4, F: 4, RM: 4 → **Weighted Score:
    4.55 (P0)**

### Gap 31: Desktop-to-Phone / Phone-to-Desktop Remote File Browsing (Opt-In Shared Root)

1.  **Feature Name**: Opt-In Browsable Shared Folder Explorer
2.  **Description**: Instead of forcing the phone to download an entire
    20 GB shared folder as a monolithic `.zip`, let the phone browse
    inside shared folders in `Files on the desktop` (folder tree view)
    and download individual files or sub-selections on demand.
3.  **Competitors**: KDE Connect (SFTP browse), AirDroid (File Manager).
4.  **User Problem**: If you drag a 50 GB project folder onto Flitnix
    just to grab two files on your phone, Flitnix currently forces
    downloading the entire 50 GB `.zip`!
5.  **Flitnix Status**: Missing (`SharedSource` already enumerates every
    `SourceItem` with `RelativePath` and `Size` in `source.go`, but
    `/api/sources` collapses folders into a single flat row!).
6.  **Why It Improves Flitnix**: Huge usability upgrade for shared
    folders using metadata Flitnix *already computes*!
7.  **Expected User Impact**: High --- saves gigabytes of unnecessary
    ZIP downloads.
8.  **Technical Complexity**: Low-Medium
9.  **Development Effort**: 1 week
10. **Security Implications**: Strictly read-only inside explicitly
    shared `SharedSource` items; download by opaque
    `(sourceId, itemIndex/itemId)`---never expose raw filesystem paths.
11. **Backend Changes**: Add `GET /api/sources/{id}/items` returning
    `RelativePath`, `Name`, `Size`, and opaque item IDs, and allow
    `createDownloadGrant` to target specific items inside a
    `SharedSource`.
12. **Frontend Changes**: None needed (already shows item count &
    bytes).
13. **Mobile Changes**: Expandable folder tree / breadcrumb drill-down
    in `app.js` `Files on the desktop`.
14. **Testing**: Path-traversal and unshared-item isolation tests.
15. **Dependencies**: None.

-   **Scores**: UV: 4, SA: 5, CA: 4, F: 5, RM: 5 → **Weighted Score:
    4.50 (P0)** *(Massive hidden-value win!)*

### Gap 22: Modification Timestamp & File Metadata Preservation

1.  **Feature Name**: Original Modification Timestamp (`mtime`)
    Preservation
2.  **Description**: Allow upload manifests (`BatchManifest`) to carry
    original `File.lastModified` timestamps from the phone/CLI and
    restore them via `os.Chtimes` upon atomic publication on desktop.
3.  **Competitors**: Blip, Syncthing, AirDrop, Quick Share.
4.  **User Problem**: When backing up 500 vacation photos from phone to
    PC, every file gets "today's" modification date, breaking
    chronological sorting in file explorers.
5.  **Flitnix Status**: Partial (Desktop→Phone ZIP preserves `ModTime`
    truncated to 2s in `archive.go:206`, but Phone→Desktop discards
    `mtime`).
6.  **Why It Improves Flitnix**: Keeps photo libraries and project
    backups chronologically accurate.
7.  **Expected User Impact**: Medium-High for photo/archive backup
    users.
8.  **Technical Complexity**: Low
9.  **Development Effort**: 2--3 days
10. **Security Implications**: Clamp/validate timestamps (e.g., reject
    negative or far-future dates) and apply `os.Chtimes` *before*
    `os.Link` or safely on the committed path inside `withinDir`.
11. **Backend Changes**: Add optional `LastModifiedMs int64` to
    `ManifestItem` (`batch.go`) and `store.go` `CommitWithModTime`.
12. **Frontend Changes**: Optional toggle "Preserve original file dates"
    (default On) in `SettingsView.svelte`.
13. **Mobile Changes**: Include `lastModified: f.lastModified` in
    `POST /api/batches` items (`app.js`).
14. **Testing**: Cross-platform `os.Chtimes` unit tests.
15. **Dependencies**: None.

-   **Scores**: UV: 4, SA: 5, CA: 3, F: 5, RM: 5 → **Weighted Score:
    4.30 (P1)** *(high ROI quick win!)*

------------------------------------------------------------------------

## Phase 2 --- Discovery & Trusted Devices

**Proposed release:** v0.3.0\
**Gap count:** 6\
**Priority mix:** P0: 6\
**Objective:** Make nearby Flitnix devices discoverable, support
camera-free connection, complete mobile trusted pairing and enable a
direct desktop-to-desktop GUI workflow.

### Phase execution order

1.  G15 --- establish visibility modes and conditional auto-approval
    policy rules before exposing discovery broadly.
2.  G13 --- add a short-lived, single-use six-digit connection code with
    strict rate limiting and lockout.
3.  G4 --- complete mobile browser key enrollment, pairing approval and
    trusted-device reconnection.
4.  G3 --- implement the discovery service bound to the selected LAN
    interface; discovery packets must contain no session tokens.
5.  G9 --- connect the existing desktop client to the GUI and use
    discovered peers or the short code as transfer targets.
6.  G27 --- improve Connection Doctor with read-only OS firewall
    inspection and actionable guidance.

### Phase deliverables

-   Visibility controls: Hidden/Off, Trusted Only, temporary open window
    and Everyone/Open Session.
-   Six-digit code connection flow with expiry, one-time redemption and
    brute-force protection.
-   Mobile trusted-device pairing and one-tap reconnect.
-   Nearby desktop/CLI peer discovery on the local network.
-   Desktop-to-desktop send workflow in the Wails GUI.
-   Firewall diagnostics and copyable remediation guidance.

### Phase exit criteria

-   [ ] Discovery sends only sanitized, non-secret device metadata and
    is bound to the intended active network interface.
-   [ ] Discovery does not leak session tokens, bearer credentials or
    private key material.
-   [ ] Visibility policy is enforced on both discovery and incoming
    transfer requests.
-   [ ] Six-digit codes expire, are single-use and are protected by rate
    limits/lockout.
-   [ ] A previously paired mobile browser can authenticate without
    scanning a fresh QR code, subject to browser storage availability.
-   [ ] Desktop peers can be selected in the GUI and transfer files with
    HostID verification and integrity checks.
-   [ ] Firewall inspection is read-only; Flitnix never silently changes
    firewall policy.
-   [ ] QR and manual-code fallbacks remain available when multicast is
    blocked.

### Detailed gap specifications

### Gap 15: Device Visibility Modes & Granular Auto-Approval Policies

1.  **Feature Name**: Device Visibility Controls & Conditional
    Auto-Accept Rules
2.  **Description**: Add AirDrop/LocalSend-style visibility modes
    (**Everyone / Open Session**, **Trusted Devices Only**, **10-Minute
    Temporary Window**, **Hidden / Off**) plus conditional auto-accept
    rules (e.g., auto-approve files `< 100 MB` from trusted devices, or
    always prompt for executables/scripts).
3.  **Competitors**: AirDrop (10-min Everyone / Contacts / Off), Quick
    Share (Everyone / Contacts / Your Devices / Hidden), LocalSend (Off
    / Favorites / On).
4.  **User Problem**: Currently, Flitnix has only a binary per-device
    `AutoApprove` boolean (all-or-nothing) and no timed "Everyone for 10
    Minutes" mode for safe use in classrooms/offices.
5.  **Flitnix Status**: Partial (`Device.AutoApprove` boolean exists; no
    visibility state machine or conditional policy).
6.  **Why It Improves Flitnix**: Brings AirDrop's anti-spam ergonomics
    to always-on background/tray mode.
7.  **Expected User Impact**: High --- essential safety control once
    tray + mDNS discovery are enabled.
8.  **Technical Complexity**: Low-Medium
9.  **Development Effort**: 1 week
10. **Security Implications**: Prevents "cyberflashing" / unsolicited
    LAN popups when Flitnix runs in background; timed 10-minute open
    mode auto-reverts safely.
11. **Backend Changes**: Add `VisibilityMode` + timer in `app.go` &
    `settings.go`, enforce in `internal/discovery` and incoming
    `/api/batches` / `/api/uploads`, add dangerous-extension prompt
    guard even when `AutoApprove` is true.
12. **Frontend Changes**: Quick visibility switcher pill in
    `PersonalTransferView.svelte` header & tray menu + per-device policy
    rules in `TrustedDevicesView.svelte`.
13. **Mobile Changes**: Clear status message if host is in "Trusted
    Devices Only" or "Hidden" mode.
14. **Testing**: Timer reversion tests, policy enforcement tests for
    executable extensions vs media.
15. **Dependencies**: System Tray (Gap 5) & LAN Discovery (Gap 3).

-   **Scores**: UV: 4, SA: 5, CA: 3, F: 5, RM: 5 → **Weighted Score:
    4.30 (P0)**

### Gap 13: 6-Digit One-Time Transfer / Pairing Code

1.  **Feature Name**: 6-Digit Short Code Connection & Transfer Key
2.  **Description**: Generate a short-lived 6-digit numeric code (e.g.,
    `482-915`) alongside the QR code (and a "Join with Code" input on
    desktop/mobile `/join` or mDNS lookup) so devices without a working
    camera or when typing between two laptops can connect effortlessly.
3.  **Competitors**: Send Anywhere (6-digit key), PairDrop (6-digit
    pairing), LocalSend (PIN).
4.  **User Problem**: Laptops don't have rear cameras to scan QR codes,
    and copying a 65-character `http://192.168.1.42:43821/#...` URL
    between devices is painful.
5.  **Flitnix Status**: Missing for session connection (SAS uses 6
    digits *after* QR scan in CLI pairing only).
6.  **Why It Improves Flitnix**: Makes cross-laptop and camera-less
    pairing as fast as Send Anywhere while staying 100% offline on LAN.
7.  **Expected User Impact**: High --- removes friction whenever QR
    scanning isn't convenient.
8.  **Technical Complexity**: Low-Medium
9.  **Development Effort**: 1 week
10. **Security Implications**: A 6-digit code has only $10^6$
    combinations---must **not** be a static password! Mitigate via:
    strict 2--3 minute TTL, mandatory **rate limiting / exponential
    lockout after 3--5 failed guesses** (reusing `PairingCoordinator`
    lockout logic), single-use redemption exchanged for a full 256-bit
    Bearer token, and optional desktop visual confirmation.
11. **Backend Changes**: Add `CreateShortCode` / `RedeemShortCode` on
    `session.Manager` + `POST /api/connect-code` (unauthenticated,
    IP-rate-limited + global 5-failure lockout) + mDNS TXT/service
    lookup so typing `482915` on another Flitnix desktop resolves the
    host IP automatically!
12. **Frontend Changes**: Show large `482 915` code pill under QR in
    `PersonalTransferView.svelte` + "Connect via 6-Digit Code" modal.
13. **Mobile Changes**: Support `http://<host>:43821/` landing screen
    offering a "Enter 6-Digit Code" fallback in addition to QR scan.
14. **Testing**: Brute-force lockout tests, TTL expiration tests,
    single-use replay rejection tests.
15. **Dependencies**: Optional synergy with LAN Discovery (Gap 3) so
    desktop peers don't even need the IP.

-   **Scores**: UV: 4, SA: 5, CA: 4, F: 4, RM: 4 → **Weighted Score:
    4.25 (P0)**

### Gap 4: Mobile Browser Trusted-Device Pairing & One-Tap Reconnect

1.  **Feature Name**: Mobile Web Trusted-Device Pairing & Key-Backed
    Reconnect
2.  **Description**: Implement the missing client-side WebCrypto/Ed25519
    keypair + IndexedDB persistence in `app.js` to complete the existing
    `internal/devices` SAS pairing flow, plus a dedicated `/auth/device`
    token-exchange endpoint so a paired phone bookmark reconnects in one
    tap *without* scanning a new QR code.
3.  **Competitors**: PairDrop (Persistent Paired Devices), LocalSend
    (Favorites + Quick Save), KDE Connect.
4.  **User Problem**: Right now, users must scan a new QR code every
    single time they open Flitnix, and `TrustedDevicesView.svelte`
    advertises mobile browser pairing that doesn't exist in `app.js`.
5.  **Flitnix Status**: Partial (`internal/devices` + `/api/pair/*` +
    `TrustedDevicesView.svelte` exist, but `app.js` has 0 lines of
    pairing code and `/api/auth/challenge` requires an existing session
    token!).
6.  **Why It Improves Flitnix**: Fulfills an already-built & advertised
    security subsystem and gives returning phone users AirDrop-like
    one-tap reconnection via home-screen bookmark.
7.  **Expected User Impact**: Very High --- eliminates daily QR scanning
    for personal phones/tablets.
8.  **Technical Complexity**: Medium-High
9.  **Development Effort**: 2 weeks
10. **Security Implications**: **Critical detail**: `crypto.subtle`
    Ed25519 on plain HTTP (`http://192.168.x.x`, non-secure context) is
    **blocked by browsers** (`window.crypto.subtle` is `undefined`
    outside HTTPS/localhost!). Solution: bundle a lightweight, audited
    pure-JS Ed25519 + X25519 library (e.g., `@noble/ed25519` /
    `tweetnacl` embedded in `/static/`), store private key in
    `IndexedDB` (or prompt passphrase export), and enforce SAS 6-digit
    visual confirmation + `AutoApprove` toggle on desktop. Expose
    `POST /api/device-sessions` (outside `requireSession`,
    rate-limited + challenge-response signed by enrolled Ed25519 key) to
    mint a scoped session token for an already-enrolled device!
11. **Backend Changes**: Add unauthenticated-session challenge-response
    device login (`POST /api/device-sessions/challenge` + `/verify`) in
    `routes.go` backed by `devices.Store` + rate limiter; pass
    `OnPairingPrompt` callback in `app.go` to surface toast/modal
    anywhere in desktop UI.
12. **Frontend Changes**: Global pairing approval modal (not buried only
    inside `TrustedDevicesView.svelte`), device status badge.
13. **Mobile Changes**: Add "Pair This Device" banner/button in
    `index.html`/`app.js`, SAS verification modal, IndexedDB key
    storage, and "Auto-reconnect to `<PC Name>`{=html}" flow when opened
    from bookmark without `#token`.
14. **Testing**: Cross-browser (`tweetnacl`/Ed25519) vector tests
    against Go `internal/devices`, brute-force lockout tests on
    `/api/device-sessions`, revocation tests.
15. **Dependencies**: Mobile PWA / Home Screen Shortcut (Gap 11) makes
    bookmark reconnection feel like a native app.

-   **Scores**: UV: 5, SA: 5, CA: 4, F: 4, RM: 3 → **Weighted Score:
    4.45 (P0)**

### Gap 3: Automatic Nearby Device Discovery (mDNS / UDP Multicast)

1.  **Feature Name**: Zero-Config LAN Discovery (mDNS / UDP Multicast)
2.  **Description**: Broadcast and discover running Flitnix desktop/CLI
    instances on the local subnet via mDNS (`_flitnix._tcp.local.`) and
    UDP multicast fallback, showing a live "Nearby Devices" radar/list
    with device name, OS icon, and trust badge.
3.  **Competitors**: LocalSend, AirDrop, Quick Share, KDE Connect,
    LANDrop, Warpinator.
4.  **User Problem**: Users with two laptops (or desktop + Linux
    handheld/Steam Deck) currently have to manually copy-paste a long
    `http://ip:43821/#token` URL into `flitnix send`.
5.  **Flitnix Status**: Missing.
6.  **Why It Improves Flitnix**: Transforms Flitnix from
    "desktop-to-phone only" into an effortless all-device LAN mesh
    comparable to LocalSend and AirDrop.
7.  **Expected User Impact**: Very High --- removes manual IP/URL
    copy-paste between computers and enables instant peer selection.
8.  **Technical Complexity**: Medium
9.  **Development Effort**: 2 weeks
10. **Security Implications**: Discovery packets must **never** carry
    session tokens or sensitive metadata---only device display name
    (sanitized via `SanitizeDeviceName`), OS type, port, protocol
    version, and public HostID fingerprint (`devices.HostIdentity`).
    Must support visibility modes (Off / Trusted Only / Everyone) and
    bind strictly to the active interface (`netenv`), not VPNs.
11. **Backend Changes**: New `internal/discovery` package (mDNS + UDP
    multicast announcer/listener bound to selected `netenv.View` IP),
    `ListNearbyDevices()` Wails method, `GET /api/nearby` or desktop
    client sender flow using `internal/client`.
12. **Frontend Changes**: Add "Nearby Flitnix Devices" strip/cards in
    `PersonalTransferView.svelte` with click-to-send and visibility
    pill.
13. **Mobile Changes**: None required (mobile browsers cannot listen on
    raw UDP/mDNS, so mobile continues via QR/6-digit code/bookmarks;
    optionally show friendly desktop name).
14. **Testing**: Unit tests with mock multicast transport, multi-homed
    VPN exclusion tests, discovery timeout/stale-peer eviction tests.
15. **Dependencies**: Desktop-to-Desktop GUI Sender (Gap 6), Device
    Visibility Controls (Gap 14).

-   **Scores**: UV: 5, SA: 5, CA: 4, F: 4, RM: 4 → **Weighted Score:
    4.55 (P0)**

### Gap 9: Desktop-to-Desktop Direct Transfer Workflow (GUI Client Mode)

1.  **Feature Name**: Desktop-to-Desktop Direct Transfer (Send Tab /
    Nearby Peer Picker)
2.  **Description**: Expose `internal/client` inside the Wails desktop
    GUI so a user on Desktop A can click a discovered Flitnix peer (or
    paste a 6-digit code / URL) and send files/folders directly from the
    GUI without dropping to the terminal `flitnix send`.
3.  **Competitors**: LocalSend, LANDrop, PairDrop, AirDrop, Quick Share,
    Blip, Send Anywhere.
4.  **User Problem**: Desktop-to-desktop transfers require CLI
    (`flitnix send`); non-technical users on two laptops have no GUI way
    to send directly to another Flitnix desktop.
5.  **Flitnix Status**: Partial (`internal/client` + `send_cli.go`
    exist, zero GUI integration).
6.  **Why It Improves Flitnix**: Unlocks the entire PC↔PC, Mac↔Windows,
    Linux↔Mac market segment inside the GUI.
7.  **Expected User Impact**: Very High --- doubles the addressable use
    cases beyond phone↔PC.
8.  **Technical Complexity**: Medium
9.  **Development Effort**: 1.5--2 weeks
10. **Security Implications**: Reuses `internal/client` strict `HostID`
    TOFU pinning, SAS verification, and SHA-256 pre-computed manifest
    digests; upgrade Desktop↔Desktop transport to TLS/mTLS (Gap 10).
11. **Backend Changes**: Wrap `internal/client` in an async transfer
    manager in `app.go` (`SendToPeer(target, paths)`), emitting progress
    events to Svelte.
12. **Frontend Changes**: Add "Send to Nearby Device / Enter Code"
    target selector in `PersonalTransferView.svelte` alongside the QR
    Phone mode.
13. **Mobile Changes**: None.
14. **Testing**: Integration tests between two `server.Server` +
    `app.go` instances.
15. **Dependencies**: Automatic LAN Discovery (Gap 3) and/or 6-Digit
    Code (Gap 12).

-   **Scores**: UV: 5, SA: 5, CA: 4, F: 4, RM: 4 → **Weighted Score:
    4.55 (P0)**

### Gap 27: Firewall Guidance, Auto-Diagnostic Ping & Mobile-Side Connection Helper

1.  **Feature Name**: Actionable OS Firewall Guidance & Interactive
    Connection Probe
2.  **Description**: Enhance `internal/doctor` to actively inspect OS
    firewall rules (Windows Defender Firewall `netsh advfirewall`, Linux
    `ufw`/`firewalld`, macOS `socketfilterfw`) and give copyable
    one-click commands/guidance, plus allow the phone QR landing page
    (if unreachable) to show troubleshooting tips.
3.  **Competitors**: LocalSend (inbound/outbound reachability
    indicators), KDE Connect community guides.
4.  **User Problem**: Connection Doctor checks local loopback
    `/healthz`, which passes even when Windows Defender Firewall or
    `firewalld` silently drops external packets on TCP port `43821`!
5.  **Flitnix Status**: Partial (`doctor.go` checks loopback `/healthz`
    and warns generically on Wi-Fi/multi-homed, but never checks OS
    firewall state for port 43821).
6.  **Why It Improves Flitnix**: Turns Connection Doctor from "good" to
    truly diagnostic for the #1 real-world failure mode (host firewall
    blocking LAN inbound TCP).
7.  **Expected User Impact**: High for first-time Windows and
    Fedora/Ubuntu users.
8.  **Technical Complexity**: Medium
9.  **Development Effort**: 1 week
10. **Security Implications**: Read-only firewall inspection only; never
    silently alter firewall rules without explicit user/OS UAC action
    (and NSIS installer can optionally register the Windows firewall
    rule at install time!).
11. **Backend Changes**: Add 8th check `firewall_status` in
    `internal/doctor/doctor.go` (query `firewall-cmd`/`ufw`/`netsh`
    read-only with 1.5s timeout); optionally register app rule in
    `build/windows/installer/project.nsi`.
12. **Frontend Changes**: Display exact fix command + "Copy Command"
    button in `ConnectionDoctorModal.svelte`.
13. **Mobile Changes**: None.
14. **Testing**: Mocked command-output parser tests per OS.
15. **Dependencies**: None.

-   **Scores**: UV: 4, SA: 5, CA: 4, F: 4, RM: 4 → **Weighted Score:
    4.25 (P0)**

------------------------------------------------------------------------

## Phase 3 --- Always-Ready Desktop

**Proposed release:** v0.3.0\
**Gap count:** 4\
**Priority mix:** P0: 3, P1: 1\
**Objective:** Add a real-time event channel, background/tray lifecycle,
native notifications and durable personal transfer history.

### Phase execution order

1.  G10 --- build the real-time event channel with authentication and a
    polling/reconciliation fallback.
2.  G5 --- implement the minimum tray/background lifecycle first:
    close-to-tray behavior, restore, quit and optional autostart.
3.  G8 --- add native OS notifications for incoming approval, pairing
    and completion events.
4.  G5 --- finish tray menu actions and settings integration after
    notification callbacks exist.
5.  G6 --- persist personal transfer history and expose safe
    Open/Reveal/Reshare/Clear actions.
6.  G26 --- complete the history-retention and cleanup integration now
    that G6's persistent store exists.

### Phase deliverables

-   SSE or WebSocket event delivery for desktop and mobile clients.
-   System tray/menu-bar operation and optional launch-on-login.
-   Background receiving while the main window is hidden.
-   Native notifications with safe, useful approval/open actions.
-   Persistent personal transfer history with filters and file actions.
-   History/log retention and storage visibility integration.

### Phase exit criteria

-   [ ] Hiding or closing the window according to settings does not
    interrupt active transfers.
-   [ ] Explicit Quit shuts down cleanly.
-   [ ] Background mode does not bypass approval or trusted-device
    policy.
-   [ ] Notifications are sanitized, contain no tokens and are not
    emitted once per file in a large batch.
-   [ ] Event connections authenticate safely; credentials never appear
    in URL query strings.
-   [ ] Mobile clients reconnect and reconcile state after background
    suspension or dropped event connections.
-   [ ] Personal history survives application restart and file actions
    enforce path-confinement checks.
-   [ ] History retention is bounded and user-configurable.

### Detailed gap specifications

### Gap 10: Real-Time WebSockets (or SSE) Replacing HTTP Polling

1.  **Feature Name**: WebSocket Real-Time Event Channel (Desktop &
    Mobile)
2.  **Description**: Replace the 500--2500 ms HTTP polling loops in
    `app.js`, `guest.js`, and `App.svelte` with a lightweight WebSocket
    (or SSE + Wails `EventsEmit`) push channel for instant approval
    prompts, batch state transitions, Quick Text delivery, and live
    download/upload progress.
3.  **Competitors**: PairDrop (WebSockets), Snapdrop (WebSockets),
    AirDroid.
4.  **User Problem**: Currently, pressing "Approve" on desktop takes up
    to 1.2s for the phone to notice (`POLL_MS = 1200`), Quick Text
    messages lag up to 2.5s, and 72,000+ redundant Wails/HTTP poll
    requests occur during idle sessions.
5.  **Flitnix Status**: Missing (HTTP polling only; `gorilla/websocket`
    is already in `go.mod` as an indirect dep!).
6.  **Why It Improves Flitnix**: Makes interactions feel instantaneous
    (AirDrop-like snappiness), cuts idle battery/CPU usage on phones,
    and surfaces incoming requests immediately.
7.  **Expected User Impact**: High --- perceptibly faster UI
    responsiveness and lower mobile battery drain.
8.  **Technical Complexity**: Medium
9.  **Development Effort**: 1.5 weeks
10. **Security Implications**: Must enforce strict Origin/Host checks on
    WebSocket upgrade (`securityMiddleware` + `requireSession`
    Bearer/subprotocol or initial ticket exchange --- never put the
    session token in the WS query string!), plus per-message size caps.
11. **Backend Changes**: Add `GET /api/events` (SSE or WebSocket via
    `gorilla/websocket` / `coder/websocket`) and `GET /guest-api/events`
    in `internal/server/`; emit Wails events from
    `Coordinator`/`Service` hooks in `app.go` instead of 800 ms polling.
12. **Frontend Changes**: Subscribe via `EventsOn` in `App.svelte` with
    slow-fallback reconciliation.
13. **Mobile Changes**: Connect `EventSource`/`WebSocket` in `app.js`
    and `guest.js`, keeping existing polling as automatic fallback if WS
    is blocked.
14. **Testing**: Reconnect/disconnect tests, Origin-spoofing WS
    rejection tests, Safari background tab wakeup reconciliation tests.
15. **Dependencies**: None.

-   **Scores**: UV: 4, SA: 5, CA: 3, F: 4, RM: 4 → **Weighted Score:
    4.05 (P1)**

### Gap 5: System Tray / Menu Bar, Background Receiving & Autostart

1.  **Feature Name**: System Tray Daemon Mode, Background Receiving & OS
    Autostart
2.  **Description**: Run Flitnix unobtrusively in the system tray /
    macOS menu bar with "Minimize/Close to Tray", optional "Start on
    Login" across Windows/macOS/Linux, and automatic background
    session/listener readiness for trusted devices.
3.  **Competitors**: LocalSend, KDE Connect, Blip, AirDroid, Quick
    Share, LANDrop, Send Anywhere.
4.  **User Problem**: Currently closing the Flitnix window terminates
    the server (`shutdown`), and Flitnix doesn't start on boot---making
    it unavailable when you casually want to beam a photo from your
    phone without walking over to launch the desktop app first.
5.  **Flitnix Status**: Missing (`main.go` closes on window exit;
    `AutoStartSession` only triggers after manual app launch).
6.  **Why It Improves Flitnix**: Turns Flitnix into an always-ready
    local drop target like AirDrop/LocalSend instead of an app you have
    to open on both ends every time.
7.  **Expected User Impact**: Very High --- essential for daily-driver
    adoption.
8.  **Technical Complexity**: Medium
9.  **Development Effort**: 2 weeks
10. **Security Implications**: When running in background, unsolicited
    connections must remain gated by explicit approval (or strictly
    limited to `AutoApprove` trusted devices + optional Visibility =
    Off/Trusted Only), and idle power must stay near zero.
11. **Backend Changes**: Integrate Wails v2 tray / `fyne.io/systray` (or
    `wails` HideWindowOnClose + native tray menu), add
    `internal/autostart` (Windows Registry `HKCU\...\Run`, macOS
    `LaunchAgent` plist, Linux `~/.config/autostart/flitnix.desktop`),
    and extend `settings.Settings` with `MinimizeToTray` and
    `LaunchOnLogin`.
12. **Frontend Changes**: Add "Run in System Tray" and "Launch at Login"
    toggles in `SettingsView.svelte` General tab.
13. **Mobile Changes**: None.
14. **Testing**: Windows/Linux/macOS lifecycle tests (hide/restore
    window without dropping active HTTP transfers; single-instance focus
    restoration via `onSecondInstanceLaunch`).
15. **Dependencies**: Native OS Notifications (Gap 8) so background
    approval requests surface immediately!

-   **Scores**: UV: 5, SA: 5, CA: 3, F: 4, RM: 3 → **Weighted Score:
    4.25 (P0)**

### Gap 8: Native OS Desktop Notifications & Interactive Approval Actions

1.  **Feature Name**: Native OS Desktop Notifications (with Quick
    Approve/Open)
2.  **Description**: Trigger native Windows Toast, macOS
    UserNotification, and Linux D-Bus `org.freedesktop.Notifications`
    alerts when a phone requests transfer/pairing approval or completes
    a transfer while Flitnix is unfocused or minimized in the tray.
3.  **Competitors**: LocalSend, KDE Connect, AirDrop, Quick Share,
    AirDroid, Send Anywhere.
4.  **User Problem**: Currently, if Flitnix is behind a browser or
    editor, the user has no idea their phone is stuck on *"Waiting for
    approval on the desktop..."* (`Toast.svelte` is invisible when
    minimized).
5.  **Flitnix Status**: Missing (`go.mod` actually has `go-toast/v2` and
    `godbus/dbus/v5` as indirect dependencies from Wails/power, yet
    neither is used for user notifications!).
6.  **Why It Improves Flitnix**: Essential companion to System Tray mode
    and drastically cuts approval wait times.
7.  **Expected User Impact**: High --- eliminates "why isn't my upload
    starting?" confusion.
8.  **Technical Complexity**: Low-Medium
9.  **Development Effort**: 1 week
10. **Security Implications**: Sanitize filenames/device names before OS
    notification rendering (already sanitized via
    `SanitizeName`/`SanitizeDeviceName`); optionally allow hiding
    filenames on lock screens; never include tokens.
11. **Backend Changes**: New `internal/notify` package using D-Bus
    (`godbus/dbus/v5` already in `go.mod`!) on Linux, `go-toast/v2`
    (already indirect in `go.mod`!) on Windows, and
    `osascript`/UNUserNotificationCenter on macOS; invoke on
    `OnBatchPending`, `OnPairingPrompt`, and completion.
12. **Frontend Changes**: Add "Desktop Notifications" toggle & "Quiet /
    Do Not Disturb" option in `SettingsView.svelte`.
13. **Mobile Changes**: Optional Web Notification API prompt (when
    permitted) when desktop shares a new file or approves a batch.
14. **Testing**: Interface-mocked unit tests + headless/display-safe
    no-op fallback tests.
15. **Dependencies**: Pairs with System Tray (Gap 5).

-   **Scores**: UV: 4, SA: 5, CA: 3, F: 5, RM: 4 → **Weighted Score:
    4.20 (P0)**

### Gap 6: Persistent Personal Transfer History & Actionable File Management

1.  **Feature Name**: Persistent Personal Transfer History & File
    Actions
2.  **Description**: Persist all Personal Transfer uploads, downloads,
    and batches in a bounded local store (`history.jsonl` or SQLite/WAL
    alongside `audit.jsonl`), with device attribution, duration/avg
    speed stats, search/date/device filters, and per-item actions:
    **Open File**, **Reveal Exact File in File Manager**, **Reshare to
    Phone**, and **Delete/Clear History**.
3.  **Competitors**: LocalSend (persistent history), AirDroid, Send
    Anywhere.
4.  **User Problem**: Right now `TransferHistoryView.svelte` loses every
    personal transfer on app restart, caps at 64 items, only opens the
    top-level folder (not the file), and cannot resend or clear records.
5.  **Flitnix Status**: Partial (`TransferHistoryView.svelte` exists
    with search/direction/status filters, but reads ephemeral in-memory
    registries).
6.  **Why It Improves Flitnix**: Makes the existing History tab
    genuinely useful as a durable ledger and quick-resend hub.
7.  **Expected User Impact**: High --- solves "where did that file I
    received yesterday go?"
8.  **Technical Complexity**: Low-Medium
9.  **Development Effort**: 1--1.5 weeks
10. **Security Implications**: Never persist raw session tokens or
    sensitive file contents; enforce `withinDir` checks before OS
    "Open/Reveal/Delete" calls; keep private `0600` file permissions;
    provide "Clear History" and auto-retention cap (e.g., 90 days /
    5,000 items).
11. **Backend Changes**: Extend `internal/audit` or add
    `internal/history` JSONL store recording completed/failed personal
    uploads, batches, and downloads with `DeviceName`, `DurationMs`,
    `AvgSpeedBps`, `PublishedRelPath`; expose `QueryTransferHistory`,
    `ClearTransferHistory`, `OpenHistoryFile`, `RevealHistoryFile`,
    `ReshareHistoryFile` in `app.go`.
12. **Frontend Changes**: Wire `TransferHistoryView.svelte` to
    persistent backend query, add device/date filters, summary stats bar
    (total sent/received), and per-row action buttons.
13. **Mobile Changes**: Optionally show session-local history summary.
14. **Testing**: Persistence across simulated restarts, path-confinement
    tests on Reveal/Delete, pagination/filtering tests.
15. **Dependencies**: None.

-   **Scores**: UV: 4, SA: 5, CA: 3, F: 5, RM: 5 → **Weighted Score:
    4.30 (P0)**

------------------------------------------------------------------------

## Phase 4 --- Secure & Advanced Sharing

**Proposed release:** v0.4.0\
**Gap count:** 5\
**Priority mix:** P0: 1, P1: 4\
**Objective:** Add encrypted peer transport, installable mobile sharing,
multi-recipient rooms, protected share sessions and opt-in clipboard
synchronization.

### Phase execution order

1.  G11 --- implement TLS 1.3/certificate pinning for desktop-to-desktop
    and CLI peers; design paired-browser E2EE/optional HTTPS without
    breaking ad-hoc QR guest UX.
2.  G12 --- add the installable mobile PWA and Web Share Target flow,
    using the secure-origin support required by browsers for full
    share-target functionality.
3.  G19 --- extend Guest Drop Box into explicitly configured
    upload-only, download-only and two-way rooms with per-guest
    isolation.
4.  G23 --- add optional PIN protection, expiration and download-count
    limits to share sessions.
5.  G17 --- add opt-in clipboard synchronization only for trusted
    devices, using the real-time event channel and explicit user
    controls.

### Phase deliverables

-   Pinned encrypted transport for desktop/CLI peers.
-   Optional browser-session encryption/HTTPS path with a clear security
    indicator.
-   Installable mobile PWA and supported OS share-target integration.
-   Multi-recipient local sharing rooms.
-   PIN-protected and download-limited share sessions.
-   Opt-in bidirectional clipboard synchronization.

### Phase exit criteria

-   [ ] Certificate mismatch and MITM simulation tests fail closed for
    pinned peers.
-   [ ] No silent downgrade from a required encrypted peer connection.
-   [ ] Ad-hoc browser QR flows do not unexpectedly become unusable
    because of certificate warnings.
-   [ ] Guest room modes enforce their intended upload/download
    permissions and preserve guest isolation.
-   [ ] PIN attempts are rate-limited and download limits are enforced
    atomically.
-   [ ] Clipboard sync is off until explicitly enabled, is limited to
    trusted devices and includes pause/disable controls.
-   [ ] PWA behavior is tested on supported browsers; unsupported
    secure-context capabilities are clearly explained.

### Detailed gap specifications

### Gap 11: Encrypted Transport for CLI/Desktop-to-Desktop (mTLS / E2EE) & Optional Local HTTPS

1.  **Feature Name**: Hybrid Encrypted Transport (Mandatory mTLS/E2EE
    for Peers + Optional Local HTTPS)
2.  **Description**: Implement TLS 1.3 with Ed25519-pinned self-signed
    certificates (`internal/devices`) for all Desktop↔Desktop and CLI
    (`flitnix send`/`pair`) traffic, plus an opt-in local HTTPS mode (or
    application-layer JS E2EE envelope via WebCrypto/tweetnacl for
    paired browsers) while preserving zero-friction plain HTTP as the
    default for ad-hoc guest QR scans.
3.  **Competitors**: LocalSend (TLS on native apps), KDE Connect (TLS +
    cert pinning), Blip (mTLS over TLS 1.3), PairDrop (WebRTC DTLS),
    Croc (PAKE E2EE).
4.  **User Problem**: On shared university, café, or office Wi-Fi,
    plaintext HTTP exposes file contents, filenames, Quick Text notes,
    and guest cookies to anyone sniffing the wireless segment.
5.  **Flitnix Status**: Missing
    (`docs/TRANSPORT_ENCRYPTION_EVALUATION.md` specifically proposed
    mTLS/E2EE for app-to-app/desktop-to-desktop in Section 4.2,
    unimplemented).
6.  **Why It Improves Flitnix**: Closes Flitnix's largest security gap
    against LocalSend/KDE Connect/Blip without breaking zero-install QR
    scans.
7.  **Expected User Impact**: Medium-High (High for
    security/enterprise/power users; transparent to normal users).
8.  **Technical Complexity**: High
9.  **Development Effort**: 2--3 weeks
10. **Security Implications**: Prevents passive eavesdropping and active
    MITM tampering on LANs. Must carefully avoid browser cert
    interstitials for ad-hoc QR guests (keep HTTP fallback or explore
    WebCrypto/pure-JS authenticated encryption over HTTP for paired
    browsers, noting that JS served over plain HTTP still relies on
    initial QR trust).
11. **Backend Changes**: TLS listener / dual-protocol ALPN or dedicated
    TLS port (e.g. `43822`) in `internal/server/`, cert generation tied
    to `HostIdentity` Ed25519 key (`internal/devices`),
    `internal/client` certificate pinning verification.
12. **Frontend Changes**: Display encryption badge (`🔒 TLS Pinned` vs
    `LAN HTTP`) in Connection Hub & Settings toggle for "Require
    Encrypted Transport for Desktop/CLI".
13. **Mobile Changes**: Optional E2EE payload encryption for paired
    mobile browsers + clear security status indicator.
14. **Testing**: MITM cert-mismatch rejection tests, TLS 1.3 throughput
    benchmarks (note `benchmarks/tls_eval` already exists!).
15. **Dependencies**: Mobile Trusted Pairing (Gap 4) /
    Desktop-to-Desktop (Gap 9).

-   **Scores**: UV: 4, SA: 5, CA: 4, F: 3, RM: 3 → **Weighted Score:
    4.00 (P1)**

### Gap 12: Installable Mobile PWA with Web Share Target API

1.  **Feature Name**: Mobile Web App (PWA) & Android/OS Share Sheet
    Integration
2.  **Description**: Add a `manifest.webmanifest`, service worker (shell
    caching + offline recovery page), and `share_target` POST handler in
    `internal/server/assets/` so mobile users can "Install Flitnix" to
    their home screen and share photos/files/URLs directly from
    Gallery/Files/Chrome into Flitnix.
3.  **Competitors**: PairDrop (PWA + Web Share Target),
    LocalSend/AirDrop/Quick Share (native Share Sheet).
4.  **User Problem**: Currently, sending a photo from Android Gallery
    requires opening the browser, tapping "Pick files", navigating
    through Android's document picker, and hunting for the photo.
5.  **Flitnix Status**: Missing (no manifest, no service worker, no
    `share_target`).
6.  **Why It Improves Flitnix**: Delivers native-app Share Sheet
    ergonomics **without** app store downloads or maintaining
    Swift/Kotlin codebases!
7.  **Expected User Impact**: Very High (on Android Chrome/Edge; partial
    on iOS Safari Home Screen Web App).
8.  **Technical Complexity**: Medium
9.  **Development Effort**: 1--1.5 weeks
10. **Security Implications**: **Important browser constraint**:
    `ServiceWorker` and `share_target` generally require a **secure
    context** (HTTPS) on non-localhost IPs in Chrome/Safari! Thus PWA
    Share Target works best paired with optional local CA/trusted origin
    or `.local` mDNS + cert, or on desktop `localhost`/Android Chrome
    flags; however, basic Home Screen bookmark + app icon
    (`apple-touch-icon.png` already exists in `assets/`!) works
    immediately over HTTP, and full `share_target` activates when
    HTTPS/local origin is enabled.
11. **Backend Changes**: Serve `/manifest.webmanifest` and `/sw.js` with
    proper `Service-Worker-Allowed` and CSP headers (`routes.go`).
12. **Frontend Changes**: None.
13. **Mobile Changes**: Create `manifest.webmanifest`, `sw.js`, handle
    shared `FormData` files/text/URLs and route directly into
    `addFiles()` / Quick Text.
14. **Testing**: Lighthouse PWA validation + Android Chrome Share Target
    test.
15. **Dependencies**: Mobile Trusted Pairing (Gap 4) so launching the
    PWA reconnects without re-scanning QR; Local HTTPS (Gap 11) for full
    `share_target` support on non-localhost IPs.

-   **Scores**: UV: 4, SA: 5, CA: 4, F: 3, RM: 4 → **Weighted Score:
    4.10 (P1)**

### Gap 19: Multi-Recipient Desktop-to-Many Broadcasting (Local Share Room)

1.  **Feature Name**: Desktop-to-Multiple-Phones Sharing Room
    (One-to-Many Broadcast)
2.  **Description**: Extend `SharedSource` / session sharing so a
    teacher, presenter, or host can display one QR code (similar to
    Guest Drop Box, but **download-only** or **two-way** room) allowing
    10--100 attendees on local Wi-Fi to download the shared slides,
    PDFs, or event photos concurrently.
3.  **Competitors**: PairDrop (Public Rooms), Send Anywhere (Link
    sharing), Quick Share (QR link).
4.  **User Problem**: Guest Drop Box is strictly upload-only (`guest.go`
    isolates guests from desktop files); Personal Transfer is meant for
    1-to-1 pairing and limits `maxGrantsPerSession` / `active` download
    per session.
5.  **Flitnix Status**: Partial (Guest Drop Box exists for *receiving*
    from a crowd; Personal session only supports 1 active archive
    download per session key in `grants.go:209`).
6.  **Why It Improves Flitnix**: Completes the classroom/conference
    story: collect assignments *and* distribute handouts/media locally
    with zero internet.
7.  **Expected User Impact**: High for classrooms, workshops, and events
    (Flitnix's signature differentiator!).
8.  **Technical Complexity**: Medium
9.  **Development Effort**: 1.5 weeks
10. **Security Implications**: Read-only access strictly scoped to
    explicitly shared sources; optional room PIN/passcode or
    host-approve-join gate; download rate/concurrency cap to avoid Wi-Fi
    saturation.
11. **Backend Changes**: Add `RoomMode` (`upload_only`, `download_only`,
    `two_way`) to `internal/guest/room.go`, allow `guest-api` read-only
    `/guest-api/files` + download grants when enabled, lift
    single-session archive lock across distinct guest IDs.
12. **Frontend Changes**: Mode selector in `GuestDropBoxView.svelte`
    Step 1 + Shared Files picker when `download_only`/`two_way` is
    selected.
13. **Mobile Changes**: Conditionally show "Shared by Host" download
    section in `guest.html`/`guest.js`.
14. **Testing**: Cross-mode isolation tests (verify `upload_only` rooms
    still return 403/404 on `/guest-api/files`).
15. **Dependencies**: Builds cleanly on `internal/guest`.

-   **Scores**: UV: 4, SA: 5, CA: 5, F: 4, RM: 4 → **Weighted Score:
    4.45 (P0)**

### Gap 23: Password-Protected & One-Time / Download-Limited Share Links

1.  **Feature Name**: Passcode-Protected & Download-Count-Limited Share
    Sessions
2.  **Description**: Allow desktop users sharing files to optionally
    attach a room/session PIN/password, custom link expiration, and a
    max-download count (including "1-time self-destruct share") for
    local LAN links or Guest rooms.
3.  **Competitors**: Send Anywhere (paid password/expiry), Quick Share
    Private Sharing, LocalSend (PIN).
4.  **User Problem**: On a shared office/classroom Wi-Fi, someone who
    glimpses the screen URL or receives a forwarded link could download
    files multiple times or after the intended recipient finishes.
5.  **Flitnix Status**: Partial (Guest rooms have duration expiry, no
    passcode or download-count limit; Personal sessions have 10-min idle
    expiry, no PIN or download-count cap).
6.  **Why It Improves Flitnix**: Adds Send Anywhere Pro-grade link
    controls locally for free.
7.  **Expected User Impact**: Medium.
8.  **Technical Complexity**: Low-Medium
9.  **Development Effort**: 1 week
10. **Security Implications**: Rate-limit passcode attempts
    (`PairingCoordinator`-style lockout); hash passcode in memory with
    `subtle.ConstantTimeCompare`.
11. **Backend Changes**: Optional `PIN` and `MaxDownloads` fields on
    `session` / `DownloadGrantStore` / `guest.Room`.
12. **Frontend Changes**: Optional "Require PIN" & "Expire after N
    downloads" checkboxes in Share/Guest wizard.
13. **Mobile Changes**: Simple PIN prompt card in `app.js` / `guest.js`
    before showing shared files.
14. **Testing**: Download counter exhaustion tests, PIN brute-force
    lockout tests.
15. **Dependencies**: None.

-   **Scores**: UV: 3, SA: 4, CA: 3, F: 5, RM: 4 → **Weighted Score:
    3.65 (P1)**

### Gap 17: Opt-In Automatic Clipboard Synchronization

1.  **Feature Name**: Opt-In Bidirectional Clipboard Sync
2.  **Description**: Add an explicit opt-in toggle in desktop & paired
    mobile sessions to automatically push newly copied text/URLs (up to
    64 KiB) across paired devices, with sensitive-content exclusions
    (e.g., ignore password manager concealed clipboard hints on
    Windows/macOS/Linux where available, pause button, and clear
    notification).
3.  **Competitors**: KDE Connect, AirDrop/Apple Universal Clipboard,
    AirDroid.
4.  **User Problem**: Users copy 2FA codes, URLs, or terminal commands
    constantly between PC and phone; currently they must switch to Quick
    Text, paste, and hit Send every time.
5.  **Flitnix Status**: Partial (`internal/textmsg` + Quick Text UI
    exist, manual send only).
6.  **Why It Improves Flitnix**: Elevates Quick Text from a simple chat
    box to effortless KDE Connect-level productivity.
7.  **Expected User Impact**: High --- beloved daily workflow feature
    for developers and power users.
8.  **Technical Complexity**: Medium
9.  **Development Effort**: 1.5 weeks
10. **Security Implications**: **High sensitivity**: Must be **strictly
    opt-in** (off by default), disabled in Guest Drop Box rooms,
    automatically skip empty/oversized/unchanged loops (echo suppression
    via hash), respect OS concealed/password clipboard markers
    (`org.nspasteboard.ConcealedType`,
    `ExcludeClipboardContentFromMonitorProcessing`), and ideally require
    encrypted transport or paired trust.
11. **Backend Changes**: Add OS clipboard watcher
    (`golang.design/x/clipboard` or native OS hooks) gated by
    `Settings.AutoClipboardSync` + echo-hash dedup; push via
    `textmsg.Store`.
12. **Frontend Changes**: Prominent opt-in toggle + "Pause Sync"
    indicator in Quick Text header & `SettingsView.svelte`.
13. **Mobile Changes**: Browser `navigator.clipboard.readText()` only
    works on explicit focus/user gesture due to browser
    security---implement "Tap to Sync Clipboard" / auto-read-on-focus
    when user grants permission, plus one-tap auto-copy button on
    incoming desktop clips.
14. **Testing**: Echo-loop prevention tests, concealed-type filtering
    tests, 64 KiB boundary tests.
15. **Dependencies**: WebSockets (Gap 10) for instant delivery; Trusted
    Devices (Gap 4).

-   **Scores**: UV: 4, SA: 4, CA: 4, F: 3, RM: 3 → **Weighted Score:
    3.75 (P1)**

------------------------------------------------------------------------

## Phase 5 --- Productivity & User Experience

**Proposed release:** v0.4.1\
**Gap count:** 7\
**Priority mix:** P1: 5, P2: 2\
**Objective:** Improve file recognition, duplicate handling, queue
control, destination organization, mobile usability, bandwidth control
and OS-level sharing.

### Phase execution order

1.  G14 --- add safe media thumbnails and previews with strict
    content-type handling.
2.  G18 --- add duplicate detection and explicit conflict policies,
    keeping Keep Both as the safe default.
3.  G16 --- add bounded small-file concurrency and queue
    pause/reorder/prioritize controls.
4.  G25 --- add optional destination organization by device, type or
    date.
5.  G29 --- bring mobile theme, localization and file filtering/search
    closer to desktop parity.
6.  G21 --- add optional bandwidth caps and scheduled transfers.
7.  G24 --- improve macOS Finder and Windows multi-select share
    integration.

### Phase deliverables

-   Sanitized image/video thumbnails and lightweight previews.
-   Skip-identical and user-configurable file conflict handling.
-   Queue controls and bounded parallel transfer streams.
-   Optional folder organization and destination profiles.
-   Mobile light/dark themes, translations and file filters.
-   Bandwidth limits and scheduling.
-   Reliable multi-file context-menu/share-sheet integration.

### Phase exit criteria

-   [ ] Untrusted SVG/HTML is never rendered as active preview content.
-   [ ] Overwrite is opt-in and cannot replace symlinks/directories or
    escape the receive root.
-   [ ] Concurrency remains bounded and coordinator race invariants pass
    under the race detector.
-   [ ] Device/type/date destination names are sanitized and
    path-confined.
-   [ ] Mobile theme/localization/filter changes pass accessibility and
    contrast checks.
-   [ ] Bandwidth limits do not trigger false stall timeouts.
-   [ ] Windows/macOS multi-select invocations are coalesced into one
    safe transfer batch.

### Detailed gap specifications

### Gap 14: Media Thumbnails, File Previews & Gallery Selector

1.  **Feature Name**: Media Thumbnails & In-App File Previews (Desktop &
    Mobile)
2.  **Description**: Render image/video thumbnails and lightweight
    in-app previews (images, video, audio, PDF/text) in the desktop
    Shared list, Approval Inbox, Transfer History, and mobile
    `Files on the desktop` list.
3.  **Competitors**: AirDrop, Quick Share, AirDroid (Gallery module),
    PairDrop (image preview on incoming prompt).
4.  **User Problem**: Seeing `IMG_20261124_184211.jpg` gives zero visual
    confirmation of *which* photo is being approved or downloaded.
5.  **Flitnix Status**: Missing (displays text filenames and generic
    extension chips like `JPG`, `MP4` only).
6.  **Why It Improves Flitnix**: Dramatic UX polish---users immediately
    recognize photos/videos before approving or downloading.
7.  **Expected User Impact**: High --- especially for photo/video
    workflows, Flitnix's primary mobile use case.
8.  **Technical Complexity**: Medium
9.  **Development Effort**: 2 weeks
10. **Security Implications**: **Never** generate thumbnails via unsafe
    native C parsers without sandboxing, and **never** render untrusted
    HTML/SVG inline! On mobile uploads, generate small JPEG/WebP preview
    thumbnails client-side on `<canvas>` (or `createImageBitmap`) and
    upload alongside metadata (capped at e.g. 32 KiB) only after
    validation; on desktop Shared files, serve sanitized thumbnails via
    `/api/files/{id}/thumb` with `X-Content-Type-Options: nosniff` and
    `Content-Disposition` / strict image types (`image/jpeg`,
    `image/png`, `image/webp`)---excluding SVG to prevent XSS.
11. **Backend Changes**: Add `/api/files/{id}/thumb` and
    `/api/files/{id}/preview` (range-aware) with bounded thumbnail cache
    (`sync`/LRU) and Go `image` resizing; optionally accept small
    preview thumbnail in upload/batch declaration.
12. **Frontend Changes**: Render thumbnail grid/list toggle in
    `PersonalTransferView.svelte` & `TransferHistoryView.svelte` +
    lightbox modal.
13. **Mobile Changes**: Render file thumbnails in `app.js` upload queue
    (via `URL.createObjectURL` revoked on completion) and desktop shared
    gallery cards.
14. **Testing**: Corrupt/malformed image fuzz tests, SVG/XSS rejection
    tests, memory-leak tests for object URLs.
15. **Dependencies**: None.

-   **Scores**: UV: 4, SA: 4, CA: 4, F: 4, RM: 4 → **Weighted Score:
    4.00 (P1)**

### Gap 18: Duplicate Detection & Configurable File Conflict Policy

1.  **Feature Name**: Duplicate File Detection & Conflict Resolution
    Policy
2.  **Description**: Allow users to choose how incoming Personal
    Transfer filename collisions are handled (**Keep Both** \[current
    default\], **Skip Identical \[size + SHA-256 match\]**,
    **Overwrite**, or **Ask**), and optionally skip re-transferring
    identical files in multi-hundred-file folder uploads.
3.  **Competitors**: Syncthing, Blip, standard OS file managers.
4.  **User Problem**: Re-sending an updated folder of 500 photos
    currently creates 500 duplicate `photo (1).jpg` files on disk.
5.  **Flitnix Status**: Partial (Guest Drop Box supports `preserve_both`
    vs `reject`; Personal Transfer hardcodes `variantName` keep-both in
    `store.go:485`).
6.  **Why It Improves Flitnix**: Prevents storage bloat and makes
    repeated folder transfers smart and clean.
7.  **Expected User Impact**: Medium-High.
8.  **Technical Complexity**: Medium
9.  **Development Effort**: 1 week
10. **Security Implications**: Overwrite mode must be **opt-in** (never
    default) and strictly confined via `withinDir` + regular-file check
    (never overwrite symlinks or directories) to preserve Flitnix's
    "never silently clobber files" guarantee.
11. **Backend Changes**: Add `ConflictPolicy` (`keep_both`,
    `skip_identical`, `overwrite`) to `settings.go` / batch declaration;
    pre-flight manifest check compares existing destination file size +
    SHA-256 against declared SHA-256 before streaming bytes!
12. **Frontend Changes**: Conflict policy selector in
    `SettingsView.svelte` Storage tab & per-batch approval card ("12
    files already exist identically --- Skip duplicates?").
13. **Mobile Changes**: Compute SHA-256 in worker (or quick partial/full
    hash) when `skip_identical` is enabled; show
    `"Skipped (Already on desktop)"` badge.
14. **Testing**: Collision, symlink refusal, and hash-mismatch
    overwrite/keep-both tests.
15. **Dependencies**: None.

-   **Scores**: UV: 4, SA: 4, CA: 3, F: 4, RM: 4 → **Weighted Score:
    3.80 (P1)**

### Gap 16: Adaptive Transfer Concurrency & Queue Management (Reorder / Pause / Prioritize)

1.  **Feature Name**: Configurable Parallel Transfers (Small Files) &
    Queue Controls
2.  **Description**: Allow configurable concurrency (e.g., 1--4 streams,
    auto-pipelining small files `< 10 MB` concurrently while keeping
    multi-GB files sequential to avoid disk thrashing) plus queue
    pause/resume, drag-to-reorder, and prioritize-next buttons.
3.  **Competitors**: LocalSend, Send Anywhere, Blip, AirDroid.
4.  **User Problem**: Sending 1,000 tiny 50 KB photos sequentially over
    HTTP/1.1 wastes RTT latency per item (`claim` + `PUT` + `fsync` +
    `poll` round-trips), making bulk photo dumps much slower than raw
    bandwidth allows.
5.  **Flitnix Status**: Partial (strictly 1 active stream per session in
    `coordinator.go`, no pause/reorder).
6.  **Why It Improves Flitnix**: Dramatically speeds up
    multi-hundred-file photo/project transfers while preserving bounded
    memory (`sync.Pool` capped at `MaxConcurrentStreams * 256 KiB`).
7.  **Expected User Impact**: High --- major speed boost for folders and
    photo rolls.
8.  **Technical Complexity**: Medium-High
9.  **Development Effort**: 2 weeks
10. **Security Implications**: Must keep strict `MaxActive` /
    `QuotaManager` memory & disk bounds (e.g., max 4 concurrent streams)
    so parallel streams cannot OOM low-RAM devices.
11. **Backend Changes**: Generalize `coordinatorSession.active` from
    single `*activeLease` to bounded `active map[string]*activeLease`
    (default limit 1 for huge files, up to N=3--4 configurable), add
    `ReorderBatchItems` / `PauseSession` / `ResumeSession`.
12. **Frontend Changes**: Concurrency slider (`1`--`4`, default `Auto`)
    in `SettingsView.svelte` + Pause/Prioritize controls in
    `PersonalTransferView.svelte`.
13. **Mobile Changes**: Parallel Web Worker pool (up to 3 workers) in
    `upload-client.js` + Pause/Prioritize buttons in `app.js` queue UI.
14. **Testing**: Race-detector stress tests (`go test -race`) for
    concurrent `CompleteItem` / `CancelItem` / `ReserveCancel`
    invariants!
15. **Dependencies**: Careful preservation of `Coordinator` race
    guarantees.

-   **Scores**: UV: 4, SA: 4, CA: 3, F: 3, RM: 3 → **Weighted Score:
    3.55 (P1)**

### Gap 25: Automatic Media Organization & Multiple Transfer Destinations

1.  **Feature Name**: Smart Subfolder Sorting & Per-Device / Per-Type
    Destinations
2.  **Description**: Optional rules to automatically organize received
    files into subfolders by date (`YYYY-MM`), file type (`Photos/`,
    `Videos/`, `Documents/`), or sender device name (`Pixel-9/`), plus
    quick-switch destination profiles.
3.  **Competitors**: AirDroid, Syncthing, custom LocalSend requests.
4.  **User Problem**: Dumping 1,000 mixed PDFs, memes, and 4K videos
    into one flat `Downloads/Flitnix/` directory creates instant
    clutter.
5.  **Flitnix Status**: Missing (all personal uploads go flat into
    `store.Dir()`, except folders preserve internal tree).
6.  **Why It Improves Flitnix**: Zero-effort organization for heavy
    users while keeping default flat behavior intact.
7.  **Expected User Impact**: Medium.
8.  **Technical Complexity**: Low
9.  **Development Effort**: 4--5 days
10. **Security Implications**: Subfolder names (`DeviceName` / date /
    category) must pass `SanitizeName` and `withinDir` checks.
11. **Backend Changes**: Add `OrganizationRule` (`flat`, `by_device`,
    `by_type`, `by_date`) in `settings.go` and apply subpath routing in
    `store.OpenPart`.
12. **Frontend Changes**: Dropdown in `SettingsView.svelte` Storage tab.
13. **Mobile Changes**: None.
14. **Testing**: Path confinement tests for sanitized device subfolders.
15. **Dependencies**: None.

-   **Scores**: UV: 3, SA: 4, CA: 3, F: 5, RM: 5 → **Weighted Score:
    3.75 (P1)**

### Gap 29: Mobile Web UI Theme Parity, Localization & Multi-File Gallery Filter

1.  **Feature Name**: Mobile Web Light/Dark Theme, Localization &
    File-Type Filtering
2.  **Description**: Bring desktop's theme parity
    (`light`/`dark`/`system`), i18n catalog (`en.json` + community
    locales), and search/filter chips (`Images`, `Videos`, `Docs`) to
    the mobile web interfaces (`index.html`/`app.js` and
    `guest.html`/`guest.js`).
3.  **Competitors**: LocalSend (localized + themed across all
    platforms), PairDrop (localized + light/dark).
4.  **User Problem**: Desktop supports Light/Dark and i18n, but the
    mobile page is locked to dark mode and hardcoded English, and
    scrolling 100 shared desktop files on a phone has no search/filter.
5.  **Flitnix Status**: Missing on mobile (`index.html`/`guest.html`
    hardcode dark theme and English).
6.  **Why It Improves Flitnix**: Cohesive cross-device polish and
    usability for large shared file lists.
7.  **Expected User Impact**: Medium.
8.  **Technical Complexity**: Low
9.  **Development Effort**: 1 week
10. **Security Implications**: None.
11. **Backend Changes**: Serve locale dictionary or pass active
    theme/locale hint via `/api/session-info`.
12. **Frontend Changes**: Reuse/sync `frontend/src/locales/` strings.
13. **Mobile Changes**: Add CSS light-mode variables
    (`prefers-color-scheme`), search bar + filter pills in
    `Files on the desktop`.
14. **Testing**: Mobile asset syntax/unit tests + contrast checks.
15. **Dependencies**: None.

-   **Scores**: UV: 3, SA: 4, CA: 2, F: 5, RM: 5 → **Weighted Score:
    3.55 (P1)**

### Gap 21: Bandwidth Limiting & Transfer Scheduling

1.  **Feature Name**: Configurable Bandwidth Cap & Scheduled/Off-Peak
    Transfers
2.  **Description**: Allow capping upload/download throughput (e.g., 5
    MB/s, 20 MB/s, Unlimited) via token-bucket rate limiting and
    scheduling large queued desktop-to-desktop transfers for a specified
    time.
3.  **Competitors**: Syncthing (rate limits), enterprise transfer tools.
4.  **User Problem**: A 50 GB transfer saturating a shared home/office
    Wi-Fi router can starve video calls or gaming for everyone else on
    the network.
5.  **Flitnix Status**: Missing (`phase4_network_simulation_test.go` has
    a test-only token-bucket shaper, not production).
6.  **Why It Improves Flitnix**: Gives power users polite bandwidth
    control on shared networks.
7.  **Expected User Impact**: Low-Medium (niche but valued by power
    users).
8.  **Technical Complexity**: Low-Medium
9.  **Development Effort**: 1 week
10. **Security Implications**: Minimal; avoid division-by-zero or
    stall-watchdog false timeouts when bandwidth cap is set very low
    (adjust `stallBodyReader` timeout dynamically based on rate limit!).
11. **Backend Changes**: Wrap `streamBody` and download writers with an
    optional `rate.Limiter` (from `golang.org/x/time/rate`) configured
    via `settings.Settings`.
12. **Frontend Changes**: Speed limit dropdown/input in
    `SettingsView.svelte` Network tab.
13. **Mobile Changes**: Respect server-enforced backpressure naturally
    via TCP window.
14. **Testing**: Rate accuracy tests + stall-timeout interaction tests
    at 512 KB/s.
15. **Dependencies**: None.

-   **Scores**: UV: 2, SA: 4, CA: 2, F: 5, RM: 4 → **Weighted Score:
    3.15 (P2)**

### Gap 24: Deep OS Share Sheet & File Manager Integration (macOS & Windows)

1.  **Feature Name**: macOS Finder Quick Action / Share Extension &
    Multi-Select Windows Context Menu
2.  **Description**: Add a macOS Finder Quick Action / Share Extension
    (and `.pkg`/`.dmg` helper) plus Windows 11 modern context menu /
    SendTo multi-file batching so right-clicking 20 files in Finder or
    Explorer opens them in one Flitnix share batch (not 20 separate
    invocations).
3.  **Competitors**: AirDrop, LocalSend (`v1.15.0` Windows context
    menu), Quick Share, KDE Connect, Blip.
4.  **User Problem**: Linux has Nautilus & Dolphin scripts, and Windows
    NSIS adds basic `*`/`Directory` registry entries, **but macOS has
    zero Finder context menu/Share Sheet**, and Windows selecting 10
    files spawns 10 parallel `flitnix.exe "%1"` processes racing
    `SingleInstanceLock`.
5.  **Flitnix Status**: Partial (`build/linux/nautilus`, `servicemenus`,
    `project.nsi`, `Mac.OnFileOpen`).
6.  **Why It Improves Flitnix**: Makes right-click sharing genuinely
    seamless on macOS and reliable for multi-file selections on Windows.
7.  **Expected User Impact**: Medium-High on Windows/macOS.
8.  **Technical Complexity**: Medium-High (macOS Share Extension
    requires Swift appex bundle & App Group IPC; Windows 11 top-level
    menu requires sparse package or SendTo shortcut).
9.  **Development Effort**: 2--3 weeks
10. **Security Implications**: Validate all OS-delivered paths through
    `ExtractSharePaths`; avoid spawning shell commands with unquoted
    paths.
11. **Backend Changes**: Coalesce rapid `onSecondInstanceLaunch` calls
    within a 300 ms debounce window into a single notification; add
    macOS Automator/Quick Action or Swift Share Extension target in
    `build/darwin/`.
12. **Frontend Changes**: Toast `"Added 12 files from Explorer/Finder"`.
13. **Mobile Changes**: Covered by PWA Share Target (Gap 12).
14. **Testing**: Multi-file `onSecondInstanceLaunch` batching tests;
    manual macOS/Windows packaging tests.
15. **Dependencies**: None.

-   **Scores**: UV: 3, SA: 4, CA: 3, F: 3, RM: 3 → **Weighted Score:
    3.25 (P2)**

------------------------------------------------------------------------

## Phase 6 --- Advanced & Optional Features

**Proposed release:** v0.5.0 and beyond\
**Gap count:** 4\
**Priority mix:** P2: 1, P3: 3\
**Objective:** Offer optional remote connectivity, incremental folder
sync, consent-based media optimization and compact desktop convenience
features.

### Phase execution order

1.  G20 --- add the opt-in WebRTC/self-hosted relay mode only after the
    encrypted peer transport foundation is complete.
2.  G30 --- add watch folders and incremental synchronization between
    mutually trusted desktop devices.
3.  G28 --- add explicit, consent-based image/media optimization while
    preserving originals by default.
4.  G32 --- add compact/floating drop-zone mode and a user-triggered
    device ping/ring.

### Phase deliverables

-   Optional WebRTC and self-hosted relay connectivity.
-   Watch-folder and incremental sync for trusted desktop peers.
-   Original-quality default with optional image/video optimization.
-   Compact desktop drop mode and user-controlled device ping.

### Phase exit criteria

-   [ ] Remote mode is disabled by default, clearly disclosed and uses
    end-to-end encrypted payloads.
-   [ ] No third-party relay is silently selected as a default.
-   [ ] Folder sync is limited to explicitly selected roots and trusted
    peers.
-   [ ] Remote deletion is not silently enabled; safe append/update
    behavior is the default.
-   [ ] Media conversion requires explicit consent and clearly explains
    metadata/quality changes.
-   [ ] Floating mode and ping are optional, rate-limited and do not
    create unnecessary idle activity.

### Detailed gap specifications

### Gap 20: Optional Self-Hosted Relay / WebRTC Internet Mode

1.  **Feature Name**: Opt-In Peer-to-Peer WebRTC / Self-Hosted Relay
    Mode
2.  **Description**: Add an explicitly disabled-by-default "Remote /
    Restricted Wi-Fi Bridge" using WebRTC DataChannels + user-configured
    (or Flitnix-optional) STUN/TURN/signaling relay (`coturn`/custom
    lightweight Go signaling binary), enabling transfers when
    hotel/campus Wi-Fi enforces AP Client Isolation or devices are on
    different networks.
3.  **Competitors**: PairDrop (WebRTC + TURN), Send Anywhere, Blip,
    Croc.
4.  **User Problem**: Connection Doctor detects Wi-Fi Client Isolation
    and advises switching to a phone hotspot---but in some
    offices/hotels, users cannot use hotspots and local HTTP is blocked
    by AP isolation.
5.  **Flitnix Status**: Missing.
6.  **Why It Improves Flitnix**: Solves the hardest LAN blocker (AP
    isolation) and enables optional remote sharing *without* mandatory
    cloud storage or accounts.
7.  **Expected User Impact**: Medium-High (critical when AP isolation
    blocks local HTTP).
8.  **Technical Complexity**: Very High
9.  **Development Effort**: 4--6 weeks
10. **Security Implications**: **High**: Must remain **explicitly
    opt-in** with clear warning that signaling/TURN metadata touches a
    relay; all payloads must be E2EE (WebRTC DTLS + SAS verification);
    never default to third-party cloud relays silently.
11. **Backend Changes**: Integrate `pion/webrtc` in a new
    `internal/remote` package + tiny self-hostable signaling/relay
    server binary under `cmd/flitnix-relay`.
12. **Frontend Changes**: Opt-in "Enable Remote / Isolated-Wi-Fi Relay"
    toggle in `SettingsView.svelte` + Connection Doctor quick action
    when AP isolation fails.
13. **Mobile Changes**: WebRTC fallback client when `/ping` fails due to
    AP isolation.
14. **Testing**: NAT traversal, DTLS verification, fallback switching
    tests.
15. **Dependencies**: Encrypted Transport (Gap 11).

-   **Scores**: UV: 4, SA: 3, CA: 4, F: 2, RM: 2 → **Weighted Score:
    3.25 (P2)**

### Gap 30: Watch Folders & One-Way / Two-Way Incremental Folder Sync

1.  **Feature Name**: Optional Watch Folder / Incremental Sync Mode
2.  **Description**: Designate a folder on Desktop A (or CLI) that
    watches for new/modified files and automatically syncs deltas
    (comparing size + SHA-256) to a paired trusted Desktop B.
3.  **Competitors**: Syncthing, Google Quick Share (auto photo backup to
    PC).
4.  **User Problem**: Photographers or developers working across a
    desktop and laptop repeatedly manually re-share the same working
    folder.
5.  **Flitnix Status**: Missing.
6.  **Why It Improves Flitnix**: Bridges ad-hoc sharing and lightweight
    local folder sync for paired devices.
7.  **Expected User Impact**: Medium (power users).
8.  **Technical Complexity**: High
9.  **Development Effort**: 3--4 weeks
10. **Security Implications**: Must only run between mutually paired
    `AutoApprove` trusted devices with strict root confinement and
    deletion-safety (append/update only by default, never silent remote
    delete).
11. **Backend Changes**: New `internal/sync` package using `fsnotify`,
    manifest diffing endpoint `POST /api/sync/diff`.
12. **Frontend Changes**: "Watch Folders" panel under Settings or
    Devices.
13. **Mobile Changes**: Out of scope (browsers cannot watch OS folders
    in background).
14. **Testing**: Loop-prevention tests, partial-write debounce tests.
15. **Dependencies**: Desktop-to-Desktop Client (Gap 9), Trusted Devices
    (Gap 4), Duplicate Detection (Gap 18).

-   **Scores**: UV: 3, SA: 3, CA: 3, F: 2, RM: 2 → **Weighted Score:
    2.75 (P3)**

### Gap 28: Consent-Based Media Compression & Format Compatibility (HEIC/4K Toggle)

1.  **Feature Name**: Opt-In Media Compression & Mobile Gallery Picker
    Mode
2.  **Description**: Keep Original Quality as the strict default, while
    offering an explicit opt-in toggle on mobile/desktop to
    transcode/resize photos (e.g., HEIC→JPEG or "Share-Ready 1080p")
    with estimated size reduction before sending.
3.  **Competitors**: AirDrop (Automatic vs Keep Originals format
    toggle), Send Anywhere (quality options).
4.  **User Problem**: Non-technical users sometimes want to send 50
    iPhone `.HEIC` photos to an older Windows app or compress a 2 GB 4K
    clip to 150 MB for quick email/upload without leaving Flitnix.
5.  **Flitnix Status**: Missing.
6.  **Why It Improves Flitnix**: Adds flexibility without violating
    Flitnix's "never compress without consent" rule.
7.  **Expected User Impact**: Medium.
8.  **Technical Complexity**: Medium-High (video compression in pure
    browser is slow/limited; image resizing via `<canvas>` is easy).
9.  **Development Effort**: 1.5 weeks (images) / 4+ weeks (video).
10. **Security Implications**: Canvas re-encoding strips EXIF unless
    explicitly preserved---must clearly warn user that "Optimize Size"
    strips metadata vs "Original Quality (Default)".
11. **Backend Changes**: None (or optional metadata flag).
12. **Frontend Changes**: None.
13. **Mobile Changes**: Toggle `"Original Quality (Recommended)"` vs
    `"Smaller JPEG"` in `app.js` queue header with size estimate.
14. **Testing**: Verify default path sends byte-identical files
    untouched.
15. **Dependencies**: None.

-   **Scores**: UV: 3, SA: 3, CA: 2, F: 3, RM: 3 → **Weighted Score:
    2.80 (P3)**

### Gap 32: Floating Desktop Drop-Zone Widget & "Find My Device" Ping

1.  **Feature Name**: Compact Floating Drop Widget & Device Ring ("Find
    My Device")
2.  **Description**: Provide an optional tiny always-on-top drop target
    (or compact mode) for quick drag-and-drop without maximizing the
    full Wails window, plus a simple "Ping / Ring" button for connected
    devices.
3.  **Competitors**: KDE Connect (Find My Device), AirDrop/macOS
    Dropzone apps.
4.  **User Problem**: Opening a 1024×768 window just to drag 1 file
    feels heavy; conversely, "Find My Device" is only feasible if a
    browser tab/audio context is active (limited on locked phones).
5.  **Flitnix Status**: Missing.
6.  **Why It Improves Flitnix**: Nice quality-of-life polish, though
    secondary to core transfer/discovery gaps.
7.  **Expected User Impact**: Low-Medium.
8.  **Technical Complexity**: Medium (Wails v2 supports single window
    cleanly; secondary floating window requires OS-level or Wails
    v3/toggleable compact window mode).
9.  **Development Effort**: 1.5 weeks
10. **Security Implications**: Low; rate-limit ring/ping to prevent
    annoyance.
11. **Backend Changes**: `runtime.WindowSetSize` compact mode toggle in
    `app.go`; optional `/api/ping-alert` event.
12. **Frontend Changes**: Compact "Mini Drop Mode" toggle button in
    header.
13. **Mobile Changes**: Play audio chime on explicit user-enabled alert.
14. **Testing**: Window resize/state restoration tests.
15. **Dependencies**: System Tray (Gap 5).

-   **Scores**: UV: 2, SA: 3, CA: 2, F: 3, RM: 3 → **Weighted Score:
    2.50 (P3)**

------------------------------------------------------------------------

## 4. Master tracking register

Use this register as the one-page progress view. A gap should be checked
only after its definition of done is met. Priority labels are from the
source report.

### Phase 1 --- Unbreakable Transfers

-   [ ] **Gap 26 --- Storage Usage Dashboard, Disk Headroom Warnings &
    Auto-Cleanup** --- P1
-   [ ] **Gap 1 --- Resumable Chunked Uploads & Crash-Safe Offset
    Recovery** --- P0
-   [ ] **Gap 7 --- Automatic Stream Retry with Exponential Backoff &
    Network Roaming Recovery** --- P0
-   [ ] **Gap 2 --- Resumable Byte-Range Downloads
    (`Accept-Ranges: bytes`) for Mutable Files** --- P0
-   [ ] **Gap 31 --- Desktop-to-Phone / Phone-to-Desktop Remote File
    Browsing (Opt-In Shared Root)** --- P0
-   [ ] **Gap 22 --- Modification Timestamp & File Metadata
    Preservation** --- P1

### Phase 2 --- Discovery & Trusted Devices

-   [ ] **Gap 15 --- Device Visibility Modes & Granular Auto-Approval
    Policies** --- P0
-   [ ] **Gap 13 --- 6-Digit One-Time Transfer / Pairing Code** --- P0
-   [ ] **Gap 4 --- Mobile Browser Trusted-Device Pairing & One-Tap
    Reconnect** --- P0
-   [ ] **Gap 3 --- Automatic Nearby Device Discovery (mDNS / UDP
    Multicast)** --- P0
-   [ ] **Gap 9 --- Desktop-to-Desktop Direct Transfer Workflow (GUI
    Client Mode)** --- P0
-   [ ] **Gap 27 --- Firewall Guidance, Auto-Diagnostic Ping &
    Mobile-Side Connection Helper** --- P0

### Phase 3 --- Always-Ready Desktop

-   [ ] **Gap 10 --- Real-Time WebSockets (or SSE) Replacing HTTP
    Polling** --- P1
-   [ ] **Gap 5 --- System Tray / Menu Bar, Background Receiving &
    Autostart** --- P0
-   [ ] **Gap 8 --- Native OS Desktop Notifications & Interactive
    Approval Actions** --- P0
-   [ ] **Gap 6 --- Persistent Personal Transfer History & Actionable
    File Management** --- P0

### Phase 4 --- Secure & Advanced Sharing

-   [ ] **Gap 11 --- Encrypted Transport for CLI/Desktop-to-Desktop
    (mTLS / E2EE) & Optional Local HTTPS** --- P1
-   [ ] **Gap 12 --- Installable Mobile PWA with Web Share Target API**
    --- P1
-   [ ] **Gap 19 --- Multi-Recipient Desktop-to-Many Broadcasting (Local
    Share Room)** --- P0
-   [ ] **Gap 23 --- Password-Protected & One-Time / Download-Limited
    Share Links** --- P1
-   [ ] **Gap 17 --- Opt-In Automatic Clipboard Synchronization** --- P1

### Phase 5 --- Productivity & User Experience

-   [ ] **Gap 14 --- Media Thumbnails, File Previews & Gallery
    Selector** --- P1
-   [ ] **Gap 18 --- Duplicate Detection & Configurable File Conflict
    Policy** --- P1
-   [ ] **Gap 16 --- Adaptive Transfer Concurrency & Queue Management
    (Reorder / Pause / Prioritize)** --- P1
-   [ ] **Gap 25 --- Automatic Media Organization & Multiple Transfer
    Destinations** --- P1
-   [ ] **Gap 29 --- Mobile Web UI Theme Parity, Localization &
    Multi-File Gallery Filter** --- P1
-   [ ] **Gap 21 --- Bandwidth Limiting & Transfer Scheduling** --- P2
-   [ ] **Gap 24 --- Deep OS Share Sheet & File Manager Integration
    (macOS & Windows)** --- P2

### Phase 6 --- Advanced & Optional Features

-   [ ] **Gap 20 --- Optional Self-Hosted Relay / WebRTC Internet Mode**
    --- P2
-   [ ] **Gap 30 --- Watch Folders & One-Way / Two-Way Incremental
    Folder Sync** --- P3
-   [ ] **Gap 28 --- Consent-Based Media Compression & Format
    Compatibility (HEIC/4K Toggle)** --- P3
-   [ ] **Gap 32 --- Floating Desktop Drop-Zone Widget & "Find My
    Device" Ping** --- P3

------------------------------------------------------------------------

## 5. Dependency map and sequencing decisions

This section turns the source gap cards' dependency notes into an
executable order. Where the source lists a dependency that belongs to a
later phase, the work is split into a safe foundation and a later
integration.

### 5.1 Transfer reliability

1.  **G26 core disk protection:** add free-space checks and pre-approval
    quota protection before resumable storage can grow.
2.  **G1 resumable upload engine:** implement durable `.part`/metadata
    checkpoints, offset probing and recovery wiring.
3.  **G7 automatic retry:** retry transient failures and resume from the
    committed offset.
4.  **G2 byte-range downloads:** add validators and safe range support
    for mutable files.
5.  **G31 shared-folder browsing:** expose only explicitly shared items
    through opaque identifiers.
6.  **G22 timestamp preservation:** restore validated source
    modification times on publication.
7.  **G6 history integration:** G26's history-retention/cleanup portion
    is completed after persistent personal history exists in Phase 3.

### 5.2 Discovery and trusted devices

1.  **G15 policy foundation:** define visibility and auto-approval
    policy before discovery is exposed.
2.  **G13 short code:** provide a secure QR alternative with short TTL,
    one-time redemption and lockout.
3.  **G4 mobile trusted pairing:** complete device key enrollment and
    authenticated reconnection.
4.  **G3 discovery service:** implement LAN discovery without secrets
    and bind it to the selected interface.
5.  **G9 GUI sender:** integrate discovery/short-code targets with
    `internal/client`.
6.  **G27 firewall diagnostics:** provide connection troubleshooting and
    read-only firewall inspection.

**Dependency note:** The G3 card's dependency line refers to
"Desktop-to-Desktop GUI Sender (Gap 6)" and "Device Visibility Controls
(Gap 14)". Those numeric references do not match the feature numbering
in the same 32-gap list. This roadmap preserves the original gap card
verbatim and resolves the dependency by feature name: desktop GUI sender
is G9 and visibility controls are G15. The implementation sequence
therefore builds the discovery service first, then connects the sender
UI. The short-code path remains a fallback if multicast discovery is
blocked.

**Visibility integration note:** G15's policy model and server
enforcement are started in Phase 2. Its tray-menu controls are connected
in Phase 3 after G5's tray foundation exists.

### 5.3 Background operation

1.  **G10 event channel:** establish authenticated real-time state
    delivery and a reconciliation fallback.
2.  **G5 tray foundation:** implement hide/restore/quit and background
    listener lifecycle.
3.  **G8 native notifications:** connect approval/pairing/completion
    callbacks to OS notifications.
4.  **G5 completion:** add tray actions and settings after notification
    callbacks exist.
5.  **G6 persistent history:** store completed/failed personal transfers
    and add safe file actions.
6.  **G26 completion:** add retention and history cleanup using G6's
    persistent store.

**Circular dependency note:** G5 and G8 refer to one another as
dependencies in their source cards. Break the cycle by implementing the
minimum tray lifecycle first, then notifications, then completing tray
approval/visibility actions.

### 5.4 Secure and advanced sharing

-   **G11** follows G4 and G9 so paired browser and peer identity
    foundations are available.
-   **G12** follows G4 and G11 for persistent PWA reconnection and full
    secure-origin Share Target behavior.
-   **G19** extends the existing `internal/guest` foundation and must
    preserve guest isolation.
-   **G23** can be implemented as a share-session policy layer with
    rate-limited PIN attempts and atomic download limits.
-   **G17** follows G10 and G4 so clipboard events can be delivered to
    authenticated trusted devices.

### 5.5 Productivity and UX

-   **G14** needs strict media content-type handling and bounded
    thumbnail storage.
-   **G18** must keep "Keep Both" as the safe default; overwrite remains
    opt-in.
-   **G16** must preserve `Coordinator` concurrency and cancellation
    invariants.
-   **G25** must sanitize generated folder names and enforce path
    confinement.
-   **G29** can reuse desktop locale resources and theme conventions.
-   **G21** must account for low-rate bandwidth caps in stall-timeout
    logic.
-   **G24** should reuse existing OS file-open and path-extraction
    handling while batching multi-select launches safely.

### 5.6 Advanced features

-   **G20** follows G11; remote relay mode must be opt-in and encrypted.
-   **G30** follows G9, G4 and G18; synchronization is restricted to
    mutually trusted desktop peers.
-   **G28** keeps original quality as the default and requires explicit
    consent for transformations.
-   **G32** follows G5 so compact mode can reuse the background/tray
    lifecycle.

------------------------------------------------------------------------

## 6. Release plan

### v0.3.0 --- Seamless & Unbreakable

**Included:** Phases 1, 2 and 3.

Primary outcomes:

-   Resumable uploads and downloads.
-   Automatic retry and recovery.
-   Disk protection.
-   Shared-folder browsing.
-   Nearby discovery and desktop GUI sending.
-   Mobile trusted pairing and six-digit code.
-   Visibility and firewall controls.
-   Real-time events.
-   System tray and background receiving.
-   Native notifications.
-   Persistent personal history.
-   Original modification timestamp preservation.

**Release gate:** Do not ship this milestone until large-transfer
recovery, discovery authorization, background approval behavior and
persistent history pass their acceptance tests.

### v0.4.0 --- Secure & Expanded Sharing

**Included:** Phase 4.

Primary outcomes:

-   Encrypted peer transport.
-   Mobile PWA and Share Target support where browser secure-context
    requirements permit.
-   Multi-recipient sharing rooms.
-   PIN-protected and download-limited shares.
-   Opt-in trusted-device clipboard sync.

**Release gate:** Verify certificate pinning, guest isolation, room
modes, PIN lockout and clipboard consent.

### v0.4.1 --- Productivity & UX

**Included:** Phase 5.

Primary outcomes:

-   Media previews.
-   Duplicate/conflict policies.
-   Queue concurrency and controls.
-   Smart destination organization.
-   Mobile theme/localization/filter parity.
-   Bandwidth caps and scheduling.
-   macOS/Windows OS sharing integration.

**Release gate:** Verify safe previews, path confinement,
non-destructive conflict defaults, concurrency limits and
platform-specific packaging.

### v0.5.0 and beyond --- Advanced Optional Features

**Included:** Phase 6.

Primary outcomes:

-   Optional WebRTC/self-hosted relay.
-   Watch folders and incremental sync.
-   Consent-based media optimization.
-   Floating drop widget and device ping.

**Release gate:** Verify opt-in behavior, encryption, deletion safety,
original-quality defaults and idle-resource limits.

------------------------------------------------------------------------

## 7. Top 10 cross-gap implementation groups

The source report identifies these as the highest-impact combinations.
They should be treated as cohesive implementation groups, even though
the detailed register continues to track each gap separately.

1.  **G1 + G7:** Resumable chunked uploads, auto-retry and crash
    recovery.
2.  **G2 + G31:** Resumable downloads and shared-folder item browsing.
3.  **G3 + G9:** LAN discovery and desktop-to-desktop GUI sender.
4.  **G4:** Mobile trusted-device pairing and one-tap reconnect.
5.  **G5:** System tray, background receiving and autostart.
6.  **G8:** Native OS notifications for approval and completion.
7.  **G6:** Persistent transfer history and actionable file management.
8.  **G13 + G15:** Six-digit connection code and visibility controls.
9.  **G10:** Real-time WebSockets/SSE event channel.
10. **G11:** Hybrid encrypted transport for desktop/CLI peers and
    optional paired-browser encryption.

**Natural companions:** G19 multi-recipient rooms and G22
modification-time preservation.

------------------------------------------------------------------------

## 8. Development effort guide

The estimates below are the individual effort ranges in the source
report. They are not total phase durations and should not be added
together as a calendar forecast. Shared infrastructure may reduce
duplicated work; integration, packaging and cross-platform validation
may increase elapsed time.

### Low effort / quick-win range

-   G2 --- 1 week in its detailed gap card; the source complexity
    summary also groups range-download work among lower-complexity
    tasks.
-   G22 --- 2--3 days.
-   G25 --- 4--5 days.
-   G29 --- 1 week.
-   G31 --- 1 week.

### Medium effort range

-   G3 --- 2 weeks.
-   G4 --- 2 weeks.
-   G5 --- 2 weeks.
-   G6 --- 1--1.5 weeks.
-   G7 --- 1.5 weeks.
-   G8 --- 1 week.
-   G9 --- 1.5--2 weeks.
-   G10 --- 1.5 weeks.
-   G12 --- 1--1.5 weeks.
-   G13 --- 1 week.
-   G14 --- 2 weeks.
-   G15 --- 1 week.
-   G17 --- 1.5 weeks.
-   G18 --- 1 week.
-   G19 --- 1.5 weeks.
-   G21 --- 1 week.
-   G23 --- 1 week.
-   G24 --- 2--3 weeks.
-   G26 --- 1 week.
-   G27 --- 1 week.
-   G32 --- 1.5 weeks.

### High effort / architectural range

-   G1 --- 3--4 weeks.
-   G11 --- 2--3 weeks.
-   G16 --- 2 weeks (medium-high complexity).
-   G20 --- 4--6 weeks.
-   G28 --- 1.5 weeks for images; 4+ weeks for video.
-   G30 --- 3--4 weeks.

**Note:** Use the effort estimate in each original gap card as the
primary feature-level estimate. The grouping above is only a planning
aid; where the source's broad complexity summary differs from an
individual gap card, retain the detailed gap-card estimate for task
planning.

------------------------------------------------------------------------

## 9. Risk register --- issues to keep visible

  -----------------------------------------------------------------------
  Risk                    Related gaps            Required control
  ----------------------- ----------------------- -----------------------
  Resume offset abuse or  G1, G7                  Bind offsets to
  corrupt partial files                           authenticated
                                                  session/item leases;
                                                  enforce committed
                                                  offset; durable
                                                  checkpoint and recovery
                                                  tests

  Disk exhaustion         G1, G26                 Pre-approval disk-space
                                                  check, bounded partial
                                                  storage and 507
                                                  response

  File mutation during    G2                      ETag/If-Range
  range download                                  validation and abort if
                                                  source changes

  Discovery leaks secrets G3                      Only sanitized public
                                                  metadata; no tokens or
                                                  sensitive session data

  Six-digit code brute    G13                     Short TTL, single-use
  force                                           redemption, IP/global
                                                  rate limit and lockout

  Background unsolicited  G5, G15                 Visibility modes,
  requests                                        trusted-only policy and
                                                  explicit approval

  WebSocket credential    G10                     Authenticate without
  leakage                                         putting bearer tokens
                                                  in URL query strings;
                                                  size limits and
                                                  origin/host checks

  Plaintext peer traffic  G11                     TLS 1.3 and pinned
                                                  identity for
                                                  desktop/CLI peers;
                                                  explicit browser
                                                  security design

  Guest room permission   G19, G23                Mode isolation, scoped
  expansion                                       source access, PIN rate
                                                  limits and download
                                                  limits

  Unsafe previews         G14                     No active SVG/HTML
                                                  rendering; strict image
                                                  types, bounded cache
                                                  and fuzz tests

  Destructive conflict    G18                     Keep Both default;
  behavior                                        overwrite opt-in;
                                                  refuse
                                                  symlinks/directories

  Concurrency race or     G16                     Bounded worker count,
  memory growth                                   coordinator invariants
                                                  and race-detector
                                                  stress tests

  Path traversal in       G25, G31, G30           Sanitization, opaque
  auto-organization                               item IDs and
                                                  `withinDir`/root
                                                  confinement

  Relay privacy/downgrade G20                     Disabled by default,
                                                  explicit disclosure,
                                                  E2EE and no silent
                                                  third-party relay

  Silent remote deletion  G30                     Append/update-first
                                                  behavior; no silent
                                                  remote delete

  Metadata loss from      G28                     Original-quality
  conversion                                      default and explicit
                                                  warning/consent

  Tray platform           G5, G32                 Platform-specific tests
  inconsistency                                   and graceful fallback
                                                  when tray support is
                                                  unavailable
  -----------------------------------------------------------------------

------------------------------------------------------------------------

## 10. Features explicitly out of scope

The source report recommends not copying every competitor feature. Keep
Flitnix focused on local-first file transfer and sharing.

Do not add these as part of this roadmap:

-   Mandatory accounts or sign-in.
-   Mandatory cloud storage.
-   Advertisements.
-   Telemetry or phone-home tracking.
-   Forced image/video compression.
-   Unnecessary phone permissions.
-   Remote camera, SMS mirroring or screen mirroring.
-   Silent third-party relay usage.
-   Unbounded background activity.

These are product boundaries, not gaps to be filled.

------------------------------------------------------------------------

## 11. First implementation sequence

The first work should begin with the transfer foundation, not UI polish.

### First task group: G26 core + G1 design

1.  Audit `internal/transfer/store.go`, `service.go`, `coordinator.go`,
    `internal/recovery`, `internal/server` upload handlers, `app.go` and
    `cli.go`.
2.  Confirm how `.part` files, metadata and the recovery journal are
    currently created, cleaned up and restored.
3.  Define the durable upload-offset contract and the relationship
    between the session, batch, item lease and recovery metadata.
4.  Implement the disk-space precheck and bounded partial-storage
    policy.
5.  Wire `RecoveryDir` into the production application and CLI entry
    points.
6.  Implement resumable upload handlers and client chunk/offset logic.
7.  Add crash-boundary, offset-tampering, race, disk-quota and
    network-drop tests.
8.  Only after this foundation is stable, implement G7 automatic retry
    against the resumable protocol.

### First task group exit criteria

-   No unbounded partial-file growth.
-   No arbitrary offset writes.
-   No token/session bypass on resume.
-   No partial file is published as complete.
-   Recovery state is authenticated and validated.
-   Tests prove correct recovery after process termination and
    connection reset.
-   Existing streaming memory bounds and integrity checks remain intact.

### Next task after G1/G7

Proceed to G2 resumable downloads, then G31 folder browsing and G22
metadata preservation, following Phase 1.

------------------------------------------------------------------------

## 12. Progress and change-control policy

For each gap, maintain:

-   **Status:** Not started / In progress / In review / Testing /
    Complete / Blocked.
-   **Owner:** Person or agent implementing the task.
-   **Branch/commit:** Exact Git reference.
-   **Files changed:** Backend, frontend, mobile and packaging files.
-   **Tests:** Commands executed and results.
-   **Security review:** Findings and resolution.
-   **Known limitations:** Browser/platform-specific caveats.
-   **Next dependency:** The next gap unlocked by this work.

### Change-control rules

-   Do not renumber the 32 gaps.
-   Do not silently change a P0/P1/P2/P3 score.
-   If implementation reveals a new dependency, document it before
    changing phase order.
-   If a gap is split into sub-tasks, keep one parent gap in the master
    register and track sub-tasks underneath it.
-   If a feature is deferred, record the reason; do not mark it
    complete.
-   Re-audit existing code before implementing a gap because the
    codebase may have changed since the source report was written.
-   Keep the original detailed gap card as the reference for intended
    scope, then record any approved implementation deviation separately.

------------------------------------------------------------------------

## 13. Source basis and interpretation notes

This roadmap is based on the attached **Flitnix --- Competitive
Analysis, Codebase Audit & Feature Gap Discovery** report, especially:

-   Section 7 --- Top 32 Feature Opportunities (Detailed Gap Breakdown).
-   Section 8 --- Prioritization Framework & P0/P1/P2/P3 Roadmap.
-   Section 9 --- Top 10 Recommended Features and Architectural
    Feasibility.
-   Sections 10--13 --- Product boundaries, security assessment,
    reliability opportunities and recommended release scope.

The 32 original gap cards are reproduced under their assigned phases.
Their descriptions, status assessments, technical notes, security
implications, tests, dependencies, effort estimates and scores are
retained as source material.

### Interpretation notes

-   The phase sequence is the proposed execution plan, not a claim that
    all dependencies are already satisfied.
-   G26 is split operationally: core disk-space protection is early;
    history retention integrates after G6.
-   G3's source dependency line contains numeric references that do not
    align with the current gap numbering. The original line is
    preserved; this plan resolves the dependency by feature name.
-   G3/G9 discovery and GUI-sender work is sequenced as discovery
    service first, sender integration second, with G13 as a fallback.
-   G5/G8 have a circular dependency in their source notes. This plan
    breaks it into tray foundation, notifications, then final tray
    integration.
-   G15's policy model starts before discovery; tray controls are
    connected when G5 is implemented.
-   Effort estimates are planning inputs, not guaranteed delivery dates.
-   All implementation details must be rechecked against the current
    repository before coding.

------------------------------------------------------------------------

## 14. Final execution principle

**Finish the current gap, prove it works, document it and commit it
before moving to the next unrelated gap.**

The target is not simply to add 32 features. The target is to make
Flitnix more reliable, secure, discoverable and useful while preserving
its lightweight, local-first, privacy-respecting identity.

## Appendix A --- Original weighted prioritization table

The following prioritization framework and table are retained from the
source report. The phase sequence above may reorder work to satisfy
implementation dependencies.

## 8. Prioritization Framework & P0/P1/P2/P3 Roadmap

Weighted Formula: `0.30*UV + 0.25*SA + 0.20*CA + 0.15*F + 0.10*RM`

  -----------------------------------------------------------------------------------------------------
  Rank    ID      Feature                  UV      SA      CA      F (15%) RM      Weighted   Tier
                                           (30%)   (25%)   (20%)           (10%)   Score      
  ------- ------- ------------------------ ------- ------- ------- ------- ------- ---------- ---------
  1       Gap 2   Resumable Byte-Range     5       5       4       4       4       **4.55**   **P0**
                  Downloads (`HTTP 206`)                                                      

  2       Gap 3   Automatic Nearby         5       5       4       4       4       **4.55**   **P0**
                  Discovery (mDNS /                                                           
                  Multicast)                                                                  

  3       Gap 9   Desktop-to-Desktop GUI   5       5       4       4       4       **4.55**   **P0**
                  Sender                                                                      
                  (`internal/client` UI)                                                      

  4       Gap 1   Resumable Chunked        5       5       5       3       3       **4.50**   **P0**
                  Uploads & Recovery                                                          
                  Activation                                                                  

  5       Gap 31  Browse & Pick Files      4       5       4       5       5       **4.50**   **P0**
                  Inside Shared Folders                                                       
                  (No Forced ZIP)                                                             

  6       Gap 4   Mobile Browser Trusted   5       5       4       4       3       **4.45**   **P0**
                  Pairing & 1-Tap                                                             
                  Reconnect                                                                   

  7       Gap 19  Two-Way / Download-Only  4       5       5       4       4       **4.45**   **P0**
                  Crowd Sharing Rooms                                                         

  8       Gap 6   Persistent Personal      4       5       3       5       5       **4.30**   **P0**
                  Transfer History & File                                                     
                  Actions                                                                     

  9       Gap 7   Auto-Retry Data Streams  5       5       4       3       3       **4.30**   **P0**
                  & Wi-Fi Roaming Recovery                                                    

  10      Gap 15  Device Visibility Modes  4       5       3       5       5       **4.30**   **P0**
                  & Conditional                                                               
                  Auto-Approve                                                                

  11      Gap 5   System Tray / Menu Bar,  5       5       3       4       3       **4.25**   **P0**
                  Background Mode &                                                           
                  Autostart                                                                   

  12      Gap 13  6-Digit One-Time Connect 4       5       4       4       4       **4.25**   **P0**
                  / Transfer Code                                                             

  13      Gap 27  OS Firewall Inspection & 4       5       4       4       4       **4.25**   **P0**
                  Actionable Doctor                                                           
                  Guidance                                                                    

  14      Gap 8   Native OS Desktop        4       5       3       5       4       **4.20**   **P0**
                  Notifications (Approval                                                     
                  & Completion)                                                               

  15      Gap 22  Modification Timestamp   4       5       3       5       5       **4.30**   **P1**
                  (`mtime`) Preservation                                                      *(Quick
                                                                                              Win)*

  16      Gap 12  Installable Mobile PWA & 4       5       4       3       4       **4.10**   **P1**
                  Web Share Target API                                                        

  17      Gap 10  Real-Time WebSockets /   4       5       3       4       4       **4.05**   **P1**
                  SSE Event Channel                                                           

  18      Gap 11  Encrypted Transport      4       5       4       3       3       **4.00**   **P1**
                  (mTLS for Peers + Opt                                                       
                  Local HTTPS)                                                                

  19      Gap 14  Media Thumbnails &       4       4       4       4       4       **4.00**   **P1**
                  In-App File Previews                                                        

  20      Gap 26  Storage Usage Dashboard  4       5       2       4       4       **3.85**   **P1**
                  & Pre-Approval Disk                                                         
                  Quota Check                                                                 

  21      Gap 18  Duplicate File Detection 4       4       3       4       4       **3.80**   **P1**
                  & Conflict Policy                                                           

  22      Gap 17  Opt-In Automatic         4       4       4       3       3       **3.75**   **P1**
                  Clipboard                                                                   
                  Synchronization                                                             

  23      Gap 25  Smart Subfolder          3       4       3       5       5       **3.75**   **P1**
                  Organization                                                                
                  (`by_device` / `by_type`                                                    
                  / `by_date`)                                                                

  24      Gap 23  PIN-Protected &          3       4       3       5       4       **3.65**   **P1**
                  Download-Count-Limited                                                      
                  Share Sessions                                                              

  25      Gap 16  Adaptive Small-File      4       4       3       3       3       **3.55**   **P1**
                  Concurrency & Queue                                                         
                  Reordering                                                                  

  26      Gap 29  Mobile Web Light/Dark    3       4       2       5       5       **3.55**   **P1**
                  Theme, i18n &                                                               
                  Search/Filter                                                               

  27      Gap 20  Opt-In WebRTC /          4       3       4       2       2       **3.25**   **P2**
                  Self-Hosted Relay Mode                                                      

  28      Gap 24  macOS Finder/Share Ext & 3       4       3       3       3       **3.25**   **P2**
                  Win11 Multi-Select                                                          
                  Batching                                                                    

  29      Gap 21  Bandwidth Limiting &     2       4       2       5       4       **3.15**   **P2**
                  Scheduled Transfers                                                         

  30      Gap 28  Opt-In Consent-Based     3       3       2       3       3       **2.80**   **P3**
                  Media Compression                                                           

  31      Gap 30  Watch Folders &          3       3       3       2       2       **2.75**   **P3**
                  Incremental Delta Sync                                                      

  32      Gap 32  Floating Compact Drop    2       3       2       3       3       **2.50**   **P3**
                  Widget & Find-My-Device                                                     
                  Ping                                                                        
  -----------------------------------------------------------------------------------------------------

*(Note: Gap 22 scored 4.30 mathematically, placed at the top of P1/Quick
Wins as a 2-day patch alongside the P0 flagship releases; P0 focuses on
the 14 foundational reliability, discovery, background-operation, and UX
gaps.)*

------------------------------------------------------------------------
