# Changelog

All notable changes to Flitnix will be documented in this file.

The format is based on [Keep a Changelog](https://keepachangelog.com/en/1.1.0/),
and this project adheres to [Semantic Versioning](https://semver.org/spec/v2.0.0.html).

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
