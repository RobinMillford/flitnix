# Batch transfer contract

This document defines the batch contract through Phase 5. Phase 3 added
phone-to-desktop folder selection and sequential folder uploads. Phase 4 adds
desktop-private file/folder source selection, native folder enumeration, and
Wails file-drop registration. It does not add desktop-to-phone batch
downloads, and Phase 5 adds browser-native desktop-to-phone downloads. Queue
persistence and resumable transfers are still not included.

## Domain model

`TransferBatch` is an in-memory, session-scoped internal record with an opaque
cryptographically random ID, direction, kind, optional folder root, ordered
items, declared and transferred byte totals, approval state, lifecycle state,
timestamps, and an error. `TransferItem` has its own opaque ID, parent batch
ID, sequence, relative path or display name, declared size, transferred byte
count, state, checksum, error, and (for desktop-originated sharing only) a
private source path.

`BatchView` and `ItemView` are separate phone-safe projections. They do not
contain session ownership keys, bearer tokens, or absolute desktop paths.
The server calculates aggregate declared bytes from validated item sizes.
No batch state is persisted in this phase.

Desktop selections use a separate `SharedSource` record. A file source has
one regular-file item and retains the existing direct-download registration.
A folder source has a private absolute root and trusted items with normalized
paths relative to that root. `SourceView` contains only an opaque ID,
kind, display/root name, item count, total bytes, and creation time. Folder
The phone-facing `/api/sources` projection includes both source records and
legacy shared files without exposing their paths.

## Desktop-to-phone downloads

`POST /api/download-grants` accepts an ordered list of source IDs and
pre-validates the complete archive manifest before returning. A single regular
file remains a direct streamed response; collections and folders are streamed
as an uncompressed `zip.Store` archive. Folder roots, nested paths, and empty
directory entries are retained. Duplicate archive names receive deterministic
numeric suffixes.

The response contains a short-lived opaque download URL and a progress ID.
The URL contains no bearer token. A separate random, `HttpOnly`,
`SameSite=Strict` cookie is scoped to that exact URL and is consumed
atomically, so grants are one-use, expire after approximately one minute, and
cannot be replayed. Grant and progress state are in-memory and session-bound
to the authenticated creator; they are removed with process state and are not
resumable.

Archive generation validates trusted source files again immediately before
opening them, streams one file at a time, and does not buffer the archive or
write a temporary complete ZIP. Progress reports server-side bytes processed;
it cannot prove that a mobile browser finished saving a native download.
Disconnects and source changes fail the operation honestly. The browser
cannot create arbitrary folders in Downloads, so ZIP extraction remains a
user/device action.

## Coordinator and queue semantics

The Phase 1 coordinator is independent of Wails, HTTP, the frontend, and the
operating system. It owns in-memory batches for active sessions and verifies
the internal session ownership key on every operation. Ownership keys and
tokens are not included in phone-safe views.

Each session has one FIFO queue across all of its batches:

1. Batches are considered in registration order.
2. Items are considered in accepted manifest order.
3. At most one item payload may be active for a session.
4. Different sessions can transfer independently.
5. A failed item pauses the session; later work cannot bypass it.
6. Retrying the failed item resets its partial progress and places it before
   subsequent items.

`ClaimNext` returns an opaque lease ID. Progress, completion, and failure must
present the active lease. A retry creates a new lease generation, so an old
stream cannot complete or update the retried item. The coordinator performs
no file or network I/O while holding its mutex.

Incoming phone-to-desktop batches transition as follows:

```text
pending approval -> approved/queued -> transferring -> completed
       |                 |                |
       v                 v                v
    rejected          canceled          failed -> queued (retry)
```

Desktop-to-phone batches are created approved and queued. A batch is completed
only after every item completes. Completed items remain completed when another
item fails. Rejection and cancellation are distinct batch states; canceling a
batch marks unfinished items canceled, while rejecting a pending batch marks
unfinished items rejected.

An active item can register a `context.CancelFunc`. Canceling an item, batch,
session, or coordinator detaches that callback under the lock and invokes it
after unlocking. Session close is idempotent, prevents future claims and
cancels unfinished work. Application shutdown applies the same cancellation
policy to every session.

An item's bytes are published at most once, and publication and cancellation
contend for a single decision taken under the upload record's lock before any
publishing work. Whichever side takes it first wins, and the other is refused
instead of being reported as a second, contradictory outcome. A withdrawal that
wins makes the stream discard its temporary data, so no final file can appear;
a withdrawal that arrives after publication has been claimed is answered 409,
and the item keeps its completed state, its completed record and its file.

Aggregate transferred bytes are maintained incrementally. An update is an
absolute item byte count, must be non-negative and no greater than the
declared size, and may not move backward. Repeating a count is idempotent.
Partial bytes from a failed item remain visible as bytes transferred during
that attempt; retrying or canceling the item removes those partial bytes
before a new attempt can start. Snapshots are copied before returning so
callers cannot mutate coordinator state.

## Paths and collisions

For a folder batch, `rootName` is the selected folder's basename and every
`relativePath` is relative to the inside of that folder. The root name is not
repeated in an item path:

```text
Photos/readme.txt       -> rootName: Photos, relativePath: readme.txt
Photos/2026/image.jpg   -> rootName: Photos, relativePath: 2026/image.jpg
```

The receive destination for an accepted folder batch is:

```text
~/Downloads/Flitnix/<collision-safe-root>/<relativePath>
```

The server atomically reserves one collision-safe root directory for the
batch before its item records are created. If `Photos` exists, the batch uses
the next established collision variant such as `Photos (1)`; all items in
that batch use the same resolved root. `Mkdir` performs the reservation, so
concurrent batches cannot accidentally share a root. Existing and newly
created path components are checked for symlinks before a part file is
opened. Individual files are then published with the existing atomic,
no-overwrite hard-link policy.

Later phases must use private staging, destination confinement, symlink
policy, and no-overwrite publication. A flat file collection retains
selection order and uses each file's basename. Duplicate flat basenames are
valid and are resolved at final publication by the existing collision-safe
receiving policy. Folder manifests reject duplicate normalized relative paths.

`SanitizeRelativePath` is pure syntactic validation. It canonicalizes
backslashes to forward slashes and rejects absolute paths, drive and UNC
syntax, traversal, empty components, control characters, invalid UTF-8,
overlong/deep paths, colons, trailing spaces/dots, and Windows device names.
This does not by itself prove that a later filesystem operation is safe.

Desktop folder enumeration does not follow directory symlinks and skips
symlinked files and special files. It records empty directories internally,
but does not claim they are downloadable until archive generation supports
them. Enumeration is bounded to 100,000 files and 100,000 empty directories;
each normalized source path is bounded to 4,096 bytes. A failed enumeration
does not register a partial source.

Initial defaults are 5,000 items, 4,096 bytes per canonical relative path,
230 bytes per component, 64 components per path, and an `int64` total-size
ceiling. Limits are configurable through `ManifestLimits`.

## Planned transfer behavior

Future queue phases must allow one active file payload per session across all
batches by default. Items transfer in accepted manifest order. An incoming
batch requires explicit approval before any file content is accepted. A
failure pauses the queue while preserving completed items. Session cancellation
and application shutdown cancel active work and clean staging data.

No resumable partial-item protocol is included yet.

## Browser and archive limitations

Phone-to-desktop folder selection will use `webkitdirectory` where available,
with a flat multi-file fallback. Standard browser folder selection does not
reliably expose empty directories.

Desktop-to-phone folders will use a streamed, uncompressed ZIP as the
universal default. Ordinary mobile browsers cannot universally create
arbitrary directory trees in Downloads, and File System Access API support and
secure-context requirements vary. ZIP extraction remains a user/device action.

## Desktop source selection

The native Wails multi-file picker remains the direct single-file workflow.
The directory picker and native file-drop callback feed the same source
registry. Re-adding the same file source is idempotent; separate folder
selections always receive distinct opaque identities, even when their display
names match. Removing a source only unlists it and never modifies the
original files. Wails drop listeners are registered once on component mount
and removed on unmount.

## Approval authority and failure handling (stabilization)

- A phone-to-desktop batch is approved only as a batch. Its per-item upload
  records are marked batch-linked: they never appear as standalone approval
  requests or recent uploads, and the legacy single-upload approve, reject and
  cancel calls refuse them. All of a batch's records are decided together and
  atomically (`Service.DecideBatchItems`); the coordinator then releases the
  batch. Approving an already approved batch is an idempotent success;
  approving a rejected or canceled batch is a conflict.
- The pending-approval projection lists only batches whose approval is pending
  and whose state is pending. Approved, completed, rejected and canceled
  batches leave it.
- A content stream that fails or is interrupted ends its batch as failed
  (the failed item keeps its error, unstarted items are canceled, no partial
  bytes are counted) without pausing the session queue; the phone recovers by
  declaring a new batch. A cancellation is not a failure: an item whose upload
  record was withdrawn ends canceled, its partial bytes are discarded, and its
  siblings keep going. A desktop cancel stays canceled.
- Sessions are pinned for the life of an upload or download stream, and the
  idle window restarts when the stream ends. Idle expiry still applies to a
  session with no live stream and releases its pending and approved records.
- Download filenames come from server-side source metadata: one file keeps its
  name, one folder becomes `<folder>.zip`, any other selection is
  `Flitnix-download.zip`. `Content-Disposition` carries an ASCII fallback plus
  an RFC 5987 `filename*`. A grant is redeemed by GET only, exactly once.
- If an archive stream fails after headers are sent, the connection is aborted
  (no central directory is written, no JSON is appended) so the browser
  reports a failed download instead of saving a truncated ZIP.
  Browser downloads use a longer write-stall window than uploads because a
  phone browser can legitimately stop reading while it shows a save dialog.
