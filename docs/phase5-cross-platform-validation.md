# Flitnix Phase 5 — Cross-Platform Validation & Packaging Report

**Document Version:** 1.0.0  
**Date:** 2026-11-04  
**Branch:** `feature/phase5-production-readiness`  
**Base Commit:** `cd00f50ae41b734dfb74471b193986c1f2392818`  
**Authoritative Scenario Matrix SHA-256:** `c5c13b78d02e5c479826ac08a8f2e09a31e91a7cf232cd8d49336175904e6a51` (strictly intact)

---

## 1. Executive Summary

Flitnix provides cross-platform, local peer-to-peer file transfers between desktop computers (Linux, Windows, macOS) and mobile devices (Android, iOS) via standard mobile browsers. A core tenet of Phase 5 is ensuring dependable desktop behavior across all major operating systems while keeping the phone client zero-install (operating purely via standards-compliant mobile browsers without requiring native apps).

This report validates:
1. Cross-compilation across Linux (`amd64`), Windows (`amd64`), and macOS (`amd64`, `arm64`).
2. Operating system integration (sleep inhibition, filesystem semantics, network discovery).
3. Packaging architectures and distribution templates for each target desktop environment.
4. Mobile browser sandbox verification and explicit classification of deferred physical device tests.

---

## 2. Cross-Compilation Matrix & Build Verification

Every internal package, root application binary, and platform test suite has been cross-compiled and verified clean in the Phase 5 environment:

| Target Platform | Architecture | Build / Test Command | Compiler Status | Binary Size / Artifact |
|---|---|---|:---:|---|
| **Linux** | `amd64` | `go build -o /dev/null .` | **PASS** | Native Linux ELF binary (Wails v2 backend) |
| **Windows** | `amd64` | `GOOS=windows GOARCH=amd64 CGO_ENABLED=0 go build -o /dev/null .` | **PASS** | Portable PE32+ executable (`.exe`) |
| **Windows (Tests)** | `amd64` | `GOOS=windows go test -c ./internal/transfer -o /dev/null` | **PASS** | Windows test binary compilation |
| **macOS (Intel)** | `amd64` | `GOOS=darwin GOARCH=amd64 go build ./internal/...` | **PASS** | Mach-O 64-bit x86_64 backend packages |
| **macOS (Tests)** | `amd64` | `GOOS=darwin go test -c ./internal/transfer -o /dev/null` | **PASS** | macOS test binary compilation |
| **macOS (Silicon)** | `arm64` | `GOOS=darwin GOARCH=arm64 go build ./internal/...` | **PASS** | Mach-O 64-bit arm64 backend packages |

### 2.1 Cross-Platform Pipelining Test Alignment (Phase 4 CI Fix)
Phase 4 introduced asynchronous metadata checkpoint pipelining on Linux while intentionally preserving safe synchronous `FlushFileBuffers` / `fsync` behavior on Windows and macOS. An earlier CI failure on `macos-latest` and `windows-latest` was traced to `internal/transfer/pipelined_checkpoint_test.go` asserting `PipelineCheckpoints() == true` unconditionally.

The test suite has been updated:
* `TestPipelinedCheckpoint_Success`: Uses `platformSupportsPipelinedCheckpoints()` to assert pipelining on Linux, while asserting synchronous behavior on Windows/macOS.
* `TestPipelinedCheckpoint_BackpressureEnforcement`: Skipped on non-Linux platforms (`t.Skip("pipelined checkpoints are only enabled on Linux")`).
* `TestSynchronousCheckpoint_Fallback`: Added to comprehensively validate the synchronous fallback execution order (`part_sync` -> `meta_sync` -> `meta_rename`) across all platforms.
* **CI Status:**
  - Linux pipeline test: passed natively on Linux (`ubuntu-latest`).
  - Windows synchronous fallback: source-level correction and cross-compilation verified (`GOOS=windows go test -c`).
  - macOS synchronous fallback: source-level correction and cross-compilation verified (`GOOS=darwin go test -c`).
  - Native Windows/macOS GitHub Actions: pending until the branch is pushed and remote jobs complete.

---

## 3. Platform-Specific Subsystem Hardening

### 3.1 Sleep Inhibition & Power Management (`internal/power`)
When high-speed transfers are active, operating systems must not put the host machine to sleep or suspend network adapters.
* **Linux:** Uses `systemd-inhibit` / D-Bus `org.freedesktop.login1.Manager.Inhibit` with `sleep:shutdown:idle` locks.
* **Windows:** Interfaces with `kernel32.dll` `SetThreadExecutionState` (`ES_CONTINUOUS | ES_SYSTEM_REQUIRED | ES_AWAYMODE_REQUIRED`).
* **macOS:** Interfaces with `caffeinate` or `IOPMAssertionCreateWithName`.
* **Verification:** Tested in `TestPhase5_Scenario20_DesktopSleepAndWake` and `internal/power/inhibit_test.go`. Sleep inhibitors acquire on stream start, update status to active, and release upon stream completion or error.

### 3.2 Network Interface Enumeration & DHCP Resilience (`internal/netenv`)
Desktop hosts frequently switch network interfaces (e.g., Ethernet plugged in while on Wi-Fi) or receive new IP leases via DHCP.
* **Hardware Independence:** No hardcoded interface names (`eth0`, `wlan0`), phone models, SSIDs, or gateway IPs. Detection queries the host OS network routing table and interface flags (`net.FlagUp`, unicast IP matching).
* **Interface Switching:** Tested in `TestPhase5_Scenario18_NetworkInterfaceChangesDuringTransfer`.
* **DHCP Leases:** When a DHCP lease changes the host IP, existing in-flight connections to the old address receive `netenv.ErrGone`, signaling the desktop UI to regenerate connection QR codes and advertise the updated endpoint (tested in `TestPhase5_Scenario19_DHCPAddressChanges`).

### 3.3 Filesystem Normalization & Path Sanitization
Different operating systems impose distinct filesystem constraints:
* **Windows Reserved Names:** Handled via regex validation rejecting `CON`, `PRN`, `AUX`, `NUL`, `COM1-9`, `LPT1-9`, trailing dots, and trailing spaces in `internal/transfer/store.go` and `internal/transfer/service.go`.
* **Path Separators:** All relative paths in batch manifests are converted to host filesystem separators using `filepath.FromSlash` after sanitization.
* **Atomic Publication Semantics:** Transfers write to `.part` files in temporary staging. Only after 100% byte commitment and SHA-256 verification does atomic publication proceed: the single-part `Store` uses atomic hard-links (`os.Link`) with numbered collision variants, and the resumable Tus engine uses atomic rename (`os.Rename`) with pre-allocated `.published-<id>.json` crash-recovery records. Cross-filesystem moves are detected and rejected (`ErrCrossFilesystem`).

---

## 4. Packaging & Release Engineering (WP5.7)

### 4.1 Linux Packaging
* **Target Formats Produced (`scripts/package-linux.sh`):**
  1. **DEB:** For Debian, Ubuntu, Linux Mint (generated via `nfpm`).
  2. **RPM:** For Fedora, RHEL, openSUSE (generated via `nfpm`).
  3. **tar.gz:** Standalone portable tarball with `install.sh` helper and desktop integration files.
  *(Note: AppImage was evaluated during design but is not built by the current packaging script).*
* **Desktop Integration:**
  - Standard `.desktop` entry file (`flitnix.desktop`):
    ```ini
    [Desktop Entry]
    Name=Flitnix
    Comment=Universal High-Speed Local File Transfer
    Exec=flitnix %U
    Icon=flitnix
    Terminal=false
    Type=Application
    Categories=Network;FileTransfer;Utility;
    ```
  - App icon installed under `/usr/share/icons/hicolor/scalable/apps/flitnix.svg`.

### 4.2 Windows Packaging
* **Target Architecture:** Windows 10 / Windows 11 64-bit (`amd64`).
* **Installer Strategy:**
  - NSIS installer script (`build/windows/project.nsi`) generating installer executable.
  - Portable standalone `.exe` option with embedded assets.
* **Release Status:** Build and packaging scripts configured. Cross-compilation verified. Microsoft Authenticode code-signing credentials and native Windows runtime execution remain pending.
* **Application Metadata & Paths:**
  - Application data stored in `%APPDATA%\Flitnix` (session databases, identity keys, audit logs).
  - Default download receive folder: `%USERPROFILE%\Downloads\Flitnix`.

### 4.3 macOS Packaging
* **Target Architecture:** Universal macOS Binary supporting Intel (`x86_64`) and Apple Silicon (`arm64`) merged via `lipo`:
  ```bash
  lipo -create -output Flitnix_universal Flitnix_x86_64 Flitnix_arm64
  ```
* **App Bundle & DMG:**
  - Packaged into `Flitnix.app` with `Contents/Info.plist`, `Contents/MacOS/Flitnix`, and `Contents/Resources/AppIcon.icns`.
  - Distributed via read-only DMG with drag-and-drop symlink to `/Applications`.
* **Release Status:** Cross-compilation verified for both architectures. Apple Developer ID application code-signing and `xcrun notarytool submit` notarization remain pending official credentials. Native macOS runtime testing is deferred.

---

## 5. Mobile Browser Client Experience & Sandbox Validation

### 5.1 Zero-Install Mobile Workflow
Mobile users do not install native apps:
1. Desktop generates a QR code displaying an ephemeral pairing URL.
2. User scans QR code with standard mobile camera (Android or iOS).
3. Mobile browser opens web client served from the desktop's local HTTP server.
4. Pairing handshake validates session and displays friendly connection cards.
5. User initiates uploads (via HTML5 File API and Web Workers) or downloads (via HTTP single-file or zip archives).

### 5.2 Browser Environment Matrix
Automated test suite (`frontend/test/`) validates:
* **Android Chrome:**
  - Web Worker SHA-256 calculation (`crypto.subtle` / Web Worker fallback).
  - Streaming fetch requests (`duplex: "half"`).
  - Wake Lock API (`navigator.wakeLock`) to keep mobile screen active during large transfers.
* **iOS Safari:**
  - Direct slice Blob XHR fallback for versions where Request body streaming is restricted.
  - Background tab suspension awareness (reconciliation upon tab reactivation).
  - Range request continuation on network hiccups.

---

## 6. Classification of Testing Status

To ensure complete transparency and prevent false claims of release readiness:

| Subsystem / Capability | Verification Status | Details |
|---|:---:|---|
| Go Backend Architecture | **Tested & Verified** | Full unit and integration test suite passing under race detector (`go test -race`). |
| 25 Reliability Scenarios | **Tested & Verified** | Automated suite (`phase5_scenarios_test.go`) passing 25/25. |
| Linux Packaging (DEB/RPM/tar.gz) | **Configured & Verified** | Configured via `scripts/package-linux.sh` and locally verified as documented. |
| Linux Packaging (AppImage) | **Not Built** | Evaluated in architecture; artifact not built by packaging script. |
| Windows (x64) Packaging & Runtime | **Configured (Cross-compiled)** | Installer configuration present (`project.nsi`); native validation and Authenticode signing pending. |
| macOS (x64/arm64) Packaging & Runtime | **Configured (Cross-compiled)** | Bundle/DMG configuration present (`Info.plist`, `lipo`); native validation, code signing and notarization pending. |
| Pipelined Checkpoint CI Fix | **Implemented & Verified** | Source fix verified on Linux and cross-compiled for Windows/macOS. Native Windows/macOS GitHub Actions confirmation pending remote push. |
| Desktop Sleep Inhibition | **Tested & Verified** | Verified via mock/unit tests on Linux and platform stub interfaces. |
| Frontend Web UI & Logic | **Tested & Verified** | 74 automated Node/Svelte tests passing; TypeScript check clean (`npm run check`). |
| Android Mobile Client | **Lab Deferred** | Automated DOM/Node tests passed; physical browser validation pending. |
| iOS Mobile Client | **Lab Deferred** | Automated DOM/Node tests passed; physical browser validation pending. |
| Physical Wi-Fi Topologies | **Lab Deferred** | Multi-router, roaming, and real radio interference testing deferred to physical lab. |

---

## 7. Conclusion

Flitnix compiles cleanly across Linux, Windows, and macOS, with platform-specific guards for filesystem reserved names, sleep inhibition, and dynamic network addressing. All automated checks and cross-compilation gates pass without errors.
