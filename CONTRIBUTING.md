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
   git clone https://github.com/itsyamin/flitnix.git
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

## Licensing Terms

By contributing to Flitnix, you agree that your contributions will be licensed under the [Apache License, Version 2.0](LICENSE) in accordance with Section 5 of the Apache-2.0 license terms.
