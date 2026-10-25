# Batch transfer contract

This document defines the Phase 0 contract for future multi-file and folder
transfers. Phase 0 adds domain types, pure metadata validation, and tests only.
It does not add queue coordination, HTTP endpoints, folder traversal, UI
changes, drag-and-drop, ZIP downloads, or runtime transfer behavior.

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
