# ADR-001: Real-Time Event Delivery (SSE + Wails EventsEmit)

## Status

Accepted (Phase 3)

## Context

Flitnix previously relied on multiple fixed-interval polling loops:
- Desktop UI (`App.svelte`): 17 Wails IPC queries every `800 ms`.
- Mobile batch approval/progress (`app.js`): HTTP `GET /api/batches/{id}` every `1200 ms`.
- Mobile download progress (`app.js`): HTTP `GET /api/download-progress/{id}` every `500 ms`.
- Mobile Quick Text (`app.js`): HTTP `GET /api/text` every `2500 ms`.
- Guest Drop Box (`guest.js`): HTTP `GET /guest-api/status` every `1500 ms` and `/guest-api/uploads/{id}` every `1000 ms`.

These loops generated continuous background traffic and introduced up to 1.2–2.5 seconds of latency when approving transfers or exchanging Quick Text messages.

## Options Evaluated

1. **WebSockets (`gorilla/websocket`)**
   - Pros: Full-duplex binary/text framing, wide browser support.
   - Cons: Browser `new WebSocket(url)` cannot set `Authorization: Bearer <token>` headers, forcing either a separate single-use ticket exchange (`POST /api/events/ticket`) or `Sec-WebSocket-Protocol` workarounds; connection hijacking complicates Go `http.Server.Shutdown` draining and rolling `http.ResponseController` deadlines; state-changing operations already use authenticated REST endpoints.
2. **Fetch-Based Server-Sent Events (`GET /api/events`, `GET /guest-api/events`) + Wails `EventsEmit`**
   - Pros:
     - Uses standard `fetch()` with `ReadableStream` (`response.body.getReader()`) on mobile browsers, passing `Authorization: Bearer <token>` natively in HTTP headers (no credentials in URL query strings, no extra ticket endpoint).
     - Guest Drop Box uses the existing `HttpOnly`, `SameSite=Lax` cookie (`flitnix_guest_token`) on same-origin `fetch('/guest-api/events')`, with immediate HTTP 401/403 status inspection.
     - Pure Go `net/http` (`http.Flusher` + `http.ResponseController`) integrates cleanly with `requireSession`, `securityMiddleware`, `tlsPeekListener`, and immediate cancellation on server shutdown.
     - Desktop Wails v2 uses native `runtime.EventsEmit` / `EventsOn` directly over IPC without extra HTTP/WebSocket connections.
     - Falls back seamlessly to existing HTTP polling if streaming `ReadableStream` is unavailable or interrupted.
   - Cons: Unidirectional server-to-client push (acceptable because all commands and state mutations remain ordinary authenticated HTTP APIs).

## Decision

Adopt **Authenticated Fetch-Based Server-Sent Events (`GET /api/events` and `GET /guest-api/events`)** for mobile/HTTP clients combined with **Wails `runtime.EventsEmit` / `EventsOn`** for the desktop UI, backed by a bounded, non-blocking `internal/events.Hub`.

## Security & Reliability Invariants

- Session tokens and guest tokens are never placed in URL query strings.
- SSE endpoints enforce `Host`, `Origin`, and `Sec-Fetch-Site` validation, existing session/guest authentication, device revocation checks, and bounded connection lifetimes (`15 min` max stream TTL, or session/room expiry).
- Event payloads never carry session tokens, private keys, or raw filesystem paths.
- Per-subscriber queues are strictly bounded (`64` events); progress events are throttled (`200 ms`) and shed under backpressure before critical state transitions, with `event: resync` fallback and authoritative HTTP reconciliation on reconnect or `visibilitychange`.
