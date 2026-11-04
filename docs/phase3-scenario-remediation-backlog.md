# Flitnix Phase 3 — Scenario Remediation Backlog

This backlog is derived from [`phase3-scenario-coverage.md`](./phase3-scenario-coverage.md).
It is a test and verification backlog only; no item below authorizes production
changes by itself.

## 1. Data loss and crash recovery

| Scenarios | Missing work | Implementation assessment | Proposed regression test | Dependencies | Risk |
|---|---|---|---|---|---|
| 11, 14, 36 | Exercise remaining restart boundaries in the actual desktop/server process, including final publication rename, desktop restart, and listener admission ordering. | The existing crash harness proves manager recovery; actual desktop restart and finalization/listener fault boundaries remain incomplete. | Inject Sync/rename/startup faults and run the real desktop/server acceptance flow. | Fault-injection hooks and isolated process fixture. | High — incorrect ordering can lose data or over-admit quota. |
| 1–3 | Replace loopback interruption with controlled LAN/Wi-Fi interruption evidence. | Protocol behavior is implemented and loopback-tested. | Run a repeatable network impairment on a disposable LAN device and compare committed offsets and retransmission. | Dedicated test network and device. | High — radio behavior differs from TCP truncation. |

## 2. Integrity and publication safety

| Scenarios | Missing work | Implementation assessment | Proposed regression test | Dependencies | Risk |
|---|---|---|---|---|---|
| 11, 18 | Add boundary-specific filesystem faults and verify publication recovery and autonomous expiry cleanup. | Scenarios 40, 47, and 48 have passing software tests; hard finalization crash and autonomous expiry/coordinator-release timing remain incomplete. Zero `ExpiresAt` is exempt from PATCH-time expiry enforcement and `CleanupExpired()` in the implementation. | Inject rename and kill-at-rename faults; advance a fake clock for cleanup; assert durable metadata, quarantine, coordinator release, and response codes for genuinely expired uploads. | Deterministic filesystem fault adapter and injectable clock. | High — false success can publish corrupt data. |

## 3. Session isolation and revocation

| Scenarios | Missing work | Implementation assessment | Proposed regression test | Dependencies | Risk |
|---|---|---|---|---|---|
| 19 | Test desktop revocation while PATCH is active and after a continuation ticket is issued. | No adequate executed evidence identified. | Revoke a session during a blocked PATCH; assert 401, ticket purge, lock release, and isolation from another session. | Revocation API fixture and blocked request. | High — stale credentials may retain write access. |
| 20, 49 | Add guest and desktop end-to-end cancellation/reconnect acceptance in real browser clients. | Guest/Node and loopback software paths are covered and passing; browser lifecycle is not. | Exercise guest room interruption and AbortController cancellation in Chrome. | Browser automation and guest fixture. | Medium. |

## 4. Upload lifecycle and cancellation

| Scenarios | Missing work | Implementation assessment | Proposed regression test | Dependencies | Risk |
|---|---|---|---|---|---|
| 18 | Prove autonomous expiry cleanup and coordinator release rather than relying on test-side lifecycle setup. | Deterministic expiry and zero-expiry cleanup exemption are covered; autonomous cleanup timing and coordinator release still need independent proof. | Advance a fake clock, run cleanup, and assert coordinator release and file deletion for an expired upload without an explicit DELETE. | Injectable clock and cleanup worker. | Medium. |

## 5. Guest transfer recovery

| Scenarios | Missing work | Implementation assessment | Proposed regression test | Dependencies | Risk |
|---|---|---|---|---|---|
| 20 | Validate Guest Drop Box recovery through the browser and reconnect path. | Guest server path is passing in loopback tests; browser reconnect remains untested. | Interrupt and resume a guest upload in Chrome against a disposable room. | Browser and guest room fixture. | Medium. |

## 6. Browser UI and status recovery

| Scenarios | Missing work | Implementation assessment | Proposed regression test | Dependencies | Risk |
|---|---|---|---|---|---|
| 12, 15, 35, 39, 42, 45, 49 | Run the existing VM assertions in real Chromium and Safari-compatible environments, including reload, close, prompt, status polling, and fallback memory behavior. | Client state machine is covered by Node VM tests; Scenario 35 now has an independent deferred-completion test, but the client exposes no explicit `verifying` state and browser behavior is not tested. | Define the UI/status `verifying` contract, then run a browser matrix with network interception, IndexedDB persistence, tab close/reopen, and UI completion assertions. | UI contract, Playwright/WebDriver, Chromium and Safari device. | Medium to high. |

## 7. Resource quota and checkpoint efficiency

| Scenarios | Missing work | Implementation assessment | Proposed regression test | Dependencies | Risk |
|---|---|---|---|---|---|
| 17, 36 | Measure reservation restoration-before-listener under production startup. | Scenario 48 has a passing exact-EOF checkpoint test; production listener ordering remains incomplete for Scenario 36. | Start server with staged reservations and block listener readiness until replay completes. | Startup readiness signal and filesystem instrumentation. | Medium. |

## Environment acceptance backlog

1. Real LAN/Wi-Fi interruption and reconnect.
2. Android Chrome upload, reload, and guest recovery.
3. iOS Safari upload, reload, and guest recovery.
4. Desktop process restart and revocation acceptance.
