# Flitnix — Scenario 43: Durable Identity and Coordinator Recovery Design

**Status:** Architecture proposal only — no production implementation authorized by this document  
**Scenario classification:** **PARTIAL**  
**Scope:** Personal Transfer session identity, batch/item ownership, and resumable-upload continuation after an abrupt server-process restart  
**Primary constraint:** Preserve maximum practical transfer speed. Reliability must not introduce per-chunk persistence, extra streaming copies, repeated hashing, or weaker durability.

---

## 1. Executive decision

Recommend **Architecture A: a durable session-identity registry plus a durable coordinator lifecycle journal**, restored into the existing in-memory session and coordinator structures before the HTTP listener accepts requests.

Do not introduce an independently authorized per-upload bearer credential as the primary recovery mechanism at this stage.

Architecture A is the smaller semantic change to the existing security model:

- The phone continues to authenticate with its existing paired-session bearer token or supported session cookie.
- The server persists a verifier for that token, not the raw token.
- The existing stable session ownership key remains bound to resumable metadata.
- Batch/item IDs, ownership, approval, and lifecycle state are restored before handlers can use them.
- `ResumableManager` remains responsible for durable bytes, committed offsets, prefix digests, and publication.
- HTTP handlers continue to enforce authentication, session ownership, approval, and item lifecycle checks after restart.

**Performance rule:** durable identity/coordinator records are written only for relatively rare lifecycle transitions (pair/session creation or rotation, batch declaration, approval, cancellation, revocation, and terminal completion). There must be no identity-ledger write or `fsync` in the PATCH byte-copy loop, per chunk, per checkpoint, or per UI progress event. The existing resumable checkpoint policy remains **32 MB / 500 ms**. The streaming pipeline remains single-pass write-then-hash.

This design does not claim that Scenario 43 is fixed. It specifies the work required to fix it safely.

## 2. Evidence basis and current-state findings

The following observations come from the latest OpenCode source-inspection session and the previously inspected Revision 2.8 architecture material. They are not a fresh live checkout inspection by this document's author; implementation must re-check the current source and line numbers before coding.

### 2.1 Current lifecycle

The current Personal Transfer path is understood to be:

`Session → Batch → Item → ResumableUpload → Completion → Publication`

1. A paired session is represented by a bearer token and an in-memory `session.Manager` registration.
2. `Session.Key()` is derived from the random session token and is used as the stable owner key associated with an upload. The current resumable metadata includes a `sessionKey` field.
3. `transfer.Coordinator.CreateBatch` creates batch/item identities and coordinator state in memory. The latest inspection found no supported API to restore an existing batch/item with the same IDs in a fresh process.
4. Personal Tus handlers authenticate the session and compare the resumable upload's owner key with the active session key. The earlier security audit also records that cross-session access is rejected and revoked sessions fail authentication.
5. The resumable manager persists upload bytes and metadata under its staging area. The metadata includes fields such as `transferId`, `batchId`, `itemId`, `sessionKey`, `totalSize`, `committedOffset`, `prefixDigest`, hash state, upload state, timestamps, and expiry.
6. Completion checks the expected SHA-256 and lifecycle/ownership conditions, then uses the existing durable publication mechanism. The architecture specifies a publication record bound to `transferId`, expected digest, and target path before atomic publication.

### 2.2 Persisted versus memory-only state

| State | Current evidence | Restart consequence |
|---|---|---|
| `.part` payload bytes | Persisted in staging | Can survive process termination. |
| `.meta` resumable metadata | Atomic, durable sidecar | Can restore transfer ID, committed offset, owner key, digest/state metadata, and expiry. |
| Publication record | Durable record used by finalization/recovery | Supports idempotent publication recovery; must remain bound to verified digest and target. |
| Session registration/token lookup | In-memory manager, per latest inspection | A fresh manager cannot authenticate the old bearer token without a durable verifier record. |
| Stable session ownership key | Derived from the random session token; also associated with upload metadata | Must remain identical after restart or be safely re-bound through an explicit trusted flow. |
| Coordinator batch/item records and approval | In-memory coordinator, per latest inspection | New coordinator creates new IDs; old item ownership/approval is unavailable. |
| Quota reservations | Reconstructed from resumable staging during manager startup | Must still be restored before listener admission. |
| Per-upload lifecycle mutex/active request state | Runtime-only synchronization | Must be recreated fresh; never serialize mutexes, contexts, goroutines, or open file handles. |

### 2.3 Why Scenario 43 currently stops

The resumable manager can recover the upload's durable metadata, but the fresh server cannot currently reconstruct the old session registration and coordinator batch/item ownership through supported production APIs. The HTTP handler therefore cannot safely treat the recovered `.meta` file as sufficient authority to continue or publish the file.

That rejection is correct fail-closed behavior. The fix must restore the missing authorization and lifecycle records, not bypass the checks.

## 3. Goals and non-goals

### Goals

- Preserve the same session identity across an abrupt process restart while the session remains valid and unrevoked.
- Restore the same batch/item IDs, ownership, approval state, and queue/lifecycle semantics.
- Reconcile durable coordinator state with resumable metadata and publication records.
- Restore quota reservations and upload registrations before listening.
- Allow the original client to use the original authenticated Tus URL: `HEAD`, then completion, without re-uploading bytes.
- Ensure revocation, expiry, cancellation, and approval changes survive restart.
- Fail closed on missing, corrupt, inconsistent, or untrusted records.
- Keep steady-state streaming throughput and durability behavior unchanged.

### Non-goals

- Replacing Tus or redesigning the upload protocol.
- Replacing the existing in-memory session/coordinator runtime structures; they can remain in-memory caches restored from durable state.
- Persisting every progress update or byte offset in a second database. The resumable `.meta` file remains authoritative for committed byte progress.
- Persisting goroutines, mutexes, request contexts, open file descriptors, or live worker state.
- Treating an upload ID, batch ID, item ID, filename, or `.part` path as an authentication credential.
- Automatically adopting old unbound staging files without trusted ownership validation.
- Claiming Wails process restart, physical Wi-Fi, Android Chrome, or iOS Safari acceptance from this design alone.

## 4. Architecture alternatives

### 4.1 Option A — Durable session and coordinator restoration

Persist the minimum authoritative identity and lifecycle state required to reconstruct the existing session manager and coordinator.

**Advantages**

- Preserves the current bearer-session security boundary.
- Retains existing owner checks based on the stable session key.
- Retains existing batch/item IDs and approval requirements.
- Does not introduce a new browser-held upload secret.
- Keeps all hot-path authorization checks in memory after startup restoration.
- One durable state model can support resumable uploads, batch queue continuity, cancellation, and session revocation—not just Scenario 43.

**Costs and risks**

- Requires a versioned durable state store and replay/reconciliation logic.
- Session token verification, revocation, and expiry semantics must be made durable.
- Coordinator event transitions must be crash-consistent with the existing `.meta` and publication records.
- Migration must not silently grant ownership to old uploads for which no durable owner/approval record exists.

### 4.2 Option B — Independent per-upload continuation credentials

Mint a high-entropy upload-scoped bearer capability at Tus creation and bind it to one upload. Persist only a verifier server-side, and require the capability for continuation after restart.

**Advantages**

- Can narrow a credential's authority to one upload rather than an entire session.
- Could decouple upload continuation from restoring the complete coordinator object graph.
- May be useful later for a deliberately designed delegated-upload protocol.

**Costs and risks**

- The credential is a new bearer secret. If the browser must survive reload, it needs durable client-side storage or a cookie-based redesign.
- The existing architecture explicitly says IndexedDB must not store pairing tokens, authentication cookies, or grant secrets. Persisting a new capability there would violate that rule.
- A capability must still bind to a durable owner/session identity, expiry, approval decision, and revocation generation. Otherwise revocation after restart becomes unsafe.
- A capability alone does not restore coordinator completion state, item approval, batch isolation, or terminal cancellation state.
- Adds another credential issuance, rotation, storage, validation, logging-redaction, replay, and revocation surface.
- It risks becoming a second authorization model that can diverge from the paired-session model.

### 4.3 Decision matrix

| Requirement | Option A: durable session/coordinator | Option B: upload capability |
|---|---|---|
| Preserve current paired-session authentication | **Direct fit** | Requires parallel auth path or continued session restore anyway |
| Restore batch/item approval | **Directly restores authoritative records** | Still needs durable approval/owner evidence |
| Revocation after restart | Durable session status/generation | Durable owner revocation index still required |
| Avoid new browser-held secret | **Yes** | Usually no, unless cookie/client architecture changes |
| IndexedDB “no sensitive tokens” rule | **Preserved** | At risk if capability must survive reload |
| Runtime validation cost | In-memory token-verifier and ownership lookup | Additional capability lookup/validation |
| Scope beyond Scenario 43 | Supports broader coordinator/session recovery | Primarily upload-specific |
| Implementation complexity | Higher persistence/replay work, but one coherent lifecycle | Higher security-protocol complexity plus remaining coordinator persistence |

**Recommendation: Option A.** Option B should be reconsidered only if Flitnix later has a concrete delegated-upload requirement that justifies a separate capability model. It is not the shortest safe fix for the current restart failure.

## 5. Recommended architecture: durable identity/coordinator ledger

### 5.1 Runtime model

Keep the existing in-memory `session.Manager` and `transfer.Coordinator` APIs for request-time behavior. Add a durable state repository beneath their lifecycle operations.

At startup:

- Replay and validate the durable ledger.
- Reconstruct active sessions and coordinator batches/items in memory with their original IDs and ownership.
- Restore resumable uploads from `.meta` and `.part` using the existing manager.
- Reconcile the two sources of truth.
- Restore quota reservations.
- Only then declare the HTTP listener ready.

During normal operation, requests continue to use in-memory maps and per-upload locks. The durable repository is not queried for every streamed buffer.

### 5.2 State ownership boundaries

Do not duplicate all resumable state in the identity ledger.

| Data | Authoritative owner |
|---|---|
| Bearer token verifier, session ID, session key, session status, generation, expiry, revocation | Durable identity ledger |
| Batch ID, item ID, session ownership, approval decision, item lifecycle/queue state, transfer association | Durable coordinator ledger |
| Transfer ID, committed offset, total size, prefix digest/hash state, `.part` integrity, resumable state, transfer expiry | Existing resumable `.meta` |
| Publication intent, expected digest, target binding, publication completion | Existing durable publication record plus completed resumable metadata |
| Live request state, mutexes, cancellation contexts, open descriptors, streaming hasher instance | Reconstructed/runtime-only state |

The identity/coordinator ledger is authoritative for **who is allowed to act on an upload**. The resumable metadata is authoritative for **which bytes have been durably committed**. Neither source can independently authorize completion.

## 6. Proposed durable records

The following are logical schema proposals, not current production structs. Use explicit schema versions and typed validation.

### 6.1 Session record

```json
{
  "schemaVersion": 1,
  "recordType": "session",
  "sessionId": "sess_<random-id>",
  "sessionKey": "<existing stable owner-key derivation>",
  "tokenVerifier": {
    "algorithm": "HMAC-SHA256",
    "keyId": "install-key-v1",
    "digest": "<hex verifier>"
  },
  "generation": 1,
  "status": "active",
  "createdAt": "2026-11-02T00:00:00Z",
  "expiresAt": "2026-11-09T00:00:00Z",
  "revokedAt": null,
  "deviceLabel": "<non-secret label>"
}
```

Rules:

- Never persist the raw bearer token.
- Generate bearer tokens from a cryptographically secure random source with at least 256 bits of entropy.
- Store a keyed verifier (HMAC-SHA256) under an installation secret. Validate an incoming token by deriving its verifier and looking up the record; use constant-time comparison for verifier values.
- Preserve the exact existing `Session.Key()` compatibility contract during migration. If it is currently derived from the token, persist the resulting stable `sessionKey`; do not silently change the value for existing `.meta` ownership fields.
- Keep `sessionId` separate from `sessionKey` and from the bearer token. The ID is an identity reference, not a credential.
- Persist explicit status and generation. Revocation or credential rotation increments the generation and invalidates older verifier records.
- Persist absolute expiry. If the current implementation also has a sliding idle timeout, preserve its semantics with a bounded durable activity checkpoint (for example, at most once per configured activity window), not on each streamed chunk. On restart, a stale activity timestamp may shorten a session's remaining life; it must never extend authorization.

### 6.2 Batch record

```json
{
  "schemaVersion": 1,
  "recordType": "batch",
  "batchId": "b_<stable-id>",
  "sessionId": "sess_<random-id>",
  "sessionKey": "<stable owner key>",
  "manifestDigest": "<sha256 of canonical manifest>",
  "approval": {
    "state": "approved",
    "approvedAt": "2026-11-02T00:02:00Z",
    "approvedBy": "desktop-user",
    "policyVersion": 1
  },
  "createdAt": "2026-11-02T00:01:00Z",
  "items": ["it_<id_1>", "it_<id_2>"]
}
```

Persist the approval decision and the identity of the authority that made it. A batch must not become approved merely because an upload `.meta` exists.

### 6.3 Item record

```json
{
  "schemaVersion": 1,
  "recordType": "item",
  "batchId": "b_<stable-id>",
  "itemId": "it_<stable-id>",
  "sessionId": "sess_<random-id>",
  "sessionKey": "<stable owner key>",
  "state": "awaiting_digest",
  "approvalState": "approved",
  "transferId": "<transfer-id>",
  "name": "archive.tar",
  "sanitizedName": "archive.tar",
  "destinationRootId": "<trusted configured root id>",
  "relativePath": "backups/archive.tar",
  "totalSize": 1073741824,
  "createdAt": "2026-11-02T00:03:00Z",
  "updatedAt": "2026-11-02T00:04:00Z",
  "expiresAt": "2026-11-03T00:03:00Z"
}
```

Rules:

- Persist identifiers and validated destination references, not arbitrary unchecked absolute paths supplied by a remote client.
- Resolve `destinationRootId` against desktop-configured trusted roots at startup and revalidate `relativePath` against traversal and symlink policies.
- Do not persist byte progress here. Read committed offset from resumable metadata.
- Persist lifecycle transitions and approval/cancellation state, not every percentage update.
- The `state` above is illustrative; map it to the actual coordinator state machine during implementation. Do not conflate coordinator item state with the resumable manager's `awaiting_digest` state.

### 6.4 Append-only event envelope

Use a small standard-library-only journal initially. One logical event per lifecycle transition:

```json
{
  "schemaVersion": 1,
  "sequence": 1842,
  "eventId": "evt_<random-id>",
  "type": "ItemUploadAllocated",
  "occurredAt": "2026-11-02T00:03:00Z",
  "payload": { "batchId": "b_<id>", "itemId": "it_<id>", "transferId": "<id>" },
  "integrity": { "algorithm": "HMAC-SHA256", "keyId": "install-key-v1", "value": "<mac>" }
}
```

The event format should be length-framed so startup can distinguish an incomplete trailing record from corruption in the middle of the journal. Bound the maximum record length. Include monotonic sequence numbers and event IDs for ordering and idempotency.

### 6.5 Storage location and key protection

Use a central application state directory, separate from individual destination directories and separate from each destination's `.flitnix-tmp` payload staging directory:

```text
<Flitnix application state directory>/
  identity/
    state.snapshot.json
    state.wal
    install-key-v1
    server.lock
```

The exact platform path must be confirmed against the current application configuration. Do not scatter session/coordinator state across arbitrary download destinations.

- Linux/macOS: directories `0700`, files `0600`.
- Windows: restrictive user-only ACLs; do not assume POSIX modes provide equivalent protection.
- Generate the installation key with `crypto/rand`; write it atomically and durably before using it.
- Prefer OS-protected secret storage where the current platform integration can provide it without introducing an undeclared dependency. If a protected key cannot be opened, fail closed for session restoration and require re-pairing; never silently regenerate a new key and accept old records.
- Keep the key ID in records to support deliberate key rotation. Never log tokens, token verifiers, HMAC keys, or full credential-bearing records.

## 7. Journal durability and consistency

### 7.1 Append protocol

For a critical event:

1. Validate the requested state transition in memory.
2. Construct a complete versioned event with the next sequence number.
3. Serialize and authenticate the event.
4. Append one complete length-framed record under a journal mutex.
5. Call `Sync()` on the journal file.
6. Only after durable append succeeds, publish the corresponding in-memory state transition and acknowledge the operation.

If append or `Sync()` fails, do not report the transition as successful. Keep the previous in-memory state authoritative or mark the affected subsystem unavailable until reconciliation; do not proceed as if persistence succeeded.

### 7.2 Replay protocol

At startup:

- Verify each complete record's framing, schema, sequence, and integrity value.
- A genuinely incomplete final frame may be truncated to the last complete valid offset after preserving diagnostic evidence.
- A bad MAC/checksum, invalid schema, duplicate/conflicting sequence, or corruption before the physical end is not a harmless trailing tear. Fail closed and require recovery/quarantine; do not skip the bad record and replay later events.
- Make event application idempotent using sequence/event IDs.

### 7.3 Snapshot and compaction

The journal must not grow without bound. Add snapshot compaction only after the basic replay path is proven:

1. Build a consistent in-memory snapshot through sequence N.
2. Write `state.snapshot.json.tmp`.
3. `Sync()` the temp file, close it, atomically replace the snapshot, and sync its parent directory where supported.
4. Only after the snapshot is durable, rotate/truncate the WAL while retaining every event after N.
5. Make replay understand the snapshot sequence and remaining WAL sequence range.

Use the existing cross-platform atomic-replace/directory-sync helpers where appropriate. Do not invent weaker durability semantics for the identity store than for resumable metadata.

## 8. Lifecycle transaction boundaries

The journal and resumable metadata are separate durable stores. Define deterministic reconciliation rules; do not pretend two independent filesystem writes are one atomic transaction.

### 8.1 Pair/session creation

1. Generate token and stable session identity.
2. Derive stable `sessionKey` using the current compatibility algorithm.
3. Persist `SessionCreated` with the token verifier and expiry; sync it.
4. Register the session in memory.
5. Only then return the bearer token to the client.

A crash before step 3 leaves no valid session. A crash after step 3 but before the response may leave an unused active session; this is safe and can expire normally. Do not persist the raw token to replay it to the client.

### 8.2 Batch declaration and approval

- Persist batch and item identities, owner session ID/key, manifest digest, destination root reference, and initial state before acknowledging batch creation.
- Persist the desktop approval decision before allowing Tus resource creation.
- On restore, reconstruct the exact original IDs and approval state.
- Unapproved, revoked, expired, or cancelled items remain unable to create/continue uploads.

### 8.3 Tus upload allocation

Treat allocation as a recoverable two-store operation:

1. Confirm the session is active, the session owns the batch/item, and the item is approved and eligible.
2. Allocate a stable `transferId` and durably record the item-to-transfer association (`ItemUploadAllocated`).
3. Create `.part` and atomically persist `.meta` using the existing resumable durability path.
4. Return HTTP 201 only after the resumable resource is durable and the ledger association is durable.

On restart, reconcile an allocated item with its `.meta`:

- Matching item/transfer/session/batch identity and valid metadata: restore it.
- Ledger allocation with no metadata and no payload: safely return the item to an eligible pre-upload state or require a new allocation, with an idempotent rule.
- Payload without a valid matching `.meta` and ledger owner record: quarantine as an orphan; never infer ownership from filename, path, or transfer ID.
- Mismatched IDs, owner key, destination root, size, manifest digest, or approval: quarantine/fail closed and do not expose the upload to the client.

### 8.4 PATCH streaming — performance-critical path

The streaming path must remain unchanged in its essential performance properties:

- One continuous request stream for the primary supported browser path.
- Existing write-then-hash pipeline: only successfully written bytes feed the running SHA-256 hasher.
- Existing checkpoint policy remains **32 MB / 500 ms**.
- Existing `partFile.Sync()` and atomic `.meta` update semantics remain authoritative.
- No coordinator WAL append per buffer, per chunk, per checkpoint, or progress percentage.
- No second copy of the request body and no per-chunk journal serialization.
- No synchronous central-state database lookup per read/write loop iteration. The session and coordinator have already been restored into in-memory indexes.

An HTTP request may perform the normal in-memory session/owner/approval checks once at entry. Streaming progress can update in-memory coordinator state. Durable byte progress remains in the existing resumable checkpoint metadata.

If session activity timestamps require persistence, checkpoint activity at a coarse bounded interval or at lifecycle/request boundaries—not in the byte loop. The durable timestamp must not extend a session beyond the configured expiry policy.

### 8.5 Completion and publication

1. Authenticate the original session token against the restored session verifier.
2. Verify session status/generation/expiry and revocation state.
3. Verify the exact batch/item IDs, owner session key, approval state, item eligibility, and matching transfer ID.
4. Acquire the existing per-upload lifecycle lock and any required session-level revocation gate.
5. Validate the client-supplied expected SHA-256 against the authoritative server-computed digest using the existing resumable finalization path.
6. Reuse the existing durable publication record and atomic same-filesystem rename protocol.
7. Persist the coordinator terminal `Completed` event only after publication is durably established. Make reconciliation idempotent if a crash occurs between publication and the coordinator terminal event.
8. Return HTTP 200 only when the completed publication is authoritative.

If publication exists but the coordinator terminal event is missing, startup may mark the item completed only after validating the publication record and actual target digest against the expected SHA-256 and checking the matching owner/item association. File size alone is never proof of completion.

### 8.6 Cancellation and revocation

**Cancellation** is an item lifecycle transition. Persist the cancel intent/state with an idempotency key, coordinate with the upload lifecycle lock, stop the active request, release the lock, then purge staging and quota reservations under the established cancellation order. Do not let cancellation of one item cascade to unrelated items.

**Session revocation** is a security boundary:

- Serialize revocation against new authenticated operations using a session-level gate/generation.
- Persist and sync a `SessionRevoked` event (or generation increment) before reporting revocation success.
- Mark the in-memory session revoked immediately after durable commit and cancel all active requests belonging to it.
- Future requests must fail authentication (401), including after another process restart.
- A crash after the revocation event is durable but before the success response must leave the session revoked after restart. Security takes precedence over retry convenience.
- Completion must re-check session validity at its authorization boundary; an upload reaching 100% does not grant a right to publish after the owner is revoked.

## 9. Startup restoration order

The HTTP listener must not accept requests until every required stage succeeds.

1. **Acquire single-instance state lock.** Prevent two server processes from concurrently replaying/writing the same identity ledger and staging state.
2. **Open protected application state directory and installation key.** Validate permissions/ACLs and key ID. If key integrity cannot be established, fail closed for session restoration.
3. **Load snapshot and replay WAL.** Validate sequence, integrity, schema versions, sessions, revocation generations, batches, items, approvals, and terminal states.
4. **Restore session manager.** Rebuild token-verifier indexes and stable session-key mappings. Exclude revoked and expired sessions from active lookup.
5. **Restore coordinator.** Reconstruct batch/item objects with the original IDs and owner references. Recreate runtime locks/channels from scratch. Reconcile queued/active/interrupted/verifying/completed/cancelled states; do not mark an interrupted stream as live.
6. **Recover resumable staging.** Scan `.meta`, call the existing startup recovery path, validate `.part` size versus committed offset, truncate only uncommitted trailing bytes as currently specified, quarantine missing/corrupt/inconsistent data, and retain `awaiting_digest` without premature publication.
7. **Cross-validate identity and resumable metadata.** Every resumable Personal upload must match a valid restored session owner, batch, item, and transfer association. An orphan `.meta` is not authority. A coordinator item without a matching upload must follow a deterministic pending/retry/failure rule.
8. **Reconcile publication records.** Validate transfer IDs, expected digest, target binding, and file digest. Resolve crashes around rename idempotently; never trust same-size files.
9. **Restore quota reservations.** Rebuild outstanding reservations from validated staging records before new upload admission.
10. **Run expiry/cancellation cleanup safely.** Use existing per-upload lifecycle locking and preserve zero-expiry semantics. Do not delete a record until ownership and publication/recovery status are understood.
11. **Publish readiness and open listener.** Only now may the HTTP server report ready and accept Tus requests.

If any global ledger corruption prevents reliable owner/revocation reconstruction, do not start an upload listener in a degraded permissive mode. Provide a diagnostic path that does not leak token or filesystem secrets.

## 10. Fail-closed recovery matrix

| Condition | Required behavior |
|---|---|
| Session record missing but `.meta` exists | Do not authenticate or adopt. Quarantine/mark orphaned; require trusted desktop re-pair and explicit reauthorization. |
| Session revoked or expired | Reject Personal endpoints with the existing auth failure semantics (401). Do not complete/publish. |
| Token verifier/key missing or invalid | Fail closed; do not accept old tokens. Require re-pair or an explicit trusted recovery path. |
| Batch/item record missing | Do not infer approval from `.meta`; quarantine/mark orphaned. |
| Approval missing/revoked | Reject create/continue/complete according to the established approval contract. Never auto-approve on restart. |
| `.meta` missing while `.part` exists | Quarantine orphan payload. Never guess offset or owner. |
| `.part` shorter than committed offset | Treat as data loss/corruption; quarantine and require clean restart. Never fabricate bytes. |
| `.part` longer than committed offset | Truncate only to the durable committed offset using existing crash-recovery rules. |
| `.meta` corrupt or unsupported version | Quarantine metadata and associated payload; do not expose an uncertain offset. |
| Identity/metadata IDs or owner keys disagree | Quarantine/fail closed. Do not repair by rewriting owner IDs automatically. |
| `awaiting_digest` with valid owner and valid durable bytes | Preserve `awaiting_digest`; expose it via HEAD; accept completion after fresh authorization and digest validation. |
| Publication record exists but target digest mismatches | Do not mark completed; quarantine/fail according to existing publication-integrity policy. |
| Revocation occurs during upload | Abort/stop active operations, persist revocation, reject later continuation and completion. |
| WAL has incomplete final record | Preserve diagnostics; truncate only the incomplete tail to last valid frame. |
| WAL has corruption before the end or invalid authentication tag | Fail closed; do not skip invalid records and continue replay. |

## 11. Performance requirements — non-negotiable

The durability feature must not become a second write path attached to the transfer stream.

### 11.1 Hot-path invariants

- **No WAL write per byte, buffer, chunk, PATCH checkpoint, or progress event.**
- **No extra `fsync` inside the data-copy loop.** Keep the existing 32 MB / 500 ms durable checkpoint behavior.
- **No extra request-body copy.** Preserve direct streaming and native backpressure.
- **No second streaming hash.** Preserve the existing single-pass SHA-256 pipeline.
- **No per-chunk coordinator persistence.** Coordinator progress is reconstructible from resumable metadata and can remain in memory while the process runs.
- **No per-buffer central-store lookup.** Restore session/coordinator state to in-memory indexes before listening.
- **No full-file scan of all `.part` files on every request.** Startup reconciles metadata and file sizes; expensive hash rebuilds remain lazy and only occur where the existing integrity design requires them.
- **No weakening of Sync, atomic metadata replacement, directory durability, digest validation, or publication safety to achieve a benchmark.**

### 11.2 Acceptable costs

Durable append+sync may add bounded latency to lifecycle operations such as session creation, batch declaration, approval, revocation, cancellation, and terminal completion. Those are control-plane events, not sustained data-plane streaming operations.

A token verifier calculation and in-memory map lookup occur once per HTTP request/authorization boundary, not once per streamed chunk.

Startup replay adds restart time proportional to the ledger tail. Snapshots bound that cost. Restart time is not sustained transfer throughput, but should still be measured.

### 11.3 Acceptance gates

- Preserve the existing uninterrupted Config-1 baseline and production Config-4 measurement methodology.
- Keep the existing target: Config-4 throughput at least **95%** of Config-1 for each of 1 GB, 5 GB, and 20 GB payloads on the defined acceptance environment.
- The previously reported Run #4 loopback ratios were approximately 81% at each size and were below target; this design does **not** claim to close that gap.
- Run comparable before/after benchmarks with identical fixture, filesystem, storage, concurrency, and checkpoint defaults. Report each size separately; do not average away a failing size.
- Profile any regression. The identity/coordinator ledger must not affect the PATCH streaming hot path. If Config-4 drops measurably after this change, treat it as a release-blocking regression and identify why.
- Separately measure startup replay time, lifecycle-operation latency, and completion latency. Do not disguise these as throughput.

## 12. Migration and backward compatibility

### 12.1 Existing resumable metadata

Existing `.meta` versions contain `sessionKey`, batch/item IDs, transfer ID, and transfer state, but may have no corresponding durable session/coordinator ledger record.

There is no safe way to infer that the current desktop user still approves an old session solely because a `.part` or `.meta` exists.

Recommended migration behavior:

- Continue parsing supported metadata versions using existing migration code.
- Mark records with no durable identity/coordinator association as `legacy_unbound`/orphaned in a separate recovery index; do not silently rewrite their owner key.
- Preserve payload bytes for a bounded recovery period rather than deleting them immediately.
- Require the desktop owner to re-pair the device and explicitly approve/rebind a selected legacy batch/item through a trusted local UI operation.
- The trusted rebind must create a new durable ownership/approval association and audit event. It must verify the destination root, relative path, size, transfer metadata, and integrity state. Never accept a remote request to self-claim an orphan.
- If safe rebind cannot be proven, retain quarantine and let the user restart from byte zero.

### 12.2 Session token migration

- Existing in-memory sessions from before the new ledger cannot be restored after the process has already exited because their raw tokens were not persisted and no durable verifier record exists.
- Existing clients may need to pair again after upgrading.
- New sessions must persist their verifier before the token is returned.
- Do not retroactively treat `sessionKey` in `.meta` as an authentication token.

### 12.3 Rollback

The new ledger format must be versioned. A downgrade to an older binary that cannot interpret the ledger must not ignore it and start with empty session/coordinator state while continuing to serve old staging records. Either block downgrade with a clear recovery message or provide an explicitly designed, tested compatibility mode.

## 13. API and code-surface proposal

Names are illustrative. Align them with existing package conventions before implementation.

### Session package

- `OpenDurableSessionStore(config)` — open key material, snapshot, WAL, and replay state.
- `CreateSession(...)` — create verifier and stable identity; durable commit before token return.
- `RestoreSessions(...)` — reconstruct active in-memory session map.
- `ValidateToken(token)` — verify token against in-memory verifier index and status/generation.
- `RevokeSession(sessionID)` — durable revocation before success; cancel active session operations.

### Coordinator package

- `OpenDurableCoordinatorStore(config)` — replay coordinator lifecycle records.
- `RestoreCoordinator(snapshot)` — reconstruct batches/items with original IDs and approval state.
- `PersistBatchCreated`, `PersistApprovalChanged`, `PersistItemTransition`, `PersistUploadAllocated`, `PersistItemCancelled`, `PersistItemCompleted` — durable lifecycle events.
- Coordinator state transitions must be idempotent and sequence-checked.

### Server startup

- Construct durable stores and recover their records before `NewServer`/router readiness is exposed, or add a constructor that returns an error unless restoration completes.
- Avoid a lazy `EnsureResumable()` path that lets handlers run before cross-store reconciliation.
- Expose readiness only after identity, coordinator, resumable staging, publication, and quota reconciliation all complete.

No production API should accept an arbitrary caller-supplied session key, batch owner, or approval state as a substitute for durable restoration.

## 14. Acceptance test plan

Keep the existing tests:

- `TestWp4Crash_Scenarios14_43_RestartInAwaitingDigestCompletesWithoutReupload` — manager-level crash/recovery evidence.
- `TestWp4Scenario43HTTPServerProcessRestart` — real HTTP process boundary and current blocker evidence. Update its expected result only after the production design is implemented; retain the SIGKILL/restart behavior.

Add the following tests as implementation proceeds.

### 14.1 Core Scenario 43 end-to-end

1. Child server A starts with the durable state directory.
2. Create/restore a valid session, approved batch, and item through real APIs.
3. POST and final PATCH the complete deterministic payload through HTTP.
4. Assert 201, then 204; final offset equals size; state is `awaiting_digest`; prefix digest equals the payload SHA-256.
5. SIGKILL child A; preserve staging and application state.
6. Child server B starts against the same directories and completes startup replay before listener readiness.
7. Original bearer token authenticates against the restored verifier.
8. Original batch/item IDs and approval/ownership are restored.
9. HEAD on the original Tus URL returns 200, full offset/length, `awaiting_digest`, and the correct prefix digest.
10. POST the original completion request with the expected digest.
11. Assert HTTP 200, completed state, exact output bytes, exact size and digest.
12. Assert no second PATCH was sent after restart.
13. Repeat completion and verify idempotency.

### 14.2 Session security and revocation

- Valid non-revoked token validates after restart.
- Revoked token remains invalid after restart (401).
- Token rotation invalidates the old generation and preserves only the new authorized verifier.
- Expired session is not restored as active.
- Tampered verifier/MAC fails closed.
- Missing/incorrect installation key does not silently generate a new key and restore old sessions.
- Another valid session cannot HEAD/PATCH/DELETE/complete the upload (preserve existing 404 isolation semantics).
- Revocation racing with PATCH/complete stops or serializes operations and prevents post-revocation publication.

### 14.3 Coordinator integrity

- Batch/item IDs are identical after restart.
- Approval remains approved only when a durable approval record exists.
- Unapproved/revoked items cannot create or complete Tus uploads.
- Cancelled items remain cancelled after restart.
- Completed items reconcile from verified publication records.
- One interrupted item does not cancel later pending items in the batch.
- Missing batch/item ownership record with existing `.meta` is quarantined, not adopted.
- Duplicate/replayed journal events are applied once.

### 14.4 Crash consistency

Inject crashes at:

- Session event written before `Sync()`.
- Session event durable before token response.
- Batch created before approval.
- Approval durable before Tus POST.
- Upload allocation event before `.meta` creation.
- `.meta` durable before HTTP 201.
- Final PATCH `.part` sync before metadata replace.
- Metadata replace before 204 response.
- Publication record before rename.
- Rename before coordinator completed event.
- Revocation event durable before revocation response.
- Snapshot replacement and WAL rotation.

For every boundary, verify deterministic replay, no invented offsets, no unauthorized ownership, and idempotent retry behavior.

### 14.5 Performance regression tests

- Compare Config-1 and Config-4 using the exact existing benchmark harness and fixtures.
- Preserve checkpoint defaults: 32 MB / 500 ms.
- Confirm no durable identity/coordinator journal writes occur during PATCH byte streaming or per checkpoint.
- Confirm no additional request-body copy or second hash pass is introduced.
- Benchmark authorization overhead separately from transfer streaming.
- Measure startup replay with realistic session/batch/item counts and journal sizes.
- Retain existing data-integrity, race, and cross-platform build gates.

## 15. Implementation phases after design approval

1. **Inventory and contract freeze:** re-inspect actual current source; write exact session/token expiry, coordinator transition, and authorization contracts with tests before changing persistence.
2. **Durable identity ledger:** session creation, token verifier, expiry, rotation, and revocation; unit tests for replay and corruption.
3. **Durable coordinator journal:** stable batch/item IDs, owner binding, approval, queue/lifecycle transitions, and idempotent restore.
4. **Cross-store reconciliation:** join ledger items with `.meta`/`.part`/publication records and quota reservations; fail closed on orphan/mismatch.
5. **Listener readiness gate:** prove no HTTP request is accepted before all restoration stages complete.
6. **Scenario 43 HTTP restart acceptance:** complete the SIGKILL A/B test through original HEAD and completion endpoints with zero PATCH retransmission.
7. **Security and performance gates:** revocation race tests, corrupted-state tests, race suite, benchmark comparison, and platform-specific durability checks.
8. **Migration acceptance:** test old metadata, missing ledger, key loss, downgrade, and explicit desktop rebind behavior.

Do not implement Option B in parallel. Revisit only if an independent product requirement justifies upload-scoped delegated credentials.

## 16. Open decisions to resolve during implementation inventory

These are not assumptions to hardcode in the first patch:

- Exact application state directory and how it is shared by all transfer destination roots.
- Current session absolute/idle expiry rules and exact token generation/rotation behavior.
- Exact coordinator states and transitions, including approval semantics and how pending queue order is represented.
- Whether a session token can have multiple active credentials and how cookies/bearer tokens are reconciled.
- How current `Session.Key()` is derived and how to preserve existing `.meta` ownership compatibility.
- Existing publication record format and startup reconciliation ordering.
- Whether current server startup already has a single-instance lock and readiness gate.
- Windows ACL and file replacement implementation details.
- Whether a cross-process crash leaves `.meta` hash state sufficient for immediate finalization or requires a single lazy local re-hash. Do not add repeated hashing to the network path.
- How legacy unbound uploads are presented for explicit desktop reauthorization without exposing them to another session.

## 17. Scenario 43 acceptance definition and current status

Scenario 43 is **PASS** only when a fresh server process can:

- restore the original non-revoked session verifier;
- restore the same coordinator batch/item identity and approval;
- restore the same resumable upload from durable metadata;
- serve authoritative HTTP HEAD at `awaiting_digest`;
- accept the original completion HTTP request;
- publish the exact expected bytes and SHA-256;
- do so without retransmitting any payload bytes;
- reject the same continuation if the session was revoked or expired.

Current classification remains:

**Scenario 43 — PARTIAL**

This document is a design proposal only. It does not change production code, prove implementation, or authorize upgrading the scenario status.

---

## Performance commitment

Flitnix's primary product requirement remains **maximum practical transfer speed without sacrificing reliability**, with a target of at least **95% of the uninterrupted baseline throughput** for the primary continuous-streaming path on the defined local gigabit Wi-Fi acceptance environment.

Durable identity and coordinator recovery must remain a control-plane concern. It must not insert persistence barriers, database round trips, copies, or extra hashing into the streaming hot path. If the implementation cannot preserve that separation and maintain the throughput target, it is not acceptable as complete.
