# Contributing to Flitnix

Thank you for your interest in contributing to Flitnix! Flitnix is focused on high-performance, private, offline-first local file transfers with rock-solid concurrency guarantees.

---

## Getting Started

### Prerequisites

- **Go**: 1.24+ (1.27+ recommended)
- **Node.js**: 22+ (LTS) & npm
- **Wails CLI**: `go install github.com/wailsapp/wails/v2/cmd/wails@latest`
- **Linux Requirements**: `gtk3-devel`, `webkit2gtk4.1-devel` (Fedora) or `libgtk-3-dev`, `libwebkit2gtk-4.1-dev` (Debian/Ubuntu)

### Local Environment Setup

1. Fork and clone the repository:
   ```bash
   git clone https://github.com/RobinMillford/flitnix.git
   cd flitnix
   ```
2. Install frontend dependencies:
   ```bash
   cd frontend
   npm ci
   cd ..
   ```
3. Run the application in development mode:
   ```bash
   # On modern Linux (Fedora 39+, Ubuntu 24.04+):
   wails dev -tags webkit2_41

   # On Windows or macOS:
   wails dev
   ```

---

## Development Standards & Architecture

### Concurrency & Data Integrity Invariants

Flitnix enforces strict guarantees around publication and cancellation:
- **Two-phase publication reservation**: An item upload that successfully commits to disk must never be marked canceled or left in a partial state due to late-arriving cancellation requests.
- **Race safety**: Concurrency-sensitive packages (`internal/transfer`, `internal/server`, `internal/session`) must always pass with the Go race detector enabled.
- **Temporary staging cleanup**: In-flight uploads stream to temporary staging files. Temporary files must be securely deleted if canceled or aborted before reservation.

### Code Style

- **Go**: Format all Go code with standard `gofmt` or `goimports`. Preserve existing docstrings and package structure.
- **Frontend**: Follow Svelte 5 and TypeScript conventions. Run `npm run check` before submitting.
- **No Telemetry**: Do not introduce third-party analytics, tracking pixels, or outbound phone-home connections.

---

## Testing Requirements

Before opening a pull request, ensure all test suites pass locally:

```bash
# 1. Run all Go tests
go test -count=1 ./...

# 2. Run concurrency race detector
go test -race ./internal/transfer ./internal/server .

# 3. Verify mobile web assets
node --check internal/server/assets/app.js

# 4. Run frontend tests and type checks
cd frontend
npm test
npm run check
npm run build
cd ..

# 5. Verify installer scripts
sh -n install.sh
./tests/install_test.sh
```

---

## Submitting Pull Requests

1. Create a descriptive feature branch:
   ```bash
   git checkout -b feature/my-new-feature
   ```
2. Make atomic, well-tested commits using conventional commit messages (e.g., `feat:`, `fix:`, `docs:`, `test:`, `refactor:`).
3. Do not push unformatted files or break existing tests.
4. Open a pull request against the `main` branch using the provided PR template.

---

## Licensing Terms & Contributor Rights

Flitnix is free software distributed under the GNU Affero General Public License v3.0 only (`AGPL-3.0-only`).

By submitting a pull request, issue, or other contribution to Flitnix:

- **Inbound Licensing**: Contributions are accepted under the GNU Affero General Public License, Version 3 (`AGPL-3.0-only`), unless explicitly agreed otherwise in writing.
- **Copyright Retention**: You retain copyright ownership of your original contributions. Submitting a pull request or patch does not transfer or assign copyright ownership to Flitnix or the repository maintainer. No mandatory Contributor License Agreement (CLA) or copyright assignment is required.
- **Right to Contribute**: You represent and warrant that you have the full legal right to submit the contribution, and that doing so does not violate any third-party agreements, patents, or intellectual property rights.
- **Third-Party Code**: If your contribution includes or is based on third-party code, assets, or libraries, you must clearly identify the original copyright holder and verify that the third-party license is compatible with GNU AGPLv3 (e.g., MIT, BSD-2-Clause, BSD-3-Clause, Apache-2.0). All existing copyright and license notices must be preserved.
- **Upstream Collaboration**: Upstream pull requests, bug fixes, and improvements are strongly encouraged to benefit the wider user community. Independent developers and downstream forks are not legally obligated to submit all private improvements upstream, but contributing back helps maintain an open, robust ecosystem.
- **Issue & Security Reporting**: Bug reports, feature suggestions, and general issues should be submitted through the official [GitHub Issues](https://github.com/RobinMillford/flitnix/issues) tracker. For security vulnerabilities, please refer to our [Security Policy](SECURITY.md) for private reporting procedures.
