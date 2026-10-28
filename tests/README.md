# Flitnix Test Suite Index & Organization Guide

This document indexes all test suites in the Flitnix repository, explains the architectural layout, and provides instructions for running individual, package, and end-to-end regression tests.

---

## 1. Test Architecture & Directory Organization

Flitnix follows standard Go package-local test design principles. Tests are intentionally organized by architectural boundary rather than placed into a single flat directory:

```text
Flitnix/
├── app_*.go                        # Desktop App integration & lifecycle tests (package main)
├── internal/
│   ├── server/                     # HTTP server, routing, mobile API & stream tests
│   │   ├── *_test.go
│   ├── transfer/                   # Streaming upload service, coordinator & store tests
│   │   ├── *_test.go
│   ├── session/                    # Session tokens, pairing lifecycle & expiry tests
│   │   ├── session_test.go
│   ├── netenv/                     # Interface detection & network metadata tests
│   │   ├── metadata_linux_test.go
│   │   └── netenv_test.go
│   └── qr/                         # Pairing QR code generation tests
│       └── qr_test.go
├── tests/                          # Central test documentation & cross-package guides
│   └── README.md                   # This index
└── frontend/test/                  # Frontend unit tests (Node.js test runner)
    └── refreshScheduler.test.ts
```

### Why Package-Coupled Tests Remain with Their Packages

1. **Encapsulation & Private State**: Crucial components (such as `upload` mutexes and publication phase states in `transfer`, HTTP route handlers in `server`, and `App` fields in `main`) rely on unexported structures. Relocating these tests to an external test directory would require exporting sensitive implementation details solely for tests, violating encapsulation.
2. **Go `main` Package Rules**: The root desktop application package is `package main`. Under Go module semantics, `main` cannot be imported by another package (e.g. `tests/`). All tests driving the top-level `App` and its Wails bindings must reside in `package main` in the repository root.
3. **Internal Package Boundaries**: Go's `internal/` package rules restrict imports to the repository tree rooted at the parent. Keeping unit and integration tests adjacent to their packages avoids circular dependencies and guarantees standard `go test ./...` discovery without custom build tags.

---

## 2. Complete Inventory of Test Files

Across the repository, there are **37 Go test files** and **1 frontend TypeScript test file**:

### A. Root Desktop App Tests (`package main`)
| File | Scope & Focus |
|---|---|
| [`app_test.go`](../app_test.go) | Desktop app initialization, session creation, pairing QR, network binding, and session revocation. |
| [`app_batch_test.go`](../app_batch_test.go) | Desktop batch manifest validation, approval, rejection, and the shared HTTP test harness (`batchHarness`). |
| [`app_batch_telemetry_test.go`](../app_batch_telemetry_test.go) | Real-time byte accounting, transfer progress reporting, and completion events. |
| [`app_batch_cancel_publication_test.go`](../app_batch_cancel_publication_test.go) | Cancellation vs publication ordering during whole-batch cancellation, idle expiry, network change, and shutdown. |
| [`app_cancel_publication_test.go`](../app_cancel_publication_test.go) | Standalone upload cancellation vs publication ordering. |
| [`app_folder_root_test.go`](../app_folder_root_test.go) | Folder root directory reservation, collision variant resolution, and cleanup of abandoned empty roots. |
| [`app_handoff_publication_race_test.go`](../app_handoff_publication_race_test.go) | Publication-reservation handoff concurrency between upload service and coordinator under batch cancellation and session teardown. |

### B. HTTP Server & Mobile API Tests (`internal/server/`)
| File | Scope & Focus |
|---|---|
| [`internal/server/api_test.go`](../internal/server/api_test.go) | Core HTTP API endpoints, authentication bearer checks, batch creation, and HTTP test environment (`testEnv`). |
| [`internal/server/api_integration_test.go`](../internal/server/api_integration_test.go) | End-to-end phone upload workflow from declaration to final completion response. |
| [`internal/server/routes_test.go`](../internal/server/routes_test.go) | URL routing, path sanitization, content classification, and error responses. |
| [`internal/server/server_test.go`](../internal/server/server_test.go) | HTTP server lifecycle, graceful shutdown, and address reuse. |
| [`internal/server/upload_regression_test.go`](../internal/server/upload_regression_test.go) | Large file streaming, continuous slow uploads across stall windows (`pacedBody`), and quarter-mark milestone telemetry. |
| [`internal/server/cancel_item_test.go`](../internal/server/cancel_item_test.go) | Phone-side item cancellation (`DELETE /api/batches/{id}/items/{itemId}`). |
| [`internal/server/cancel_publication_test.go`](../internal/server/cancel_publication_test.go) | Server-level HTTP cancellation vs publication contest. |
| [`internal/server/download_stability_test.go`](../internal/server/download_stability_test.go) | Direct and zip-streamed downloads, byte ranges, and connection drops. |
| [`internal/server/download_revocation_test.go`](../internal/server/download_revocation_test.go) | Mid-stream session revocation on direct downloads cutting streams without falsely claiming success. |
| [`internal/server/session_stability_test.go`](../internal/server/session_stability_test.go) | Concurrent HTTP requests under active and revoking sessions. |
| [`internal/server/bidirectional_stability_test.go`](../internal/server/bidirectional_stability_test.go) | Simultaneous upload and download on a single session without interference. |
| [`internal/server/archive_name_test.go`](../internal/server/archive_name_test.go) | Zip archive entry name sanitization, path traversal defense, and colon handling. |

### C. Transfer Engine, Coordinator & Store Tests (`internal/transfer/`)
| File | Scope & Focus |
|---|---|
| [`internal/transfer/service_test.go`](../internal/transfer/service_test.go) | Upload service state transitions, content submission, commit-window cancel races, and terminal outcomes. |
| [`internal/transfer/coordinator_test.go`](../internal/transfer/coordinator_test.go) | Batch and item state machine, FIFO leasing, and queue manipulation. |
| [`internal/transfer/coordinator_stability_test.go`](../internal/transfer/coordinator_stability_test.go) | Multi-goroutine queue concurrency, rapid leasing, and cancellation stress. |
| [`internal/transfer/publication_decision_test.go`](../internal/transfer/publication_decision_test.go) | `ReserveCancel` contest vs `pubPublishing` reservation logic. |
| [`internal/transfer/batch_test.go`](../internal/transfer/batch_test.go) | Coordinator batch creation, item sequencing, and terminal aggregation. |
| [`internal/transfer/batch_link_test.go`](../internal/transfer/batch_link_test.go) | Association between coordinator items and streaming upload records (`SetItemContentID`). |
| [`internal/transfer/batch_cancel_publication_test.go`](../internal/transfer/batch_cancel_publication_test.go) | Coordinator item cancellation against active publishing leases. |
| [`internal/transfer/cancel_item_test.go`](../internal/transfer/cancel_item_test.go) | Coordinator-level item cancellation semantics. |
| [`internal/transfer/store_test.go`](../internal/transfer/store_test.go) | Atomic hard-link commit, part-file staging, collision variants, and directory confinement. |
| [`internal/transfer/stream_test.go`](../internal/transfer/stream_test.go) | Streaming body I/O, 64 KiB buffer sizing, SHA-256 calculation, and exact size enforcement. |
| [`internal/transfer/sanitize_test.go`](../internal/transfer/sanitize_test.go) | Filename and path sanitization rules, traversal rejection, and UTF-8 handling. |
| [`internal/transfer/registry_test.go`](../internal/transfer/registry_test.go) | Shared files and sources registry. |
| [`internal/transfer/source_test.go`](../internal/transfer/source_test.go) | Shared directory source tree enumeration and tracking. |
| [`internal/transfer/download_test.go`](../internal/transfer/download_test.go) | Download ticket registry and token validation. |

### D. Infrastructure & Support Tests
| File | Package | Focus |
|---|---|---|
| [`internal/session/session_test.go`](../internal/session/session_test.go) | `session` | Token generation, crypto-safe entropy, expiry timers, and revocation events. |
| [`internal/netenv/netenv_test.go`](../internal/netenv/netenv_test.go) | `netenv` | Network interface filtering, loopback exclusion, and IP selection. |
| [`internal/netenv/metadata_linux_test.go`](../internal/netenv/metadata_linux_test.go) | `netenv` | Linux-specific interface and Wi-Fi SSID metadata discovery. |
| [`internal/qr/qr_test.go`](../internal/qr/qr_test.go) | `qr` | QR code PNG and data URI rendering. |
| [`frontend/test/refreshScheduler.test.ts`](../frontend/test/refreshScheduler.test.ts) | `frontend` | Desktop UI IPC refresh coalescing, slow-round ordering, and teardown. |

---

## 3. Thematic Test Lookup

When investigating a specific subsystem or regression, consult these files:

### Publication vs. Cancellation Contention
- **Core Record Contest**: [`internal/transfer/service_test.go`](../internal/transfer/service_test.go) (`TestServiceCancelDuringSuccessfulCommitKeepsCompleted`, `TestServiceCancelDuringCommitKeepsCanceled`)
- **Authority Reservation**: [`internal/transfer/publication_decision_test.go`](../internal/transfer/publication_decision_test.go)
- **Handoff Atomicity**: [`app_handoff_publication_race_test.go`](../app_handoff_publication_race_test.go)
- **Batch & Session Boundaries**: [`app_batch_cancel_publication_test.go`](../app_batch_cancel_publication_test.go), [`internal/transfer/batch_cancel_publication_test.go`](../internal/transfer/batch_cancel_publication_test.go)

### Session Lifecycle & Teardown Races
- **Idle Expiry & Network Changes**: [`app_batch_cancel_publication_test.go`](../app_batch_cancel_publication_test.go) (`TestIdleExpiryAndNetworkChangePreservePublicationInvariant`)
- **App Shutdown In-Flight**: [`app_batch_cancel_publication_test.go`](../app_batch_cancel_publication_test.go) (`TestShutdownDuringPublicationPreservesFileWithoutLeaks`)
- **Explicit Stop & Closed History**: [`app_handoff_publication_race_test.go`](../app_handoff_publication_race_test.go) (`TestHandoffSessionTeardownConcurrentWithPublishClaim`)

### Large Streams & Paced Uploads
- **Stall Windows & Progress Pacing**: [`internal/server/upload_regression_test.go`](../internal/server/upload_regression_test.go) (`TestSlowContinuousUploadExceedsStallWindow`)
- **Quarter-Mark Telemetry**: [`internal/server/upload_regression_test.go`](../internal/server/upload_regression_test.go) (`TestLargeStreamQuarterMarkDiagnostics`)

### Folder Roots & Atomic Linking
- **Collision Variants & Atomic Commit**: [`internal/transfer/store_test.go`](../internal/transfer/store_test.go)
- **Abandoned Empty Root Cleanup**: [`app_folder_root_test.go`](../app_folder_root_test.go)

---

## 4. Execution Cheatsheet

### Run the Complete Go Suite
```bash
go test -count=1 ./...
```

### Run with the Race Detector
```bash
go test -race -count=1 ./internal/transfer ./internal/server .
```

### Run by Package
```bash
go test ./...                         # All packages
go test .                             # Desktop App tests
go test ./internal/server             # HTTP server & routing
go test ./internal/transfer           # Transfer engine & coordinator
go test ./internal/session            # Session lifecycle
go test ./internal/netenv             # Network environment
go test ./internal/qr                 # QR generation
```

### Run Focused Tests by Name Pattern
```bash
# Publication & cancellation handoff
go test -v -run TestHandoff .

# Publication vs cancel across App and Transfer engine
go test -v -run "Publication|Cancel" . ./internal/transfer

# Large streaming stall window regression
go test -v -run TestSlowContinuousUploadExceedsStallWindow ./internal/server

# Download mid-stream revocation
go test -v -run TestDirectDownloadRevocationMidStream ./internal/server
```

### Run Frontend Tests
```bash
cd frontend && npm test       # Run Svelte/TS unit tests with Node test runner
cd frontend && npm run check  # Type check with svelte-check
cd frontend && npm run build  # Vite production bundle build
```
