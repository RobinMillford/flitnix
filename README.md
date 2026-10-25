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

## Network metadata support

Flitnix discovers usable local IPv4 addresses generically on every platform.
Connection metadata is platform-specific:

- Linux uses NetworkManager's `nmcli` when available to identify the active
  Wi-Fi SSID. Systems using another network manager retain interface and
  address discovery and show an explicit unavailable-name fallback.
- Windows uses the Native WLAN API for connected Wi-Fi adapters and keeps the
  saved profile name separate from the SSID.
- macOS uses CoreWLAN for the active interface's SSID. The macOS provider
  requires a native macOS build; SSIDs unavailable because of disconnection or
  platform privacy permissions use the same safe fallback.

No provider uses a gateway address, requires administrator/root privileges, or
infers a phone model from a network name. Native Windows and macOS runtime
verification must be performed on their respective operating systems.

A build without the tag fails with `Package webkit2gtk-4.0 not found`.
