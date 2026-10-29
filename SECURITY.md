# Security Policy

## Supported Versions

Only the latest release of Flitnix receives active security patches.

| Version | Supported          |
| :---    | :---               |
| 0.1.x   | :white_check_mark: |
| < 0.1.0 | :x:                |

---

## Security Model Overview

Flitnix is designed for private, local network transfers between paired devices:

1. **Local Binding**: The transfer server binds exclusively to the local network adapter selected for pairing. It does not open ports to the public internet or communicate with relay servers.
2. **Ephemeral QR Pairing Tokens**: Pairing QR codes contain unique, ephemeral session tokens required to access the mobile web interface and API endpoints. Requests without a valid token are rejected.
3. **Guest Drop Box Isolation**:
   - Guest rooms require a room invitation capability secret embedded in the QR fragment.
   - Upon joining, the host must approve the guest. Approved guests receive a random cryptographically secure token stored in an HttpOnly cookie and verified on API calls.
   - Guest rooms are strictly **upload-only**; guests have zero access to download shared desktop files or view transfers from other guests.
   - Destination isolation guarantees that each guest's uploads are placed in a dedicated, sanitized subfolder (`<Destination>/Flitnix Guest Drop/<Room-ID>/<Guest-ID>-<Guest-Name>/`).
   - Host approval is enforced on every individual upload before file bytes are accepted.
   - The host can revoke any individual guest or end the entire room at any time.
4. **Quick Text & Link Bounding**:
   - Message payloads are strictly bounded to 64 KiB.
   - Payloads are validated as valid UTF-8.
   - Text is treated as plain text without HTML interpretation. Safe URL detection strictly permits `http://` and `https://` protocols, preventing `javascript:` or data URI injection.
5. **Path Traversal Protection**: Upload endpoints strictly sanitize incoming filenames to prevent path traversal (`../`) attacks outside the configured download directory.
6. **Publication Reservation**: File publication uses two-phase atomic state management to prevent race conditions from compromising local filesystem state.
7. **End-to-End Cryptographic File Integrity**:
   - Inbound uploads from phones and guests calculate SHA-256 hashes inline and verify or record them on disk.
   - Outbound single-file and folder ZIP downloads compute and verify SHA-256 digests transmitted via standard RFC 9530 `Content-Digest` headers (`sha-256=:<base64>:`).
   - Multi-file ZIP archives are deterministically constructed across passes; if files on disk are mutated while streaming, the connection is instantly severed.
   - Mobile browsers support bounded-memory SHA-256 verification via `File.stream()` without loading full files into device memory.
8. **Local Plaintext HTTP Notice**: Transfers occur over plain HTTP across your trusted local network or hotspot. Connect only devices you control.

---

## Reporting a Vulnerability

If you discover a security vulnerability within Flitnix, please do **NOT** open a public issue.

Instead, please report the vulnerability confidentially:

- **Email**: Send details to RobinMillford at [issan.yamin@gmail.com](mailto:issan.yamin@gmail.com) with the subject `[SECURITY] Flitnix Vulnerability Report`.
- **GitHub Security Advisory**: Alternatively, you can submit a private advisory through GitHub Security Advisories at `https://github.com/RobinMillford/flitnix/security/advisories/new`.

Please include:
- A description of the issue and potential impact.
- Step-by-step instructions or proof-of-concept code to reproduce the issue.
- The operating system and Flitnix version where the issue was observed.

We will acknowledge receipt within 48 hours and work with you on a resolution prior to public disclosure.
