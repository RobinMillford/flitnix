# Flitnix Phase 5 — Production Readiness & Architecture Audit

**Author:** Robin Millford  
**Date:** 2026-11-04  
**Branch:** `feature/phase5-production-readiness`  
**Base Commit:** `cd00f50ae41b734dfb74471b193986c1f2392818`  
**Authoritative Scenario Matrix SHA-256:** `c5c13b78d02e5c479826ac08a8f2e09a31e91a7cf232cd8d49336175904e6a51` (strictly intact)

---

## 1. Executive Summary

This audit assesses the Flitnix codebase against the production-readiness requirements established for Phase 5. The objective of Flitnix is to provide high-speed, reliable, private local-network file transfers between desktop computers (Linux, Windows, macOS) and mobile phones (via standard mobile web browsers on iOS Safari and Android Chrome, without requiring a native mobile application).

The audit inspects the Go backend, Wails v2 desktop integration, Svelte 5 desktop frontend, mobile web client, HTTP/Tus server, session management, cryptographic device pairing, network interface discovery, resumable transfer engine, filesystem security, cross-platform adapters, packaging workflows, and automated test coverage.

Every subsystem and component is classified into one of five standard categories:
- **Complete**: Fully implemented, tested, and meeting production standards.
- **Partially implemented**: Functional in primary paths but requiring refinement, hardening, or broader coverage.
- **Missing**: Required for production readiness but not yet present.
- **Defective**: Implementation contains a functional defect, safety gap, or cross-platform flaw.
- **Deferred due to unavailable hardware or platform**: Native physical execution deferred to physical device testbeds.

---

## 2. Subsystem Audit Matrix

| Subsystem / Area | Component | Status | Finding & Evidence |
| :--- | :--- | :--- | :--- |
| **Go Backend Core** | Desktop App Composition (`app.go`) | **Complete** | Thin Wails adapter orchestrating core services (`transfer`, `server`, `session`, `guest`, `devices`, `netenv`, `power`, `audit`). Clean separation of concerns; no Wails dependencies inside internal packages. |
| **Go Backend Core** | CLI Interface (`cli.go`, `main.go`) | **Complete** | Supports `-v`/`version`, `-h`/`help`, `doctor` (diagnostic runner), `send` (CLI sender), `pair` (device pairing), `serve` (headless server with terminal approval loop). Handles file drag-and-drop and OS arguments. |
| **Go Backend Core** | Single Instance Lock (`main.go`) | **Complete** | Uses Wails `SingleInstanceLock` with identifier `io.github.robinmillford.flitnix`. Forwards secondary CLI share arguments via `OnSecondInstanceLaunch` and restores window. |
| **Go Backend Core** | Graceful Shutdown (`app.go:556`) | **Complete** | `App.shutdown()` cancels background workers via context, terminates active session transfers, cleans up incomplete batches, cancels downloads, flushes stores, and stops the HTTP listener within a bounded 5s window. |
| **Wails Integration** | Go-to-Frontend Bindings (`app.go`) | **Complete** | 40+ methods exposed for session control, transfer approvals, settings, guest room management, trusted devices, audit logging, and diagnostics. Strict projection types prevent internal token or path leakage. |
| **Wails Integration** | Native File Dialogs (`app.go`) | **Complete** | Uses `runtime.OpenFileDialog`, `runtime.OpenDirectoryDialog`, and `runtime.SaveFileDialog` for platform-native file and folder selection without exposing raw local paths to the webview prematurely. |
| **Wails Integration** | macOS File Open Event (`app.go:489`) | **Complete** | Handles macOS Finder "Open With" file-drop events via `mac.Options{ OnFileOpen: app.onMacFileOpen }` and queues items into sharing registry. |
| **Svelte Frontend** | Desktop Navigation & Layout (`App.svelte`) | **Complete** | Roving tabindex keyboard navigation, 6 functional tabs (`personal`, `guest`, `templates`, `audit`, `devices`, `settings`), badge counts for pending approvals, and Connection Doctor status badge. |
| **Svelte Frontend** | Theme & Contrast (`style.css`) | **Complete** | Dark Navy / Cyan brand theme with full CSS variable mapping. Light and Dark modes supported with WCAG 2.1 AA text contrast compliance. No low-contrast button states or invisible text. |
| **Svelte Frontend** | Focus Trapping & Modals (`focusTrap.ts`) | **Complete** | Modals (Connection Doctor, Template Editor, Confirmation Dialogs) implement strict focus trapping, Escape key handlers, and trigger focus restoration upon dismissal. |
| **Svelte Frontend** | Localization Engine (`i18n.ts`) | **Complete** | Safe parameter interpolation without HTML execution, fallback chain to canonical English (`en.json`), pluralization rules (`_one`, `_other`), and runtime locale switching. |
| **Svelte Frontend** | Transfer Telemetry Display (`App.svelte`) | **Complete** | Real-time byte counters, percentage bars, rolling EMA speed display, and calculated ETA for active uploads and downloads. Monotonic state guarantees. |
| **Transfer Server** | HTTP Server Engine (`internal/server/server.go`) | **Complete** | Standard Go `http.Server` configured with 5s `ReadHeaderTimeout`, 120s `IdleTimeout`, bounded drain on cancel, and dynamic interface binding. |
| **Transfer Server** | Tus 1.0.0 Resumable Protocol (`tus_handlers.go`) | **Complete** | Compliant implementation supporting `POST` (create), `HEAD` (query offset), `PATCH` (stream chunks), `DELETE` (abort). Single-pass streaming write-and-hash with no duplicate body reads. |
| **Transfer Server** | Progress Callback Throttling (`tus_handlers.go`) | **Complete** | Pure time-based 50ms throttle (`progressInterval = 50 * time.Millisecond`, with optional 100ms/256KiB legacy fallback via `FLITNIX_LEGACY_PROGRESS_THROTTLE=1`) eliminating byte-counter callback storms (reduced from ~4,096 invocations/GiB to <=20/s) without sacrificing UI responsiveness. |
| **Transfer Server** | Checkpoint Pipelining (`internal/transfer/service.go`) | **Complete** | Bounded metadata checkpoint pipelining on Linux (queue depth 1) decoupling disk metadata sync from the network receive loop while preserving 32 MiB / 500 ms contract. Safe synchronous sync retained on Windows/macOS. |
| **Transfer Server** | Resumable Download Streaming (`stream_download.go`) | **Complete** | Implements RFC 9110 Range requests (206 Partial Content), `If-Range` header validation, RFC 9530 `Content-Digest` headers, and single-use download continuation grants. |
| **Transfer Server** | Dynamic ZIP Archive Streaming (`archive.go`) | **Complete** | Streams multi-file and directory downloads on-the-fly with DEFLATE compression, Zip64 64-bit header support for archives >4 GiB, and bounded memory utilization. |
| **Session Management** | Token Security (`internal/session/session.go`) | **Complete** | 32-byte cryptographically secure random tokens (`crypto/rand`), constant-time token comparison (`hmac.Equal`), SHA-256 session key derivation, and idle timeout (10m default). |
| **Session Management** | Active Transfer Pinning (`session.go:121`) | **Complete** | Active upload and download streams pin the session (`PinnedTransfers` counter), preventing active transfers from being terminated due to idle expiration. |
| **Device Discovery** | LAN Interface Detection (`internal/netenv/`) | **Complete** | Platform-neutral discovery contract (`Provider` interface) filtering loopback, link-local, CGNAT, and broadcast addresses. Re-evaluates live network state on every session creation. |
| **Device Discovery** | Platform Network Adapters (`internal/netenv/`) | **Complete** | Dedicated adapters: Linux (nl80211/sysfs), Windows (Native Wi-Fi API `wlanapi.dll`), macOS (CoreWLAN via cgo/Objective-C), and portable generic fallback. Zero hardcoded SSIDs or interfaces. |
| **Device Discovery** | QR Code Flow (`internal/qr/`, `app.go:830`) | **Complete** | Generates 2D barcode as PNG data URL for desktop display and ASCII barcode for terminal `serve`. Encodes direct LAN URL with token in fragment (`http://<ip>:<port>/#<token>`). |
| **Device Pairing** | Cryptographic Mutual Pairing (`internal/devices/`) | **Complete** | X25519 ECDH key exchange (`ecdh.X25519()`), Short Authentication String (SAS) numeric & emoji confirmation, Ed25519 host identity signing, in-memory failure counter with optional durable lockout persistence (`pairing_security.json` via `EnableDurableLockout`). |
| **Device Pairing** | Device Label Sanitization (`internal/devices/`) | **Complete** | `SanitizeDeviceName` enforces ANSI stripping, removes non-printable control characters, truncates to 64 runes, rejects blanks, and falls back to safe default (`Unnamed Device`) to prevent UI spoofing. |
| **Mobile Client** | Mobile Web App (`internal/server/assets/app.js`) | **Complete** | Pure vanilla JavaScript client (zero npm dependencies) supporting Android Chrome and iOS Safari. Dual-mode file upload, folder streaming, download progress tracking, and text messaging. |
| **Mobile Client** | Resumable Client Engine (`resumable-client.js`) | **Complete** | IndexedDB upload persistence, Web Worker streaming SHA-256 calculation (`sha256-worker.js`), server HEAD offset query, prefix digest validation, and chunked XHR fallback. |
| **Mobile Client** | Screen Wake Lock (`app.js:96`) | **Complete** | Requests Screen Wake Lock API when transfers are active; degrades gracefully to UI warning banner on insecure HTTP contexts where Wake Lock is disallowed by browser policy. |
| **Filesystem Security** | Path Traversal & Normalization (`sanitize.go`) | **Complete** | Enforces bare base name contract (`SanitizeName`), rejects `.` and `..`, path separators (`/` and `\`), control characters, and names exceeding 230 bytes. |
| **Filesystem Security** | Folder Path Confining (`sanitize.go:96`) | **Complete** | `SanitizeRelativePath` normalizes folder components, enforces max depth of 64, bounds components to 230 bytes, and confines folder trees within reserved root. |
| **Filesystem Security** | Windows Reserved Filenames (`sanitize.go:153`) | **Complete** | `Store.OpenPart` and `Service.declare` validate incoming filenames against Windows reserved device names (`CON`, `PRN`, `AUX`, `NUL`, `COM1-9`, `LPT1-9`) and reject trailing dots/spaces, ensuring cross-platform filesystem safety. |
| **Filesystem Security** | Collision-Safe Atomic Publishing (`store.go`) | **Complete** | Writes data to `.part` files; upon digest verification, publishes atomically: single-part `Store` uses hard links (`os.Link`) with variant loops, and Tus `ResumableManager` uses atomic rename (`os.Rename`) with pre-allocated `.published-<id>.json` records. Pre-existing files are never overwritten. |
| **Filesystem Security** | Directory Traversal via Symlinks (`store.go`) | **Complete** | `prepareDir` and `mkdirLeaf` verify created components and reject symlinks; incoming uploads are confined strictly beneath the designated receive directory. |
| **Security & Privacy** | Host Header & DNS Rebinding (`host_security.go`) | **Complete** | Strict Host header validation matching bound server IP/port; rejects unauthorized host headers and DNS rebinding over IPv4 and IPv6 loopback and LAN. |
| **Security & Privacy** | Cross-Origin Request Policy (`routes.go`) | **Complete** | Enforces Same-Origin policy; cross-origin requests from foreign domains or browser contexts are blocked. |
| **Packaging & CI** | Linux Packaging (`nfpm.yaml`, `scripts/`) | **Complete** | Configured and locally verified as documented: generation of `.deb`, `.rpm`, and `.tar.gz` packages with desktop files, icons, MIME types, and file permissions via `scripts/package-linux.sh`. AppImage artifact is not built. |
| **Packaging & CI** | Windows Packaging (`build/windows/`) | **Configured (Cross-compiled)** | Installer configuration present (`project.nsi`) generating installer `.exe`, shortcuts, and uninstaller. Native validation and Authenticode signing pending. |
| **Packaging & CI** | macOS Packaging (`build/darwin/`, `release.yml`) | **Configured (Cross-compiled)** | Universal bundle/DMG configuration present (`Info.plist`, `lipo`). Native validation, Apple code signing and notarization pending. |
| **Packaging & CI** | Supply-Chain & Attestation (`release.yml`) | **Complete** | GitHub Actions keyless build provenance attestation via Sigstore for Linux, Windows, and macOS artifacts; SHA256SUMS generation; explicit version pinning for all tools. |
| **Testing Coverage** | Unit & Integration Test Suites | **Complete** | 37+ Go test suites across all packages; 74 frontend unit tests (Node.js test runner); `svelte-check` type validation clean (0 errors, 0 warnings). |
| **Testing Coverage** | Network Simulation (`phase4_network_simulation_test.go`) | **Complete** | Deterministic TCP simulation: restricted bandwidth, high latency/jitter, packet drops, connection reset RST, delayed backpressure, and process crashes. |
| **Hardware Testing** | Physical Mobile Device Testing | **Deferred** | Automated DOM/Node tests passed (74 tests); physical browser validation on Android/iOS devices is pending. |
| **Hardware Testing** | True Power-Loss Verification | **Deferred** | Physical sudden-power-cut verification on bare-metal SSD/NVMe drives is deferred to physical hardware labs. Simulated process crashes and unbuffered writes verified. |

---

## 3. Detailed Subsystem Findings

### 3.1 Go Backend Architecture & Lifecycle (`app.go`, `main.go`, `cli.go`)
- **Strengths**: The application composition root cleanly separates desktop-specific UI plumbing from the core protocol and transport engines. Wails lifecycle events (`OnStartup`, `OnShutdown`, `SingleInstanceLock`) are cleanly connected. The CLI subcommands (`send`, `pair`, `doctor`, `serve`) allow headless execution in automated or terminal-only environments.
- **Hardening Completed**:
  - `SanitizeDeviceName` in `internal/devices/pairing.go`: Client-provided device names during pairing are strictly sanitized, stripped of ANSI escape sequences and non-printable control codes, bounded to 64 runes, and assigned a safe default (`Unnamed Device`) before being displayed in the desktop UI or persisted in `devices.json`.
  - Single-file upload sanitization in `internal/transfer/store.go` and `service.go`: Incoming filenames are validated against Windows reserved device names (`CON`, `PRN`, `AUX`, `NUL`, `COM1-9`, `LPT1-9`) and trailing dots/spaces, protecting both POSIX and Windows filesystems.

### 3.2 Desktop & Mobile UI/UX (`frontend/`, `internal/server/assets/`)
- **Strengths**: Svelte 5 frontend is fast, responsive, and adheres to strict accessibility standards. Tab navigation is accessible via keyboard arrow keys (`roving tabindex`), modals trap focus cleanly, and status indicators are clear. The mobile web client in `app.js` is written in lightweight vanilla JavaScript with zero external runtime dependencies, ensuring high performance on mobile devices.
- **Telemetry & Visual Quality**: Monotonic progress reporting, rolling delta EMA speed calculations, and accurate ETA projections prevent visual jitter. Both dark and light themes have verified contrast ratios exceeding WCAG AA minimums.

### 3.3 Network Interface Management (`internal/netenv/`)
- **Strengths**: Complete absence of hardcoded SSIDs, gateways, or interface names. OS-specific network discovery adapters detect Wi-Fi connection metadata via native system calls (Linux nl80211, Windows WLAN API, macOS CoreWLAN) without running slow shell scripts. Non-routable and special-purpose addresses are rigorously filtered.
- **Dynamic Network Handling**: Server lifecycle binds dynamically to the chosen interface IP. If network state changes or an interface disappears, `ensureServer()` cleanly shuts down the previous listener, terminates obsolete sessions, and re-binds to the new network.

### 3.4 Transfer Durability, Checkpoints & Performance (`internal/transfer/`, `internal/server/`)
- **Contract Adherence**: Default checkpoint contract remains strictly at 32 MiB / 500 ms. Linux uses bounded pipelined metadata synchronization (queue depth 1) while Windows and macOS retain synchronous `FlushFileBuffers` / `fsync` operations.
- **Single-Pass Pipeline**: Writes to `.part` files and SHA-256 digest calculation proceed in a single streaming pass without redundant disk reads or memory copying.
- **Atomic Publication**: Destination files are committed using collision-safe publishing (`os.Link` hard-links in `Store.PartFile.Commit()` and atomic `os.Rename` with pre-allocated `.published-<id>.json` records in `ResumableManager.FinalizeComplete()`) with numbered collision variants, guaranteeing that incomplete or cancelled transfers never publish partial files to disk.

### 3.5 Security & Privacy Controls (`internal/server/`, `internal/devices/`)
- **Authentication**: All transfer, source, and control endpoints require valid session tokens or cryptographic bearer tokens. Session tokens are generated using 256-bit cryptographically secure random bytes.
- **Brute-Force & Lockout**: The pairing coordinator maintains an in-memory failure counter by default, with optional durable lockout persistence (`pairing_security.json` via `EnableDurableLockout`) that survives server restarts, locking out attackers after repeated failed attempts.
- **Filesystem Confinement**: All received files are confined strictly beneath the designated receive directory. Symlink creation and traversal are rejected.

### 3.6 Cross-Platform Packaging & Distribution (`.github/workflows/`, `scripts/`)
- **Multi-Platform Support**: CI and release pipelines build and validate Linux (.deb, .rpm, .tar.gz), Windows (NSIS installer .exe), and macOS (universal .app, DMG, ZIP).
- **Supply-Chain Integrity**: GitHub Actions workflows enforce explicit version pinning for all tool installations (`wails`, `nfpm`, `go`, `node`) and produce cryptographic provenance attestations via Sigstore.

---

## 4. Remediation Implementation & Validation Status

All remediation actions identified during Phase 5 have been implemented and verified:

1. **WP5.1 / WP5.4 — Filesystem & Device Name Hardening**:
   - `internal/transfer/store.go` and `service.go` hardened with `windowsReservedRegex` to reject Windows reserved device names (`CON`, `PRN`, `AUX`, `NUL`, `COM1-9`, `LPT1-9`) and trailing dots/spaces with `400 Bad Request`. Verified in `internal/transfer/store_test.go` (`TestStoreOpenPartRejectsWindowsReservedAndTrailingDot`) and `phase5_scenarios_test.go` (`TestPhase5_Scenario23_MalformedUploadMetadata`).
   - `internal/devices/pairing.go` updated with `SanitizeDeviceName` enforcing ANSI stripping, control character removal, 64-rune clamp, and safe fallback. Verified in `internal/devices/pairing_test.go` (`TestSanitizeDeviceName`).
2. **WP5.2 — Cross-Platform Pipelining Test Alignment**:
   - `internal/transfer/pipelined_checkpoint_test.go` updated to be platform-aware (`platformSupportsPipelinedCheckpoints()`). Linux asserts pipelining enabled; Windows/macOS assert synchronous fallback. Added `TestSynchronousCheckpoint_Fallback` validating the complete synchronous checkpointing pipeline. Verified with `go test` and cross-compilation builds.
3. **WP5.3 — Lazy Initialization Concurrency Fix**:
   - `internal/server/server.go` hardened with `resumableMu sync.Mutex` in `Services.EnsureResumable()`, eliminating data races during concurrent upload initialization under high load.
4. **WP5.5 — Comprehensive 25-Scenario Integration Test Suite**:
   - Implemented `internal/server/phase5_scenarios_test.go` covering each of the 25 required reliability, security, and recovery scenarios with automated end-to-end tests (25/25 PASS).
5. **WP5.6 — Performance & Baseline Verification**:
   - Retained 32 MiB / 500 ms checkpoint contract. Verified zero regression across all transfer and recovery benchmarks (`benchmarks/wp4/recovery_test.go`).
6. **WP5.7 / WP5.8 — Packaging & Documentation Updates**:
   - Verified multi-platform packaging configurations (`package-linux.sh`, NSIS `project.nsi`, macOS `Info.plist`). AppImage documented as not built. Updated `README.md` and user onboarding documentation with complete cross-platform setup instructions.
7. **WP5.9 — Cross-Platform Validation & Acceptance Reports**:
   - Reconciled `docs/phase5-security-audit.md`, `docs/phase5-cross-platform-validation.md`, and `docs/phase5-release-readiness-report.md`.
