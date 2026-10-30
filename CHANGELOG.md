# Changelog

All notable changes to Flitnix will be documented in this file.

The format is based on [Keep a Changelog](https://keepachangelog.com/en/1.1.0/),
and this project adheres to [Semantic Versioning](https://semver.org/spec/v2.0.0.html).

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
