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
3. **Path Traversal Protection**: Upload endpoints strictly sanitize incoming filenames to prevent path traversal (`../`) attacks outside the configured download directory.
4. **Publication Reservation**: File publication uses two-phase atomic state management to prevent race conditions from compromising local filesystem state.

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
