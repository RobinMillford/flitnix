# Flitnix

Offline-first, high-speed local file transfer between a Fedora (Linux) desktop and a phone browser,
built with Go + Wails v2 + Svelte + TypeScript. No cloud, no relay servers, no accounts.

## Status

Stage 1 (environment & skeleton) — complete. No transfer functionality yet.

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
