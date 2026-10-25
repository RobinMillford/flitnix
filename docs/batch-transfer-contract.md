# Batch transfer contract

This document defines the Phase 0 and Phase 1 contract for future multi-file
and folder transfers. Phase 1 adds a tested in-memory coordinator only. It
does not add HTTP endpoints, folder traversal, UI changes, drag-and-drop, ZIP
downloads, or live batch file transfers.

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

The conceptual receive destination is:

```text
~/Downloads/Flitnix/<collision-safe-root>/<relativePath>
```

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
