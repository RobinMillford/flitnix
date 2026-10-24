# Flitnix

Offline-first, high-speed local file transfer between a Fedora (Linux) desktop and a phone browser,
built with Go + Wails v2 + Svelte + TypeScript. No cloud, no relay servers, no accounts.

## Status

Stage 1 (environment & skeleton) — complete.
Stage 2 (standalone phone-page server) — complete. No transfer functionality yet.

The phone webpage server lives in `internal/` (standard library only, no Wails imports) and is
covered by `go test ./...`.

## Loopback smoke test

Run the phone-page server without the desktop UI:

```bash
go run ./cmd/flitnix-serve
# flitnix-serve: serving the phone page at http://127.0.0.1:43821 (Ctrl+C to stop)
```

Then verify with:

```bash
curl -i http://127.0.0.1:43821/
curl -i http://127.0.0.1:43821/healthz
```

The listen address is configurable via `-addr` (e.g. `-addr 127.0.0.1:44000`); if the port is
occupied, the server exits with a clear error instead of silently choosing another port. For now it
binds loopback only — LAN binding (with QR pairing) comes in a later stage.

## Live Development

```bash
wails dev
```

## Building

On Fedora 44 (which ships only the WebKit2GTK 4.1 branch), builds must use the `webkit2_41` tag:

```bash
wails build -tags webkit2_41
wails dev -tags webkit2_41
```

A build without the tag fails with `Package webkit2gtk-4.0 not found`.
