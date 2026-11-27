/* Flitnix phone page — plain JavaScript, no external scripts.
 *
 * Session token handling: the token arrives only in the URL fragment
 * (#43-char-base64url). It is copied into a closure variable and the
 * fragment is scrubbed from the address bar immediately. It is never
 * stored, never logged, and never placed in a query string; every API
 * call carries it as a Bearer header.
 */
(function () {
  "use strict";

  var TOKEN_RE = /^[A-Za-z0-9_-]{43}$/;
  var POLL_MS = 1200;           // approval polling cadence (fallback when SSE unavailable)
  var POLL_BACKOFF_MAX_MS = 5000; // ceiling for retry delay after a failed poll
  var SSE_BACKUP_POLL_MS = 10000; // slow safety-net poll when SSE is connected
  var PROGRESS_POLL_MS = 500;   // download progress cadence while it is live
  var SSE_PROGRESS_BACKUP_MS = 5000; // slow safety-net poll for downloads when SSE is connected
  var MAX_PROGRESS_FAILURES = 20; // consecutive failed progress polls before giving up
  var PAINT_MIN_MS = 200;       // upload progress repaint throttle
  var CANCEL_RETRIES = 3;       // DELETE attempts before showing the real state
  var MAX_AUTO_RETRIES = 6;     // bounded automatic reconnect attempts on transient drops
  var RETRY_BASE_MS = 1000;     // 1s, 2s, 4s, 8s, 15s jittered exponential backoff
  var RETRY_MAX_MS = 15000;
  var MAX_AUTO_RETRY_WAIT_MS = 5 * 60 * 1000; // matches Go: MaxAutoRetryWait
  var FIREFOX_LARGE_UPLOAD_BYTES = 256 * 1024 * 1024;

  // ------------------------------------------------------------------
  // Token: read once, then scrub the fragment.
  // ------------------------------------------------------------------
  var token = null;
  var requestedDeviceId = null;
  var frag = window.location.hash || "";
  if (frag.length > 1) {
    var cand = frag.slice(1);
    try { cand = decodeURIComponent(cand); } catch (err) { /* keep raw */ }
    if (TOKEN_RE.test(cand)) {
      token = cand;
    } else if (cand.indexOf("device=") === 0) {
      requestedDeviceId = cand.slice(7).split("&")[0] || null;
    }
  }

  function diagnoseUpload(kind, job) {
    if (!window.console || typeof window.console.info !== "function") return;
    window.console.info("flitnix upload", {
      kind: kind, phase: job.phase, size: job.size,
      computable: job.size > 0
    });
  }

  function firefoxLargeUploadWarning(bytes) {
    return /Firefox/i.test(navigator.userAgent || "") &&
      bytes >= FIREFOX_LARGE_UPLOAD_BYTES;
  }
  if (token && frag) {
    // One-time use: the link disappears from the address bar (and from any
    // "reopen tab" paste target) the moment the page takes the token.
    window.history.replaceState(null, "",
      window.location.pathname + window.location.search);
  }

  // Handshake ping to verify phone-to-desktop reachability
  fetch("/ping").catch(function () {});

  // ------------------------------------------------------------------
  // Handles
  // ------------------------------------------------------------------
  var statusEl = document.getElementById("status");
  var statusText = statusEl ? statusEl.querySelector(".status__text") : null;
  var flowEl = document.getElementById("flow");
  var rescanCard = document.getElementById("rescan");
  var rescanText = document.getElementById("rescan-text");
  var connectCodeForm = document.getElementById("connect-code-form");
  var connectCodeInput = document.getElementById("connect-code-input");
  var connectCodeBtn = document.getElementById("connect-code-btn");
  var connectCodeError = document.getElementById("connect-code-error");
  var reconnectPanel = document.getElementById("reconnect-panel");
  var reconnectLabel = document.getElementById("reconnect-label");
  var reconnectBtn = document.getElementById("reconnect-btn");
  var reconnectForgetBtn = document.getElementById("reconnect-forget-btn");
  var reconnectError = document.getElementById("reconnect-error");
  var trustPanel = document.getElementById("trust-panel");
  var trustBadge = document.getElementById("trust-badge");
  var trustDesc = document.getElementById("trust-desc");
  var pairControls = document.getElementById("pair-controls");
  var pairDeviceNameInput = document.getElementById("pair-device-name");
  var pairStartBtn = document.getElementById("pair-start-btn");
  var pairSasBox = document.getElementById("pair-sas-box");
  var pairSasCode = document.getElementById("pair-sas-code");
  var pairStatusText = document.getElementById("pair-status-text");
  var pairedInfoBox = document.getElementById("paired-info-box");
  var pairedMetaText = document.getElementById("paired-meta-text");
  var copyReconnectBtn = document.getElementById("copy-reconnect-btn");
  var unpairLocalBtn = document.getElementById("unpair-local-btn");
  var pairError = document.getElementById("pair-error");
  var pickBtn = document.getElementById("pick-btn");
  var pickFolderBtn = document.getElementById("pick-folder-btn");
  var fileInput = document.getElementById("file-input");
  var folderInput = document.getElementById("folder-input");
  var queueEl = document.getElementById("queue");
  var filesEl = document.getElementById("files");
  var filesEmptyEl = document.getElementById("files-empty");
  var folderNavEl = document.getElementById("folder-nav");
  var refreshBtn = document.getElementById("refresh-btn");
  var downloadSelectedBtn = document.getElementById("download-selected-btn");
  var filesRefreshGeneration = 0;
  var browseState = {
    source: null,
    prefix: "",
    items: [],
    totalEntries: 0,
    hasMore: false,
    nextCursor: null,
    selected: {},
    itemNames: {}
  };

  var textForm = document.getElementById("text-form");
  var textInput = document.getElementById("text-input");
  var textSendBtn = document.getElementById("text-send-btn");
  var textMessagesEl = document.getElementById("text-messages");
  var textCharCount = document.getElementById("text-char-count");
  var textMessagesCache = [];
  var textPollTimer = null;

  var appState = { ended: false };
  var queue = [];      // upload jobs, FIFO
  var pumpBusy = false; // one upload in flight at a time
  var activeBatch = null;

  // ------------------------------------------------------------------
  // Screen Wake Lock & Background Awareness
  // ------------------------------------------------------------------
  var awarenessBanner = document.getElementById("awareness-banner");
  var awarenessBannerText = document.getElementById("awareness-banner-text");
  var awarenessBannerClose = document.getElementById("awareness-banner-close");
  var wakeLockBadge = document.getElementById("wake-lock-badge");
  var wakeLockContainer = document.getElementById("wake-lock-container");
  var bannerTimer = null;

  var isWakeLockSupported = !!(window.isSecureContext && navigator.wakeLock && typeof navigator.wakeLock.request === "function");
  var userWakeLockRequested = false;
  var wakeLockSentinel = null;
  var wakeLockAcquiring = false;
  var wasBackgroundedDuringTransfer = false;
  var activeDownloads = {};
  var activeDownloadPolls = {};

  function showAwarenessBanner(msg) {
    if (!awarenessBanner || !awarenessBannerText) return;
    if (bannerTimer) { clearTimeout(bannerTimer); bannerTimer = null; }
    awarenessBannerText.textContent = msg;
    awarenessBanner.hidden = false;
    bannerTimer = setTimeout(function () {
      if (awarenessBanner) awarenessBanner.hidden = true;
      bannerTimer = null;
    }, 7000);
  }

  function hideAwarenessBanner() {
    if (bannerTimer) { clearTimeout(bannerTimer); bannerTimer = null; }
    if (awarenessBanner) awarenessBanner.hidden = true;
  }

  if (awarenessBannerClose) {
    awarenessBannerClose.addEventListener("click", hideAwarenessBanner);
  }

  function hasActiveTransfers() {
    if (appState.ended) return false;
    if (pumpBusy) return true;
    if (activeBatch && activeBatch.jobs && activeBatch.jobs.some(function (j) {
      return !j.removed && (j.phase === "declaring" || j.phase === "waiting" || j.phase === "sending" || j.phase === "canceling");
    })) return true;
    if (queue && queue.some(function (j) {
      return !j.removed && (j.phase === "queued" || j.phase === "declaring" || j.phase === "waiting" || j.phase === "sending" || j.phase === "canceling");
    })) return true;
    for (var k in activeDownloads) {
      if (activeDownloads.hasOwnProperty(k) && activeDownloads[k]) return true;
    }
    return false;
  }

  function updateAwarenessUI() {
    if (!wakeLockBadge || !wakeLockContainer) return;

    if (!isWakeLockSupported) {
      wakeLockBadge.textContent = "Best-effort";
      wakeLockBadge.className = "badge badge--waiting";
      wakeLockContainer.innerHTML = '<p class="wake-lock-unsupported"><span aria-hidden="true">🔒</span> Screen wake lock requires HTTPS or localhost and is unavailable on this local HTTP connection. Keep this tab open and visible during transfers.</p>';
      return;
    }

    var cb = document.getElementById("wake-lock-cb");
    if (!cb) {
      wakeLockContainer.innerHTML = [
        '<label class="wake-lock-label" for="wake-lock-cb">',
        '  <input type="checkbox" id="wake-lock-cb">',
        '  <span>Keep screen awake during transfer</span>',
        '</label>',
        '<p class="wake-lock-note">Prevents display timeout while transfers are active. Uses more battery. Transfers may still pause if the browser is backgrounded.</p>'
      ].join("");
      cb = document.getElementById("wake-lock-cb");
      if (cb) {
        cb.checked = userWakeLockRequested;
        cb.addEventListener("change", function () {
          userWakeLockRequested = !!cb.checked;
          syncWakeLock();
        });
      }
    } else {
      cb.checked = userWakeLockRequested;
    }

    var active = wakeLockSentinel !== null && !wakeLockSentinel.released;
    var transfersActive = hasActiveTransfers();

    if (active) {
      wakeLockBadge.textContent = "Screen wake active";
      wakeLockBadge.className = "badge badge--approved";
    } else if (userWakeLockRequested) {
      if (wakeLockAcquiring) {
        wakeLockBadge.textContent = "Acquiring wake lock…";
        wakeLockBadge.className = "badge badge--uploading";
      } else {
        wakeLockBadge.textContent = "Screen wake standby";
        wakeLockBadge.className = "badge badge--waiting";
      }
    } else {
      if (transfersActive) {
        wakeLockBadge.textContent = "Active transfer";
        wakeLockBadge.className = "badge badge--uploading";
      } else {
        wakeLockBadge.textContent = "Idle";
        wakeLockBadge.className = "badge badge--waiting";
      }
    }
  }

  function acquireWakeLock() {
    if (!isWakeLockSupported || !userWakeLockRequested) return;
    if (wakeLockSentinel !== null || wakeLockAcquiring) return;
    if (document.visibilityState !== "visible") return;
    if (!hasActiveTransfers()) return;

    wakeLockAcquiring = true;
    updateAwarenessUI();

    navigator.wakeLock.request("screen").then(function (sentinel) {
      wakeLockAcquiring = false;
      wakeLockSentinel = sentinel;
      sentinel.addEventListener("release", function () {
        wakeLockSentinel = null;
        updateAwarenessUI();
      });
      updateAwarenessUI();
    }).catch(function (err) {
      wakeLockAcquiring = false;
      wakeLockSentinel = null;
      if (window.console && typeof window.console.warn === "function") {
        window.console.warn("Screen wake lock request failed:", err);
      }
      updateAwarenessUI();
    });
  }

  function releaseWakeLock() {
    if (wakeLockSentinel !== null) {
      var s = wakeLockSentinel;
      wakeLockSentinel = null;
      try { s.release(); } catch (err) {}
    }
    updateAwarenessUI();
  }

  function syncWakeLock() {
    if (!isWakeLockSupported) {
      updateAwarenessUI();
      return;
    }
    if (userWakeLockRequested && hasActiveTransfers() && document.visibilityState === "visible") {
      acquireWakeLock();
    } else {
      if (wakeLockSentinel !== null) {
        releaseWakeLock();
      } else {
        updateAwarenessUI();
      }
    }
  }

  function reconcileActiveTransfers() {
    if (activeBatch && !appState.ended) {
      if (activeBatch.timer) {
        clearTimeout(activeBatch.timer);
        activeBatch.timer = null;
      }
      pollBatch();
    }
    for (var pid in activeDownloadPolls) {
      if (activeDownloadPolls.hasOwnProperty(pid) && typeof activeDownloadPolls[pid] === "function") {
        try { activeDownloadPolls[pid](); } catch (e) {}
      }
    }
    if (!appState.ended) {
      refreshFiles();
    }
  }

  function onVisibilityChange() {
    if (document.visibilityState === "hidden") {
      if (hasActiveTransfers()) {
        wasBackgroundedDuringTransfer = true;
        if (window.console && typeof window.console.info === "function") {
          window.console.info("Flitnix: browser page moved to background during transfer");
        }
      }
    } else if (document.visibilityState === "visible") {
      if (wasBackgroundedDuringTransfer) {
        wasBackgroundedDuringTransfer = false;
        showAwarenessBanner("Flitnix was moved to the background. Transfer status has been refreshed.");
        reconcileActiveTransfers();
      }
      if (!appState.ended && token && !sseState.active && !sseState.connecting) {
        connectEvents();
      }
      syncWakeLock();
    }
    updateAwarenessUI();
  }

  document.addEventListener("visibilitychange", onVisibilityChange);

  // ------------------------------------------------------------------
  // Small helpers
  // ------------------------------------------------------------------
  function el(tag, className, text) {
    var n = document.createElement(tag);
    if (className) n.className = className;
    if (text != null) n.textContent = text;
    return n;
  }

  function copyToClipboard(text, onSuccess, onFailure) {
    if (navigator.clipboard && navigator.clipboard.writeText) {
      navigator.clipboard.writeText(text).then(function () {
        if (onSuccess) onSuccess();
      }).catch(function () {
        fallbackCopy(text, onSuccess, onFailure);
      });
      return;
    }
    fallbackCopy(text, onSuccess, onFailure);
  }

  function fallbackCopy(text, onSuccess, onFailure) {
    try {
      var ta = document.createElement("textarea");
      ta.value = text;
      ta.style.position = "fixed";
      ta.style.left = "-9999px";
      ta.style.top = "-9999px";
      ta.setAttribute("readonly", "");
      document.body.appendChild(ta);
      ta.focus();
      ta.select();
      var ok = document.execCommand("copy");
      document.body.removeChild(ta);
      if (ok) {
        if (onSuccess) onSuccess();
      } else {
        if (onFailure) onFailure();
      }
    } catch (e) {
      if (onFailure) onFailure(e);
    }
  }

  function fmtBytes(n) {
    if (typeof n !== "number" || !isFinite(n) || n < 0) return "";
    var units = ["B", "KB", "MB", "GB", "TB"];
    var v = n, u = 0;
    while (v >= 1000 && u < units.length - 1) { v /= 1000; u += 1; }
    var digits = u === 0 || v >= 100 ? 0 : v >= 10 ? 1 : 2;
    return v.toFixed(digits) + " " + units[u];
  }

  function fmtEta(seconds) {
    if (typeof seconds !== "number" || !isFinite(seconds) || seconds <= 0) return "< 1s";
    if (seconds < 60) return seconds + "s";
    var m = Math.floor(seconds / 60);
    var s = seconds % 60;
    return m + "m " + (s < 10 ? "0" : "") + s + "s";
  }

  function setStatus(stateName, text) {
    if (statusEl) statusEl.dataset.state = stateName;
    if (statusText) statusText.textContent = text;
  }

  // statusLabel: the one-line status word. The default names a genuine
  // authority-side ending; callers whose cause is not an authority ending
  // (a reload without a pairing token, an unrecognized token) must pass
  // their own so the status never claims a session the server never
  // declared dead.
  function endSession(title, why, statusLabel) {
    if (appState.ended) return;
    appState.ended = true;
    teardownJobs();
    if (statusEl) statusEl.dataset.state = "ended";
    if (statusText) statusText.textContent = statusLabel || "Session ended";
    if (flowEl) flowEl.hidden = true;
    if (rescanCard) {
      var h = rescanCard.querySelector("h2");
      if (h && title) h.textContent = title;
      if (rescanText && why) rescanText.textContent = why;
      rescanCard.hidden = false;
    }
  }

  // A 401 from a token the desktop once issued means the session really ended
  // (idle expiry or revoked on the desktop). Anything else — a dropped network,
  // a 5xx, a timeout — is NOT a session ending and never reaches this path.
  function endUnauthorized(code) {
    if (code === "session_expired") {
      endSession("Session expired",
        "This session expired after sitting idle. Scan the QR code shown on the Flitnix desktop to start a new one.");
      return;
    }
    if (code === "session_revoked") {
      endSession("Session ended",
        "The desktop ended this transfer session. Scan the QR code shown on the Flitnix desktop again to start a fresh one.");
      return;
    }
    // The desktop rejected the token outright: this page is presenting a
    // token it never issued (stale reload, pair-of-sessions confusion). It
    // did NOT end any session, so the wording must not claim it did.
    endSession("Transfer stopped",
      "The desktop did not recognize this session. Open this page again from the QR code shown on the Flitnix desktop.",
      "Pairing required");
  }

  // The desktop is unreachable right now. This is transient: the established
  // session is kept, transfers stay as they are, and polling retries.
  //
  // Each polling loop reports for itself. An upload batch poll (1.2 s, backing
  // off to 5 s) and a download progress poll (0.5 s) run at the same time, so
  // one shared flag let a single successful download poll clear the banner the
  // failing batch poll had just raised — and the reverse. The banner stays up
  // while any loop reports the desktop unreachable, and clears when the last
  // of them gets through again.
  var reconnecting = false;
  var unreachable = {};
  function setReconnecting(on, who) {
    if (appState.ended) return;
    if (on) unreachable[who] = true;
    else delete unreachable[who];
    var lost = Object.keys(unreachable).length > 0;
    if (lost === reconnecting) return;
    reconnecting = lost;
    if (lost) setStatus("reconnecting", "Lost contact with the desktop — retrying…");
    else setStatus("ready", "Connected — pick files to send or download the shared ones");
  }

  // Delay before retry number n (1-based): grows from POLL_MS, bounded.
  function backoffMs(failures) {
    return Math.min(POLL_BACKOFF_MAX_MS, POLL_MS * Math.pow(1.6, Math.max(0, failures - 1)));
  }

  // Wake any waiting poll or dormant SSE stream as soon as the network returns.
  var wakeups = [];
  window.addEventListener("online", function () {
    var fns = wakeups; wakeups = [];
    fns.forEach(function (fn) { fn(); });
    if (!appState.ended && token && !sseState.active && !sseState.connecting) {
      if (sseState.reconnectTimer) {
        clearTimeout(sseState.reconnectTimer);
        sseState.reconnectTimer = null;
      }
      connectEvents();
    }
  });
  function waitThen(holder, ms, fn) {
    var done = false;
    function run() {
      if (done) return;
      done = true;
      clearTimeout(holder.timer);
      holder.timer = null;
      var i = wakeups.indexOf(run);
      if (i >= 0) wakeups.splice(i, 1);
      fn();
    }
    holder.timer = setTimeout(run, ms);
    wakeups.push(run);
  }

  // ------------------------------------------------------------------
  // API plumbing (Bearer only — the token never rides in a URL)
  // ------------------------------------------------------------------
  function readBody(res) {
    return res.text().then(function (text) {
      if (!text) return null;
      try { return JSON.parse(text); } catch (err) { return null; }
    });
  }

  function api(path, opts) {
    if (token == null) {
      var denied = new Error("no session token");
      denied.silent = true;
      return Promise.reject(denied);
    }
    var o = opts || {};
    var headers = o.headers || {};
    headers["Authorization"] = "Bearer " + token;
    headers["Accept"] = "application/json";
    return fetch(path, {
      method: o.method || "GET",
      headers: headers,
      body: o.body,
      cache: "no-store"
    }).then(function (res) {
      return readBody(res).then(function (body) {
        if (res.status === 401) { endUnauthorized(body && body.code); }
        if (res.status === 403 && body && body.code === "qr_sharing_paused") {
          setStatus("reconnecting", body.error || "QR sharing is currently paused by the host");
        }
        if (res.status >= 200 && res.status < 300) return body;
        var msg = body && body.error ? body.error
          : "Request failed (" + res.status + ")";
        var err = new Error(msg);
        err.status = res.status;
        err.code = body && body.code ? body.code : "";
        err.silent = res.status === 401; // session UI already switching over
        throw err;
      });
    });
  }

  // ------------------------------------------------------------------
  // Real-time Server-Sent Events (Bearer-authenticated via fetch stream)
  // ------------------------------------------------------------------
  var sseState = {
    active: false,
    connecting: false,
    wasEverConnected: false,
    controller: null,
    reconnectTimer: null,
    failures: 0,
    lastEventId: 0
  };

  function disconnectEvents() {
    if (sseState.reconnectTimer) {
      clearTimeout(sseState.reconnectTimer);
      sseState.reconnectTimer = null;
    }
    if (sseState.controller) {
      try { sseState.controller.abort(); } catch (e) { /* gone */ }
      sseState.controller = null;
    }
    sseState.active = false;
    sseState.connecting = false;
  }

  function scheduleEventReconnect(immediate) {
    if (appState.ended || !token) return;
    if (sseState.reconnectTimer) return;
    var delay = immediate ? 120 : retryBackoffMs(Math.max(1, sseState.failures));
    sseState.reconnectTimer = setTimeout(function () {
      sseState.reconnectTimer = null;
      connectEvents();
    }, delay);
  }

  function reconcileAfterEventStream() {
    if (appState.ended || !token) return;
    reconcileActiveTransfers();
    fetchTextMessages();
  }

  function dispatchSSEBlock(rawBlock) {
    if (!rawBlock) return;
    var lines = rawBlock.split(/\r?\n/);
    var eventType = "message";
    var eventId = 0;
    var dataLines = [];
    for (var i = 0; i < lines.length; i++) {
      var line = lines[i];
      if (!line || line.charAt(0) === ":") continue;
      var colon = line.indexOf(":");
      var field = colon >= 0 ? line.slice(0, colon) : line;
      var val = colon >= 0 ? line.slice(colon + 1) : "";
      if (val.charAt(0) === " ") val = val.slice(1);
      if (field === "event") eventType = val;
      else if (field === "id") {
        var parsedId = parseInt(val, 10);
        if (!isNaN(parsedId) && parsedId > 0) eventId = parsedId;
      } else if (field === "data") {
        dataLines.push(val);
      }
    }
    if (dataLines.length === 0) return;

    if (eventId > 0) {
      if (eventId <= sseState.lastEventId) return;
      sseState.lastEventId = eventId;
    }

    var parsed = null;
    try {
      parsed = JSON.parse(dataLines.join("\n"));
    } catch (e) {
      return;
    }

    var envType = (parsed && parsed.type) ? parsed.type : eventType;
    var payload = (parsed && parsed.payload !== undefined) ? parsed.payload : parsed;

    if (envType === "ready") {
      var reconnected = sseState.wasEverConnected || sseState.failures > 0;
      sseState.wasEverConnected = true;
      sseState.active = true;
      sseState.failures = 0;
      setReconnecting(false, "events");
      if (textPollTimer) {
        clearInterval(textPollTimer);
        textPollTimer = null;
      }
      if (reconnected) {
        reconcileAfterEventStream();
      }
      return;
    }

    if (envType === "resync") {
      reconcileAfterEventStream();
      return;
    }

    if (envType === "reconnect") {
      return;
    }

    if (envType === "session.ended") {
      endUnauthorized((payload && payload.code) || "session_ended");
      return;
    }

    if (envType === "files.updated") {
      refreshFiles();
      return;
    }

    if (envType === "text.message" || envType === "text.cleared") {
      fetchTextMessages();
      return;
    }

    if (envType === "batch.pending" || envType === "batch.updated") {
      if (activeBatch && !appState.ended && (!payload || !payload.id || payload.id === activeBatch.id)) {
        if (activeBatch.timer) {
          clearTimeout(activeBatch.timer);
          activeBatch.timer = null;
        }
        pollBatch();
      }
      return;
    }

    if (envType === "download.updated" || envType === "download.progress") {
      if (payload && payload.id && activeDownloadPolls[payload.id]) {
        try { activeDownloadPolls[payload.id](); } catch (e) {}
      }
    }
  }

  function connectEvents() {
    if (appState.ended || !token || sseState.active || sseState.connecting) return;
    if (typeof fetch !== "function" || typeof ReadableStream === "undefined" || typeof TextDecoder === "undefined") {
      return;
    }
    if (sseState.reconnectTimer) {
      clearTimeout(sseState.reconnectTimer);
      sseState.reconnectTimer = null;
    }

    var controller = typeof AbortController === "function" ? new AbortController() : null;
    sseState.controller = controller;
    sseState.connecting = true;

    fetch("/api/events", {
      method: "GET",
      headers: {
        "Authorization": "Bearer " + token,
        "Accept": "text/event-stream"
      },
      cache: "no-store",
      signal: controller ? controller.signal : undefined
    }).then(function (res) {
      if (res.status === 401) {
        return readBody(res).then(function (body) {
          sseState.connecting = false;
          endUnauthorized(body && body.code);
        });
      }
      if (!res.ok || !res.body || typeof res.body.getReader !== "function") {
        throw new Error("event stream status " + res.status);
      }

      sseState.connecting = false;
      var reader = res.body.getReader();
      var decoder = new TextDecoder("utf-8");
      var buffer = "";
      var sawReconnectDirective = false;

      function readLoop() {
        return reader.read().then(function (step) {
          if (appState.ended || sseState.controller !== controller) {
            try { reader.cancel(); } catch (e) {}
            return;
          }
          if (step.done) {
            sseState.active = false;
            sseState.controller = null;
            if (!appState.ended) {
              scheduleTextPoll();
              scheduleEventReconnect(sawReconnectDirective);
            }
            return;
          }
          buffer += decoder.decode(step.value, { stream: true });
          var match;
          while ((match = /\r?\n\r?\n/.exec(buffer)) !== null) {
            var block = buffer.slice(0, match.index);
            buffer = buffer.slice(match.index + match[0].length);
            if (block.indexOf("event: reconnect") >= 0) {
              sawReconnectDirective = true;
            }
            dispatchSSEBlock(block);
          }
          return readLoop();
        });
      }

      return readLoop();
    }).catch(function (err) {
      if (sseState.controller === controller) {
        sseState.controller = null;
      }
      sseState.connecting = false;
      sseState.active = false;
      if (appState.ended || (err && err.name === "AbortError")) return;
      sseState.failures += 1;
      if (sseState.failures >= 2) {
        setReconnecting(true, "events");
      }
      scheduleTextPoll();
      scheduleEventReconnect(false);
    });
  }

  // ------------------------------------------------------------------
  // Upload queue
  //
  // Job phases: queued → declaring → waiting (pending approval) → sending
  // → done; or failed / canceled at any point. Everything from declare to
  // content PUT is sequential: the desktop approves one file at a time.
  // ------------------------------------------------------------------
  function phaseText(job) {
    switch (job.phase) {
      case "queued": return "Queued";
      case "declaring": return "Contacting the desktop…";
      case "waiting": return "Waiting for approval on the desktop…";
      case "sending": return "Uploading…";
      case "resuming": return "Resuming upload…";
      case "reconnecting": {
        var recText = job.reconnectAttempt && job.reconnectMax
          ? "Reconnecting — attempt " + job.reconnectAttempt + "/" + job.reconnectMax
          : "Reconnecting…";
        if (job.err) recText += " · " + job.err;
        return recText;
      }
      case "interrupted": return "Interrupted — ready to resume";
      case "paused": return "Paused";
      case "verifying": return "Verifying…";
      case "canceling": return "Canceling…";
      case "done": return "Completed";
      case "canceled": return "Canceled";
      case "discarded": return "Discarded";
      case "failed": return "Failed";
      case "retrying": return "Resuming…";
    }
    return job.phase;
  }

  function paint(job) {
    var li = job.el;
    if (!li) return;
    li.dataset.phase = job.phase;
    var r = job.refs;
    var text = phaseText(job);
    var active = job.phase === "sending" || job.phase === "resuming" ||
                 job.phase === "reconnecting" ||
                 job.phase === "interrupted" || job.phase === "paused" ||
                 job.phase === "verifying" || job.phase === "done" ||
                 (job.phase === "canceling" && job.uploaded > 0);
    r.bar.hidden = !active;
    var renderedPercent = job.size > 0
      ? Math.min(100, Math.max(0, Math.round((job.uploaded / job.size) * 100)))
      : (job.phase === "done" ? 100 : 0);
    if (job.phase === "done") renderedPercent = 100;
    r.fill.style.width = renderedPercent + "%";
    var ariaStep = job.phase === "done" ? 100 : Math.floor(renderedPercent / 20) * 20;
    if (r.bar.getAttribute("aria-valuenow") !== String(ariaStep)) {
      r.bar.setAttribute("aria-valuenow", String(ariaStep));
    }
    if ((job.phase === "sending" || job.phase === "resuming" || job.phase === "interrupted" || job.phase === "paused") &&
        job.uploaded > 0 && job.size > 0) {
      text += " " + renderedPercent + "%";
      if ((job.phase === "sending" || job.phase === "resuming") && job.speed > 0) {
        text += " · " + fmtBytes(job.speed) + "/s";
        if (job.eta != null && job.eta > 0) {
          text += " · ETA " + fmtEta(job.eta);
        }
      }
    }
    r.state.textContent = text;
    r.err.hidden = !job.err;
    if (job.err) r.err.textContent = job.err;
    // Terminal jobs release their File object cleanly. Interrupted / paused /
    // retryable jobs keep the File reference so Resume can slice from Upload-Offset.
    if (job.file && (job.phase === "done" || job.phase === "canceled" || job.phase === "discarded" ||
                     (job.phase === "failed" && !job.retryable))) {
      job.file = null;
    }
    // Button is state-dependent.
    var label, action;
    if (job.phase === "sending" || job.phase === "resuming" || job.phase === "reconnecting" ||
        job.phase === "queued" || job.phase === "declaring" || job.phase === "waiting" || job.phase === "verifying") {
      label = "Cancel"; action = function () { requestCancel(job); };
    } else if (job.phase === "canceling") {
      label = "Canceling…"; action = function () { /* in flight */ };
    } else if (job.phase === "done" || job.phase === "discarded") {
      label = "Dismiss"; action = function () { removeJob(job); };
    } else if (job.phase === "interrupted" || job.phase === "paused" || job.resumable) {
      label = "Resume";
      action = function () { retryJob(job); };
    } else {
      label = job.retryable ? "Retry" : "Dismiss";
      action = job.retryable
        ? function () { retryJob(job); }
        : function () { removeJob(job); };
    }
    r.btn.textContent = label;
    r.btn.disabled = job.phase === "canceling";
    if (job.btnAction) r.btn.removeEventListener("click", job.btnAction);
    job.btnAction = action;
    r.btn.addEventListener("click", action);
  }

  function buildItem(job) {
    var li = el("li", "queue-item");
    var top = el("div", "queue-item__top");
    top.appendChild(el("span", "queue-item__name", job.name));
    top.appendChild(el("span", "queue-item__size", fmtBytes(job.size)));
    var row = el("div", "queue-item__row");
    var state = el("span", "queue-item__state");
    var bar = el("div", "queue-item__bar");
    bar.setAttribute("role", "progressbar");
    bar.setAttribute("aria-valuemin", "0");
    bar.setAttribute("aria-valuemax", "100");
    bar.setAttribute("aria-valuenow", "0");
    var fill = el("i", "queue-item__fill");
    bar.appendChild(fill);
    bar.hidden = true;
    var btn = el("button", "queue-item__btn");
    btn.type = "button";
    var errLine = el("p", "queue-item__err");
    errLine.hidden = true;
    row.appendChild(state);
    row.appendChild(bar);
    row.appendChild(btn);
    li.appendChild(top);
    li.appendChild(row);
    li.appendChild(errLine);
    job.el = li;
    job.refs = { state: state, bar: bar, fill: fill, btn: btn, err: errLine };
    paint(job);
    return li;
  }

  function addFiles(fileList, folder) {
    Array.prototype.slice.call(fileList).forEach(function (f) {
      var job = {
        // name/size are cached here: paint, buildItem and the manifest
        // read the cache, so a job never has to dereference its File
        // object again. File objects are the only large references a
        // job holds and can then be released at the terminal phase.
        file: f, name: f.name, size: f.size,
        lastModifiedMs: typeof f.lastModified === "number" && f.lastModified > 0 ? Math.round(f.lastModified) : 0,
        id: null, phase: "queued", uploaded: 0,
        failures: 0, autoRetries: 0, reconnectAttempt: 0, reconnectMax: MAX_AUTO_RETRIES,
        retryable: false, err: null,
        xhr: null, timer: null, removed: false, cancelRequested: false,
        cancelInFlight: false,
        el: null, refs: null, btnAction: null, batchID: null, itemID: null,
        relativePath: folder ? f.webkitRelativePath.slice(folder.length + 1) : "",
        rootName: folder || "", group: folder ? "folder:" + folder : "files"
      };
      queue.push(job);
      queueEl.appendChild(buildItem(job));
      if (firefoxLargeUploadWarning(job.size)) {
        setStatus("warning", "Firefox may pause during very large uploads. Keep this page visible; Chrome or Chromium is a temporary alternative if it stalls.");
      }
    });
    syncWakeLock();
    pump();
  }

  function teardownJobs() {
    disconnectEvents();
    if (textPollTimer) { clearInterval(textPollTimer); textPollTimer = null; }
    releaseWakeLock();
    queue.forEach(function (job) {
      job.removed = true;
      if (job.timer) { clearTimeout(job.timer); job.timer = null; }
      if (job.xhr) { job.cancelRequested = true; try { job.xhr.abort(); } catch (e) { /* gone */ } }
    });
  }

  function removeJob(job) {
    job.removed = true;
    if (job.timer) { clearTimeout(job.timer); job.timer = null; }
    if (job.xhr) { job.cancelRequested = true; try { job.xhr.abort(); } catch (e) { /* gone */ } }
    var i = queue.indexOf(job);
    if (i >= 0) queue.splice(i, 1);
    if (job.el && job.el.parentNode) job.el.parentNode.removeChild(job.el);
    syncWakeLock();
    // If the removed job was the blocker, let the queue advance.
    if (!pumpBusy && !appState.ended) pump();
  }

  function requestCancel(job) {
    if (job.removed || appState.ended) return;
    if (job.phase === "queued") {
      // Nothing was declared to the desktop yet, so there is nothing to
      // withdraw: removing the row is already the whole cancellation.
      removeJob(job);
      return;
    }
    if (job.phase === "declaring") {
      // The batch POST is in flight and this job has no IDs yet. The intent
      // is carried to the response handler, which withdraws the declared
      // item instead of uploading it.
      job.cancelRequested = true;
      job.phase = "canceling";
      paint(job);
      return;
    }
    if (job.phase === "waiting" || job.phase === "sending" || job.phase === "resuming" || job.phase === "reconnecting") {
      job.cancelRequested = true;
      job.phase = "canceling";
      if (job.timer) { clearTimeout(job.timer); job.timer = null; }
      if (job.xhr) {
        try { job.xhr.abort(); } catch (e) { /* gone */ }
      }
      setReconnecting(false, "upload");
      paint(job);
      cancelDeclaredItem(job, 1);
      return;
    }
    // done / canceled / failed rows expose Retry or Dismiss instead.
  }

  // cancelDeclaredItem asks the desktop to withdraw one already declared
  // item. The response (or an authoritative refresh after a refusal) is the
  // only thing allowed to turn the row into "canceled": a cancel is never
  // shown as done from local intent alone.
  function cancelDeclaredItem(job, attempt) {
    if (job.removed || appState.ended) return;
    // A retry that lost its purpose (the snapshot already settled the row)
    // must not send another request.
    if (job.phase !== "canceling") return;
    if (!job.batchID || !job.itemID) { refreshBatchAfterCancel(); return; }
    job.cancelInFlight = true;
    api("/api/batches/" + encodeURIComponent(job.batchID) +
        "/items/" + encodeURIComponent(job.itemID), { method: "DELETE" })
      .then(function (batch) {
        job.cancelInFlight = false;
        if (job.removed || appState.ended) return;
        applyBatchSnapshot(batch);
        if (activeBatch) watchBatch();
        else pump();
      })
      .catch(function (err) {
        job.cancelInFlight = false;
        if (job.removed || appState.ended || (err && err.silent)) return;
        if (err && (err.status === 404 || err.status === 409)) {
          // 409 means the item moved past cancellation (completed, failed or
          // the batch ended); 404 means the desktop no longer owns it. Either
          // way the authoritative snapshot decides what to show.
          refreshBatchAfterCancel();
          return;
        }
        if (attempt < CANCEL_RETRIES) {
          setReconnecting(true, "batch");
          waitThen(job, backoffMs(attempt), function () {
            cancelDeclaredItem(job, attempt + 1);
          });
          return;
        }
        refreshBatchAfterCancel();
      });
  }

  function refreshBatchAfterCancel() {
    if (!activeBatch || appState.ended) return;
    api("/api/batches/" + encodeURIComponent(activeBatch.id)).then(function (batch) {
      if (!activeBatch || appState.ended) return;
      applyBatchSnapshot(batch);
      if (activeBatch) watchBatch();
      else pump();
    }).catch(function (err) {
      if (!activeBatch || appState.ended || (err && err.silent)) return;
      if (err && err.status === 404) {
        // The desktop no longer knows the batch. Nothing can be confirmed,
        // so the row says so instead of pretending it was canceled.
        activeBatch.jobs.forEach(function (other) {
          if (other.removed || other.phase === "done" || other.phase === "canceled") return;
          other.phase = "failed";
          other.err = "The desktop no longer has this transfer. Retry to send it again.";
          other.retryable = true;
          other.cancelRequested = false;
          paint(other);
        });
        activeBatch = null; pumpBusy = false; pump();
        return;
      }
      // Still unreachable: keep the row in "canceling" and let the regular
      // batch poll establish the truth when the connection returns.
      activeBatch.failures += 1;
      setReconnecting(true, "batch");
      var batch = activeBatch;
      waitThen(batch, backoffMs(batch.failures), pollBatch);
    });
  }


  function cancelJob(job, msg) {
    if (job.removed || appState.ended) return;
    job.xhr = null;
    job.phase = "canceled";
    job.err = msg || null;
    paint(job);
    pumpBusy = false;
    pump();
  }

  function failJob(job, msg, retryable) {
    if (job.removed || appState.ended) { pumpBusy = false; return; }
    job.xhr = null;
    job.phase = "failed";
    job.err = msg || "Failed";
    job.retryable = !!retryable;
    paint(job);
    pumpBusy = false;
    pump();
  }

  function doneJob(job) {
    if (job.removed || appState.ended) { pumpBusy = false; return; }
    job.xhr = null;
    job.timer = null;
    job.phase = "done";
    job.err = null;
    paint(job);
    pumpBusy = false;
    pump();
  }

  function retryJob(job) {
    if (appState.ended || !job.file) return;
    job.err = null;
    job.failures = 0;
    job.autoRetries = 0;
    job.reconnectAttempt = 0;
    job.speed = 0;
    job.eta = 0;
    job.retryable = false;
    job.cancelRequested = false;
    job.cancelInFlight = false;
    if (job.batchID && job.itemID && job.resumable) {
      // Resume existing approved batch item directly from authoritative server Upload-Offset
      job.phase = "resuming";
      paint(job);
      syncWakeLock();
      pumpBusy = true;
      var resumeSiblings = queue.filter(function (q) {
        return !q.removed && q.batchID === job.batchID;
      });
      activeBatch = { id: job.batchID, jobs: resumeSiblings.length ? resumeSiblings : [job], failures: 0, timer: null };
      sendBatchItem(job);
      return;
    }
    job.id = null;
    job.batchID = null;
    job.itemID = null;
    job.resumable = false;
    job.phase = "retrying";
    job.uploaded = 0;
    if (job.refs && job.refs.fill) job.refs.fill.style.width = "0%";
    if (job.refs && job.refs.bar) job.refs.bar.hidden = true;
    paint(job);
    syncWakeLock();
    // Fresh restart when no durable server batch remains
    setTimeout(function () {
      if (job.removed || appState.ended) return;
      job.phase = "queued";
      paint(job);
      pump();
    }, 200);
  }

  function pump() {
    if (pumpBusy || appState.ended) return;
    var first = queue.find(function (job) { return job.phase === "queued"; });
    if (!first) return;
    var jobs = queue.filter(function (job) {
      return job.phase === "queued" && job.group === first.group;
    });
    if (!jobs.length) return;
    pumpBusy = true;
    jobs.forEach(function (job) { job.phase = "declaring"; paint(job); });
    api("/api/batches", {
      method: "POST",
      headers: { "Content-Type": "application/json" },
      body: JSON.stringify({
        direction: "phone_to_desktop",
        kind: jobs[0].rootName ? "folder" : "files",
        rootName: jobs[0].rootName || undefined,
        items: jobs.map(function (job) {
          return {
            relativePath: jobs[0].rootName ? job.relativePath : job.name,
            size: job.size,
            lastModifiedMs: job.lastModifiedMs || undefined
          };
        })
      })
    }).then(function (batch) {
      if (appState.ended) { pumpBusy = false; return; }
      if (!batch || !batch.id || !Array.isArray(batch.items) ||
          batch.items.length !== jobs.length) {
        throw new Error("The desktop gave an unexpected batch reply");
      }
      activeBatch = { id: batch.id, jobs: jobs, failures: 0, timer: null };
      syncWakeLock();
      var cancels = [];
      batch.items.forEach(function (item, index) {
        var job = jobs[index];
        if (job.removed) return;
        job.batchID = batch.id;
        job.itemID = item.id;
        if (job.cancelRequested) {
          // Withdrawn while the declaration was in flight: now that the item
          // has an ID, withdraw it on the desktop instead of uploading it.
          job.phase = "canceling";
          paint(job);
          cancels.push(job);
        } else {
          job.phase = "waiting";
          paint(job);
        }
      });
      pumpBusy = false;
      cancels.forEach(function (job) { cancelDeclaredItem(job, 1); });
      watchBatch();
    }).catch(function (err) {
      jobs.forEach(function (job) {
        if (job.removed) return;
        if (job.cancelRequested) {
          // The declaration itself failed, so nothing exists on the desktop:
          // a local cancellation is accurate and needs no server call.
          job.phase = "canceled";
          job.err = "Canceled before it was sent.";
          job.cancelRequested = false;
          paint(job);
          return;
        }
        job.phase = "failed";
        if (err && err.status === 507) {
          job.err = "Desktop storage is low. Free up disk space before sending these files.";
        } else {
          job.err = (!err || !err.status || err.status >= 500)
            ? "Could not reach the desktop. Check the Wi-Fi connection and retry."
            : (err.message || "The desktop refuses this batch.");
        }
        job.retryable = true;
        paint(job);
      });
      pumpBusy = false;
      syncWakeLock();
    });
  }

  function watchBatch() {
    if (!activeBatch || appState.ended) return;
    activeBatch.timer = setTimeout(pollBatch, sseState.active ? SSE_BACKUP_POLL_MS : POLL_MS);
  }

  // applyBatchSnapshot folds one authoritative BatchView into the local job
  // rows. Only the desktop decides that a file is done, failed or canceled;
  // local intent never does. A row stays "canceling" while its DELETE is in
  // flight, and after an unconfirmed cancel it falls back to the real queue
  // state instead of claiming a cancellation that never happened.
  function applyBatchSnapshot(batch) {
    if (!activeBatch || !batch || !Array.isArray(batch.items)) return;
    batch.items.forEach(function (item) {
      var job = activeBatch.jobs[item.sequence];
      if (!job || job.removed) return;
      var changed = false;
      if (typeof item.transferred === "number" && item.transferred !== job.uploaded) {
        job.uploaded = item.transferred;
        changed = true;
      }
      var phase = job.phase;
      var err = job.err;
      var retryable = job.retryable;
      // A cancel is still progressing while its DELETE is in flight or a
      // retry is scheduled. A snapshot from that window must not silently
      // abandon the intent: the row stays "canceling" until the request is
      // answered or the retries are exhausted.
      var cancelPending = job.phase === "canceling" && (job.cancelInFlight || !!job.timer);
      var decided = true;
      switch (item.state) {
        case "completed":
          phase = "done"; err = null; retryable = false; job.resumable = false; break;
        case "canceled":
          phase = "canceled"; err = "Upload canceled."; retryable = false; job.resumable = false; break;
        case "discarded":
          phase = "discarded"; err = "Partial upload discarded."; retryable = false; job.resumable = false; break;
        case "rejected":
          phase = "failed"; err = "The desktop declined this file."; retryable = false; job.resumable = false; break;
        case "failed":
          phase = "failed"; err = item.error || "Transfer failed."; retryable = true; job.resumable = false; break;
      case "interrupted":
        decided = false;
        if (!cancelPending && job.phase !== "resuming" && job.phase !== "sending" && job.phase !== "reconnecting") {
          phase = "interrupted";
          err = item.error || "Interrupted — ready to resume.";
          retryable = true;
          job.resumable = true;
        }
        break;
        case "paused":
          decided = false;
          if (!cancelPending) {
            phase = "paused";
            err = "Paused on desktop.";
            retryable = true;
            job.resumable = true;
          }
          break;
        case "transferring":
          decided = false;
          if (!cancelPending) {
            if (job.phase === "canceling") {
              job.cancelRequested = false;
              err = "Cancellation was not confirmed; the file is still transferring.";
            }
            if (job.phase !== "resuming") phase = "sending";
          }
          break;
        default: // pending / approved / queued: not started yet
          decided = false;
          if (!cancelPending && job.phase === "canceling") {
            job.cancelRequested = false;
            phase = "waiting";
            err = "Cancellation was not confirmed; the file is still queued.";
          }
      }
      if (decided) job.cancelRequested = false;
      if (phase !== job.phase || err !== job.err) changed = true;
      job.phase = phase;
      job.err = err;
      job.retryable = retryable;
      if (changed) paint(job);
    });
  }

  function pollBatch() {
    if (!activeBatch || appState.ended) return;
    api("/api/batches/" + encodeURIComponent(activeBatch.id)).then(function (batch) {
      if (!activeBatch || appState.ended) return;
      activeBatch.failures = 0;
      setReconnecting(false, "batch");
      applyBatchSnapshot(batch);
      if (!activeBatch) return;
      if (batch.state === "paused") {
        activeBatch.jobs.forEach(function (job) {
          if (job.phase === "waiting") job.phase = "queued";
          paint(job);
        });
        activeBatch = null; pumpBusy = false; syncWakeLock(); pump(); return;
      }
      if (batch.approvalState === "rejected" || batch.state === "rejected") {
        activeBatch = null; pumpBusy = false; syncWakeLock(); pump(); return;
      }
      // Uploads are strictly FIFO across the batch. The head job — the first
      // row that is not terminal — decides what happens next: a canceling
      // head blocks later items until its DELETE is acknowledged (claiming a
      // later item would be refused by the desktop), a waiting head uploads
      // once approved, and no head at all means the batch is settled.
      var head = null;
      for (var i = 0; i < activeBatch.jobs.length; i++) {
        var job = activeBatch.jobs[i];
        if (job.removed || job.phase === "done" || job.phase === "canceled" ||
            job.phase === "discarded" || job.phase === "failed") continue;
        head = job;
        break;
      }
      if (!head) {
        activeBatch = null; pumpBusy = false; syncWakeLock(); pump(); return;
      }
      if (head.phase === "canceling" || head.phase === "interrupted" || head.phase === "paused" || head.phase === "reconnecting") {
        watchBatch(); return;
      }
      if (head.phase === "waiting" &&
          (batch.approvalState === "approved" || batch.state === "queued" || batch.state === "interrupted")) {
        head.phase = head.uploaded > 0 ? "resuming" : "sending";
        paint(head);
        sendBatchItem(head);
        return;
      }
      watchBatch();
    }).catch(function (err) {
      if (!activeBatch || appState.ended || (err && err.silent)) return;
      if (err && err.status === 404) {
        // The desktop genuinely no longer knows this batch.
        activeBatch.jobs.forEach(function (job) {
          if (job.removed || job.phase === "done" || job.phase === "canceled") return;
          job.phase = "failed"; job.err = "The desktop no longer has this transfer. Retry to send it again."; job.retryable = true;
          job.cancelRequested = false;
          paint(job);
        });
        activeBatch = null; pumpBusy = false;
        syncWakeLock();
        return;
      }
      // Anything else (network drop, timeout, 5xx) is transient. Keep the
      // session and the queue, show that we are reconnecting, and retry with
      // a bounded backoff. A real session end arrives as a 401 above.
      activeBatch.failures += 1;
      setReconnecting(true, "batch");
      var batch = activeBatch;
      waitThen(batch, backoffMs(batch.failures), pollBatch);
    });
  }

  function retryBackoffMs(attempt) {
    var base = Math.min(RETRY_MAX_MS, RETRY_BASE_MS * Math.pow(2, Math.max(0, attempt - 1)));
    var jitter = 0.8 + 0.4 * Math.random();
    return Math.round(base * jitter);
  }

  // Server-requested delay via the Retry-After header (delta seconds or HTTP
  // date); 0 when absent/invalid. Parsing lives in upload-client.js so the
  // worker and page share one policy.
  function retryAfterMsOf(e) {
    if (!e || typeof e.retryAfterMs !== "number" || e.retryAfterMs <= 0) return 0;
    return e.retryAfterMs;
  }

  function formatRetryAfterMs(ms) {
    if (ms >= 60000) return Math.round(ms / 1000 / 60) + " min";
    return Math.round(ms / 100) / 10 + " s";
  }

  function scheduleAutoRetry(job, e) {
    job.autoRetries = (job.autoRetries || 0) + 1;
    if (job.autoRetries > MAX_AUTO_RETRIES || !job.file) {
      setReconnecting(false, "upload");
      job.phase = "interrupted";
      job.err = (e && e.message) || "Upload interrupted — tap Resume to continue.";
      job.retryable = true;
      job.resumable = true;
      paint(job);
      pumpBusy = false;
      syncWakeLock();
      return;
    }
    job.phase = "reconnecting";
    job.reconnectAttempt = job.autoRetries;
    job.reconnectMax = MAX_AUTO_RETRIES;
    job.resumable = true;
    job.retryable = true;
    job.err = null;
    setReconnecting(true, "upload");
    paint(job);

    // Honor any server-requested Retry-After: the wait is the longer of the
    // jittered backoff and the server request. Past MAX_AUTO_RETRY_WAIT_MS the
    // item stays deferred (manual retry via the Resume button) instead of
    // retrying earlier than the server asked.
    var delay = retryBackoffMs(job.autoRetries);
    var ra = retryAfterMsOf(e);
    if (ra > delay) delay = ra;
    if (delay > MAX_AUTO_RETRY_WAIT_MS) {
      job.err = "Server asked to retry after " + formatRetryAfterMs(delay) +
        " — tap Resume when ready.";
      paint(job);
      setReconnecting(false, "upload");
      return;
    }

    waitThen(job, delay, function () {
      if (job.removed || appState.ended || job.cancelRequested || !activeBatch) {
        setReconnecting(false, "upload");
        return;
      }
      fetch("/ping", { cache: "no-store" }).then(function (pingRes) {
        if (job.removed || appState.ended || job.cancelRequested) return;
        if (!pingRes.ok) throw new Error("ping failed");
        setReconnecting(false, "upload");
        job.phase = "resuming";
        paint(job);
        sendBatchItem(job);
      }).catch(function () {
        if (job.removed || appState.ended || job.cancelRequested) return;
        scheduleAutoRetry(job, e);
      });
    });
  }

  function sendBatchItem(job) {
    putBatchContent(job).then(function () {
      if (job.phase === "done" || job.phase === "canceled" || job.phase === "discarded") return;
      setReconnecting(false, "upload");
      job.autoRetries = 0;
      job.phase = "done"; job.err = null; job.resumable = false; paint(job);
      if (activeBatch) {
        activeBatch.jobs.forEach(function (other) {
          if (other !== job && other.phase === "queued") other.phase = "waiting";
          if (other !== job) paint(other);
        });
      }
      watchBatch();
    }).catch(function (e) {
      if (!activeBatch || job.removed || appState.ended) return;
      if (job.phase === "done" || job.phase === "canceled" || job.phase === "discarded") return;
      if (job.cancelRequested) {
        setReconnecting(false, "upload");
        job.phase = "canceling"; job.err = null; job.retryable = false;
        paint(job);
        if (activeBatch) watchBatch();
        return;
      }
      if (e && e.aborted) {
        setReconnecting(false, "upload");
        job.phase = "canceled"; job.err = "Upload canceled."; job.resumable = false;
        job.batchID = null; job.itemID = null; job.uploaded = 0;
        activeBatch.jobs.forEach(function (other) {
          if (other !== job && other.phase === "waiting") other.phase = "queued";
          if (other !== job) paint(other);
        });
        paint(job);
        activeBatch = null; pumpBusy = false;
        syncWakeLock();
        return;
      }
      if (e && e.resumable) {
        if (typeof e.offset === "number" && e.offset > job.uploaded) {
          job.uploaded = e.offset;
        }
        if (e.transient) {
          scheduleAutoRetry(job, e);
          return;
        }
        setReconnecting(false, "upload");
        job.phase = e.paused ? "paused" : "interrupted";
        job.err = (e && e.message) || "Upload interrupted — tap Resume to continue.";
        job.retryable = true;
        job.resumable = true;
        paint(job);
        pumpBusy = false;
        syncWakeLock();
        return;
      }
      job.phase = "failed";
      job.err = (e && e.message) || "Upload failed.";
      job.retryable = true;
      job.resumable = false;
      job.batchID = null;
      job.itemID = null;
      job.uploaded = 0;
      activeBatch.jobs.forEach(function (other) {
        if (other !== job && other.phase === "waiting") other.phase = "queued";
        if (other !== job) paint(other);
      });
      paint(job);
      activeBatch = null; pumpBusy = false;
      syncWakeLock();
    });
  }

  function errorCode(res) {
    try { var parsed = JSON.parse(res.responseText); return parsed && parsed.code; } catch (e) { return undefined; }
  }

  function putBatchContent(job) {
    var lastPaint = 0;
    var lastLoaded = job.uploaded || 0;
    var lastTime = Date.now();
    var currentSpeed = 0;

    var stream = FlitnixUpload.streamResumable({
      url: "/api/batches/" + encodeURIComponent(job.batchID) +
        "/items/" + encodeURIComponent(job.itemID) + "/content",
      headers: {
        "Authorization": "Bearer " + token,
        "X-Flitnix-Compact-Ack": "1"
      },
      skipInitialHead: job.uploaded === 0 && job.phase !== "resuming",
      file: job.file,
      onProgress: function (loaded, total) {
        job.uploaded = Math.min(job.size, loaded);
        var now = Date.now();
        var dt = (now - lastTime) / 1000;
        if (dt >= 0.25 && loaded >= lastLoaded) {
          var instant = (loaded - lastLoaded) / dt;
          currentSpeed = currentSpeed === 0 ? instant : (0.7 * currentSpeed + 0.3 * instant);
          job.speed = currentSpeed;
          lastLoaded = loaded;
          lastTime = now;
          if (currentSpeed > 0 && job.size > job.uploaded) {
            job.eta = Math.round((job.size - job.uploaded) / currentSpeed);
          }
        }
        if (loaded > 0 && job.phase === "resuming") job.phase = "sending";
        if (now - lastPaint >= PAINT_MIN_MS || loaded === total) { lastPaint = now; paint(job); }
      }
    });
    job.xhr = { abort: stream.abort };

    return stream.promise.then(function (res) {
      job.xhr = null;
      if (job.removed || appState.ended) throw { aborted: true };
      if (res.status === 401) { endUnauthorized(errorCode(res)); throw { aborted: true }; }
      if (res.status >= 200 && res.status < 300) { diagnoseUpload("completed", job); return; }
      var code = errorCode(res);
      if (res.status === 507) {
        var lowErr = new Error("Desktop storage is low. Free up disk space before resuming.");
        lowErr.status = 507;
        lowErr.resumable = true;
        lowErr.offset = typeof res.offset === "number" ? res.offset : job.uploaded;
        lowErr.retryAfterMs = retryAfterMsOf(res);
        throw lowErr;
      }
      if (code === "paused") {
        var pauseErr = new Error("Upload paused on desktop.");
        pauseErr.resumable = true;
        pauseErr.paused = true;
        pauseErr.offset = typeof res.offset === "number" ? res.offset : job.uploaded;
        throw pauseErr;
      }
      if (code === "chunk_interrupted" || code === "stall" ||
          res.status === 408 || res.status === 429 ||
          res.status === 502 || res.status === 503 || res.status === 504) {
        var intErr = new Error("Upload interrupted — reconnecting to continue from saved bytes.");
        intErr.resumable = true;
        intErr.transient = true;
        intErr.offset = typeof res.offset === "number" ? res.offset : job.uploaded;
        intErr.retryAfterMs = retryAfterMsOf(res);
        throw intErr;
      }
      var msg = "Upload failed (" + res.status + ")";
      try { var parsed = JSON.parse(res.responseText); if (parsed && parsed.error) msg = parsed.error; } catch (e) {}
      throw new Error(msg);
    }, function (err) {
      job.xhr = null;
      if (err && err.aborted) {
        diagnoseUpload(job.cancelRequested ? "canceled" : "aborted", job);
        throw { aborted: true };
      }
      var off = (err && typeof err.offset === "number") ? err.offset : job.uploaded;
      if (err && err.timeout) {
        diagnoseUpload("timeout", job);
        var tErr = new Error("Upload timed out. Saved bytes are preserved — reconnecting.");
        tErr.resumable = true;
        tErr.transient = true;
        tErr.offset = off;
        throw tErr;
      }
      diagnoseUpload("network-error", job);
      var nErr = new Error("Network error while uploading. Saved bytes are preserved — reconnecting.");
      nErr.resumable = true;
      nErr.transient = true;
      nErr.offset = off;
      throw nErr;
    });
  }

  // ------------------------------------------------------------------
  // Shared files (desktop → phone)
  // ------------------------------------------------------------------
  function refreshFiles() {
    var generation = ++filesRefreshGeneration;
    refreshBtn.disabled = true;
    var prev = refreshBtn.textContent;
    refreshBtn.textContent = "Loading…";
    api("/api/sources").then(function (data) {
      if (appState.ended || generation !== filesRefreshGeneration) return;
      var list = Array.isArray(data) ? data : [];
      if (browseState.source) {
        var stillShared = list.filter(function (s) { return s && s.id === browseState.source.id; })[0];
        if (stillShared) {
          browseState.source = stillShared;
          return loadFolderPage(stillShared, browseState.prefix, 0, false);
        }
        resetBrowseState();
      }
      drawFiles(list);
    }).catch(function (err) {
      if (appState.ended || (err && err.silent)) return;
      if (generation === filesRefreshGeneration) {
        resetBrowseState();
        drawFiles([]);
        filesEmptyEl.textContent = err.message || "Could not load the desktop files.";
      }
    }).then(function () {
      if (generation === filesRefreshGeneration) {
        refreshBtn.disabled = false;
        refreshBtn.textContent = prev;
      }
    });
  }

  function resetBrowseState() {
    browseState.source = null;
    browseState.prefix = "";
    browseState.items = [];
    browseState.totalEntries = 0;
    browseState.hasMore = false;
    browseState.nextCursor = null;
    browseState.selected = {};
    browseState.itemNames = {};
    if (folderNavEl) {
      folderNavEl.hidden = true;
      folderNavEl.textContent = "";
    }
  }

  function drawFiles(list) {
    if (folderNavEl) {
      folderNavEl.hidden = true;
      folderNavEl.textContent = "";
    }
    filesEl.textContent = "";
    var items = (Array.isArray(list) ? list : []).filter(function (f) {
      return f && f.id && f.name && (f.kind === "file" || f.kind === "folder");
    });
    filesEmptyEl.hidden = items.length > 0;
    var actions = document.getElementById("file-actions");
    if (actions) actions.hidden = items.length === 0;
    downloadSelectedBtn.hidden = items.length === 0;
    downloadSelectedBtn.disabled = items.length === 0;
    if (!items.length) {
      filesEmptyEl.textContent = "Nothing is shared with this session yet.";
      return;
    }
    items.forEach(function (f) {
      filesEl.appendChild(fileRow(f));
    });
    updateDownloadSelection();
  }

  function fileRow(f) {
    var li = el("li", "file-item");
    var left = el("div", "file-item__meta");
    left.appendChild(el("span", "file-item__name", f.name));
    left.appendChild(el("span", "file-item__size", f.kind === "folder"
      ? (f.itemCount + " items · " + fmtBytes(f.totalBytes))
      : fmtBytes(f.totalBytes)));
    var check = document.createElement("input");
    check.type = "checkbox";
    check.className = "source-check";
    check.value = f.id;
    check.setAttribute("aria-label", "Select " + f.name);
    check.addEventListener("change", updateDownloadSelection);
    li.appendChild(check);
    li.appendChild(left);
    var actions = el("div", "file-item__actions");
    if (f.kind === "folder") {
      var browseBtn = el("button", "btn btn--small btn--ghost", "Browse");
      browseBtn.type = "button";
      browseBtn.setAttribute("aria-label", "Browse folder " + f.name);
      browseBtn.addEventListener("click", function () {
        resetBrowseState();
        browseState.source = f;
        loadFolderPage(f, "", 0, false);
      });
      actions.appendChild(browseBtn);
    }
    var btn = el("button", "btn btn--small");
    btn.type = "button";
    btn.textContent = "Download";
    btn.addEventListener("click", function () { downloadSources([f], btn); });
    actions.appendChild(btn);
    li.appendChild(actions);
    return li;
  }

  function loadFolderPage(source, prefix, cursor, append) {
    var url = "/api/sources/" + encodeURIComponent(source.id) + "/items?prefix=" + encodeURIComponent(prefix || "") +
      "&cursor=" + encodeURIComponent(String(cursor || 0)) + "&limit=200";
    return api(url).then(function (page) {
      if (appState.ended) return;
      browseState.source = source;
      browseState.prefix = page.prefix || "";
      browseState.totalEntries = page.totalEntries || 0;
      browseState.hasMore = Boolean(page.hasMore);
      browseState.nextCursor = typeof page.nextCursor === "number" ? page.nextCursor : null;
      var newItems = Array.isArray(page.items) ? page.items : [];
      browseState.items = append ? browseState.items.concat(newItems) : newItems;
      newItems.forEach(function (entry) {
        if (entry && entry.id) {
          browseState.itemNames[entry.id] = entry.name;
        }
      });
      renderFolderView();
    }).catch(function (err) {
      if (appState.ended || (err && err.silent)) return;
      filesEmptyEl.hidden = false;
      filesEmptyEl.textContent = (err && err.message) || "Could not open folder.";
    });
  }

  function renderFolderBreadcrumbs() {
    if (!folderNavEl || !browseState.source) return;
    folderNavEl.hidden = false;
    folderNavEl.textContent = "";

    var backBtn = el("button", "btn btn--small btn--ghost", "← All shared");
    backBtn.type = "button";
    backBtn.addEventListener("click", function () {
      resetBrowseState();
      refreshFiles();
    });
    folderNavEl.appendChild(backBtn);

    var crumbs = el("nav", "breadcrumbs");
    crumbs.setAttribute("aria-label", "Folder path");

    function addCrumb(label, targetPrefix, isCurrent) {
      var b = el("button", "breadcrumb-btn", label);
      b.type = "button";
      if (isCurrent) {
        b.setAttribute("aria-current", "location");
        b.disabled = true;
      } else {
        b.addEventListener("click", function () {
          loadFolderPage(browseState.source, targetPrefix, 0, false);
        });
      }
      crumbs.appendChild(b);
    }

    var segs = browseState.prefix ? browseState.prefix.split("/").filter(Boolean) : [];
    addCrumb(browseState.source.name, "", segs.length === 0);
    var acc = "";
    segs.forEach(function (seg, idx) {
      acc = acc ? (acc + "/" + seg) : seg;
      crumbs.appendChild(el("span", "breadcrumb-sep", "/"));
      var target = acc;
      addCrumb(seg, target, idx === segs.length - 1);
    });
    folderNavEl.appendChild(crumbs);
  }

  function renderFolderView() {
    renderFolderBreadcrumbs();
    filesEl.textContent = "";
    var entries = browseState.items || [];
    filesEmptyEl.hidden = entries.length > 0;
    if (!entries.length) {
      filesEmptyEl.textContent = "This folder is empty.";
    }
    var actions = document.getElementById("file-actions");
    if (actions) actions.hidden = false;
    downloadSelectedBtn.hidden = false;

    entries.forEach(function (entry) {
      filesEl.appendChild(browseEntryRow(entry));
    });

    if (browseState.hasMore && browseState.nextCursor !== null) {
      var moreLi = el("li", "");
      var moreBtn = el("button", "btn btn--small btn--ghost browse-load-more",
        "Load more (" + entries.length + " / " + browseState.totalEntries + ")");
      moreBtn.type = "button";
      moreBtn.addEventListener("click", function () {
        moreBtn.disabled = true;
        moreBtn.textContent = "Loading…";
        loadFolderPage(browseState.source, browseState.prefix, browseState.nextCursor, true);
      });
      moreLi.appendChild(moreBtn);
      filesEl.appendChild(moreLi);
    }
    updateDownloadSelection();
  }

  function browseEntryRow(entry) {
    var li = el("li", "file-item");
    var left = el("div", "file-item__meta");
    if (entry.kind === "dir") {
      left.appendChild(el("span", "file-item__name", "📁 " + entry.name + "/"));
      left.appendChild(el("span", "file-item__size",
        (entry.itemCount || 0) + " items · " + fmtBytes(entry.totalBytes || 0)));
      li.appendChild(left);
      var openBtn = el("button", "btn btn--small btn--ghost", "Open");
      openBtn.type = "button";
      openBtn.setAttribute("aria-label", "Open folder " + entry.name);
      openBtn.addEventListener("click", function () {
        loadFolderPage(browseState.source, entry.path, 0, false);
      });
      li.appendChild(openBtn);
      return li;
    }

    var check = document.createElement("input");
    check.type = "checkbox";
    check.className = "source-check browse-item-check";
    check.value = entry.id;
    check.checked = Boolean(browseState.selected[entry.id]);
    check.setAttribute("aria-label", "Select " + entry.name);
    check.addEventListener("change", function () {
      if (check.checked) {
        browseState.selected[entry.id] = true;
      } else {
        delete browseState.selected[entry.id];
      }
      updateDownloadSelection();
    });
    li.appendChild(check);

    left.appendChild(el("span", "file-item__name", entry.name));
    left.appendChild(el("span", "file-item__size", fmtBytes(entry.size || 0)));
    li.appendChild(left);

    var dlBtn = el("button", "btn btn--small", "Download");
    dlBtn.type = "button";
    dlBtn.addEventListener("click", function () {
      downloadSelectedFolderItems(browseState.source, [entry.id], [{ id: entry.id, name: entry.name }], dlBtn);
    });
    li.appendChild(dlBtn);
    return li;
  }

  var downloadIntegrityEl = document.getElementById("download-integrity");

  function renderDownloadIntegrityCard(grant, sources, btn) {
    if (!downloadIntegrityEl) return null;
    downloadIntegrityEl.hidden = false;
    downloadIntegrityEl.innerHTML = "";

    var header = el("div", "download-integrity__header");
    var title = el("h3", "download-integrity__title", grant.filename || "Downloaded file");
    var badge = el("span", "download-integrity__badge badge--downloading", "Downloading");
    header.appendChild(title);
    header.appendChild(badge);
    downloadIntegrityEl.appendChild(header);

    var msg = el("p", "download-integrity__msg", "Downloading file from desktop…");
    downloadIntegrityEl.appendChild(msg);

    var hashbox = el("div", "download-integrity__hashbox");
    var code = el("code", "", "Expected SHA-256: " + (grant.sha256 ? grant.sha256 : "Calculating…"));
    hashbox.appendChild(code);
    var copyBtn = null;
    function addCopyBtn(digest) {
      if (copyBtn) return;
      copyBtn = el("button", "btn btn--small btn--ghost", "Copy");
      copyBtn.type = "button";
      copyBtn.addEventListener("click", function () {
        copyToClipboard(digest, function () {
          copyBtn.textContent = "Copied!";
          setTimeout(function () { copyBtn.textContent = "Copy"; }, 2000);
        });
      });
      hashbox.appendChild(copyBtn);
    }
    if (grant.sha256) {
      addCopyBtn(grant.sha256);
    }
    downloadIntegrityEl.appendChild(hashbox);

    var actions = el("div", "download-integrity__actions");
    downloadIntegrityEl.appendChild(actions);

    return {
      setBadge: function (text, className) {
        badge.textContent = text;
        badge.className = "download-integrity__badge " + className;
      },
      setMessage: function (text) {
        msg.textContent = text;
      },
      setDigest: function (digest) {
        code.textContent = "Expected SHA-256: " + digest;
        addCopyBtn(digest);
      },
      setActions: function (nodes) {
        actions.innerHTML = "";
        nodes.forEach(function (n) { actions.appendChild(n); });
      }
    };
  }

  function verifyPickedFile(file, expectedHex, card, sources, btn) {
    if (!expectedHex) {
      card.setBadge("Verification unavailable", "badge--unavailable");
      card.setMessage("No expected checksum was provided by the server.");
      return;
    }
    var hasher = (typeof sha256 !== "undefined" && sha256.create) ? sha256.create() : null;
    if (!hasher) {
      card.setBadge("Verification unavailable in this browser", "badge--unavailable");
      card.setMessage("Incremental cryptographic hashing is not available in this browser.");
      return;
    }

    card.setBadge("Verifying…", "badge--verifying");
    card.setMessage("Reading and hashing \"" + file.name + "\" in bounded 64 KB chunks… (0%)");
    card.setActions([]);

    var totalSize = file.size;
    var processed = 0;
    var lastUpdate = 0;

    function finishHash() {
      var actualDigest = hasher.hex().toLowerCase();
      var wantDigest = expectedHex.toLowerCase();
      if (actualDigest === wantDigest) {
        card.setBadge("Verified", "badge--verified");
        card.setMessage("Verified — Exact SHA-256 match! Received bytes are identical to source (" + actualDigest.slice(0, 16) + "…).");
      } else {
        card.setBadge("Verification failed", "badge--failed");
        card.setMessage("Verification failed — Checksum mismatch! Actual: " + actualDigest.slice(0, 16) + "…, Expected: " + wantDigest.slice(0, 16) + "…");
        var retry = el("button", "btn btn--small", "Retry Download");
        retry.type = "button";
        retry.addEventListener("click", function () {
          if (card && card.retry) card.retry(); else downloadSources(sources, btn);
        });
        card.setActions([retry]);
      }
    }

    if (typeof file.stream === "function") {
      var reader = file.stream().getReader();
      function readChunk() {
        reader.read().then(function (result) {
          if (result.done) {
            finishHash();
            return;
          }
          hasher.update(result.value);
          processed += result.value.length;
          var now = Date.now();
          if (now - lastUpdate > 150) {
            lastUpdate = now;
            var pct = totalSize > 0 ? Math.min(100, Math.round((processed / totalSize) * 100)) : 100;
            card.setMessage("Reading and hashing \"" + file.name + "\" in bounded 64 KB chunks… (" + pct + "%)");
          }
          readChunk();
        }).catch(function (err) {
          card.setBadge("Verification failed", "badge--failed");
          card.setMessage("Error reading file stream: " + (err && err.message ? err.message : err));
        });
      }
      readChunk();
    } else {
      var chunkSize = 64 * 1024;
      var offset = 0;
      function readSlice() {
        if (offset >= totalSize) {
          finishHash();
          return;
        }
        var slice = file.slice(offset, offset + chunkSize);
        var fr = new FileReader();
        fr.onload = function (e) {
          hasher.update(new Uint8Array(e.target.result));
          offset += chunkSize;
          processed = Math.min(totalSize, offset);
          var now = Date.now();
          if (now - lastUpdate > 150) {
            lastUpdate = now;
            var pct = totalSize > 0 ? Math.min(100, Math.round((processed / totalSize) * 100)) : 100;
            card.setMessage("Reading and hashing \"" + file.name + "\" in bounded 64 KB chunks… (" + pct + "%)");
          }
          readSlice();
        };
        fr.onerror = function () {
          card.setBadge("Verification failed", "badge--failed");
          card.setMessage("File read error during checksum verification.");
        };
        fr.readAsArrayBuffer(slice);
      }
      readSlice();
    }
  }

  function downloadSources(sources, btn) {
    return runDownloadGrant({ sourceIds: sources.map(function (source) { return source.id; }) }, sources, btn);
  }

  function downloadSelectedFolderItems(source, itemIds, displaySources, btn) {
    return runDownloadGrant({ sourceId: source.id, itemIds: itemIds }, displaySources, btn);
  }

  function runDownloadGrant(grantBody, sources, btn) {
    btn.disabled = true;
    var prev = btn.textContent;
    btn.textContent = "Preparing…";
    fetch("/api/download-grants", {
      method: "POST",
      headers: { "Authorization": "Bearer " + token, "Content-Type": "application/json" },
      body: JSON.stringify(grantBody),
      cache: "no-store"
    }).then(function (res) {
      if (res.status === 401) {
        return res.json().catch(function () { return null; }).then(function (body) {
          endUnauthorized(body && body.code);
          throw new Error("silent");
        });
      }
      if (!res.ok) {
        return res.text().then(function (t) {
          var msg = "Download failed (" + res.status + ")";
          try {
            var parsed = JSON.parse(t);
            if (parsed && parsed.error) msg = parsed.error;
          } catch (e) { /* plain body */ }
          throw new Error(msg);
        });
      }
      return res.json().then(function (grant) {
        if (!grant || !grant.downloadUrl) throw new Error("The desktop returned an invalid download grant.");
        var dlKey = grant.progressId || grant.downloadUrl;
        activeDownloads[dlKey] = true;
        syncWakeLock();
        var card = renderDownloadIntegrityCard(grant, sources, btn);
        if (card) {
          card.retry = function () { runDownloadGrant(grantBody, sources, btn); };
        }

        // Check if browser supports showSaveFilePicker in secure context
        if (typeof window.showSaveFilePicker === "function") {
          window.showSaveFilePicker({ suggestedName: grant.filename }).then(function (handle) {
            if (card) {
              card.setBadge("Downloading", "badge--downloading");
              card.setMessage("Streaming to storage and hashing incrementally…");
            }
            return handle.createWritable().then(function (writable) {
              var MAX_DL_RETRIES = 6;
              var received = 0;
              var totalSize = null;
              var validatorETag = "";
              var retries = 0;
              var streamHasher = (typeof sha256 !== "undefined" && sha256.create) ? sha256.create() : null;

              function isStrongETag(tag) {
                return Boolean(tag && tag.indexOf("W/") !== 0 && tag.indexOf("w/") !== 0);
              }

              function parseContentRange(cr) {
                if (!cr) return null;
                var m = String(cr).match(/^bytes\s+(\d+)-(\d+)\/(\d+|\*)$/i);
                if (!m) return null;
                return {
                  start: parseInt(m[1], 10),
                  end: parseInt(m[2], 10),
                  total: m[3] === "*" ? null : parseInt(m[3], 10)
                };
              }

              function finishAndVerify() {
                return writable.close().then(function () {
                  delete activeDownloads[dlKey];
                  syncWakeLock();
                  if (card) card.setBadge("Verifying…", "badge--verifying");
                  var checkDigest = grant.sha256;
                  function verifyStream() {
                    if (streamHasher && checkDigest) {
                      var actualHex = streamHasher.hex().toLowerCase();
                      if (actualHex === checkDigest.toLowerCase()) {
                        if (card) {
                          card.setBadge("Verified", "badge--verified");
                          card.setMessage("Verified — Exact SHA-256 match! Saved directly to disk (" + actualHex.slice(0, 16) + "…).");
                        }
                      } else if (card) {
                        card.setBadge("Verification failed", "badge--failed");
                        card.setMessage("Verification failed — Streamed checksum mismatch!");
                        var retry = el("button", "btn btn--small", "Retry Download");
                        retry.type = "button";
                        retry.addEventListener("click", function () {
                          if (card && card.retry) card.retry(); else downloadSources(sources, btn);
                        });
                        card.setActions([retry]);
                      }
                    } else if (card) {
                      card.setBadge("Downloaded", "badge--waiting");
                      card.setMessage("File saved directly to disk.");
                    }
                  }
                  if (!checkDigest && grant.progressId) {
                    api("/api/download-progress/" + encodeURIComponent(grant.progressId)).then(function (p) {
                      if (p && p.sha256) checkDigest = p.sha256;
                      if (card && card.setDigest && checkDigest) card.setDigest(checkDigest);
                      verifyStream();
                    }).catch(verifyStream);
                  } else {
                    verifyStream();
                  }
                });
              }

              function scheduleRetry(errMsg, retryAfterMs) {
                retries += 1;
                var base = Math.min(15000, 1000 * Math.pow(2, retries - 1));
                var delay = Math.round(base * (0.8 + Math.random() * 0.4));
                // Honor a server-requested Retry-After (never shorter than the
                // jittered backoff; the caller rejects waits beyond the cap).
                if (typeof retryAfterMs === "number" && retryAfterMs > delay) delay = retryAfterMs;
                if (card) {
                  card.setBadge("Reconnecting…", "badge--waiting");
                  card.setMessage("Interrupted (" + errMsg + ") — resuming from " + fmtBytes(received) + " (attempt " + retries + "/" + MAX_DL_RETRIES + ")…");
                }
                return new Promise(function (resolve) {
                  setTimeout(resolve, delay);
                }).then(fetchAttempt);
              }

              function fetchAttempt() {
                var headers = {};
                if (received > 0) {
                  headers["Range"] = "bytes=" + received + "-";
                  if (isStrongETag(validatorETag)) {
                    headers["If-Range"] = validatorETag;
                  }
                }
                return fetch(grant.downloadUrl, { headers: headers, cache: "no-store" }).then(function (resp) {
                  if (resp.status === 416 && received > 0 && totalSize !== null && received === totalSize) {
                    return finishAndVerify();
                  }
                  if (!resp.ok) {
                    var httpErr = new Error("HTTP " + resp.status);
                    httpErr.status = resp.status;
                    httpErr.retryAfterMs = window.FlitnixUpload ?
                      FlitnixUpload.parseRetryAfterMs(resp.headers && resp.headers.get("Retry-After"), Date.now()) : 0;
                    throw httpErr;
                  }
                  var respETag = resp.headers ? resp.headers.get("ETag") : "";
                  if (resp.status === 200) {
                    var cl = resp.headers ? parseInt(resp.headers.get("Content-Length") || "", 10) : NaN;
                    totalSize = isFinite(cl) && cl >= 0 ? cl : null;
                    if (isStrongETag(respETag)) validatorETag = respETag;
                    if (received > 0) {
                      // Server returned a fresh 200 representation (e.g. If-Range mismatch or archive).
                      received = 0;
                      streamHasher = (typeof sha256 !== "undefined" && sha256.create) ? sha256.create() : null;
                      return writable.truncate(0).then(function () {
                        return writable.seek(0);
                      }).then(function () {
                        return consumeBody(resp);
                      });
                    }
                    return consumeBody(resp);
                  }
                  if (resp.status === 206) {
                    var cr = parseContentRange(resp.headers ? resp.headers.get("Content-Range") : "");
                    if (!cr || cr.start !== received) {
                      var mismatchErr = new Error("Unexpected Content-Range offset");
                      mismatchErr.fatal = true;
                      throw mismatchErr;
                    }
                    if (cr.total !== null) totalSize = cr.total;
                    if (!validatorETag && isStrongETag(respETag)) validatorETag = respETag;
                    return consumeBody(resp);
                  }
                  return consumeBody(resp);
                }).catch(function (err) {
                  if (appState.ended) throw err;
                  var retryAfterMs = err && typeof err.retryAfterMs === "number" ? err.retryAfterMs : 0;
                  var retryable = !(err && (err.fatal || err.status === 401 || err.status === 403 || err.status === 404 || err.status === 410));
                  if (retryable && retries < MAX_DL_RETRIES && received > 0) {
                    if (retryAfterMs > MAX_AUTO_RETRY_WAIT_MS) {
                      // Deferred: the server asked for a longer wait than the
                      // automatic policy allows — surface it instead of retrying early.
                      throw new Error("Server requested retry after " + formatRetryAfterMs(retryAfterMs));
                    }
                    return scheduleRetry(err && err.message ? err.message : "network error", retryAfterMs);
                  }
                  throw err;
                });
              }

              function consumeBody(resp) {
                var reader = resp.body.getReader();
                var lastUpdate = 0;
                function pump() {
                  return reader.read().then(function (result) {
                    if (result.done) {
                      if (totalSize !== null && received < totalSize) {
                        var shortErr = new Error("stream ended early at " + fmtBytes(received) + " / " + fmtBytes(totalSize));
                        if (retries < MAX_DL_RETRIES) {
                          return scheduleRetry(shortErr.message);
                        }
                        throw shortErr;
                      }
                      return finishAndVerify();
                    }
                    retries = 0;
                    if (streamHasher) streamHasher.update(result.value);
                    received += result.value.length;
                    var now = Date.now();
                    if (now - lastUpdate > 200) {
                      lastUpdate = now;
                      if (card) {
                        card.setBadge("Downloading", "badge--downloading");
                        card.setMessage("Streaming and hashing incrementally… (" + fmtBytes(received) + (totalSize ? " / " + fmtBytes(totalSize) : "") + ")");
                      }
                    }
                    return writable.write(result.value).then(pump);
                  });
                }
                return pump();
              }

              return fetchAttempt().catch(function (err) {
                return writable.abort().catch(function () {}).then(function () { throw err; });
              });
            });
          }).catch(function (e) {
            // User cancelled picker or browser refused stream -> fall back to native download
            if (e && e.name === "AbortError") {
              delete activeDownloads[dlKey];
              syncWakeLock();
              if (card) {
                card.setBadge("Canceled", "badge--canceled");
                card.setMessage("Download save prompt was canceled.");
              }
              return;
            }
            if (e && (e.status === 401 || e.status === 403 || e.status === 404 || e.status === 410)) {
              delete activeDownloads[dlKey];
              syncWakeLock();
              if (card) {
                card.setBadge("Canceled or interrupted", "badge--canceled");
                card.setMessage("Download could not be resumed: " + e.message);
              }
              return;
            }
            // Fall back to native download
            startNativeDownload(grant.downloadUrl, grant.progressId);
            if (grant.progressId) watchDownloadProgress(grant.progressId, grant, card, sources, btn);
          });
        } else {
          // Native download fallback (mobile browsers / HTTP)
          startNativeDownload(grant.downloadUrl, grant.progressId);
          if (grant.progressId) watchDownloadProgress(grant.progressId, grant, card, sources, btn);
        }
      });
    }).catch(function (err) {
      if (err && err.message === "silent") return;
      var msg = err && err.message ? err.message : "Download failed.";
      if (err instanceof TypeError) {
        msg = "Network error — could not reach the desktop.";
      }
      showDownloadError(sources[0], msg);
    }).then(function () {
      btn.disabled = false;
      btn.textContent = prev;
    });
  }

  // The browser's own download manager fetches the grant URL. It is loaded in
  // an off-screen frame (never display:none, which iOS Safari can ignore), and
  // never by navigating this page: if the server answers with an error instead
  // of an attachment, a top-level navigation would replace this page and
  // discard the session token held only in memory.
  var downloadFrames = {};
  function startNativeDownload(url, progressId) {
    var frame = document.createElement("iframe");
    frame.className = "offscreen-frame";
    frame.setAttribute("aria-hidden", "true");
    frame.tabIndex = -1;
    frame.src = url;
    document.body.appendChild(frame);
    var key = progressId || url;
    downloadFrames[key] = frame;
    // Safety net only; normally the watcher removes it at the terminal state.
    setTimeout(function () { removeDownloadFrame(key); }, 30 * 60 * 1000);
  }
  function removeDownloadFrame(key) {
    var frame = downloadFrames[key];
    if (!frame) return;
    delete downloadFrames[key];
    if (frame.parentNode) frame.parentNode.removeChild(frame);
  }

  function updateDownloadSelection() {
    if (browseState.source) {
      var ids = Object.keys(browseState.selected);
      downloadSelectedBtn.disabled = ids.length === 0;
      downloadSelectedBtn.textContent = ids.length > 0 ? ("Download selected (" + ids.length + ")") : "Download selected";
      return;
    }
    var selected = filesEl.querySelectorAll(".source-check:checked");
    downloadSelectedBtn.disabled = selected.length === 0;
    downloadSelectedBtn.textContent = "Download selected";
  }

  // Follows one archive/file download. It polls only while the download is
  // live and stops at the first terminal answer; a transient failure retries
  // with a bounded backoff instead of abandoning the watch.
  function watchDownloadProgress(id, grant, card, sources, btn) {
    var failures = 0;
    var fallbackOffered = false;
    var startedAt = Date.now();
    activeDownloads[id] = true;
    activeDownloadPolls[id] = tick;
    syncWakeLock();
    function tick() {
      if (appState.ended) return;
      api("/api/download-progress/" + encodeURIComponent(id)).then(function (p) {
        failures = 0;
        setReconnecting(false, "download");
        if (p.state === "pending" && !fallbackOffered && Date.now() - startedAt > 2500 && card && grant && grant.downloadUrl) {
          fallbackOffered = true;
          card.setMessage("Waiting for browser download prompt… If it didn't appear (common on iOS Safari), tap Save File below.");
          var directLink = el("a", "btn btn--small", "Save File");
          directLink.href = grant.downloadUrl;
          if (grant.filename) directLink.setAttribute("download", grant.filename);
          card.setActions([directLink]);
        }
        if (p.state === "failed" || p.state === "expired") {
          delete activeDownloads[id];
          delete activeDownloadPolls[id];
          syncWakeLock();
          removeDownloadFrame(id);
          if (card) {
            card.setBadge("Canceled or interrupted", "badge--canceled");
            card.setMessage("Download was interrupted or canceled: " + (p.error || "source unavailable"));
            if (sources && btn) {
              var retryBtn = el("button", "btn btn--small", "Retry Download");
              retryBtn.type = "button";
              retryBtn.addEventListener("click", function () {
                if (card && card.retry) card.retry(); else downloadSources(sources, btn);
              });
              card.setActions([retryBtn]);
            }
          }
          filesEmptyEl.hidden = false;
          filesEmptyEl.textContent = "Download failed: " + (p.error || "source unavailable");
          setTimeout(function () { if (!appState.ended) refreshFiles(); }, 3500);
          return;
        }
        if (p.state === "completed") {
          delete activeDownloads[id];
          delete activeDownloadPolls[id];
          syncWakeLock();
          removeDownloadFrame(id);
          if (card && grant) {
            var finalDigest = (p && p.sha256) || grant.sha256;
            if (finalDigest && card.setDigest) {
              card.setDigest(finalDigest);
            }
            card.setBadge("Checksum available", "badge--waiting");
            card.setMessage("Checksum available; download not automatically verified by native browser. You can verify your saved file below:");
            var verifyLabel = el("label", "btn btn--small", "Verify Downloaded File");
            var fileInput = document.createElement("input");
            fileInput.type = "file";
            fileInput.hidden = true;
            fileInput.addEventListener("change", function () {
              if (fileInput.files && fileInput.files[0]) {
                verifyPickedFile(fileInput.files[0], finalDigest, card, sources, btn);
              }
            });
            verifyLabel.appendChild(fileInput);
            var redownloadBtn = el("button", "btn btn--small btn--ghost", "Download Again");
            redownloadBtn.type = "button";
            redownloadBtn.addEventListener("click", function () {
              if (card && card.retry) card.retry(); else downloadSources(sources, btn);
            });
            card.setActions([verifyLabel, redownloadBtn]);
          }
          return;
        }
        setTimeout(tick, sseState.active ? SSE_PROGRESS_BACKUP_MS : PROGRESS_POLL_MS);
      }).catch(function (err) {
        if (appState.ended || (err && err.silent)) return;
        if (err && err.status === 404) {
          delete activeDownloads[id];
          delete activeDownloadPolls[id];
          syncWakeLock();
          return; // record already cleaned up
        }
        failures += 1;
        if (failures >= MAX_PROGRESS_FAILURES) {
          delete activeDownloads[id];
          delete activeDownloadPolls[id];
          syncWakeLock();
          setReconnecting(false, "download");
          return;
        }
        setTimeout(tick, backoffMs(failures));
      });
    }
    setTimeout(tick, sseState.active ? SSE_PROGRESS_BACKUP_MS : PROGRESS_POLL_MS);
  }

  function showDownloadError(f, msg) {
    // Reuse the empty line as an inline error carrier.
    filesEmptyEl.hidden = false;
    filesEmptyEl.textContent = "Saving \"" + f.name + "\": " + msg;
    setTimeout(function () {
      if (!appState.ended) refreshFiles();
    }, 3500);
  }

  function saveBlob(blob, name) {
    var url = URL.createObjectURL(blob);
    var a = document.createElement("a");
    a.href = url;
    a.download = name;
    document.body.appendChild(a);
    a.click();
    a.remove();
    window.setTimeout(function () { URL.revokeObjectURL(url); }, 10000);
  }

  /* Content-Disposition parsing (mime.FormatMediaType output):
   *   attachment; filename="a b.png"            (ASCII names)
   *   attachment; filename*=utf-8''a%20b.png    (non-ASCII names)
   * Percent-decodes RFC 5987 values; falls back to the listed name. */
  function dispositionName(cd, fallback) {
    if (!cd) return fallback;
    var m = cd.match(/filename\*\s*=\s*([^;]+)/i);
    if (m) {
      var starred = m[1].trim();
      var trio = starred.match(/^[^']*'[^']*'(.*)$/);
      var value = trio ? trio[1] : starred;
      try { value = decodeURIComponent(value); } catch (err) { /* raw */ }
      return value || fallback;
    }
    m = cd.match(/filename\s*=\s*("[^"]*"|[^;]+)/i);
    if (m) {
      var plain = m[1].trim();
      if (plain.charAt(0) === "\"") plain = plain.slice(1, -1);
      plain = plain.replace(/\\(.)/g, "$1");
      return plain || fallback;
    }
    return fallback;
  }

  // ------------------------------------------------------------------
  // Quick Text & Link Sharing
  // ------------------------------------------------------------------
  function renderTextMessages(messages) {
    if (!textMessagesEl) return;
    textMessagesEl.innerHTML = "";
    if (!messages || !messages.length) {
      var emptyLi = el("li", "muted", "No messages yet. Send a note or link to get started.");
      emptyLi.style.padding = "0.5rem 0";
      textMessagesEl.appendChild(emptyLi);
      return;
    }
    // Render newest message first so the latest note is visible at top
    var items = messages.slice().reverse();
    items.forEach(function (msg) {
      var li = el("li", "text-msg");
      var hdr = el("div", "text-msg__header");
      var senderLabel = msg.sender === "desktop" ? "PC (Desktop)" : "Phone";
      var senderBadge = el("span", "text-msg__badge text-msg__badge--" + (msg.sender === "desktop" ? "desktop" : "phone"), senderLabel);
      var timeStr = "";
      try {
        timeStr = new Date(msg.createdAt).toLocaleTimeString([], { hour: "2-digit", minute: "2-digit" });
      } catch (e) { timeStr = ""; }
      var timeSpan = el("span", "", timeStr);
      hdr.appendChild(senderBadge);
      hdr.appendChild(timeSpan);
      li.appendChild(hdr);

      var body = el("div", "text-msg__content", msg.content);
      li.appendChild(body);

      var actions = el("div", "text-msg__actions");
      var copyBtn = el("button", "text-msg__btn", "Copy");
      copyBtn.type = "button";
      copyBtn.addEventListener("click", function () {
        copyToClipboard(msg.content, function () {
          copyBtn.textContent = "Copied!";
          setTimeout(function () { copyBtn.textContent = "Copy"; }, 1500);
        });
      });
      actions.appendChild(copyBtn);

      if (msg.isLink && msg.url) {
        var openBtn = el("a", "text-msg__btn", "Open Link");
        openBtn.href = msg.url;
        openBtn.target = "_blank";
        openBtn.rel = "noopener noreferrer";
        actions.appendChild(openBtn);
      }
      li.appendChild(actions);
      textMessagesEl.appendChild(li);
    });
  }

  function fetchTextMessages() {
    if (appState.ended || !token) return;
    api("/api/text").then(function (res) {
      if (appState.ended) return;
      var list = (res && Array.isArray(res.messages)) ? res.messages : (Array.isArray(res) ? res : []);
      textMessagesCache = list;
      renderTextMessages(textMessagesCache);
    }).catch(function () {
      // transient failures are ignored during background polling
    });
  }

  function scheduleTextPoll() {
    if (appState.ended) return;
    if (textPollTimer) {
      clearInterval(textPollTimer);
      textPollTimer = null;
    }
    fetchTextMessages();
    if (sseState.active) return;
    textPollTimer = setInterval(function () {
      if (appState.ended || sseState.active) {
        if (textPollTimer) { clearInterval(textPollTimer); textPollTimer = null; }
        return;
      }
      fetchTextMessages();
    }, 2500);
  }

  var quickTextInitialized = false;
  function initQuickText() {
    if (quickTextInitialized || !textForm || !textInput) return;
    quickTextInitialized = true;
    function updateSendBtn() {
      var len = textInput.value.length;
      var trimmed = textInput.value.trim();
      if (textCharCount) {
        textCharCount.textContent = len > 1024 ? Math.round(len / 1024) + " KB / 64 KB" : len + " / 64 KB";
      }
      if (textSendBtn) textSendBtn.disabled = trimmed.length === 0 || len > 65536;
    }
    textInput.addEventListener("input", updateSendBtn);
    textInput.addEventListener("keydown", function (e) {
      if (e.key === "Enter" && (e.ctrlKey || e.metaKey)) {
        e.preventDefault();
        textForm.dispatchEvent(new Event("submit"));
      }
    });

    textForm.addEventListener("submit", function (e) {
      e.preventDefault();
      var text = textInput.value.trim();
      if (!text || text.length > 65536) return;
      if (textSendBtn) textSendBtn.disabled = true;
      api("/api/text", {
        method: "POST",
        headers: { "Content-Type": "application/json" },
        body: JSON.stringify({ content: text, text: text })
      }).then(function () {
        textInput.value = "";
        if (textCharCount) textCharCount.textContent = "0 / 64 KB";
        if (textSendBtn) textSendBtn.disabled = true;
        fetchTextMessages();
      }).catch(function (err) {
        if (textSendBtn) textSendBtn.disabled = false;
        alert("Failed to send text: " + ((err && err.message) || "Unknown error"));
      });
    });
  }

  function defaultBrowserDeviceName() {
    var ua = navigator.userAgent || "";
    if (/iPhone/i.test(ua)) return "iPhone Safari";
    if (/iPad/i.test(ua)) return "iPad Safari";
    if (/Android/i.test(ua)) return "Android Browser";
    return "Mobile Browser";
  }

  function showPairError(msg) {
    if (!pairError) return;
    pairError.textContent = msg || "";
    pairError.hidden = !msg;
  }

  function showReconnectError(msg) {
    if (!reconnectError) return;
    reconnectError.textContent = msg || "";
    reconnectError.hidden = !msg;
  }

  function setPairedUI(rec) {
    if (!trustPanel) return;
    if (rec && rec.deviceId) {
      if (trustBadge) {
        trustBadge.className = "badge badge--done";
        trustBadge.textContent = "Paired";
      }
      if (pairControls) pairControls.hidden = true;
      if (pairSasBox) pairSasBox.hidden = true;
      if (pairedInfoBox) pairedInfoBox.hidden = false;
      if (pairedMetaText) {
        pairedMetaText.textContent = "Trusted as \"" + (rec.deviceName || "This Browser") + "\". Bookmark the one-tap URL below to reconnect without scanning a new QR code.";
      }
      if (copyReconnectBtn) {
        copyReconnectBtn.onclick = function () {
          var bookmarkUrl = window.location.origin + "/#device=" + encodeURIComponent(rec.deviceId);
          copyToClipboard(bookmarkUrl, function () {
            copyReconnectBtn.textContent = "Copied Bookmark URL!";
            setTimeout(function () { copyReconnectBtn.textContent = "Copy One-Tap Bookmark URL"; }, 1800);
          });
        };
      }
      if (unpairLocalBtn) {
        unpairLocalBtn.onclick = function () {
          unpairLocalBtn.disabled = true;
          api("/api/devices/" + encodeURIComponent(rec.deviceId), { method: "DELETE" }).catch(function () {})
            .then(function () {
              return window.FlitnixDeviceCrypto.removePairedHost(rec.deviceId);
            })
            .then(function () {
              setPairedUI(null);
            })
            .finally(function () {
              unpairLocalBtn.disabled = false;
            });
        };
      }
      return;
    }
    if (trustBadge) {
      trustBadge.className = "badge badge--waiting";
      trustBadge.textContent = "Not paired";
    }
    if (pairControls) pairControls.hidden = false;
    if (pairSasBox) pairSasBox.hidden = true;
    if (pairedInfoBox) pairedInfoBox.hidden = true;
  }

  function setPairingStorageUnavailableUI(reason) {
    if (trustBadge) {
      trustBadge.className = "badge badge--waiting";
      trustBadge.textContent = "Unavailable";
    }
    if (trustDesc) {
      trustDesc.textContent = reason || "Trusted device pairing requires persistent IndexedDB storage and is disabled in private/blocked browser modes. QR and 6-digit codes continue working normally.";
    }
    if (pairControls) pairControls.hidden = true;
    if (pairSasBox) pairSasBox.hidden = true;
    if (pairedInfoBox) pairedInfoBox.hidden = true;
    if (reconnectPanel) reconnectPanel.hidden = true;
  }

  var trustedPairingBound = false;
  function initTrustedPairingUI() {
    if (!trustPanel) return;
    var cryptoApi = window.FlitnixDeviceCrypto;
    if (!cryptoApi || typeof indexedDB === "undefined") {
      setPairingStorageUnavailableUI();
      return;
    }
    if (pairDeviceNameInput && !pairDeviceNameInput.value) {
      pairDeviceNameInput.value = defaultBrowserDeviceName();
    }
    cryptoApi.probeStorage().then(function () {
      return Promise.all([
        cryptoApi.listPairedHostsForOrigin(window.location.origin),
        cryptoApi.hasValidIdentity()
      ]);
    }).then(function (res) {
      var hosts = res[0];
      var hasIdentity = res[1];
      var match = (hosts && hosts.length > 0) ? hosts[0] : null;
      if (!match) {
        setPairedUI(null);
        return;
      }
      if (!hasIdentity) {
        // Key missing or corrupt -> purge orphaned host record and fail closed.
        return cryptoApi.removePairedHost(match.deviceId).then(function () { setPairedUI(null); });
      }
      return api("/api/devices").then(function (list) {
        var enrolled = Array.isArray(list) && list.some(function (d) { return d && d.id === match.deviceId; });
        if (!enrolled) {
          return cryptoApi.removePairedHost(match.deviceId).then(function () { setPairedUI(null); });
        }
        setPairedUI(match);
      }).catch(function () {
        setPairedUI(match);
      });
    }).catch(function () {
      setPairingStorageUnavailableUI();
    });

    if (trustedPairingBound || !pairStartBtn) return;
    trustedPairingBound = true;
    pairStartBtn.addEventListener("click", function () {
      var deviceName = (pairDeviceNameInput && pairDeviceNameInput.value.trim()) || defaultBrowserDeviceName();
      showPairError("");
      pairStartBtn.disabled = true;
      var pubIdentity;
      var eph;
      var challengeResp;
      cryptoApi.probeStorage().then(function () {
        return cryptoApi.ensurePublicIdentity();
      }).then(function (idView) {
        pubIdentity = idView;
        eph = cryptoApi.createEphemeralPairingData();
        return api("/api/pair/challenge", {
          method: "POST",
          headers: { "Content-Type": "application/json" },
          body: JSON.stringify({
            clientPublicKey: pubIdentity.publicKeyHex,
            clientCommitment: eph.commitmentHex
          })
        });
      }).then(function (chal) {
        challengeResp = chal;
        return api("/api/pair/reveal", {
          method: "POST",
          headers: { "Content-Type": "application/json" },
          body: JSON.stringify({
            pairingNonce: chal.pairingNonce,
            clientEphemeralKey: eph.ephemeralPublicKeyHex,
            clientSalt: eph.saltHex
          })
        });
      }).then(function (rev) {
        return cryptoApi.completeHostRevealWithStoredIdentity({
          token: token,
          expectedHostIdHex: challengeResp.hostId,
          hostPublicKeyHex: challengeResp.hostPublicKey,
          hostCommitmentHex: challengeResp.hostCommitment,
          hostEphemeralHex: rev.hostEphemeralKey,
          hostSaltHex: rev.hostSalt,
          hostProofHex: rev.hostProof,
          pairingNonceHex: challengeResp.pairingNonce,
          clientCommitmentHex: eph.commitmentHex,
          clientEphemeralPublicKey: eph.ephemeralPublicKey,
          clientEphemeralSecretKey: eph.ephemeralSecretKey
        });
      }).then(function (verified) {
        if (pairSasCode) pairSasCode.textContent = verified.sas;
        if (pairStatusText) pairStatusText.textContent = "Confirm this code on the Flitnix desktop prompt to approve…";
        if (pairSasBox) pairSasBox.hidden = false;

        return api("/api/pair/request", {
          method: "POST",
          headers: { "Content-Type": "application/json" },
          body: JSON.stringify({
            deviceName: deviceName,
            clientPublicKey: pubIdentity.publicKeyHex,
            pairingNonce: challengeResp.pairingNonce,
            clientProof: verified.clientProofHex
          })
        });
      }).then(function (enrolled) {
        if (!enrolled || !enrolled.deviceId) {
          throw new Error("Desktop rejected pairing or returned invalid response");
        }
        return cryptoApi.savePairedHost({
          origin: window.location.origin,
          hostId: challengeResp.hostId,
          deviceId: enrolled.deviceId,
          deviceName: deviceName,
          hostPublicKey: challengeResp.hostPublicKey
        }).catch(function (saveErr) {
          // Roll back desktop enrollment if persistent IndexedDB save fails so we never leave half-paired state.
          return api("/api/devices/" + encodeURIComponent(enrolled.deviceId), { method: "DELETE" })
            .catch(function () {})
            .then(function () { throw saveErr; });
        }).then(function (saved) {
          try {
            if (window.history && window.history.replaceState) {
              window.history.replaceState(null, "", "#device=" + encodeURIComponent(enrolled.deviceId));
            }
          } catch (_) {}
          setPairedUI(saved);
        });
      }).catch(function (err) {
        if (pairSasBox) pairSasBox.hidden = true;
        showPairError((err && err.message) || "Pairing failed.");
      }).finally(function () {
        pairStartBtn.disabled = false;
      });
    });
  }

  function performOneTapReconnect(rec) {
    var cryptoApi = window.FlitnixDeviceCrypto;
    if (!cryptoApi || !rec || !rec.deviceId || !rec.hostId) return Promise.reject(new Error("Missing paired device"));
    showReconnectError("");
    if (reconnectBtn) reconnectBtn.disabled = true;
    var challengeData;
    return cryptoApi.probeStorage().then(function () {
      return cryptoApi.hasValidIdentity();
    }).then(function (hasIdentity) {
      if (!hasIdentity) {
        throw new Error("Device signing key not found in browser storage. Pair this browser again.");
      }
      return fetch("/api/device-sessions/challenge", {
        method: "POST",
        headers: { "Content-Type": "application/json", "Accept": "application/json" },
        body: JSON.stringify({ deviceId: rec.deviceId }),
        cache: "no-store"
      });
    }).then(function (res) {
      return readBody(res).then(function (body) {
        if (res.status >= 200 && res.status < 300) return body;
        var err = new Error((body && body.error) || ("Challenge failed (" + res.status + ")"));
        err.code = body && body.code;
        err.status = res.status;
        throw err;
      });
    }).then(function (chal) {
      if (!chal || String(chal.hostId || "").toLowerCase() !== String(rec.hostId).toLowerCase() ||
          String(chal.deviceId || "").toLowerCase() !== String(rec.deviceId).toLowerCase()) {
        throw new Error("Host identity mismatch during reconnect challenge; refused to sign.");
      }
      challengeData = chal;
      return cryptoApi.signDeviceSessionChallengeWithStoredIdentity({
        expectedOrigin: window.location.origin,
        hostId: chal.hostId,
        deviceId: chal.deviceId,
        nonce: chal.nonce,
        timestamp: chal.timestamp
      });
    }).then(function (signature) {
      return fetch("/api/device-sessions/verify", {
        method: "POST",
        headers: { "Content-Type": "application/json", "Accept": "application/json" },
        body: JSON.stringify({
          deviceId: challengeData.deviceId,
          hostId: challengeData.hostId,
          nonce: challengeData.nonce,
          timestamp: challengeData.timestamp,
          signature: signature
        }),
        cache: "no-store"
      });
    }).then(function (res) {
      return readBody(res).then(function (body) {
        if (res.status >= 200 && res.status < 300 && body && body.token) return body;
        var err = new Error((body && body.error) || ("Verification failed (" + res.status + ")"));
        err.code = body && body.code;
        err.status = res.status;
        throw err;
      });
    }).then(function (sessionResp) {
      token = sessionResp.token;
      try {
        if (window.history && window.history.replaceState) {
          window.history.replaceState(null, "", "#device=" + encodeURIComponent(sessionResp.deviceId));
        }
      } catch (_) {}
      startConnectedSession();
    }).catch(function (err) {
      if (err && err.code === "device_revoked") {
        cryptoApi.removePairedHost(rec.deviceId).catch(function () {});
        if (reconnectPanel) reconnectPanel.hidden = true;
        showCodeError("This browser's trusted pairing was removed on the desktop. Scan QR or use a 6-digit code.");
      } else {
        showReconnectError((err && err.message) || "Reconnect failed.");
      }
    }).finally(function () {
      if (reconnectBtn) reconnectBtn.disabled = false;
    });
  }

  function initOneTapReconnect() {
    var cryptoApi = window.FlitnixDeviceCrypto;
    if (!cryptoApi || typeof indexedDB === "undefined" || !reconnectPanel) {
      if (reconnectPanel) reconnectPanel.hidden = true;
      return;
    }
    cryptoApi.probeStorage().then(function () {
      var lookup = requestedDeviceId
        ? cryptoApi.getPairedHostByDeviceId(requestedDeviceId)
        : cryptoApi.listPairedHostsForOrigin(window.location.origin).then(function (list) {
            return (list && list.length > 0) ? list[0] : null;
          });
      return Promise.all([lookup, cryptoApi.hasValidIdentity()]);
    }).then(function (res) {
      var rec = res[0];
      var hasIdentity = res[1];
      if (!rec || !rec.deviceId || !hasIdentity || rec.origin !== window.location.origin) {
        reconnectPanel.hidden = true;
        return;
      }
      if (reconnectLabel) {
        reconnectLabel.textContent = "Paired browser detected (" + (rec.deviceName || "Trusted Device") + ")";
      }
      reconnectPanel.hidden = false;
      if (reconnectBtn) {
        reconnectBtn.onclick = function () { performOneTapReconnect(rec); };
      }
      if (reconnectForgetBtn) {
        reconnectForgetBtn.onclick = function () {
          cryptoApi.removePairedHost(rec.deviceId).then(function () {
            reconnectPanel.hidden = true;
            try {
              if (window.history && window.history.replaceState) {
                window.history.replaceState(null, "", window.location.pathname + window.location.search);
              }
            } catch (_) {}
          }).catch(function () {
            reconnectPanel.hidden = true;
          });
        };
      }
      if (requestedDeviceId && rec.deviceId === requestedDeviceId) {
        performOneTapReconnect(rec);
      }
    }).catch(function () {
      reconnectPanel.hidden = true;
    });
  }

  function startConnectedSession() {
    appState.ended = false;
    if (rescanCard) rescanCard.hidden = true;
    setStatus("ready", "Connected — pick files to send or download the shared ones");
    flowEl.hidden = false;
    updateAwarenessUI();
    initTrustedPairingUI();
    refreshFiles();
    initQuickText();
    connectEvents();
    scheduleTextPoll();
  }

  function showCodeError(msg) {
    if (!connectCodeError) return;
    connectCodeError.textContent = msg || "";
    connectCodeError.hidden = !msg;
  }

  function setupConnectCodeForm() {
    if (!connectCodeForm || !connectCodeInput) return;
    connectCodeInput.addEventListener("input", function () {
      var digits = (connectCodeInput.value || "").replace(/\D+/g, "").slice(0, 6);
      connectCodeInput.value = digits.length > 3 ? (digits.slice(0, 3) + " " + digits.slice(3)) : digits;
      showCodeError("");
    });
    connectCodeForm.addEventListener("submit", function (e) {
      e.preventDefault();
      var digits = (connectCodeInput.value || "").replace(/\D+/g, "");
      if (digits.length !== 6) {
        showCodeError("Enter all 6 digits shown on the Flitnix desktop.");
        return;
      }
      showCodeError("");
      if (connectCodeBtn) connectCodeBtn.disabled = true;
      fetch("/api/connect-code", {
        method: "POST",
        headers: { "Content-Type": "application/json", "Accept": "application/json" },
        body: JSON.stringify({ code: digits }),
        cache: "no-store"
      }).then(function (res) {
        return readBody(res).then(function (body) {
          if (res.status >= 200 && res.status < 300 && body && body.token) {
            return body;
          }
          var errMsg = (body && body.error) ? body.error : ("Could not connect (" + res.status + ")");
          throw new Error(errMsg);
        });
      }).then(function (body) {
        token = body.token;
        connectCodeInput.value = "";
        try {
          if (window.history && window.history.replaceState) {
            window.history.replaceState(null, "", window.location.pathname + window.location.search);
          }
        } catch (_) {}
        startConnectedSession();
      }).catch(function (err) {
        showCodeError((err && err.message) || "Invalid connection code.");
      }).finally(function () {
        if (connectCodeBtn) connectCodeBtn.disabled = false;
      });
    });
  }

  // ------------------------------------------------------------------
  // Boot
  // ------------------------------------------------------------------
  function boot() {
    setupConnectCodeForm();
    pickBtn.addEventListener("click", function () { fileInput.click(); });
    if (folderInput && pickFolderBtn &&
        "webkitdirectory" in HTMLInputElement.prototype) {
      pickFolderBtn.hidden = false;
      pickFolderBtn.addEventListener("click", function () { folderInput.click(); });
      folderInput.addEventListener("change", function () {
        var files = Array.prototype.slice.call(folderInput.files || []);
        var roots = files.map(function (f) {
          var path = f.webkitRelativePath || "";
          return path.split("/")[0] || "";
        });
        var root = roots[0];
        if (!root || roots.some(function (candidate) { return candidate !== root; }) ||
            files.some(function (f) {
              return !f.webkitRelativePath ||
                f.webkitRelativePath.split("/").length < 2;
            })) {
          setStatus("error", "The selected folder could not be read safely. Please try again.");
          folderInput.value = "";
          return;
        }
        addFiles(files, root);
        folderInput.value = "";
      });
    }
    fileInput.addEventListener("change", function () {
      addFiles(fileInput.files, "");
      fileInput.value = "";
    });
    var sendPanel = document.getElementById("send-panel");
    if (sendPanel) {
      sendPanel.addEventListener("dragover", function (e) {
        e.preventDefault();
        sendPanel.classList.add("panel--dragover");
      });
      sendPanel.addEventListener("dragleave", function () {
        sendPanel.classList.remove("panel--dragover");
      });
      sendPanel.addEventListener("drop", function (e) {
        e.preventDefault();
        sendPanel.classList.remove("panel--dragover");
        if (e.dataTransfer && e.dataTransfer.files && e.dataTransfer.files.length) {
          addFiles(e.dataTransfer.files, "");
        }
      });
    }
    refreshBtn.addEventListener("click", refreshFiles);
    downloadSelectedBtn.addEventListener("click", function () {
      if (browseState.source) {
        var ids = Object.keys(browseState.selected);
        if (!ids.length) return;
        var label = ids.length === 1 ? (browseState.itemNames[ids[0]] || "selected file") : (browseState.source.name + ".zip");
        downloadSelectedFolderItems(browseState.source, ids, [{ id: ids[0], name: label }], downloadSelectedBtn);
        return;
      }
      var selected = Array.prototype.slice.call(filesEl.querySelectorAll(".source-check:checked"));
      var sources = selected.map(function (input) {
        return { id: input.value, name: input.getAttribute("aria-label") || "selected source" };
      });
      if (sources.length) downloadSources(sources, downloadSelectedBtn);
    });

    if (!token) {
      // No pairing token in this load. No session was ever opened here,
      // so nothing "ended": the status line names the real next step.
      endSession("Scan QR or enter 6-digit code",
        "Scan the QR code on the Flitnix desktop, enter the 6-digit connection code, or tap One-Tap Reconnect if previously paired.",
        "Ready to connect");
      initOneTapReconnect();
      return;
    }

    // The token passes the format check; the desktop itself validates it on
    // the first API call (fresh sessions die if they were revoked already).
    startConnectedSession();
  }

  boot();
})();
