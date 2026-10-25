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
  var POLL_MS = 1200;           // approval polling cadence
  var MAX_POLL_FAILURES = 12;   // stop a spinner that has lost its peer

  // ------------------------------------------------------------------
  // Token: read once, then scrub the fragment.
  // ------------------------------------------------------------------
  var token = null;
  var frag = window.location.hash || "";
  if (frag.length > 1) {
    var cand = frag.slice(1);
    try { cand = decodeURIComponent(cand); } catch (err) { /* keep raw */ }
    if (TOKEN_RE.test(cand)) token = cand;
  }
  if (frag) {
    // One-time use: the link disappears from the address bar (and from any
    // "reopen tab" paste target) the moment the page takes the token.
    window.history.replaceState(null, "",
      window.location.pathname + window.location.search);
  }

  // ------------------------------------------------------------------
  // Handles
  // ------------------------------------------------------------------
  var statusEl = document.getElementById("status");
  var statusText = statusEl ? statusEl.querySelector(".status__text") : null;
  var flowEl = document.getElementById("flow");
  var rescanCard = document.getElementById("rescan");
  var rescanText = document.getElementById("rescan-text");
  var pickBtn = document.getElementById("pick-btn");
  var fileInput = document.getElementById("file-input");
  var queueEl = document.getElementById("queue");
  var filesEl = document.getElementById("files");
  var filesEmptyEl = document.getElementById("files-empty");
  var refreshBtn = document.getElementById("refresh-btn");

  var appState = { ended: false };
  var queue = [];      // upload jobs, FIFO
  var pumpBusy = false; // one upload in flight at a time
  var activeBatch = null;

  // ------------------------------------------------------------------
  // Small helpers
  // ------------------------------------------------------------------
  function el(tag, className, text) {
    var n = document.createElement(tag);
    if (className) n.className = className;
    if (text != null) n.textContent = text;
    return n;
  }

  function fmtBytes(n) {
    if (typeof n !== "number" || !isFinite(n) || n < 0) return "";
    var units = ["B", "KB", "MB", "GB", "TB"];
    var v = n, u = 0;
    while (v >= 1000 && u < units.length - 1) { v /= 1000; u += 1; }
    var digits = u === 0 || v >= 100 ? 0 : v >= 10 ? 1 : 2;
    return v.toFixed(digits) + " " + units[u];
  }

  function setStatus(stateName, text) {
    if (statusEl) statusEl.dataset.state = stateName;
    if (statusText) statusText.textContent = text;
  }

  function endSession(title, why) {
    if (appState.ended) return;
    appState.ended = true;
    teardownJobs();
    if (statusEl) statusEl.dataset.state = "ended";
    if (statusText) statusText.textContent = "Session ended";
    if (flowEl) flowEl.hidden = true;
    if (rescanCard) {
      var h = rescanCard.querySelector("h2");
      if (h && title) h.textContent = title;
      if (rescanText && why) rescanText.textContent = why;
      rescanCard.hidden = false;
    }
  }

  function endUnauthorized() {
    endSession("Session ended",
      "The desktop ended this transfer session. Scan the QR code shown on the Flitnix desktop again to start a fresh one.");
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
        if (res.status === 401) { endUnauthorized(); }
        if (res.status >= 200 && res.status < 300) return body;
        var msg = body && body.error ? body.error
          : "Request failed (" + res.status + ")";
        var err = new Error(msg);
        err.status = res.status;
        err.silent = res.status === 401; // session UI already switching over
        throw err;
      });
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
      case "sending": return "Transferring…";
      case "done": return "Sent";
      case "canceled": return "Canceled";
      case "failed": return "Failed";
    }
    return job.phase;
  }

  function paint(job) {
    var li = job.el;
    if (!li) return;
    li.dataset.phase = job.phase;
    var r = job.refs;
    var text = phaseText(job);
    r.bar.hidden = !(job.phase === "sending" || job.phase === "done");
    if (job.phase === "done") { r.bar.value = r.bar.max; }
    if (job.phase === "sending" && job.uploaded > 0 && job.file.size > 0) {
      text += " " + Math.min(100, Math.round((job.uploaded / job.file.size) * 100)) + "%";
    }
    r.state.textContent = text;
    r.err.hidden = !job.err;
    if (job.err) r.err.textContent = job.err;
    // Button is state-dependent.
    var label, action;
    if (job.phase === "sending") {
      label = "Cancel"; action = function () { if (job.xhr) job.xhr.abort(); };
    } else if (job.phase === "queued" || job.phase === "declaring" ||
               job.phase === "waiting") {
      label = "Cancel"; action = function () { removeJob(job); };
    } else if (job.phase === "done") {
      label = "Dismiss"; action = function () { removeJob(job); };
    } else {
      label = job.retryable ? "Retry" : "Dismiss";
      action = job.retryable
        ? function () { retryJob(job); }
        : function () { removeJob(job); };
    }
    r.btn.textContent = label;
    if (job.btnAction) r.btn.removeEventListener("click", job.btnAction);
    job.btnAction = action;
    r.btn.addEventListener("click", action);
  }

  function buildItem(job) {
    var li = el("li", "queue-item");
    var top = el("div", "queue-item__top");
    top.appendChild(el("span", "queue-item__name", job.file.name));
    top.appendChild(el("span", "queue-item__size", fmtBytes(job.file.size)));
    var row = el("div", "queue-item__row");
    var state = el("span", "queue-item__state");
    var bar = el("progress", "queue-item__bar");
    bar.max = job.file.size > 0 ? job.file.size : 1;
    bar.value = job.uploaded;
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
    job.refs = { state: state, bar: bar, btn: btn, err: errLine };
    paint(job);
    return li;
  }

  function addFiles(fileList) {
    Array.prototype.slice.call(fileList).forEach(function (f) {
      var job = {
        file: f, id: null, phase: "queued", uploaded: 0,
        failures: 0, retryable: false, err: null,
        xhr: null, timer: null, removed: false,
        el: null, refs: null, btnAction: null, batchID: null, itemID: null
      };
      queue.push(job);
      queueEl.appendChild(buildItem(job));
    });
    pump();
  }

  function teardownJobs() {
    queue.forEach(function (job) {
      job.removed = true;
      if (job.timer) { clearTimeout(job.timer); job.timer = null; }
      if (job.xhr) { try { job.xhr.abort(); } catch (e) { /* gone */ } }
    });
  }

  function removeJob(job) {
    job.removed = true;
    if (job.timer) { clearTimeout(job.timer); job.timer = null; }
    if (job.xhr) { try { job.xhr.abort(); } catch (e) { /* gone */ } }
    var i = queue.indexOf(job);
    if (i >= 0) queue.splice(i, 1);
    if (job.el && job.el.parentNode) job.el.parentNode.removeChild(job.el);
    // If the removed job was the blocker, let the queue advance.
    if (!pumpBusy && !appState.ended) pump();
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
    if (appState.ended) return;
    job.id = null; job.phase = "queued"; job.err = null;
    job.uploaded = 0; job.failures = 0; job.retryable = false;
    job.refs.bar.value = 0;
    job.refs.bar.hidden = true;
    paint(job);
    pump();
  }

  function pump() {
    if (pumpBusy || appState.ended) return;
    var jobs = queue.filter(function (job) { return job.phase === "queued"; });
    if (!jobs.length) return;
    pumpBusy = true;
    jobs.forEach(function (job) { job.phase = "declaring"; paint(job); });
    api("/api/batches", {
      method: "POST",
      headers: { "Content-Type": "application/json" },
      body: JSON.stringify({
        direction: "phone_to_desktop",
        kind: "files",
        items: jobs.map(function (job) {
          return { relativePath: job.file.name, size: job.file.size };
        })
      })
    }).then(function (batch) {
      if (appState.ended) { pumpBusy = false; return; }
      if (!batch || !batch.id || !Array.isArray(batch.items) ||
          batch.items.length !== jobs.length) {
        throw new Error("The desktop gave an unexpected batch reply");
      }
      activeBatch = { id: batch.id, jobs: jobs, failures: 0, timer: null };
      batch.items.forEach(function (item, index) {
        jobs[index].batchID = batch.id;
        jobs[index].itemID = item.id;
        jobs[index].phase = "waiting";
        paint(jobs[index]);
      });
      pumpBusy = false;
      watchBatch();
    }).catch(function (err) {
      jobs.forEach(function (job) {
        if (job.removed) return;
        job.phase = "failed";
        job.err = (!err || !err.status || err.status >= 500)
          ? "Could not reach the desktop. Check the Wi-Fi connection and retry."
          : (err.message || "The desktop refuses this batch.");
        job.retryable = true;
        paint(job);
      });
      pumpBusy = false;
    });
  }

  function watchBatch() {
    if (!activeBatch || appState.ended) return;
    activeBatch.timer = setTimeout(pollBatch, POLL_MS);
  }

  function pollBatch() {
    if (!activeBatch || appState.ended) return;
    api("/api/batches/" + encodeURIComponent(activeBatch.id)).then(function (batch) {
      if (!activeBatch || appState.ended) return;
      activeBatch.failures = 0;
      batch.items.forEach(function (item) {
        var job = activeBatch.jobs[item.sequence];
        if (!job) return;
        job.uploaded = item.transferred || 0;
        if (item.state === "completed") job.phase = "done";
        else if (item.state === "failed") {
          job.phase = "failed"; job.err = item.error || "Transfer failed."; job.retryable = true;
        } else if (item.state === "canceled") {
          job.phase = "canceled"; job.err = "Upload canceled.";
        } else if (item.state === "uploading") job.phase = "sending";
        paint(job);
      });
      if (batch.state === "paused") {
        activeBatch.jobs.forEach(function (job) {
          if (job.phase === "waiting") job.phase = "queued";
          paint(job);
        });
        activeBatch = null; pumpBusy = false; pump(); return;
      }
      var next = activeBatch.jobs.find(function (job) { return job.phase === "waiting"; });
      if (batch.approvalState === "rejected" || batch.state === "rejected") {
        activeBatch.jobs.forEach(function (job) {
          if (job.phase !== "done") { job.phase = "failed"; job.err = "The desktop declined this batch."; job.retryable = false; paint(job); }
        });
        activeBatch = null; pump(); return;
      }
      if (next && (batch.approvalState === "approved" || batch.state === "queued")) {
        next.phase = "sending"; next.uploaded = 0; paint(next);
        sendBatchItem(next);
        return;
      }
      if (!next) {
        activeBatch = null; pumpBusy = false; pump(); return;
      }
      watchBatch();
    }).catch(function (err) {
      if (!activeBatch || appState.ended || (err && err.silent)) return;
      activeBatch.failures += 1;
      if (activeBatch.failures >= MAX_POLL_FAILURES) {
        activeBatch.jobs.forEach(function (job) {
          if (job.phase !== "done") { job.phase = "failed"; job.err = "Lost contact with the desktop. Rescan or retry."; job.retryable = true; paint(job); }
        });
        activeBatch = null; pumpBusy = false; return;
      }
      watchBatch();
    });
  }

  function sendBatchItem(job) {
    putBatchContent(job).then(function () {
      job.phase = "done"; job.err = null; paint(job);
      watchBatch();
    }).catch(function (e) {
      if (!activeBatch || job.removed || appState.ended) return;
      if (e && e.aborted) { job.phase = "canceled"; job.err = "Upload canceled."; }
      else { job.phase = "failed"; job.err = (e && e.message) || "Transfer failed."; job.retryable = true; }
      activeBatch.jobs.forEach(function (other) {
        if (other !== job && other.phase === "waiting") other.phase = "queued";
        if (other !== job) paint(other);
      });
      paint(job);
      activeBatch = null; pumpBusy = false;
    });
  }

  function putBatchContent(job) {
    return new Promise(function (resolve, reject) {
      var xhr = new XMLHttpRequest();
      job.xhr = xhr;
      xhr.open("PUT", "/api/batches/" + encodeURIComponent(job.batchID) +
        "/items/" + encodeURIComponent(job.itemID) + "/content");
      xhr.setRequestHeader("Authorization", "Bearer " + token);
      xhr.setRequestHeader("Content-Type", "application/octet-stream");
      xhr.onload = function () {
        job.xhr = null;
        if (job.removed || appState.ended) { reject({ aborted: true }); return; }
        if (xhr.status === 401) { endUnauthorized(); reject({ aborted: true }); return; }
        if (xhr.status >= 200 && xhr.status < 300) { resolve(); return; }
        var msg = "Upload failed (" + xhr.status + ")";
        try { var parsed = JSON.parse(xhr.responseText); if (parsed && parsed.error) msg = parsed.error; } catch (e) {}
        reject(new Error(msg));
      };
      xhr.upload.onprogress = function (ev) {
        if (!ev.lengthComputable) return;
        job.uploaded = ev.loaded; paint(job);
      };
      xhr.onerror = function () { job.xhr = null; reject(new Error("Network error while uploading.")); };
      xhr.onabort = function () { job.xhr = null; reject({ aborted: true }); };
      xhr.send(job.file);
    });
  }

  function watch(job) {
    if (job.removed || appState.ended) return;
    job.timer = setTimeout(function () { pollOnce(job); }, POLL_MS);
  }

  function pollOnce(job) {
    if (job.removed || appState.ended) return;
    api("/api/uploads/" + encodeURIComponent(job.id))
      .then(function (up) {
        if (job.removed || appState.ended) return;
        if (!up || !up.state) throw new Error("The desktop gave an unexpected reply");
        var s = up.state;
        if (s === "approved") {
          job.failures = 0;
          job.phase = "sending";
          job.uploaded = 0;
          paint(job);
          send(job);
          return;
        }
        if (s === "rejected") { failJob(job, up.reason || "The desktop declined this file.", false); return; }
        if (s === "canceled") { cancelJob(job, "This transfer was canceled on the desktop."); return; }
        if (s === "failed") { failJob(job, up.reason || "Transfer failed on the desktop.", false); return; }
        if (s === "completed") { doneJob(job); return; }
        // pending (or uploading from an outside party): keep waiting.
        job.failures = 0;
        watch(job);
      })
      .catch(function (err) {
        if (job.removed || appState.ended) return;
        if (err && err.silent) return;
        job.failures += 1;
        if (job.failures >= MAX_POLL_FAILURES) {
          failJob(job, "Lost contact with the desktop. Rescan or retry.", true);
          return;
        }
        watch(job);
      });
  }

  function send(job) {
    putContent(job).then(function () {
      doneJob(job);
    }).catch(function (e) {
      if (job.removed || appState.ended) { pumpBusy = false; return; }
      if (e && e.aborted) { cancelJob(job, "Upload canceled."); return; }
      failJob(job, (e && e.message) || "Transfer failed.", true);
    });
  }

  function putContent(job) {
    return new Promise(function (resolve, reject) {
      var xhr = new XMLHttpRequest();
      job.xhr = xhr;
      xhr.open("PUT", "/api/uploads/" + encodeURIComponent(job.id) + "/content");
      xhr.setRequestHeader("Authorization", "Bearer " + token);
      xhr.setRequestHeader("Content-Type", "application/octet-stream");
      xhr.onload = function () {
        job.xhr = null;
        if (job.removed || appState.ended) { reject({ aborted: true }); return; }
        if (xhr.status === 401) { endUnauthorized(); reject({ aborted: true }); return; }
        if (xhr.status >= 200 && xhr.status < 300) {
          var body = null;
          try { body = JSON.parse(xhr.responseText); } catch (e) { body = null; }
          resolve(body);
          return;
        }
        var msg = "Upload failed (" + xhr.status + ")";
        try {
          var parsed = JSON.parse(xhr.responseText);
          if (parsed && parsed.error) msg = parsed.error;
        } catch (e2) { /* plain body */ }
        reject(new Error(msg));
      };
      xhr.upload.onprogress = function (ev) {
        if (!ev.lengthComputable) return;
        job.uploaded = ev.loaded;
        if (job.file.size > 0 && job.file.size > 1) {
          job.refs.bar.value = Math.min(ev.loaded, job.file.size);
        }
        if (!job.removed && !appState.ended) paint(job);
      };
      xhr.onerror = function () {
        job.xhr = null;
        reject(new Error("Network error while uploading. Check the Wi-Fi connection and retry."));
      };
      xhr.onabort = function () {
        job.xhr = null;
        reject({ aborted: true });
      };
      xhr.send(job.file);
    });
  }

  // ------------------------------------------------------------------
  // Shared files (desktop → phone)
  // ------------------------------------------------------------------
  function refreshFiles() {
    refreshBtn.disabled = true;
    var prev = refreshBtn.textContent;
    refreshBtn.textContent = "Loading…";
    api("/api/files").then(function (data) {
      if (appState.ended) return;
      drawFiles(data || []);
    }).catch(function (err) {
      if (appState.ended || (err && err.silent)) return;
      filesEmptyEl.textContent = err.message || "Could not load the desktop files.";
      filesEmptyEl.hidden = false;
    }).then(function () {
      refreshBtn.disabled = false;
      refreshBtn.textContent = prev;
    });
  }

  function drawFiles(list) {
    filesEl.textContent = "";
    var items = Array.isArray(list) ? list : [];
    filesEmptyEl.hidden = items.length > 0;
    if (!items.length) {
      filesEmptyEl.textContent = "Nothing is shared with this session yet.";
      return;
    }
    items.forEach(function (f) {
      if (!f || !f.id || !f.name) return;
      filesEl.appendChild(fileRow(f));
    });
  }

  function fileRow(f) {
    var li = el("li", "file-item");
    var left = el("div", "file-item__meta");
    left.appendChild(el("span", "file-item__name", f.name));
    left.appendChild(el("span", "file-item__size", fmtBytes(f.size)));
    var btn = el("button", "btn btn--small");
    btn.type = "button";
    btn.textContent = "Download";
    btn.addEventListener("click", function () { download(f, btn); });
    li.appendChild(left);
    li.appendChild(btn);
    return li;
  }

  function download(f, btn) {
    btn.disabled = true;
    var prev = btn.textContent;
    btn.textContent = "Preparing…";
    fetch("/api/files/" + encodeURIComponent(f.id) + "/download", {
      headers: { "Authorization": "Bearer " + token },
      cache: "no-store"
    }).then(function (res) {
      if (res.status === 401) {
        endUnauthorized();
        throw new Error("silent");
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
      var cd = res.headers.get("Content-Disposition") || "";
      return res.blob().then(function (blob) {
        saveBlob(blob, dispositionName(cd, f.name));
      });
    }).catch(function (err) {
      if (err && err.message === "silent") return;
      var msg = err && err.message ? err.message : "Download failed.";
      if (err instanceof TypeError) {
        msg = "Network error — could not reach the desktop.";
      }
      showDownloadError(f, msg);
    }).then(function () {
      btn.disabled = false;
      btn.textContent = prev;
    });
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
  // Boot
  // ------------------------------------------------------------------
  function boot() {
    pickBtn.addEventListener("click", function () { fileInput.click(); });
    fileInput.addEventListener("change", function () {
      addFiles(fileInput.files);
      fileInput.value = "";
    });
    refreshBtn.addEventListener("click", refreshFiles);

    if (!token) {
      endSession("Scan the QR code",
        "Open this page by scanning the QR code shown on the Flitnix desktop. If you reloaded an old tab, the old link was single-use — rescan to reconnect.");
      return;
    }

    // The token passes the format check; the desktop itself validates it on
    // the first API call (fresh sessions die if they were revoked already).
    setStatus("ready", "Connected — pick files to send or download the shared ones");
    flowEl.hidden = false;
    refreshFiles();
  }

  boot();
})();
