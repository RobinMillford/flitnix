# Flitnix Architecture

Flitnix is an offline-first desktop application designed for private, high-speed local file transfer between computers and mobile devices without cloud intermediaries, external accounts, or mobile app installations.

---

## 1. High-Level System Overview

```
+-------------------------------------------------------------------------+
|                              FLITNIX DESKTOP                            |
|                                                                         |
|  +-------------------------------------------------------------------+  |
|  |                 Frontend (Svelte 5 + TypeScript)                  |  |
|  |  [Personal Transfer]   [Guest Drop Box]   [Connection Doctor]     |  |
|  +-------------------------------------------------------------------+  |
|                                  | (Wails v2 IPC Bindings)              |
|                                  v                                      |
|  +-------------------------------------------------------------------+  |
|  |                        Desktop Backend (Go)                       |  |
|  |  - app.go (Wails Service Layer)                                   |  |
|  |  - internal/netenv (Network Adapter & Wi-Fi SSID Detection)       |  |
|  |  - internal/transfer (Two-Phase Publication Engine)               |  |
|  |  - internal/guest (Multi-Guest Drop Box Room Manager)             |  |
|  |  - internal/doctor (7-Point Connection Diagnostics Engine)        |  |
|  |  - internal/textmsg (Bounded Plaintext & Link Store)              |  |
|  |  - internal/server (Local HTTP Server, Route Handlers, Assets)    |  |
|  +-------------------------------------------------------------------+  |
+-------------------------------------------------------------------------+
                                   |
                         Local Wi-Fi or Hotspot
                                   |
         +-------------------------+-------------------------+
         |                                                   |
         v                                                   v
+-------------------------------+           +-------------------------------+
|     Personal Phone Browser    |           |    Guest Phone(s) Browser     |
|   (/ endpoint with Bearer)    |           |    (/guest with Capability)   |
|  - Full bidirectional files   |           |  - Upload-only file drop      |
|  - Real-time text & links     |           |  - Per-guest isolated folder  |
|  - Real-time speed & ETA      |           |  - Progress, speed & ETA      |
+-------------------------------+           +-------------------------------+
```

---

## 2. Subsystems

### 2.1 Personal Transfer Subsystem (`internal/transfer`, `internal/session`)
- **Single-Pairing Ephemeral Token**: Scanning the desktop pairing QR loads the mobile web app with a 43-character URL-safe base64 token in the fragment (`#token`). The fragment is immediately stripped from the address bar, and the token is supplied strictly via the HTTP `Authorization: Bearer <token>` header.
- **Two-Phase Publication Reservation**: When receiving files from a phone, incoming content streams into temporary staging `.part` files. Upon stream completion, the server checks whether the file transfer was withdrawn by the client; if not, it atomically renames the staging file into the destination folder. If a cancellation races with a successful commit, completed files are safely kept and never corrupted or left in limbo.
- **Batch Transfer Contract**: Handles multi-file and folder hierarchy streaming with atomic item status updates, avoiding orphaned temporary files.

### 2.2 Guest Drop Box Subsystem (`internal/guest`)
The Guest Drop Box turns Flitnix into an upload-only collection room for classrooms, meetings, events, or workshops.

1. **Room Lifecycle**: Rooms transition deterministically through `Starting` → `Active` → `Expiring` (last 5 minutes) → `Expired` → `Ended`. A background manager sweeps expired rooms.
2. **Capability Secret**: The room invite URL embeds a random 32-character capability secret in the URL fragment (`/guest#<secret>`).
3. **Dual Approval Gate**:
   - **Guest Gate**: When a guest enters their nickname and submits a join request (`POST /guest-api/join`), they enter the host's **Pending Guests** approval queue. The host must explicitly approve or decline the guest.
   - **Upload Gate**: For every file a guest queues, a declaration (`POST /guest-api/uploads`) is created in the host's **Upload Inbox**. The host reviews the filename, size, and guest nickname before authorizing the upload.
4. **Per-Guest Token & Cookie**: Approved guests receive a random cryptographically secure token set as an `HttpOnly`, `SameSite=Lax` cookie (`flitnix_guest_token`) and mirrored in memory for `Authorization: Bearer` fallback.
5. **Destination Isolation**:
   ```
   <Chosen Destination>/
     └── Flitnix Guest Drop/
         └── <Room Date and ID>/
             ├── <Guest1 ID>-<Sanitized Nickname>/
             │   └── presentation.pdf
             └── <Guest2 ID>-<Sanitized Nickname>/
                 └── photo.jpg
   ```
   Each guest has an isolated `transfer.Store` scoped to their private directory. Directory creation is lazy (created only upon first approved write), preventing empty folder clutter from idle or rejected guests.
6. **Strict Upload-Only Policy**: Guest endpoints only support upload declarations, upload status polling, and chunked PUT streaming. Guests have zero access to desktop shared files or other guests' files.

### 2.3 Connection Doctor Subsystem (`internal/doctor`)
The Connection Doctor diagnoses why a mobile device cannot connect to the desktop. It runs 7 automated checks:

| Check ID | Diagnostic Test | Failure / Warning Condition | Remediation Guidance |
| :--- | :--- | :--- | :--- |
| `adapter_presence` | Network interfaces | No active network adapter found | Connect to a Wi-Fi network or Ethernet cable. |
| `ipv4_address` | IPv4 configuration | No valid, non-loopback IPv4 address | Ensure DHCP assigned an IP address or configure static IP. |
| `port_available` | Port 43821 availability | Port 43821 already bound by another process | Close conflicting application or restart Flitnix. |
| `server_health` | Local HTTP health probe | `GET http://127.0.0.1:43821/healthz` fails | Restart session or verify firewall rules. |
| `connection_medium`| Wi-Fi SSID / Hotspot | Connected over public/enterprise Wi-Fi | Warn about client isolation; advise using mobile hotspot. |
| `host_reachability`| Host address binding | Bound address not in valid CIDR | Select matching network adapter from dropdown. |
| `multihomed_routing`| Multiple network adapters | Multiple active adapters (e.g. VPN + Wi-Fi) | Choose adapter matching the phone's Wi-Fi network. |

Reports categorize overall health as `PASS`, `WARNING`, or `FAIL` and provide one-click triggers for **Recheck** and **Regenerate QR**.

### 2.4 Quick Text & Link Sharing Subsystem (`internal/textmsg`)
- **Bounded In-Memory Store**: Enforces a strict 64 KiB maximum payload size per message.
- **UTF-8 Validation**: Rejects invalid UTF-8 byte sequences.
- **XSS & Injection Protection**: Renders content strictly as plain text without HTML parsing.
- **Protocol-Strict Link Detection**: Scans messages for URLs; links are accepted only if they strictly begin with `http://` or `https://` (blocking `javascript:`, `data:`, or `file:` schemes). Links open safely with `rel="noopener noreferrer"`.
- **Bidirectional Synchronization**: Supported on both desktop and mobile web interfaces with instantaneous clipboard copying.

### 2.5 Transfer Progress & Recovery Subsystem
- **Smoothed Speed Calculation**: Uses an Exponential Moving Average (EMA) with rolling deltas ($\alpha = 0.3$) over a minimum 250ms sampling window to filter out TCP burst noise:
  $$\text{speed}_{\text{new}} = 0.7 \cdot \text{speed}_{\text{prev}} + 0.3 \cdot \text{speed}_{\text{instant}}$$
- **ETA Computation**: Calculates accurate remaining time estimates:
  $$\text{ETA} = \frac{\text{Bytes}_{\text{total}} - \text{Bytes}_{\text{transferred}}}{\text{speed}_{\text{smoothed}}}$$
- **Resumable Chunked Uploads & Crash-Safe Recovery (`HEAD` / `PATCH`)**: Resumable clients discover the server's authoritative committed byte offset via `HEAD .../content` (`Upload-Offset`, `Upload-Length`, `Upload-Chunk-Size`) and stream bounded sequential slices via `PATCH .../content` (`Content-Type: application/offset+octet-stream`). Each committed chunk is persisted in crash-safe order (`write .part` → `fsync .part` → `write .meta.tmp` with HMAC-SHA256 using an installation-derived `staging-meta-v1` subkey and incremental SHA-256 hash state → `fsync .meta.tmp` → atomic rename `.meta` → `syncDir`) before acknowledging `204 No Content`.
  - **Mid-Chunk Rollback & Sibling Preservation**: If a chunk drops before completion, uncommitted tail bytes are truncated back to the last durably committed `.meta` offset and the coordinator marks the active item `interrupted` while leaving sibling batch items queued intact.
  - **Phases 6–10 Startup Recovery**: `Server.Recover` binds `RecoveryDir` across desktop (`App`) and headless CLI (`flitnix serve`) modes, truncates torn uncommitted tails, cross-validates `.meta` against active sessions and durable coordinator batches, quarantines corrupted/truncated staging artifacts individually to `RecoveryDir/quarantine`, reconstructs storage quota reservations, and purges expired/orphan staging data cleanly.
  - **Automatic Stream Retry & Wi-Fi Roaming Recovery (G7)**: Transient transport drops, `408` stalls, `502`/`503`/`504` responses, and `chunk_interrupted` faults trigger bounded jittered exponential backoff (`1s`, `2s`, `4s`, `8s`, `15s` with ±20% jitter, plus browser `online` wakeup), `/ping` reachability checks, and `HEAD` `Upload-Offset` reconciliation in both the mobile client and `internal/client`. Permanent faults (`401`, `404`, `507`, `canceled`, `discarded`, `rejected`, `quarantined`, `digest_mismatch`) fail fast without retrying. Desktop `handleNetworkChange` preserves sessions and resumable staging across temporary Wi-Fi disconnects and IP roaming.
  - **Legacy Single-Pass `PUT`**: Direct single-pass `PUT .../content` streams remain supported for backward compatibility, keeping fail-closed cleanup if interrupted mid-PUT.

### 2.6 Outbound File Integrity Subsystem (`internal/transfer`, `internal/server`)
- **RFC 9530 Structured Field Syntax**: Outbound file and archive downloads emit standard `Content-Digest: sha-256=:<base64>:` headers along with exact `Content-Length` headers, allowing HTTP clients, scripts, and browsers to verify byte integrity without custom protocols.
- **Two-Pass Deterministic Archive Streaming**: Multi-file and folder downloads stream uncompressed ZIP archives (`zip.Store`). To emit an exact `Content-Digest` header prior to transmitting the HTTP body without staging multi-gigabyte temporary files on disk or buffering in RAM, Flitnix uses a deterministic two-pass pipeline:
  1. **Pass 1 (Pre-computation)**: Inspects archive entries, normalizes paths, and truncates timestamps to 2-second resolution (`ModTime.Truncate(2 * time.Second)` for DOS date-time determinism). Streams the exact ZIP headers and entry contents directly into `sha256.New()`, recording the exact archive size and SHA-256 digest with bounded 64 KiB memory.
  2. **Pass 2 (HTTP Response Streaming)**: Sets `Content-Digest` and `Content-Length` headers, then streams the identical ZIP structure directly into the HTTP response socket. An inline tee-hasher verifies the transmitted bytes match the Pass 1 digest; if underlying disk files mutate mid-stream, the server immediately aborts the connection.
- **Single-File Direct Downloads**: Pre-hashes the source file using 64 KiB bounded buffers before emitting `Content-Digest`, then verifies transmission inline.
- **Browser-Side Integrity Verification**:
  - **Streaming File Sink (`showSaveFilePicker`)**: In supported environments (such as Chromium desktops), the browser streams received chunks through `js-sha256` directly into the filesystem handle without in-memory buffering.
  - **Native Download Fallback (`iframe` / direct trigger)**: Standard mobile browsers on plain HTTP local networks do not support the File System Access API. Flitnix routes the download through the browser's native download manager to ensure reliable file saving without memory bloat. A dedicated **Download Integrity** card displays the expected SHA-256 hash with one-click copy, and provides a **Verify Downloaded File** button allowing the user to select the downloaded file from their Downloads folder for zero-install, bounded-memory verification via `File.stream()` (64 KiB chunks).
  - **Client Verification States**: The UI cleanly reports states: `waiting`, `downloading`, `verifying`, `verified`, `failed`, `unavailable`, and `canceled`.

### 2.7 Multi-Interface & IPv6 Networking Subsystem (`internal/netenv`, `internal/server`)
- **Address Family Support**:
  - **IPv4**: Unicast LAN addresses (private RFC 1918 and routable LANs) fully supported.
  - **IPv6 Global Unicast (`2000::/3`) & Unique-Local (`fc00::/7`)**: Fully supported for interface discovery, explicit TCP socket binding (`net.Listen`), session creation, and QR generation (`http://[<ipv6>]:43821/#<token>`).
  - **IPv6 Link-Local (`fe80::/10`) Policy**: Explicitly unsupported for QR onboarding. Standard mobile web browsers (Safari, Chrome) do not reliably parse or route zone IDs (`%iface`) in URL authorities. Flitnix flags link-local candidates as unsuitable for QR pairing with clear guidance.
- **Dual-Stack & Candidate Normalization**: Interfaces with both IPv4 and IPv6 addresses expose both candidates in the network picker. IPv4-mapped IPv6 addresses (`::ffff:x.x.x.x`) are unmapped and deduplicated to prevent phantom interfaces.
- **Explicit Binding Security**: Flitnix binds strictly to the chosen candidate address (never `0.0.0.0` or `[::]`), preventing unintended exposure on secondary network adapters or VPN tunnels.
- **Host & Origin Validation**: Bracketed IPv6 host headers (`[<ipv6>]:<port>`) are parsed and validated strictly against malformed brackets and DNS-rebinding attacks.
- **Physical Network Verification Status**: Automated test suites cover IPv6 discovery, loopback binding, and bracketed HTTP security. Physical mobile phone verification over an actual IPv6-enabled Wi-Fi network requires a dual-stack router and mobile hardware.

### 2.8 Update & Versioning Subsystem (`internal/version`, `internal/update`)
- **Authoritative Semantic Versioning**: `internal/version` defines the single source of truth for application versioning (SemVer 2.0.0). Exposes version metadata, commit hash, build date, and semver comparison utilities (`IsNewer`, `Compare`).
- **Release Feed Discovery**: The Go backend queries GitHub Releases (`/repos/RobinMillford/flitnix/releases/latest`) with an `If-None-Match` ETag caching layer to conserve rate limits. It maps platform and architecture to release assets (`.tar.gz`, `.deb`, `.rpm`, `.exe`, `.dmg`).
- **Two-Stage Cryptographic Integrity Verification**:
  1. **Ed25519 Detached Signature**: Downloads `SHA256SUMS.txt.sig` and verifies the signature of `SHA256SUMS.txt` against the compiled-in official Ed25519 public key.
  2. **SHA-256 Digest Matching**: Parses `SHA256SUMS.txt` to find the expected digest for the target asset and verifies the downloaded binary during streaming using bounded 64 KiB buffers.
- **Platform-Specific Safe Installation Strategies**:
  - **Standalone / Portable Linux (`InstallTypeDirect`)**: Stages the update in a secure temporary directory, sets executable permissions (`0755`), atomically renames over the running binary path (`os.Rename`), and relaunches the application cleanly.
  - **Windows Installer (`InstallTypeInstaller`)**: Downloads the NSIS setup executable (`.exe`), verifies checksum and signature, launches the installer with user privileges, and gracefully exits the running application.
  - **Linux System Packages (`InstallTypePackageManager`)**: If Flitnix is installed system-wide in `/usr/bin/flitnix` via `.deb` or `.rpm`, in-place binary overwrite without root is rejected. Instead, the verified package is saved to the user's Downloads directory and the UI displays the exact package manager installation command (`sudo dpkg -i` or `sudo rpm -Uvh`).
  - **macOS (`InstallTypeManual`)**: Downloads the verified `.dmg` and opens the disk image or download folder for drag-and-drop installation into `/Applications`.

### 2.9 Modernized Design System & Accessibility
- **Slate Token Architecture**: Unified modern palette based on Slate tones (`--bg-app`, `--bg-surface`, `--bg-surface-elevated`, `--text-primary`, `--accent: #3b82f6` dark / `#2563eb` light).
- **Light & Dark Theme Parity**: Full support for system-following, manual dark, and manual light themes with persistent storage and zero contrast regressions.
- **Restrained Motion Design**: Strict transition timings (150ms fast, 200ms base) with complete suppression under `@media (prefers-reduced-motion: reduce)`.
- **Keyboard & Screen Reader Ergonomics**: Full roving `tabindex` keyboard navigation across tabs, visible high-contrast focus rings (`outline: 2px solid var(--accent)`), ARIA live region announcements for transfer approvals, and tabular numerals (`font-variant-numeric: tabular-nums`) to prevent layout jitter during fast byte transfers.

### 2.10 Storage Quota & Disk-Space Protection Subsystem (`internal/transfer`, `internal/guest`, `internal/server`)
- **Cross-Platform Disk-Space Probing**: Queries actual filesystem free space (`unix.Statfs` on Linux/macOS/BSD and `windows.GetDiskFreeSpaceEx` on Windows) after walking up non-existent subdirectories to the nearest existing mount point. When a platform cannot report free space, `unknown` is surfaced clearly in status snapshots rather than failing silently.
- **Configured Buffer Reserve & Hard Cap**: `QuotaManager` compares usable headroom (`FreeBytes - BufferReserveBytes`) against active committed reservations (`Σ max(0, declared - committed)`). When a configured receive cap (`MaxReceiveBytes > 0`) is set, approved in-flight allocations are also bounded against that cap.
- **Admission & Atomic Reservations**:
  - **Pending Declarations (`CheckAvailable`)**: Unapproved batch/upload declarations validate that the incoming size fits within current headroom without locking quota away from sibling requests prior to approval.
  - **Approval / Auto-Approve (`Reserve` / `ReserveBatch` / `Replace`)**: Explicitly approved items, auto-approved batches, and guest room uploads atomically reserve headroom before staging begins. Restored approved transfers from durable recovery logs re-acquire reservations at startup via `RestoreReservation`.
  - **Mid-Stream Safety (`CheckWrite`, `ENOSPC`)**: Upload streaming re-checks remaining disk headroom every `4 MiB` (`diskCheckInterval`) and maps OS `ENOSPC` / `ERROR_DISK_FULL` write failures into typed `ErrInsufficientStorage`, returning HTTP `507 Insufficient Storage` (`code: "insufficient_storage"`) cleanly without panicking or leaving orphan reservations.
- **Scope Boundary**: G26 delivers real-time disk-space protection, reservation accounting, desktop headroom visibility, and manual cleanup. Automated transfer-history retention and auto-pruning policies are deferred to **G6 (Phase 3)**.

### 2.11 Resumable HTTP Byte-Range Download Subsystem (`internal/server`)
- **Strong Validators Across All Shared Single Files**:
  - **Tier 1 (OS-Enforced Immutable)**: Preserves `"sha256-<hex>"` ETags and OS mandatory lock / read-only volume enforcement across continuation cookies (`flitnix-cont-<grantID>`).
  - **Tier 2 (Mutable Single Files & Direct `/api/files/{id}/download`)**: Advertises `Accept-Ranges: bytes` and emits a strong cross-platform file-metadata ETag (`"flit-<size>-<mtime_ns>-<file_id>"` using device+inode on Unix and `GetFileInformationByHandle` volume+fileIndex on Windows).
  - **Tier 3 (Dynamic Streaming ZIP Archives)**: Preserves `Accept-Ranges: none` for on-the-fly multi-file ZIP responses.
- **RFC 9110 Range Semantics & Mutation Guard**: `shouldUseRangeServeContent` + `guardedRangeSeeker` serve `206 Partial Content` on valid `Range` + strong `If-Range`, return `416 Range Not Satisfiable` (`Content-Range: bytes */<size>`) for out-of-bounds ranges, fall back to a fresh `200 OK` full response (with `Content-Digest` trailer) when `If-Range` is weak or mismatched, support `HEAD` without creating spurious download records, and abort mid-stream via `fileMatchesSnapshot` if the source file mutates or is atomically replaced while reading.
- **Mobile Streaming Sink Resumption (`internal/server/assets/app.js`)**: When saving via File System Access (`showSaveFilePicker`), interrupted downloads automatically retry using `Range` + strong `If-Range` backoff against the open writable file while preserving incremental SHA-256 verification and resetting safely if the server responds `200 OK` with a newer representation.

### 2.12 Browseable Shared-Folder Explorer (`internal/transfer`, `internal/server`)
- **Opaque Per-Item Identities & Zero Path Exposure**: `enumerateFolder` assigns each regular file inside a `SharedSource` a fresh opaque random ID (`SourceItem.ID`) alongside sanitized `RelativePath`, size, and timestamp. Neither `SourceBrowsePage`, `SourceBrowseEntry`, nor HTTP responses expose host filesystem paths.
- **Path-Confined Hierarchy Pagination (`GET /api/sources/{id}/items`)**: Accepts `prefix`, `cursor`, and bounded `limit` (default `200`, max `500`), strictly validating prefixes against traversal segments (`..`, `.`, `\`, NUL), symlinks escaping the root via `withinSourceRoot`, and empty/missing directories. Directories sort alphabetically before files with aggregated item counts and byte totals.
- **Selective Single-File & Multi-File Grants (`POST /api/download-grants`)**: Accepts optional `{ sourceId, itemIds }` selections inside a shared folder. Selecting a single file creates a direct single-file grant (`Archive: false`) supporting G2 HTTP byte-range resumption; selecting multiple items streams an uncompressed ZIP containing only the chosen entries.


