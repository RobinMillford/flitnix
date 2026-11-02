# Flitnix Phase 3 — 49-Scenario Coverage Audit

This audit uses the authoritative requirements in
[`phase3-scenario-matrix.md`](./phase3-scenario-matrix.md), Section 16 of
*Flitnix — Phase 3: Resumable Transfer Protocol*, Revision 2.8. The matrix is
the source for the exact requirement, injection mechanism, and expected
recovery wording; this document records the implementation and executed-test
evidence.

## Evidence classification

- **PASS**: the critical recovery behavior is asserted by an executed test.
- **PARTIAL**: useful implementation or test evidence exists, but an important
  boundary (usually a real application restart, browser, or device) is absent.
- **NOT TESTED**: no adequate executed evidence exists.

Loopback HTTP, Node browser stubs, and cross-compilation are not evidence of
physical Wi-Fi, Chrome/Safari, Android, or iOS behavior.

## Summary

| Status | Count |
| --- | ---: |
| PASS | 36 |
| PARTIAL | 12 |
| NOT TESTED | 1 |
| **Total** | **49** |

### Execution evidence

| Environment | Result |
| --- | --- |
| Go unit/integration tests | Focused reliability and recovery selection passed |
| Frontend Node harness | `node --test frontend/test/resumableUpload.test.ts`: 15/15 passed; full `npm --prefix frontend test`: 74/74 passed |
| Frontend check | `npm --prefix frontend run check`: 0 errors, 0 warnings |
| Loopback HTTP | Covered by `httptest` and the WP4 harness |
| Process crash/restart | Scenarios 8, 14/43, and 36 use the subprocess harness; Scenario 43 has a real HTTP child-process boundary test covering abrupt termination, startup restoration, HTTP HEAD, and completion |
| Real LAN/Wi-Fi | Not executed |
| Physical Android | Not executed |
| Physical iOS | Not executed |

### Quality-gate result table

These are the previously recorded full quality-gate results; no full gate was
rerun for this documentation and frontend-test-only correction.

| Command | Exit code |
|---|---:|
| `npm --prefix frontend test` | 0 |
| `npm --prefix frontend run check` | 0 |
| `go test -count=1 ./...` | 0 |
| `go test -race -count=1 ./...` | 0 |
| `go vet ./...` | 0 |
| `go build ./...` | 0 |
| `GOOS=windows GOARCH=amd64 go build ./...` | 0 |
| `GOOS=darwin GOARCH=amd64 go build ./...` | 0 |
| `GOOS=darwin GOARCH=arm64 go build ./...` | 0 |
| `wails build -tags webkit2_41` | 0 |

## Scenario mapping

The Requirement, Injection mechanism, and Expected recovery columns below are
the exact Section 16 rows, reproduced from the canonical matrix. “Production
implementation” and “Assertions” identify what the current tests actually
prove; a related test is not treated as proof when it does not exercise the
specified fault.

| ID | Requirement | Injection mechanism | Expected recovery | Production implementation | Test file / test name | Assertions and environment | Status | Missing work |
|---:|---|---|---|---|---|---|---|---|
| 1 | Wi-Fi dropped at 10% | Abort TCP connection at 10% bytes. | Retain committed `.part`; item Interrupted; batch remains active. | Tus PATCH and resumable manager checkpointing. | `internal/server/tus_upload_test.go` — `TestTusScenarios_1_2_3_InterruptionAndResume`; `benchmarks/wp4/suite.go` — `RunConfig6_InterruptionRecovery` | Offset preservation and resume over loopback HTTP; no physical radio. | PARTIAL | Execute with a real network drop. |
| 2 | Wi-Fi dropped at 50% | Toggle airplane mode mid-stream. | Exact checkpoint; client backoff retry. | Tus checkpoint and client retry state. | Same grouped test and Config-6 harness. | 50% interruption/resume assertions over loopback. | PARTIAL | Real airplane-mode/network evidence. |
| 3 | Wi-Fi dropped at 99% | Cut socket immediately before completion. | Preserve 99%; resume final 1%; complete. | Tus final checkpoint and completion endpoint. | Same grouped test and Config-6 harness. | 99% resume and completion over loopback. | PARTIAL | Real network-drop evidence. |
| 4 | Lost POST response | Drop TCP after `.part` creation. | Retry POST returns existing resource with 201. | Idempotent Tus creation. | `internal/server/tus_upload_test.go` — `TestTusCreate_PersonalAndIdempotency` | Repeated creation returns the same resource and headers. | PASS | — |
| 5 | Lost PATCH response | Drop TCP after final chunk sync. | HEAD finds size; client submits digest. | HEAD, PATCH, and completion state machine. | `internal/server/tus_upload_test.go` — `TestTusScenario_5_LostPatchResponseAfterServerSync` | The transport discards the completed PATCH response; HEAD reports the committed offset/state and completion succeeds without retransmitting bytes. Loopback. | PASS | — |
| 6 | Duplicate PATCH request | Send overlapping PATCH streams. | Second stream receives 423 Locked. | Per-upload lifecycle lock. | `internal/server/tus_upload_test.go` — `TestTusScenario_6_DuplicatePatchLocked` | Overlap is injected and 423 is asserted. Go HTTP integration. | PASS | — |
| 7 | Wrong Upload-Offset | Send offset 20 MB while server is at 10 MB. | 409 Conflict and authoritative Upload-Offset. | Offset validation in Tus PATCH handler. | `internal/server/tus_upload_test.go` — `TestTusScenario_7_OffendingOffsetAfterTruncation` | A valid 10 MiB prefix is committed; an offending 20 MiB offset returns 409 with `Upload-Offset: 10485760`, does not advance state, and the correct resume completes with the original digest. Loopback. | PASS | — |
| 8 | Uncheckpointed trailing bytes | Crash during write before checkpoint sync. | Restart truncates `.part` to committed offset. | Durable metadata and `RecoverOnStartup`. | `internal/transfer/crash_subprocess_test.go` — `TestWp4Crash_Scenario8_UncheckpointedTailTruncated` | Real subprocess exits mid-ingest; fresh manager truncates and resumes. | PASS | — |
| 9 | Missing partial file | Delete `.part` while `.meta` remains. | Detect inconsistency; 410; restart from zero. | Missing-staging validation and Tus error mapping. | `internal/server/tus_upload_test.go` — `TestTusScenario_9_PartFileLossRestart` | PATCH after `.part` loss returns 410; startup recovery retains metadata; DELETE cancels the original coordinator item; a newly approved item starts at offset zero and completes. Go loopback. | PASS | — |
| 10 | Corrupt metadata | Write invalid JSON to `.meta`. | Quarantine metadata; return 404; no unsafe offset. | Metadata parsing and quarantine. | `internal/transfer/resumable_test.go` — corrupt metadata recovery test | Corrupt file is isolated and upload is not restored. Go unit/recovery. | PASS | — |
| 11 | Crash during finalization | Kill -9 during rename to final. | Publication record makes recovery idempotent. | Publication record and finalization recovery. | `internal/transfer/resumable_test.go` — finalization recovery coverage | Failure paths are simulated in-process; no kill at the rename boundary. | PARTIAL | Add a subprocess crash exactly at publication rename. |
| 12 | Modified file on reselection | Change byte at 50 MB and reselect. | Prefix hash mismatch prompts restart at byte zero. | Worker prefix verification and client resume guard. | `frontend/test/resumableUpload.test.ts` — `Scenario 15 (Edge Case): Modified reselected file...` | Modified prefix is rejected and resume upload is prevented. Node VM harness. | PASS | Browser prompt/UI acceptance remains untested. |
| 13 | Identical header, altered body | Same name, size, and mtime; altered tail. | Fast screen passes; prefix SHA-256 catches mismatch. | Tier-2 worker hashing. | `frontend/test/resumableUpload.test.ts` — `Scenario 13 & 45: REBUILD_HASHER...` | Exact prefix and altered content are distinguished. Node VM harness. | PASS | — |
| 14 | Desktop application restart | SIGTERM desktop app mid-upload. | Restore metadata/hash or rehash committed bytes. | Durable manager startup recovery. | `internal/transfer/crash_subprocess_test.go` — `TestWp4Crash_Scenarios14_43_RestartInAwaitingDigestCompletesWithoutReupload` | Real process restart restores metadata and completes without bytes re-uploaded. | PARTIAL | Exercise the actual desktop process and worker. |
| 15 | Browser reload/kill | Refresh tab or kill browser process. | IndexedDB and prefix checks resume without retransmission. | IndexedDB client restore and worker rebuild. | `frontend/test/resumableUpload.test.ts` — `Scenario 15` plus edge cases | Metadata restore, authoritative HEAD, prefix verification, and resume asserted under Node stubs. | PARTIAL | Real Chrome/Safari lifecycle test. |
| 16 | Disk full (ENOSPC) | Fill target drive during upload. | Preserve last block; paused_storage_full; 507. | Ingest error handling and storage error mapping. | `internal/server/tus_upload_test.go` — `TestTusScenario_16_MidUploadENOSPC` | The staged file is swapped to `/dev/full`; the active upload receives 507, retains the prior offset, reports `paused_storage_full`, resumes after restoration, and completes with the original digest. Go loopback. | PASS | — |
| 17 | Concurrent upload quota | Attempt 15 parallel uploads. | Enforce ten active resumables; reject excess. | `QuotaManager` and Tus creation admission. | `internal/server/tus_upload_test.go` — `TestTusScenario_17_ConcurrencyCapDefaultQuota` | Exactly ten uploads are accepted, the 11th is rejected with 429, and DELETE releases capacity for another upload. Go integration. | PASS | — |
| 18 | Expired upload session | PATCH after 24-hour expiry. | 404/410 and orphan cleanup. | Expiry enforcement and cleanup. | `internal/server/tus_upload_test.go` — `TestTusScenario_18_ExpiryDeterministic`; `internal/transfer/resumable.go` cleanup | Upload A returns 410 after deterministic expiry and is purged; live B survives cleanup and completes; zero `ExpiresAt` disables PATCH-time enforcement and is also skipped by `CleanupExpired()`, then C PATCHes and completes; the guest manager independently enforces expiry. Loopback. | PARTIAL | Verify autonomous cleanup timing and coordinator release without relying on test-side lifecycle setup. |
| 19 | Session revoked by desktop | Click Revoke Device during upload. | Abort upload; purge tickets; return 401. | Session/revocation integration surface. | No adequate executed test identified. | No independent revocation assertion. | NOT TESTED | Add revocation integration test with in-flight PATCH. |
| 20 | Guest Drop Box interruption | Interrupt Guest Drop Box upload. | Guest record remains resumable after reconnect. | Guest Tus handlers and guest coordinator state. | `internal/server/tus_upload_test.go` — `TestTusScenario_20_GuestInterruptionResumeAndIsolation` | Guest-specific interruption, resume, digest completion, and isolation are asserted on the Guest Drop Box routes over loopback. | PASS | Real guest browser/network acceptance remains untested. |
| 21 | If-Range match | Valid matching If-Range with Range. | 206 requested slice. | Tier-1 download handler. | `internal/server/download_range_test.go` — `TestDownloadRange_Scenario21_30_MatchingIfRangeReturns206` | 206, range, and body are asserted. Go HTTP integration. | PASS | — |
| 22 | If-Range mismatch | Source changes; old Range validator. | 200 full new body. | Strong validator handling. | `internal/server/download_range_test.go` — `TestDownloadRange_Scenario22_31_MismatchingIfRangeReturns200Full` | Full response and changed body asserted. | PASS | — |
| 23 | 416 out of bounds | Request range beyond file. | 416 and `Content-Range: bytes */total`. | Range parser and response mapping. | `internal/server/download_range_test.go` — `TestDownloadRange_Scenario23_OutOfBoundsRangeReturns416` | Status and header asserted. | PASS | — |
| 24 | Dynamic ZIP Range | Range request on generated multi-file ZIP. | Ignore Range; 200 full stream; no ranges. | Dynamic archive response path. | `internal/server/download_range_test.go` — `TestDownloadRange_Scenario24_DynamicZIPIgnoresRange` | 200, full body, and Accept-Ranges behavior asserted. | PASS | — |
| 25 | Weak ETag rejection | Weak If-Range against Tier 1. | 200 full response. | `http.ServeContent` validator path. | `internal/server/download_range_test.go` — weak ETag range test | Weak validator does not produce partial content. | PASS | — |
| 26 | Expired download ticket | Range resume after idle expiry. | 401/410; obtain fresh grant. | Download ticket validation. | `internal/server/download_range_test.go` — expired-ticket test | Expired authorization is rejected. | PASS | — |
| 27 | Cookie isolation | Four simultaneous interrupted downloads. | Unique continuation cookie per download. | Continuation cookie path scoping. | `internal/server/download_range_test.go` — cookie isolation test | Independent cookie values and paths are asserted. | PASS | — |
| 28 | Tier-2 Range without If-Range | Range on mutable file without validator. | 200 complete representation. | Tier-2 mutable download path. | `internal/server/download_range_test.go` — Tier-2 range test | Range is ignored and full body returned. | PASS | — |
| 29 | Tier-2 weak If-Range | Range with weak validator. | 200 complete representation. | Tier-2 validator policy. | `internal/server/download_range_test.go` — weak Tier-2 validator test | Weak validator is ignored. | PASS | — |
| 30 | Tier-1 strong If-Range | Matching strong SHA-256 If-Range. | 206 requested slice. | Tier-1 immutable file path. | `internal/server/download_range_test.go` — matching strong validator test | Partial status, range, and body asserted. | PASS | — |
| 31 | Tier-1 invalid If-Range | Mismatched strong validator. | Ignore Range; 200 full. | Tier-1 validator mismatch path. | `internal/server/download_range_test.go` — mismatching validator test | Full representation asserted. | PASS | — |
| 32 | Checkpoint Sync failure | Inject error during `partFile.Sync()`. | Do not advance committed offset; return error. | Checkpoint and durable-offset update. | `internal/transfer/resumable_test.go` — checkpoint Sync failure test | Durable offset remains at prior checkpoint. Go unit. | PASS | — |
| 33 | Metadata replace failure | Fail atomic `.meta.tmp` rename. | No new offset acknowledgement; safe rollback. | Atomic metadata replacement. | `internal/transfer/resumable_test.go` — metadata replacement failure test | Replacement failure preserves prior metadata. Go unit. | PASS | — |
| 34 | Two concurrent PATCH clients | Concurrent PATCH requests to one upload. | First lock wins; second gets 423 and Retry-After. | Tus lifecycle mutex and lock headers. | `internal/server/tus_upload_test.go` — concurrent PATCH test | 423 and retry response are asserted. Go HTTP integration. | PASS | — |
| 35 | Full offset with status check | Reach 100%; query status. | Awaiting_digest/verifying do not render completion. | Client completion state machine and digest endpoint. | `frontend/test/resumableUpload.test.ts` — `Scenario 35: Full byte progress does not report success before digest verification` | Deferred completion proves ordered 100% progress, digest submission, pending completion, success only after a successful response, and no success callback after digest rejection. The client exposes no `verifying` callback/state; Node VM. | PARTIAL | Add an explicit UI/status contract for `verifying`, then validate it in a browser/UI acceptance test. |
| 36 | Quota restoration on restart | Restart with outstanding reservations. | Restore reservations before accepting new uploads. | `RecoverFromStaging` and manager startup. | `internal/transfer/crash_subprocess_test.go` — `TestWp4Crash_Scenario36_QuotaReservationsRestoredFromStaging` | Real child crash leaves reservation; fresh manager rejects 5 MB and accepts remaining 4 MB. | PARTIAL | Prove listener admission ordering in the desktop server. |
| 37 | Concurrent reads | Multiple simultaneous Range requests. | Independent file handles; no seek interference. | Download file serving. | `internal/server/download_range_test.go` — concurrent read test | Concurrent bodies and offsets are asserted. Go integration. | PASS | — |
| 38 | Completion digest | Submit matching expected SHA-256. | Verify accumulated digest and atomically publish. | `FinalizeComplete`. | `internal/server/tus_upload_test.go` and `internal/transfer/resumable_test.go` — completion digest tests | Matching digest publishes and state becomes completed. | PASS | — |
| 39 | Browser close at 100% | Close after final PATCH before complete. | Reopen and complete without re-upload. | Awaiting-digest persistence and client completion. | `frontend/test/resumableUpload.test.ts` — `Scenario 39 & 43` | Rebuilds hash and completes at full offset under Node stubs. | PARTIAL | Real browser close/reopen acceptance. |
| 40 | Completion digest mismatch | Submit incorrect digest. | Reject, fail, and quarantine `.part`. | Digest verification and quarantine. | `internal/server/tus_upload_test.go` and `internal/transfer/resumable_test.go` — mismatch tests | Wrong digest is rejected and publication is withheld. | PASS | — |
| 41 | Same-size substitution | Replace destination with foreign same-size file. | Detect SHA mismatch and fail publication. | Finalization target integrity check. | `internal/transfer/resumable_test.go` — substitution test | Same-size foreign content is rejected. | PASS | — |
| 42 | Browser streaming fallback | Use direct Blob XHR slices. | Sequential bounded-memory slices and completion. | Worker fallback path. | `frontend/test/resumableUpload.test.ts` — `Scenario 42` | Slice order, offsets, and digest flow asserted in Node VM. | PARTIAL | Real unsupported-browser execution and memory measurement. |
| 43 | Restart awaiting_digest | Crash at 100% before digest. | Preserve awaiting_digest; reconnect and complete. | Startup recovery and completion. | `internal/transfer/crash_subprocess_test.go` — `TestWp4Crash_Scenarios14_43_RestartInAwaitingDigestCompletesWithoutReupload`; `internal/server/scenario43_http_restart_test.go` — `TestWp4Scenario43HTTPServerProcessRestart` | The manager test restores `awaiting_digest`, the committed offset, and staged bytes. The HTTP test performs the real Tus POST/PATCH through child server A, forcibly terminates it with SIGKILL after the complete PATCH response, and starts fresh server B over the same durable directory. B restores the original session and coordinator identity before readiness; HTTP HEAD recovers the original upload at full offset in `awaiting_digest`, and HTTP completion publishes the exact bytes with no PATCH retransmission. Repeated completion is also accepted. | PASS | — |
| 44 | Repeated completion | Submit matching completion repeatedly. | Idempotent 200 without error. | Publication record/idempotent finalization. | `internal/transfer/crash_subprocess_test.go` and resumable tests | Matching repeated finalization returns existing publication. | PASS | — |
| 45 | Interrupted PATCH hasher rebuild | Drop at 350 MB; server committed 320 MB. | HEAD, rebuild prefix, verify, continue. | Worker rebuild and resumable hash state. | `frontend/test/resumableUpload.test.ts` — `Scenario 13 & 45` | Prefix rebuild and continuation are asserted. Node VM. | PASS | — |
| 46 | Lifecycle mutex serialization | Concurrent PATCH and complete/DELETE. | Serialize; competing request gets 423. | `lifecycleMu` in Tus handler. | `internal/server/tus_upload_test.go`; transfer lifecycle tests | Competing lifecycle operations and lock result asserted. | PASS | — |
| 47 | Final EOF checkpoint failure | Fail final Sync or metadata replace. | No 204; preserve prior offset; return error. | EOF checkpoint error path. | `internal/transfer/resumable_test.go` — final checkpoint failure test | Error and unchanged durable offset asserted. Go unit. | PASS | — |
| 48 | Duplicate checkpoint prevention | Threshold fires exactly at EOF. | Skip redundant Sync/metadata replace; return 204. | EOF checkpoint guard. | `internal/transfer/resumable_test.go` — exact-EOF checkpoint test | Zero uncommitted bytes avoids duplicate barrier and succeeds. | PASS | — |
| 49 | Active cancellation | Abort PATCH during active stream. | Sync uncommitted, release lock, DELETE cleanup, isolate batch. | Abort/context handling and DELETE. | `frontend/test/resumableUpload.test.ts` — `Scenario 49`; Tus cancellation tests | Abort, 423 retry, DELETE, cleanup, and batch isolation asserted. Node/Go loopback. | PARTIAL | Real browser abort and desktop cancellation acceptance. |

## Remaining gaps

The only **NOT TESTED** scenario is Scenario 19 (desktop revocation). Partial
scenarios are 1, 2, 3, 11, 14, 15, 18, 35, 36, 39, 42, and 49. These are
partial because their critical boundary is not covered, not because the related
implementation is absent.

Physical Wi-Fi, Android Chrome, and iOS Safari evidence remains outstanding.
