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
- **Safe Zero-Byte Retry**: Failed transfers cleanly reset transferred bytes to 0, tear down partial staging resources, and restart streaming from the beginning, preventing byte alignment corruption.

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
