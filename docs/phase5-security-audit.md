# Flitnix Phase 5 — Production Security Review & Hardening Report

**Document Version:** 1.0.0  
**Date:** 2026-11-04  
**Branch:** `feature/phase5-production-readiness`  
**Authoritative Scenario Matrix SHA-256:** `c5c13b78d02e5c479826ac08a8f2e09a31e91a7cf232cd8d49336175904e6a51` (strictly intact)

---

## 1. Executive Summary & Review Scope

This document represents an **internal engineering security review** conducted by the Flitnix development team. It is **not** an independent third-party penetration test, and it does not constitute a formal external security certification or compliance audit. Its purpose is to systematically evaluate the architecture, inspect threat model boundaries, document hardening implementations, and verify automated security regression tests across all supported operating systems.

Flitnix is designed for high-throughput, wireless peer-to-peer file transfers between desktop systems (Linux, Windows, macOS) and mobile devices (Android, iOS) via standard web browsers over local area networks (LAN). Because LAN environments often contain untrusted devices and mobile browsers operate without native agent software, the application's security model must enforce defense-in-depth across the network, session, filesystem, and presentation layers without degrading transfer performance.

This internal review evaluates the security architecture of Flitnix across five core domains:
1. **Device Pairing, Authentication & Session Integrity**
2. **HTTP Protocol, Origin Validation & Browser Sandbox Security**
3. **Filesystem Defense, Path Traversal & Cross-Platform Sanitization**
4. **Denial of Service (DoS) Mitigation & Resource Protection**
5. **Phase 5 Hardening Implementations and Verification**

---

## 2. Threat Model & Security Boundaries

### 2.1 Threat Actors & Capabilities
* **Untrusted LAN Observers:** Eavesdroppers or malicious nodes on the same Wi-Fi subnet attempting to intercept transfers, spoof Host headers, hijack unauthenticated endpoints, or replay tokens.
* **Malicious Browser Clients:** Rogue scripts, compromised browser extensions, or malicious peer web clients attempting CSRF, directory traversal, uploading executable payloads, or exhausting host storage.
* **Spoofed Device Identities:** Untrusted clients supplying forged device identifiers, ANSI terminal escape injection payloads, or unverified device names to trick desktop operators.

### 2.2 Security Invariants
1. **Zero Unauthenticated State Changes:** No file content, batch, upload declaration, or download grant may be created, read, or modified without a cryptographically valid session token or signed device pairing.
2. **Single-Pass Integrity Verification:** No received file may be published into user storage without verifying its full-stream SHA-256 digest against the declared digest.
3. **Strict Filesystem Isolation:** Received files must never escape designated receive directories, traverse directories via `..`, or trigger operating-system reserved namespace conflicts.
4. **Non-Destructive Collisions:** Existing files on disk must never be overwritten, truncated, or clobbered by incoming transfers.
5. **Session Isolation:** A session token may only query, stream, and manage its own batches, uploads, and download grants. Cross-session access is strictly refused (401/404).

---

## 3. Pairing, Authentication & Session Architecture

### 3.1 Pairing Protocol
* **Cryptographic Foundation:** Flitnix implements an asymmetric challenge-response pairing protocol using Ed25519 signing keys and X25519 ECDH ephemeral key exchange with cryptographic salt commitments (`ComputeCommitment(clientEphemeral, salt)`).
* **Opening and Verification:**
  - Client sends initial challenge with public key and commitment hash.
  - Server generates a cryptographically secure random pairing nonce (`crypto/rand`).
  - Client reveals ephemeral key and salt; server verifies commitment before evaluating the signature.
  - Desktop operator is prompted with an approval dialog showing device information.
* **Brute-Force & Lockout Protection:** The pairing coordinator maintains an in-memory failure counter (`failureTimestamps []time.Time`) by default. Consecutive failed signature verifications increment `FailureCount()`, preventing offline or online brute-force exploitation of pairing nonces (verified in `TestPhase5_Scenario21_InvalidOrExpiredPairing`). For desktop deployments requiring persistence across daemon or application restarts, the coordinator provides `EnableDurableLockout(dir string)` which persists and reloads HMAC-authenticated `PairingSecurityRecord` state (`pairing_security.json`) using key derivation from host pairing secrets.

### 3.2 Session Ownership & Token Management
* **Bearer Token Generation:** Tokens are 256-bit cryptographic random hex strings generated via `crypto/rand`.
* **Session Key Derivation:** To prevent timing and dictionary leaks of the raw bearer token in internal registries, the coordinator and transfer managers index sessions by `session.Key()`, which is a SHA-256 digest of the raw bearer token (`sessionKey := sha256.Sum256([]byte(token))`).
* **Session Lifecycle & Expiration:** Inactive sessions expire after a configurable idle duration. Expired sessions are pruned, revoking all outstanding in-flight operations.
* **Guest Access Scoping:** Guest uploads for one-off QR code scanning are cryptographically isolated under a separate `guest-<id>` namespace and cannot access personal files or other guest uploads.

### 3.3 Download Grants and Cookie Security
* **Two-Phase Download Security:** Downloads require redeeming a short-lived download grant issued via `POST /api/download-grants`.
* **Cookie Flags:** Grants issue continuation cookies configured per RFC 6265:
  - `HttpOnly: true` (prevents JavaScript access in the browser)
  - `SameSite: SameSiteStrictMode` (prevents cross-site request forgery)
  - `Path: /api/downloads/<grantID>` (strictly scoped to the single download resource)
  - Bound to the issuing session's key; cross-session grant redemption yields 404/403.

---

## 4. HTTP Protocol & Browser Security

### 4.1 Host Header & Request Validation
* The server validates Host and Origin headers on incoming HTTP requests. Arbitrary hostnames or spoofed proxy headers are rejected at the routing boundary (`newRouter`).

### 4.2 Cross-Origin Resource Sharing (CORS) & CSRF Defense
* **API Endpoints:** Authenticated API routes (`/api/...`) enforce strict Authorization header checks.
* **State-Changing Operations:** All state changes (batch creation, chunk uploads, approvals, cancellations) require `POST`, `PUT`, `PATCH`, or `DELETE` with explicit `Authorization: Bearer <token>` or valid session cookies.
* **Tus Streaming:** Tus requests require `Tus-Resumable: 1.0.0` and `Content-Type: application/offset+octet-stream`. Browser cross-site form submissions cannot emit these custom headers, providing automatic CSRF protection.

### 4.3 Content Security Policy (CSP)
* Static phone assets served at `/` and `/static/...` are served with strict CSP headers restricting scripts to self-hosted sources and disabling inline script evaluation.

---

## 5. Filesystem Security & Path Traversal Prevention

### 5.1 Directory Traversal Mitigation
* **Sanitization:** All client-declared filenames and relative paths are filtered through `filepath.Clean` and `filepath.Base` before directory resolution.
* **Rejection of Relative Escapes:** Declarations containing `../`, leading slashes, backslashes, or relative traversal paths are rejected with `400 Bad Request` at upload declaration time (verified in `TestPhase5_Scenario23_MalformedUploadMetadata`).

### 5.2 Windows Reserved Filenames Hardening
Windows operating systems treat certain filenames as hardware device files (`CON`, `PRN`, `AUX`, `NUL`, `COM1` through `COM9`, `LPT1` through `LPT9`), regardless of file extension (e.g., `aux.txt`, `con.bin`), and strip trailing dots and spaces from file paths. Opening or creating such paths on Windows filesystems or across Samba shares can cause host hangs, crashes, or denial of service.

**Phase 5 Hardening Actions Implemented:**
1. **`internal/transfer/store.go` (`Store.OpenPart`):**
   - Added regex-based detection for Windows reserved names:
     ```go
     var windowsReservedRegex = regexp.MustCompile(`^(?i:(CON|PRN|AUX|NUL|COM[1-9]|LPT[1-9]))(\..*)?$`)
     ```
   - Rejects any upload attempting to create a part file matching these reserved names.
   - Rejects filenames with trailing dots (`.`) or trailing spaces (` `).
2. **`internal/transfer/service.go` (`Service.declare`):**
   - Added identical validation during the initial upload declaration phase, rejecting invalid names with `ErrInvalidFilename` before any part file or state is allocated.
3. **Verification:**
   - Unit test `TestStoreOpenPartRejectsWindowsReservedAndTrailingDot` in `internal/transfer/store_test.go` verifies rejection of `CON`, `aux.txt`, `NUL.dat`, `COM1`, `trailing.`, and `trailingspace `.
   - Integration test `TestPhase5_Scenario23_MalformedUploadMetadata` verifies rejection via HTTP API.

### 5.3 Device Name ANSI Injection Sanitization
Client devices connecting via QR code or pairing challenge declare a friendly device name (e.g., `"Alice's Pixel"`). Untrusted clients could supply ANSI terminal escape sequences (e.g., `\x1b[2J`, `\x1b[31m`), control characters (null bytes, carriage returns), or excessively long strings to tamper with desktop terminal logs, desktop UI notifications, or pairing confirmation dialogs.

**Phase 5 Hardening Actions Implemented:**
1. **`internal/devices/pairing.go` (`SanitizeDeviceName`):**
   - Strips ANSI escape sequences via `ansiEscapeRegex = regexp.MustCompile(`\x1b\[[0-9;]*[a-zA-Z]`)`.
   - Filters out non-printable ASCII and Unicode control characters (`unicode.IsControl(r)`), preserving valid Unicode letters, marks, digits, punctuation, and symbols across all international languages.
   - Clamps device name length to a maximum of 64 runes.
   - Falls back to `"Unnamed Device"` if the sanitized string is empty or whitespace-only.
2. **Integration:** Integrated directly into `PairingCoordinator.PrepareApproval`.
3. **Verification:** Unit test `TestSanitizeDeviceName` in `internal/devices/pairing_test.go` tests ANSI sequences, control characters, empty fallbacks, whitespace trimming, and 64-rune truncation.

### 5.4 Collision Avoidance & Atomic Publication
* **Non-Destructive Storage:** If a file with the target name already exists in the destination folder, variant generators allocate a non-conflicting name (e.g., `file (1).ext`, `file (2).ext`). The original pre-existing file is never overwritten, truncated, or clobbered (verified in `TestPhase5_Scenario10_DuplicateFilenameHandling` and `TestPhase5_Scenario11_DestinationAlreadyExists`).
* **Publication Engines:**
  - **Single-Part Store (`Store.PartFile.Commit()`):** Uses atomic hard-linking (`os.Link`) with collision variant iteration. Once the hard link is established at the destination path, the temporary `.part` file is unlinked. If hard links are unsupported across filesystems, it returns an explicit cross-filesystem error.
  - **Chunked Resumable Engine (`ResumableManager.FinalizeComplete()`):** Uses atomic rename (`os.Rename` via `renameFileWithRetry`) following SHA-256 stream verification, resolving collision variants dynamically. To guarantee crash recovery and prevent duplicate publication on process restart, a pre-allocated `.published-<id>.json` record is written to disk before final cleanup of the `.part` file.
* **Permissions:** Newly created part files and published files receive `0600` permissions (read/write by owner only), and directories receive `0700`.

---

## 6. Denial of Service (DoS) & Resource Protection

### 6.1 Request Size Boundaries
* **Metadata Declarations:** Request bodies for `/api/uploads` and `/api/batches` are wrapped in `http.MaxBytesReader` with `maxMetadataBytes` (64 KiB), rejecting oversized JSON payloads before parsing.
* **Control Fields:** JSON decoders use `dec.DisallowUnknownFields()`, preventing memory bloat via unexpected payload injection.

### 6.2 Storage Quota Management
* **Pre-Allocation & Reservation:** The `QuotaManager` calculates available filesystem storage and safety margins before accepting upload declarations (`Reserve(session, uploadID, size)`).
* **Storage Rejection:** Requests exceeding available storage or reserved safety thresholds are rejected with `507 Insufficient Storage` (`ErrInsufficientStorage`).
* **Concurrent Upload Cap:** Per-session active uploads are bounded to `maxConcurrentResumables` (default: 10 active resumables), preventing memory exhaustion attacks via parallel partial transfers (verified in `TestPhase5_Scenario12_InsufficientDiskSpace`).

### 6.3 Concurrency Control & Stream Locking
* **Single Active Stream Lock:** Tus uploads acquire an active stream lock (`atomic.CompareAndSwapInt32(&u.streaming, 0, 1)`). Concurrent overlapping PATCH streams for the same upload are rejected with `423 Locked`.
* **Prompt Disconnect Settlement:** When a client abruptly disconnects, the stream handler drains or aborts the context, releasing the lock within milliseconds so subsequent resumed connections are not blocked (verified in `TestPhase5_Scenario14_ClientDisconnection`).

### 6.4 Metadata Reconciliation & Quarantine
* **Startup Integrity:** On application launch, `ResumableManager.RecoverOnStartup()` scans the resumable directory, verifies metadata JSON files, and checks part file lengths against recorded committed offsets.
* **Corrupted Record Quarantine:** Incomplete, unreadable, or tampered metadata files that cannot be validated are quarantined to a separate `.quarantine` subfolder, ensuring corrupted transfers never crash the server or corrupt the transfer database (verified in `TestPhase5_Scenario24_CorruptedTransferMetadata`).

---

## 7. Security Audit Findings & Resolution Matrix

| Vulnerability / Risk | Severity | Status | Mitigation / Remediation |
|---|---|---|---|
| Windows reserved device names in part files (`CON`, `PRN`, `AUX`, etc.) | High | **RESOLVED** | Added `windowsReservedRegex` in `internal/transfer/store.go` and `service.go` to reject reserved names and trailing dots/spaces with `400 Bad Request`. |
| ANSI terminal escape / control character injection in client device names | Medium | **RESOLVED** | Implemented `SanitizeDeviceName` in `internal/devices/pairing.go` stripping ANSI codes, control characters, bounding to 64 runes, with safe fallback. |
| Path traversal via declared upload filenames (`../../etc/passwd`) | High | **PREVENTED** | Strict path cleaning and rejection via `filepath.Clean` and HTTP validation returning `400 Bad Request`. |
| Overwriting pre-existing user files on destination | High | **PREVENTED** | Non-destructive variant resolution ensuring existing destination files are never overwritten or clobbered. |
| Incomplete upload premature publication | Critical | **PREVENTED** | Single-pass atomic publication (`os.Link` / `os.Rename`) only after 100% bytes committed and SHA-256 verified against declared digest. |
| Cross-session transfer hijacking | High | **PREVENTED** | All batch, item, and grant endpoints validate caller session key; cross-session requests rejected (401/404). |
| Resource exhaustion via concurrent uploads / disk flooding | Medium | **PREVENTED** | Quota manager enforces active concurrent upload caps and storage safety margins (507 Insufficient Storage). |
| Client disconnect hanging stream locks | Medium | **PREVENTED** | Context cancellation promptly releases upload lock, verified in <500ms settlement tests. |

---

## 8. Conclusion & Scope Limitations

Flitnix Phase 5 demonstrates a resilient internal security architecture across its evaluated threat model. Cryptographic session isolation, defense-in-depth against filesystem exploits (including Windows-specific device namespaces and path normalization), ANSI sanitization of device identities, strict SHA-256 digest verification, and bounded resource quotas provide robust protections on untrusted local networks.

As an internal engineering review, this report is not a substitute for an external professional penetration test or third-party security audit. Prior to widespread enterprise or public release, independent penetration testing and formal code signing (Microsoft Authenticode and Apple Developer ID notarization) should be conducted.
