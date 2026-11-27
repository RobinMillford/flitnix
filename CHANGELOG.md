# Changelog

All notable changes to Flitnix will be documented in this file.

The format is based on [Keep a Changelog](https://keepachangelog.com/en/1.1.0/),
and this project adheres to [Semantic Versioning](https://semver.org/spec/v2.0.0.html).

---

## [Unreleased]

### Added
- **Persistent Transfer History & Configurable Retention (Phase 3 — G6 & G26)**:
  - Added durable, privacy-preserving personal transfer history (`internal/history`, `<dataDir>/history.jsonl`) surviving restarts across incoming, batch, phone download, and computer-sharing transfers, upgraded `TransferHistoryView.svelte` with search, direction/status/date/device filters, summary metrics, pagination, `Open`/`Reveal`/`Reshare` (with strict receive-directory symlink confinement), and safe `Clear History`.
  - Completed deferred G26 history retention (`CurrentSchemaVersion = 6`): configurable age/count limits (`HistoryRetentionDays = 90`, `HistoryMaxRecords = 5000`), non-blocking startup and periodic pruning, and Settings Storage UI controls that prune only metadata without touching transferred files.
- **Native OS Notifications & Interactive Approval (Phase 3 — G8)**:
  - Implemented `internal/notify` supporting D-Bus `org.freedesktop.Notifications` (Linux), `go-toast/v2` (Windows), and argument-safe macOS notifications, backed by sanitized Pango/BIDI-safe text, single-use 128-bit action tokens, state/visibility/quota revalidation, SAS-only pairing focus protection, privacy filename masking, quiet mode, and per-category settings.
- **System Tray, Background Receiving & Login Autostart (Phase 3 — G5)**:
  - Implemented `internal/tray` (Linux D-Bus SNI + DBusMenu, Windows Win32 tray, macOS Cocoa `NSStatusBar`, safe headless fallback) and `internal/autostart` (`flitnix.desktop`, Windows HKCU `Run`, macOS `LaunchAgent` plist) with `--background` startup, close/minimize-to-tray preserving active sessions and transfers, live visibility/DND tray controls, and explicit `Quit Flitnix`.
- **Real-Time WebSockets/SSE Event Infrastructure (Phase 3 — G10)**:
  - Added `internal/events` pub/sub Hub, `docs/ADR-001-realtime-events.md`, authenticated fetch-stream SSE endpoints (`GET /api/events` & `GET /guest-api/events`), desktop `events.Hub` -> Wails `"flitnix:event"` coalescer (`frontend/src/realtimeEvents.ts`), and mobile SSE clients (`app.js`, `guest.js`) with Origin validation, TTL/revocation enforcement, queue backpressure shedding, and seamless polling fallback.
- **Desktop-to-Desktop GUI Sharing & Single-Port TLS 1.3 Multiplexing (Phase 2 — G9)**:
  - Added single-port TLS 1.3 + HTTP connection multiplexing (`internal/server/tls_mux.go`) backed by each desktop host's Ed25519 self-signed identity certificate (`NewHostIdentityCertificate`), preserving zero-install HTTP QR browsing alongside TOFU-pinned TLS 1.3 desktop transfers (`internal/client/tls.go`).
  - Added `POST /api/peer/challenge`, `POST /api/peer/connect`, `peer_transfer.go` orchestration, bidirectional file/folder staging, live job progress/cancellation, and the dedicated **Computer Sharing** tab (`frontend/src/views/ComputerSharingView.svelte`) across Windows, macOS, and Linux.
- **Automatic Nearby LAN Discovery (Phase 2 — G3)**:
  - Implemented `internal/discovery` combining standard `_flitnix._tcp.local.` mDNS announcements (`mdns_wire.go`) and UDP multicast (`239.255.43.82:43822`) bound strictly to `internal/netenv`'s active non-VPN interface.
  - Added Ed25519-signed sanitized discovery frames, unlinkable HMAC trusted probes in `VisibilityTrustedOnly`, strict radio silence in `VisibilityHidden`, stale-peer eviction, and live `"discovery:peers"` desktop updates.
- **Firewall Diagnostics & Connection Helper (Phase 2 — G27)**:
  - Added `firewall_status` as the 8th Connection Doctor check (`internal/doctor/firewall.go`) supporting Windows (`netsh advfirewall`), Linux (`ufw`/`firewall-cmd`/`nft`/`iptables`), and macOS (`socketfilterfw`) with a 1.5s probe timeout, `affectedPort` metadata, copyable OS remediation commands in `ConnectionDoctorModal.svelte`, and mobile reachability tips.
- **Mobile Browser Trusted-Device Pairing & One-Tap Reconnect (Phase 2 — G4)**:
  - Vendored `nacl-fast.min.js` (`tweetnacl@1.0.3`) and implemented `internal/server/assets/device-crypto.js` using Ed25519 + X25519 + 271-byte SAS verification + IndexedDB storage so standard mobile browsers on plain HTTP LAN origins can pair and reconnect without installing an app or PWA.
  - Added `POST /api/device-sessions/challenge` and `POST /api/device-sessions/verify` (`internal/server/device_sessions.go`), `#device=` one-tap reconnect URLs, and a global desktop SAS pairing approval modal in `App.svelte`.
- **Six-Digit One-Time Connection Code (Phase 2 — G13)**:
  - Added `internal/session/shortcode.go` (`ShortCodeManager`) issuing `crypto/rand` 6-digit single-use codes with a 3-minute TTL, constant-time HMAC validation, per-IP rate limiting, and `POST /api/connect-code`, paired with a `"Connect with 6-Digit Code"` form on the mobile browser landing page.
- **Device Visibility & Conditional Auto-Approval (Phase 2 — G15)**:
  - Added a persistent 4-mode visibility state machine (`hidden`, `trusted`, `temporary_everyone` with 10-minute auto-revert, `everyone`) in `internal/devices/visibility.go` and `internal/settings` (`CurrentSchemaVersion = 3`), plus independent QR pause/revoke controls (`SetQRPaused`, `RevokeByDeviceID`).
  - Added per-device conditional auto-approval (`internal/devices/policy.go`) with configurable byte size limits and executable/script blocking (`AutoApproveMaxBytes`, `BlockExecutables`, `BlockScripts`), trust/reason badges, and policy controls in `TrustedDevicesView.svelte` and `PersonalTransferView.svelte`.
- **QR-First Mobile Hardening (Phase 2 — Priority Zero)**:
  - Fixed iOS Safari single-file download fallback, mobile `fmtBytes` reference error, and drag-and-drop styling on `#send-panel`.
- **Original Modification Timestamp Preservation (Phase 1 — G22)**:
  - Propagated validated sender modification timestamps (`lastModifiedMs`) from the mobile browser (`File.lastModified`) and Go CLI (`client.BuildManifest`) through `POST /api/batches` and `POST /api/uploads`, persisting them in recovery journals and resumable `.meta` sidecars across pauses and restarts.
  - Applied `os.Chtimes` atomically to staged `.part` files prior to publication in both single-pass `PUT` and resumable `PATCH` commits so received files retain their original capture/edit dates immediately upon appearing on disk.
  - Added the `"Preserve original file dates"` setting (`PreserveModTimes`, default ON) in `internal/settings`, Wails bindings, and `SettingsView.svelte`.
- **Browseable Shared-Folder Explorer (Phase 1 — G31)**:
  - Assigned opaque per-item IDs inside shared folders (`transfer.SourceItem.ID`) and added strictly confined hierarchical folder browsing (`GET /api/sources/{id}/items?prefix=&cursor=&limit=`) without exposing host filesystem paths.
  - Extended `POST /api/download-grants` with selective folder item IDs (`sourceId` + `itemIds`): downloading one file streams directly with full G2 byte-range resumption (`Accept-Ranges: bytes`), while selecting multiple files across subdirectories streams a targeted ZIP archive.
  - Built an interactive mobile shared-folder explorer in `index.html`, `styles.css`, and `app.js` with `Browse` buttons, breadcrumb navigation, pagination, and multi-file checkboxes across subdirectories.
- **Resumable HTTP Byte-Range Downloads (Phase 1 — G2)**:
  - Enabled `Accept-Ranges: bytes`, strong file-metadata validators (`ETag: "flit-<size>-<mtime_ns>-<file_id>"`), `Range` (`206 Partial Content`), `If-Range` verification (with `200 OK` full fallback on mismatch or weak validators), `416 Range Not Satisfiable`, and `HEAD` support across mutable single-file download grants (`/api/downloads/{id}`) and direct file downloads (`/api/files/{id}/download`), while keeping OS-immutable Tier 1 (`"sha256-..."`) and Tier 3 dynamic ZIP (`Accept-Ranges: none`) contracts intact.
  - Added mid-stream file-mutation and atomic-replacement guards (`fileMatchesSnapshot`) so modified or swapped source files cleanly abort live streams instead of serving spliced bytes.
  - Upgraded mobile File System Access (`showSaveFilePicker`) streaming downloads in `app.js` with bounded automatic byte-range resumption (`Range` + `If-Range`) and incremental SHA-256 verification.
- **Automatic Stream Retry & Wi-Fi Roaming Recovery (Phase 1 — G7)**:
  - Implemented `RetryPolicy` and error classification (`internal/client/retry.go`) with jittered exponential backoff (`1s`, `2s`, `4s`, `8s`, `15s` ±20%), `/ping` reachability probing, and automatic `HEAD` + `PATCH` offset recovery in Go CLI (`internal/client`, `send_cli.go`).
  - Added bounded automatic chunk retry and `"Reconnecting — attempt X/Y"` progress UI in the mobile browser (`app.js`) with immediate `online` event wakeup and non-destructive batch sibling preservation.
  - Preserved sessions, coordinator batches, and resumable staging across temporary Wi-Fi drops and IP roaming (`handleNetworkChange` + `AbortActiveUploads` interruption handling).
- **Resumable Chunked Uploads & Crash-Safe Offset Recovery (Phase 1 — G1)**:
  - Implemented `HEAD` / `PATCH` resumable chunked upload endpoints on `/api/batches/{batchID}/items/{itemID}/content` and `/api/uploads/{id}/content` using `Upload-Offset`, `Upload-Length`, `Upload-Chunk-Size`, and `409 Conflict` offset-mismatch reconciliation while keeping legacy `PUT` streams compatible.
  - Added crash-safe chunk durability (`fsync(.part)` → HMAC-authenticated `.meta.tmp` with incremental SHA-256 hash state → `fsync` → atomic rename → `syncDir`) using an installation-derived `staging-meta-v1` subkey, rolling back uncommitted torn tails automatically.
  - Wired `RecoveryDir` into both desktop (`App`) and `flitnix serve` CLI, completing startup recovery Phases 6–10 (`recovery_startup.go`): per-upload quarantine isolation, quota reservation reconstruction, session/coordinator reconciliation, and Phase 10 TTL/orphan staging cleanup.
  - Added non-destructive `PauseBatch`, `ResumeBatch`, `DiscardBatch`, and `DiscardUpload` lifecycle controls in Coordinator, Transfer Service, Wails adapter, and Desktop Svelte UI (`Uploading`, `Paused`, `Interrupted`, `Resuming`, `Completed`, `Failed`, `Discarded`), plus Web Worker `File.slice()` chunking in mobile `upload-worker.js` / `upload-client.js` / `app.js` and Go CLI `internal/client`.
- **Storage Quota & Disk-Space Protection (Phase 1 — G26)**:
  - Cross-platform filesystem free-space probing (`unix.Statfs` / `GetDiskFreeSpaceEx`) with configurable minimum disk reserve buffer (`BufferReserveBytes`, default `1 GiB`) and optional hard receive cap (`MaxReceiveBytes`).
  - Atomic `QuotaManager` headroom reservations at transfer approval (and auto-approve / guest-room admission), plus mid-stream 4 MiB headroom checks and `ENOSPC` / `ERROR_DISK_FULL` mapping to HTTP `507 Insufficient Storage` (`code: "insufficient_storage"`).
  - Desktop Storage Headroom telemetry in Settings and Approval Inbox (fit/exceeds status indicators) and clear mobile error reporting (`507`) when desktop storage is full.
  - Note: History auto-retention policies are intentionally deferred to Phase 3 (G6).

---

## [0.2.7.4] - 2026-11-10

### Fixed
- **Restored File-Transfer Throughput**: Large uploads no longer run their HTTP PUT on the browser main thread. Uploads are streamed from a dedicated Web Worker with an identical main-thread fallback, so painting, progress math, and layout work can no longer starve the socket write path.
- **Progress Repaint Throttling**: Host and guest progress panels repaint at most once every 200 ms, with a forced repaint on completion, preventing UI churn from degrading sustained transfer speed on low-end phones.

### Changed
- Added an explicit `worker-src 'self'` Content-Security-Policy directive so the upload worker loads under the hardened header policy.
- Bumped application release version to `v0.2.7.4` across runtime metadata, Wails packaging configuration, frontend UI views, localization catalogs, packaging scripts, and documentation.

---

## [0.2.7.3] - 2026-11-08

### Security
- **Defensive HTTP Response Headers**: Emitted `X-Content-Type-Options: nosniff`, `X-Frame-Options: DENY`, `Referrer-Policy: no-referrer`, and strict `Content-Security-Policy` with `frame-ancestors 'none'` across all server endpoints to prevent clickjacking and MIME confusion.
- **CSRF Referer Enforcement**: Validated `Referer` headers against `r.Host` on state-changing operations when `Origin` is omitted.
- **Archive Export Hardening**: Hardened Guest Drop Box ZIP export against Zip Slip and path traversal using forward-slash normalization, drive volume removal, and strict room boundary containment checks.
- **Safe Protocol Schemes**: Enforced `http:` and `https:` URL protocol validation in desktop external URL opener to block execution of dangerous or unvalidated URI schemes.

### Changed
- Bumped application release version to `v0.2.7.3` across runtime metadata, Wails packaging configuration, frontend UI views, localization catalogs, and packaging scripts.

---

## [0.2.0] - 2026-10-30

Major release introducing high-performance single-pass streaming, multi-tier buffer pooling, Trusted Devices authentication, Guest Drop Box, Connection Doctor, Quick Text sharing, and enhanced desktop integration.

### Added
- **Transfer Engine v2 & Single-Pass Streaming**:
  - Immediate-start single-pass file and ZIP archive downloads with pipelined SHA-256 digest computation (`PipelinedHashReader`).
  - Standard RFC 9530 `Content-Digest` HTTP header support for client-side download integrity verification.
  - Multi-tier zero-allocation buffer pooling (`sync.Pool` with 64 KiB, 256 KiB, and 1 MiB tiers) reducing memory churn and GC pressure during multi-gigabit LAN transfers.
  - Bounded-memory streaming verification on mobile clients via Web Streams API (`File.stream()`).
- **Trusted Devices Protocol v1**:
  - Mutual cryptographic device pairing using ECDSA P-256 keys and challenge-response authentication.
  - Configurable auto-accept policies for paired trusted devices with instant token revocation.
  - Dedicated CLI `pair` command for headless device pairing.
- **Guest Drop Box (Multi-Device Upload Rooms)**:
  - Ephemeral multi-device upload rooms with cryptographic invitation capabilities in pairing URLs.
  - Dual host-approval gates: room join approval and per-batch upload review.
  - Sandboxed folder isolation per guest (`Flitnix Guest Drop/<Room-ID>/<Guest-ID>-<Guest-Name>/`).
  - Upload-only access model ensuring complete isolation from host downloads and other guests.
- **Connection Doctor**:
  - Comprehensive 7-check network diagnostic engine evaluating network adapters, IPv4 configuration, port 43821 availability, `/healthz` HTTP loopback probes, client isolation detection, and multi-homed routing.
  - Interactive desktop diagnostic UI and CLI `doctor` subcommand with `--json` output.
- **Quick Text & Link Transfer**:
  - Instant, bidirectional short text and URL sharing with 64 KiB payload bounding and strict UTF-8 enforcement.
  - Safe link preview filtering (`http://` and `https://` only) with clipboard integration.
- **Desktop & CLI Integration**:
  - Single-instance application lock preventing port collisions and duplicate instances.
  - Linux desktop environment integration: Nautilus script (`Share with Flitnix`), KDE Dolphin service menu (`flitnix.desktop`), and standard XDG menu entry.
  - Headless server mode (`flitnix serve`) with interactive terminal approval loop.
  - Terminal file sending client (`flitnix send`) for scripting and automation.
- **Accessibility & Internationalization**:
  - Comprehensive keyboard navigation, focus management, ARIA landmarks, and live status announcements.
  - Modular internationalization framework with localization support.

### Changed
- Refactored download pipeline to stream file bytes immediately without pre-reading or blocking on complete archive construction.
- Enhanced transfer speed calculations with exponential moving average (EMA) smoothing for stable throughput and ETA display.
- Upgraded default package configuration to version 0.2.0 across Linux, macOS, and Windows distribution manifests.

### Security
- Sanitized public repository metadata, commit identities, and confidential advisory paths.
- Added path traversal sanitization and strict capability token verification across all API endpoints.

---

## [0.1.0] - 2026-10-28

Initial public release of Flitnix: fast, private, offline-first local file transfer between desktop and phone.

### Added
- **Local QR Pairing**: One-scan pairing with ephemeral cryptographic session tokens; no accounts or cloud relays required.
- **Mobile Web Client**: Pure browser-based transfer interface served directly by the desktop application. Supports iOS Safari, Android Chrome, Firefox, and other modern browsers without app store downloads.
- **Two-Way Transfers**: High-speed chunked file uploads from phone to desktop and direct downloads from desktop to phone.
- **Atomic Publication Reservation**: Two-phase reservation protocol ensuring that files committing to disk remain completed even if cancellation requests race with the disk write.
- **Session Teardown & Lifecycle Consistency**: Revocation and teardown clean up in-flight staging resources without leaving orphaned files or conflicting states.
- **Multi-Platform Network Discovery**: Automatic network adapter selection and Wi-Fi SSID discovery across Linux (`nmcli`), Windows (Native WLAN API), and macOS (`CoreWLAN`).
- **Dark Navy / Cyan Visual Identity**: Polished desktop UI built with Wails v2 and Svelte 5, and matching mobile interface.
- **Cross-Platform Packaging**:
  - Windows x64 NSIS installer (`.exe`)
  - Linux RPM (`.rpm`) and Debian (`.deb`) packages with desktop entry and hicolor icons
  - Linux manual installation archive (`.tar.gz`)
  - macOS Universal DMG and ZIP archives
- **Automated Installers**:
  - POSIX shell installer (`install.sh`) with distro auto-detection and SHA-256 checksum verification
  - Windows PowerShell installer (`install.ps1`) with architecture checks and SHA-256 validation
- **Automated CI/CD**:
  - GitHub Actions CI workflow executing Go tests, concurrency race detector, and frontend checks
  - Release workflow building cross-platform packages and publishing checksummed draft releases
- **Open Source Licensing**: Distributed under the authoritative Apache License 2.0 (`Apache-2.0`).
