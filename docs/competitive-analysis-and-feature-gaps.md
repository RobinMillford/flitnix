# Flitnix — Competitive Analysis, Codebase Audit & Feature Gap Discovery

---

## 1. Executive Summary

Flitnix (`v0.2.7.4`) already possesses an unusually strong engineering core for a young local-first file-transfer application: single-pass streaming I/O with bounded memory, atomic hard-link publication (`os.Link`) with non-destructive filename collision handling, inline SHA-256 verification (including RFC 9530 `Content-Digest` headers on downloads and deterministic two-pass ZIP streaming), a full mutual-commitment Ed25519/X25519 Trusted Devices backend (`internal/devices`), a differentiated **Guest Drop Box** workflow (`internal/guest`) with rosters/forms/ZIP export, a 7-point **Connection Doctor** (`internal/doctor`), OS sleep inhibition (`internal/power`), and a signed auto-updater (`internal/update`).

However, inspecting the actual implementation reveals **critical structural disconnects** between Flitnix's backend primitives and its everyday user experience:

1. **Zero-byte restart on interrupted transfers**: Despite older design docs (`docs/phase4-architecture-performance-assessment.md`, `docs/phase5-security-audit.md`) referencing `tus_handlers.go` and `resumable.go`, those files **do not exist** in `internal/server/` or `internal/transfer/`. Uploads (`internal/server/assets/app.js`, `internal/transfer/service.go`) strictly restart from byte zero on failure, and normal mutable downloads (`internal/server/archive.go`) send `Accept-Ranges: none`. For 10–100 GB files over Wi-Fi or mobile browsers that throttle background tabs, this is the #1 reliability blocker.
2. **Trusted Devices exist for CLI, not for the Mobile Browser**: `internal/devices` and `routes.go` implement Ed25519 + 6-digit SAS pairing and `AutoApprove`, and `TrustedDevicesView.svelte` tells users *"tap 'Pair Device' on your phone's browser when scanning the QR code"*, yet `internal/server/assets/app.js` contains **no pairing or device-auth client code**, and `/api/pair/*` sits behind `requireSession` (still requiring an ephemeral QR scan first).
3. **No automatic nearby discovery, one-click reconnect, or persistent transfer history**: Flitnix has no mDNS/Bonjour or UDP multicast discovery, no desktop-to-desktop discovery UI, no system tray / background daemon mode, and `TransferHistoryView.svelte` reads exclusively from **in-memory** arrays capped at 64 items (`RecentUploads`, `RecentDownloads`, `ListBatches`)—losing all personal transfer history on restart while `audit.jsonl` logs only Guest Drop Box events.
4. **Transport confidentiality gap on untrusted Wi-Fi**: `server.go` serves plain `http://` exclusively (`docs/TRANSPORT_ENCRYPTION_EVALUATION.md` documents the valid browser-warning trade-off for ad-hoc RFC 1918 IPs, yet competitors like LocalSend, KDE Connect, PairDrop, and Blip provide TLS/DTLS/E2EE—and Flitnix lacks even application-layer E2EE or TLS for CLI/desktop-to-desktop peers where certificate UX warnings do not apply).
5. **Polling instead of real-time push**: Contrary to the high-level description mentioning WebSockets, `internal/server/` uses **no WebSockets**—both desktop (`App.svelte` / `refreshScheduler.ts` polling every ~800 ms) and mobile (`app.js` polling batches every 1,200 ms, downloads every 500 ms, Quick Text every 2,500 ms) rely on HTTP polling.

**Bottom line:** Flitnix does not need to become a bloated remote-control suite like AirDroid or abandon its zero-install browser philosophy. By adding **resumable chunked transfers + HTTP Range downloads**, **mDNS/LAN auto-discovery + persistent Trusted Browser/Desktop reconnection**, **system tray + background receiving + native OS notifications**, **persistent personal transfer history + media previews**, and **WebSocket real-time state + E2EE for paired/CLI peers**, Flitnix can combine **AirDrop/LocalSend-level effortlessness** with **zero-install mobile browser reach** and **unmatched Guest Drop Box & integrity guarantees**.

---

## 2. Current Flitnix Capability Audit

Every claim below was verified directly against the source tree in the repository root.

### 2.1 Verification of the 25 Advertised / Documented Capabilities

| # | Capability | Status | Source Evidence & Behavioral Nuances |
|---|---|---|---|
| 1 | Bidirectional desktop ↔ phone browser transfers | **YES** | Phone→Desktop via `POST /api/batches` + `PUT /api/batches/{batchID}/items/{itemID}/content` ([routes.go](../internal/server/routes.go#L849-L1234)); Desktop→Phone via `POST /api/download-grants` + `GET /api/downloads/{id}/content` ([archive.go](../internal/server/archive.go#L91-L148)) and `GET /api/files/{id}/download` ([routes.go](../internal/server/routes.go#L1341-L1470)). |
| 2 | QR-code-based mobile pairing | **YES** | [qr.go](../internal/qr/qr.go#L43-L86) renders PNG (`DefaultSize = 288`) & terminal ASCII QR encoding `http://<ip>:43821/#<43-char-token>`. Fragment scrubbed immediately in [app.js](../internal/server/assets/app.js#L44-L49). |
| 3 | Entire folder & directory transfers | **YES** | Phone→Desktop uses `<input webkitdirectory directory>` ([index.html](../internal/server/assets/index.html#L72)) & `BatchKindFolder` ([batch.go](../internal/transfer/batch.go#L24)); Desktop→Phone enumerates files & empty dirs (`MaxSourceItems = 100000`, [source.go](../internal/transfer/source.go#L24-L155)) and streams uncompressed `zip.Store` ([archive.go](../internal/server/archive.go#L594-L622)). Note: Phone→Desktop folder upload skips empty directories (browser `webkitdirectory` only yields `File` objects). |
| 4 | Full-resolution photo & video transfers | **YES** | Raw `File` objects streamed untouched via `xhr.send(opts.file)` ([upload-worker.js](../internal/server/assets/upload-worker.js#L81), [upload-client.js](../internal/server/assets/upload-client.js#L145)); no transcoding or image resizing anywhere in backend/frontend. |
| 5 | Direct streaming without buffering whole files in RAM | **YES (with minor note)** | `streamBody` copies in 64 KiB chunks (`streamCopySize = 64 * 1024`, [stream.go](../internal/transfer/stream.go#L51-L123)); downloads use `CopyBufferSize = 256 * 1024` from `sync.Pool` ([pool.go](../internal/transfer/pool.go#L7-L28)). Note: `streamBody` allocates `make([]byte, 64*1024)` per call rather than using `GetCopyBuffer()`. |
| 6 | Dedicated upload Web Worker with fallback | **YES** | [upload-client.js](../internal/server/assets/upload-client.js) dispatches `PUT` XHR inside [upload-worker.js](../internal/server/assets/upload-worker.js) and falls back to `streamOnMainThread` if `Worker` fails. |
| 7 | Sequential transfer queues | **YES** | `Coordinator.ClaimItem` enforces single `session.active` lease per session ([coordinator.go](../internal/transfer/coordinator.go#L527-L548)); `app.js` `pump()` / `pollBatch()` uploads strictly one head item at a time ([app.js](../internal/server/assets/app.js#L808-L1000)). |
| 8 | Real-time transfer speed and ETA | **YES** | Phone uses EMA (`0.7 * prev + 0.3 * instant`) over `>= 0.25s` delta ([app.js](../internal/server/assets/app.js#L1085-L1096)); desktop computes speed in [service.go](../internal/transfer/service.go#L624-L629) & [download.go](../internal/transfer/download.go#L129-L131) and ETA via `calcEta` in [PersonalTransferView.svelte](../frontend/src/views/PersonalTransferView.svelte#L368). |
| 9 | SHA-256 integrity verification | **YES (asymmetric)** | Uploads hashed inline in `streamBody` ([stream.go](../internal/transfer/stream.go#L80-L122)); pre-declared digest enforced for CLI/Trusted sends (`ErrDigestMismatch`, [service.go](../internal/transfer/service.go#L653-L661)), whereas browser uploads do not pre-compute SHA-256 before sending. Outbound downloads emit `Trailer: Content-Digest, X-Content-SHA256` ([archive.go](../internal/server/archive.go#L761-L772)); browser verifies inline via `showSaveFilePicker` or manually via `verifyPickedFile` (`File.stream()` + `sha256.min.js`) ([app.js](../internal/server/assets/app.js#L1253-L1445)). |
| 10 | Atomic file publication | **YES** | `PartFile.Commit()` fsyncs, closes, then calls `os.Link` across up to 10,000 collision variants before `os.Remove(p.path)` ([store.go](../internal/transfer/store.go#L467-L510)); fails closed if filesystem rejects hard links (`isLinkUnsupported`). |
| 11 | Cancellation and safe cleanup | **YES** | Two-phase `ReserveCancel` vs `pubPublishing` mutex race protection ([service.go](../internal/transfer/service.go#L670-L770)), `Sink.Abort()` deletes `.part.*`, `DiscardFolderRoot` removes empty reserved folder roots ([app.go](../app.go#L1203-L1225)). |
| 12 | Collision-safe filenames | **YES** | `variantName(p.name, n)` produces `"file (1).ext"`, etc., up to 10,000 attempts ([store.go](../internal/transfer/store.go#L485-L509)); Guest rooms also offer `CollisionReject` vs `CollisionPreserveBoth` ([room.go](../internal/guest/room.go#L74-L77)). |
| 13 | Guest Drop Box for multiple uploaders | **YES** | Full subsystem in [internal/guest/room.go](../internal/guest/room.go) (`MaxGuestsPerRoom = 256`), presets, custom form schemas, participant rosters, deadlines, and ZIP export with manifest JSON/CSV ([export.go](../internal/guest/export.go)). |
| 14 | Individual guest approval | **YES** | `POST /guest-api/join` sets `GuestPending`, host calls `ApproveGuest` / `RejectGuest` / `RevokeGuest` ([app.go](../app.go#L1989-L2038)). |
| 15 | Individual incoming-file approval | **YES** | Personal single uploads (`ApproveUpload`/`RejectUpload`), personal batches (`ApproveBatch`/`RejectBatch`), and guest uploads (`ApproveGuestUpload`/`RejectGuestUpload`). |
| 16 | Isolated storage for each guest | **YES** | `ApproveGuest` creates an isolated `transfer.Store` at `<RoomFolder>/<guestID>-<safeSlug>/` ([room.go](../internal/guest/room.go#L655-L662)); `handleGuestUploadStatus` / `Content` enforce `up.GuestID == g.ID` ([guest_routes.go](../internal/server/guest_routes.go#L378)). |
| 17 | Bidirectional Quick Text & URL sharing | **YES** | `internal/textmsg/store.go` caps at 64 KiB (`MaxContentBytes`), 100 messages/session, strict UTF-8 & `http`/`https` extraction (`ExtractSafeURL`), polled every 2.5s on mobile ([app.js](../internal/server/assets/app.js#L1707-L1717)) and ~0.8s on desktop. |
| 18 | Connection Doctor (7 diagnostic checks) | **YES** | [doctor.go](../internal/doctor/doctor.go#L48-L370): `adapter_presence`, `ipv4_address`, `port_available`, `server_health`, `connection_medium`, `host_reachability`, `multihomed_routing`; UI modal + `flitnix doctor [--json]`. |
| 19 | IPv4 and IPv6 network support | **YES** | [netenv.go](../internal/netenv/netenv.go#L189-L487) enumerates IPv4 + IPv6 global/ULA, unmaps `::ffff:x.x.x.x`, excludes IPv6 link-local `fe80::/10` for QR, brackets `[ipv6]:43821`, validates bracketed Host headers ([routes.go](../internal/server/routes.go#L115-L262)). |
| 20 | Multiple network-interface selection | **YES** | Desktop adapter selector in [PersonalTransferView.svelte](../frontend/src/views/PersonalTransferView.svelte#L169-L180) + `PreferredInterface` persistence in [settings.go](../internal/settings/settings.go#L35) + live `netenv.Monitor` ([monitor.go](../internal/netenv/monitor.go)). |
| 21 | Ephemeral pairing tokens & auth | **YES** | 256-bit `crypto/rand` token (`TokenLen = 32`, 43-char base64url), SHA-256 keyed lookup, 10-min sliding idle timeout pinned during active streams ([session.go](../internal/session/session.go)). |
| 22 | CLI-based file transfer & management | **YES** | `flitnix [paths...]`, `send`, `pair`, `doctor`, `serve`, `version`, `help` ([cli.go](../cli.go#L40-L96), [send_cli.go](../send_cli.go), [pair_cli.go](../pair_cli.go)). |
| 23 | Light and dark themes | **YES** | `system`, `dark`, `light` persisted in `settings.json` and applied via `data-theme` ([App.svelte](../frontend/src/App.svelte#L60-L68), [SettingsView.svelte](../frontend/src/views/SettingsView.svelte#L224-L257)). Note: mobile web UI (`index.html`/`guest.html`) hardcodes dark color scheme (`<meta name="color-scheme" content="dark">`). |
| 24 | Keyboard and accessibility support | **YES** | Roving `tabindex` (`ArrowLeft`/`ArrowRight`/`Home`/`End` in `handleTabKeydown`, [App.svelte](../frontend/src/App.svelte#L131-L157)), `focusTrap.ts`, ARIA live announcements, `aria-valuenow` stepped progress bars, `prefers-reduced-motion`. |
| 25 | Automatic application update functionality | **YES (check/download/apply UI)** | [internal/update/service.go](../internal/update/service.go) queries GitHub Releases with ETag caching, verifies Ed25519 signature on `SHA256SUMS.txt` + SHA-256 of asset, and atomically swaps binary (portable Linux), launches NSIS `.exe` (Windows), or opens folder (deb/rpm/macOS). Note: check is triggered from UI/Settings (`UpdateModal.svelte`), not a silent background auto-updater. |

### 2.2 Investigation of Critical Architecture Questions & Hidden Gaps

1. **Upload Recovery & Resumability**:
   - **Verified Reality**: Uploads **restart from byte zero** on any interruption. `app.js:799` explicitly comments `// Fresh restart: start streaming from byte zero with a new transfer allocation`, clears `job.batchID = null; job.itemID = null; job.uploaded = 0`, and `service.go:637` calls `sink.Abort()` which deletes `.part.*`.
   - **Downloads**: `archive.go:818-843` supports `Accept-Ranges: bytes` *only* for Tier 1 OS-immutable files (`isOSImmutable`), whereas ordinary mutable files (`Tier 2`) and ZIP archives (`Tier 3`) explicitly set `Accept-Ranges: none` and ignore `Range` headers.
   - **Unused Recovery Journal**: `internal/recovery/store.go` implements an HMAC-authenticated WAL (`state.wal`), and `internal/server/recovery_startup.go` restores sessions and coordinator batches—**but neither `app.go` nor `cli.go` sets `server.Config.RecoveryDir`** (confirmed in [pairing_security.go](../internal/devices/pairing_security.go#L10-L12): *"no production entry point sets it"*).
2. **Persistent Trusted-Device Pairing**:
   - **Verified Reality**: **PARTIAL (Backend + CLI only; broken/missing for Mobile Web)**. `internal/devices` (`devices.go`, `pairing.go`, `sas.go`, `pairing_security.go`) and `routes.go` implement a rigorous 3-phase Ed25519 + X25519 6-digit SAS pairing protocol with `AutoApprove` and AuthV2 manifest signing. `flitnix pair` and `flitnix send` use it. **However**:
     - `internal/server/assets/app.js` has **zero** pairing, WebCrypto key storage, or `/api/auth/challenge` code (even though `TrustedDevicesView.svelte:182` instructs users to *"tap 'Pair Device' on your phone's browser"*).
     - All `/api/pair/*` and `/api/auth/challenge` routes sit inside `newAPIMux` behind `requireSession` ([routes.go](../internal/server/routes.go#L93,L330-L338)), so even a previously paired device cannot initiate a connection without first obtaining a fresh ephemeral QR Bearer token!
3. **Automatic Nearby Discovery**:
   - **Verified Reality**: **NO**. Grep across the entire codebase confirms zero mDNS/DNS-SD/Bonjour, UDP multicast, BLE, or Wi-Fi Direct discovery. Discovery is strictly manual QR scan, URL copy-paste, or `flitnix send --to <url>`.
4. **Persistent Transfer History**:
   - **Verified Reality**: **PARTIAL (Guest Audit only; Personal History is ephemeral)**. `TransferHistoryView.svelte` aggregates in-memory `RecentUploads` (max 64), `RecentDownloads` (max 64), and `ListBatches` (max 64 per session) — all wiped when Flitnix closes. Meanwhile `internal/audit/audit.go` (`audit.jsonl`, max 10,000 entries) persists on disk (`AuditHistoryView.svelte`), but `Audit` is only called in [app.go](../app.go#L1842-L2302) and [guest_routes.go](../internal/server/guest_routes.go#L163-L475) for Guest Drop Box events, **not** for Personal Transfers.
5. **Background Receiving & System Tray**:
   - **Verified Reality**: **NO**. `main.go` runs a standard Wails v2 window (`wails.Run`) with no system tray icon, no minimize/close-to-tray, no OS autostart registration, and no desktop native OS notifications (only in-app `Toast.svelte` and ARIA live text). Headless `flitnix serve` exists in CLI, but requires interactive terminal `[y/N]` stdin confirmation per batch (`runServeApprovalLoop`, [cli.go](../cli.go#L241-L329)).
6. **Encrypted Transport**:
   - **Verified Reality**: **NO**. `server.go:393` hardcodes `"http://" + addr.String()`. There is no TLS/HTTPS, no DTLS/WebRTC, and no application-layer payload encryption. On an untrusted or shared Wi-Fi network, file bytes, filenames, Quick Text notes, and session cookies (though `HttpOnly`, sent over plaintext HTTP) are readable by passive LAN/Wi-Fi sniffers, and an active MITM can intercept or inject payloads.
7. **WebSockets vs HTTP Polling**:
   - **Verified Reality**: **NO WebSockets** in Flitnix's own protocol (`internal/server/`). Real-time status relies on `setTimeout`/`setInterval` HTTP polling on mobile and 800 ms Wails IPC polling on desktop (`refreshScheduler.ts`).

---

## 3. Competitor-by-Competitor Analysis

### 3.1 LocalSend (`localsend/localsend`)
- **Overview & Website**: Free, open-source (MIT/Apache), zero-account Flutter/Dart + Rust (`rhttp`) cross-platform LAN sharing app (`localsend.org`). Recent releases (`v1.18.x`–`v1.19.x`) added native `.rpm` packages, streamlined CLI (`send`, `--text -`, `--to`), Rust `rhttp` for faster mobile-to-PC transfers, opt-in checksums, keyboard shortcuts for accept/decline, and custom Web Share HTML/drag-and-drop.
- **Platforms**: Windows, macOS, Linux (`.deb`, `.rpm`, AppImage, Flatpak, Snap), Android, iOS, plus built-in **Web Share** ("Share via link" / `web.localsend.org` browser fallback).
- **Free vs Paid / Local vs Cloud**: 100% free, no ads, no accounts, 100% local LAN/hotspot.
- **Standout Capabilities**:
  - UDP multicast / HTTP fallback auto-discovery with generated fun aliases + OS/device icons.
  - **Quick Save** & **Favorites** (auto-accept from trusted/pinned devices) + optional **PIN** code.
  - **Receive modes**: Off / Favorites Only / On (visibility control).
  - **System tray / menu bar** + autostart + "Start hidden" + Windows Explorer context menu (`v1.15.0+`).
  - Built-in **Transfer History** log and **Web Share** (browser download link without installing app on recipient).
  - **HTTPS with on-the-fly TLS certificates** between native apps (and plain HTTP fallback notice on Web Share to avoid browser cert warnings).
- **User Feedback & Weaknesses**:
  - Frequent GitHub issues around **lack of pause/resume** on dropped multi-GB transfers (e.g., `#2820`, `#3741`, and Web Share lacking `Accept-Ranges` for download managers).
  - **iOS background suspension** drops large transfers when screen locks (`#1468`, `#2610`, `#2621`).
  - **Multicast discovery failures** on VPN/TUN adapters or AP-isolated Wi-Fi, and **autostart/start-hidden bugs** on Linux AppImage/Flatpak/Windows (`#464`, `#1412`, `#1818`).
  - Requires app installation on both ends for normal bidirectional/upload flows (Web Share is primarily download-oriented unless using experimental web drop).

### 3.2 PairDrop (`schlagmichdoch/PairDrop`) & Snapdrop (`RobinLinus/snapdrop`)
- **Overview & Website**:
  - **Snapdrop**: Original WebRTC/WebSocket browser AirDrop clone (`snapdrop.net` was acquired by LimeWire in early 2025, triggering community migration to forks/mirrors like `snapdrop.me`).
  - **PairDrop**: Actively maintained open-source successor (`pairdrop.net`, `github.com/schlagmichdoch/PairDrop`, `v1.11.x` + active master PRs into 2026).
- **Platforms**: Pure browser / installable **PWA** across Windows, macOS, Linux, Android, iOS; self-hostable via Docker (`linuxserver/pairdrop`), Node.js, or `npx pairdrop`.
- **Free vs Paid / Local vs Cloud**: 100% free, open-source. Local discovery via WebSocket signaling server (public `pairdrop.net` or self-hosted) + WebRTC P2P; **Internet transfers** via **6-digit pairing / QR codes** and **temporary public rooms**, with optional **TURN relay** (`coturn`) when NAT traversal fails.
- **Standout Capabilities**:
  - **Zero-install browser-to-browser auto-discovery** on same LAN (when using signaling server).
  - **Persistent Paired Devices** (6-digit code / QR) that reconnect across *different* networks (VPN, cellular, Private Relay).
  - **Temporary Public Rooms** via short room codes.
  - **Web Share Target API** (PWA appears in Android/Windows Share Sheet) + right-click edit/auto-accept per paired device.
  - Recent master work: **OPFS Web Worker streaming** (eliminating browser RAM bottlenecks for multi-GB files), backpressure-paced WebRTC, clipboard paste shortcuts.
- **User Feedback & Weaknesses**:
  - **Requires internet / signaling server** unless user self-hosts a Docker signaling node on LAN (fails Flitnix's pure offline-first test out of the box on public instance).
  - **No pause/resume** on dropped WebRTC streams; **no persistent transfer history**; multi-file transfers bundle into `.zip` on receipt; WebRTC still vulnerable to browser background throttling on mobile.

### 3.3 LANDrop (`landrop.app`)
- **Overview & Website**: Cross-platform LAN sharing app (`v2.8.x`), formerly open source (`LANDrop/LANDrop`), later moved closed-source for newer builds.
- **Platforms**: Windows, macOS, Linux, Android, iOS, Android TV.
- **Free vs Paid / Local vs Cloud**: Free; primarily local LAN/hotspot, plus WebRTC discovery assist for nearby devices not on the exact same subnet config.
- **Standout Capabilities**: Clean minimal UI, **Trusted Devices** (auto-accept), original-quality photos/videos, encrypted transfers, Android TV support.
- **User Feedback & Weaknesses**: Strong community backlash over **closing source** and perceived slow/stagnant maintenance on GitHub; missing clipboard history; cannot clear history cleanly in UI; occasional Windows Public-network firewall discovery blocks and Linux tray quirks.

### 3.4 Apple AirDrop
- **Overview**: Native Apple OS sharing across iOS, iPadOS, macOS (iOS 17/18, macOS Sequoia).
- **Platforms**: Apple-only (iPhone, iPad, Mac, Apple Watch for NameDrop).
- **Free vs Paid / Local vs Cloud**: Built-in free; uses BLE discovery + peer-to-peer AWDL Wi-Fi (no router needed, encrypted TLS over AWDL), plus **iOS 17+ Internet continuation** via iCloud when devices move out of local Wi-Fi/BLE range.
- **Standout Capabilities**: Zero-friction native OS Share Sheet & Finder integration, **Everyone for 10 Minutes** anti-spam auto-revert, **Contacts Only / Receiving Off**, proximity trigger (**NameDrop** / bring-phones-together), seamless **internet fallback resume**, original quality (with optional format compatibility toggle).
- **User Feedback & Weaknesses**: Strictly locked to Apple devices (unless paired with recent Android Quick Share interop on supported Pixel/Galaxy phones); users still report occasional "Waiting..." hangs or dropped multi-GB video transfers when discovery glitches, and no granular transfer queue or browser fallback for Linux/Windows.

### 3.5 Google Quick Share & Samsung Quick Share
- **Overview**: Unified Android/Windows/ChromeOS sharing protocol (merged Nearby Share + Samsung Quick Share in 2024; 2025–2026 added **AirDrop interoperability** on Pixel 8a/9/10 and flagship Galaxy S24–S26 when Apple device is in "Everyone for 10 min").
- **Platforms**: Android, ChromeOS, Windows 10/11 (separate Google vs Samsung Windows apps). No Linux or macOS app; iOS supported via QR/link download or AirDrop interop on newer flagships.
- **Free vs Paid / Local vs Cloud**: Free. Local P2P via Bluetooth + Wi-Fi Direct/LAN; **QR code & link sharing** (`quickshare.google` / Samsung Cloud) uploads files to temporary cloud links (2-day expiry, file/daily caps) for cross-OS or remote recipients; **Private Sharing** (Samsung, DRM/expiry/block-save).
- **Standout Capabilities**: OS-level Share Sheet & Windows Explorer right-click integration, **visibility modes** (Your Devices / Contacts / Everyone / Hidden), **QR code link generation** (with screen brightness boost), automatic photo/video backup to Windows PC.
- **User Feedback & Weaknesses**: **Notorious Windows driver & hardware fragility** (requires specific Intel Wi-Fi `>=22.50.07` & Bluetooth `>=22.50.02` drivers; older Intel AC-8265 cards blocked; Wi-Fi/BT coexistence drops Bluetooth audio/mice); Google vs Samsung Windows app confusion; silent crashes; discovery drops from Windows adapter power management; no Linux/macOS support.

### 3.6 KDE Connect (`kdeconnect.kde.org`)
- **Overview**: Open-source (GPL) cross-device productivity & file-sharing bridge for KDE Plasma, Linux, Windows, macOS, Android, iOS.
- **Platforms**: Linux (first-class), Windows (Microsoft Store), macOS (ARM/Intel builds), Android, iOS.
- **Free vs Paid / Local vs Cloud**: 100% free, open-source, 100% local LAN (UDP broadcast/mDNS discovery + TCP/TLS with TOFU certificate pinning).
- **Standout Capabilities**:
  - **Automatic background discovery & trusted device pinning** (once paired, zero daily friction).
  - **Bidirectional automatic Clipboard Sync** (text) & **URL push** (auto-open or share links).
  - **Remote Filesystem Mount (SFTP)** — browse Android storage directly inside Dolphin/Finder/Explorer.
  - **Native OS integration**: Windows/Linux/macOS share/context menus, desktop notifications with interactive actions, **Find My Device** (ring phone), media controls, presentation remote, virtual touchpad/keyboard, SMS & notification sync, battery status cards.
- **User Feedback & Weaknesses**:
  - **Android Doze / battery drain** causes frequent silent disconnects unless battery optimization is disabled.
  - **iOS heavily crippled** by Apple background restrictions (crashes on large uploads `#1638`, no reliable background sync).
  - **No chunked resume** on large file drops; batch transfers occasionally stall on Windows; requires installing native app on phone; no Guest Drop Box or browser-only zero-install workflow.

### 3.7 AirDroid Personal (`airdroid.com`)
- **Overview**: Commercial cross-device phone management & file transfer suite (desktop apps, Android/iOS apps, plus `web.airdroid.com` and local IP `http://192.168.x.x:8888` "Lite Mode").
- **Platforms**: Windows, macOS, Android, iOS, Web (`web.airdroid.com`).
- **Free vs Paid / Local vs Cloud**: Freemium (**Free** has ads, 2-device limit, **200 MB/month** remote data cap, **30 MB** remote/web single-file cap, no free PC-to-phone folder transfer; **Premium** ~$3.99/mo or $29.99/yr lifts caps up to 1 GB/file remote & unlimited remote data). Local LAN mode allows unlimited local transfers (typically ~20 MB/s).
- **Standout Capabilities**:
  - **Gallery / Photo / Video preview & management** in desktop/web client (browse thumbnails, stream/preview before downloading, bulk export).
  - **Remote File Manager** (browse phone folders, rename/delete/upload/download).
  - **Clipboard sync**, SMS, notification mirroring, screen mirroring, remote camera.
- **User Feedback & Weaknesses**: Heavy **battery drain and RAM overhead** from persistent background daemons; **aggressive paywalls, ads, and nagging**; **privacy concerns** around mandatory account sign-in for full features and cloud-relayed remote traffic.

### 3.8 Send Anywhere (`send-anywhere.com`) & Other Peers (Blip, Croc, Warpinator, Syncthing)
- **Send Anywhere**:
  - **Platforms**: Windows, macOS, Linux, Android, iOS, Web, Browser Extensions.
  - **Free vs Paid / Local vs Cloud**: Free tier (with **heavy ads**, 1:1 **6-digit key** up to **50 GB** P2P/relay, temporary cloud **Share Links** capped at **10 GB/link**, **10 GB/month** download quota, fixed **48h expiry**, no password protection). Paid **Lite ($5.99/mo)** / **Standard ($9.99/mo)** raise link limits (20–30 GB), monthly quotas (200–500 GB), add password protection, custom expiry, and remove ads.
  - **Standout Capabilities**: Iconic **6-digit one-time transfer key** (effortless cross-device verbal/typed handoff), **Wi-Fi Direct** mode (Android), **Download-once / expiring links** (paid), **Send to nearby / My Devices**.
  - **Weaknesses**: Aggressive adware/pop-ups in free tier; cloud relay fallback raises privacy concerns; reports of stalled relays and confusing link vs key quality behavior.
- **Blip (`blip.net`)**:
  - Closed-source P2P transfer across Windows, macOS, Linux, Android, iOS (Free for personal use; ~$25–$30/mo Business). **Standout**: **mTLS over TLS 1.3**, **auto-resume across Wi-Fi drops, reboots, and unplugged drives**, direct folder transfer without zipping, 99+ TB scale, clipboard paste-to-send, local LAN auto-optimization. **Weakness**: Requires email account + 6-digit email verification + native app on every device (no zero-install web browser mode).
- **Croc (`schollz/croc`)**:
  - Open-source Go CLI tool. **Standout**: **PAKE (Password-Authenticated Key Exchange) E2EE** using human-readable code phrases, **full resumability** across interruptions, local + internet relay traversal, zero accounts. **Weakness**: CLI-only (though community GUIs exist), requires both sides to install Croc.
- **Warpinator / Syncthing**:
  - **Warpinator**: Linux Mint's LAN tool (mDNS + gRPC, group codes), plagued by firewall/Flatpak discovery bugs across OSes.
  - **Syncthing**: Best-in-class continuous **incremental folder synchronization** (block-level delta sync, versioning, local discovery + relay), but designed for persistent folder mirroring rather than ad-hoc one-off sharing.

---

## 4. Complete Feature Comparison Matrix

Statuses: **YES** (fully supported), **PARTIAL** (exists with significant limits/platform gaps), **NO** (not supported), **PLATFORM LIMITED** (only specific OS/hardware/browser), **UNKNOWN** (not publicly verifiable).

| Feature | Flitnix | LocalSend | AirDrop | Quick Share | KDE Connect | AirDroid | PairDrop | Send Anywhere | LANDrop |
|---|---|---|---|---|---|---|---|---|---|
| **Zero-Install Mobile Browser Client** | **YES** (`app.js`/`guest.js`) | **PARTIAL** (Web Share mainly download) | **NO** | **PARTIAL** (QR link download only) | **NO** | **PARTIAL** (Web client exists; mobile needs app) | **YES** (PWA/Browser) | **YES** (Web) | **NO** |
| **No Mandatory Account / Cloud** | **YES** | **YES** | **PLATFORM LIMITED** (iCloud for Contacts/Net resume) | **PARTIAL** (Optional; needed for Your Devices/Links) | **YES** | **NO** (Account required for full features) | **YES** | **PARTIAL** (Optional for 6-digit; required for links/history) | **YES** |
| **Offline LAN / Hotspot Operation** | **YES** | **YES** | **YES** (AWDL/BLE, routerless) | **YES** (BT + Wi-Fi Direct) | **YES** | **PARTIAL** (Local IP mode) | **PARTIAL** (Only if self-hosted signaling) | **PARTIAL** (Wi-Fi Direct on Android; relay needs net) | **YES** |
| **Auto Nearby Discovery (mDNS/UDP/BLE)** | **NO** | **YES** (Multicast/HTTP) | **YES** (BLE + AWDL) | **YES** (BLE + Wi-Fi) | **YES** (UDP/mDNS) | **PARTIAL** (Same account/LAN scan) | **YES** (LAN IP grouping via signaling) | **PARTIAL** (Nearby on mobile) | **YES** (UDP broadcast/WebRTC) |
| **QR Code Pairing / Transfer** | **YES** | **PARTIAL** (Web share QR) | **NO** (Proximity/NameDrop) | **YES** (QR cloud link) | **NO** | **YES** (QR web login) | **YES** (Room/Pair QR) | **YES** (QR for key/link) | **NO** |
| **6-Digit One-Time Code Pairing/Send** | **PARTIAL** (SAS in CLI `pair` only) | **PARTIAL** (Optional PIN) | **NO** | **NO** | **NO** | **NO** | **YES** (6-digit pairing code) | **YES** (6-digit key) | **NO** |
| **Persistent Trusted Devices & Auto-Approve** | **PARTIAL** (CLI/Backend `devices.go`; missing in mobile JS) | **YES** (Favorites + Quick Save) | **YES** (Contacts / Own Apple ID) | **YES** (Your Devices / Contacts) | **YES** (Paired devices) | **YES** (Bound devices) | **YES** (Persistent paired devices) | **PARTIAL** (My Devices) | **YES** (Trusted devices) |
| **Device Visibility Controls (Everyone/Trusted/Off)** | **NO** | **YES** (Off / Favorites / On) | **YES** (Off / Contacts / 10m Everyone) | **YES** (Hidden / Own / Contacts / Everyone) | **PARTIAL** (Visible while daemon active) | **PARTIAL** (Account bound) | **PARTIAL** (Public room toggle) | **PARTIAL** (Nearby toggle) | **PARTIAL** (Discoverable toggle) |
| **Encrypted Transport (TLS / DTLS / E2EE)** | **NO** (Plain HTTP) | **YES** (TLS on apps; HTTP on Web Share) | **YES** (TLS over AWDL) | **YES** (Encrypted P2P) | **YES** (TLS + cert pinning) | **PARTIAL** (HTTPS remote; HTTP local IP) | **YES** (WebRTC DTLS + WSS) | **YES** (256-bit) | **YES** (Encrypted LAN) |
| **Resumable Chunked Transfers (Upload & Download)** | **NO** (Uploads restart at 0; mutable DLs `Accept-Ranges: none`) | **NO** (Major GitHub request) | **PARTIAL** (iOS 17+ internet resume; local drops restart) | **PARTIAL** (Limited retry) | **NO** | **PARTIAL** (Desktop client retry) | **NO** | **PARTIAL** (Relay/app retry) | **NO** |
| **Folder Transfers (Without Pre-Zipping)** | **PARTIAL** (Upload raw files; Download streams ZIP) | **YES** (Native dir tree) | **YES** | **YES** (Desktop/Android) | **PARTIAL** (Folder/multi-file) | **PARTIAL** (Paid for PC→Phone folders) | **PARTIAL** (Multi-file auto-ZIP) | **YES** (App) | **YES** |
| **Empty Directory Preservation** | **PARTIAL** (Desktop→Phone ZIP yes; Phone→Desktop no) | **YES** | **YES** | **PARTIAL** | **NO** | **UNKNOWN** | **NO** | **PARTIAL** | **UNKNOWN** |
| **Transfer Concurrency / Queue Controls** | **PARTIAL** (Strict FIFO single active stream; no reorder/limit) | **YES** (Concurrent HTTP streams) | **YES** | **YES** | **PARTIAL** | **YES** | **PARTIAL** | **YES** | **YES** |
| **Integrity Verification (SHA-256)** | **YES** (Inline upload + RFC 9530 `Content-Digest`) | **PARTIAL** (Opt-in checksums) | **YES** (Internal protocol) | **YES** (Internal protocol) | **PARTIAL** (TLS integrity) | **UNKNOWN** | **PARTIAL** (WebRTC SCTP/DTLS) | **YES** (Internal check) | **YES** (Internal check) |
| **Multi-Guest Upload Room (Drop Box)** | **YES** (Rosters, forms, isolated folders, ZIP export) | **NO** | **NO** | **NO** | **NO** | **NO** | **PARTIAL** (Public rooms are many-to-many) | **PARTIAL** (Share links) | **NO** |
| **Persistent Transfer History** | **PARTIAL** (Guest `audit.jsonl` yes; Personal history RAM-only max 64) | **YES** (Persistent history log) | **NO** (No history log) | **PARTIAL** (Recent history in Windows/Android) | **NO** | **YES** | **NO** | **YES** (Transfer history) | **PARTIAL** (Basic UI list, hard to clear) |
| **Quick Text & URL Sharing** | **YES** (64 KiB, bidirectional, link guard) | **YES** (Text & clipboard send) | **PARTIAL** (URLs/Notes via Share Sheet) | **YES** (Text/links) | **YES** (Send text/URL + auto-open) | **YES** | **YES** (Text messages) | **PARTIAL** (Notes via text/clipboard) | **YES** (Text/clipboard) |
| **Automatic Clipboard Sync** | **NO** | **NO** | **YES** (Universal Clipboard) | **NO** | **YES** (Bidirectional sync) | **YES** | **NO** | **NO** | **NO** |
| **Media Thumbnails & Previews** | **NO** | **YES** (File type/image icons & previews) | **YES** (Inline photo/video preview) | **YES** | **NO** | **YES** (Full Gallery thumbnails/video player) | **YES** (Image preview in prompt) | **PARTIAL** | **PARTIAL** |
| **System Tray & Background Receiving** | **NO** (Window close quits; no tray) | **YES** (Tray + Start hidden) | **YES** (OS daemon) | **YES** (Background service) | **YES** (Background daemon + tray) | **YES** (Heavy daemon) | **PARTIAL** (Must keep tab/PWA open) | **YES** (Desktop tray) | **YES** (Tray; quirks on Linux) |
| **Start on OS Login (Autostart)** | **NO** (`AutoStartSession` only starts session on app launch) | **YES** | **YES** | **YES** | **YES** | **YES** | **NO** | **YES** | **YES** |
| **OS Native Desktop Notifications** | **NO** (In-app toast only) | **YES** | **YES** | **YES** | **YES** | **YES** | **PARTIAL** (Web Notifications API) | **YES** | **YES** |
| **OS File Manager Context Menus** | **PARTIAL** (Linux Nautilus/Dolphin + Windows NSIS reg; no macOS Finder/Share ext) | **YES** (Win/macOS/Linux) | **YES** (Finder/Share Sheet) | **YES** (Windows Explorer) | **YES** (Dolphin/Nautilus/Win) | **PARTIAL** | **PARTIAL** (PWA Web Share Target) | **YES** (Win/macOS) | **PARTIAL** |
| **Mobile Share Sheet / PWA Install** | **NO** (No PWA manifest/SW or Share Target) | **YES** (Native apps) | **YES** (Native) | **YES** (Native) | **YES** (Native) | **YES** (Native) | **YES** (PWA + Web Share Target) | **YES** (Native) | **YES** (Native) |
| **Connection Diagnostics (Doctor)** | **YES** (7 checks + CLI `--json`) | **PARTIAL** (Reachability icons) | **NO** | **NO** | **NO** | **NO** | **NO** | **NO** | **NO** |
| **CLI Transfer & Headless Server** | **YES** (`send`, `pair`, `serve`, `doctor`) | **YES** (CLI `send`, `--text -`) | **NO** | **NO** | **YES** (`kdeconnect-cli`) | **NO** | **PARTIAL** (`npx pairdrop`) | **NO** | **NO** |
| **Remote / Internet Relay Fallback** | **NO** (Strict LAN) | **NO** | **YES** (iCloud) | **YES** (Cloud link/QR) | **NO** | **YES** (Cloud, 200 MB free cap) | **YES** (WebRTC + TURN) | **YES** (Cloud relay/links) | **NO** |
| **Remote Phone File Browsing** | **NO** | **NO** | **NO** | **NO** | **YES** (SFTP mount) | **YES** (Web File Manager) | **NO** | **NO** | **NO** |
| **Original-Quality / Zero Ads** | **YES** | **YES** | **YES** | **YES** | **YES** | **NO** (Ads & free caps) | **YES** | **NO** (Ads & link caps) | **YES** |

*Competitor references: LocalSend (`github.com/localsend/localsend`, `localsend.org`), PairDrop (`github.com/schlagmichdoch/PairDrop`, `pairdrop.net`), Snapdrop (`snapdrop.net` / `github.com/RobinLinus/snapdrop`), LANDrop (`landrop.app`, `github.com/LANDrop/LANDrop`), AirDrop (`support.apple.com`), Quick Share (`android.com`, `support.google.com`, `samsung.com`), KDE Connect (`kdeconnect.kde.org`), AirDroid (`airdroid.com`), Send Anywhere (`send-anywhere.com`), Blip (`blip.net`), Croc (`github.com/schollz/croc`).*

---

## 5. List of Missing Features (Confirmed Absent in Source)

1. **Chunked Resumable Uploads** (`.part.*` is deleted on any drop/restart; no `PATCH`/offset-resume endpoint or chunk protocol).
2. **Resumable Byte-Range Downloads for Mutable Files & ZIPs** (`Accept-Ranges: none` in `archive.go` for Tier 2 & Tier 3).
3. **Automatic LAN Device Discovery** (mDNS/DNS-SD or UDP multicast discovery of other Flitnix desktops/CLI peers).
4. **Desktop-to-Desktop GUI Discovery & Sending** (Desktop UI only has a QR code for a phone/CLI to connect to *it*; there is no "Nearby Devices" picker to push files from Desktop A to Desktop B).
5. **6-Digit One-Time Transfer / Pairing Key** (for typing a quick code on another computer/phone without a camera or copying long URLs).
6. **System Tray / Menu Bar Integration & Minimize-to-Tray** (closing the Wails window terminates the app and kills active/listening sessions).
7. **OS Autostart on Login** (`AutoStartSession` only starts a session *after* the user manually opens Flitnix).
8. **Native OS Desktop Notifications** (no OS notification toast when a phone requests transfer approval, pairing, or finishes a download while Flitnix is behind other windows).
9. **Mobile PWA (`manifest.json`, Service Worker) & Web Share Target** (users cannot "Add to Home Screen" or share directly from Android Photos/Files into Flitnix's mobile page).
10. **Encrypted Transport (HTTPS/TLS or Application-Layer E2EE)** (plain HTTP everywhere; no TLS/mTLS even for CLI or Desktop-to-Desktop, and no E2EE envelope).
11. **Real-Time Push (WebSockets / SSE)** (everything uses 500–2500 ms HTTP polling).
12. **Automatic Retry with Exponential Backoff for Data Streams** (polling backs off, but interrupted `PUT` uploads immediately enter `"failed"` and require manual tap + byte-0 restart).
13. **Configurable Transfer Concurrency & Queue Reordering / Prioritization** (strict single-item FIFO queue; cannot reorder, pause/resume individual items, or enable 2–4 parallel streams for small files).
14. **Duplicate Detection & Conflict Policy in Personal Transfers** (Personal transfers always create `"file (1).ext"`; no Skip / Overwrite / Keep Both / Pre-transfer SHA-256 dedup option, unlike Guest Drop Box which at least has `reject` vs `preserve_both`).
15. **Modification Timestamp & Metadata Preservation** (received files get current OS `mtime` on commit; source `ModTime` is only preserved inside Desktop→Phone ZIP headers, not Phone→Desktop uploads).
16. **Media Thumbnails, Previews & Gallery Experience** (desktop Shared list, Approval Inbox, History, and mobile `Files on the desktop` show plain filenames and extensions only—no image/video/PDF thumbnails or lightboxes).
17. **Bandwidth Throttling & Scheduled/Deferred Transfers** (no rate limiter in `routes.go`/`stream.go` or transfer scheduler).
18. **Device Visibility Modes** (`Everyone` / `Trusted Devices Only` / `10-Minute Window` / `Off`).
19. **Opt-in Automatic Clipboard Synchronization** (Quick Text requires manual paste + Send).
20. **Optional Self-Hosted Relay / WebRTC Cross-Network Mode** (impossible to reach devices behind AP isolation or remote networks without VPN).
21. **macOS Finder / Share Extension & Windows Share Target** (macOS only has `Mac.OnFileOpen`; Windows only has NSIS Explorer right-click shell verb, not modern Windows 11 Share menu / multi-select IPC batching check).
22. **Storage Usage Dashboard & Auto-Cleanup Policies** (no visibility into total received GB, disk headroom bar, or auto-prune rules for old history/logs).
23. **Optional Consent-Based Media Compression / Format Compatibility** (e.g., opt-in HEIC→JPEG or video shrink on mobile before sending).
24. **Per-Device Transfer Permissions & Custom Receive Folders** (cannot restrict a paired device to send-only or route a device/MIME type into a specific subfolder).

---

## 6. List of Partially Implemented Features Needing Improvement

1. **Trusted Devices (`internal/devices` + `TrustedDevicesView.svelte`)**: Backend Ed25519 + X25519 SAS + `AutoApprove` is complete for CLI (`flitnix pair`, `flitnix send`), **but completely unimplemented in the mobile browser UI (`app.js`)**, and `/api/pair/*` + `/api/auth/challenge` require an active ephemeral QR session token first (defeating "zero-QR one-tap reconnect").
2. **Transfer History (`TransferHistoryView.svelte`)**: Polished UI with search, direction chips, status filter, and SHA-256 copy—**but backed only by volatile in-memory maps capped at 64 items** (`transfers.go`, `download.go`, `coordinator.go`), lacks date/device/file-type filters, lacks per-file "Open File" / "Reveal Exact File" / "Resend" / "Delete" / "Clear History", and lacks duration/speed stats. Personal transfers are never written to `audit.jsonl`.
3. **Crash Recovery Journal (`internal/recovery/store.go` & `recovery_startup.go`)**: Fully implemented HMAC-SHA256 WAL + startup reconciliation—**but disabled in production** (`RecoveryDir` is never passed in `app.go` or `cli.go`).
4. **Folder Upload & Empty Directory Preservation**: Desktop→Phone ZIP preserves empty directories (`source.go`), **but Phone→Desktop folder upload** (`<input webkitdirectory>`) skips empty directories, and there is no Desktop→Phone raw folder sync (always forces a `.zip` archive).
5. **Client-Side SHA-256 Integrity Verification**: Outbound downloads have `Content-Digest`, **but mobile browsers over plain HTTP cannot use `showSaveFilePicker`**, forcing users to manually tap *"Verify Downloaded File"* and re-pick the file from Downloads. Inbound browser uploads do not send an upfront SHA-256 digest (unlike CLI `PreComputeDigests`), so silent corruption before TCP write or partial-hash checks aren't caught upfront.
6. **File-Drop Desktop UX**: `main.go` enables `DragAndDrop` globally, and `PersonalTransferView.svelte` has a drop zone—**but it only adds files to "Shared Files"** (there is no multi-recipient target picker or drag-to-specific-device card).
7. **Theme & Localization**: Desktop has `system`/`dark`/`light` themes and `en.json` i18n framework; **mobile web pages (`index.html`, `guest.html`) are locked to dark mode** and hardcoded English strings.
8. **Auto-Updater (`internal/update`)**: Verifies Ed25519 + SHA-256 cleanly, **but requires manual modal opening** and for `.deb`/`.rpm`/`.dmg` only opens the download folder instead of offering background update checks or notification badges on launch.
9. **Memory Pooling Consistency**: `pool.go` defines `256 KiB` `sync.Pool`, used in `archive.go` and `serveFileDownload`, **but `streamBody` (`stream.go:81`) still allocates a fresh `64 KiB` slice per stream** and `archive.go` pre-read / two-pass behavior vs `streamBody` could be unified.
10. **Headless Server (`flitnix serve`)**: Works for CLI automation, **but lacks a `--auto-approve-trusted` / non-interactive daemon flag** and cannot expose a Trusted Device persistent endpoint without printing a new ephemeral token.

---

## 7. Top 32 Feature Opportunities (Detailed Gap Breakdown)

Scoring weights: **UV** (User Value, 30%) + **SA** (Strategic Alignment, 25%) + **CA** (Competitive Advantage, 20%) + **F** (Feasibility, 15%) + **RM** (Risk-adjusted Maintainability, 10%).

---

### Gap 1: Resumable Chunked Uploads & Crash-Safe Offset Recovery
1. **Feature Name**: Resumable Chunked Uploads & Offset Recovery
2. **Description**: Replace single-shot byte-0 upload retries with chunked/offset-resumable `PATCH` (or Tus-compatible) streaming plus `HEAD` offset probing, backed by `.part` + `.meta` checkpoints and `RecoveryDir` enabled in `app.go` and `cli.go`.
3. **Competitors**: Blip (auto-resume across drops/reboots), Croc (full resume), AirDrop (iOS 17+ internet resume). *LocalSend & PairDrop lack this—massive differentiator!*
4. **User Problem**: Uploading a 10–100 GB video or folder over Wi-Fi fails if the phone locks, switches APs, or hits a 30s stall, forcing a frustrating restart from 0%.
5. **Flitnix Status**: Missing in runtime (`app.js` restarts at byte 0; `internal/recovery/store.go` exists but is unwired in `app.go`/`cli.go`).
6. **Why It Improves Flitnix**: Makes multi-gigabyte mobile-to-desktop transfers reliable in real-world RF environments while beating LocalSend and PairDrop on core reliability.
7. **Expected User Impact**: Very High — eliminates the #1 pain point for large media transfers.
8. **Technical Complexity**: High
9. **Development Effort**: 3–4 weeks
10. **Security Implications**: Must bind upload offsets to authenticated session/item lease, enforce `offset == committedOffset` (no arbitrary sparse writes/overwrites), and cap concurrent resumable `.part` storage via disk-quota checks (`507 Insufficient Storage`).
11. **Backend Changes**: Add `internal/transfer/resumable.go` (checkpointed `.part` + `.meta` with pipelined/periodic `Sync`), `PATCH`/`HEAD` handlers in `internal/server/`, wire `RecoveryDir` in `app.go` & `cli.go`.
12. **Frontend Changes**: Show `"Paused / Resumable (4.2 / 12.0 GB)"` badge and Resume/Discard actions in `PersonalTransferView.svelte`.
13. **Mobile Changes**: Update `upload-worker.js` and `upload-client.js` to slice `File.slice(offset, offset + chunkSize)` (e.g., 16–32 MiB chunks), query `HEAD` on reconnect, and auto-retry with backoff from `committedOffset`.
14. **Testing**: Subprocess crash tests (`os.Exit(1)` at sync/rename boundaries), TCP RST proxy drop-and-resume tests, race detector tests, 10–50 GB soak tests.
15. **Dependencies**: Automatic Retry with Backoff (Gap 7) pairs tightly with this.
- **Scores**: UV: 5, SA: 5, CA: 5, F: 3, RM: 3 → **Weighted Score: 4.50 (P0)**

### Gap 2: Resumable Byte-Range Downloads (`Accept-Ranges: bytes`) for Mutable Files
1. **Feature Name**: Resumable Byte-Range Downloads (`HTTP 206`)
2. **Description**: Support `Range` / `If-Range` (`ETag` backed by file size + `mtime` + inode/quick-fingerprint) for normal desktop-shared single files (`Tier 2` in `archive.go`), so mobile/desktop browsers and download managers can pause and resume large downloads.
3. **Competitors**: Standard web servers, Blip, Send Anywhere (relay), requested heavily in LocalSend `#3741`.
4. **User Problem**: Currently `archive.go` returns `Accept-Ranges: none` for all mutable files; a 15 GB movie download that hiccups at 90% on Safari/Chrome fails completely.
5. **Flitnix Status**: Partial (`Tier 1` immutable files support `http.ServeContent` ranges in `archive.go:818-840`, but normal files hit `Tier 2` with `Accept-Ranges: none`).
6. **Why It Improves Flitnix**: Enables reliable 10–100 GB Desktop→Phone downloads using the phone's native download manager.
7. **Expected User Impact**: High — prevents failed multi-GB downloads on mobile browsers.
8. **Technical Complexity**: Medium
9. **Development Effort**: 1 week
10. **Security Implications**: Must validate `If-Range` / `ETag` (`size + mtimeNs`) and abort (`panic(http.ErrAbortHandler)` / close) if source file mutates mid-download so a client never stitches two versions of a file together.
11. **Backend Changes**: Update `archive.go` Tier 2 & `serveFileDownload` (`routes.go`) to validate `If-Range`, serve `206 Partial Content` via `http.ServeContent` wrapped in `progressSeeker` + `stallWriter`, and compute `Content-Digest` on full `200` streams.
12. **Frontend Changes**: Update `DownloadView` in `PersonalTransferView.svelte` to aggregate resumed range bytes cleanly instead of resetting to 0.
13. **Mobile Changes**: Handle `206` in `showSaveFilePicker` stream writer and show accurate resume state in Download Integrity card.
14. **Testing**: `download_range_test.go` cases for Tier 2 `Range`, `If-Range` mismatch after file edit (returns `200` fresh), and `416 Range Not Satisfiable`.
15. **Dependencies**: None.
- **Scores**: UV: 5, SA: 5, CA: 4, F: 4, RM: 4 → **Weighted Score: 4.55 (P0)**

### Gap 3: Automatic Nearby Device Discovery (mDNS / UDP Multicast)
1. **Feature Name**: Zero-Config LAN Discovery (mDNS / UDP Multicast)
2. **Description**: Broadcast and discover running Flitnix desktop/CLI instances on the local subnet via mDNS (`_flitnix._tcp.local.`) and UDP multicast fallback, showing a live "Nearby Devices" radar/list with device name, OS icon, and trust badge.
3. **Competitors**: LocalSend, AirDrop, Quick Share, KDE Connect, LANDrop, Warpinator.
4. **User Problem**: Users with two laptops (or desktop + Linux handheld/Steam Deck) currently have to manually copy-paste a long `http://ip:43821/#token` URL into `flitnix send`.
5. **Flitnix Status**: Missing.
6. **Why It Improves Flitnix**: Transforms Flitnix from "desktop-to-phone only" into an effortless all-device LAN mesh comparable to LocalSend and AirDrop.
7. **Expected User Impact**: Very High — removes manual IP/URL copy-paste between computers and enables instant peer selection.
8. **Technical Complexity**: Medium
9. **Development Effort**: 2 weeks
10. **Security Implications**: Discovery packets must **never** carry session tokens or sensitive metadata—only device display name (sanitized via `SanitizeDeviceName`), OS type, port, protocol version, and public HostID fingerprint (`devices.HostIdentity`). Must support visibility modes (Off / Trusted Only / Everyone) and bind strictly to the active interface (`netenv`), not VPNs.
11. **Backend Changes**: New `internal/discovery` package (mDNS + UDP multicast announcer/listener bound to selected `netenv.View` IP), `ListNearbyDevices()` Wails method, `GET /api/nearby` or desktop client sender flow using `internal/client`.
12. **Frontend Changes**: Add "Nearby Flitnix Devices" strip/cards in `PersonalTransferView.svelte` with click-to-send and visibility pill.
13. **Mobile Changes**: None required (mobile browsers cannot listen on raw UDP/mDNS, so mobile continues via QR/6-digit code/bookmarks; optionally show friendly desktop name).
14. **Testing**: Unit tests with mock multicast transport, multi-homed VPN exclusion tests, discovery timeout/stale-peer eviction tests.
15. **Dependencies**: Desktop-to-Desktop GUI Sender (Gap 6), Device Visibility Controls (Gap 14).
- **Scores**: UV: 5, SA: 5, CA: 4, F: 4, RM: 4 → **Weighted Score: 4.55 (P0)**

### Gap 4: Mobile Browser Trusted-Device Pairing & One-Tap Reconnect
1. **Feature Name**: Mobile Web Trusted-Device Pairing & Key-Backed Reconnect
2. **Description**: Implement the missing client-side WebCrypto/Ed25519 keypair + IndexedDB persistence in `app.js` to complete the existing `internal/devices` SAS pairing flow, plus a dedicated `/auth/device` token-exchange endpoint so a paired phone bookmark reconnects in one tap *without* scanning a new QR code.
3. **Competitors**: PairDrop (Persistent Paired Devices), LocalSend (Favorites + Quick Save), KDE Connect.
4. **User Problem**: Right now, users must scan a new QR code every single time they open Flitnix, and `TrustedDevicesView.svelte` advertises mobile browser pairing that doesn't exist in `app.js`.
5. **Flitnix Status**: Partial (`internal/devices` + `/api/pair/*` + `TrustedDevicesView.svelte` exist, but `app.js` has 0 lines of pairing code and `/api/auth/challenge` requires an existing session token!).
6. **Why It Improves Flitnix**: Fulfills an already-built & advertised security subsystem and gives returning phone users AirDrop-like one-tap reconnection via home-screen bookmark.
7. **Expected User Impact**: Very High — eliminates daily QR scanning for personal phones/tablets.
8. **Technical Complexity**: Medium-High
9. **Development Effort**: 2 weeks
10. **Security Implications**: **Critical detail**: `crypto.subtle` Ed25519 on plain HTTP (`http://192.168.x.x`, non-secure context) is **blocked by browsers** (`window.crypto.subtle` is `undefined` outside HTTPS/localhost!). Solution: bundle a lightweight, audited pure-JS Ed25519 + X25519 library (e.g., `@noble/ed25519` / `tweetnacl` embedded in `/static/`), store private key in `IndexedDB` (or prompt passphrase export), and enforce SAS 6-digit visual confirmation + `AutoApprove` toggle on desktop. Expose `POST /api/device-sessions` (outside `requireSession`, rate-limited + challenge-response signed by enrolled Ed25519 key) to mint a scoped session token for an already-enrolled device!
11. **Backend Changes**: Add unauthenticated-session challenge-response device login (`POST /api/device-sessions/challenge` + `/verify`) in `routes.go` backed by `devices.Store` + rate limiter; pass `OnPairingPrompt` callback in `app.go` to surface toast/modal anywhere in desktop UI.
12. **Frontend Changes**: Global pairing approval modal (not buried only inside `TrustedDevicesView.svelte`), device status badge.
13. **Mobile Changes**: Add "Pair This Device" banner/button in `index.html`/`app.js`, SAS verification modal, IndexedDB key storage, and "Auto-reconnect to <PC Name>" flow when opened from bookmark without `#token`.
14. **Testing**: Cross-browser (`tweetnacl`/Ed25519) vector tests against Go `internal/devices`, brute-force lockout tests on `/api/device-sessions`, revocation tests.
15. **Dependencies**: Mobile PWA / Home Screen Shortcut (Gap 11) makes bookmark reconnection feel like a native app.
- **Scores**: UV: 5, SA: 5, CA: 4, F: 4, RM: 3 → **Weighted Score: 4.45 (P0)**

### Gap 5: System Tray / Menu Bar, Background Receiving & Autostart
1. **Feature Name**: System Tray Daemon Mode, Background Receiving & OS Autostart
2. **Description**: Run Flitnix unobtrusively in the system tray / macOS menu bar with "Minimize/Close to Tray", optional "Start on Login" across Windows/macOS/Linux, and automatic background session/listener readiness for trusted devices.
3. **Competitors**: LocalSend, KDE Connect, Blip, AirDroid, Quick Share, LANDrop, Send Anywhere.
4. **User Problem**: Currently closing the Flitnix window terminates the server (`shutdown`), and Flitnix doesn't start on boot—making it unavailable when you casually want to beam a photo from your phone without walking over to launch the desktop app first.
5. **Flitnix Status**: Missing (`main.go` closes on window exit; `AutoStartSession` only triggers after manual app launch).
6. **Why It Improves Flitnix**: Turns Flitnix into an always-ready local drop target like AirDrop/LocalSend instead of an app you have to open on both ends every time.
7. **Expected User Impact**: Very High — essential for daily-driver adoption.
8. **Technical Complexity**: Medium
9. **Development Effort**: 2 weeks
10. **Security Implications**: When running in background, unsolicited connections must remain gated by explicit approval (or strictly limited to `AutoApprove` trusted devices + optional Visibility = Off/Trusted Only), and idle power must stay near zero.
11. **Backend Changes**: Integrate Wails v2 tray / `fyne.io/systray` (or `wails` HideWindowOnClose + native tray menu), add `internal/autostart` (Windows Registry `HKCU\...\Run`, macOS `LaunchAgent` plist, Linux `~/.config/autostart/flitnix.desktop`), and extend `settings.Settings` with `MinimizeToTray` and `LaunchOnLogin`.
12. **Frontend Changes**: Add "Run in System Tray" and "Launch at Login" toggles in `SettingsView.svelte` General tab.
13. **Mobile Changes**: None.
14. **Testing**: Windows/Linux/macOS lifecycle tests (hide/restore window without dropping active HTTP transfers; single-instance focus restoration via `onSecondInstanceLaunch`).
15. **Dependencies**: Native OS Notifications (Gap 8) so background approval requests surface immediately!
- **Scores**: UV: 5, SA: 5, CA: 3, F: 4, RM: 3 → **Weighted Score: 4.25 (P0)**

### Gap 6: Persistent Personal Transfer History & Actionable File Management
1. **Feature Name**: Persistent Personal Transfer History & File Actions
2. **Description**: Persist all Personal Transfer uploads, downloads, and batches in a bounded local store (`history.jsonl` or SQLite/WAL alongside `audit.jsonl`), with device attribution, duration/avg speed stats, search/date/device filters, and per-item actions: **Open File**, **Reveal Exact File in File Manager**, **Reshare to Phone**, and **Delete/Clear History**.
3. **Competitors**: LocalSend (persistent history), AirDroid, Send Anywhere.
4. **User Problem**: Right now `TransferHistoryView.svelte` loses every personal transfer on app restart, caps at 64 items, only opens the top-level folder (not the file), and cannot resend or clear records.
5. **Flitnix Status**: Partial (`TransferHistoryView.svelte` exists with search/direction/status filters, but reads ephemeral in-memory registries).
6. **Why It Improves Flitnix**: Makes the existing History tab genuinely useful as a durable ledger and quick-resend hub.
7. **Expected User Impact**: High — solves "where did that file I received yesterday go?"
8. **Technical Complexity**: Low-Medium
9. **Development Effort**: 1–1.5 weeks
10. **Security Implications**: Never persist raw session tokens or sensitive file contents; enforce `withinDir` checks before OS "Open/Reveal/Delete" calls; keep private `0600` file permissions; provide "Clear History" and auto-retention cap (e.g., 90 days / 5,000 items).
11. **Backend Changes**: Extend `internal/audit` or add `internal/history` JSONL store recording completed/failed personal uploads, batches, and downloads with `DeviceName`, `DurationMs`, `AvgSpeedBps`, `PublishedRelPath`; expose `QueryTransferHistory`, `ClearTransferHistory`, `OpenHistoryFile`, `RevealHistoryFile`, `ReshareHistoryFile` in `app.go`.
12. **Frontend Changes**: Wire `TransferHistoryView.svelte` to persistent backend query, add device/date filters, summary stats bar (total sent/received), and per-row action buttons.
13. **Mobile Changes**: Optionally show session-local history summary.
14. **Testing**: Persistence across simulated restarts, path-confinement tests on Reveal/Delete, pagination/filtering tests.
15. **Dependencies**: None.
- **Scores**: UV: 4, SA: 5, CA: 3, F: 5, RM: 5 → **Weighted Score: 4.30 (P0)**

### Gap 7: Automatic Stream Retry with Exponential Backoff & Network Roaming Recovery
1. **Feature Name**: Automatic Data-Stream Retry & Wi-Fi Roaming Recovery
2. **Description**: When a `PUT` upload or download stream drops due to transient Wi-Fi roaming, AP handoff, or socket reset (`network` / `timeout`), automatically pause, probe `/ping`, re-query `HEAD` committed offset (with Gap 1), and resume with exponential backoff (e.g., 1s, 2s, 4s, 8s) instead of immediately failing the whole batch (`FailBatch`).
3. **Competitors**: Blip, Croc, AirDrop.
4. **User Problem**: Walking between rooms or a 2-second Wi-Fi blip currently fails an entire multi-file batch (`FailBatch` cancels remaining items in `routes.go:1241`).
5. **Flitnix Status**: Partial (`app.js` retries control-plane *polling* with `backoffMs`, and `netenv.Monitor` detects desktop IP changes, but data streams abort the entire batch on first socket error).
6. **Why It Improves Flitnix**: Directly addresses the "Physical RF Wi-Fi & AP Handoff" gap noted in `README.md:134`.
7. **Expected User Impact**: High — transfers survive brief Wi-Fi dropouts transparently.
8. **Technical Complexity**: Medium
9. **Development Effort**: 1.5 weeks
10. **Security Implications**: Ensure failed-item retry preserves batch/item lease validation and does not bypass session expiration or device revocation checks.
11. **Backend Changes**: Add `ItemStatePaused` / soft-retry grace window in `coordinator.go` (do not immediately cancel all sibling batch items on transient stream EOF/reset).
12. **Frontend Changes**: Show `"Reconnecting (attempt 2/5)…"` state in Active Transfers dock.
13. **Mobile Changes**: Add retry state machine around `FlitnixUpload.stream` in `app.js` + `online` event listener wakeup.
14. **Testing**: Extend `phase4_streaming_network_simulation_test.go` TCP RST proxy tests to verify multi-file batch auto-recovery without manual button clicks.
15. **Dependencies**: Resumable Chunked Uploads (Gap 1) maximizes value; partial benefit even for small files if batch isn't torn down.
- **Scores**: UV: 5, SA: 5, CA: 4, F: 3, RM: 3 → **Weighted Score: 4.30 (P0)**

### Gap 8: Native OS Desktop Notifications & Interactive Approval Actions
1. **Feature Name**: Native OS Desktop Notifications (with Quick Approve/Open)
2. **Description**: Trigger native Windows Toast, macOS UserNotification, and Linux D-Bus `org.freedesktop.Notifications` alerts when a phone requests transfer/pairing approval or completes a transfer while Flitnix is unfocused or minimized in the tray.
3. **Competitors**: LocalSend, KDE Connect, AirDrop, Quick Share, AirDroid, Send Anywhere.
4. **User Problem**: Currently, if Flitnix is behind a browser or editor, the user has no idea their phone is stuck on *"Waiting for approval on the desktop…"* (`Toast.svelte` is invisible when minimized).
5. **Flitnix Status**: Missing (`go.mod` actually has `go-toast/v2` and `godbus/dbus/v5` as indirect dependencies from Wails/power, yet neither is used for user notifications!).
6. **Why It Improves Flitnix**: Essential companion to System Tray mode and drastically cuts approval wait times.
7. **Expected User Impact**: High — eliminates "why isn't my upload starting?" confusion.
8. **Technical Complexity**: Low-Medium
9. **Development Effort**: 1 week
10. **Security Implications**: Sanitize filenames/device names before OS notification rendering (already sanitized via `SanitizeName`/`SanitizeDeviceName`); optionally allow hiding filenames on lock screens; never include tokens.
11. **Backend Changes**: New `internal/notify` package using D-Bus (`godbus/dbus/v5` already in `go.mod`!) on Linux, `go-toast/v2` (already indirect in `go.mod`!) on Windows, and `osascript`/UNUserNotificationCenter on macOS; invoke on `OnBatchPending`, `OnPairingPrompt`, and completion.
12. **Frontend Changes**: Add "Desktop Notifications" toggle & "Quiet / Do Not Disturb" option in `SettingsView.svelte`.
13. **Mobile Changes**: Optional Web Notification API prompt (when permitted) when desktop shares a new file or approves a batch.
14. **Testing**: Interface-mocked unit tests + headless/display-safe no-op fallback tests.
15. **Dependencies**: Pairs with System Tray (Gap 5).
- **Scores**: UV: 4, SA: 5, CA: 3, F: 5, RM: 4 → **Weighted Score: 4.20 (P0)**

### Gap 9: Desktop-to-Desktop Direct Transfer Workflow (GUI Client Mode)
1. **Feature Name**: Desktop-to-Desktop Direct Transfer (Send Tab / Nearby Peer Picker)
2. **Description**: Expose `internal/client` inside the Wails desktop GUI so a user on Desktop A can click a discovered Flitnix peer (or paste a 6-digit code / URL) and send files/folders directly from the GUI without dropping to the terminal `flitnix send`.
3. **Competitors**: LocalSend, LANDrop, PairDrop, AirDrop, Quick Share, Blip, Send Anywhere.
4. **User Problem**: Desktop-to-desktop transfers require CLI (`flitnix send`); non-technical users on two laptops have no GUI way to send directly to another Flitnix desktop.
5. **Flitnix Status**: Partial (`internal/client` + `send_cli.go` exist, zero GUI integration).
6. **Why It Improves Flitnix**: Unlocks the entire PC↔PC, Mac↔Windows, Linux↔Mac market segment inside the GUI.
7. **Expected User Impact**: Very High — doubles the addressable use cases beyond phone↔PC.
8. **Technical Complexity**: Medium
9. **Development Effort**: 1.5–2 weeks
10. **Security Implications**: Reuses `internal/client` strict `HostID` TOFU pinning, SAS verification, and SHA-256 pre-computed manifest digests; upgrade Desktop↔Desktop transport to TLS/mTLS (Gap 10).
11. **Backend Changes**: Wrap `internal/client` in an async transfer manager in `app.go` (`SendToPeer(target, paths)`), emitting progress events to Svelte.
12. **Frontend Changes**: Add "Send to Nearby Device / Enter Code" target selector in `PersonalTransferView.svelte` alongside the QR Phone mode.
13. **Mobile Changes**: None.
14. **Testing**: Integration tests between two `server.Server` + `app.go` instances.
15. **Dependencies**: Automatic LAN Discovery (Gap 3) and/or 6-Digit Code (Gap 12).
- **Scores**: UV: 5, SA: 5, CA: 4, F: 4, RM: 4 → **Weighted Score: 4.55 (P0)**

### Gap 10: Real-Time WebSockets (or SSE) Replacing HTTP Polling
1. **Feature Name**: WebSocket Real-Time Event Channel (Desktop & Mobile)
2. **Description**: Replace the 500–2500 ms HTTP polling loops in `app.js`, `guest.js`, and `App.svelte` with a lightweight WebSocket (or SSE + Wails `EventsEmit`) push channel for instant approval prompts, batch state transitions, Quick Text delivery, and live download/upload progress.
3. **Competitors**: PairDrop (WebSockets), Snapdrop (WebSockets), AirDroid.
4. **User Problem**: Currently, pressing "Approve" on desktop takes up to 1.2s for the phone to notice (`POLL_MS = 1200`), Quick Text messages lag up to 2.5s, and 72,000+ redundant Wails/HTTP poll requests occur during idle sessions.
5. **Flitnix Status**: Missing (HTTP polling only; `gorilla/websocket` is already in `go.mod` as an indirect dep!).
6. **Why It Improves Flitnix**: Makes interactions feel instantaneous (AirDrop-like snappiness), cuts idle battery/CPU usage on phones, and surfaces incoming requests immediately.
7. **Expected User Impact**: High — perceptibly faster UI responsiveness and lower mobile battery drain.
8. **Technical Complexity**: Medium
9. **Development Effort**: 1.5 weeks
10. **Security Implications**: Must enforce strict Origin/Host checks on WebSocket upgrade (`securityMiddleware` + `requireSession` Bearer/subprotocol or initial ticket exchange — never put the session token in the WS query string!), plus per-message size caps.
11. **Backend Changes**: Add `GET /api/events` (SSE or WebSocket via `gorilla/websocket` / `coder/websocket`) and `GET /guest-api/events` in `internal/server/`; emit Wails events from `Coordinator`/`Service` hooks in `app.go` instead of 800 ms polling.
12. **Frontend Changes**: Subscribe via `EventsOn` in `App.svelte` with slow-fallback reconciliation.
13. **Mobile Changes**: Connect `EventSource`/`WebSocket` in `app.js` and `guest.js`, keeping existing polling as automatic fallback if WS is blocked.
14. **Testing**: Reconnect/disconnect tests, Origin-spoofing WS rejection tests, Safari background tab wakeup reconciliation tests.
15. **Dependencies**: None.
- **Scores**: UV: 4, SA: 5, CA: 3, F: 4, RM: 4 → **Weighted Score: 4.05 (P1)**

### Gap 11: Encrypted Transport for CLI/Desktop-to-Desktop (mTLS / E2EE) & Optional Local HTTPS
1. **Feature Name**: Hybrid Encrypted Transport (Mandatory mTLS/E2EE for Peers + Optional Local HTTPS)
2. **Description**: Implement TLS 1.3 with Ed25519-pinned self-signed certificates (`internal/devices`) for all Desktop↔Desktop and CLI (`flitnix send`/`pair`) traffic, plus an opt-in local HTTPS mode (or application-layer JS E2EE envelope via WebCrypto/tweetnacl for paired browsers) while preserving zero-friction plain HTTP as the default for ad-hoc guest QR scans.
3. **Competitors**: LocalSend (TLS on native apps), KDE Connect (TLS + cert pinning), Blip (mTLS over TLS 1.3), PairDrop (WebRTC DTLS), Croc (PAKE E2EE).
4. **User Problem**: On shared university, café, or office Wi-Fi, plaintext HTTP exposes file contents, filenames, Quick Text notes, and guest cookies to anyone sniffing the wireless segment.
5. **Flitnix Status**: Missing (`docs/TRANSPORT_ENCRYPTION_EVALUATION.md` specifically proposed mTLS/E2EE for app-to-app/desktop-to-desktop in Section 4.2, unimplemented).
6. **Why It Improves Flitnix**: Closes Flitnix's largest security gap against LocalSend/KDE Connect/Blip without breaking zero-install QR scans.
7. **Expected User Impact**: Medium-High (High for security/enterprise/power users; transparent to normal users).
8. **Technical Complexity**: High
9. **Development Effort**: 2–3 weeks
10. **Security Implications**: Prevents passive eavesdropping and active MITM tampering on LANs. Must carefully avoid browser cert interstitials for ad-hoc QR guests (keep HTTP fallback or explore WebCrypto/pure-JS authenticated encryption over HTTP for paired browsers, noting that JS served over plain HTTP still relies on initial QR trust).
11. **Backend Changes**: TLS listener / dual-protocol ALPN or dedicated TLS port (e.g. `43822`) in `internal/server/`, cert generation tied to `HostIdentity` Ed25519 key (`internal/devices`), `internal/client` certificate pinning verification.
12. **Frontend Changes**: Display encryption badge (`🔒 TLS Pinned` vs `LAN HTTP`) in Connection Hub & Settings toggle for "Require Encrypted Transport for Desktop/CLI".
13. **Mobile Changes**: Optional E2EE payload encryption for paired mobile browsers + clear security status indicator.
14. **Testing**: MITM cert-mismatch rejection tests, TLS 1.3 throughput benchmarks (note `benchmarks/tls_eval` already exists!).
15. **Dependencies**: Mobile Trusted Pairing (Gap 4) / Desktop-to-Desktop (Gap 9).
- **Scores**: UV: 4, SA: 5, CA: 4, F: 3, RM: 3 → **Weighted Score: 4.00 (P1)**

### Gap 12: Installable Mobile PWA with Web Share Target API
1. **Feature Name**: Mobile Web App (PWA) & Android/OS Share Sheet Integration
2. **Description**: Add a `manifest.webmanifest`, service worker (shell caching + offline recovery page), and `share_target` POST handler in `internal/server/assets/` so mobile users can "Install Flitnix" to their home screen and share photos/files/URLs directly from Gallery/Files/Chrome into Flitnix.
3. **Competitors**: PairDrop (PWA + Web Share Target), LocalSend/AirDrop/Quick Share (native Share Sheet).
4. **User Problem**: Currently, sending a photo from Android Gallery requires opening the browser, tapping "Pick files", navigating through Android's document picker, and hunting for the photo.
5. **Flitnix Status**: Missing (no manifest, no service worker, no `share_target`).
6. **Why It Improves Flitnix**: Delivers native-app Share Sheet ergonomics **without** app store downloads or maintaining Swift/Kotlin codebases!
7. **Expected User Impact**: Very High (on Android Chrome/Edge; partial on iOS Safari Home Screen Web App).
8. **Technical Complexity**: Medium
9. **Development Effort**: 1–1.5 weeks
10. **Security Implications**: **Important browser constraint**: `ServiceWorker` and `share_target` generally require a **secure context** (HTTPS) on non-localhost IPs in Chrome/Safari! Thus PWA Share Target works best paired with optional local CA/trusted origin or `.local` mDNS + cert, or on desktop `localhost`/Android Chrome flags; however, basic Home Screen bookmark + app icon (`apple-touch-icon.png` already exists in `assets/`!) works immediately over HTTP, and full `share_target` activates when HTTPS/local origin is enabled.
11. **Backend Changes**: Serve `/manifest.webmanifest` and `/sw.js` with proper `Service-Worker-Allowed` and CSP headers (`routes.go`).
12. **Frontend Changes**: None.
13. **Mobile Changes**: Create `manifest.webmanifest`, `sw.js`, handle shared `FormData` files/text/URLs and route directly into `addFiles()` / Quick Text.
14. **Testing**: Lighthouse PWA validation + Android Chrome Share Target test.
15. **Dependencies**: Mobile Trusted Pairing (Gap 4) so launching the PWA reconnects without re-scanning QR; Local HTTPS (Gap 11) for full `share_target` support on non-localhost IPs.
- **Scores**: UV: 4, SA: 5, CA: 4, F: 3, RM: 4 → **Weighted Score: 4.10 (P1)**

### Gap 13: 6-Digit One-Time Transfer / Pairing Code
1. **Feature Name**: 6-Digit Short Code Connection & Transfer Key
2. **Description**: Generate a short-lived 6-digit numeric code (e.g., `482-915`) alongside the QR code (and a "Join with Code" input on desktop/mobile `/join` or mDNS lookup) so devices without a working camera or when typing between two laptops can connect effortlessly.
3. **Competitors**: Send Anywhere (6-digit key), PairDrop (6-digit pairing), LocalSend (PIN).
4. **User Problem**: Laptops don't have rear cameras to scan QR codes, and copying a 65-character `http://192.168.1.42:43821/#...` URL between devices is painful.
5. **Flitnix Status**: Missing for session connection (SAS uses 6 digits *after* QR scan in CLI pairing only).
6. **Why It Improves Flitnix**: Makes cross-laptop and camera-less pairing as fast as Send Anywhere while staying 100% offline on LAN.
7. **Expected User Impact**: High — removes friction whenever QR scanning isn't convenient.
8. **Technical Complexity**: Low-Medium
9. **Development Effort**: 1 week
10. **Security Implications**: A 6-digit code has only $10^6$ combinations—must **not** be a static password! Mitigate via: strict 2–3 minute TTL, mandatory **rate limiting / exponential lockout after 3–5 failed guesses** (reusing `PairingCoordinator` lockout logic), single-use redemption exchanged for a full 256-bit Bearer token, and optional desktop visual confirmation.
11. **Backend Changes**: Add `CreateShortCode` / `RedeemShortCode` on `session.Manager` + `POST /api/connect-code` (unauthenticated, IP-rate-limited + global 5-failure lockout) + mDNS TXT/service lookup so typing `482915` on another Flitnix desktop resolves the host IP automatically!
12. **Frontend Changes**: Show large `482 915` code pill under QR in `PersonalTransferView.svelte` + "Connect via 6-Digit Code" modal.
13. **Mobile Changes**: Support `http://<host>:43821/` landing screen offering a "Enter 6-Digit Code" fallback in addition to QR scan.
14. **Testing**: Brute-force lockout tests, TTL expiration tests, single-use replay rejection tests.
15. **Dependencies**: Optional synergy with LAN Discovery (Gap 3) so desktop peers don't even need the IP.
- **Scores**: UV: 4, SA: 5, CA: 4, F: 4, RM: 4 → **Weighted Score: 4.25 (P0)**

### Gap 14: Media Thumbnails, File Previews & Gallery Selector
1. **Feature Name**: Media Thumbnails & In-App File Previews (Desktop & Mobile)
2. **Description**: Render image/video thumbnails and lightweight in-app previews (images, video, audio, PDF/text) in the desktop Shared list, Approval Inbox, Transfer History, and mobile `Files on the desktop` list.
3. **Competitors**: AirDrop, Quick Share, AirDroid (Gallery module), PairDrop (image preview on incoming prompt).
4. **User Problem**: Seeing `IMG_20261124_184211.jpg` gives zero visual confirmation of *which* photo is being approved or downloaded.
5. **Flitnix Status**: Missing (displays text filenames and generic extension chips like `JPG`, `MP4` only).
6. **Why It Improves Flitnix**: Dramatic UX polish—users immediately recognize photos/videos before approving or downloading.
7. **Expected User Impact**: High — especially for photo/video workflows, Flitnix's primary mobile use case.
8. **Technical Complexity**: Medium
9. **Development Effort**: 2 weeks
10. **Security Implications**: **Never** generate thumbnails via unsafe native C parsers without sandboxing, and **never** render untrusted HTML/SVG inline! On mobile uploads, generate small JPEG/WebP preview thumbnails client-side on `<canvas>` (or `createImageBitmap`) and upload alongside metadata (capped at e.g. 32 KiB) only after validation; on desktop Shared files, serve sanitized thumbnails via `/api/files/{id}/thumb` with `X-Content-Type-Options: nosniff` and `Content-Disposition` / strict image types (`image/jpeg`, `image/png`, `image/webp`)—excluding SVG to prevent XSS.
11. **Backend Changes**: Add `/api/files/{id}/thumb` and `/api/files/{id}/preview` (range-aware) with bounded thumbnail cache (`sync`/LRU) and Go `image` resizing; optionally accept small preview thumbnail in upload/batch declaration.
12. **Frontend Changes**: Render thumbnail grid/list toggle in `PersonalTransferView.svelte` & `TransferHistoryView.svelte` + lightbox modal.
13. **Mobile Changes**: Render file thumbnails in `app.js` upload queue (via `URL.createObjectURL` revoked on completion) and desktop shared gallery cards.
14. **Testing**: Corrupt/malformed image fuzz tests, SVG/XSS rejection tests, memory-leak tests for object URLs.
15. **Dependencies**: None.
- **Scores**: UV: 4, SA: 4, CA: 4, F: 4, RM: 4 → **Weighted Score: 4.00 (P1)**

### Gap 15: Device Visibility Modes & Granular Auto-Approval Policies
1. **Feature Name**: Device Visibility Controls & Conditional Auto-Accept Rules
2. **Description**: Add AirDrop/LocalSend-style visibility modes (**Everyone / Open Session**, **Trusted Devices Only**, **10-Minute Temporary Window**, **Hidden / Off**) plus conditional auto-accept rules (e.g., auto-approve files `< 100 MB` from trusted devices, or always prompt for executables/scripts).
3. **Competitors**: AirDrop (10-min Everyone / Contacts / Off), Quick Share (Everyone / Contacts / Your Devices / Hidden), LocalSend (Off / Favorites / On).
4. **User Problem**: Currently, Flitnix has only a binary per-device `AutoApprove` boolean (all-or-nothing) and no timed "Everyone for 10 Minutes" mode for safe use in classrooms/offices.
5. **Flitnix Status**: Partial (`Device.AutoApprove` boolean exists; no visibility state machine or conditional policy).
6. **Why It Improves Flitnix**: Brings AirDrop's anti-spam ergonomics to always-on background/tray mode.
7. **Expected User Impact**: High — essential safety control once tray + mDNS discovery are enabled.
8. **Technical Complexity**: Low-Medium
9. **Development Effort**: 1 week
10. **Security Implications**: Prevents "cyberflashing" / unsolicited LAN popups when Flitnix runs in background; timed 10-minute open mode auto-reverts safely.
11. **Backend Changes**: Add `VisibilityMode` + timer in `app.go` & `settings.go`, enforce in `internal/discovery` and incoming `/api/batches` / `/api/uploads`, add dangerous-extension prompt guard even when `AutoApprove` is true.
12. **Frontend Changes**: Quick visibility switcher pill in `PersonalTransferView.svelte` header & tray menu + per-device policy rules in `TrustedDevicesView.svelte`.
13. **Mobile Changes**: Clear status message if host is in "Trusted Devices Only" or "Hidden" mode.
14. **Testing**: Timer reversion tests, policy enforcement tests for executable extensions vs media.
15. **Dependencies**: System Tray (Gap 5) & LAN Discovery (Gap 3).
- **Scores**: UV: 4, SA: 5, CA: 3, F: 5, RM: 5 → **Weighted Score: 4.30 (P0)**

### Gap 16: Adaptive Transfer Concurrency & Queue Management (Reorder / Pause / Prioritize)
1. **Feature Name**: Configurable Parallel Transfers (Small Files) & Queue Controls
2. **Description**: Allow configurable concurrency (e.g., 1–4 streams, auto-pipelining small files `< 10 MB` concurrently while keeping multi-GB files sequential to avoid disk thrashing) plus queue pause/resume, drag-to-reorder, and prioritize-next buttons.
3. **Competitors**: LocalSend, Send Anywhere, Blip, AirDroid.
4. **User Problem**: Sending 1,000 tiny 50 KB photos sequentially over HTTP/1.1 wastes RTT latency per item (`claim` + `PUT` + `fsync` + `poll` round-trips), making bulk photo dumps much slower than raw bandwidth allows.
5. **Flitnix Status**: Partial (strictly 1 active stream per session in `coordinator.go`, no pause/reorder).
6. **Why It Improves Flitnix**: Dramatically speeds up multi-hundred-file photo/project transfers while preserving bounded memory (`sync.Pool` capped at `MaxConcurrentStreams * 256 KiB`).
7. **Expected User Impact**: High — major speed boost for folders and photo rolls.
8. **Technical Complexity**: Medium-High
9. **Development Effort**: 2 weeks
10. **Security Implications**: Must keep strict `MaxActive` / `QuotaManager` memory & disk bounds (e.g., max 4 concurrent streams) so parallel streams cannot OOM low-RAM devices.
11. **Backend Changes**: Generalize `coordinatorSession.active` from single `*activeLease` to bounded `active map[string]*activeLease` (default limit 1 for huge files, up to N=3–4 configurable), add `ReorderBatchItems` / `PauseSession` / `ResumeSession`.
12. **Frontend Changes**: Concurrency slider (`1`–`4`, default `Auto`) in `SettingsView.svelte` + Pause/Prioritize controls in `PersonalTransferView.svelte`.
13. **Mobile Changes**: Parallel Web Worker pool (up to 3 workers) in `upload-client.js` + Pause/Prioritize buttons in `app.js` queue UI.
14. **Testing**: Race-detector stress tests (`go test -race`) for concurrent `CompleteItem` / `CancelItem` / `ReserveCancel` invariants!
15. **Dependencies**: Careful preservation of `Coordinator` race guarantees.
- **Scores**: UV: 4, SA: 4, CA: 3, F: 3, RM: 3 → **Weighted Score: 3.55 (P1)**

### Gap 17: Opt-In Automatic Clipboard Synchronization
1. **Feature Name**: Opt-In Bidirectional Clipboard Sync
2. **Description**: Add an explicit opt-in toggle in desktop & paired mobile sessions to automatically push newly copied text/URLs (up to 64 KiB) across paired devices, with sensitive-content exclusions (e.g., ignore password manager concealed clipboard hints on Windows/macOS/Linux where available, pause button, and clear notification).
3. **Competitors**: KDE Connect, AirDrop/Apple Universal Clipboard, AirDroid.
4. **User Problem**: Users copy 2FA codes, URLs, or terminal commands constantly between PC and phone; currently they must switch to Quick Text, paste, and hit Send every time.
5. **Flitnix Status**: Partial (`internal/textmsg` + Quick Text UI exist, manual send only).
6. **Why It Improves Flitnix**: Elevates Quick Text from a simple chat box to effortless KDE Connect-level productivity.
7. **Expected User Impact**: High — beloved daily workflow feature for developers and power users.
8. **Technical Complexity**: Medium
9. **Development Effort**: 1.5 weeks
10. **Security Implications**: **High sensitivity**: Must be **strictly opt-in** (off by default), disabled in Guest Drop Box rooms, automatically skip empty/oversized/unchanged loops (echo suppression via hash), respect OS concealed/password clipboard markers (`org.nspasteboard.ConcealedType`, `ExcludeClipboardContentFromMonitorProcessing`), and ideally require encrypted transport or paired trust.
11. **Backend Changes**: Add OS clipboard watcher (`golang.design/x/clipboard` or native OS hooks) gated by `Settings.AutoClipboardSync` + echo-hash dedup; push via `textmsg.Store`.
12. **Frontend Changes**: Prominent opt-in toggle + "Pause Sync" indicator in Quick Text header & `SettingsView.svelte`.
13. **Mobile Changes**: Browser `navigator.clipboard.readText()` only works on explicit focus/user gesture due to browser security—implement "Tap to Sync Clipboard" / auto-read-on-focus when user grants permission, plus one-tap auto-copy button on incoming desktop clips.
14. **Testing**: Echo-loop prevention tests, concealed-type filtering tests, 64 KiB boundary tests.
15. **Dependencies**: WebSockets (Gap 10) for instant delivery; Trusted Devices (Gap 4).
- **Scores**: UV: 4, SA: 4, CA: 4, F: 3, RM: 3 → **Weighted Score: 3.75 (P1)**

### Gap 18: Duplicate Detection & Configurable File Conflict Policy
1. **Feature Name**: Duplicate File Detection & Conflict Resolution Policy
2. **Description**: Allow users to choose how incoming Personal Transfer filename collisions are handled (**Keep Both** [current default], **Skip Identical [size + SHA-256 match]**, **Overwrite**, or **Ask**), and optionally skip re-transferring identical files in multi-hundred-file folder uploads.
3. **Competitors**: Syncthing, Blip, standard OS file managers.
4. **User Problem**: Re-sending an updated folder of 500 photos currently creates 500 duplicate `photo (1).jpg` files on disk.
5. **Flitnix Status**: Partial (Guest Drop Box supports `preserve_both` vs `reject`; Personal Transfer hardcodes `variantName` keep-both in `store.go:485`).
6. **Why It Improves Flitnix**: Prevents storage bloat and makes repeated folder transfers smart and clean.
7. **Expected User Impact**: Medium-High.
8. **Technical Complexity**: Medium
9. **Development Effort**: 1 week
10. **Security Implications**: Overwrite mode must be **opt-in** (never default) and strictly confined via `withinDir` + regular-file check (never overwrite symlinks or directories) to preserve Flitnix's "never silently clobber files" guarantee.
11. **Backend Changes**: Add `ConflictPolicy` (`keep_both`, `skip_identical`, `overwrite`) to `settings.go` / batch declaration; pre-flight manifest check compares existing destination file size + SHA-256 against declared SHA-256 before streaming bytes!
12. **Frontend Changes**: Conflict policy selector in `SettingsView.svelte` Storage tab & per-batch approval card ("12 files already exist identically — Skip duplicates?").
13. **Mobile Changes**: Compute SHA-256 in worker (or quick partial/full hash) when `skip_identical` is enabled; show `"Skipped (Already on desktop)"` badge.
14. **Testing**: Collision, symlink refusal, and hash-mismatch overwrite/keep-both tests.
15. **Dependencies**: None.
- **Scores**: UV: 4, SA: 4, CA: 3, F: 4, RM: 4 → **Weighted Score: 3.80 (P1)**

### Gap 19: Multi-Recipient Desktop-to-Many Broadcasting (Local Share Room)
1. **Feature Name**: Desktop-to-Multiple-Phones Sharing Room (One-to-Many Broadcast)
2. **Description**: Extend `SharedSource` / session sharing so a teacher, presenter, or host can display one QR code (similar to Guest Drop Box, but **download-only** or **two-way** room) allowing 10–100 attendees on local Wi-Fi to download the shared slides, PDFs, or event photos concurrently.
3. **Competitors**: PairDrop (Public Rooms), Send Anywhere (Link sharing), Quick Share (QR link).
4. **User Problem**: Guest Drop Box is strictly upload-only (`guest.go` isolates guests from desktop files); Personal Transfer is meant for 1-to-1 pairing and limits `maxGrantsPerSession` / `active` download per session.
5. **Flitnix Status**: Partial (Guest Drop Box exists for *receiving* from a crowd; Personal session only supports 1 active archive download per session key in `grants.go:209`).
6. **Why It Improves Flitnix**: Completes the classroom/conference story: collect assignments *and* distribute handouts/media locally with zero internet.
7. **Expected User Impact**: High for classrooms, workshops, and events (Flitnix's signature differentiator!).
8. **Technical Complexity**: Medium
9. **Development Effort**: 1.5 weeks
10. **Security Implications**: Read-only access strictly scoped to explicitly shared sources; optional room PIN/passcode or host-approve-join gate; download rate/concurrency cap to avoid Wi-Fi saturation.
11. **Backend Changes**: Add `RoomMode` (`upload_only`, `download_only`, `two_way`) to `internal/guest/room.go`, allow `guest-api` read-only `/guest-api/files` + download grants when enabled, lift single-session archive lock across distinct guest IDs.
12. **Frontend Changes**: Mode selector in `GuestDropBoxView.svelte` Step 1 + Shared Files picker when `download_only`/`two_way` is selected.
13. **Mobile Changes**: Conditionally show "Shared by Host" download section in `guest.html`/`guest.js`.
14. **Testing**: Cross-mode isolation tests (verify `upload_only` rooms still return 403/404 on `/guest-api/files`).
15. **Dependencies**: Builds cleanly on `internal/guest`.
- **Scores**: UV: 4, SA: 5, CA: 5, F: 4, RM: 4 → **Weighted Score: 4.45 (P0)**

### Gap 20: Optional Self-Hosted Relay / WebRTC Internet Mode
1. **Feature Name**: Opt-In Peer-to-Peer WebRTC / Self-Hosted Relay Mode
2. **Description**: Add an explicitly disabled-by-default "Remote / Restricted Wi-Fi Bridge" using WebRTC DataChannels + user-configured (or Flitnix-optional) STUN/TURN/signaling relay (`coturn`/custom lightweight Go signaling binary), enabling transfers when hotel/campus Wi-Fi enforces AP Client Isolation or devices are on different networks.
3. **Competitors**: PairDrop (WebRTC + TURN), Send Anywhere, Blip, Croc.
4. **User Problem**: Connection Doctor detects Wi-Fi Client Isolation and advises switching to a phone hotspot—but in some offices/hotels, users cannot use hotspots and local HTTP is blocked by AP isolation.
5. **Flitnix Status**: Missing.
6. **Why It Improves Flitnix**: Solves the hardest LAN blocker (AP isolation) and enables optional remote sharing *without* mandatory cloud storage or accounts.
7. **Expected User Impact**: Medium-High (critical when AP isolation blocks local HTTP).
8. **Technical Complexity**: Very High
9. **Development Effort**: 4–6 weeks
10. **Security Implications**: **High**: Must remain **explicitly opt-in** with clear warning that signaling/TURN metadata touches a relay; all payloads must be E2EE (WebRTC DTLS + SAS verification); never default to third-party cloud relays silently.
11. **Backend Changes**: Integrate `pion/webrtc` in a new `internal/remote` package + tiny self-hostable signaling/relay server binary under `cmd/flitnix-relay`.
12. **Frontend Changes**: Opt-in "Enable Remote / Isolated-Wi-Fi Relay" toggle in `SettingsView.svelte` + Connection Doctor quick action when AP isolation fails.
13. **Mobile Changes**: WebRTC fallback client when `/ping` fails due to AP isolation.
14. **Testing**: NAT traversal, DTLS verification, fallback switching tests.
15. **Dependencies**: Encrypted Transport (Gap 11).
- **Scores**: UV: 4, SA: 3, CA: 4, F: 2, RM: 2 → **Weighted Score: 3.25 (P2)**

### Gap 21: Bandwidth Limiting & Transfer Scheduling
1. **Feature Name**: Configurable Bandwidth Cap & Scheduled/Off-Peak Transfers
2. **Description**: Allow capping upload/download throughput (e.g., 5 MB/s, 20 MB/s, Unlimited) via token-bucket rate limiting and scheduling large queued desktop-to-desktop transfers for a specified time.
3. **Competitors**: Syncthing (rate limits), enterprise transfer tools.
4. **User Problem**: A 50 GB transfer saturating a shared home/office Wi-Fi router can starve video calls or gaming for everyone else on the network.
5. **Flitnix Status**: Missing (`phase4_network_simulation_test.go` has a test-only token-bucket shaper, not production).
6. **Why It Improves Flitnix**: Gives power users polite bandwidth control on shared networks.
7. **Expected User Impact**: Low-Medium (niche but valued by power users).
8. **Technical Complexity**: Low-Medium
9. **Development Effort**: 1 week
10. **Security Implications**: Minimal; avoid division-by-zero or stall-watchdog false timeouts when bandwidth cap is set very low (adjust `stallBodyReader` timeout dynamically based on rate limit!).
11. **Backend Changes**: Wrap `streamBody` and download writers with an optional `rate.Limiter` (from `golang.org/x/time/rate`) configured via `settings.Settings`.
12. **Frontend Changes**: Speed limit dropdown/input in `SettingsView.svelte` Network tab.
13. **Mobile Changes**: Respect server-enforced backpressure naturally via TCP window.
14. **Testing**: Rate accuracy tests + stall-timeout interaction tests at 512 KB/s.
15. **Dependencies**: None.
- **Scores**: UV: 2, SA: 4, CA: 2, F: 5, RM: 4 → **Weighted Score: 3.15 (P2)**

### Gap 22: Modification Timestamp & File Metadata Preservation
1. **Feature Name**: Original Modification Timestamp (`mtime`) Preservation
2. **Description**: Allow upload manifests (`BatchManifest`) to carry original `File.lastModified` timestamps from the phone/CLI and restore them via `os.Chtimes` upon atomic publication on desktop.
3. **Competitors**: Blip, Syncthing, AirDrop, Quick Share.
4. **User Problem**: When backing up 500 vacation photos from phone to PC, every file gets "today's" modification date, breaking chronological sorting in file explorers.
5. **Flitnix Status**: Partial (Desktop→Phone ZIP preserves `ModTime` truncated to 2s in `archive.go:206`, but Phone→Desktop discards `mtime`).
6. **Why It Improves Flitnix**: Keeps photo libraries and project backups chronologically accurate.
7. **Expected User Impact**: Medium-High for photo/archive backup users.
8. **Technical Complexity**: Low
9. **Development Effort**: 2–3 days
10. **Security Implications**: Clamp/validate timestamps (e.g., reject negative or far-future dates) and apply `os.Chtimes` *before* `os.Link` or safely on the committed path inside `withinDir`.
11. **Backend Changes**: Add optional `LastModifiedMs int64` to `ManifestItem` (`batch.go`) and `store.go` `CommitWithModTime`.
12. **Frontend Changes**: Optional toggle "Preserve original file dates" (default On) in `SettingsView.svelte`.
13. **Mobile Changes**: Include `lastModified: f.lastModified` in `POST /api/batches` items (`app.js`).
14. **Testing**: Cross-platform `os.Chtimes` unit tests.
15. **Dependencies**: None.
- **Scores**: UV: 4, SA: 5, CA: 3, F: 5, RM: 5 → **Weighted Score: 4.30 (P1)** *(high ROI quick win!)*

### Gap 23: Password-Protected & One-Time / Download-Limited Share Links
1. **Feature Name**: Passcode-Protected & Download-Count-Limited Share Sessions
2. **Description**: Allow desktop users sharing files to optionally attach a room/session PIN/password, custom link expiration, and a max-download count (including "1-time self-destruct share") for local LAN links or Guest rooms.
3. **Competitors**: Send Anywhere (paid password/expiry), Quick Share Private Sharing, LocalSend (PIN).
4. **User Problem**: On a shared office/classroom Wi-Fi, someone who glimpses the screen URL or receives a forwarded link could download files multiple times or after the intended recipient finishes.
5. **Flitnix Status**: Partial (Guest rooms have duration expiry, no passcode or download-count limit; Personal sessions have 10-min idle expiry, no PIN or download-count cap).
6. **Why It Improves Flitnix**: Adds Send Anywhere Pro-grade link controls locally for free.
7. **Expected User Impact**: Medium.
8. **Technical Complexity**: Low-Medium
9. **Development Effort**: 1 week
10. **Security Implications**: Rate-limit passcode attempts (`PairingCoordinator`-style lockout); hash passcode in memory with `subtle.ConstantTimeCompare`.
11. **Backend Changes**: Optional `PIN` and `MaxDownloads` fields on `session` / `DownloadGrantStore` / `guest.Room`.
12. **Frontend Changes**: Optional "Require PIN" & "Expire after N downloads" checkboxes in Share/Guest wizard.
13. **Mobile Changes**: Simple PIN prompt card in `app.js` / `guest.js` before showing shared files.
14. **Testing**: Download counter exhaustion tests, PIN brute-force lockout tests.
15. **Dependencies**: None.
- **Scores**: UV: 3, SA: 4, CA: 3, F: 5, RM: 4 → **Weighted Score: 3.65 (P1)**

### Gap 24: Deep OS Share Sheet & File Manager Integration (macOS & Windows)
1. **Feature Name**: macOS Finder Quick Action / Share Extension & Multi-Select Windows Context Menu
2. **Description**: Add a macOS Finder Quick Action / Share Extension (and `.pkg`/`.dmg` helper) plus Windows 11 modern context menu / SendTo multi-file batching so right-clicking 20 files in Finder or Explorer opens them in one Flitnix share batch (not 20 separate invocations).
3. **Competitors**: AirDrop, LocalSend (`v1.15.0` Windows context menu), Quick Share, KDE Connect, Blip.
4. **User Problem**: Linux has Nautilus & Dolphin scripts, and Windows NSIS adds basic `*`/`Directory` registry entries, **but macOS has zero Finder context menu/Share Sheet**, and Windows selecting 10 files spawns 10 parallel `flitnix.exe "%1"` processes racing `SingleInstanceLock`.
5. **Flitnix Status**: Partial (`build/linux/nautilus`, `servicemenus`, `project.nsi`, `Mac.OnFileOpen`).
6. **Why It Improves Flitnix**: Makes right-click sharing genuinely seamless on macOS and reliable for multi-file selections on Windows.
7. **Expected User Impact**: Medium-High on Windows/macOS.
8. **Technical Complexity**: Medium-High (macOS Share Extension requires Swift appex bundle & App Group IPC; Windows 11 top-level menu requires sparse package or SendTo shortcut).
9. **Development Effort**: 2–3 weeks
10. **Security Implications**: Validate all OS-delivered paths through `ExtractSharePaths`; avoid spawning shell commands with unquoted paths.
11. **Backend Changes**: Coalesce rapid `onSecondInstanceLaunch` calls within a 300 ms debounce window into a single notification; add macOS Automator/Quick Action or Swift Share Extension target in `build/darwin/`.
12. **Frontend Changes**: Toast `"Added 12 files from Explorer/Finder"`.
13. **Mobile Changes**: Covered by PWA Share Target (Gap 12).
14. **Testing**: Multi-file `onSecondInstanceLaunch` batching tests; manual macOS/Windows packaging tests.
15. **Dependencies**: None.
- **Scores**: UV: 3, SA: 4, CA: 3, F: 3, RM: 3 → **Weighted Score: 3.25 (P2)**

### Gap 25: Automatic Media Organization & Multiple Transfer Destinations
1. **Feature Name**: Smart Subfolder Sorting & Per-Device / Per-Type Destinations
2. **Description**: Optional rules to automatically organize received files into subfolders by date (`YYYY-MM`), file type (`Photos/`, `Videos/`, `Documents/`), or sender device name (`Pixel-9/`), plus quick-switch destination profiles.
3. **Competitors**: AirDroid, Syncthing, custom LocalSend requests.
4. **User Problem**: Dumping 1,000 mixed PDFs, memes, and 4K videos into one flat `Downloads/Flitnix/` directory creates instant clutter.
5. **Flitnix Status**: Missing (all personal uploads go flat into `store.Dir()`, except folders preserve internal tree).
6. **Why It Improves Flitnix**: Zero-effort organization for heavy users while keeping default flat behavior intact.
7. **Expected User Impact**: Medium.
8. **Technical Complexity**: Low
9. **Development Effort**: 4–5 days
10. **Security Implications**: Subfolder names (`DeviceName` / date / category) must pass `SanitizeName` and `withinDir` checks.
11. **Backend Changes**: Add `OrganizationRule` (`flat`, `by_device`, `by_type`, `by_date`) in `settings.go` and apply subpath routing in `store.OpenPart`.
12. **Frontend Changes**: Dropdown in `SettingsView.svelte` Storage tab.
13. **Mobile Changes**: None.
14. **Testing**: Path confinement tests for sanitized device subfolders.
15. **Dependencies**: None.
- **Scores**: UV: 3, SA: 4, CA: 3, F: 5, RM: 5 → **Weighted Score: 3.75 (P1)**

### Gap 26: Storage Usage Dashboard, Disk Headroom Warnings & Auto-Cleanup
1. **Feature Name**: Storage Usage Bar, Pre-Transfer Disk Check & History Auto-Retention
2. **Description**: Show real-time free disk space in the target receive directory, warn/reject oversized incoming batches *before* approval (`507 Insufficient Storage` check up front), show storage breakdown in History, and configure automatic pruning of old history/logs.
3. **Competitors**: AirDroid (storage panel), Blip (pre-checks).
4. **User Problem**: Docs mention `QuotaManager` (`phase5-security-audit.md`), **yet `QuotaManager` does not exist in `internal/transfer/`**—a user can approve a 40 GB batch onto a drive with 12 GB free and fail mid-transfer!
5. **Flitnix Status**: Missing (no disk-space check in `store.go` or `service.go`!).
6. **Why It Improves Flitnix**: Prevents wasted multi-GB transfers and gives instant storage visibility.
7. **Expected User Impact**: Medium-High.
8. **Technical Complexity**: Low-Medium
9. **Development Effort**: 1 week
10. **Security Implications**: Prevents local disk-exhaustion DoS (`507 Insufficient Storage` before allocating `.part` files).
11. **Backend Changes**: Cross-platform `diskFreeBytes(dir)` (`syscall.Statfs` / `GetDiskFreeSpaceExW`), pre-approval check + `GetStorageStatus()` in `app.go`, retention cleanup job.
12. **Frontend Changes**: Disk headroom indicator on Approval Inbox cards + Storage settings retention slider.
13. **Mobile Changes**: Friendly `"Desktop storage is low"` error on `507`.
14. **Testing**: Mocked low-disk-space tests returning `507`.
15. **Dependencies**: Persistent History (Gap 6).
- **Scores**: UV: 4, SA: 5, CA: 2, F: 4, RM: 4 → **Weighted Score: 3.85 (P1)**

### Gap 27: Firewall Guidance, Auto-Diagnostic Ping & Mobile-Side Connection Helper
1. **Feature Name**: Actionable OS Firewall Guidance & Interactive Connection Probe
2. **Description**: Enhance `internal/doctor` to actively inspect OS firewall rules (Windows Defender Firewall `netsh advfirewall`, Linux `ufw`/`firewalld`, macOS `socketfilterfw`) and give copyable one-click commands/guidance, plus allow the phone QR landing page (if unreachable) to show troubleshooting tips.
3. **Competitors**: LocalSend (inbound/outbound reachability indicators), KDE Connect community guides.
4. **User Problem**: Connection Doctor checks local loopback `/healthz`, which passes even when Windows Defender Firewall or `firewalld` silently drops external packets on TCP port `43821`!
5. **Flitnix Status**: Partial (`doctor.go` checks loopback `/healthz` and warns generically on Wi-Fi/multi-homed, but never checks OS firewall state for port 43821).
6. **Why It Improves Flitnix**: Turns Connection Doctor from "good" to truly diagnostic for the #1 real-world failure mode (host firewall blocking LAN inbound TCP).
7. **Expected User Impact**: High for first-time Windows and Fedora/Ubuntu users.
8. **Technical Complexity**: Medium
9. **Development Effort**: 1 week
10. **Security Implications**: Read-only firewall inspection only; never silently alter firewall rules without explicit user/OS UAC action (and NSIS installer can optionally register the Windows firewall rule at install time!).
11. **Backend Changes**: Add 8th check `firewall_status` in `internal/doctor/doctor.go` (query `firewall-cmd`/`ufw`/`netsh` read-only with 1.5s timeout); optionally register app rule in `build/windows/installer/project.nsi`.
12. **Frontend Changes**: Display exact fix command + "Copy Command" button in `ConnectionDoctorModal.svelte`.
13. **Mobile Changes**: None.
14. **Testing**: Mocked command-output parser tests per OS.
15. **Dependencies**: None.
- **Scores**: UV: 4, SA: 5, CA: 4, F: 4, RM: 4 → **Weighted Score: 4.25 (P0)**

### Gap 28: Consent-Based Media Compression & Format Compatibility (HEIC/4K Toggle)
1. **Feature Name**: Opt-In Media Compression & Mobile Gallery Picker Mode
2. **Description**: Keep Original Quality as the strict default, while offering an explicit opt-in toggle on mobile/desktop to transcode/resize photos (e.g., HEIC→JPEG or "Share-Ready 1080p") with estimated size reduction before sending.
3. **Competitors**: AirDrop (Automatic vs Keep Originals format toggle), Send Anywhere (quality options).
4. **User Problem**: Non-technical users sometimes want to send 50 iPhone `.HEIC` photos to an older Windows app or compress a 2 GB 4K clip to 150 MB for quick email/upload without leaving Flitnix.
5. **Flitnix Status**: Missing.
6. **Why It Improves Flitnix**: Adds flexibility without violating Flitnix's "never compress without consent" rule.
7. **Expected User Impact**: Medium.
8. **Technical Complexity**: Medium-High (video compression in pure browser is slow/limited; image resizing via `<canvas>` is easy).
9. **Development Effort**: 1.5 weeks (images) / 4+ weeks (video).
10. **Security Implications**: Canvas re-encoding strips EXIF unless explicitly preserved—must clearly warn user that "Optimize Size" strips metadata vs "Original Quality (Default)".
11. **Backend Changes**: None (or optional metadata flag).
12. **Frontend Changes**: None.
13. **Mobile Changes**: Toggle `"Original Quality (Recommended)"` vs `"Smaller JPEG"` in `app.js` queue header with size estimate.
14. **Testing**: Verify default path sends byte-identical files untouched.
15. **Dependencies**: None.
- **Scores**: UV: 3, SA: 3, CA: 2, F: 3, RM: 3 → **Weighted Score: 2.80 (P3)**

### Gap 29: Mobile Web UI Theme Parity, Localization & Multi-File Gallery Filter
1. **Feature Name**: Mobile Web Light/Dark Theme, Localization & File-Type Filtering
2. **Description**: Bring desktop's theme parity (`light`/`dark`/`system`), i18n catalog (`en.json` + community locales), and search/filter chips (`Images`, `Videos`, `Docs`) to the mobile web interfaces (`index.html`/`app.js` and `guest.html`/`guest.js`).
3. **Competitors**: LocalSend (localized + themed across all platforms), PairDrop (localized + light/dark).
4. **User Problem**: Desktop supports Light/Dark and i18n, but the mobile page is locked to dark mode and hardcoded English, and scrolling 100 shared desktop files on a phone has no search/filter.
5. **Flitnix Status**: Missing on mobile (`index.html`/`guest.html` hardcode dark theme and English).
6. **Why It Improves Flitnix**: Cohesive cross-device polish and usability for large shared file lists.
7. **Expected User Impact**: Medium.
8. **Technical Complexity**: Low
9. **Development Effort**: 1 week
10. **Security Implications**: None.
11. **Backend Changes**: Serve locale dictionary or pass active theme/locale hint via `/api/session-info`.
12. **Frontend Changes**: Reuse/sync `frontend/src/locales/` strings.
13. **Mobile Changes**: Add CSS light-mode variables (`prefers-color-scheme`), search bar + filter pills in `Files on the desktop`.
14. **Testing**: Mobile asset syntax/unit tests + contrast checks.
15. **Dependencies**: None.
- **Scores**: UV: 3, SA: 4, CA: 2, F: 5, RM: 5 → **Weighted Score: 3.55 (P1)**

### Gap 30: Watch Folders & One-Way / Two-Way Incremental Folder Sync
1. **Feature Name**: Optional Watch Folder / Incremental Sync Mode
2. **Description**: Designate a folder on Desktop A (or CLI) that watches for new/modified files and automatically syncs deltas (comparing size + SHA-256) to a paired trusted Desktop B.
3. **Competitors**: Syncthing, Google Quick Share (auto photo backup to PC).
4. **User Problem**: Photographers or developers working across a desktop and laptop repeatedly manually re-share the same working folder.
5. **Flitnix Status**: Missing.
6. **Why It Improves Flitnix**: Bridges ad-hoc sharing and lightweight local folder sync for paired devices.
7. **Expected User Impact**: Medium (power users).
8. **Technical Complexity**: High
9. **Development Effort**: 3–4 weeks
10. **Security Implications**: Must only run between mutually paired `AutoApprove` trusted devices with strict root confinement and deletion-safety (append/update only by default, never silent remote delete).
11. **Backend Changes**: New `internal/sync` package using `fsnotify`, manifest diffing endpoint `POST /api/sync/diff`.
12. **Frontend Changes**: "Watch Folders" panel under Settings or Devices.
13. **Mobile Changes**: Out of scope (browsers cannot watch OS folders in background).
14. **Testing**: Loop-prevention tests, partial-write debounce tests.
15. **Dependencies**: Desktop-to-Desktop Client (Gap 9), Trusted Devices (Gap 4), Duplicate Detection (Gap 18).
- **Scores**: UV: 3, SA: 3, CA: 3, F: 2, RM: 2 → **Weighted Score: 2.75 (P3)**

### Gap 31: Desktop-to-Phone / Phone-to-Desktop Remote File Browsing (Opt-In Shared Root)
1. **Feature Name**: Opt-In Browsable Shared Folder Explorer
2. **Description**: Instead of forcing the phone to download an entire 20 GB shared folder as a monolithic `.zip`, let the phone browse inside shared folders in `Files on the desktop` (folder tree view) and download individual files or sub-selections on demand.
3. **Competitors**: KDE Connect (SFTP browse), AirDroid (File Manager).
4. **User Problem**: If you drag a 50 GB project folder onto Flitnix just to grab two files on your phone, Flitnix currently forces downloading the entire 50 GB `.zip`!
5. **Flitnix Status**: Missing (`SharedSource` already enumerates every `SourceItem` with `RelativePath` and `Size` in `source.go`, but `/api/sources` collapses folders into a single flat row!).
6. **Why It Improves Flitnix**: Huge usability upgrade for shared folders using metadata Flitnix *already computes*!
7. **Expected User Impact**: High — saves gigabytes of unnecessary ZIP downloads.
8. **Technical Complexity**: Low-Medium
9. **Development Effort**: 1 week
10. **Security Implications**: Strictly read-only inside explicitly shared `SharedSource` items; download by opaque `(sourceId, itemIndex/itemId)`—never expose raw filesystem paths.
11. **Backend Changes**: Add `GET /api/sources/{id}/items` returning `RelativePath`, `Name`, `Size`, and opaque item IDs, and allow `createDownloadGrant` to target specific items inside a `SharedSource`.
12. **Frontend Changes**: None needed (already shows item count & bytes).
13. **Mobile Changes**: Expandable folder tree / breadcrumb drill-down in `app.js` `Files on the desktop`.
14. **Testing**: Path-traversal and unshared-item isolation tests.
15. **Dependencies**: None.
- **Scores**: UV: 4, SA: 5, CA: 4, F: 5, RM: 5 → **Weighted Score: 4.50 (P0)** *(Massive hidden-value win!)*

### Gap 32: Floating Desktop Drop-Zone Widget & "Find My Device" Ping
1. **Feature Name**: Compact Floating Drop Widget & Device Ring ("Find My Device")
2. **Description**: Provide an optional tiny always-on-top drop target (or compact mode) for quick drag-and-drop without maximizing the full Wails window, plus a simple "Ping / Ring" button for connected devices.
3. **Competitors**: KDE Connect (Find My Device), AirDrop/macOS Dropzone apps.
4. **User Problem**: Opening a 1024×768 window just to drag 1 file feels heavy; conversely, "Find My Device" is only feasible if a browser tab/audio context is active (limited on locked phones).
5. **Flitnix Status**: Missing.
6. **Why It Improves Flitnix**: Nice quality-of-life polish, though secondary to core transfer/discovery gaps.
7. **Expected User Impact**: Low-Medium.
8. **Technical Complexity**: Medium (Wails v2 supports single window cleanly; secondary floating window requires OS-level or Wails v3/toggleable compact window mode).
9. **Development Effort**: 1.5 weeks
10. **Security Implications**: Low; rate-limit ring/ping to prevent annoyance.
11. **Backend Changes**: `runtime.WindowSetSize` compact mode toggle in `app.go`; optional `/api/ping-alert` event.
12. **Frontend Changes**: Compact "Mini Drop Mode" toggle button in header.
13. **Mobile Changes**: Play audio chime on explicit user-enabled alert.
14. **Testing**: Window resize/state restoration tests.
15. **Dependencies**: System Tray (Gap 5).
- **Scores**: UV: 2, SA: 3, CA: 2, F: 3, RM: 3 → **Weighted Score: 2.50 (P3)**

---

## 8. Prioritization Framework & P0/P1/P2/P3 Roadmap

Weighted Formula: `0.30*UV + 0.25*SA + 0.20*CA + 0.15*F + 0.10*RM`

| Rank | ID | Feature | UV (30%) | SA (25%) | CA (20%) | F (15%) | RM (10%) | Weighted Score | Tier |
|---|---|---|---|---|---|---|---|---|---|
| 1 | Gap 2 | Resumable Byte-Range Downloads (`HTTP 206`) | 5 | 5 | 4 | 4 | 4 | **4.55** | **P0** |
| 2 | Gap 3 | Automatic Nearby Discovery (mDNS / Multicast) | 5 | 5 | 4 | 4 | 4 | **4.55** | **P0** |
| 3 | Gap 9 | Desktop-to-Desktop GUI Sender (`internal/client` UI) | 5 | 5 | 4 | 4 | 4 | **4.55** | **P0** |
| 4 | Gap 1 | Resumable Chunked Uploads & Recovery Activation | 5 | 5 | 5 | 3 | 3 | **4.50** | **P0** |
| 5 | Gap 31 | Browse & Pick Files Inside Shared Folders (No Forced ZIP) | 4 | 5 | 4 | 5 | 5 | **4.50** | **P0** |
| 6 | Gap 4 | Mobile Browser Trusted Pairing & 1-Tap Reconnect | 5 | 5 | 4 | 4 | 3 | **4.45** | **P0** |
| 7 | Gap 19 | Two-Way / Download-Only Crowd Sharing Rooms | 4 | 5 | 5 | 4 | 4 | **4.45** | **P0** |
| 8 | Gap 6 | Persistent Personal Transfer History & File Actions | 4 | 5 | 3 | 5 | 5 | **4.30** | **P0** |
| 9 | Gap 7 | Auto-Retry Data Streams & Wi-Fi Roaming Recovery | 5 | 5 | 4 | 3 | 3 | **4.30** | **P0** |
| 10 | Gap 15 | Device Visibility Modes & Conditional Auto-Approve | 4 | 5 | 3 | 5 | 5 | **4.30** | **P0** |
| 11 | Gap 5 | System Tray / Menu Bar, Background Mode & Autostart | 5 | 5 | 3 | 4 | 3 | **4.25** | **P0** |
| 12 | Gap 13 | 6-Digit One-Time Connect / Transfer Code | 4 | 5 | 4 | 4 | 4 | **4.25** | **P0** |
| 13 | Gap 27 | OS Firewall Inspection & Actionable Doctor Guidance | 4 | 5 | 4 | 4 | 4 | **4.25** | **P0** |
| 14 | Gap 8 | Native OS Desktop Notifications (Approval & Completion) | 4 | 5 | 3 | 5 | 4 | **4.20** | **P0** |
| 15 | Gap 22 | Modification Timestamp (`mtime`) Preservation | 4 | 5 | 3 | 5 | 5 | **4.30** | **P1** *(Quick Win)* |
| 16 | Gap 12 | Installable Mobile PWA & Web Share Target API | 4 | 5 | 4 | 3 | 4 | **4.10** | **P1** |
| 17 | Gap 10 | Real-Time WebSockets / SSE Event Channel | 4 | 5 | 3 | 4 | 4 | **4.05** | **P1** |
| 18 | Gap 11 | Encrypted Transport (mTLS for Peers + Opt Local HTTPS) | 4 | 5 | 4 | 3 | 3 | **4.00** | **P1** |
| 19 | Gap 14 | Media Thumbnails & In-App File Previews | 4 | 4 | 4 | 4 | 4 | **4.00** | **P1** |
| 20 | Gap 26 | Storage Usage Dashboard & Pre-Approval Disk Quota Check | 4 | 5 | 2 | 4 | 4 | **3.85** | **P1** |
| 21 | Gap 18 | Duplicate File Detection & Conflict Policy | 4 | 4 | 3 | 4 | 4 | **3.80** | **P1** |
| 22 | Gap 17 | Opt-In Automatic Clipboard Synchronization | 4 | 4 | 4 | 3 | 3 | **3.75** | **P1** |
| 23 | Gap 25 | Smart Subfolder Organization (`by_device` / `by_type` / `by_date`) | 3 | 4 | 3 | 5 | 5 | **3.75** | **P1** |
| 24 | Gap 23 | PIN-Protected & Download-Count-Limited Share Sessions | 3 | 4 | 3 | 5 | 4 | **3.65** | **P1** |
| 25 | Gap 16 | Adaptive Small-File Concurrency & Queue Reordering | 4 | 4 | 3 | 3 | 3 | **3.55** | **P1** |
| 26 | Gap 29 | Mobile Web Light/Dark Theme, i18n & Search/Filter | 3 | 4 | 2 | 5 | 5 | **3.55** | **P1** |
| 27 | Gap 20 | Opt-In WebRTC / Self-Hosted Relay Mode | 4 | 3 | 4 | 2 | 2 | **3.25** | **P2** |
| 28 | Gap 24 | macOS Finder/Share Ext & Win11 Multi-Select Batching | 3 | 4 | 3 | 3 | 3 | **3.25** | **P2** |
| 29 | Gap 21 | Bandwidth Limiting & Scheduled Transfers | 2 | 4 | 2 | 5 | 4 | **3.15** | **P2** |
| 30 | Gap 28 | Opt-In Consent-Based Media Compression | 3 | 3 | 2 | 3 | 3 | **2.80** | **P3** |
| 31 | Gap 30 | Watch Folders & Incremental Delta Sync | 3 | 3 | 3 | 2 | 2 | **2.75** | **P3** |
| 32 | Gap 32 | Floating Compact Drop Widget & Find-My-Device Ping | 2 | 3 | 2 | 3 | 3 | **2.50** | **P3** |

*(Note: Gap 22 scored 4.30 mathematically, placed at the top of P1/Quick Wins as a 2-day patch alongside the P0 flagship releases; P0 focuses on the 14 foundational reliability, discovery, background-operation, and UX gaps.)*

---

## 9. Top 10 Recommended Features — Architectural Feasibility & UX Design

For clarity, here are the **10 highest-impact features** selected across the P0/P1 tiers (combining highest weighted score with cohesive architectural synergy):

1. **Resumable Chunked Uploads + Auto-Retry & Crash Recovery** (Gaps 1 + 7)
2. **Resumable Byte-Range Downloads (`HTTP 206`) + Shared Folder File Browser** (Gaps 2 + 31)
3. **Automatic LAN Discovery (mDNS/Multicast) + Desktop-to-Desktop GUI Sender** (Gaps 3 + 9)
4. **Mobile Browser Trusted Pairing & One-Tap Reconnect** (Gap 4)
5. **System Tray / Menu Bar, Background Receiving & Autostart** (Gap 5)
6. **Native OS Desktop Notifications (Approval & Completion)** (Gap 8)
7. **Persistent Personal Transfer History & Actionable File Management** (Gap 6)
8. **6-Digit One-Time Connect Code + Visibility Controls** (Gaps 13 + 15)
9. **Real-Time WebSockets / SSE Event Channel** (Gap 10)
10. **Hybrid Encrypted Transport (mTLS for Desktop/CLI + Opt-In Local HTTPS/E2EE)** (Gap 11)

*(Plus Gap 19 Two-Way Crowd Rooms & Gap 22 `mtime` Preservation as natural low-effort companions.)*

---

### 9.1 Architectural Feasibility (Go, Wails v2, Svelte 5, TypeScript)

Flitnix's existing stack (**Go stdlib HTTP + Wails v2 + Svelte 5 + TypeScript**) is ideal for all 10 features. **No framework replacement is warranted.**

#### 1. Resumable Chunked Uploads + Auto-Retry & Crash Recovery
- **Architecture Fit**: Directly restores the missing resumable layer cleanly inside `internal/transfer` without touching Wails or breaking single-pass memory bounds (`256 KiB` `sync.Pool`).
- **Go Packages**: Extend `internal/transfer` (new `resumable.go` + `quota.go`), `internal/server` (new `tus_handlers.go` or `PATCH`/`HEAD` on `/api/batches/{batchID}/items/{itemID}/content`), and enable `RecoveryDir: filepath.Join(resolveDataDir(), "recovery")` in `app.go` & `cli.go`.
- **API & WS**: `HEAD /api/batches/{b}/items/{i}/content` returns `Upload-Offset`, `Upload-Length`; `PATCH` accepts `Upload-Offset` and streams up to EOF, checkpointing `.meta` periodically (e.g., every 32 MiB) and verifying full SHA-256 before `os.Link` commit. `Coordinator` adds `ItemStatePaused` and `ResumeItem`.
- **Svelte & Mobile**: Update `PersonalTransferView.svelte` progress cards; update `upload-worker.js` / `upload-client.js` to slice `File.slice(offset)` and retry on socket error with exponential backoff.
- **Risks**: Must preserve `ReserveCancel` vs `pubPublishing` lock ordering and truncate uncommitted `.part` tails beyond durable `.meta` offset on startup (`internal/recovery`).

#### 2. Resumable Byte-Range Downloads + Shared Folder File Browser
- **Architecture Fit**: Extends `internal/server/archive.go` and `internal/transfer/registry.go` (`SharedSource` already indexes every `SourceItem`!).
- **API**: Add `GET /api/sources/{id}/items` returning sanitized tree metadata + opaque `itemId`; extend `POST /api/download-grants` to accept `{ sourceIds, itemIds }`. In `archive.go` Tier 2, compute a weak/strong validator `ETag` from `inode + size + mtime_ns` so `http.ServeContent` can serve `206 Partial Content` safely while aborting if the file mutates.
- **Svelte & Mobile**: No desktop changes needed; `app.js` renders expandable folder rows with checkboxes per file and tracks resumed download bytes.
- **Risks**: File mutation between range requests (mitigated by strict `If-Range` ETag validation).

#### 3. Automatic LAN Discovery + Desktop-to-Desktop GUI Sender
- **Architecture Fit**: New package `internal/discovery` using pure Go UDP multicast / mDNS (`224.0.0.251:5353` + lightweight fallback beacon on a dedicated LAN UDP port), strictly bound to the chosen `netenv` interface IP (never VPN TUNs).
- **Go Packages**: New `internal/discovery`; extend `app.go` to wrap `internal/client` (`SendToPeer`, `ListNearbyPeers`, `PairWithPeer`); extend `internal/server/routes.go` with unauthenticated `GET /api/ping-info` (public HostID fingerprint + display name + OS + TLS capability, zero secrets) or mDNS TXT records.
- **Svelte**: `PersonalTransferView.svelte` adds a "Nearby Devices" strip above or beside the QR hub; dragging files onto a peer card invokes `SendToPeer`.
- **Risks**: Corporate/public Wi-Fi multicast blocking (mitigated by keeping QR + 6-digit code fallback) and Windows Defender Firewall prompts on UDP 5353.

#### 4. Mobile Browser Trusted Pairing & One-Tap Reconnect
- **Architecture Fit**: Completes `internal/devices` (`PairingCoordinator`, `devices.json`) for the browser client.
- **API**: Add unauthenticated rate-limited `POST /api/device-sessions/challenge` and `POST /api/device-sessions/verify` in `routes.go` (outside `requireSession`), issuing a real `session.Session` Bearer token after verifying Ed25519 signature + `HostID` + nonce.
- **Svelte & Mobile**: Wire `PairingCoordinator.OnPairingPrompt` in `app.go` to `EventsEmit(ctx, "pairing:prompt", ...)` so a global approval modal appears regardless of active tab. Embed pure-JS Ed25519/X25519 in `internal/server/assets/` (since `crypto.subtle` is disabled on HTTP origins!) storing keys in `IndexedDB`.
- **Risks**: `IndexedDB` eviction in Safari Private Browsing (warn user clearly); pure-JS crypto size (~15 KiB gzipped).

#### 5. System Tray / Menu Bar, Background Receiving & Autostart
- **Architecture Fit**: Wraps `main.go` `wails.Run` with `HideWindowOnClose: true` + native tray (`fyne.io/systray` or Wails tray integration) and new `internal/autostart` package.
- **Platform Code**: Small OS-specific adapters in `internal/autostart` (`autostart_windows.go`, `autostart_darwin.go`, `autostart_linux.go`) and tray menu (`Show Flitnix`, `Visibility: Trusted Only / 10m Everyone / Off`, `Quit`).
- **Svelte**: Add toggles in `SettingsView.svelte`.
- **Risks**: Linux Wayland/GNOME AppIndicator extension variability (graceful fallback if tray DBus unavailable); ensure `Quit` in tray calls `app.shutdown` cleanly while window close `OnBeforeClose` only hides to tray when enabled.

#### 6. Native OS Desktop Notifications
- **Architecture Fit**: New `internal/notify` package triggered by `app.go` callbacks (`OnBatchPending`, `OnPairingPrompt`, `OnUploadCompleted`).
- **Platform Code**: Reuse `github.com/godbus/dbus/v5` (already in `go.mod`!) for Linux `org.freedesktop.Notifications` with action buttons (`Approve` / `Open`), `git.sr.ht/~jackmordaunt/go-toast/v2` (already in `go.mod`!) for Windows, and `osascript` / native Cocoa helper for macOS.
- **Risks**: Notification spam during 100-file batches (mitigated by notifying **once per batch**, never per item).

#### 7. Persistent Personal Transfer History & Actionable File Management
- **Architecture Fit**: New `internal/history` append-only JSONL store (`<dataDir>/transfer_history.jsonl`, modeled directly on `internal/audit/audit.go`, capped at e.g. 5,000 entries + retention days) or unified `audit.jsonl` schema v2.
- **Go**: Record terminal events from `Service.SubmitContentWithPublish`, `Coordinator.CompleteItem`/`FailBatch`, and `DownloadRegistry.FinishWithDigest`. Add `OpenHistoryFile` / `RevealHistoryFile` (`explorer /select,`, `open -R`, `dbus FileManager1.ShowItems` / `xdg-open` parent) and `ReshareHistoryFile`.
- **Svelte**: Connect `TransferHistoryView.svelte` directly to `QueryTransferHistory(filter)` with device/date/type/status filters and summary stats.

#### 8. 6-Digit One-Time Connect Code + Visibility Controls
- **Architecture Fit**: Extend `internal/session` with an optional short-lived `ShortCode` (`6` digits, `2m` TTL, single-use) protected by `PairingCoordinator`-style global 5-failure lockout. Add `VisibilityMode` (`off`, `trusted`, `temporary_10m`, `everyone`) in `internal/settings` + `app.go`.
- **API**: `POST /api/connect-code` exchanges `code` for session Bearer token (IP-throttled + global lockout); `GET /` landing page offers code entry if opened without `#token`.
- **Risks**: Brute-force on 6-digit space (strictly mitigated by 5-attempt / 15-min lockout + 2-minute TTL).

#### 9. Real-Time WebSockets / SSE Event Channel
- **Architecture Fit**: Replace polling in `app.js`, `guest.js`, and `App.svelte`. Use `runtime.EventsEmit` for Go→Svelte desktop updates, and SSE (`text/event-stream`) or WebSocket (`gorilla/websocket`, already in `go.mod`!) for Server→Mobile updates (`batch:updated`, `text:added`, `pair:decided`, `session:ended`).
- **Security**: For WebSockets/SSE, authenticate via short-lived one-time WS ticket obtained via `POST /api/events/ticket` (Bearer-authenticated) or `Authorization` header (for SSE `fetch`-stream) so tokens **never** appear in URLs or server logs!
- **Risks**: Safari/iOS dropping idle WS connections on screen lock (keep automatic `visibilitychange` + fallback poll reconcile).

#### 10. Hybrid Encrypted Transport (mTLS for Desktop/CLI + Opt-In E2EE/HTTPS)
- **Architecture Fit**: Implements the ADR in `docs/TRANSPORT_ENCRYPTION_EVALUATION.md` Section 4.2.
- **Go**: Generate an X.509 certificate wrapping `HostIdentity` Ed25519 key (`internal/devices`), serve TLS 1.3 on a companion port (e.g. `43822` or ALPN/SNI) for `internal/client` (`flitnix send`, `flitnix pair`, Desktop↔Desktop) with pinned `HostID` verification (`VerifyPeerCertificate`), eliminating MITM and eavesdropping without WebPKI friction. For mobile browsers, offer opt-in application-layer stream chunk AEAD (XChaCha20-Poly1305 / AES-GCM via embedded JS + Go middleware) for paired devices, plus optional HTTPS toggle for advanced users with local CA.

---

### 9.2 UX & Product Design for the Top 10 Features

Design principle: **"AirDrop's zero-thought calm on the surface; Flitnix's transparency and control one click below."**

1. **Unified "Connect & Share" Hub (`PersonalTransferView.svelte`)**:
   - **User Journey**: Open Flitnix (or click tray icon). Top strip shows **Nearby Devices** (auto-discovered laptops/paired phones as clean pill cards with OS icon, name, and `Trusted` shield). Right beside/below is the **Phone QR + 6-Digit Code** card (`482 915`) and a **Visibility Pill** (`Trusted Only` | `Everyone (10m)` | `Off`).
   - **Interactions**: Drag files onto a Nearby Device card → sends immediately; or drop into "Shared Files" → any connected phone/browser can browse folders and pull files.
   - **Empty / Loading / Error / Success States**:
     - *Empty*: *"Searching local network… Scan the QR or enter `482 915` on another device."*
     - *Loading*: Pulsing radar ring + `"Connecting (TLS Pinned)…"`
     - *Error*: Inline Connection Doctor link (*"Windows Firewall may be blocking port 43821 — Fix it"*).
     - *Success*: Crisp toast + progress bar in Active Transfers dock.

2. **Seamless Resumable Transfers & Roaming Recovery (Desktop & Mobile)**:
   - **Mobile Journey**: User selects a 20 GB 4K video. Phone uploads chunk-by-chunk in Web Worker. Halfway through, user walks upstairs and Wi-Fi flickers.
   - **States**: Status pill smoothly shifts from `Uploading 48% · 42 MB/s` → amber `Paused at 9.6 GB — waiting for Wi-Fi…` → auto-resumes `Uploading 49%` without losing a single megabyte. If desktop restarts, reopening Flitnix shows `Interrupted (9.6 / 20.0 GB) [Resume] [Discard]`.

3. **Browseable Shared Folders (`app.js`)**:
   - **Mobile Journey**: Desktop shares `Vacation_2026/` (400 photos, 6 GB). Mobile lists `📁 Vacation_2026 (400 items · 6.0 GB)` with `[Download ZIP]` and `[Browse]`. Tapping `Browse` expands the folder tree so the user selects 3 specific photos and taps `Download Selected (3)` — no 6 GB ZIP required!

4. **One-Tap Paired Mobile Reconnect + Global Pairing Prompt**:
   - **Mobile Journey**: After scanning QR once, user taps `"Trust this PC"` → verifies matching 6-digit SAS (`491 027`) on both screens → saves bookmark/PWA to Home Screen. Next week, tapping the Flitnix icon immediately shows `"Reconnect to Robin-Laptop"` with one tap — no QR scan!
   - **Desktop**: Global modal pops up from *any* tab (or OS notification if minimized) showing `"iPhone 16 wants to pair — Code: 491 027 [Allow] [Deny]"`.

5. **Always-Ready System Tray + Native OS Notifications**:
   - **Journey**: Closing the window minimizes to the system tray (optional badge dot when transfers are pending). When a paired phone starts sending a file, an OS notification pops up: **"Pixel 9 wants to send 4 photos (18.4 MB)"** with `[Accept]` and `[Open Flitnix]` buttons.

6. **Actionable Persistent Transfer History (`TransferHistoryView.svelte`)**:
   - **Journey**: Open History tab days later. See grouped timeline (`Today`, `Yesterday`, `Last Week`) with device badge (`From Pixel 9`), duration & avg speed (`1.2 GB in 24s · 50 MB/s`), and buttons `[Open]`, `[Reveal File]`, `[Send Again]`, plus `[Clear History]` confirmation.

---

## 10. Features We Should NOT Blindly Copy

Evaluating competitor features that would **harm** Flitnix's core philosophy and competitive moat:

| Competitor Feature | Why Competitors Do It | Why Flitnix MUST Reject It |
|---|---|---|
| **Mandatory User Accounts / Login** (AirDroid, Blip email verification, Quick Share "Your Devices") | Enables cloud device graphs, billing, and telemetry | Destroys zero-friction offline/classroom/guest use and creates an unnecessary PII/auth surface. Keep cryptographic local Ed25519 keys instead. |
| **Mandatory Cloud Storage / Default Cloud Relay** (Send Anywhere links, Quick Share QR links, AirDroid remote) | Bridges NATs and enables async links | Violates Flitnix's privacy & zero-cloud promise, introduces hosting costs, legal/DMCA liability, and slow upload-then-download bottlenecks. Any remote relay must remain an **opt-in, self-hostable** P2P/TURN fallback (P2). |
| **Advertising & Nagging Upgrade Popups** (Send Anywhere Free, AirDroid Free) | Monetizes free tier | Erodes trust immediately in privacy-sensitive open-source software. |
| **Analytics, Telemetry & Phone-Home Trackers** | Product metrics | Explicitly banned by `CONTRIBUTING.md:53` and `README.md:160`; violates user trust. |
| **Forced File Compression / Silent EXIF Stripping** (Messaging apps, AirDroid web free limits) | Saves relay bandwidth & server cost | Ruins original-quality photos/4K videos; compression must *only* ever be explicit, opt-in, and labeled. |
| **Subscription Paywalls on Basic Features** (AirDroid 30 MB/folder paywalls, Send Anywhere password paywalls) | SaaS revenue | Flitnix is AGPL-3.0 open-source; gating folder transfers or security (passcodes) behind paywalls alienates the community. |
| **Heavy Always-On Background Battery-Draining Daemons** (AirDroid, KDE Connect Doze issues) | Continuous SMS/notification/camera/clipboard polling | Drains laptop/phone batteries and turns a crisp transfer tool into bloatware. Background tray mode must idle at ~0% CPU with event-driven sockets/mDNS. |
| **Remote Camera, SMS Mirroring, Virtual Touchpad & Screen Mirroring** (AirDroid, KDE Connect) | Full phone-companion suite | Turns Flitnix into a complicated remote-desktop / MDM app, massively expanding OS permission demands (accessibility, screen recording, SMS) and maintenance burden across 5 OSes. Keep Flitnix laser-focused on **files, folders, drops, and quick text/clips**. |
| **Excessive Permissions** (Location/Contacts/Call Logs required just for LAN sharing) | OS BLE/Wi-Fi APIs or contact-list matching | Breaks privacy positioning; QR/mDNS/Ed25519 needs zero access to Contacts or GPS. |

---

## 11. Security Gap Assessment

Flitnix's **application-layer, session, and filesystem security** is remarkably thorough (`docs/phase5-security-audit.md`, `internal/transfer/sanitize.go`, `internal/devices/pairing.go`, `internal/server/routes.go`):
- Host-header DNS-rebinding protection (`isAllowedHost`), strict CSRF `Sec-Fetch-Site` / `Origin` / `Referer` checks, strict CSP (`worker-src 'self'`, `frame-ancestors 'none'`), `DisallowUnknownFields()` + `MaxBytesReader`, Windows reserved device name & trailing dot/space blocking, ANSI/bidi device-name stripping, non-destructive `os.Link` publication, SHA-256 manifest signing, and HMAC-authenticated durable lockout (`pairing_security.json`).

**However, five real security gaps exist in the current implementation:**

1. **Plaintext HTTP Transport on Shared Networks (Critical)**:
   - As verified in `server.go:393`, all traffic (uploads, downloads, Quick Text, `flitnix_guest_token` cookies, `flitnix-download-grant` cookies, and `Authorization: Bearer` headers) travels in **cleartext** over Wi-Fi/LAN.
   - *Nuance*: While `docs/TRANSPORT_ENCRYPTION_EVALUATION.md` rightly notes that self-signed HTTPS causes browser interstitial warnings for ad-hoc phones on RFC 1918 IPs, **CLI and Desktop-to-Desktop peers have zero browser-warning excuse** and currently suffer unnecessary plaintext exposure. Even on mobile browsers, paired sessions can layer AEAD encryption or opt-in local HTTPS.
2. **Unwired Recovery Journal & Missing Disk-Quota Pre-Check (Medium)**:
   - `RecoveryDir` is unused in production, and there is no disk-space `QuotaManager` in `internal/transfer/` (meaning an approved/auto-approved upload can fill 100% of the host's disk).
3. **Trusted Device Endpoints Sit Behind Ephemeral Session Gate (Medium)**:
   - `TrustedDevicesView.svelte` promises QR-less convenience, yet `app.js` has no pairing client, and `/api/auth/challenge` sits behind `requireSession`. Additionally, `PairingCoordinator` only allows one `active` pairing handshake at a time (`ErrPairingInProgress`), which is safe against DoS but needs clean UX feedback if two devices try simultaneously.
4. **No Rate Limiting on `/guest-api/join` & Unauthenticated Routes (Low-Medium)**:
   - While `PairingCoordinator` has strong 5-failure / 15-min lockout, `/guest-api/join` (`guest_routes.go`) validates `InviteSecret` and caps `MaxGuestsPerRoom = 256` without per-IP rate limiting on wrong-secret probes. Adding a token-bucket / failure backoff on bad invite secrets and future 6-digit codes is essential.
5. **Code Signing & Notarization Pending (Distribution Security)**:
   - `README.md:131-132` notes that Windows Authenticode signing and macOS Apple Developer ID notarization are pending, causing SmartScreen / Gatekeeper warnings on fresh installs.

---

## 12. Performance & Reliability Improvement Opportunities

1. **Eliminate Byte-0 Upload Restarts (Highest Reliability Win)**: Implement resumable `PATCH`/`HEAD` uploads (`Gap 1`) + automatic exponential-backoff retry (`Gap 7`) so 10–100 GB transfers survive Wi-Fi roaming, router handoffs, and browser hiccups.
2. **Enable `Accept-Ranges: bytes` for Tier 2 Mutable Downloads (`Gap 2`)**: Stops 10–100 GB mobile downloads from dying permanently on transient network drops.
3. **Fix Buffer Allocation in `streamBody` (`internal/transfer/stream.go:81`)**: `streamBody` currently calls `make([]byte, 64*1024)` instead of reusing `GetCopyBuffer()` (`256 KiB` from `pool.go`), missing out on both zero-alloc pooling and larger 256 KiB syscall efficiency for multi-gigabit LAN/NVMe writes.
4. **Replace 500–2500 ms HTTP/IPC Polling with WebSockets / SSE + Wails Events (`Gap 10`)**: Cuts ~1.2s approval start latency to `<20ms`, eliminates thousands of idle HTTP requests, and lowers mobile battery consumption.
5. **Pre-Flight SHA-256 Option / Adaptive Concurrency for Small Files (`Gap 16`)**: Uploading 1,000 small photos sequentially pays 1,000 × (HTTP round-trip + `PartFile.Sync()` + `Poll` latency). Allowing 3–4 concurrent workers for files `< 10 MiB` (while keeping large files sequential to preserve disk I/O locality and bounded RAM `< 4 MiB`) can speed up photo-album uploads by 3x–5x.
6. **Direct Folder Item Browsing (`Gap 31`)**: Avoids generating and streaming unnecessary multi-GB ZIP archives when the phone only needs specific files from a shared folder.
7. **Wire `RecoveryDir` in Production (`app.go` / `cli.go`)**: Activates the already-tested HMAC WAL (`internal/recovery/store.go`) so desktop crashes/updates don't lose active batches.
8. **HTTP/2 or Keep-Alive Tuning for TLS Peer Transport (`Gap 11`)**: Use `net/http` `Transport` connection pooling in `internal/client` (`MaxIdleConnsPerHost`) + TLS 1.3 session resumption.

---

## 13. Estimated Development Complexity & Recommended Next Major Release Scope

### Estimated Development Complexity
- **Low (2–5 days each)**: `mtime` preservation (Gap 22), Smart Subfolder Organization (Gap 25), Mobile Theme/i18n/Filter (Gap 29), Tier 2 `Range` downloads (Gap 2), Shared Folder File Browser (Gap 31).
- **Medium (1–2 weeks each)**: Persistent Transfer History (Gap 6), OS Firewall Doctor + NSIS rule (Gap 27), Native OS Notifications (Gap 8), 6-Digit Connect Code + Visibility Modes (Gaps 13 & 15), Persistent Mobile Pairing (Gap 4), WebSockets/SSE Push (Gap 10), mDNS Discovery + Desktop GUI Sender (Gaps 3 & 9), System Tray & Autostart (Gap 5), Crowd Two-Way Rooms (Gap 19), PWA Share Target (Gap 12), Storage Quota Check (Gap 26), Duplicate Detection (Gap 18).
- **High (2.5–6 weeks each)**: Resumable Chunked Uploads + Roaming Auto-Retry (Gaps 1 & 7), Hybrid mTLS / E2EE Transport (Gap 11), Adaptive Concurrency (Gap 16), Opt-In WebRTC / Self-Hosted Relay (Gap 20), Watch Folders / Incremental Sync (Gap 30).

### Recommended Next Major Release Scope (`v0.3.0` — "Seamless & Unbreakable")
Focus `v0.3.0` strictly on closing the highest-impact gaps without bloating the app:
1. **Unbreakable Large Transfers**: Resumable chunked uploads + auto-retry (`Gaps 1, 7`) + `HTTP 206` range downloads (`Gap 2`) + wire `RecoveryDir` + pre-approval disk quota check (`Gap 26`) + `mtime` preservation (`Gap 22`).
2. **Zero-Friction Discovery & Reconnection**: mDNS/UDP LAN Discovery (`Gap 3`) + Desktop-to-Desktop GUI Sender (`Gap 9`) + 6-Digit Connect Code (`Gap 13`) + Mobile Browser Trusted Pairing & One-Tap Reconnect (`Gap 4`) + Device Visibility Controls (`Gap 15`).
3. **Always-Ready Desktop & Instant Feedback**: System Tray / Autostart (`Gap 5`) + Native OS Notifications (`Gap 8`) + WebSocket/SSE real-time push (`Gap 10`) + Firewall Doctor check (`Gap 27`).
4. **Everyday Usability**: Persistent Personal Transfer History with Open/Reveal/Resend (`Gap 6`) + Browseable Shared Folders (`Gap 31`).

*(Defer **Hybrid mTLS/E2EE**, **PWA Share Target**, **Media Thumbnails**, **Opt-in Clipboard Sync**, and **Two-Way Crowd Rooms** to `v0.4.0`, and **WebRTC Self-Hosted Relay** to `v0.5.0`.)*

---

## 14. Final Question: Which 10 Features Should We Build, Why, and in What Order?

If we could implement **only 10 features** to make Flitnix dramatically better than it is today, here is the exact prioritized build order and rationale:

1. **Resumable Chunked Uploads & Auto-Retry with Exponential Backoff (plus `RecoveryDir` activation)**
   - *Why #1*: Reliability is Flitnix's #1 promise, yet right now a single Wi-Fi hiccup or phone screen lock restarts a 20 GB upload from byte 0. This solves Flitnix's biggest weakness and beats **both LocalSend and PairDrop**, neither of which has reliable resumable transfers.
2. **Resumable Byte-Range Downloads (`Accept-Ranges: bytes`) + Browseable Shared Folders**
   - *Why #2*: Low-complexity, massive payoff on the Desktop→Phone path. Fixes failed multi-GB downloads on mobile browsers and stops forcing users to download an entire 30 GB folder `.zip` just to grab two files.
3. **Persistent Personal Transfer History (with Open File, Reveal Exact File, Resend & Clear)**
   - *Why #3*: The `Transfer History` tab already exists in Svelte (`TransferHistoryView.svelte`), yet losing all personal transfer records on restart breaks user trust. High user delight for ~1 week of engineering.
4. **Mobile Browser Trusted-Device Pairing & One-Tap Reconnect**
   - *Why #4*: The Ed25519 + 6-digit SAS backend (`internal/devices`) and `TrustedDevicesView.svelte` are already built and advertised, yet completely missing from `app.js`. Completing this removes daily QR scanning for personal phones.
5. **System Tray / Menu Bar Mode & OS Autostart on Login**
   - *Why #5*: Paired devices cannot one-tap reconnect if Flitnix quits whenever you close the window. Running silently at ~0% idle CPU in the tray turns Flitnix into an always-ready AirDrop/LocalSend alternative.
6. **Native OS Desktop Notifications (with Batch Approval & Completion Alerts)**
   - *Why #6*: Required companion to System Tray mode—ensures users never leave their phone hanging on *"Waiting for approval on the desktop…"* while Flitnix is minimized or behind other apps.
7. **Automatic LAN Device Discovery (mDNS/Multicast) + Desktop-to-Desktop GUI Sender**
   - *Why #7*: Unlocks effortless PC↔PC / Mac↔Linux / Laptop↔Desktop transfers directly inside the GUI using Flitnix's existing `internal/client`, eliminating manual URL copy-paste.
8. **6-Digit One-Time Connect Code + Device Visibility Controls (`Trusted Only` / `10m Everyone` / `Off`)**
   - *Why #8*: Makes camera-less laptop/cross-device pairing instant (`482 915`) while giving always-on tray/discovery users AirDrop-grade protection against unsolicited LAN popups.
9. **Real-Time WebSockets / SSE Event Push (Replacing 500–2500 ms Polling)**
   - *Why #9*: Makes approvals, Quick Text, and live progress feel instantaneous (`<20ms`) across desktop and mobile while eliminating wasteful background polling and saving mobile battery.
10. **Hybrid Encrypted Transport (Mandatory mTLS for Desktop/CLI Peers + Opt-In E2EE/Local HTTPS)**
    - *Why #10*: Closes the plaintext HTTP security gap against LocalSend, KDE Connect, and Blip on shared Wi-Fi networks—pinning Ed25519 `HostIdentity` keys directly for app/CLI peers without inflicting untrusted-certificate warnings on casual QR guests.

---

### Assumptions & Unknowns Noted
- **Physical mobile & Wi-Fi validation**: As documented in `README.md:133-134`, physical multi-room RF Wi-Fi roaming and iOS/Android hardware matrix tests are not yet executed in-repo; all Flitnix behavior claims above are verified directly from source code (`app.go`, `cli.go`, `internal/*`, `frontend/src/*`, `internal/server/assets/*`).
- **Competitor proprietary internals**: For closed-source tools (AirDrop, Quick Share, AirDroid, Send Anywhere, LANDrop v2.8+, Blip), exact internal checksum/chunking details not published in official documentation are marked `UNKNOWN` or noted from vendor docs and public issue trackers rather than guessed.
