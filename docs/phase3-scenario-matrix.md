# Flitnix Phase 3 — Scenario Matrix (Revision 2.8, Section 16)

**Document status:** This file is the tracked, in-repository reproduction of the authoritative
49-scenario failure, recovery & safety requirements used by the Phase 3 WP4 audit, testing, and
reporting. The original document lives outside the repository — see provenance below.

---

## Provenance

This document reproduces the 49-scenario requirements matrix from:

> **Flitnix — Phase 3: Resumable Transfer Protocol**
> **Final Approved Architecture (Revision 2.8 Implementation-Safety Corrections)**
> Section 16: *Expanded 49-Scenario Failure, Recovery & Safety Test Matrix*

How the source was recovered:

1. The materialized Revision 2.8 copy used for this transcription is located at
   `/home/itsyamin/.gemini/antigravity-cli/brain/34777493-b277-42de-a96d-1efe3c810bff/phase-3-resumable-transfer-architecture.md`
   (79,050 bytes; last modified 2026-11-01 12:08 +0600). Its Section 16 table spans lines 802–855
   and is reproduced verbatim below.
2. The working session referred to the original paste under the name
   `Pasted text(20261101-061112).txt`. That literal filename does **not** exist anywhere on disk:
   the editor materializes pastes as `Pasted text #N.txt`, and none of the materialized attachments
   contains the matrix. The brain-path copy above is the recovered Revision 2.8 content.
3. Because the original document lives outside this repository, this tracked Markdown is a carefully
   transcribed reproduction. All 49 rows are transcribed in full, with the original four columns
   (ID | Failure Scenario | Injection Mechanism | Expected System Recovery), exact scenario names,
   exact HTTP codes, headers, sizes, and state names. Nothing is paraphrased or invented.
4. Correction of record: an earlier revision of this file claimed the original document had "not
   been located" and reproduced only a condensed three-column summary derived from the WP4 mission
   statement. That claim was wrong; the condensed table has been replaced by the full transcription.
   If the original Revision 2.8 document is later added to the repository, it should replace this
   reproduction and the coverage mapping should be re-diffed against it.

---

## Section 16 — Expanded 49-Scenario Failure, Recovery & Safety Test Matrix

| ID | Failure Scenario | Injection Mechanism | Expected System Recovery |
| :---: | :--- | :--- | :--- |
| **1** | Wi-Fi dropped at 10% | Abort TCP connection at 10% bytes. | Server retains committed bytes in `.part`; item transitions to `Interrupted`; no batch cancel. |
| **2** | Wi-Fi dropped at 50% | Toggle airplane mode mid-stream. | Server records exact 50% checkpoint in `.meta`; client detects drop and schedules backoff retry. |
| **3** | Wi-Fi dropped at 99% | Cut socket right before completion. | 99% bytes preserved; client resumes remaining 1% on reconnect; total transfer completes. |
| **4** | Lost `POST` response | Drop TCP packet right after server creates `.part`. | Client retries `POST`; server idempotently returns `201 Created` with existing resource URL. |
| **5** | Lost `PATCH` response | Drop TCP packet right after server syncs final chunk. | Server is at 100% (`awaiting_digest`); client retries `HEAD`; discovers offset == size; submits digest. |
| **6** | Duplicate `PATCH` request | Client sends two overlapping `PATCH` streams. | First stream proceeds; second stream receives `423 Locked`. |
| **7** | Client sends wrong offset | Send `PATCH` with offset 20 MB when server is at 10 MB. | Server rejects immediately with `409 Conflict` and `Upload-Offset: 10485760`. |
| **8** | Uncheckpointed trailing bytes | Server crashes during write before checkpoint sync. | Server restarts, truncates `.part` to `meta.committedOffset`; offset consistency restored. |
| **9** | Missing partial file | Delete `.part` while `.meta` remains on disk. | Server detects inconsistency, returns `410 Gone`; client restarts fresh upload from byte 0. |
| **10** | Corrupted metadata file | Inject invalid JSON bytes into `.meta`. | Server quarantines `.meta.corrupted`, returns `404 Not Found`; prevents undefined offset reads. |
| **11** | Hard crash during finalization | Kill -9 server process while renaming `.part` to final. | On restart, server checks durable publication record; if verified, marks completed idempotently. |
| **12** | Modified file on re-selection | Modify byte at offset 50 MB, re-select, attempt resume. | Tier 2 incremental SHA-256 detects hash mismatch; prompts user: "File modified, restart from 0". |
| **13** | Identical header, altered body | File with identical name, size, mtime, but altered tail. | Fast screen passes, but Tier 2 prefix SHA-256 catches mismatch before sending `PATCH`. |
| **14** | Desktop application restart | SIGTERM Flitnix desktop mid-upload; restart app. | Background worker loads `.meta`; restores binary hash state or falls back to re-hashing committed bytes. |
| **15** | Browser reload / kill | User refreshes browser tab or OS kills tab. | IndexedDB restores transfer metadata; user re-selects file; Tier 1 + Tier 2 verifies and resumes. |
| **16** | Disk full (`ENOSPC`) | Artificially fill target drive during upload stream. | Server catches `ENOSPC`, syncs last valid block, updates state to `paused_storage_full`, returns `507`. |
| **17** | Concurrent upload quota | Attempt 15 parallel multi-gigabyte uploads. | Quota manager enforces max 10 active resumables; rejects excess with `507` or `429 Too Many Requests`. |
| **18** | Expired upload session | Send `PATCH` after 24-hour expiration window. | Server returns `404 Not Found` or `410 Gone`; cleanup worker purges orphaned files. |
| **19** | Session revoked by desktop | User clicks "Revoke Device" on desktop UI. | In-flight upload immediately aborted; all continuation tickets for session purged; returns `401`. |
| **20** | Guest Drop Box interruption | Interrupt upload to Guest Drop Box room. | Guest room retains partial upload record; guest resumes upon Wi-Fi reconnect. |
| **21** | Download `If-Range` match | Issue `GET Range` with valid matching `If-Range` (Tier 1). | Server returns `206 Partial Content` with requested byte slice. |
| **22** | Download `If-Range` mismatch | Source file updated on desktop, client issues old Range. | Server ignores Range, returns `200 OK` with full new file body from byte 0. |
| **23** | Download `416 Out of Bounds` | Client requests `Range: bytes=9999999999-`. | Server returns `416 Range Not Satisfiable` with `Content-Range: bytes */total`. |
| **24** | Dynamic ZIP Range attempt | Client requests `Range: bytes=100-` on multi-file ZIP. | Server ignores Range, serves full stream with `Accept-Ranges: none` and `200 OK`. |
| **25** | Weak ETag rejection (Tier 1) | Client sends `W/"..."` weak ETag with `If-Range` to Tier 1 file. | Go `http.ServeContent` rejects weak ETags for Range requests per RFC 9110 Section 13.1.2; serves `200 OK` full file. |
| **26** | Expired download ticket | Client attempts Range resume after 30 min idle. | Server returns `401 Unauthorized` or `410 Gone`; client must request fresh download grant. |
| **27** | Concurrent Cookie Isolation | Run 4 simultaneous downloads in one browser; interrupt & resume each. | Each download utilizes its unique `flitnix-cont-<id>` cookie at `Path=/api/downloads/<id>`; zero cross-talk or collision. |
| **28** | Tier 2 Range without If-Range | Client sends `Range: bytes=100-` to Tier 2 mutable file. | Server explicitly ignores Range header; serves complete representation with `200 OK` from byte 0. |
| **29** | Tier 2 Range with weak If-Range | Client sends `Range: bytes=100-` and `If-Range: W/"..."` to Tier 2 file. | Server explicitly ignores Range header; serves complete representation with `200 OK` from byte 0. |
| **30** | Tier 1 valid strong If-Range | Client sends `Range: bytes=52428800-` and matching `If-Range: "sha256-<hex>"` to Tier 1 file. | Server evaluates `http.ServeContent`; returns `206 Partial Content` with requested byte slice. |
| **31** | Tier 1 invalid If-Range | Client sends `Range: bytes=52428800-` and mismatched `If-Range: "sha256-wrong"` to Tier 1 file. | Server ignores Range per RFC 9110; serves complete representation with `200 OK` from byte 0. |
| **32** | Checkpoint `f.Sync()` failure | Inject disk write error during mid-stream `partFile.Sync()`. | Server aborts checkpoint, does NOT advance `committedOffset`, retains last durable checkpoint, returns error. |
| **33** | Metadata replace failure | Simulate filesystem error during atomic rename of `.meta.tmp`. | Server does NOT acknowledge new offset to client; crash recovery safely rolls back uncommitted bytes back to last valid `.meta`. |
| **34** | Two Concurrent PATCH Clients | Two separate HTTP clients issue concurrent `PATCH` to same upload. | First client acquires lock; second client immediately receives `423 Locked` with `Retry-After: 1`. |
| **35** | Full Offset with Status Check | Upload reaches 100% bytes; client checks status endpoint. | Server returns `state: awaiting_digest` then `verifying`; does not render success check until completion succeeds. |
| **36** | Quota Restoration on Restart | Restart desktop server with 2 interrupted uploads totaling 15 GB unwritten. | `QuotaManager.RecoverFromStaging` restores 15 GB reservation before listeners open; rejects excessive new upload with 507. |
| **37** | Concurrent Reads of Same File | Multiple clients issue simultaneous Range requests to same Tier 1 file. | Server serves all clients concurrently using independent `*os.File` handles with zero seek cursor interference. |
| **38** | Completion Digest Submission | Client sends `POST .../complete` with matching `expectedSHA256`. | Server verifies against write-time accumulated digest, atomically publishes, updates state to `completed`. |
| **39** | Browser Close at 100% Bytes | Browser closes right after final PATCH response before `POST .../complete`. | User re-opens, reselects file; client sends `POST .../complete`; server finalizes without re-upload. |
| **40** | Completion Digest Mismatch | Client sends `POST .../complete` with mismatched digest. | Server compares against accumulated write-time digest, rejects publication, marks `failed`, quarantines `.part`. |
| **41** | Same-Size File Substitution | External process places same-size foreign file at destination before restart. | On restart, server checks target file SHA-256 against `expectedSHA256`; detects mismatch, rejects publication, marks `failed`. |
| **42** | Browser Streaming Fallback | Test client using direct `Blob` XHR fallback on unsupported browser. | Slices stream sequentially; worker hashes per chunk; memory $< 16\text{ MB}$; finalizes successfully via `POST .../complete`. |
| **43** | **Restart in `awaiting_digest`** | Server crashes while upload is at 100% bytes in `awaiting_digest` state. | Startup recovery preserves upload in `awaiting_digest` without premature publication or failure; client reconnects and completes. |
| **44** | **Idempotent Repeated Completion** | Client sends repeated `POST .../complete` with matching `expectedSHA256`. | Server verifies existing completed publication record and returns idempotent `200 OK` without error. |
| **45** | **Interrupted PATCH Hasher Rebuild** | Connection drops at 350 MB. Server committed 320 MB. | Client queries `HEAD`, Web Worker rebuilds on `[0..320MB]`, verifies prefix, and continues streaming into same hasher. |
| **46** | **Lifecycle Mutex Serialization** | Concurrent `PATCH` and `complete` / `DELETE` requests issued. | Server serializes via `lifecycleMu`; concurrent request returns `423 Locked`. |
| **47** | **Final EOF Checkpoint Failure** | Inject I/O error during final `partFile.Sync()` or `.meta` replace at EOF. | Server catches error, does NOT return 204 No Content, preserves previous committed offset, returns error. Client does not enter `awaiting_digest`. |
| **48** | **Duplicate-Checkpoint Prevention** | Threshold checkpoint syncs right at EOF boundary (`uncommittedBytes == 0`). | Server detects `uncommittedBytes == 0` at EOF, skips redundant `Sync()` and `.meta` rename, avoids double barrier stall, responds with 204. |
| **49** | **Active Upload Cancellation** | User clicks Cancel during active 10 GB stream. | Client aborts PATCH via `AbortController`; server Go handler catches context cancellation, syncs uncommitted, releases lock; client sends `DELETE`; staging purged; other batch items unaffected. |

*Note: rows 43–49 carry bold scenario titles in the original source; rows 1–42 do not. Row 42's
"memory $< 16\text{ MB}$" reproduces the original document's wording verbatim.*

---

## Corrections to earlier mappings (recorded for audit trail)

- **Scenario 16** is *Disk full / ENOSPC (507)*, not upload-quota rejection.
- **Scenario 17** is *Concurrent upload quota* (limit of 10 active resumables).
  The historical test `TestTusScenario_16_17_StorageQuotaRejection` covers both via an injected
  quota manager / free-space function, but each scenario's PASS status must be established
  independently — see the coverage document.
- Wake-lock test labels "12"/"13" in `frontend/test/transferAwareness.test.ts` are **case indices of the
  wake-lock suite, not scenario IDs**, and are excluded from this mapping.
- Benchmark and recovery runs in `benchmarks/wp4/` execute over loopback HTTP on the local
  workstation. Loopback results are **not** physical Wi-Fi or physical-device results; environments
  are classified per scenario in the coverage document and never conflated.

## Status legend

Each of the 49 scenario IDs carries exactly one status in
[`docs/phase3-scenario-coverage.md`](./phase3-scenario-coverage.md):

- **PASS** — requirement verified by a passing automated test with meaningful assertions.
- **PARTIAL** — production behavior exists but verification is incomplete (e.g. in-process substitute for
  process crash, grouped coverage without dedicated assertions).
- **NOT TESTED** — no automated verification exists.

Execution environments (Go unit, Go integration, Vitest, loopback HTTP, crash simulation, browser,
physical) are classified per scenario in the coverage document and are never conflated: loopback and
software fault-injection results are **not** evidence of physical Wi-Fi or physical-device behavior.
